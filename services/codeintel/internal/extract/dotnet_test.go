package extract

import (
	"slices"
	"sort"
	"strings"
	"testing"
)

func TestDotnetFixture(t *testing.T) {
	res, err := Repo("testdata/dotnetrepo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		// controllers: [controller]/[action] tokens, Name=, absolute "/" and "~/" templates, AcceptVerbs + const route,
		// route inherited from an abstract base controller with a const prefix
		"endpoint|Orders.Api|GET /api/Orders", "endpoint|Orders.Api|GET /api/Orders/:id", "endpoint|Orders.Api|POST /api/Orders",
		"endpoint|Orders.Api|DELETE /admin/orders/:id", "endpoint|Orders.Api|PUT /api/Orders/Archive",
		"endpoint|Orders.Api|GET /api/Orders/search/:term", "endpoint|Orders.Api|HEAD /api/Orders/search/:term",
		"endpoint|Orders.Api|PATCH /v1/orders/:id/status",
		"endpoint|Orders.Api|GET /api/v2/Invoices/:id", "endpoint|Orders.Api|POST /api/v2/Invoices/:id/Pay",
		"endpoint|Orders.Api|GRPC /Greeter/SayHello",
		"dependency|Orders.Api|db · postgres", "dependency|Orders.Api|http · api.stripe.com",
		"dependency|Orders.Api|http · inventory.internal.example.com", "dependency|Orders.Api|http · status.example.net",
		// minimal APIs: groups, chained groups, MapMethods, extension-method mount in another file, nested group there
		"endpoint|Catalog|GET /healthz", "endpoint|Catalog|GET /api/v1/products/:id", "endpoint|Catalog|POST /api/v1/products",
		"endpoint|Catalog|DELETE /admin/cache/:key", "endpoint|Catalog|GET /ping", "endpoint|Catalog|HEAD /ping",
		"endpoint|Catalog|GET /ready", "endpoint|Catalog|GET /orders/:id", "endpoint|Catalog|PUT /orders/:orderId/items/:itemId",
		"dependency|Catalog|db · sqlserver", "dependency|Catalog|cache · redis",
		// worker: MassTransit, Confluent.Kafka (group), RabbitMQ, Service Bus, SQS-reading BackgroundService
		"consumer|Worker|OrderCreatedConsumer|OrderCreated,OrderCancelled",
		"consumer|Worker|billing-workers|payments.captured,payments.failed", "consumer|Worker|emails|emails",
		"consumer|Worker|billing|shipments", "consumer|Worker|returns|returns", "consumer|Worker|SqsPoller|refunds",
		"dependency|Worker|queue · kafka", "dependency|Worker|queue · rabbitmq", "dependency|Worker|queue · azure-servicebus",
		"dependency|Worker|queue · sqs",
		// Hangfire, Quartz, Coravel
		"cron|Worker|nightly-invoices|0 0 * * *", "cron|Worker|cleanup|*/5 * * * *", "cron|Worker|weekly-report|0 17 * * 5",
		"cron|Worker|Reindexer.Reindex|15 * * * *", "cron|Worker|SendEmailJob|0 0/5 * * * ?", "cron|Worker|purge-trigger|0 0 3 * * ?",
		"cron|Worker|SendDailyReport|30 8 * * *", "cron|Worker|PruneSessions|0 */6 * * *",
	}
	absent := []string{
		"GET /api/Orders/commented", "GET /api/Orders/block-commented", "GET /api/Orders/in-string", "GET /api/Orders/doc-comment",
		"GET /api/Orders/non-action", "ANY /api/Orders/NotAnAction", "GET /fake/from-tests", "GET /verbatim-string", "GET /raw-string",
		"GET /block-commented", "GET /line-commented", "GET /in-interpolation", "consumer|Worker|HeartbeatService", "k",
		"GRPC /Greeter/ToString", "http · docs.example.org", "http · in-verbatim.example.com", "http · sqs.eu-west-1.amazonaws.com",
	}
	got := index(res.Items)
	for _, w := range want {
		parts := strings.Split(w, "|")
		it, ok := got[strings.Join(parts[:3], "|")]
		if !ok {
			t.Errorf("missing %s", w)
			continue
		}
		if len(parts) == 4 && it.Source != parts[3] {
			t.Errorf("%s: source %q", w, it.Source)
		}
		if it.File == "" || it.Line == 0 || it.CodeHash == "" || it.Language != "csharp" {
			t.Errorf("%s: file/line/hash/language not set: %+v", w, it)
		}
		if it.Kind != "dependency" && it.Snippet == "" {
			t.Errorf("%s: no snippet", w)
		}
	}
	for _, it := range res.Items {
		for _, a := range absent {
			if it.Key == a || it.Kind+"|"+it.Service+"|"+it.Key == a {
				t.Errorf("unexpected %s %s (%s:%d)", it.Kind, it.Key, it.File, it.Line)
			}
		}
	}
	if !slices.Equal(res.Languages, []string{"csharp"}) {
		t.Errorf("languages %v", res.Languages)
	}
	if !slices.Equal(res.Services, []string{"Catalog", "Orders.Api", "Worker"}) {
		t.Errorf("services %v", res.Services)
	}
	get := got["endpoint|Orders.Api|GET /api/Orders/:id"]
	if get.Handler != "OrdersController.Get" || get.Framework != "aspnetcore" || !strings.Contains(get.Snippet, "public IActionResult Get(Guid id)") ||
		!strings.Contains(get.Snippet, `[HttpGet("{id:guid}"`) {
		t.Errorf("controller item %+v", get)
	}
	prod := got["endpoint|Catalog|GET /api/v1/products/:id"]
	if prod.Handler != "GetProduct" || !strings.Contains(prod.Snippet, "static IResult GetProduct") || prod.Framework != "aspnetcore-minimal" {
		t.Errorf("method-group handler %+v", prod)
	}
	if p := got["endpoint|Catalog|POST /api/v1/products"]; !strings.Contains(p.Snippet, "SaveChangesAsync") {
		t.Errorf("lambda snippet %q", p.Snippet)
	}
}

