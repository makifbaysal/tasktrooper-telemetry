package sim

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

func expf(x float64) float64 { return math.Exp(x) }

// poisson draws a Poisson-distributed count with mean l.
func poisson(l float64) int {
	if l <= 0 {
		return 0
	}
	if l > 30 {
		return int(math.Max(0, math.Round(l+rand.NormFloat64()*math.Sqrt(l))))
	}
	L, k, p := math.Exp(-l), 0, 1.0
	for {
		p *= rand.Float64()
		if p <= L {
			return k
		}
		k++
	}
}

// ---------------------------------------------------------------- client apps (mirrors frontend.ts / mock.ts)

type apiCall struct {
	svc, method, path string // path may contain {id} {sku} {slug}
	p                 float64
	blocking          bool // the view waits for it (counts into load / LCP)
}

type page struct {
	name     string
	weight   float64
	render   float64 // median client render ms after the data arrived
	errP     float64 // JS error / crash probability per view
	calls    []apiCall
	checkout bool // submit: POST /v1/orders → POST /v1/charges
}

type clientApp struct {
	name, version, platform, sdk, sdkVersion string
	mobile                                   bool
	viewsPerMin                              float64 // mock rpm (page views / min)
	ttfb                                     float64
	pages                                    []page
	devices                                  []device
	coldStart                                float64
}

type device struct {
	typ, manufacturer, model, osName, osVersion, browser, browserVersion, ua string
	w, h                                                                     int
}

