package route

import "testing"

func TestNormalize(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/orders/{id}", "/orders/:id"},
		{"/files/{path...}", "/files/:path"},
		{"/{$}", "/"},
		{"/users/{id:[0-9]+}", "/users/:id"},
		{"/users/<int:user_id>/", "/users/:user_id"},
		{"/posts/<slug>", "/posts/:slug"},
		{"/blog/[slug]", "/blog/:slug"},
		{"/docs/[...parts]", "/docs/:parts"},
		{"/docs/[[...parts]]", "/docs/:parts"},
		{"/a/:id?", "/a/:id"},
		{`/a/:id(\d+)`, "/a/:id"},
		{"/static/*filepath", "/static/:filepath"},
		{"/static/*", "/static/:wildcard"},
		{"//a///b/", "/a/b"},
		{"/x?y=1", "/x"},
	}
	for _, tt := range tests {
		if got := Normalize(tt.in); got != tt.want {
			t.Errorf("Normalize(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestCanonMatchesWriterRoutes(t *testing.T) {
	tests := []struct{ code, observed string }{
		{"/orders/:id", "/orders/{id}"},     // Go 1.22 http.route
		{"/orders/:orderId", "/orders/:id"}, // writer normalization of a raw path
		{"/v1/users/{uid}/cart", "/v1/users/550e8400-e29b-41d4-a716-446655440000/cart"},
		{"/Health", "/health"},
	}
	for _, tt := range tests {
		if a, b := Canon(tt.code), Canon(tt.observed); a != b {
			t.Errorf("Canon(%q)=%q != Canon(%q)=%q", tt.code, a, tt.observed, b)
		}
	}
	if Canon("/orders/:id") == Canon("/orders/new") {
		t.Error("static segment must not match a parameter")
	}
}

func TestKeyAndMethods(t *testing.T) {
	if k := Key("get", "/a/{id}"); k != "GET /a/:id" {
		t.Fatal(k)
	}
	if k := Key("", "/a"); k != "ANY /a" {
		t.Fatal(k)
	}
	if m, p := SplitKey("POST /x"); m != "POST" || p != "/x" {
		t.Fatal(m, p)
	}
	if !MethodMatches("ANY", "POST") || MethodMatches("GET", "POST") || !MethodMatches("get", "GET") {
		t.Fatal("method matching")
	}
	if Join("/api/", "/orders") != "/api/orders" || Join("", "/x") != "/x" || Join("/api", "/") != "/api" {
		t.Fatal("join")
	}
}

func TestFromRegex(t *testing.T) {
	tests := []struct{ in, want string }{
		{`^articles/(?P<year>[0-9]{4})/$`, "/articles/:year"},
		{`^users/(?P<pk>\d+)/orders/(\d+)/$`, "/users/:pk/orders/:param"},
		{`^health/?$`, "/health"},
	}
	for _, tt := range tests {
		if got := FromRegex(tt.in); got != tt.want {
			t.Errorf("FromRegex(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