// csItems runs the extractor over in-memory C# files ("path", "source", …) and returns sorted kind|key|source rows.
func csItems(t *testing.T, kv ...string) []string {
	t.Helper()
	var files []srcFile
	for i := 0; i+1 < len(kv); i += 2 {
		files = append(files, srcFile{rel: kv[i], lang: "csharp", src: []byte(kv[i+1])})
	}
	var out []string
	for _, it := range extractDotnet(files) {
		if it.Kind == "dependency" {
			continue
		}
		row := it.Kind + "|" + it.Key
		if it.Source != "" {
			row += "|" + it.Source
		}
		out = append(out, row)
	}
	sort.Strings(out)
	return out
}

func TestDotnetParsing(t *testing.T) {
	tests := []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name: "controller and action tokens, area, ActionName, absolute templates",
			files: []string{"A.cs", `
[Area("Billing")]
[Route("[area]/[Controller]")]
public class PaymentsController : Controller
{
    [HttpGet("[action]/{id?}")] public IActionResult DetailsAsync(int? id) => View();
    [HttpPost, ActionName("charge")] [Route("[action]")] public IActionResult Pay() => Ok();
    [HttpGet("/abs/{id:guid}")] public IActionResult Abs(Guid id) => Ok();
    [HttpGet("~/tilde")] public IActionResult Tilde() => Ok();
    private IActionResult Hidden() => Ok();
    public override void Dispose() { }
}`},
			want: []string{"endpoint|GET /Billing/Payments/Details/:id", "endpoint|GET /abs/:id", "endpoint|GET /tilde",
				"endpoint|POST /Billing/Payments/charge"},
		},
		{
			name: "multiple class routes and Http attrs with and without templates",
			files: []string{"B.cs", `
[ApiController]
[Route("api/users")]
[Route("api/v2/users")]
public class UsersController : ControllerBase
{
    [HttpGet] [HttpHead] public IActionResult List() => Ok();
    [HttpGet("{id}")] [HttpGet("by-id/{id}")] public IActionResult One(int id) => Ok();
    public IActionResult NoRoute() => Ok();
}`},
			want: []string{"endpoint|GET /api/users", "endpoint|GET /api/users/:id", "endpoint|GET /api/users/by-id/:id",
				"endpoint|GET /api/v2/users", "endpoint|GET /api/v2/users/:id", "endpoint|GET /api/v2/users/by-id/:id",
				"endpoint|HEAD /api/users", "endpoint|HEAD /api/v2/users"},
		},
		{
			name: "conventional routing when MapControllerRoute is used",
			files: []string{"Program.cs", `app.MapControllerRoute("default", "{controller=Home}/{action=Index}/{id?}");`,
				"HomeController.cs", `public class HomeController : Controller
{
    public IActionResult Index() => View();
    [HttpPost] public IActionResult Contact(Form f) => View();
}`},
			want: []string{"endpoint|ANY /Home/Index", "endpoint|POST /Home/Contact"},
		},
		{
			name: "nested MapGroup chains, variables and generic mounts",
			files: []string{"Program.cs", `
const string Base = "/api";
var api = app.MapGroup(Base);
var v1 = api.MapGroup("v1").WithOpenApi();
var admin = v1.MapGroup("/admin")
              .RequireAuthorization();
admin.MapPatch("/users/{id:int}", (int id) => Results.Ok());
app.MapGroup("/a").MapGroup("/b").WithTags("t").MapGet("/c", () => "x");
api.MapGroup("/todos").MapTodos();
app.MapGet(Base + "/concat", () => 1);
mapper.Map(source, target);
`, "Todos.cs", `
public static class Todos
{
    public static IEndpointRouteBuilder MapTodos(this IEndpointRouteBuilder routes)
    {
        routes.MapGet("/", () => "all");
        routes.MapDelete("{id}", (int id) => Results.NoContent());
        return routes;
    }
}`},
			want: []string{"endpoint|DELETE /api/todos/:id", "endpoint|GET /a/b/c", "endpoint|GET /api/concat", "endpoint|GET /api/todos",
				"endpoint|PATCH /api/v1/admin/users/:id"},
		},
		{
			name: "verbatim, raw, interpolated strings and comments do not register routes",
			files: []string{"P.cs", `
var a = @"app.MapGet(""/v1"", h);";
var b = """
  app.MapGet("/raw", h);
  """;
var c = $"x {app.MapGet("/interp", h)} y";
var d = '"'; app.MapGet(@"/verbatim/{id}", h);
// app.MapGet("/line", h);
/* app.MapGet("/block", h); */
#if NEVER_DIRECTIVE_IS_BLANKED
#endif
app.MapPost($"/interp/{nameof(d)}", h);
`},
			want: []string{"endpoint|GET /verbatim/:id", "endpoint|POST /interp/d"},
		},
		{
			name: "consumers: kafka const topic + config group, NServiceBus handler, plain BackgroundService skipped",
			files: []string{"W.cs", `
using Confluent.Kafka;
public static class Topics { public const string Orders = "orders.v1"; }
public class OrdersReader : BackgroundService
{
    protected override Task ExecuteAsync(CancellationToken ct)
    {
        var cfg = new ConsumerConfig { GroupId = "order-readers" };
        consumer.Subscribe(Topics.Orders);
        return Task.CompletedTask;
    }
}
public class Ticker : BackgroundService { protected override Task ExecuteAsync(CancellationToken ct) => Task.CompletedTask; }
public class PlaceOrderHandler : IHandleMessages<PlaceOrder> { public Task Handle(PlaceOrder m, IMessageHandlerContext c) => Task.CompletedTask; }
public class Streams : BackgroundService
{
    protected override async Task ExecuteAsync(CancellationToken ct) { await db.StreamReadGroupAsync("events", "g", ">"); }
}`},
			want: []string{"consumer|PlaceOrderHandler|PlaceOrder", "consumer|Streams|events", "consumer|order-readers|orders.v1"},
		},
		{
			name: "hangfire Cron helpers and default job ids; ConcurrentDictionary.AddOrUpdate ignored",
			files: []string{"J.cs", `
using Hangfire;
RecurringJob.AddOrUpdate("m", () => A.Run(), Cron.Minutely);
RecurringJob.AddOrUpdate("d", () => A.Run(), Cron.Daily(3, 30));
RecurringJob.AddOrUpdate("mo", () => A.Run(), Cron.Monthly());
RecurringJob.AddOrUpdate("q", "critical", () => A.Run(), "0 * * * *");
RecurringJob.AddOrUpdate<Mailer>(m => m.Flush(), Cron.Weekly());
dict.AddOrUpdate("key", 1, (k, v) => v + 1);
`},
			want: []string{"cron|Mailer.Flush|0 0 * * 1", "cron|d|30 3 * * *", "cron|m|* * * * *", "cron|mo|0 0 1 * *", "cron|q|0 * * * *"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := csItems(t, tt.files...)
			want := slices.Clone(tt.want)
			sort.Strings(want)
			if !slices.Equal(got, want) {
				t.Errorf("got\n  %s\nwant\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

func TestCleanCSharp(t *testing.T) {
	src := "var a = @\"x\"\"y\"; // c \"q\"\nvar b = \"\"\"\n    raw \"q\"\n      in\n    \"\"\";\nvar c = $\"h{(ok ? \"t\" : \"f\")}{{z}}\"; var d = '\\''; var e = \"esc\\\"aped\";\n/* multi\nline */ end"
	code, strs := cleanCSharp([]byte(src))
	if len(code) != len(src) || strings.Count(string(code), "\n") != strings.Count(src, "\n") {
		t.Fatalf("offsets not preserved")
	}
	var vals []string
	offs := make([]int, 0, len(strs))
	for off := range strs {
		offs = append(offs, off)
	}
	sort.Ints(offs)
	for _, off := range offs {
		vals = append(vals, strs[off])
	}
	want := []string{`x"y`, "raw \"q\"\n  in", `h{(ok ? "t" : "f")}{z}`, `esc"aped`}
	if !slices.Equal(vals, want) {
		t.Fatalf("values %q", vals)
	}
	for _, leak := range []string{"raw", "esc", "multi", "// c", "'\\''"} {
		if strings.Contains(string(code), leak) {
			t.Errorf("%q not blanked in %q", leak, code)
		}
	}
	if !strings.Contains(string(code), "end") || !strings.Contains(string(code), "var e =") {
		t.Errorf("code lost: %q", code)
	}
}