var webDevices = []device{
	{"desktop", "Apple", "Macintosh", "macOS", "14.6", "Chrome", "130.0", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10_15_7) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36", 1512, 982},
	{"desktop", "", "PC", "Windows", "11", "Edge", "130.0", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36 Edg/130.0.0.0", 1920, 1080},
	{"mobile", "Apple", "iPhone", "iOS", "18.1", "Safari", "18.1", "Mozilla/5.0 (iPhone; CPU iPhone OS 18_1 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/18.1 Mobile/15E148 Safari/604.1", 393, 852},
	{"mobile", "Samsung", "SM-S921B", "Android", "15", "Chrome", "130.0", "Mozilla/5.0 (Linux; Android 15; SM-S921B) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Mobile Safari/537.36", 412, 915},
	{"desktop", "Apple", "Macintosh", "macOS", "15.0", "Firefox", "131.0", "Mozilla/5.0 (Macintosh; Intel Mac OS X 10.15; rv:131.0) Gecko/20100101 Firefox/131.0", 1440, 900},
}

func mobileDevices(app, ver string) []device {
	return []device{
		{"mobile", "Apple", "iPhone 15", "iOS", "19.0", "", "", app + "/" + ver + " CFNetwork/1568.100.1 Darwin/24.0.0", 393, 852},
		{"mobile", "Google", "Pixel 9", "Android", "16", "", "", "okhttp/4.12.0", 412, 923},
		{"mobile", "Samsung", "SM-A556B", "Android", "15", "", "", "okhttp/4.12.0", 412, 915},
	}
}

func apps() []clientApp {
	bff := func(method, path string, p float64, blocking bool) apiCall {
		return apiCall{"storefront-bff", method, path, p, blocking}
	}
	svc := func(s, method, path string, p float64, blocking bool) apiCall {
		return apiCall{s, method, path, p, blocking}
	}
	shopPages := func(mobile bool) []page {
		n := func(web, scr string) string {
			if mobile {
				return scr
			}
			return web
		}
		return []page{
			{name: n("/", "Home"), weight: 48, render: 1180, errP: 0.0024, calls: []apiCall{bff("GET", "/v1/home", 1, true), svc("auth-api", "GET", "/v1/auth/me", 0.5, false)}},
			{name: n("/p/:slug", "ProductDetail"), weight: 61, render: 1750, errP: 0.004, calls: []apiCall{bff("GET", "/v1/product/{slug}", 1, true), svc("recommendation-api", "POST", "/v1/events", 1, false), svc("catalog-api", "GET", "/v1/items/{id}", 0.4, false)}},
			{name: n("/search", "Search"), weight: 30, render: 1500, errP: 0.0036, calls: []apiCall{bff("GET", "/v1/search", 1, true), svc("catalog-api", "GET", "/v1/items", 0.6, false)}},
			{name: n("/cart", "Cart"), weight: 12, render: 1250, errP: 0.006, calls: []apiCall{svc("orders-api", "GET", "/v1/cart", 1, true), svc("orders-api", "PUT", "/v1/cart/items/{sku}", 0.4, false)}},
			{name: n("/checkout", "Checkout"), weight: 7, render: 2500, errP: 0.019, checkout: true, calls: []apiCall{svc("orders-api", "GET", "/v1/cart", 1, true)}},
			{name: n("/account", "Account"), weight: 5, render: 1150, errP: 0.0012, calls: []apiCall{svc("auth-api", "GET", "/v1/auth/me", 1, true), svc("orders-api", "GET", "/v1/orders", 1, false)}},
			{name: n("/account/orders/:id", "OrderTracking"), weight: 4, render: 1300, errP: 0.0022, calls: []apiCall{svc("orders-api", "GET", "/v1/orders/{id}", 1, true), svc("orders-api", "POST", "/v1/orders/{id}/cancel", 0.06, false)}},
		}
	}
	return []clientApp{
		{name: "shop-web", version: "2026.10.24", platform: "PLATFORM_WEB", sdk: "@trooper/react", sdkVersion: "0.4.0", viewsPerMin: 3900, ttfb: 320,
			pages: shopPages(false), devices: webDevices},
		{name: "shop-mobile", version: "4.12.0", platform: "PLATFORM_REACT_NATIVE", sdk: "@trooper/react-native", sdkVersion: "0.4.0", mobile: true, viewsPerMin: 2100, ttfb: 0,
			pages: shopPages(true)[:7], devices: mobileDevices("shop-mobile", "4.12.0"), coldStart: 1450},
		{name: "backoffice-web", version: "2026.10.20", platform: "PLATFORM_WEB", sdk: "@trooper/react", sdkVersion: "0.4.0", viewsPerMin: 310, ttfb: 280, devices: webDevices[:2],
			pages: []page{
				{name: "/orders", weight: 40, render: 1300, errP: 0.002, calls: []apiCall{svc("orders-api", "GET", "/v1/orders", 1, true)}},
				{name: "/orders/:id", weight: 30, render: 1200, errP: 0.002, calls: []apiCall{svc("orders-api", "GET", "/v1/orders/{id}", 1, true), svc("orders-api", "POST", "/v1/orders/{id}/cancel", 0.15, false)}},
				{name: "/refunds", weight: 12, render: 1500, errP: 0.002, calls: []apiCall{svc("payments-api", "GET", "/v1/charges/{id}", 1, true)}},
				{name: "/customers/:id", weight: 10, render: 1400, errP: 0.001, calls: []apiCall{svc("orders-api", "GET", "/v1/orders", 1, true)}},
				{name: "/reports", weight: 8, render: 2400, errP: 0.003},
			}},
		{name: "seller-portal", version: "3.8.1", platform: "PLATFORM_WEB", sdk: "@trooper/vue", sdkVersion: "0.4.0", viewsPerMin: 640, ttfb: 420, devices: webDevices,
			pages: []page{
				{name: "/dashboard", weight: 35, render: 1900, errP: 0.005, calls: []apiCall{svc("orders-api", "GET", "/v1/orders", 1, true)}},
				{name: "/orders", weight: 30, render: 1700, errP: 0.004, calls: []apiCall{svc("orders-api", "GET", "/v1/orders", 1, true)}},
				{name: "/products", weight: 18, render: 1800, errP: 0.004, calls: []apiCall{svc("catalog-api", "GET", "/v1/items", 1, true)}},
				{name: "/products/:id/edit", weight: 10, render: 1600, errP: 0.006, calls: []apiCall{svc("catalog-api", "GET", "/v1/items/{id}", 1, true), svc("catalog-api", "POST", "/v1/items", 0.3, false)}},
				{name: "/payouts", weight: 7, render: 2100, errP: 0.003},
			}},
		{name: "landing-web", version: "2026.10.22", platform: "PLATFORM_WEB", sdk: "@trooper/react", sdkVersion: "0.4.0", viewsPerMin: 1450, ttfb: 180, devices: webDevices,
			pages: []page{
				{name: "/", weight: 50, render: 900, errP: 0.0006, calls: []apiCall{bff("GET", "/v1/home", 0.35, true)}},
				{name: "/pricing", weight: 20, render: 850, errP: 0.0004},
				{name: "/blog/:slug", weight: 18, render: 1000, errP: 0.0006},
				{name: "/campaign/:slug", weight: 12, render: 1100, errP: 0.0008},
			}},
		{name: "courier-app", version: "2.3.0", platform: "PLATFORM_FLUTTER", sdk: "trooper_flutter", sdkVersion: "0.4.0", mobile: true, viewsPerMin: 380, devices: mobileDevices("courier-app", "2.3.0"), coldStart: 1300,
			pages: []page{
				{name: "Deliveries", weight: 40, render: 700, errP: 0.003, calls: []apiCall{svc("orders-api", "GET", "/v1/orders", 1, true)}},
				{name: "DeliveryDetail", weight: 30, render: 600, errP: 0.003, calls: []apiCall{svc("orders-api", "GET", "/v1/orders/{id}", 1, true)}},
				{name: "ProofOfDelivery", weight: 20, render: 900, errP: 0.004, calls: []apiCall{svc("orders-api", "GET", "/v1/orders/{id}", 1, true)}},
				{name: "Earnings", weight: 10, render: 800, errP: 0.002},
			}},
	}
}

func (a clientApp) callsPerView() float64 {
	tw, sum := 0.0, 0.0
	for _, p := range a.pages {
		tw += p.weight
		c := 0.0
		for _, k := range p.calls {
			c += k.p
		}
		if p.checkout {
			c += 1.9
		}
		sum += p.weight * c
	}
	return sum/tw + 0.3 // + token refreshes
}

// ---------------------------------------------------------------- traffic

// Traffic drives every client: RUM sessions of the apps (whose network calls hit the backend with a
// W3C traceparent, like the browser / RN / Flutter SDKs do) and the non-app callers.
type Traffic struct {
	RT        *world.Runtime
	Log       *slog.Logger
	RPS       float64 // ≈ entry requests per second from clients (internal fan-out adds more)
	IngestURL string  // http://host:port
	IngestKey string

	client   *http.Client
	rum      *http.Client
	noise    atomic.Uint64 // float64 bits
	Requests atomic.Int64
	RumSent  atomic.Int64
	RumErr   atomic.Int64
}

// ParseDSN splits an ingest DSN (http://key@host:port/project).
func ParseDSN(dsn string) (base, key string, err error) {
	u, err := url.Parse(dsn)
	if err != nil || u.User == nil {
		return "", "", fmt.Errorf("bad DSN %q", dsn)
	}
	return u.Scheme + "://" + u.Host, u.User.Username(), nil
}

// factor is the daily traffic curve (same shape as the mock's series(): ±25 %, peak ~15:00) × slow noise.
func (t *Traffic) factor() float64 {
	now := time.Now()
	h := float64(now.Hour()) + float64(now.Minute())/60
	daily := 1 + 0.25*math.Sin((h-9)/24*2*math.Pi)
	return daily * math.Float64frombits(t.noise.Load())
}

// Run blocks until ctx ends.
func (t *Traffic) Run(ctx context.Context) {
	t.client = &http.Client{Timeout: 15 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: 256, IdleConnTimeout: 30 * time.Second}}
	t.rum = &http.Client{Timeout: 10 * time.Second}
	t.noise.Store(math.Float64bits(1))
	go func() { // random walk noise, like the mock's drift
		n := 1.0
		for sleepCtx(ctx, 10*time.Second) {
			n = 1 + (n-1)*0.85 + rand.NormFloat64()*0.04
			n = math.Max(0.7, math.Min(1.3, n))
			t.noise.Store(math.Float64bits(n))
		}
	}()

	all := apps()
	appShare := t.RPS * 0.88
	totalCalls := 0.0
	for _, a := range all {
		totalCalls += a.viewsPerMin * a.callsPerView()
	}
	scale := appShare / totalCalls // views per second per mock view/min
	const pagesPerSession = 6.5
	for _, a := range all {
		sessionsPerSec := a.viewsPerMin * scale / pagesPerSession
		t.Log.Info("client app", "app", a.name, "sessions_per_min", fmt.Sprintf("%.1f", sessionsPerSec*60))
		go t.arrivals(ctx, sessionsPerSec, func() { t.session(ctx, a) })
	}
	// non-app callers (mock CALLERS): kubelet probes, Stripe webhooks, a partner ERP, uninstrumented clients
	go func() {
		for sleepCtx(ctx, 5*time.Second) { // liveness + readiness (scaled down: one pod, not a fleet)
			go t.hit(ctx, "orders-api", "GET", "/healthz", "kube-probe/1.31", nil)
		}
	}()
	go t.arrivals(ctx, t.RPS*0.02, func() {
		t.hit(ctx, "payments-api", "POST", "/v1/webhooks/stripe", "Stripe/1.0 (+https://stripe.com/docs/webhooks)", nil)
	})
	go t.arrivals(ctx, t.RPS*0.04, func() {
		h := http.Header{"X-Api-Key": {"acme-erp"}}
		if rand.Float64() < 0.3 {
			t.hit(ctx, "orders-api", "POST", "/v1/orders", "AcmeERP/7.3 (partner integration)", h)
		} else {
			t.hit(ctx, "orders-api", "GET", fmt.Sprintf("/v1/orders/%d", 1+rand.IntN(200000)), "AcmeERP/7.3 (partner integration)", h)
		}
	})
	go t.arrivals(ctx, t.RPS*0.03, func() {
		ua := []string{"python-requests/2.32.3", "curl/8.7.1", "Go-http-client/1.1", ""}[rand.IntN(4)]
		switch rand.IntN(3) {
		case 0:
			t.hit(ctx, "catalog-api", "GET", "/v1/items", ua, nil)
		case 1:
			t.hit(ctx, "catalog-api", "GET", fmt.Sprintf("/v1/items/%d", 1000+rand.IntN(39000)), ua, nil)
		default:
			t.hit(ctx, "orders-api", "GET", fmt.Sprintf("/v1/orders/%d", 1+rand.IntN(200000)), ua, nil)
		}
	})
	<-ctx.Done()
}

