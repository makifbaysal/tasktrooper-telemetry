package norm

import "testing"

func TestRoute(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "/"},
		{"/", "/"},
		{"/health", "/health"},
		{"/v1/orders/123", "/v1/orders/:id"},
		{"/v1/orders/123/items/9", "/v1/orders/:id/items/:id"},
		{"/users/550e8400-e29b-41d4-a716-446655440000", "/users/:id"},
		{"/objects/5f8d0d55b54764421b7156c9", "/objects/:id"},                 // mongo object id (hex)
		{"/commits/a94a8fe5ccb19ba61c4c0873d391e987982fbbd3", "/commits/:id"}, // sha1
		{"/events/01ARZ3NDEKTSV4RRFFQ69G5FAV", "/events/:id"},                 // ULID
		{"/v1/orders/123?expand=items#top", "/v1/orders/:id"},
		{"/a//b/", "/a/b"},
		{"https://api.x.com/v2/carts/77", "/v2/carts/:id"},
		{"/products/red-shoe", "/products/red-shoe"},
		{"/api/v2/feed", "/api/v2/feed"},
		{"/files/deadbeef", "/files/deadbeef"}, // all-letter hex is a word, not an id
		{"/invoices/INV-2024-000123", "/invoices/:id"},
		{"/t/aB3dE5fG7hJ9kL1mN3pQ", "/t/:id"}, // opaque token
	}
	for _, tt := range tests {
		if got := Route(tt.in); got != tt.want {
			t.Errorf("Route(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResourceURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://api.shop.com/v1/orders/42?a=1", "https://api.shop.com/v1/orders/:id"},
		{"https://user:pw@api.shop.com/x", "https://api.shop.com/x"},
		{"https://cdn.shop.com", "https://cdn.shop.com"},
		{"/relative/12", "/relative/:id"},
	}
	for _, tt := range tests {
		if got := ResourceURL(tt.in); got != tt.want {
			t.Errorf("ResourceURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestStripQueryValues(t *testing.T) {
	if got := StripQueryValues("https://x/a?b=1&c=two&d#frag"); got != "https://x/a?b&c&d" {
		t.Fatalf("got %q", got)
	}
}

func TestQuery(t *testing.T) {
	tests := []struct{ in, want string }{
		{"SELECT * FROM orders WHERE id = 42", "SELECT * FROM orders WHERE id = ?"},
		{"select a from t where name = 'O''Brien' and x > 3.5e2", "select a from t where name = ? and x > ?"},
		{"SELECT * FROM t WHERE id IN (1, 2, 3)", "SELECT * FROM t WHERE id IN (?)"},
		{"SELECT * FROM t WHERE id IN ($1,$2)", "SELECT * FROM t WHERE id IN ($1,$2)"},
		{"SELECT  *\n  FROM t1   -- comment\n WHERE v = -5", "SELECT * FROM t1 WHERE v = ?"},
		{"UPDATE t SET b = 0xFF /* hint */ WHERE k = 'a'", "UPDATE t SET b = ? WHERE k = ?"},
		{"INSERT INTO t (a, b) VALUES (1, 'x'), (2, 'y')", "INSERT INTO t (a, b) VALUES (?), (?)"},
	}
	for _, tt := range tests {
		if got := Query(tt.in); got != tt.want {
			t.Errorf("Query(%q)\n got %q\nwant %q", tt.in, got, tt.want)
		}
	}
	if QueryHash(Query("SELECT 1 FROM t WHERE a = 5")) != QueryHash(Query("SELECT 1 FROM t WHERE a = 77")) {
		t.Error("query hash should ignore literals")
	}
	if QueryHash("") != 0 {
		t.Error("empty query hash must be 0")
	}
}

func TestClassifyCaller(t *testing.T) {
	tests := []struct{ ua, caller, kind string }{
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 Chrome/128.0 Safari/537.36", "web", "web"},
		{"okhttp/4.12.0", "mobile", "mobile"},
		{"Shop/3 CFNetwork/1490.0.4 Darwin/23.2.0", "mobile", "mobile"},
		{"Dart/3.3 (dart:io)", "mobile", "mobile"},
		{"kube-probe/1.30", "probe", "probe"},
		{"ELB-HealthChecker/2.0", "probe", "probe"},
		{"Stripe/1.0 (+https://stripe.com/docs/webhooks)", "Stripe", "webhook"},
		{"GitHub-Hookshot/abc123", "GitHub", "webhook"},
		{"Shopify-Captain-Hook", "Shopify", "webhook"},
		{"Mozilla/5.0 (compatible; Googlebot/2.1)", "unknown", "unknown"},
		{"curl/8.4.0", "unknown", "unknown"},
		{"", "unknown", "unknown"},
	}
	for _, tt := range tests {
		c, k := ClassifyCaller(tt.ua)
		if c != tt.caller || k != tt.kind {
			t.Errorf("ClassifyCaller(%q) = %q,%q want %q,%q", tt.ua, c, k, tt.caller, tt.kind)
		}
	}
}

func TestParseUA(t *testing.T) {
	tests := []struct {
		ua   string
		want UA
	}{
		{"Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.6613.84 Safari/537.36",
			UA{"Chrome", "128.0", "macOS", "10.15", "desktop"}},
		{"Mozilla/5.0 (iPhone; CPU iPhone OS 17_4 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.4 Mobile/15E148 Safari/604.1",
			UA{"Safari", "17.4", "iOS", "17.4", "mobile"}},
		{"Mozilla/5.0 (Linux; Android 14; Pixel 8) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/127.0.0.0 Mobile Safari/537.36",
			UA{"Chrome", "127.0", "Android", "14", "mobile"}},
		{"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/128.0.0.0 Safari/537.36 Edg/128.0.2739.42",
			UA{"Edge", "128.0", "Windows", "10.0", "desktop"}},
		{"Mozilla/5.0 (iPad; CPU OS 16_6 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/16.6 Mobile/15E148 Safari/604.1",
			UA{"Safari", "16.6", "iOS", "16.6", "tablet"}},
		{"Mozilla/5.0 (X11; Linux x86_64; rv:129.0) Gecko/20100101 Firefox/129.0", UA{"Firefox", "129.0", "Linux", "", "desktop"}},
	}
	for _, tt := range tests {
		if got := ParseUA(tt.ua); got != tt.want {
			t.Errorf("ParseUA(%q)\n got %+v\nwant %+v", tt.ua, got, tt.want)
		}
	}
}

func TestErrorFingerprint(t *testing.T) {
	jsA := "TypeError: x is undefined\n    at render (https://shop.test/main.3f2a1b.js:10:5)\n    at commit (https://shop.test/vendor.js?v=12:200:1)"
	jsB := "TypeError: y is undefined\n    at render (https://shop.test/main.99ff00.js:11:7)\n    at commit (https://shop.test/vendor.js?v=13:201:9)"
	goA := "goroutine 1 [running]:\nmain.handler(0xc000123)\n\t/app/main.go:42 +0x1d"
	goB := "goroutine 7 [running]:\nmain.handler(0xc000999)\n\t/app/main.go:44 +0x2f"
	tests := []struct {
		name string
		a, b [4]string // service, type, message, stack
		same bool
	}{
		{"js frames ignore line/col, bundle hash, message", [4]string{"web", "TypeError", "x is undefined", jsA}, [4]string{"web", "TypeError", "y is undefined", jsB}, true},
		{"go frames ignore addresses and lines", [4]string{"api", "panic", "boom", goA}, [4]string{"api", "panic", "boom", goB}, true},
		{"different type", [4]string{"api", "TypeError", "m", jsA}, [4]string{"api", "RangeError", "m", jsA}, false},
		{"different service", [4]string{"a", "E", "m", ""}, [4]string{"b", "E", "m", ""}, false},
		{"message ids masked", [4]string{"api", "E", "order 42 not found", ""}, [4]string{"api", "E", "order 77 not found", ""}, true},
		{"different message", [4]string{"api", "E", "order not found", ""}, [4]string{"api", "E", "card declined", ""}, false},
	}
	for _, tt := range tests {
		fa := ErrorFingerprint(tt.a[0], tt.a[1], tt.a[2], tt.a[3])
		fb := ErrorFingerprint(tt.b[0], tt.b[1], tt.b[2], tt.b[3])
		if fa == 0 || fb == 0 {
			t.Errorf("%s: fingerprint must be non-zero", tt.name)
		}
		if (fa == fb) != tt.same {
			t.Errorf("%s: same=%v, want %v", tt.name, fa == fb, tt.same)
		}
	}
}

func TestPatternID(t *testing.T) {
	tests := []struct {
		a, b string
		same bool
	}{
		{"order 42 failed after 3 retries", "order 7781 failed after 5 retries", true},
		{"user=alice id=123 logged in", "user=alice id=999 logged in", true},
		{"request 550e8400-e29b-41d4-a716-446655440000 done in 12ms", "request 123e4567-e89b-12d3-a456-426614174000 done in 7ms", true},
		{`cache miss key="user:1"`, `cache miss key="user:2"`, true},
		{"connection refused", "connection reset", false},
		{"payment failed", "order failed", false},
	}
	for _, tt := range tests {
		if (PatternID(tt.a) == PatternID(tt.b)) != tt.same {
			t.Errorf("PatternID(%q) vs (%q): same=%v want %v", tt.a, tt.b, !tt.same, tt.same)
		}
	}
}

func BenchmarkRoute(b *testing.B) {
	for b.Loop() {
		Route("/v1/orders/123/items/550e8400-e29b-41d4-a716-446655440000")
	}
}

func BenchmarkQuery(b *testing.B) {
	q := "SELECT o.id, o.total FROM orders o JOIN users u ON u.id = o.user_id WHERE o.id IN (1,2,3,4) AND u.email = 'a@b.c' LIMIT 50"
	for b.Loop() {
		Query(q)
	}
}

func BenchmarkPatternID(b *testing.B) {
	for b.Loop() {
		PatternID("order 42 for user=alice failed after 3 retries: connection reset by peer 10.0.0.12:5432")
	}
}
