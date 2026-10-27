# tasktrooper — TaskTrooper Telemetry for Python

OpenTelemetry distribution for Python ≥ 3.9 (ADR-011, ADR-024): one-line init, OTLP/HTTP protobuf export of traces,
metrics and logs to the TaskTrooper ingest gateway, route-aware APM conventions (`trooper.*`, `.ai/contracts.md` §4),
continuous profiling (§24) and LLM monitoring for OpenAI / Anthropic (§25). Framework instrumentation comes from
OpenTelemetry contrib; the SDK never raises into your application.

## Install

```bash
pip install tasktrooper                       # core: OTel SDK + OTLP/HTTP exporter
pip install 'tasktrooper[fastapi,httpx]'      # + the instrumentations you need
pip install 'tasktrooper[all]'                # every supported instrumentation
```

Extras (each installs an OTel contrib instrumentor; `init()` enables whatever is installed **and** whose library is
importable, and silently skips the rest): `django`, `fastapi`, `starlette`, `flask`, `requests`, `httpx`, `urllib3`,
`aiohttp`, `sqlalchemy`, `psycopg`, `psycopg2`, `redis`, `celery`, `kafka` (kafka-python), `confluent-kafka`, `pika`,
`logging`; bundles `web` and `all`.

## Start

```python
import tasktrooper

tasktrooper.init(
    dsn="https://<ingest-key>@ingest.trooper.dev/<project>",   # or TROOPER_DSN
    service="orders-api",
    environment="production",
    version=os.environ.get("GIT_SHA"),                          # deploy marker
    sample_rate=1.0,                                            # ParentBased(TraceIdRatio)
)
# … then import / create your app
```

Call `init()` **before** the web app object is created (FastAPI / Flask / Starlette classes are patched at init). For
an app that already exists: `tasktrooper.instrument_app(app)`.

Zero-code alternatives:

```bash
TROOPER_DSN=... tasktrooper-run --service orders-api uvicorn app:app --workers 4
TROOPER_DSN=... tasktrooper-run --service orders-api --profiling gunicorn app:app
```

```python
import tasktrooper.auto  # noqa: F401 — init from TROOPER_* / OTEL_* env, first line of your entry module
```

`tasktrooper-run` puts a `sitecustomize` on `PYTHONPATH`, so worker processes started by uvicorn / gunicorn / celery
initialize too (an existing `sitecustomize` on the path is chained). `init()` is idempotent, returns a handle
(`force_flush()`, `shutdown()`; also `tasktrooper.force_flush()` / `tasktrooper.shutdown()`), registers an `atexit`
flush and never raises. Without a DSN nothing is exported but propagation and the conventions still work.

## What you get

| | |
|---|---|
| Resource | `service.name`, `service.version`, `deployment.environment.name`, `telemetry.distro.name=trooper-python`, `telemetry.distro.version` |
| Sampler | `ParentBased(TraceIdRatioBased(rate))`; root spans write `tt=r:<rate>` into W3C `tracestate` |
| Entry spans (local-root SERVER / CONSUMER) | `trooper.sample_rate` (upstream `tt=r:` rate for remote parents), `trooper.caller` (baggage `tt.svc`), `trooper.route` (normalized `http.route`), `trooper.process` (consumer), `trooper.entry.route` / `.method` |
| Every span below an entry | `trooper.entry.route` / `trooper.entry.method` |
| Outgoing requests | W3C `traceparent` + `baggage: tt.svc=<service>` |
| URLs | query **values** stripped (`?token=…` → `?token`) |
| Logs | stdlib `logging` → OTLP logs with trace/span ids (via `opentelemetry-instrumentation-logging` when installed) |
| Self-metric | `trooper.sdk.errors{component}` (meter `TaskTrooper.Telemetry`) counts every swallowed error |

### Route templates

The framework's own template is used and normalized like every TaskTrooper SDK (`/users/:id`):

