package aws

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	sdk "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambda"
	lambdatypes "github.com/aws/aws-sdk-go-v2/service/lambda/types"
	"github.com/aws/smithy-go"

	"github.com/tasktrooper/telemetry/services/integrations/internal/model"
)

// Lambda discovery (contracts §21): ListFunctions + ListTags + ListEventSourceMappings (required), and the
// optional GetFunctionConcurrency / GetPolicy / ListFunctionUrlConfigs (AccessDenied → skipped silently for
// the rest of the run). One serverless_functions row per function: slug aws-<region>-<name>.

// ServiceTag is the function tag that overrides the span service name.
const ServiceTag = "tasktrooper.service"

type (
	lambdaArch  = lambdatypes.Architecture
	lambdaLayer = lambdatypes.Layer
	lambdaEnv   = lambdatypes.EnvironmentResponse
)

type lambdaFn struct {
	Name, Slug string
	// Stream is true when an event source mapping reads a stream (Kinesis, DynamoDB, Kafka) → IteratorAge.
	Stream bool
}

// LambdaSlug is the serverless_functions slug of a Lambda function.
func LambdaSlug(region, name string) string { return model.Slug("aws", region+"-"+name) }

// optional tracks optional Lambda APIs the role may not be allowed to call.
type optional struct {
	mu     sync.Mutex
	denied map[string]bool
}

func (o *optional) allowed(api string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	return !o.denied[api]
}

// check returns true if err is nil; access-denied disables the API, any other error is ignored (best effort).
func (o *optional) check(api string, err error) bool {
	if err == nil {
		return true
	}
	var ae smithy.APIError
	if errors.As(err, &ae) && (strings.Contains(ae.ErrorCode(), "AccessDenied") || ae.ErrorCode() == "UnauthorizedOperation") {
		o.mu.Lock()
		o.denied[api] = true
		o.mu.Unlock()
	}
	return false
}

func (p *Provider) discoverLambda(ctx context.Context, c *clients, rs *regionState, inv *model.Inventory) error {
	var confs []lambdaConf
	pg := lambda.NewListFunctionsPaginator(c.lambda, &lambda.ListFunctionsInput{})
	for pg.HasMorePages() && len(confs) < p.cfg.MaxFunctions {
		out, err := pg.NextPage(ctx)
		if err != nil {
			return wrap(c.region, "lambda:ListFunctions", err)
		}
		for _, f := range out.Functions {
			confs = append(confs, lambdaConf{
				name: sdk.ToString(f.FunctionName), arn: sdk.ToString(f.FunctionArn), runtime: string(f.Runtime),
				pkg: string(f.PackageType), handler: sdk.ToString(f.Handler), memory: f.MemorySize, timeout: f.Timeout,
				arch: f.Architectures, lastModified: sdk.ToString(f.LastModified), state: string(f.State),
				otel: instrumented(f.Layers, f.Environment),
			})
		}
	}
	if len(confs) > p.cfg.MaxFunctions {
		confs = confs[:p.cfg.MaxFunctions]
	}
	if len(confs) == 0 {
		return nil
	}
	var errs error
	// Event source mappings of the whole region in one paginated listing, keyed by function ARN.
	esm := map[string][]string{}
	stream := map[string]bool{}
	ep := lambda.NewListEventSourceMappingsPaginator(c.lambda, &lambda.ListEventSourceMappingsInput{})
	for ep.HasMorePages() {
		out, err := ep.NextPage(ctx)
		if err != nil {
			errs = errors.Join(errs, wrap(c.region, "lambda:ListEventSourceMappings", err))
			break
		}
		for _, m := range out.EventSourceMappings {
			if st := sdk.ToString(m.State); strings.EqualFold(st, "Disabled") || strings.EqualFold(st, "Disabling") {
				continue
			}
			fn := baseARN(sdk.ToString(m.FunctionArn))
			t, isStream := EventSourceTrigger(sdk.ToString(m.EventSourceArn), m.SelfManagedEventSource != nil)
			if t == "" {
				continue
			}
			esm[fn] = append(esm[fn], t)
			stream[fn] = stream[fn] || isStream
		}
	}

	fns := make([]model.Function, len(confs))
	tagErrs := make([]error, len(confs))
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, f := range confs {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			fns[i], tagErrs[i] = p.describeLambda(ctx, c, f, esm[f.arn])
		}()
	}
	wg.Wait()
	// Report tag errors once (usually the same AccessDenied for every function).
	for _, e := range tagErrs {
		if e != nil {
			errs = errors.Join(errs, wrap(c.region, "lambda:ListTags", e))
			break
		}
	}
	for i, f := range fns {
		rs.functions = append(rs.functions, lambdaFn{Name: f.Name, Slug: f.Slug, Stream: stream[confs[i].arn]})
	}
	inv.Functions = append(inv.Functions, fns...)
	return errs
}

