package svc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/redis/go-redis/v9"
)

// ---------------------------------------------------------------- payments-api

func runPayments(ctx context.Context, a *App, port int) error {
	db, err := a.PG(ctx, "payments-db", 16, 2)
	if err != nil {
		return err
	}
	cache := a.Redis("cache-payments")
	kc, err := a.Producer()
	if err != nil {
		return err
	}
	rab := newRabbitPublisher(ctx, a)
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/charges", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		order := ID(1, 9_000_000)
		if err := cache.Get(ctx, fmt.Sprintf("idem:%d", order)).Err(); err != nil && !errors.Is(err, redis.Nil) {
			a.Log.WarnContext(ctx, "idempotency cache", "err", err.Error())
		}
		amount := float64(ID(2000, 90000)) / 100
		var id int64
		if err := db.QueryRow(ctx, `INSERT INTO charges (order_id, amount, currency, provider, status) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
			order, amount, "TRY", "iyzico", "pending").Scan(&id); err != nil {
			a.Fail(w, r, 500, err, "insert charge failed", "err", err.Error())
			return
		}
		t0 := time.Now()
		st, err := a.Call(ctx, "iyzico", "POST", "/payment/auth", fmt.Sprintf(`{"amount":%.2f}`, amount))
		if err != nil {
			_, _ = db.Exec(ctx, `UPDATE charges SET status = $1, provider_ref = $2 WHERE id = $3`, "failed", nil, id)
			msg := fmt.Sprintf("charge failed: provider timeout after %dms provider=iyzico order_id=ord_%d", time.Since(t0).Milliseconds(), order)
			if !strings.Contains(err.Error(), "deadline") && !strings.Contains(err.Error(), "Timeout") {
				msg = fmt.Sprintf("charge failed: provider error status=%d provider=iyzico order_id=ord_%d", st, order)
			}
			a.Fail(w, r, http.StatusBadGateway, err, msg)
			return
		}
		if st == http.StatusPaymentRequired {
			_, _ = db.Exec(ctx, `UPDATE charges SET status = $1, provider_ref = $2 WHERE id = $3`, "declined", nil, id)
			a.Log.ErrorContext(ctx, fmt.Sprintf("payment declined code=insufficient_funds order_id=ord_%d", order))
			JSON(w, http.StatusPaymentRequired, `{"error":"declined"}`)
			return
		}
		ref := "pr_" + HexID(24)
		_, _ = db.Exec(ctx, `UPDATE charges SET status = $1, provider_ref = $2 WHERE id = $3`, "authorized", ref, id)
		_, _ = db.Exec(ctx, `UPDATE charges SET status = $1, provider_ref = $2 WHERE id = $3`, "captured", ref, id)
		_ = cache.SetEx(ctx, fmt.Sprintf("idem:%d", order), strconv.FormatInt(id, 10), 24*time.Hour).Err()
		a.Produce(ctx, kc, "payments.events", strconv.Itoa(order), fmt.Appendf(nil, `{"type":"charge.captured","charge_id":%d,"amount":%.2f}`, id, amount))
		a.Produce(ctx, kc, "orders.status-changed", strconv.Itoa(order), fmt.Appendf(nil, `{"order_id":%d,"status":"paid"}`, order))
		rab.publish(ctx, "email.transactional.payment_receipt", fmt.Sprintf(`{"template":"receipt","charge_id":%d}`, id))
		a.Log.InfoContext(ctx, fmt.Sprintf("charge captured charge_id=ch_%d amount=%.2f provider=iyzico", id, amount))
		JSON(w, 201, fmt.Sprintf(`{"id":%d}`, id))
	})
	mux.HandleFunc("POST /v1/refunds", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		charge := ID(1, 100000)
		var amount float64
		if err := db.QueryRow(ctx, `SELECT amount FROM charges WHERE id = $1`, charge).Scan(&amount); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, "load charge failed", "err", err.Error())
			return
		}
		if _, err := a.Call(ctx, "iyzico", "POST", "/payment/refund", `{}`); err != nil {
			a.Fail(w, r, 502, err, fmt.Sprintf("refund failed: provider error provider=iyzico charge_id=ch_%d", charge))
			return
		}
		for i := 0; i < 2; i++ {
			if _, err := db.Exec(ctx, `INSERT INTO refunds (charge_id, amount, reason) VALUES ($1, $2, $3)`, charge, amount/2, "order_cancelled"); err != nil {
				a.Fail(w, r, 500, err, "insert refund failed")
				return
			}
		}
		a.Produce(ctx, kc, "payments.events", strconv.Itoa(charge), fmt.Appendf(nil, `{"type":"charge.refunded","charge_id":%d}`, charge))
		JSON(w, 201, `{"ok":true}`)
	})
	mux.HandleFunc("GET /v1/charges/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, _ := strconv.Atoi(r.PathValue("id"))
		if err := cache.Get(ctx, fmt.Sprintf("charge:%d", id)).Err(); err == nil {
			JSON(w, 200, `{}`)
			return
		}
		var status string
		if err := db.QueryRow(ctx, `SELECT status FROM charges WHERE id = $1`, id).Scan(&status); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, "load charge failed")
			return
		}
		_ = cache.SetEx(ctx, fmt.Sprintf("charge:%d", id), status, time.Minute).Err()
		JSON(w, 200, fmt.Sprintf(`{"status":%q}`, status))
	})
	mux.HandleFunc("POST /v1/webhooks/stripe", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		evt := "evt_" + HexID(20)
		if _, err := a.Call(ctx, "stripe", "GET", "/v1/events/"+evt, nil); err != nil {
			a.Fail(w, r, 502, err, "stripe event fetch failed", "event", evt)
			return
		}
		ref := fmt.Sprintf("pr_%x", ID(1, 100000))
		if _, err := db.Exec(ctx, `UPDATE charges SET status = $1 WHERE provider_ref = $2`, "captured", ref); err != nil {
			a.Fail(w, r, 500, err, "webhook update failed")
			return
		}
		if Chance(0.03) {
			err := errors.New("webhook: charge not found for provider_ref " + ref)
			a.Fail(w, r, 500, err, "stripe webhook failed: charge not found provider_ref="+ref)
			return
		}
		_, _ = db.Exec(ctx, `UPDATE charges SET status = $1 WHERE provider_ref = $2`, "settled", ref)
		a.Produce(ctx, kc, "payments.events", ref, []byte(`{"type":"charge.settled"}`))
		JSON(w, 200, `{"received":true}`)
	})
	return a.Serve(ctx, port, mux)
}

// ---------------------------------------------------------------- catalog-api

func runCatalog(ctx context.Context, a *App, port int) error {
	db, err := a.PG(ctx, "catalog-db", 16, 2)
	if err != nil {
		return err
	}
	cache := a.Redis("cache-main")
	kc, err := a.Producer()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/items", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cat := Pick([]string{"shoes", "apparel", "home", "accessories"})
		if v, err := cache.Get(ctx, "items:"+cat).Result(); err == nil {
			Work(ctx, 3, 0.4)
			JSON(w, 200, v)
			return
		}
		rows, err := db.Query(ctx, `SELECT id, sku, name, price FROM items WHERE category = $1 ORDER BY id DESC LIMIT $2`, cat, 48)
		if err != nil {
			a.Fail(w, r, 500, err, "list items failed")
			return
		}
		for rows.Next() {
		}
		rows.Close()
		_ = cache.SetEx(ctx, "items:"+cat, `{"items":[]}`, 20*time.Second).Err()
		Work(ctx, 6, 0.4)
		JSON(w, 200, `{"items":[]}`)
	})
	mux.HandleFunc("GET /v1/items/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, _ := strconv.Atoi(r.PathValue("id"))
		keys := []string{fmt.Sprintf("item:%d", id), fmt.Sprintf("item:%d", id+1), fmt.Sprintf("item:%d", id+2)}
		vals, _ := cache.MGet(ctx, keys...).Result()
		if len(vals) > 0 && vals[0] != nil {
			JSON(w, 200, `{}`)
			return
		}
		var name string
		if err := db.QueryRow(ctx, `SELECT name FROM items WHERE id = $1`, id).Scan(&name); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, "load item failed")
			return
		}
		_ = cache.SetEx(ctx, keys[0], name, 10*time.Minute).Err()
		Work(ctx, 3, 0.4)
		JSON(w, 200, fmt.Sprintf(`{"name":%q}`, name))
	})
	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		term := Pick([]string{"red", "sneaker", "lamp", "black jacket", "mug", "watch"})
		if a.RT.LLM && !a.embed(ctx, int64(len(strings.Fields(term))+ID(2, 7)), 1) { // semantic re-rank: query embedding
			a.Log.WarnContext(ctx, "query embedding rate limited, falling back to keyword search provider=openai")
		}
		rows, err := db.Query(ctx, `SELECT id, name, price FROM items WHERE name ILIKE $1 ORDER BY price LIMIT $2`, "%"+term+"%", 20)
		if err != nil {
			a.Fail(w, r, 500, err, "search failed")
			return
		}
		for rows.Next() {
		}
		rows.Close()
		_ = cache.Incr(ctx, "search:count").Err()
		Work(ctx, 20, 0.6)
		JSON(w, 200, `{"hits":[]}`)
	})
	mux.HandleFunc("POST /v1/items", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		sku := fmt.Sprintf("SKU-%d", ID(41000, 9_999_999))
		if _, err := db.Exec(ctx, `INSERT INTO items (sku, name, category, price, stock) VALUES ($1, $2, $3, $4, $5) ON CONFLICT (sku) DO NOTHING`,
			sku, "new item", "home", 19.9, 10); err != nil {
			a.Fail(w, r, 500, err, "create item failed")
			return
		}
		if a.RT.LLM { // title + description + attributes → item embedding (name, description chunks)
			a.embed(ctx, int64(ID(180, 620)), 3)
		}
		a.Produce(ctx, kc, "catalog.events", sku, []byte(`{"type":"item.created"}`))
		JSON(w, 201, `{"ok":true}`)
	})
	return a.Serve(ctx, port, mux)
}

// ---------------------------------------------------------------- auth-api

func runAuth(ctx context.Context, a *App, port int) error {
	db, err := a.PG(ctx, "auth-db", 16, 2)
	if err != nil {
		return err
	}
	cache := a.Redis("cache-main")
	kc, err := a.Producer()
	if err != nil {
		return err
	}
	// session payload; with cache-evictions the keys get no TTL and keep growing (mock: session:* has no TTL)
	payload := strings.Repeat("x", 2048)
	setSession := func(ctx context.Context, sid string) {
		if a.RT.Active("cache-evictions") {
			_ = cache.Set(ctx, "session:"+sid, payload, 0).Err()
			return
		}
		_ = cache.SetEx(ctx, "session:"+sid, payload[:256], 10*time.Minute).Err()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/auth/refresh", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		u := ID(1, 50000)
		var uid int64
		err := db.QueryRow(ctx, `SELECT user_id FROM refresh_tokens WHERE token_hash = md5($1) AND expires_at > now()`, fmt.Sprintf("rt%d", u)).Scan(&uid)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, "refresh lookup failed")
			return
		}
		Work(ctx, 8, 0.4)
		setSession(ctx, HexID(24))
		a.Produce(ctx, kc, "auth.events", strconv.Itoa(u), []byte(`{"type":"token.refreshed"}`))
		a.Log.InfoContext(ctx, fmt.Sprintf("token refreshed user_id=u_%d", u))
		JSON(w, 200, `{"access_token":"…"}`)
	})
	mux.HandleFunc("POST /v1/auth/login", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		u := ID(1, 50000)
		var hash string
		if err := db.QueryRow(ctx, `SELECT password_hash FROM users WHERE email = $1`, fmt.Sprintf("user%d@example.com", u)).Scan(&hash); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, "user lookup failed")
			return
		}
		Work(ctx, 70, 0.2) // password hashing
		ok := !Chance(0.08)
		_, _ = db.Exec(ctx, `INSERT INTO login_events (user_id, ok) VALUES ($1, $2)`, u, ok)
		if !ok {
			a.Log.WarnContext(ctx, fmt.Sprintf("login failed: invalid password user_id=u_%d", u))
			JSON(w, 401, `{"error":"invalid_credentials"}`)
			return
		}
		setSession(ctx, HexID(24))
		a.Produce(ctx, kc, "auth.events", strconv.Itoa(u), []byte(`{"type":"login"}`))
		JSON(w, 200, `{"ok":true}`)
	})
	mux.HandleFunc("GET /v1/auth/me", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		if err := cache.Get(ctx, "session:"+HexID(24)).Err(); err != nil && !errors.Is(err, redis.Nil) {
			a.Fail(w, r, 500, err, "session read failed")
			return
		}
		Work(ctx, 2, 0.3)
		JSON(w, 200, `{"id":"u"}`)
	})
	mux.HandleFunc("POST /v1/auth/logout", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_ = cache.Del(ctx, "session:"+HexID(24)).Err()
		a.Produce(ctx, kc, "auth.events", "", []byte(`{"type":"logout"}`))
		JSON(w, 204, ``)
	})
	return a.Serve(ctx, port, mux)
}

// ---------------------------------------------------------------- storefront-bff

func runBFF(ctx context.Context, a *App, port int) error {
	cache := a.Redis("cache-bff")
	mux := http.NewServeMux()
	parallel := func(ctx context.Context, fs ...func(context.Context) error) error {
		var wg sync.WaitGroup
		errs := make([]error, len(fs))
		for i, f := range fs {
			wg.Add(1)
			go func() { defer wg.Done(); errs[i] = f(ctx) }()
		}
		wg.Wait()
		return errors.Join(errs...)
	}
	call := func(peer, method, path string) func(context.Context) error {
		return func(ctx context.Context) error { _, err := a.Call(ctx, peer, method, path, nil); return err }
	}
	mux.HandleFunc("GET /v1/home", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		for _, k := range []string{"home:layout", "home:banners", "home:flags"} {
			if err := cache.Get(ctx, k).Err(); errors.Is(err, redis.Nil) {
				_ = cache.SetEx(ctx, k, "{}", 30*time.Second).Err()
			}
		}
		err := parallel(ctx, call("catalog-api", "GET", "/v1/items"), call("recommendation-api", "GET", fmt.Sprintf("/v1/recommendations?user=%d", ID(1, 30000))))
		if err != nil {
			a.Fail(w, r, 502, err, "home composition failed", "err", err.Error())
			return
		}
		Work(ctx, 25, 0.4)
		JSON(w, 200, `{"sections":[]}`)
	})
	mux.HandleFunc("GET /v1/product/{slug}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id := ID(1000, 40000)
		_ = cache.Get(ctx, "product:"+r.PathValue("slug")).Err()
		err := parallel(ctx,
			func(ctx context.Context) error {
				if err := call("catalog-api", "GET", fmt.Sprintf("/v1/items/%d", id))(ctx); err != nil {
					return err
				}
				return call("catalog-api", "GET", fmt.Sprintf("/v1/items/%d", id+1))(ctx)
			},
			call("recommendation-api", "GET", fmt.Sprintf("/v1/recommendations?user=%d", ID(1, 30000))))
		if err != nil {
			a.Fail(w, r, 502, err, "product composition failed", "err", err.Error())
			return
		}
		_ = cache.SetEx(ctx, "product:"+r.PathValue("slug"), "{}", time.Minute).Err()
		Work(ctx, 20, 0.4)
		JSON(w, 200, `{}`)
	})
	mux.HandleFunc("GET /v1/search", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		_ = cache.Get(ctx, "search:suggest").Err()
		if err := call("catalog-api", "GET", "/v1/search")(ctx); err != nil {
			a.Fail(w, r, 502, err, "search failed")
			return
		}
		Work(ctx, 10, 0.4)
		JSON(w, 200, `{}`)
	})
	if a.RT.LLM {
		// support assistant (anthropic, streaming, tool use, content capture) — llm.go
		mux.HandleFunc("POST /v1/assistant/messages", a.handleAssistant)
	}
	return a.Serve(ctx, port, mux)
}

// ---------------------------------------------------------------- recommendation-api

func runReco(ctx context.Context, a *App, port int) error {
	db, err := a.PG(ctx, "reco-db", 30, 2)
	if err != nil {
		return err
	}
	cache := a.Redis("reco-cache")
	kc, err := a.Producer()
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/recommendations", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		user, _ := strconv.Atoi(r.URL.Query().Get("user"))
		if user == 0 {
			user = ID(1, 30000)
		}
		if err := cache.Get(ctx, fmt.Sprintf("reco:%d", user)).Err(); err == nil && Chance(0.5) {
			JSON(w, 200, `{"items":[]}`)
			return
		}
		var segment string
		if err := db.QueryRow(ctx, `SELECT segment FROM user_segments WHERE user_id = $1`, user).Scan(&segment); err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, "segment lookup failed")
			return
		}
		var ids []int32
		for _, n := range []int{12, 8} { // "for you" + "similar" rails: the top query, twice
			rows, err := db.Query(ctx, `SELECT item_id, score FROM reco_items WHERE user_id = $1 ORDER BY score DESC LIMIT $2`, user, n)
			if err != nil {
				a.Fail(w, r, 500, err, "recommendation query failed", "err", err.Error())
				return
			}
			for rows.Next() {
				var id int32
				var s float32
				_ = rows.Scan(&id, &s)
				ids = append(ids, id)
			}
			rows.Close()
			if err := rows.Err(); err != nil {
				a.Fail(w, r, 500, err, "recommendation query failed", "err", err.Error())
				return
			}
		}
		rows, err := db.Query(ctx, `SELECT id, name, price FROM items WHERE id = ANY($1)`, ids)
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
		}
		Work(ctx, 15, 0.5)
		_ = cache.SetEx(ctx, fmt.Sprintf("reco:%d", user), "[]", 2*time.Minute).Err()
		JSON(w, 200, `{"items":[]}`)
	})
	mux.HandleFunc("POST /v1/events", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		u := ID(1, 50000)
		_ = cache.Incr(ctx, fmt.Sprintf("views:%d", u)).Err()
		a.Produce(ctx, kc, "reco.events", strconv.Itoa(u), fmt.Appendf(nil, `{"user":%d,"item":%d,"type":"view"}`, u, ID(1, 20000)))
		JSON(w, 202, `{}`)
	})
	go Every(ctx, 30*time.Second, func() {
		s := db.Stat()
		if s.AcquiredConns() > 20 {
			a.Log.Warn(fmt.Sprintf("connection pool saturated acquired=%d max=%d db=reco-db", s.AcquiredConns(), s.MaxConns()))
		}
	})
	return a.Serve(ctx, port, mux)
}
