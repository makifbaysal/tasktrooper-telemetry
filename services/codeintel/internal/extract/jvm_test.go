package extract

import (
	"slices"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func TestJVMFixture(t *testing.T) {
	res, err := Repo("testdata/jvmrepo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{ // kind|service|key[|source]
		// Spring MVC: class prefix, no-arg mapping, arrays, constants, RequestMapping methods / ANY
		"endpoint|orders-service|GET /api/v1/orders", "endpoint|orders-service|GET /api/v1/orders/:id",
		"endpoint|orders-service|POST /api/v1/orders", "endpoint|orders-service|POST /api/v1/orders/bulk",
		"endpoint|orders-service|PUT /api/v1/orders/:orderId/items", "endpoint|orders-service|PATCH /api/v1/orders/:orderId/items",
		"endpoint|orders-service|ANY /api/v1/orders/legacy", "endpoint|orders-service|DELETE /api/v1/orders/:id",
		// WebFlux functional routes
		"endpoint|orders-service|GET /fn/ping", "endpoint|orders-service|POST /fn/echo",
		// spring-kafka (multi-line, constant topic), amqp, jms, sqs, @Scheduled
		"consumer|orders-service|order-projector|orders.created,orders.updated", "consumer|orders-service|emails|emails",
		"consumer|orders-service|invoices|invoices", "consumer|orders-service|refunds-queue|refunds-queue",
		"cron|orders-service|nightlyReport|0 0 2 * * *", "cron|orders-service|heartbeat|every 5000ms",
		"dependency|orders-service|db · postgres", "dependency|orders-service|db · sql", "dependency|orders-service|http · api.stripe.com",
		"dependency|orders-service|queue · kafka", "dependency|orders-service|queue · rabbitmq", "dependency|orders-service|queue · sqs",
		"dependency|orders-service|http · inventory.example.com",
		// Ktor: nested route() prefixes, string templates, Route extension mounted under a prefix in another file
		"endpoint|catalog-ktor|GET /health", "endpoint|catalog-ktor|GET /v1/products", "endpoint|catalog-ktor|GET /v1/products/:id",
		"endpoint|catalog-ktor|POST /v1/products", "endpoint|catalog-ktor|DELETE /v1/products/:id",
		"endpoint|catalog-ktor|GET /v1/reviews/:productId", "endpoint|catalog-ktor|POST /v1/reviews",
		"dependency|catalog-ktor|cache · redis",
		// Kotlin Spring: companion constant prefix, array value, Kotlin array topics, timeUnit, coRouter nest
		"endpoint|billing-kotlin|GET /billing/invoices", "endpoint|billing-kotlin|GET /billing/bills",
		"endpoint|billing-kotlin|POST /billing/invoices/:id/pay", "consumer|billing-kotlin|billing-ledger|ledger.entries",
		"cron|billing-kotlin|sweep|every 30s", "endpoint|billing-kotlin|GET /kfn/status", "endpoint|billing-kotlin|POST /kfn-top",
		"dependency|billing-kotlin|http · api.paypal.com",
		// JAX-RS / Quarkus
		"endpoint|inventory-quarkus|GET /stock", "endpoint|inventory-quarkus|GET /stock/:sku", "endpoint|inventory-quarkus|POST /stock/:sku/reserve",
		"consumer|inventory-quarkus|stock-updates|stock-updates", "cron|inventory-quarkus|refresh|every 10s",
		"cron|inventory-quarkus|cleanup|0 15 10 * * ?", "dependency|inventory-quarkus|db · sql",
		// Micronaut
		"endpoint|shipping-micronaut|GET /shipments/:id", "endpoint|shipping-micronaut|POST /shipments",
		"consumer|shipping-micronaut|shipping-tracker|tracking.events",
		// Android: Activities/Fragments (through a repo base class), Compose Navigation
		"view|shop-android|MainActivity", "view|shop-android|SettingsActivity", "view|shop-android|ProfileFragment",
		"view|shop-android|LoginActivity", "view|shop-android|home", "view|shop-android|product/:productId",
		"view|shop-android|cart", "view|shop-android|CheckoutRoute", "dependency|shop-android|http · api.shop.example.com",
	}
	absent := []string{
		"GET /api/v1/orders/commented", "DELETE /api/v1/orders/block-commented", "GET /api/v1/orders/in-string", "GET /from-test",
		"GET /stock/:sku/sub", "ANY /stock/sub", "GET /stock/sub", "GET /users/:id", "GET /commented", "GET /in-string",
		"GET /billing/block-commented", "BaseActivity", "commented",
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
		if it.File == "" || it.Line == 0 || it.CodeHash == "" || (it.Language != "java" && it.Language != "kotlin") {
			t.Errorf("%s: file/line/hash/language not set: %+v", w, it)
		}
	}
	for _, it := range res.Items {
		for _, a := range absent {
			if it.Key == a {
				t.Errorf("unexpected %s %s (%s:%d)", it.Kind, it.Key, it.File, it.Line)
			}
		}
	}
	if !slices.Equal(res.Languages, []string{"java", "kotlin"}) {
		t.Errorf("languages %v", res.Languages)
	}
	ep := got["endpoint|orders-service|POST /api/v1/orders"]
	if ep.Framework != "spring" || ep.Handler != "OrderController.create" || !strings.Contains(ep.Snippet, "postForObject") ||
		!strings.Contains(ep.Snippet, "@PostMapping") {
		t.Errorf("spring endpoint %+v", ep)
	}
	if fw := got["endpoint|inventory-quarkus|GET /stock/:sku"].Framework; fw != "jaxrs" {
		t.Errorf("jaxrs framework %q", fw)
	}
	if fw := got["cron|inventory-quarkus|refresh"].Framework; fw != "quarkus" {
		t.Errorf("quarkus cron framework %q", fw)
	}
	if it := got["endpoint|orders-service|GET /api/v1/orders/:id"]; it.Line != 20 {
		t.Errorf("line %d", it.Line)
	}
}

func jvmItems(t *testing.T, lang, src string) map[string]model.Item {
	t.Helper()
	ext := map[string]string{"java": ".java", "kotlin": ".kt"}[lang]
	items := extractJVM([]srcFile{{rel: "src/main/X" + ext, lang: lang, src: []byte(src)}})
	m := map[string]model.Item{}
	for _, it := range items {
		m[it.Kind+"|"+it.Key] = it
	}
	return m
}

func TestJVMParsing(t *testing.T) {
	tests := []struct {
		name, lang, src string
		want            []string // kind|key[|source]
		absent          []string
	}{
		{
			name: "kotlin arrays and constants",
			lang: "kotlin",
			src: `import org.springframework.web.bind.annotation.*
object Paths { const val ROOT = "/v2"; const val ITEM = "$ROOT/items/{id}" }
@RestController
@RequestMapping(path = [Paths.ROOT + "/shop"])
class Shop {
    @RequestMapping(value = ["/a", "/b"], method = [RequestMethod.GET, RequestMethod.HEAD])
    fun ab() = "x"
    @PutMapping(Paths.ITEM)
    fun put(@RequestBody body: String, @RequestParam("q") q: String) {}
    @GetMapping("/one", "/two")
    fun vararg() {}
    @GetMapping(UNKNOWN_CONST)
    fun unresolved() {}
}`,
			want: []string{"endpoint|GET /v2/shop/a", "endpoint|HEAD /v2/shop/b", "endpoint|PUT /v2/shop/v2/items/:id",
				"endpoint|GET /v2/shop/one", "endpoint|GET /v2/shop/two"},
			absent: []string{"endpoint|GET /v2/shop", "endpoint|GET /v2/shop/UNKNOWN_CONST"},
		},
		{
			name: "java constants, nested annotations and comments",
			lang: "java",
			src: `import org.springframework.amqp.rabbit.annotation.*;
import org.springframework.kafka.annotation.KafkaListener;
public class L {
    public static final String BASE = "events";
    static final String TOPIC = BASE + ".created";
    @RabbitListener(id = "mailer", bindings = @QueueBinding(value = @Queue("mail.q"), exchange = @Exchange("mail")))
    public void mail(String m) {}
    @KafkaListener(topics = TOPIC) // groupId = "not-this"
    public void created(String m) {}
    /*
    @KafkaListener(topics = "commented.topic", groupId = "x")
    public void old(String m) {}
    */
    String s = "@KafkaListener(topics = \"in.string\", groupId = \"y\")";
    char c = '"';
    @KafkaListener(topics = "after.char", groupId = "g2")
    public void afterChar(String m) {}
}`,
			want:   []string{"consumer|mailer|mail.q", "consumer|events.created|events.created", "consumer|g2|after.char"},
			absent: []string{"consumer|x", "consumer|y", "consumer|not-this", "consumer|commented.topic", "consumer|in.string"},
		},
		{
			name: "nested ktor routes",
			lang: "kotlin",
			src: `import io.ktor.server.routing.*
fun Application.api() {
    routing {
        authenticate("jwt") {
            route("/admin") {
                route("/users") {
                    get("/{id?}") { if (x) { call.respond("{") } }
                    put("{id}") { }
                }
                patch { }
            }
        }
        head("/ping") { }
        val m = map.get("/not-a-route") { }
    }
}`,
			want:   []string{"endpoint|GET /admin/users/:id", "endpoint|PUT /admin/users/:id", "endpoint|PATCH /admin", "endpoint|HEAD /ping"},
			absent: []string{"endpoint|GET /not-a-route", "endpoint|PATCH /admin/users"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := jvmItems(t, tt.lang, tt.src)
			for _, w := range tt.want {
				parts := strings.Split(w, "|")
				it, ok := got[parts[0]+"|"+parts[1]]
				if !ok {
					t.Errorf("missing %s (got %v)", w, keysOf(got))
					continue
				}
				if len(parts) == 3 && it.Source != parts[2] {
					t.Errorf("%s: source %q", w, it.Source)
				}
			}
			for _, a := range tt.absent {
				if _, ok := got[a]; ok {
					t.Errorf("unexpected %s", a)
				}
			}
		})
	}
}

func keysOf(m map[string]model.Item) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

func TestCleanJVM(t *testing.T) {
	src := "a /* x\n y */ \"s//t\" // c\n'\\'' \"\"\"q\n\"r\"\"\"\" ${\"x\"}\" b"
	raw, code := cleanJVM([]byte(src))
	if len(raw) != len(src) || len(code) != len(src) || strings.Count(code, "\n") != strings.Count(src, "\n") {
		t.Fatalf("offsets not kept")
	}
	if strings.Contains(raw, "x\n") || !strings.Contains(raw, `"s//t"`) || strings.Contains(raw, "// c") {
		t.Fatalf("raw %q", raw)
	}
	if strings.ContainsAny(code, "strqx") || !strings.HasSuffix(code, " b") {
		t.Fatalf("code %q", code)
	}
}