| Framework | Template | `trooper.route` |
|---|---|---|
| FastAPI / Starlette | `/items/{item_id}`, `/files/{p:path}` | `/items/:item_id`, `/files/*p` |
| Flask | `/users/<int:user_id>`, `/f/<path:rest>` | `/users/:user_id`, `/f/*rest` |
| Django `path()` / `re_path()` | `users/<int:pk>/`, `^legacy/(?P<slug>[\w-]+)/$` | `/users/:pk`, `/legacy/:slug` |

Anything else: `tasktrooper.set_route("/orders/{id}")` inside the request. Raw ids in paths (numbers, UUIDs, hex,
ULIDs) become `:id` when only the URL is known.

### Workers

Celery tasks, kafka-python / confluent-kafka consumers and pika consumers produce CONSUMER entry spans;
`trooper.process` = `trooper.process` attribute → consumer group → Celery task name → destination. Celery retries set
`trooper.consumer.attempt` (`retries + 1`). With Celery's prefork pool prefer `tasktrooper-run celery -A app worker`.

## Continuous profiling

```python
tasktrooper.init(profiling=True)                                        # cpu
tasktrooper.init(profiling={"types": ["cpu", "wall"], "hz": 100})
```

A daemon thread samples `sys._current_frames()` (default 100 Hz for 10 s of every 60 s) and uploads folded stacks to
`POST /v1/profiles` (gzip, `runtime=python`) once per period; failed uploads are dropped with 1 s → 60 s backoff on
429 / 5xx. Frames are `qualname (path/to/file.py)` (relative to cwd, site-packages or the stdlib).