type lambdaConf struct {
	name, arn, runtime, pkg, handler, lastModified, state string
	memory, timeout                                       *int32
	arch                                                  []lambdaArch
	otel                                                  bool
}

func (p *Provider) describeLambda(ctx context.Context, c *clients, f lambdaConf, esm []string) (model.Function, error) {
	fn := model.Function{
		Slug: LambdaSlug(c.region, f.name), Name: f.name, Region: c.region, Runtime: f.runtime, Service: f.name,
		Meta: map[string]any{"arn": f.arn, "region": c.region},
	}
	if fn.Runtime == "" && f.pkg == "Image" {
		fn.Runtime = "container"
	}
	if f.memory != nil {
		fn.MemoryMB = sdk.Int(int(*f.memory))
	}
	if f.timeout != nil {
		fn.TimeoutS = sdk.Int(int(*f.timeout))
	}
	fn.Arch = "x86_64"
	if len(f.arch) > 0 {
		fn.Arch = string(f.arch[0])
	}
	for k, v := range map[string]string{"handler": f.handler, "packageType": f.pkg, "lastModified": f.lastModified, "state": f.state} {
		if v != "" {
			fn.Meta[k] = v
		}
	}
	if f.otel {
		fn.Meta["otel"] = true
	}
	triggers := slices.Clone(esm)
	var tagErr error
	if tags, err := c.lambda.ListTags(ctx, &lambda.ListTagsInput{Resource: sdk.String(f.arn)}); err != nil {
		tagErr = err
	} else if len(tags.Tags) > 0 {
		fn.Meta["tags"] = tags.Tags
		if s := strings.TrimSpace(tags.Tags[ServiceTag]); s != "" {
			fn.Service = s
		}
	}
	if p.opt.allowed("concurrency") {
		out, err := c.lambda.GetFunctionConcurrency(ctx, &lambda.GetFunctionConcurrencyInput{FunctionName: sdk.String(f.name)})
		if p.opt.check("concurrency", err) && out.ReservedConcurrentExecutions != nil {
			fn.Meta["reservedConcurrency"] = *out.ReservedConcurrentExecutions
		}
	}
	if p.opt.allowed("policy") {
		out, err := c.lambda.GetPolicy(ctx, &lambda.GetPolicyInput{FunctionName: sdk.String(f.name)})
		if p.opt.check("policy", err) {
			triggers = append(triggers, PolicyTriggers(sdk.ToString(out.Policy))...)
		}
	}
	if p.opt.allowed("urls") {
		out, err := c.lambda.ListFunctionUrlConfigs(ctx, &lambda.ListFunctionUrlConfigsInput{FunctionName: sdk.String(f.name)})
		if p.opt.check("urls", err) && len(out.FunctionUrlConfigs) > 0 {
			fn.URL = sdk.ToString(out.FunctionUrlConfigs[0].FunctionUrl)
			triggers = append(triggers, "http")
		}
	}
	sort.Strings(triggers)
	fn.Triggers = slices.Compact(triggers)
	if fn.Triggers == nil {
		fn.Triggers = []string{}
	}
	return fn, tagErr
}

