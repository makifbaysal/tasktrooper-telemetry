# Changelog

## 0.1.0 — unreleased

First version (ADR-024).

- `tasktrooper.init()` — OpenTelemetry distribution: OTLP/HTTP protobuf (gzip) traces, metrics and logs to the DSN's
  ingest gateway (`x-trooper-key`), resource with `telemetry.distro.name=trooper-python`, config precedence
  explicit > `OTEL_*` > `TROOPER_*`, idempotent, never raises, `atexit` flush.
- Trooper sampler (`ParentBased(TraceIdRatioBased)` + `tt=r:<rate>` tracestate), propagator (W3C trace context +
  baggage `tt.svc`) and span processor (`trooper.route`, `trooper.entry.route/method`, `trooper.caller`,
  `trooper.process`, `trooper.consumer.attempt`, `trooper.sample_rate`, query-value stripping, native GenAI span
  aliases).
- Auto-instrumentation through OTel contrib extras (Django, FastAPI, Starlette, Flask, requests, httpx, urllib3,
  aiohttp-client, SQLAlchemy, psycopg/psycopg2, redis, Celery, kafka-python, confluent-kafka, pika, logging);
  `instrument_app(app)`, `set_route()`.
- `tasktrooper-run` launcher and `import tasktrooper.auto`.
- Continuous profiling: `sys._current_frames()` sampler, `cpu` (Linux per-thread CPU clocks, idle heuristic
  elsewhere), `wall`, `inuse_space` (tracemalloc), folded upload with route sections, fork-safe.
- LLM monitoring: `instrument_openai`, `instrument_anthropic` (sync, async, streams, TTFT, cache tokens, error type,
  opt-in content capture), `tasktrooper.llm.start()` for other providers.
