// Command ai-demo exercises the AI provider without Postgres, ClickHouse, or a web server.
// With no AI environment variables it is entirely offline and costs nothing.
package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/tasktrooper/telemetry/services/api/internal/ai"
)

func main() {
	provider, err := ai.NewFromEnv()
	if err != nil {
		log.Fatal(err)
	}
	result, err := provider.Summarize(context.Background(), ai.Input{
		Title:    "Checkout errors rose after deploy",
		Evidence: []string{"5xx rate increased from 0.2% to 8%", "p95 latency increased from 220ms to 900ms"},
	})
	if err != nil {
		log.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
		log.Fatal(err)
	}
}
