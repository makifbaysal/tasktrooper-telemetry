package extract

import (
	"slices"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// want is "kind|service|key|source" (source optional: "kind|service|key").
func index(items []model.Item) map[string]model.Item {
	m := map[string]model.Item{}
	for _, it := range items {
		m[it.Kind+"|"+it.Service+"|"+it.Key] = it
	}
	return m
}

func TestFixtureRepos(t *testing.T) {
	tests := []struct {
		repo      string
		want      []string // kind|service|key[|source]
		absent    []string // keys that must not appear (any kind/service)
		languages []string
	}{
		{
			repo: "testdata/gorepo",
			want: []string{
				// net/http Go 1.22 patterns, SDK init names the service
				"endpoint|orders-api|GET /v1/orders/:id", "endpoint|orders-api|POST /v1/orders", "endpoint|orders-api|ANY /healthz",
				"endpoint|orders-api|ANY /static",
				// chi Route / nested Route / Group / Mount / Method
				"endpoint|orders-api|GET /v2/carts/:cartID", "endpoint|orders-api|POST /v2/admin/refunds",
				"endpoint|orders-api|DELETE /v2/admin/users/:userID", "endpoint|orders-api|PUT /v2/profile",
				"endpoint|orders-api|GET /v2/reports/daily", "endpoint|orders-api|PATCH /v2/carts/:cartID",
				// sarama / franz-go consumers keyed by group
				"consumer|orders-api|order-events.fulfilment|orders.created", "consumer|orders-api|payments-dlq-replayer|payments.dlq",
				"dependency|orders-api|db · postgres", "dependency|orders-api|cache · redis", "dependency|orders-api|queue · kafka",
				"dependency|orders-api|http · api.stripe.com", "dependency|orders-api|http · inventory.internal.example.com",
				// gin groups + group passed to a function; .trooper/config.yaml names the service
				"endpoint|payments-api|POST /api/v1/charges", "endpoint|payments-api|GET /api/v1/charges/:id",
				"endpoint|payments-api|POST /api/v1/refunds", "endpoint|payments-api|GET /healthz",
				// gRPC from the generated ServiceDesc
				"endpoint|payments-api|GRPC /payments.v1.Ledger/PostEntry", "endpoint|payments-api|GRPC /payments.v1.Ledger/WatchEntries",
				// echo / fiber, module directory name as service
				"endpoint|catalog|GET /catalog/items/:id", "endpoint|catalog|DELETE /catalog/items/:id",
				"endpoint|catalog|GET /f/v1/search", "endpoint|catalog|POST /f/upload",
				// asynq, robfig/cron, gorilla Methods + Subrouter
				"consumer|worker|email:send|email:send", "cron|worker|syncInvoices|*/15 * * * *",
				"endpoint|worker|POST /hooks/github", "endpoint|worker|GET /internal/jobs/:id", "endpoint|worker|DELETE /internal/jobs/:id",
			},
			absent:    []string{"GET /commented", "ANY /hooks/github"},
			languages: []string{"go"},
		},
		{
			repo: "testdata/jsrepo",
			want: []string{
				// express: SDK init service, mount of an imported router, route() chain, inline handlers
				"endpoint|checkout-api|GET /healthz", "endpoint|checkout-api|POST /v1/checkout",
				"endpoint|checkout-api|GET /v1/cart/:cartId", "endpoint|checkout-api|PUT /v1/cart/:cartId",
				"endpoint|checkout-api|GET /v1/orders/:id", "endpoint|checkout-api|DELETE /v1/orders/:id",
				"dependency|checkout-api|db · postgres", "dependency|checkout-api|http · api.stripe.com",
				// NestJS with global prefix, microservice event, schedule
				"endpoint|nest|GET /api/invoices", "endpoint|nest|GET /api/invoices/:id", "endpoint|nest|POST /api/invoices/:id/pay",
				"consumer|nest|invoice.created|invoice.created", "cron|nest|nightlyDunning|0 3 * * *",
				// Next.js app router (route groups, dynamic segments) and pages router
				"endpoint|web|POST /api/checkout", "endpoint|web|GET /api/checkout", "view|web|/products/:slug",
				"endpoint|web|DELETE /api/users/:id", "endpoint|web|GET /api/users/:id", "view|web|/about",
				// react-router objects and JSX
				"view|spa|/", "view|spa|/orders/:orderId", "view|spa|/settings", "view|spa|/checkout",
				// kafkajs (group, several topics), bullmq, node-cron, amqplib, sqs-consumer
				"consumer|worker|payment-events|payments.captured,payments.failed", "consumer|worker|emails|emails",
				"cron|worker|rebuildSearchIndex|0 * * * *", "consumer|worker|order.shipped|order.shipped", "consumer|worker|refunds|refunds",
				"dependency|worker|queue · kafka", "dependency|worker|http · hooks.slack.com",
			},
			absent:    []string{"GET /commented-out", "POST /also-commented", "GET /inside-string", "GET /v1/not-a-route", "GET /:id"},
			languages: []string{"typescript", "javascript"},
		},
		{
			repo: "testdata/pyrepo",
			want: []string{
				// FastAPI: router prefix + include_router prefix, multi-line decorator, api_route methods
				"endpoint|payments-py|GET /healthz", "endpoint|payments-py|POST /v1/payments/:payment_id/capture",
				"endpoint|payments-py|GET /v1/payments/:payment_id", "endpoint|payments-py|POST /v1/payments/webhook",
				"endpoint|payments-py|PUT /v1/payments/webhook", "dependency|payments-py|http · api.iyzipay.com",
				"dependency|payments-py|http · api.stripe.com",
				// Flask + Blueprint url_prefix
				"endpoint|flasksvc|POST /login", "endpoint|flasksvc|GET /accounts/:account_id", "endpoint|flasksvc|DELETE /accounts/:account_id",
				"dependency|flasksvc|cache · redis",
				// Django include() prefixes, re_path, celery tasks + beat
				"endpoint|shop|ANY /api/orders", "endpoint|shop|ANY /api/orders/:pk/refund", "endpoint|shop|ANY /health",
				"endpoint|shop|ANY /legacy/:code", "consumer|shop|orders.send_receipt|emails", "consumer|shop|charge_subscription|celery",
				"cron|shop|nightly-settlement|0 2 * * *", "cron|shop|every-30s-heartbeat|30.0",
				// k8s CronJob with TROOPER_SERVICE env
				"cron|billing-jobs|invoice-reminders|0 9 * * 1-5",
			},
			absent:    []string{"GET /in-docstring", "GET /v1/payments/commented", "GET /commented"},
			languages: []string{"python"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.repo, func(t *testing.T) {
			res, err := Repo(tt.repo, Options{})
			if err != nil {
				t.Fatal(err)
			}
			got := index(res.Items)
			for _, w := range tt.want {
				parts := strings.Split(w, "|")
				it, ok := got[strings.Join(parts[:3], "|")]
				if !ok {
					t.Errorf("missing %s", w)
					continue
				}
				if len(parts) == 4 && it.Source != parts[3] {
					t.Errorf("%s: source %q", w, it.Source)
				}
				if it.File == "" || it.Line == 0 || it.CodeHash == "" {
					t.Errorf("%s: file/line/hash not set: %+v", w, it)
				}
			}
			for _, it := range res.Items {
				for _, a := range tt.absent {
					if it.Key == a {
						t.Errorf("unexpected %s %s (%s:%d)", it.Kind, it.Key, it.File, it.Line)
					}
				}
			}
			if !slices.Equal(res.Languages, tt.languages) {
				t.Errorf("languages %v", res.Languages)
			}
		})
	}
}

func TestHandlerSnippetsAndCallees(t *testing.T) {
	res, err := Repo("testdata/gorepo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	it := index(res.Items)["endpoint|orders-api|POST /v1/orders"]
	if !strings.Contains(it.Snippet, "func createOrder") || it.Handler != "createOrder" {
		t.Fatalf("snippet %q handler %q", it.Snippet, it.Handler)
	}
	if !slices.ContainsFunc(it.Callees, func(s string) bool { return strings.HasPrefix(s, "func saveOrder(db *sql.DB) error") }) {
		t.Fatalf("callees %v", it.Callees)
	}
	js, _ := Repo("testdata/jsrepo", Options{})
	if c := index(js.Items)["endpoint|checkout-api|POST /v1/checkout"]; !strings.Contains(c.Snippet, "async function createCheckout") {
		t.Fatalf("js snippet %q", c.Snippet)
	}
}

func TestIncrementalOnly(t *testing.T) {
	res, err := Repo("testdata/jsrepo", Options{Only: map[string]bool{"api/src/routes/orders.ts": true}})
	if err != nil {
		t.Fatal(err)
	}
	keys := []string{}
	for _, it := range res.Items {
		if it.File != "api/src/routes/orders.ts" {
			t.Fatalf("item from another file: %+v", it)
		}
		keys = append(keys, it.Key)
	}
	// the mount prefix from index.ts still applies although index.ts is not in the set
	if !slices.Contains(keys, "GET /v1/orders/:id") || !slices.Contains(keys, "DELETE /v1/orders/:id") {
		t.Fatalf("keys %v", keys)
	}
}

func TestServiceMap(t *testing.T) {
	files := []srcFile{
		{rel: ".trooper/config.yaml", lang: "", src: []byte("services:\n  - path: apps/api\n    name: api\n  - path: apps/api/admin\n    name: admin-api\n")},
		{rel: "apps/api/package.json", lang: "manifest", src: []byte(`{}`)},
		{rel: "svc/orders/go.mod", lang: "manifest", src: []byte("module x")},
		{rel: "svc/orders/Dockerfile", lang: "config", src: []byte("FROM x\nENV TROOPER_SERVICE=orders-svc\n")},
		{rel: "svc/billing/go.mod", lang: "manifest", src: []byte("module y")},
		{rel: "svc/notrooper/main.go", lang: "go", src: []byte(`package main; var cfg = Config{Service: "nope"}`)},
	}
	m := newServiceMap(files, "monorepo")
	tests := map[string]string{
		"apps/api/src/index.ts":      "api",
		"apps/api/admin/x.ts":        "admin-api",
		"svc/orders/cmd/api/main.go": "orders-svc",
		"svc/billing/internal/h.go":  "billing",
		"svc/notrooper/main.go":      "monorepo",
		"scripts/tool.py":            "monorepo",
	}
	for rel, want := range tests {
		if got := m.service(rel); got != want {
			t.Errorf("service(%s) = %s, want %s", rel, got, want)
		}
	}
}

func TestTokenizer(t *testing.T) {
	toks := tokenizeJS([]byte("a = /re\\/x/g; // c\nb = `t${x}` + 'it\\'s' /* d\n */ c => 1"))
	var vals []string
	for _, tk := range toks {
		vals = append(vals, tk.val)
	}
	got := strings.Join(vals, " ")
	want := "a =  ; b = t${x} + it's c => 1"
	if got != want {
		t.Fatalf("got %q want %q", got, want)
	}
	if toks[len(toks)-1].line != 3 {
		t.Fatalf("line %d", toks[len(toks)-1].line)
	}
}
