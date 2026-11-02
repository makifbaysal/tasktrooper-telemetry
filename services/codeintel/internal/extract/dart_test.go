package extract

import (
	"slices"
	"strings"
	"testing"

	"github.com/tasktrooper/telemetry/services/codeintel/internal/model"
)

func TestDartFixture(t *testing.T) {
	res, err := Repo("testdata/flutterrepo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := checkFixture(t, res, []string{
		// go_router: nested relative paths join the parent, ShellRoute is transparent
		"view|shop_app|/", "view|shop_app|/products/:id", "view|shop_app|/products/:id/reviews", "view|shop_app|/orders/:orderId",
		// a screen no router builds
		"view|shop_app|SettingsScreen",
		"dependency|shop_app|http · api.shop.example.com", "dependency|shop_app|http · status.example.io",
		"dependency|shop_app|http · search.example.net", "dependency|shop_app|db · sqlite",
		// MaterialApp routes map (incl. a static const key), onGenerateRoute case, unrouted page
		"view|admin_app|/", "view|admin_app|/users", "view|admin_app|/reports", "view|admin_app|/users/edit",
		"view|admin_app|AuditLogPage", "dependency|admin_app|db · firestore",
	}, []string{
		// built by a route (the route is the item), not screens, generated / test files, comments and strings
		"HomePage", "ProductPage", "ReviewsScreen", "OrderDetailScreen", "DashboardPage", "UsersPage", "ReportsScreen",
		"EditUserPage", "ProductCard", "ScaffoldWithNav", "_SettingsScreenState", "AdminApp", "GeneratedScreen", "FakeScreen",
		"/commented", "/block-commented", "/doc-comment", "/in-string", "http · legacy.example.com",
		"http · in-triple-string.example.com",
	})
	if it := got["view|shop_app|/products/:id"]; it.Framework != "go_router" || it.Handler != "ProductPage" ||
		it.Meta["rumName"] != "ProductDetail" || !strings.Contains(it.Snippet, "class ProductPage") {
		t.Errorf("product route %+v", it)
	}
	if r := got["view|shop_app|/products/:id/reviews"].Meta["rumName"]; r != "reviews" {
		t.Errorf("reviews rumName %v", r)
	}
	if it := got["view|admin_app|/users"]; it.Handler != "UsersPage" || it.Framework != "flutter-routes" {
		t.Errorf("users route %+v", it)
	}
	if n := got["view|admin_app|/reports"].Meta["navigations"]; n != 1 {
		t.Errorf("reports navigations %v", n)
	}
	if fw := got["view|shop_app|SettingsScreen"].Framework; fw != "flutter" {
		t.Errorf("framework %q", fw)
	}
	if !slices.Equal(res.Languages, []string{"dart"}) {
		t.Errorf("languages %v", res.Languages)
	}
}

func TestGoRouterNested(t *testing.T) {
	src := `import 'package:go_router/go_router.dart';
final r = GoRouter(routes: [
  StatefulShellRoute.indexedStack(branches: [
    StatefulShellBranch(routes: [
      GoRoute(path: '/shop', builder: (c, s) => const ShopScreen(), routes: [
        GoRoute(path: 'cart', name: 'Cart', builder: (c, s) => const CartPage(), routes: [
          GoRoute(path: 'checkout/:step', builder: (c, s) => CheckoutPage(step: s.pathParameters['step']!)),
        ]),
        GoRoute(path: '/absolute', builder: (c, s) => const ShopScreen()),
      ]),
    ]),
  ]),
]);
@TypedGoRoute<HomeRoute>(path: '/home', routes: [TypedGoRoute<DetailsRoute>(path: 'details/:id')])
class HomeRoute extends GoRouteData {}
class ShopScreen extends StatelessWidget {}
class CartPage extends StatelessWidget {}
class CheckoutPage extends StatelessWidget {}
class OrphanPage extends StatelessWidget {}
`
	items := extractDart([]srcFile{{rel: "lib/router.dart", lang: "dart", src: []byte(src)}})
	got := map[string]model.Item{}
	for _, it := range items {
		got[it.Key] = it
	}
	for key, handler := range map[string]string{"/shop": "ShopScreen", "/shop/cart": "CartPage", "/shop/cart/checkout/:step": "CheckoutPage",
		"/absolute": "ShopScreen", "/home": "HomeRoute", "/home/details/:id": "DetailsRoute", "OrphanPage": "OrphanPage"} {
		if it, ok := got[key]; !ok || it.Handler != handler {
			t.Errorf("%s: %+v", key, it)
		}
	}
	if got["/shop/cart"].Meta["rumName"] != "Cart" || got["/shop/cart/checkout/:step"].Meta["rumName"] != "checkout/:step" {
		t.Errorf("rumNames %v %v", got["/shop/cart"].Meta, got["/shop/cart/checkout/:step"].Meta)
	}
	for _, k := range []string{"ShopScreen", "CartPage", "CheckoutPage"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s duplicated as a screen", k)
		}
	}
}

