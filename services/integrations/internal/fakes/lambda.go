package fakes

import (
	"io"
	"net/http"
	"strings"
)

// Lambda (restJson1) fixtures: three functions over two ListFunctions pages, event source mappings (SQS,
// Kinesis, one disabled DynamoDB), tags, reserved concurrency, resource policies (API Gateway, EventBridge
// schedule) and a function URL.

const lambdaPage1 = `{"Functions":[
 {"FunctionName":"orders-api","FunctionArn":"arn:aws:lambda:eu-central-1:1:function:orders-api","Runtime":"python3.12","Handler":"app.handler",
  "MemorySize":512,"Timeout":30,"Architectures":["arm64"],"PackageType":"Zip","State":"Active","LastModified":"2026-10-20T10:00:00.000+0000",
  "Layers":[{"Arn":"arn:aws:lambda:eu-central-1:901920570463:layer:aws-otel-python-arm64-ver-1-25-0:1"}],
  "Environment":{"Variables":{"SECRET_TOKEN":"do-not-store"}}},
 {"FunctionName":"orders-worker","FunctionArn":"arn:aws:lambda:eu-central-1:1:function:orders-worker","Runtime":"nodejs20.x","Handler":"index.handler",
  "MemorySize":1024,"Timeout":60,"Architectures":["x86_64"],"PackageType":"Zip","State":"Active"}],
 "NextMarker":"p2"}`

const lambdaPage2 = `{"Functions":[
 {"FunctionName":"nightly-report","FunctionArn":"arn:aws:lambda:eu-central-1:1:function:nightly-report","MemorySize":2048,"Timeout":900,
  "PackageType":"Image","State":"Active"}]}`

const lambdaESM = `{"EventSourceMappings":[
 {"UUID":"1","State":"Enabled","FunctionArn":"arn:aws:lambda:eu-central-1:1:function:orders-worker","EventSourceArn":"arn:aws:sqs:eu-central-1:1:orders"},
 {"UUID":"2","State":"Enabled","FunctionArn":"arn:aws:lambda:eu-central-1:1:function:orders-worker:live","EventSourceArn":"arn:aws:kinesis:eu-central-1:1:stream/clicks"},
 {"UUID":"3","State":"Disabled","FunctionArn":"arn:aws:lambda:eu-central-1:1:function:orders-worker","EventSourceArn":"arn:aws:dynamodb:eu-central-1:1:table/carts/stream/2026"}]}`

const apiPolicy = `{"Version":"2012-10-17","Statement":[{"Sid":"apigw","Effect":"Allow","Principal":{"Service":"apigateway.amazonaws.com"},
 "Action":"lambda:InvokeFunction","Resource":"arn:aws:lambda:eu-central-1:1:function:orders-api",
 "Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:execute-api:eu-central-1:1:abc/*/*/orders"}}},
 {"Sid":"url","Effect":"Allow","Principal":"*","Action":"lambda:InvokeFunctionUrl","Resource":"arn:aws:lambda:eu-central-1:1:function:orders-api"}]}`

const schedulePolicy = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":{"Service":"events.amazonaws.com"},
 "Action":"lambda:InvokeFunction","Condition":{"ArnLike":{"AWS:SourceArn":"arn:aws:events:eu-central-1:1:rule/nightly-schedule"}}}]}`

func jsonString(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)
	return `"` + r.Replace(s) + `"`
}

func isLambda(path string) bool {
	for _, p := range []string{"/2015-03-31/", "/2017-03-31/tags/", "/2019-09-30/functions/", "/2021-10-31/functions/"} {
		if strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

func (f *AWS) lambda(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	p := r.URL.Path
	notFound := func() {
		w.Header().Set("X-Amzn-Errortype", "ResourceNotFoundException")
		w.WriteHeader(404)
		_, _ = io.WriteString(w, `{"Type":"User","Message":"not found"}`)
	}
	switch {
	case p == "/2015-03-31/functions" || p == "/2015-03-31/functions/":
		if r.URL.Query().Get("Marker") == "p2" {
			_, _ = io.WriteString(w, lambdaPage2)
		} else {
			_, _ = io.WriteString(w, lambdaPage1)
		}
	case strings.HasPrefix(p, "/2015-03-31/event-source-mappings"):
		_, _ = io.WriteString(w, lambdaESM)
	case strings.HasPrefix(p, "/2017-03-31/tags/"):
		if strings.HasSuffix(p, "function:orders-api") {
			_, _ = io.WriteString(w, `{"Tags":{"tasktrooper.service":"orders","team":"checkout"}}`)
		} else {
			_, _ = io.WriteString(w, `{"Tags":{}}`)
		}
	case strings.HasSuffix(p, "/concurrency"):
		if strings.Contains(p, "orders-worker") {
			_, _ = io.WriteString(w, `{"ReservedConcurrentExecutions":50}`)
		} else {
			_, _ = io.WriteString(w, `{}`)
		}
	case strings.HasSuffix(p, "/policy"):
		switch {
		case strings.Contains(p, "orders-api"):
			_, _ = io.WriteString(w, `{"Policy":`+jsonString(apiPolicy)+`,"RevisionId":"r"}`)
		case strings.Contains(p, "nightly-report"):
			_, _ = io.WriteString(w, `{"Policy":`+jsonString(schedulePolicy)+`,"RevisionId":"r"}`)
		default:
			notFound()
		}
	case strings.HasSuffix(p, "/urls"):
		if strings.Contains(p, "orders-api") {
			_, _ = io.WriteString(w, `{"FunctionUrlConfigs":[{"FunctionUrl":"https://abc.lambda-url.eu-central-1.on.aws/","AuthType":"NONE",
				"FunctionArn":"arn:aws:lambda:eu-central-1:1:function:orders-api"}]}`)
		} else {
			_, _ = io.WriteString(w, `{"FunctionUrlConfigs":[]}`)
		}
	default:
		f.t.Errorf("unexpected lambda path %q", p)
		w.WriteHeader(404)
	}
}
