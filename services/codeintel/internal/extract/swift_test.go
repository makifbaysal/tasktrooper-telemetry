package extract

import (
	"slices"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

// checkFixture asserts want ("kind|service|key") items exist with file/line/hash and absent keys do not.
func checkFixture(t *testing.T, res *Result, want, absent []string) map[string]model.Item {
	t.Helper()
	got := index(res.Items)
	for _, w := range want {
		it, ok := got[w]
		if !ok {
			t.Errorf("missing %s", w)
			continue
		}
		if it.File == "" || it.Line == 0 || it.CodeHash == "" {
			t.Errorf("%s: file/line/hash not set: %+v", w, it)
		}
	}
	for _, it := range res.Items {
		if slices.Contains(absent, it.Key) {
			t.Errorf("unexpected %s %s (%s:%d)", it.Kind, it.Key, it.File, it.Line)
		}
	}
	return got
}

func TestSwiftFixture(t *testing.T) {
	res, err := Repo("testdata/iosrepo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := checkFixture(t, res, []string{
		// SwiftUI: *Screen by name; others because they are navigation targets / container children
		"view|shop-ios|HomeScreen", "view|shop-ios|SettingsScreen", "view|shop-ios|RootTabView", "view|shop-ios|OrdersView",
		"view|shop-ios|OrderDetailView", "view|shop-ios|FiltersSheet", "view|shop-ios|ProfileView",
		// UIKit, including an in-repo base class subclass
		"view|shop-ios|BaseViewController", "view|shop-ios|ProductViewController", "view|shop-ios|LegacyCheckoutViewController",
		"dependency|shop-ios|http · api.shop.example.com", "dependency|shop-ios|db · firestore",
		// Vapor: grouped vars, group closures, on(.POST), RouteCollection mounted under a group
		"endpoint|orders-server|GET /healthz", "endpoint|orders-server|GET /api/v1/orders/:id",
		"endpoint|orders-server|POST /api/v1/orders", "endpoint|orders-server|DELETE /admin/users/:userID",
		"endpoint|orders-server|GET /api/v1/todos", "endpoint|orders-server|POST /api/v1/todos",
		"endpoint|orders-server|DELETE /api/v1/todos/:todoID", "dependency|orders-server|db · postgres",
	}, []string{
		"OrderRow", "PromoBanner", "ShopApp", "Order", "MainNavigationController", "PriceFormatter", // subviews, App, containers
		"CommentedScreen", "AlsoCommentedScreen", "TemplateScreen", "TemplateTarget", "TestOnlyScreen",
		"http · commented.example.com", "GET /commented", "GET /in-string",
	})
	if r := got["view|shop-ios|HomeScreen"].Meta["rumName"]; r != "Home" {
		t.Errorf("HomeScreen rumName %v", r)
	}
	if r := got["view|shop-ios|ProductViewController"].Meta["rumName"]; r != "ProductDetail" {
		t.Errorf("ProductViewController rumName %v", r)
	}
	if it := got["endpoint|orders-server|GET /api/v1/orders/:id"]; it.Handler != "getOrder" || !strings.Contains(it.Snippet, "func getOrder") {
		t.Errorf("handler %q snippet %q", it.Handler, it.Snippet)
	}
	if it := got["view|shop-ios|OrdersView"]; it.Framework != "swiftui" || it.Language != "swift" || !strings.Contains(it.Snippet, "navigationDestination") {
		t.Errorf("OrdersView %+v", it)
	}
	if fw := got["view|shop-ios|LegacyCheckoutViewController"].Framework; fw != "uikit" {
		t.Errorf("framework %q", fw)
	}
	if !slices.Equal(res.Languages, []string{"swift"}) {
		t.Errorf("languages %v", res.Languages)
	}
}

func TestVaporGroups(t *testing.T) {
	files := []srcFile{
		{rel: "Sources/App/configure.swift", lang: "swift", src: []byte(`import Vapor
func routes(_ app: Application) throws {
    let v2 = app.grouped("v2").grouped(UserAuthenticator()).grouped("shop")
    try v2.register(collection: CartController())
    v2.get("**") { _ in "" }
    v2.on(.PATCH, "carts", .parameter("cartID"), use: patchCart)
    app.routes.get("x") { _ in "" } // receiver is not a builder: skipped
    let notRoutes = [String: String]()
    _ = notRoutes.get("y")
}`)},
		{rel: "Sources/App/CartController.swift", lang: "swift", src: []byte(`import Vapor
struct CartController: RouteCollection {
    func boot(routes r: RoutesBuilder) throws {
        r.group("carts", ":id") { cart in
            cart.get(use: show)
            cart.webSocket("live") { req, ws in }
        }
    }
}`)},
		{rel: "Sources/Other/NoVapor.swift", lang: "swift", src: []byte(`func f(app: Application) { app.get("ignored") }`)},
	}
	var keys []string
	for _, it := range extractSwift(files) {
		if it.Kind == model.KindEndpoint {
			keys = append(keys, it.Key)
		}
	}
	slices.Sort(keys)
	want := []string{"GET /v2/shop/:wildcard", "GET /v2/shop/carts/:id", "GET /v2/shop/carts/:id/live", "PATCH /v2/shop/carts/:cartID"}
	if !slices.Equal(keys, want) {
		t.Fatalf("keys %v, want %v", keys, want)
	}
}

func TestCleanSwift(t *testing.T) {
	src := "let a = \"x // not a comment\" // comment\n/* outer /* inner */ still */ let b = #\"raw \"q\" \"# + \"\\(f(\"(\"))\"\nlet c = \"\"\"\n  struct S: View {}\n  \"\"\"\n"
	m := cleanSwift(srcFile{rel: "a.swift", lang: "swift", src: []byte(src)})
	if len(m.code) != len(src) || strings.Count(string(m.code), "\n") != strings.Count(src, "\n") {
		t.Fatal("offsets not preserved")
	}
	code := string(m.code)
	for _, bad := range []string{"comment", "outer", "inner", "still", "raw", "struct S"} {
		if strings.Contains(code, bad) {
			t.Errorf("code view contains %q: %q", bad, code)
		}
	}
	if !strings.Contains(code, "let b") || !strings.Contains(code, "let c") {
		t.Errorf("code lost: %q", code)
	}
	var vals []string
	for _, l := range m.literals() {
		vals = append(vals, l.val)
	}
	if len(vals) != 4 || vals[0] != "x // not a comment" || vals[1] != `raw "q" ` || !strings.Contains(vals[3], "struct S: View {}") {
		t.Errorf("literals %q", vals)
	}
}