// baseARN strips a version / alias qualifier (arn:aws:lambda:r:a:function:name[:qualifier]).
func baseARN(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) > 7 {
		return strings.Join(parts[:7], ":")
	}
	return arn
}

// EventSourceTrigger maps an event source mapping's source ARN to a trigger (sqs:<queue>, kinesis:<stream>,
// dynamodb:<table>, kafka:<cluster>, mq:<broker>, docdb:<cluster>) and whether it is a stream (IteratorAge).
func EventSourceTrigger(arn string, selfManaged bool) (string, bool) {
	if arn == "" {
		if selfManaged {
			return "kafka", true
		}
		return "", false
	}
	parts := strings.SplitN(arn, ":", 6)
	if len(parts) < 6 {
		return "", false
	}
	svc, res := parts[2], parts[5]
	switch svc {
	case "sqs":
		return "sqs:" + res, false
	case "kinesis":
		return "kinesis:" + strings.TrimPrefix(res, "stream/"), true
	case "dynamodb":
		// table/<name>/stream/<label>
		res = strings.TrimPrefix(res, "table/")
		if i := strings.Index(res, "/"); i >= 0 {
			res = res[:i]
		}
		return "dynamodb:" + res, true
	case "kafka":
		// cluster/<name>/<uuid>
		f := strings.Split(res, "/")
		if len(f) >= 2 {
			return "kafka:" + f[1], true
		}
		return "kafka", true
	case "mq":
		// broker:<name>:<id>
		f := strings.Split(res, ":")
		if len(f) >= 2 {
			return "mq:" + f[1], false
		}
		return "mq", false
	case "rds":
		f := strings.Split(res, ":")
		return "docdb:" + f[len(f)-1], true
	}
	return svc, false
}

// PolicyTriggers reads a function's resource policy: which AWS services may invoke it. API Gateway → "api",
// EventBridge rules → "schedule:<rule>" / "eventbridge:<rule>", S3 → "s3:<bucket>", SNS → "sns:<topic>",
// ALB → "alb", IoT / Cognito / others → service prefix. Function URL permissions (lambda:InvokeFunctionUrl) are
// reported via ListFunctionUrlConfigs ("http").
func PolicyTriggers(policy string) []string {
	if policy == "" {
		return nil
	}
	var doc struct {
		Statement []struct {
			Effect    string          `json:"Effect"`
			Principal json.RawMessage `json:"Principal"`
			Action    json.RawMessage `json:"Action"`
			Condition map[string]map[string]json.RawMessage
		} `json:"Statement"`
	}
	if json.Unmarshal([]byte(policy), &doc) != nil {
		return nil
	}
	var out []string
	for _, st := range doc.Statement {
		if st.Effect != "" && st.Effect != "Allow" {
			continue
		}
		if strings.Contains(string(st.Action), "InvokeFunctionUrl") {
			continue
		}
		source := ""
		for _, cond := range st.Condition {
			for k, v := range cond {
				if strings.EqualFold(k, "AWS:SourceArn") {
					var s string
					if json.Unmarshal(v, &s) != nil {
						var l []string
						if json.Unmarshal(v, &l) == nil && len(l) > 0 {
							s = l[0]
						}
					}
					source = s
				}
			}
		}
		for _, svc := range principals(st.Principal) {
			name := strings.TrimSuffix(svc, ".amazonaws.com")
			switch name {
			case "apigateway":
				out = append(out, "api")
			case "events":
				rule := afterLast(source, "/")
				if rule == "" {
					out = append(out, "eventbridge")
				} else if r := strings.ToLower(rule); strings.Contains(r, "schedule") || strings.Contains(r, "cron") || strings.Contains(r, "rate") {
					out = append(out, "schedule:"+rule)
				} else {
					out = append(out, "eventbridge:"+rule)
				}
			case "scheduler":
				out = append(out, "schedule")
			case "s3":
				if b := afterLast(source, ":"); b != "" {
					out = append(out, "s3:"+b)
				} else {
					out = append(out, "s3")
				}
			case "sns":
				if t := afterLast(source, ":"); t != "" {
					out = append(out, "sns:"+t)
				} else {
					out = append(out, "sns")
				}
			case "elasticloadbalancing":
				out = append(out, "alb")
			case "":
			default:
				out = append(out, name)
			}
		}
	}
	return out
}