func TestAutoRoute(t *testing.T) {
	files := []srcFile{
		{rel: "lib/app_router.dart", lang: "dart", src: []byte(`import 'package:auto_route/auto_route.dart';
@AutoRouterConfig()
class AppRouter extends RootStackRouter {
  @override
  List<AutoRoute> get routes => [
        AutoRoute(page: HomeRoute.page, path: '/'),
        AutoRoute(page: DashboardRoute.page, path: '/dashboard', children: [
          AutoRoute(page: UsersRoute.page, path: 'users/:id'),
        ]),
        AutoRoute(page: AboutRoute.page),
      ];
}`)},
		{rel: "lib/pages.dart", lang: "dart", src: []byte(`import 'package:auto_route/auto_route.dart';
@RoutePage()
class HomePage extends StatelessWidget {}
@RoutePage()
class DashboardScreen extends StatelessWidget {}
@RoutePage(name: 'UsersRoute')
class UserListView extends StatelessWidget {}
@RoutePage()
class AboutPage extends StatelessWidget {}
@RoutePage()
class OrphanPage extends StatelessWidget {}
`)},
	}
	got := map[string]model.Item{}
	for _, it := range extractDart(files) {
		got[it.Key] = it
	}
	for key, handler := range map[string]string{"/": "HomePage", "/dashboard": "DashboardScreen", "/dashboard/users/:id": "UserListView",
		"AboutRoute": "AboutPage", "OrphanPage": "OrphanPage"} {
		if it, ok := got[key]; !ok || it.Handler != handler || it.Framework != "auto_route" {
			t.Errorf("%s: %+v", key, it)
		}
	}
	if got["/dashboard/users/:id"].Meta["rumName"] != "UsersRoute" || got["OrphanPage"].Meta["rumName"] != "OrphanRoute" {
		t.Errorf("rumNames %v %v", got["/dashboard/users/:id"].Meta, got["OrphanPage"].Meta)
	}
	if len(got) != 5 {
		t.Errorf("items %v", got)
	}
}

func TestCleanDart(t *testing.T) {
	src := "var a = 'it\\'s // x'; // c\n/// doc GoRoute(\n/* a /* b */ c */ var b = r'raw\\' + \"${f('}')}\";\nvar c = '''\nGoRoute(path: '/x')\n''';\n"
	m := cleanDart(srcFile{rel: "a.dart", lang: "dart", src: []byte(src)})
	if len(m.code) != len(src) || strings.Count(string(m.code), "\n") != strings.Count(src, "\n") {
		t.Fatal("offsets not preserved")
	}
	code := string(m.code)
	for _, bad := range []string{"// x", "doc", "GoRoute", "c */", "raw"} {
		if strings.Contains(code, bad) {
			t.Errorf("code view contains %q: %q", bad, code)
		}
	}
	if !strings.Contains(code, "var b") || !strings.Contains(code, "var c") {
		t.Errorf("code lost: %q", code)
	}
	var vals []string
	for _, l := range m.literals() {
		vals = append(vals, l.val)
	}
	if len(vals) != 4 || vals[0] != `it\'s // x` || vals[1] != `raw\` || !strings.Contains(vals[3], "GoRoute(path: '/x')") {
		t.Errorf("literals %q", vals)
	}
}
