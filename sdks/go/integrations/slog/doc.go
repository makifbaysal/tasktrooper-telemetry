// Package trooperslog bridges log/slog to OpenTelemetry logs (via otelslog and the global
// LoggerProvider installed by trooper.Start) and, at the same time, writes to a local handler
// with trace_id / span_id injected from the context, so stdout logs can be correlated too.
//
//	logger := slog.New(trooperslog.NewHandler("orders-api", slog.NewJSONHandler(os.Stdout, nil)))
//	slog.SetDefault(logger)
//	logger.InfoContext(ctx, "order created", "order_id", id) // use *Context methods for correlation
package trooperslog