func afterLast(s, sep string) string {
	if i := strings.LastIndex(s, sep); i >= 0 {
		return s[i+1:]
	}
	return ""
}

// principals returns the Service principals of a policy statement ("Principal": {"Service": "x" | ["x"]}).
func principals(raw json.RawMessage) []string {
	var p struct {
		Service json.RawMessage `json:"Service"`
	}
	if json.Unmarshal(raw, &p) != nil || len(p.Service) == 0 {
		return nil
	}
	var one string
	if json.Unmarshal(p.Service, &one) == nil {
		return []string{one}
	}
	var many []string
	_ = json.Unmarshal(p.Service, &many)
	return many
}

// instrumented reports whether a function looks OTel-instrumented (ADOT / OTel layer or exec wrapper). Only
// layer ARNs and env var *names* are inspected; values are never read or stored.
func instrumented(layers []lambdaLayer, env *lambdaEnv) bool {
	for _, l := range layers {
		a := strings.ToLower(sdk.ToString(l.Arn))
		if strings.Contains(a, "otel") || strings.Contains(a, "opentelemetry") || strings.Contains(a, "trooper") {
			return true
		}
	}
	if env != nil {
		if _, ok := env.Variables["AWS_LAMBDA_EXEC_WRAPPER"]; ok {
			return true
		}
	}
	return false
}

// lambdaQueries returns the CloudWatch AWS/Lambda queries of a region (keys fnKey(name), labels = §21 metrics).
func lambdaQueries(rs *regionState) []cwQuery {
	var qs []cwQuery
	for _, f := range rs.functions {
		d := [2]string{"FunctionName", f.Name}
		k := fnKey(f.Name)
		for _, s := range []struct{ metric, stat, as string }{
			{"Invocations", "Sum", model.FaaSInvocations},
			{"Errors", "Sum", model.FaaSErrors},
			{"Throttles", "Sum", model.FaaSThrottles},
			{"Duration", "Average", model.FaaSDurationAvg},
			{"Duration", "Maximum", model.FaaSDurationMax},
			{"Duration", "p95", model.FaaSDurationP95},
			{"Duration", "p99", model.FaaSDurationP99},
			{"ConcurrentExecutions", "Maximum", model.FaaSConcurrency},
		} {
			x := q(k, "AWS/Lambda", s.metric, s.stat, d)
			x.as = s.as
			qs = append(qs, x)
		}
		if f.Stream {
			x := q(k, "AWS/Lambda", "IteratorAge", "Maximum", d)
			x.as = model.FaaSIteratorAgeMs
			qs = append(qs, x)
		}
	}
	return qs
}

func fnKey(name string) string { return "lambda/" + name }

// MapLambda emits every datapoint of the window (not only the newest): Lambda sums arrive late and a minute
// with no invocations has no datapoint, so re-sending the window keeps faas_1m complete (gauge max is idempotent).
func MapLambda(rs *regionState, s Series) []model.Sample {
	var out []model.Sample
	for _, f := range rs.functions {
		a := model.FaaSAttrs(f.Slug, f.Name)
		for name, pts := range s[fnKey(f.Name)] {
			for _, pt := range pts {
				out = append(out, model.Sample{Name: name, Value: pt.Value, TS: pt.TS.UTC().Truncate(time.Minute), Region: rs.region, Attrs: a})
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Attrs["trooper.faas.id"] != out[j].Attrs["trooper.faas.id"] {
			return out[i].Attrs["trooper.faas.id"] < out[j].Attrs["trooper.faas.id"]
		}
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].TS.Before(out[j].TS)
	})
	return out
}
