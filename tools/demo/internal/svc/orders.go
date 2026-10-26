package svc

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/jackc/pgx/v5"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/redis/go-redis/v9"

	trooperamqp "github.com/tasktrooper/telemetry-go/integrations/amqp091"

	"github.com/tasktrooper/telemetry/tools/demo/internal/world"
)

// HotMerchant is the merchant whose orders all land on one orders.created partition.
const HotMerchant = "m-1042"

// merchant picks the merchant of a new order (orders are keyed by merchant_id).
func merchant(rt world.Runtime) string {
	if rt.Active("fulfilment-hot-partition") && Chance(0.31) {
		return HotMerchant
	}
	return fmt.Sprintf("m-%d", ID(1000, 1400))
}

func runOrders(ctx context.Context, a *App, port int) error {
	db, err := a.PG(ctx, "orders-db", 12, 2)
	if err != nil {
		return err
	}
	if a.RT.Scenarios["orders-db-connections"] != 0 {
		// The real pool: 25 connections per instance, kept open (min = max) — most sit idle.
		go func() {
			for !a.RT.Active("orders-db-connections") {
				select {
				case <-ctx.Done():
					return
				case <-time.After(5 * time.Second):
				}
			}
			idle, err := a.PG(ctx, "orders-db", 44, 44)
			if err != nil {
				a.Log.Warn("idle pool", "err", err)
				return
			}
			_ = idle.Ping(ctx)
			a.Log.Warn("connection pool warmed: pool_size=44 min_idle=44 db=orders-db")
		}()
	}
	cache := a.Redis("cache-main")
	kc, err := a.Producer()
	if err != nil {
		return err
	}
	rab := newRabbitPublisher(ctx, a)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cart := ID(1, 50000)
		if err := cache.Get(ctx, fmt.Sprintf("cart:%d", cart)).Err(); err != nil && !errors.Is(err, redis.Nil) {
			a.Log.WarnContext(ctx, "cache read failed", "err", err.Error())
		}
		t0 := time.Now()
		st, err := a.Call(ctx, "inventory-api", "POST", "/v1/reservations", `{"items":3}`)
		if d := time.Since(t0); d > 500*time.Millisecond {
			a.Log.WarnContext(ctx, fmt.Sprintf("inventory reservation slow duration_ms=%d sku=SKU-%d", d.Milliseconds(), ID(1000, 9999)))
		}
		if err != nil {
			a.Fail(w, r, http.StatusBadGateway, err, "inventory reservation failed", "status", st)
			return
		}
		if st == http.StatusConflict {
			JSON(w, http.StatusConflict, `{"error":"out_of_stock"}`)
			return
		}
		Work(ctx, 25, 0.5)
		total := float64(ID(2000, 90000)) / 100
		m := merchant(a.RT)
		var id int64
		err = pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			if err := tx.QueryRow(ctx, `INSERT INTO orders (customer_id, merchant_id, total, currency, status) VALUES ($1, $2, $3, $4, $5) RETURNING id`,
				ID(1, 50000), m, total, "TRY", "created").Scan(&id); err != nil {
				return err
			}
			skus := []string{fmt.Sprintf("SKU-%d", ID(1000, 9999)), fmt.Sprintf("SKU-%d", ID(1000, 9999))}
			_, err := tx.Exec(ctx, `INSERT INTO order_items (order_id, sku, qty, price) SELECT $1, unnest($2::text[]), unnest($3::int[]), unnest($4::numeric[])`,
				id, skus, []int32{1, 2}, []float64{total / 3, total / 3})
			return err
		})
		if err != nil {
			a.Fail(w, r, http.StatusInternalServerError, err, "create order failed", "err", err.Error())
			return
		}
		if Chance(0.004) {
			err := errors.New("TypeError: Cannot read properties of undefined (reading 'address')\n    at buildShipment (shipment.ts:88)\n    at createOrder (orders.controller.ts:57)")
			a.Fail(w, r, http.StatusInternalServerError, err, "TypeError: Cannot read properties of undefined (reading 'address') at buildShipment (shipment.ts:88)")
			return
		}
		_ = cache.SetEx(ctx, fmt.Sprintf("order:%d", id), `{"status":"created"}`, 5*time.Minute).Err()
		a.Produce(ctx, kc, "orders.created", m, fmt.Appendf(nil, `{"order_id":%d,"merchant_id":%q,"total":%.2f}`, id, m, total))
		rab.publish(ctx, "email.transactional.order_created", fmt.Sprintf(`{"template":"order_created","order_id":%d}`, id))
		a.Log.InfoContext(ctx, fmt.Sprintf("order created order_id=ord_%d total=%.2f currency=TRY", id, total), "merchant_id", m)
		JSON(w, http.StatusCreated, fmt.Sprintf(`{"id":%d}`, id))
	})
	mux.HandleFunc("GET /v1/orders/{id}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, _ := strconv.Atoi(r.PathValue("id"))
		if v, err := cache.Get(ctx, fmt.Sprintf("order:%d", id)).Result(); err == nil && v != "" && Chance(0.6) {
			JSON(w, 200, v)
			return
		}
		var status string
		var total float64
		err := db.QueryRow(ctx, `SELECT status, total FROM orders WHERE id = $1`, id).Scan(&status, &total)
		if errors.Is(err, pgx.ErrNoRows) {
			JSON(w, 404, `{"error":"not_found"}`)
			return
		}
		if err != nil {
			a.Fail(w, r, 500, err, "load order failed", "err", err.Error())
			return
		}
		Work(ctx, 6, 0.4)
		JSON(w, 200, fmt.Sprintf(`{"id":%d,"status":%q}`, id, status))
	})
	mux.HandleFunc("GET /v1/orders", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cust := ID(1, 50000)
		_ = cache.Get(ctx, fmt.Sprintf("customer:%d:orders", cust)).Err()
		rows, err := db.Query(ctx, `SELECT id, status, total, created_at FROM orders WHERE customer_id = $1 ORDER BY created_at DESC LIMIT $2`, cust, 20)
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
			err = rows.Err()
		}
		if err == nil {
			var n int
			err = db.QueryRow(ctx, `SELECT count(*) FROM orders WHERE customer_id = $1`, cust).Scan(&n)
		}
		if err != nil {
			a.Fail(w, r, 500, err, "list orders failed", "err", err.Error())
			return
		}
		Work(ctx, 30, 0.6)
		JSON(w, 200, `{"orders":[]}`)
	})
	mux.HandleFunc("POST /v1/orders/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		id, _ := strconv.Atoi(r.PathValue("id"))
		err := pgx.BeginFunc(ctx, db, func(tx pgx.Tx) error {
			var status string
			if err := tx.QueryRow(ctx, `SELECT status FROM orders WHERE id = $1 FOR UPDATE`, id).Scan(&status); err != nil {
				return err
			}
			if _, err := a.Call(ctx, "payments-api", "POST", "/v1/refunds", fmt.Sprintf(`{"order_id":%d}`, id)); err != nil {
				return fmt.Errorf("refund: %w", err)
			}
			if _, err := tx.Exec(ctx, `UPDATE orders SET status = $1, updated_at = now() WHERE id = $2`, "cancelled", id); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `UPDATE prices SET price = price WHERE sku IN (SELECT sku FROM order_items WHERE order_id = $1)`, id)
			return err
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			a.Fail(w, r, 500, err, fmt.Sprintf("cancel failed order_id=ord_%d: %v", id, err))
			return
		}
		_ = cache.Del(ctx, fmt.Sprintf("order:%d", id)).Err()
		a.Produce(ctx, kc, "orders.status-changed", strconv.Itoa(id), fmt.Appendf(nil, `{"order_id":%d,"status":"cancelled"}`, id))
		JSON(w, 200, `{"status":"cancelled"}`)
	})
	mux.HandleFunc("GET /v1/cart", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cart := ID(1, 50000)
		_ = cache.Get(ctx, fmt.Sprintf("cart:%d", cart)).Err()
		rows, err := db.Query(ctx, `SELECT ci.sku, ci.qty, p.price FROM cart_items ci JOIN prices p ON p.sku = ci.sku WHERE ci.cart_id = $1`, cart)
		if err == nil {
			for rows.Next() {
			}
			rows.Close()
		}
		_ = cache.SetEx(ctx, fmt.Sprintf("cart:%d", cart), `{"items":3}`, 30*time.Minute).Err()
		Work(ctx, 8, 0.4)
		JSON(w, 200, `{"items":[]}`)
	})
	mux.HandleFunc("PUT /v1/cart/items/{itemId}", func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		cart, sku := ID(1, 50000), "SKU-"+r.PathValue("itemId")
		if _, err := a.Call(ctx, "inventory-api", "GET", "/v1/stock/"+r.PathValue("itemId"), nil); err != nil {
			a.Fail(w, r, 502, err, "stock check failed")
			return
		}
		_, err := db.Exec(ctx, `INSERT INTO cart_items (cart_id, sku, qty) VALUES ($1, $2, $3) ON CONFLICT (cart_id, sku) DO UPDATE SET qty = EXCLUDED.qty`, cart, sku, ID(1, 4))
		if err == nil {
			_, err = db.Exec(ctx, `SELECT price FROM prices WHERE sku = $1`, sku)
		}
		if err != nil {
			a.Fail(w, r, 500, err, "cart update failed", "err", err.Error())
			return
		}
		_ = cache.Del(ctx, fmt.Sprintf("cart:%d", cart)).Err()
		_ = cache.SetEx(ctx, fmt.Sprintf("cart:%d:touched", cart), "1", time.Hour).Err()
		JSON(w, 200, `{"ok":true}`)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { JSON(w, 200, `{"status":"ok"}`) })

	go Every(ctx, 60*time.Second, func() {
		s := db.Stat()
		a.Log.Debug("pool stats", "total", s.TotalConns(), "idle", s.IdleConns(), "acquired", s.AcquiredConns())
	})
	return a.Serve(ctx, port, mux)
}

