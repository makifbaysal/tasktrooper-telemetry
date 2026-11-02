package extract

import (
	"slices"
	"strings"
	"testing"
)

func TestWebFixture(t *testing.T) {
	res, err := Repo("testdata/webrepo", Options{})
	if err != nil {
		t.Fatal(err)
	}
	got := index(res.Items)
	want := map[string]string{ // kind|service|key → framework ("" for dependencies)
		// vue-router: nested children (relative joined, absolute kept), named children array, param regex, catch-all
		"view|vue-shop|/": "vue-router", "view|vue-shop|/products": "vue-router", "view|vue-shop|/products/:slug": "vue-router",
		"view|vue-shop|/promo/:code": "vue-router", "view|vue-shop|/account": "vue-router",
		"view|vue-shop|/account/addresses/:addressId": "vue-router", "view|vue-shop|/:pathMatch": "vue-router",
		// fetch inside <script setup lang="ts"> of an SFC
		"dependency|vue-shop|http · api.shop.example.com": "",
		// Nuxt file-based routing: index, dynamic segment, route group
		"view|nuxt-store|/": "nuxt", "view|nuxt-store|/orders": "nuxt", "view|nuxt-store|/orders/:id": "nuxt",
		"view|nuxt-store|/about": "nuxt", "dependency|nuxt-store|http · orders.store.example.com": "",
		// Angular router: '' = parent, nested children, lazy loadChildren mounted under its parent, ** wildcard
		"view|ng-admin|/": "angular-router", "view|ng-admin|/orders": "angular-router",
		"view|ng-admin|/orders/:orderId": "angular-router", "view|ng-admin|/orders/:orderId/refund": "angular-router",
		"view|ng-admin|/admin": "angular-router", "view|ng-admin|/admin/users/:userId": "angular-router",
		"view|ng-admin|/:wildcard": "angular-router",
		// HttpClient with a generic type argument
		"dependency|ng-admin|http · api.admin.example.com": "", "dependency|ng-admin|http · payments.admin.example.com": "",
	}
	for k, fw := range want {
		it, ok := got[k]
		if !ok {
			t.Errorf("missing %s", k)
			continue
		}
		if it.Framework != fw {
			t.Errorf("%s: framework %q, want %q", k, it.Framework, fw)
		}
		if it.File == "" || it.Line == 0 || it.CodeHash == "" {
			t.Errorf("%s: file/line/hash not set: %+v", k, it)
		}
	}
	absent := []string{
		"/in-template", "/in-template-comment", "/commented-route", "/breadcrumb-only", "/spec-only", "/spec-route",
		"/home", "/old-cart", "/users/:userId", "http · styles.example.com", "/components/OrderCard",
	}
	for _, it := range res.Items {
		for _, a := range absent {
			if it.Key == a {
				t.Errorf("unexpected %s %s (%s:%d)", it.Kind, it.Key, it.File, it.Line)
			}
		}
		if strings.Contains(it.File, ".spec.") {
			t.Errorf("item from a spec file: %+v", it)
		}
	}
	// SFC items carry the script block language and the original line numbers.
	dep := got["dependency|vue-shop|http · api.shop.example.com"]
	if dep.Language != "typescript" || dep.File != "vue-shop/src/views/Home.vue" || dep.Line != 12 {
		t.Errorf("sfc dependency %+v", dep)
	}
	if v := got["view|nuxt-store|/"]; v.Language != "javascript" {
		t.Errorf("nuxt index language %q", v.Language)
	}
	if v := got["view|ng-admin|/admin/users/:userId"]; v.File != "ng-admin/src/app/admin/admin.routes.ts" || v.Line != 6 {
		t.Errorf("lazy view %+v", v)
	}
	if !slices.Equal(res.Languages, []string{"vue", "typescript"}) {
		t.Errorf("languages %v", res.Languages)
	}
}

func TestVueScript(t *testing.T) {
	src := "<template>\n  <!-- <script>bad()</script> -->\n  <p>x</p>\n</template>\n<script setup lang=\"ts\">\nconst a = 1\n</script>\n<style>p{}</style>\n"
	out, lang := vueScript([]byte(src))
	if lang != "typescript" {
		t.Fatalf("lang %q", lang)
	}
	if want := "\n\n\n\n\nconst a = 1\n\n\n"; string(out) != want {
		t.Fatalf("got %q want %q", out, want)
	}
	if _, lang := vueScript([]byte("<script>\nexport default {}\n</script>")); lang != "javascript" {
		t.Fatalf("js lang %q", lang)
	}
}