// arrivals runs f as a Poisson process with rate (per second) × the traffic factor.
func (t *Traffic) arrivals(ctx context.Context, rate float64, f func()) {
	if rate <= 0 || f == nil {
		return
	}
	for {
		r := rate * t.factor()
		wait := time.Duration(rand.ExpFloat64() / r * float64(time.Second))
		if !sleepCtx(ctx, wait) {
			return
		}
		go f()
	}
}

// hit is a request from a caller without RUM (no traceparent: the server starts the trace).
func (t *Traffic) hit(ctx context.Context, svc, method, path, ua string, h http.Header) {
	base := t.RT.Peers[svc]
	req, err := http.NewRequestWithContext(ctx, method, base+path, nil)
	if err != nil {
		return
	}
	for k, v := range h {
		req.Header[k] = v
	}
	req.Header.Set("User-Agent", ua)
	t.Requests.Add(1)
	if resp, err := t.client.Do(req); err == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}

// ---------------------------------------------------------------- RUM sessions

var userNames = []string{"ayse.k", "mehmet.y", "zeynep.a", "can.d", "elif.s", "burak.t", "deniz.o", "selin.b", "emre.c", "naz.g"}

type session struct {
	t        *Traffic
	app      clientApp
	dev      device
	id, anon string
	userID   string
	conn     string
	events   []map[string]any
	mu       sync.Mutex
}