// ---------------------------------------------------------------- RabbitMQ publisher (orders-api, payments-api)

type rabbitPublisher struct {
	a  *App
	ch chan rabbitMsg
}

type rabbitMsg struct {
	ctx  context.Context
	key  string
	body string
}

// newRabbitPublisher keeps one channel and publishes through the SDK (producer span + headers).
func newRabbitPublisher(ctx context.Context, a *App) *rabbitPublisher {
	p := &rabbitPublisher{a: a, ch: make(chan rabbitMsg, 1000)}
	go func() {
		for ctx.Err() == nil {
			conn, err := amqp.Dial(world.RabbitURL())
			if err != nil {
				a.Log.Warn("rabbitmq dial", "err", err.Error())
				time.Sleep(3 * time.Second)
				continue
			}
			ch, err := conn.Channel()
			if err != nil {
				conn.Close()
				continue
			}
			closed := conn.NotifyClose(make(chan *amqp.Error, 1))
		loop:
			for {
				select {
				case <-ctx.Done():
					break loop
				case <-closed:
					break loop
				case m := <-p.ch:
					if err := trooperamqp.Publish(m.ctx, ch, world.ExchangeEmails, m.key, false, false,
						amqp.Publishing{ContentType: "application/json", DeliveryMode: amqp.Persistent, Body: []byte(m.body), Timestamp: time.Now()}); err != nil {
						a.Log.WarnContext(m.ctx, "publish email failed", "err", err.Error())
					}
				}
			}
			conn.Close()
		}
	}()
	return p
}

func (p *rabbitPublisher) publish(ctx context.Context, key, body string) {
	select {
	case p.ch <- rabbitMsg{ctx: context.WithoutCancel(ctx), key: key, body: body}:
	default:
	}
}
