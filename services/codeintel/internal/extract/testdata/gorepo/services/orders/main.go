package main

import (
	"database/sql"
	"net/http"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/redis/go-redis/v9"
	trooper "github.com/tasktrooper/telemetry-go"
)

func main() {
	trooper.Init(trooper.Config{Service: "orders-api"})
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/orders/{id}", getOrder)
	mux.HandleFunc("POST /v1/orders", createOrder)
	mux.Handle("/healthz", http.HandlerFunc(health))
	mux.HandleFunc("example.com/static/", health)
	_ = redis.NewClient(nil)
	// mux.HandleFunc("GET /commented", nope)
	http.ListenAndServe(":8080", mux)
}

func getOrder(w http.ResponseWriter, r *http.Request) {
	resp, _ := http.Get("https://inventory.internal.example.com/v1/stock")
	_ = resp
}

func createOrder(w http.ResponseWriter, r *http.Request) {
	db, _ := sql.Open("pgx", "")
	saveOrder(db)
	req, _ := http.NewRequest("POST", "https://api.stripe.com/v1/charges", nil)
	_ = req
}

func saveOrder(db *sql.DB) error { return nil }

func health(w http.ResponseWriter, r *http.Request) {}
