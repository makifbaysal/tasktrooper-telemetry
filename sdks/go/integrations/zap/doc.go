// Package trooperzap bridges go.uber.org/zap to OpenTelemetry logs via otelzap and the global
// LoggerProvider installed by trooper.Start.
//
//	local := zapcore.NewCore(zapcore.NewJSONEncoder(zap.NewProductionEncoderConfig()), os.Stdout, zap.InfoLevel)
//	logger := zap.New(trooperzap.Tee(local, "orders-api"))
//	logger.Info("order created", trooperzap.Context(ctx), zap.String("order_id", id))
//
// trooperzap.Context(ctx) gives the OTel record its trace context; in the local core it is
// replaced by trace_id / span_id fields. TraceFields(ctx) returns those fields explicitly.
package trooperzap
