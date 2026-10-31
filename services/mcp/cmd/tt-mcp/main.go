// Command tt-mcp runs the TaskTrooper MCP server for local coding agents (ADR-025).
//
// STDOUT IS THE PROTOCOL CHANNEL. The server speaks MCP over stdin/stdout
// (mcp.StdioTransport), so every byte written to stdout must be a JSON-RPC
// message. All logging goes to stderr and nothing else may write to stdout:
// do not use services/pkg/logx here (it logs to os.Stdout), and do not add
// fmt.Print*. The end-to-end test (cmd/tt-mcp/main_test.go) asserts that a real
// stdio session produces nothing but JSON-RPC lines on stdout — keep it green.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/tasktrooper/telemetry/services/mcp/internal/apiclient"
	"github.com/tasktrooper/telemetry/services/mcp/internal/config"
	"github.com/tasktrooper/telemetry/services/mcp/internal/server"
)

func main() {
	// Fail fast, loudly and on stderr: stdout belongs to the protocol.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "tt-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}

	// logx logs to stdout — forbidden here — so build a stderr logger directly.
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: cfg.LogLevel,
	})).With("service", "mcp")

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := apiclient.New(cfg.APIURL, cfg.APIKey, 15*time.Second)
	if err := client.Check(ctx, log, cfg.OrgID, cfg.ProjectID); err != nil {
		return err
	}

	srv := server.New(client, log)
	// Run connects the stdio transport and serves until ctx is cancelled. It
	// owns the only writer on os.Stdout.
	log.Info("tt-mcp listening on stdio", "tools", server.ToolCount, "api", cfg.APIURL)
	if err := srv.Run(ctx, &mcp.StdioTransport{}); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("stdio transport: %w", err)
	}
	return nil
}
