// Command gin is an example gin service instrumented with trooper-go.
//
//	TROOPER_DSN=http://tt_dev_ingest_key@localhost:4318/1 go run ./gin
package main

import (
	"context"
	"database/sql"
	"errors"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gin-gonic/gin"

	trooper "github.com/tasktrooper/telemetry-go"
	troopergin "github.com/tasktrooper/telemetry-go/integrations/gin"
	trooperhttp "github.com/tasktrooper/telemetry-go/integrations/nethttp"
	trooperslog "github.com/tasktrooper/telemetry-go/integrations/slog"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	shutdown, err := trooper.Start(ctx, trooper.Config{
		Service:     "orders-api",
		Environment: "dev",
		Version:     "example",
	}) // DSN from TROOPER_DSN
	if err != nil {
		log.Printf("telemetry disabled: %v", err)
	}
	defer func() {
		c, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdown(c)
	}()

	logger := trooperslog.New("orders-api")
	client := trooperhttp.NewClient(trooperhttp.WithPeerService("inventory"))

	r := gin.New()
	r.Use(gin.Recovery()) // responds 500 after trooper re-panics
	troopergin.Use(r)     // spans, trooper.route, entry route, caller, panic capture
	r.GET("/orders/:id", func(c *gin.Context) {
		ctx := c.Request.Context()
		logger.InfoContext(ctx, "fetching order", slog.String("order_id", c.Param("id")))

		// Outgoing call: carries traceparent + baggage tt.svc=orders-api; span has trooper.entry.route=/orders/:id.
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost:9/stock", nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
		} else {
			trooper.CaptureError(ctx, err)
		}
		if c.Param("id") == "0" {
			trooper.CaptureError(ctx, sql.ErrNoRows)
			c.JSON(http.StatusNotFound, gin.H{"error": "not found"})
			return
		}
		c.JSON(http.StatusOK, gin.H{"id": c.Param("id")})
	})

	srv := &http.Server{Addr: ":8081", Handler: r}
	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