func randHex(n int) string {
	const hx = "0123456789abcdef"
	b := make([]byte, n)
	for i := range b {
		b[i] = hx[rand.IntN(16)]
	}
	return string(b)
}

func (s *session) add(typ string, viewID, viewName string, payloadKey string, payload map[string]any, attrs map[string]string) {
	e := map[string]any{
		"type": typ, "ts_ms": time.Now().UnixMilli(), "session_id": s.id, "anonymous_id": s.anon, "connection": s.conn,
	}
	if viewID != "" {
		e["view_id"], e["view_name"] = viewID, viewName
	}
	if s.userID != "" {
		e["user"] = map[string]any{"id": s.userID}
	}
	if payloadKey != "" {
		e[payloadKey] = payload
	}
	if len(attrs) > 0 {
		e["attributes"] = attrs
	}
	s.mu.Lock()
	s.events = append(s.events, e)
	s.mu.Unlock()
}

func (s *session) flush(ctx context.Context) {
	s.mu.Lock()
	evs := s.events
	s.events = nil
	s.mu.Unlock()
	if len(evs) == 0 {
		return
	}
	res := map[string]any{
		"app": s.app.name, "version": s.app.version, "environment": world.Environment, "platform": s.app.platform,
		"sdk":    map[string]any{"name": s.app.sdk, "version": s.app.sdkVersion},
		"device": map[string]any{"type": s.dev.typ, "manufacturer": s.dev.manufacturer, "model": s.dev.model, "locale": "tr-TR", "screen_width": s.dev.w, "screen_height": s.dev.h},
		"os":     map[string]any{"name": s.dev.osName, "version": s.dev.osVersion},
	}
	if s.dev.browser != "" {
		res["browser"] = map[string]any{"name": s.dev.browser, "version": s.dev.browserVersion}
	}
	body, _ := json.Marshal(map[string]any{"resource": res, "events": evs})
	req, err := http.NewRequestWithContext(context.WithoutCancel(ctx), "POST", s.t.IngestURL+"/v1/rum", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-trooper-key", s.t.IngestKey)
	req.Header.Set("User-Agent", s.dev.ua)
	resp, err := s.t.rum.Do(req)
	if err != nil {
		s.t.RumErr.Add(1)
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		s.t.RumErr.Add(1)
		if s.t.RumErr.Load()%50 == 1 {
			s.t.Log.Warn("rum batch rejected", "status", resp.StatusCode)
		}
		return
	}
	s.t.RumSent.Add(int64(len(evs)))
}

func rating(v, good, poor float64) string {
	switch {
	case v <= good:
		return "good"
	case v <= poor:
		return "needs-improvement"
	}
	return "poor"
}

// call performs one network request of the app the way the client SDKs do: a fresh trace id and
// span id, sent as traceparent (no baggage), and reported as a RESOURCE event with both ids.
func (s *session) call(ctx context.Context, viewID, viewName string, c apiCall) (int, float64) {
	path := strings.NewReplacer("{id}", fmt.Sprint(1+rand.IntN(200000)), "{sku}", fmt.Sprint(1000+rand.IntN(9000)),
		"{slug}", fmt.Sprintf("product-%d", 1000+rand.IntN(39000))).Replace(c.path)
	traceID, spanID := randHex(32), randHex(16)
	req, err := http.NewRequestWithContext(ctx, c.method, s.t.RT.Peers[c.svc]+path, nil)
	if err != nil {
		return 0, 0
	}
	req.Header.Set("traceparent", "00-"+traceID+"-"+spanID+"-01")
	req.Header.Set("User-Agent", s.dev.ua)
	if !s.app.mobile {
		req.Header.Set("Origin", "https://"+s.app.name+".shop.com")
	}
	t0 := time.Now()
	s.t.Requests.Add(1)
	status, size := 0, int64(0)
	if resp, err := s.t.client.Do(req); err == nil {
		size, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		status = resp.StatusCode
	}
	if s.app.mobile { // radio latency on top of the server time
		sleepCtx(ctx, time.Duration(lognormal(60, 0.5)*float64(time.Millisecond)))
	}
	ms := float64(time.Since(t0).Microseconds()) / 1000
	host := map[string]string{"storefront-bff": "bff.shop.com", "recommendation-api": "reco.shop.com"}[c.svc]
	if host == "" {
		host = "api.shop.com"
	}
	initiator := "fetch"
	switch s.app.platform {
	case "PLATFORM_REACT_NATIVE":
		initiator = "xhr"
	case "PLATFORM_FLUTTER":
		initiator = "dio"
	}
	s.add("RESOURCE", viewID, viewName, "resource", map[string]any{
		"url": "https://" + host + path, "method": c.method, "status_code": status, "duration_ms": ms,
		"response_bytes": size, "trace_id": traceID, "span_id": spanID, "initiator": initiator,
	}, nil)
	return status, ms
}

func pickPage(ps []page, entry bool) page {
	n := len(ps)
	if entry && n > 3 {
		n = 3
	}
	tw := 0.0
	for _, p := range ps[:n] {
		tw += p.weight
	}
	x := rand.Float64() * tw
	for _, p := range ps[:n] {
		if x -= p.weight; x <= 0 {
			return p
		}
	}
	return ps[0]
}

func (t *Traffic) session(ctx context.Context, a clientApp) {
	s := &session{t: t, app: a, dev: a.devices[rand.IntN(len(a.devices))], id: "s_" + randHex(16), anon: "a_" + randHex(16),
		conn: []string{"wifi", "wifi", "4g", "cellular", "unknown"}[rand.IntN(5)]}
	if !a.mobile && s.dev.typ == "desktop" {
		s.conn = "unknown"
	}
	if rand.Float64() < 0.55 {
		s.userID = fmt.Sprintf("%s.%d", userNames[rand.IntN(len(userNames))], rand.IntN(5000))
	}
	s.add("SESSION_START", "", "", "", nil, nil)
	if a.mobile {
		kind, med, ref := "cold", a.coldStart, "process"
		if rand.Float64() < 0.35 {
			kind, med, ref = "warm", 520, "foreground"
		}
		s.add("APP_START", "", "", "app_start", map[string]any{"kind": kind, "duration_ms": lognormal(med, 0.3)}, map[string]string{"start_reference": ref})
	}
	if s.userID != "" && rand.Float64() < 0.15 {
		s.call(ctx, "", "", apiCall{svc: "auth-api", method: "POST", path: "/v1/auth/login"})
	}
	n := 2 + rand.IntN(10)
	for i := 0; i < n && ctx.Err() == nil; i++ {
		p := pickPage(a.pages, i == 0)
		crashed := t.view(ctx, s, p, i == 0)
		s.flush(ctx)
		if crashed || rand.Float64() < 0.12 { // bounce / crash ends the session
			break
		}
	}
	s.flush(ctx)
}

// view simulates one page / screen view; returns true when the app crashed.
func (t *Traffic) view(ctx context.Context, s *session, p page, initial bool) bool {
	a := s.app
	viewID := "v_" + randHex(12)
	start := time.Now()
	ttfb := 0.0
	if !a.mobile {
		ttfb = lognormal(a.ttfb, 0.45)
		sleepCtx(ctx, time.Duration(ttfb)*time.Millisecond)
	}
	// blocking data calls in parallel; the view is ready when the slowest returns
	var wg sync.WaitGroup
	var mu sync.Mutex
	slowest := 0.0
	for _, c := range p.calls {
		if !c.blocking || rand.Float64() > c.p {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, ms := s.call(ctx, viewID, p.name, c)
			mu.Lock()
			slowest = math.Max(slowest, ms)
			mu.Unlock()
		}()
	}
	wg.Wait()
	render := lognormal(p.render*0.55, 0.35)
	if !a.mobile {
		render = lognormal(p.render-a.ttfb*0.4, 0.3) // first paint, fonts, images
	}
	sleepCtx(ctx, time.Duration(render*0.15)*time.Millisecond) // the rest of the render is off the critical path of the sim
	load := ttfb + slowest + render
	s.add("VIEW_START", viewID, p.name, "view", map[string]any{
		"url": viewURL(a, p.name), "loading_time_ms": load, "is_initial": initial,
	}, nil)
	if !a.mobile {
		fcp := ttfb + lognormal(420, 0.3)
		lcp := math.Max(fcp, load+lognormal(60, 0.5))
		cls := math.Max(0, lognormal(0.04, 0.9)-0.01)
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "TTFB", "value": ttfb, "rating": rating(ttfb, 800, 1800)}, nil)
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "FCP", "value": fcp, "rating": rating(fcp, 1800, 3000)}, nil)
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "LCP", "value": lcp, "rating": rating(lcp, 2500, 4000)}, nil)
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "CLS", "value": cls, "rating": rating(cls, 0.1, 0.25)}, nil)
	}
	// non-blocking calls (prefetch, beacons, background refreshes)
	for _, c := range p.calls {
		if !c.blocking && rand.Float64() < c.p {
			go s.call(ctx, viewID, p.name, c)
		}
	}
	if rand.Float64() < 0.25 {
		go s.call(ctx, viewID, p.name, apiCall{svc: "auth-api", method: "POST", path: "/v1/auth/refresh"})
	}
	// interactions
	clicks := 1 + rand.IntN(3)
	for i := 0; i < clicks; i++ {
		rage := rand.Float64() < 0.01
		attrs := map[string]string{"click_count": "1"}
		if rage {
			attrs["click_count"] = "4"
		}
		s.add("ACTION", viewID, p.name, "action", map[string]any{"name": []string{"Add to cart", "Search", "Next", "Filter", "Open"}[rand.IntN(5)],
			"target": "button", "rage": rage, "dead": rand.Float64() < 0.02}, attrs)
	}
	if p.checkout && rand.Float64() < 0.85 {
		st, _ := s.call(ctx, viewID, p.name, apiCall{svc: "orders-api", method: "POST", path: "/v1/orders"})
		if st == 201 {
			st, _ = s.call(ctx, viewID, p.name, apiCall{svc: "payments-api", method: "POST", path: "/v1/charges"})
			s.add("CUSTOM", viewID, p.name, "custom", map[string]any{"name": "checkout_completed", "props": map[string]string{"ok": fmt.Sprint(st == 201)}}, nil)
		}
	}
	crashed := false
	if rand.Float64() < p.errP {
		if a.mobile && rand.Float64() < 0.4 {
			crashed = true
			typ, msg := "SIGABRT", "NSInvalidArgumentException: -[__NSCFNumber length]: unrecognized selector"
			if s.dev.osName == "Android" {
				typ, msg = "java.lang.IllegalStateException", "Fragment CheckoutFragment not attached to a context."
			}
			if a.platform == "PLATFORM_FLUTTER" {
				typ, msg = "FlutterError", "Null check operator used on a null value"
			}
			s.add("ERROR", viewID, p.name, "error", map[string]any{"type": typ, "message": msg, "source": "crash", "is_crash": true,
				"stack": "at " + p.name + "Screen.render (index.bundle:1:48211)", "thread": "main"}, nil)
		} else {
			e := [][2]string{
				{"TypeError", "Cannot read properties of undefined (reading 'price')"},
				{"ChunkLoadError", "Loading chunk 712 failed."},
				{"Error", "Request failed with status code 502"},
				{"TypeError", "undefined is not an object (evaluating 'cart.items.length')"},
			}[rand.IntN(4)]
			s.add("ERROR", viewID, p.name, "error", map[string]any{"type": e[0], "message": e[1], "source": "source", "is_crash": false,
				"stack": e[0] + ": " + e[1] + "\n    at https://" + a.name + ".shop.com/assets/index-4f2a9c.js:2:41877"}, map[string]string{"component": "App"})
		}
	}
	// time on view (compressed think time), then INP / frames and VIEW_END
	think := time.Duration(lognormal(9000, 0.7)) * time.Millisecond
	if !crashed {
		sleepCtx(ctx, think)
	}
	if a.mobile {
		total := 60 * (1 + int(time.Since(start).Seconds()))
		slow := int(float64(total) * math.Max(0, lognormal(0.035, 0.6)))
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "SLOW_FRAMES", "value": slow}, map[string]string{"total_frames": fmt.Sprint(total)})
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "FROZEN_FRAMES", "value": poisson(0.05)}, map[string]string{"total_frames": fmt.Sprint(total)})
	} else {
		inp := lognormal(130, 0.5)
		if p.checkout {
			inp *= 1.8
		}
		s.add("VITAL", viewID, p.name, "vital", map[string]any{"name": "INP", "value": inp, "rating": rating(inp, 200, 500)}, nil)
	}
	s.add("VIEW_END", viewID, p.name, "view", map[string]any{"time_spent_ms": float64(time.Since(start).Milliseconds())}, nil)
	return crashed
}

func viewURL(a clientApp, name string) string {
	if a.mobile {
		return ""
	}
	path := strings.NewReplacer(":slug", "red-sneaker-1042", ":id", "184467").Replace(name)
	return "https://" + a.name + ".shop.com" + path
}