| Type | Unit | |
|---|---|---|
| `cpu` (default) | ns | Linux: per-thread CPU clocks (`pthread_getcpuclockid`), exact. Elsewhere: a sample counts unless the thread looks idle (leaf frame in `selectors.select`, `threading` waits, `queue.get`, socket / ssl reads, `concurrent.futures` workers …, or calling `sleep` / `acquire` / `wait` / `recv*` / `accept` / `select` / `poll` / `join` on the current line). C extensions blocking inside their own call (e.g. a DB driver's `execute`) count as CPU there. |
| `wall` | ns | every sample of every thread |
| `inuse_space` | bytes | live allocations from `tracemalloc` at the end of each period. **Overhead:** `tracemalloc` slows allocation-heavy code (often 10–30 %) and keeps per-block metadata; use it for investigations. |

Samples carry the **entry route of the thread** (`# trooper.route=… trooper.method=…` sections), so flame graphs can
be filtered per endpoint. Attribution is per thread: sync frameworks (Flask, Django, FastAPI `def` endpoints in the
thread pool) are exact; under asyncio all tasks share the loop thread and the most recently started request wins.
The sampler restarts in forked children (`os.register_at_fork`) and stops on `shutdown()`.

## AI / LLM monitoring

```python
from openai import OpenAI, AsyncOpenAI
from anthropic import Anthropic

client = tasktrooper.instrument_openai(OpenAI())        # also AsyncOpenAI, AzureOpenAI, OpenAI-compatible base_url
claude = tasktrooper.instrument_anthropic(Anthropic())  # also AsyncAnthropic
```

Each call becomes an OTel GenAI CLIENT span `"<operation> <model>"`: `gen_ai.provider.name` (from `base_url`: Azure,
Groq, DeepSeek, xAI, Mistral, Gemini, OpenRouter, Ollama … else `openai`), `gen_ai.operation.name`, request / response
model, `gen_ai.usage.input_tokens` (**includes** cache read / write tokens), `output_tokens`,
`cache_read.input_tokens`, `cache_creation.input_tokens`, finish reasons, response id, request parameters,
`trooper.llm.ttft_ms` for streams, `error.type` (provider error type, e.g. `rate_limit_error`) on failures.

Covered: OpenAI `chat.completions.create` / `.parse`, `responses.create` / `.parse`, `embeddings.create`;
Anthropic `messages.create`, `messages.stream()` (+ `beta.messages.*`) — sync, async, `stream=True` and async iteration.
The wrappers are proxies: nothing global is patched, `isinstance(client, OpenAI)` still holds, and the provider SDKs
are not dependencies. OpenAI chat streams only report tokens with `stream_options={"include_usage": True}`;
`instrument_openai(client, inject_stream_usage=True)` adds it.

Recent `anthropic` SDKs (2.x) emit GenAI spans themselves; `instrument_anthropic` then returns the client unchanged
(no duplicate span) and the trooper span processor maps those spans to the names above (cache write tokens, TTFT,
finish reason).

**Content** (prompts / completions as GenAI message JSON, ≤ 32 KB per attribute) is off by default (PII):
`init(llm={"capture_content": True})`, `instrument_openai(client, capture_content=True)` or
`TROOPER_LLM_CAPTURE_CONTENT=true`.

Other providers: `with tasktrooper.llm.start("gcp.gemini", "gemini-2.5-pro") as s: …; s.end(input_tokens=…, output_tokens=…)`.

## Configuration

Precedence: explicit argument > `OTEL_*` > `TROOPER_*` > default (`.ai/contracts.md` §2).

| Argument | Environment | Default |
|---|---|---|
| `dsn` | `TROOPER_DSN` (`OTEL_EXPORTER_OTLP_ENDPOINT` / `…_TRACES_ENDPOINT` … override the URL, `OTEL_EXPORTER_OTLP_HEADERS` adds headers) | none → no export |
| `service` | `OTEL_SERVICE_NAME`, `OTEL_RESOURCE_ATTRIBUTES` `service.name`, `TROOPER_SERVICE` | `unknown_service:python` |
| `environment` | `OTEL_RESOURCE_ATTRIBUTES` `deployment.environment.name`, `TROOPER_ENV` | — |
| `version` | `OTEL_RESOURCE_ATTRIBUTES` `service.version`, `TROOPER_VERSION` | — |
| `sample_rate` | `OTEL_TRACES_SAMPLER_ARG`, `TROOPER_SAMPLE_RATE` | `1.0` |
| `enabled` | `TROOPER_ENABLED` | `true` (`false`: no providers, nothing patched) |
| `instrument` (`True` / `False` / list of names) | — | `True` |
| `disable_instrumentations=[…]` | `TROOPER_DISABLE_INSTRUMENTATIONS` (comma list, e.g. `django,redis`) | — |
| `profiling` (`True` / dict) | `TROOPER_PROFILING`, `TROOPER_PROFILING_TYPES`, `TROOPER_PROFILING_PERIOD` (s), `TROOPER_PROFILING_CPU_DURATION` (s), `TROOPER_PROFILING_HZ` | off; `cpu`, 60 s, 10 s, 100 Hz (env ignored in AWS Lambda) |
| `llm={"capture_content", "inject_stream_usage"}` | `TROOPER_LLM_CAPTURE_CONTENT` | `false` |
| `debug` | `TROOPER_DEBUG` | `false` (logs swallowed errors on logger `tasktrooper`) |
| `traces` / `metrics` / `logs` | — | `True` |
| `resource_attributes`, `metric_interval_s` (30), `export_timeout_s` (10), `batch_delay_s` (5), `max_queue_size` (2048) | — | |

Local dev DSN: `http://tt_dev_ingest_key@localhost:4318/1`.

## Example

`examples/fastapi_openai/app.py` — FastAPI + OpenAI (fake transport unless `OPENAI_API_KEY` is set) + profiling.

## Development

```bash
cd sdks/python
uv venv .venv && uv pip install --python .venv/bin/python -e '.[dev,django]'
.venv/bin/python -m pytest -q                      # unit + contract tests
.venv/bin/python -m pytest -q -m live              # against a running ingest-gw + writer + ClickHouse (skips otherwise)
```

`tests/test_live.py` sends a FastAPI request (caller baggage, DB child), an LLM span and CPU/wall profiles to
`TROOPER_E2E_DSN` (default dev DSN) and checks `spans`, `llm_1m` and `profile_stacks` in ClickHouse
(`TROOPER_E2E_CLICKHOUSE`, default `http://localhost:8123`); its rows are deleted afterwards.
