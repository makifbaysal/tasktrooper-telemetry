# SDK tasarımı

## İlkeler

1. **Tek satır kurulum, sıfır config ile anlamlı veri.** Auto-instrumentation default açık.
2. **Host'u asla bozma.** Exception yutulur; overhead bütçesi: < %2 CPU, < 20 MB memory, istek başı < 50 µs.
3. **OpenTelemetry uyumlu.** Backend SDK'ları OTel SDK'yı sarar; kendi exporter + preset'lerimizi ekler. Kullanıcı OTel API'si ile yazdığı custom span'lar da bize gelir.
4. **Gizlilik default.** Body/header/query toplanmaz; IP sunucuda geo'ya çevrilip atılır; `beforeSend` hook'u ile maskeleme.
5. **Batch + backoff + offline kuyruk.** Browser'da `sendBeacon` ile sayfa kapanırken flush; mobilde disk kuyruğu.

## Paketler

Backend SDK'ları OpenTelemetry dağıtımıdır (ADR-011): OTel SDK + bizim preset'lerimiz (DSN, resource, sampler, `trooper.*`
attribute'ları). Client SDK'ları kendi hafif RUM katmanıdır ve `POST /v1/rum`'a JSON yollar.

| Paket | Klasör | Hedef | Kapsam |
|---|---|---|---|
| `github.com/tasktrooper/telemetry-go` (+ `integrations/*` ayrı modüller) | `sdks/go` | Go | net/http (ServeMux pattern), gin, fiber, echo, chi, grpc, franz-go, sarama, amqp091, database/sql, pgx, go-redis, slog/zap |
| `trooper-javaagent` (OTel agent + extension) | `sdks/jvm` | JVM (kod değişikliği yok) | OTel Java agent'ın tüm auto-instrumentation'ı + trooper preset'leri |
| `trooper-spring-boot-starter` | `sdks/jvm` | Spring Boot 3 | MVC/WebFlux route template, `@KafkaListener`/`@RabbitListener` process adı |
| `trooper-quarkus` | `sdks/jvm` | Quarkus 3 | quarkus-opentelemetry config'i, JAX-RS route template, reactive messaging (SmallRye Kafka/RabbitMQ consumer'larında `trooper.consumer.attempt` otomatik: `trooper` propagator'ı extract sırasında `tt-attempt` → `kafka_deliveryAttempt` → `x-delivery-count`+1 → `x-death`+1 → `delayed-retry-count`+1 okur) |
| `trooper-llm` | `sdks/jvm` | JVM | GenAI span API'si (`TrooperLlm`), Anthropic SDK sarmalayıcısı, LangChain4j listener — ADR-023 |
| `trooper-ktor` + `trooper-kotlin` | `sdks/jvm` | Kotlin / Ktor 3 | server + client plugin, coroutine context |
| `TaskTrooper.Telemetry` (+ `.AspNetCore`, `.Kafka`, `.RabbitMQ`, `.MassTransit`, `.EntityFrameworkCore`, `.Redis`) | `sdks/dotnet` | .NET 8+ | ASP.NET Core, HttpClient, SqlClient/EF Core, Npgsql, Redis, gRPC, Confluent.Kafka, RabbitMQ, MassTransit |
| `@trooper/node` | `sdks/js/packages/node` | Node.js ≥ 22.15 | `@opentelemetry/sdk-node` + OTLP/HTTP protobuf (traces/metrics/logs); http, undici, express, koa, NestJS, kafkajs, amqplib, pg, mysql2, mongodb, ioredis/redis, pino/winston log bridge; route template helper'ları (express, fastify, koa, Hono, NestJS interceptor), BullMQ processor wrapper'ı, `tt-attempt` okuma/yazma |
| `@trooper/core` + `@trooper/browser` | `sdks/js/packages` | Web | page view, SPA route, Web Vitals, fetch/XHR + traceparent, JS error, click (rage/dead), sendBeacon, offline kuyruk |
| `@trooper/replay` | `sdks/js/packages` | Web | session replay (rrweb kayıt, maskeleme, örnekleme) — ADR-021 |
| `@trooper/react`, `@trooper/vue`, `@trooper/angular` | `sdks/js/packages` | Web framework'leri | ErrorBoundary/errorHandler, router entegrasyonu (react-router, Next.js, TanStack Router, vue-router, Angular Router) → route template view adı |
| `@trooper/react-native` | `sdks/js/packages` | React Native | react-navigation ekranları, app start, JS hataları, network, AppState flush |
| `trooper-android` (+ `-okhttp`, `-compose`) | `sdks/android` | Android | Activity/Fragment/Compose ekranları, app start, crash, ANR, frozen frame, OkHttp |
| `TaskTrooper` (SPM + CocoaPods) | `sdks/ios` | iOS | UIKit/SwiftUI ekranları, app start, crash (signal + NSException), hang, URLSession |
| `trooper_flutter` (+ `trooper_dio`) | `sdks/flutter` | Flutter | Navigator/go_router, Flutter/Dart hataları, http/dio, frame timing, app start |
| `tasktrooper` (PyPI, ADR-024) | `sdks/python` | Python ≥ 3.9 | OTel dağıtımı (django, fastapi, flask, celery, sqlalchemy, requests, httpx — OTel contrib extras), route template, profiling, `instrument_openai` / `instrument_anthropic` |

Kütüphane başına özel kod sadece OTel'in vermediği bilgi için yazılır: route template (`trooper.route`), consumer process adı,
çağıran servis (`tt.svc` baggage). Geri kalan her şey OTel instrumentation'ından gelir.

## API yüzeyi

### Node

```ts
// ESM: node --import @trooper/node/register app.mjs   (CJS: --require; env'den init eder)
import { init } from '@trooper/node';

init({
  dsn: process.env.TROOPER_DSN,           // https://<ingest-key>@ingest.trooper.dev/<project>
  service: 'orders-api',
  environment: 'production',
  version: process.env.GIT_SHA,           // deploy marker
  sampleRate: 1.0,                        // ParentBased(TraceIdRatio); kök span tracestate'e tt=r:<rate> yazar
});
```

`init` idempotent'tir ve asla throw etmez; DSN yoksa export etmez (propagation çalışır). Config önceliği contracts.md §2:
explicit > `OTEL_*` > `TROOPER_*`. Resource: `service.*`, `deployment.environment.name`, `telemetry.distro.name=trooper-node`.
Span processor: local-root SERVER/CONSUMER span'ı entry olur; alt span'lara `trooper.entry.route/method` kopyalanır (route,
framework router'ı span'dan sonra eşleştirdiği için lazy resolver / OTel `RPCMetadata` / `http.route` üzerinden çözülür),
`trooper.caller` baggage `tt.svc`'den, `trooper.sample_rate` upstream `tt=r:`'den. URL attribute'larında query değerleri silinir.

Route template helper'ları: `app.use(trooperExpress())`, `app.register(trooperFastify)`, `trooperKoa()`, `trooperHono()`,
`app.useGlobalInterceptors(new TrooperInterceptor())` (NestJS). Elle: `setRoute('/users/:id')`.

Worker/consumer için ek API:

```ts
import { processMessage, instrumentBullmqProcessor, withTraceContext, injectHeaders, nextAttempt, setAttemptHeader, markDlq } from '@trooper/node';

// kafkajs / amqplib: upstream OTel instrumentation + hook → trooper.consumer.attempt (tt-attempt, x-delivery-count, x-death)
new Worker('emails', instrumentBullmqProcessor(async (job) => send(job.data)));     // attempt = attemptsStarted
await queue.add('welcome', withTraceContext({ to }));                                // BullMQ'da header yok → job data
await processMessage({ system: 'kafka', destination: 'orders', consumerGroup: 'billing', headers }, handler);
producer.send({ topic: 'orders.retry', messages: [{ value, headers: setAttemptHeader(injectHeaders(), nextAttempt()) }] });
```

Yutulan her SDK hatası `trooper.sdk.errors{component}` (meter `TaskTrooper.Telemetry`) olarak sayılır; export hataları
`component=exporter.<signal>`.

### Browser

```ts
import { init, identify, track } from '@trooper/browser';

init({ dsn, service: 'shop-web', environment: 'production', version, tracePropagationTargets: [/api\.shop\.com/] });
identify({ id: user.id, email: user.email, plan: 'pro' });   // opt-in
track('checkout_started', { cart_value: 120 });
```

`tracePropagationTargets` → fetch/XHR'a `traceparent` header'ı eklenir, frontend view ile backend trace birleşir.

Client SDK'lar (browser, React Native, Android OkHttp, iOS URLSession, Flutter http/Dio) `traceparent` ekledikleri her isteğe
`baggage: tt.svc=<app>` da ekler (değer percent-encoded; mevcut `baggage` header'ı korunur, eski `tt.svc` değiştirilir, toplam ≤ 8192 byte).
Backend bunu `trooper.caller` olarak okur, çağrı genel `web`/`mobile` yerine uygulama adıyla görünür. Cross-origin müşteri API'leri
CORS `Access-Control-Allow-Headers`'a `traceparent` ile birlikte `baggage`'ı da eklemeli.

### Session replay (ADR-021, contracts §23)

```ts
import { init } from '@trooper/browser';
import { replayIntegration } from '@trooper/replay';

init({ dsn, service: 'shop-web', integrations: [
  replayIntegration({ sampleRate: 0.1, onErrorSampleRate: 1.0, maskAllText: true, maskAllInputs: true }),
] });
```

`@trooper/replay` `@rrweb/record`'u sarar (ayrı paket; `@trooper/browser` boyutu değişmez). CDN: `trooper-replay.iife.js`
`window.TrooperReplay`. Oturum ve hata bilgisini `@trooper/browser`'ın integration kancalarından alır.

React Native: `import { startReplay } from '@trooper/react-native'; startReplay({ sampleRate: 0.1 })` (native SDK gerekir).
Flutter: `TaskTrooper.init(config, appRunner: …, replay: const TrooperReplayOptions(sampleRate: 0.1))` — Dart tarafı wireframe; unmask `TrooperUnmask(child: …)`.

Mobil (wireframe, ekran görüntüsü yok):
- Android: `TrooperReplay.install(context, ReplayOptions(sampleRate = 0.1, onErrorSampleRate = 1.0))` (`trooper-replay` modülü; offline kuyruk ≤ 2 MB, dolunca yeni segment düşer);
  Compose için `trooper-compose` semantics üzerinden. Unmask: `view.trooperUnmask()`, `Modifier.trooperUnmask()`.
- iOS: `TaskTrooper.start(config, replay: .init(sampleRate: 0.1, onErrorSampleRate: 1.0))` (`TaskTrooperReplay` target'ı,
  pod `TaskTrooper/Replay`; crash'te on-error buffer flush edilemez — signal handler); unmask
  `view.trooperUnmask = true`, SwiftUI `.trooperUnmask()`.

### Continuous profiling (ADR-022, contracts §24)

Varsayılan kapalı; `TROOPER_PROFILING=true` veya kodda:
- Go: `trooper.Start(ctx, trooper.Config{…}, trooper.WithProfiling())` — `runtime/pprof` (CPU ve goroutine pprof; heap/mutex/block kümülatif olduğu için periyot farkı folded olarak gönderilir, ilk periyot sadece baseline); HTTP/gRPC/consumer entry'lerinde goroutine'e
  `trooper.route` / `trooper.method` pprof label'ı konur → endpoint bazında flame graph.
- JVM: `-Dtrooper.profiling.enabled=true` (javaagent extension; JFR `RecordingStream`, JDK 17+). Spring Boot starter
  `trooper.profiling.enabled`. Sürekli örnekleme (CPU 20 ms; `CPU_DURATION` yok sayılır), default tipler `cpu,alloc_space,lock`
  (JFR'de in-use heap yok). Thread → entry span eşlemesiyle route etiketi (`# trooper.route=`).
- Node: `init({ profiling: true })` — `node:inspector` CPU + sampling heap profiler, folded formatta.
- .NET: `TaskTrooper.Telemetry.Profiling` paketi, `services.AddTaskTrooperProfiling()` (EventPipe, süreç içi).
- Python: `tasktrooper.init(profiling=True)` (örnekleyici thread; `TROOPER_PROFILING=true`).
- Herhangi bir dil: OTLP profiles (`/v1development/profiles`) — ör. OTel eBPF profiler / Collector.

### AI / LLM (ADR-023, contracts §25)

LLM çağrıları OTel GenAI span'ı olarak gider; içerik (`captureContent`) varsayılan kapalı.
- Node: `instrumentOpenAI(new OpenAI())`, `instrumentAnthropic(new Anthropic())` (stream dahil token + TTFT); Vercel AI SDK
  `experimental_telemetry: { isEnabled: true }` span'ları olduğu gibi gelir. `init({ llm: { captureContent: true } })`.
- Go (`github.com/tasktrooper/telemetry-go/llm`): `client := openai.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))` / Anthropic aynı (`llm.HTTPClient` bilinen
  provider host'larını tanır, request/response JSON'undan model + token okur, SSE stream'i bozmadan sayar); elle:
  `ctx, call := llm.Start(ctx, llm.Request{Provider: "anthropic", Model: m, Operation: "chat"}); defer call.End(llm.Usage{…})`.
- JVM (`trooper-llm` modülü — sadece `opentelemetry-api`, agent'ın yanında uygulama kodundan görünür; Spring starter içerir):
  Spring AI span'ları normalize edilir, LangChain4j için `TrooperChatModelListener`, Anthropic Java SDK `TrooperLlm.wrap(client)`,
  OpenAI Java SDK javaagent'ın upstream instrumentation'ı ile; elle `TrooperLlm.start(provider, model)` → `LlmCall`.
- .NET: `Microsoft.Extensions.AI`, `OpenAI.*`, `Azure.AI.Inference*`, `Microsoft.SemanticKernel*` ActivitySource'ları varsayılan
  dinlenir (`EnableLlm`, `LlmCaptureContent`); elle `TrooperLlm.Start(provider, model, op)`.

### Serverless (AWS Lambda, Azure Functions — ADR-020)

Metrikler (invocations, errors, throttles, duration, concurrency …) SDK'sız gelir: AWS/Azure entegrasyonu
(`services/integrations`, contracts §21) CloudWatch / Azure Monitor'dan çeker. **Trace ve cold start** için fonksiyon OTel
ile enstrümante edilir; span'ların `service.name`'i `serverless_functions.service` ile aynı olmalı (varsayılan fonksiyon /
app adı; override: Lambda tag'i veya Function App tag'i `tasktrooper.service`).

**Azure fonksiyon bazında metrik:** Function App'e bağlı bir Application Insights bileşeni varsa (`hidden-link` tag'i)
entegrasyon her fonksiyonu ayrı satır (`azure-<app>-<function>`) olarak açar ve metriklerini App Insights `requests`
tablosundan (ARM proxy, ek rol gerekmez) çeker; p95/p99 de gelir. `service` = fonksiyonun en sık `cloud_RoleName`'i — span
eşlemesi için `OTEL_SERVICE_NAME` (veya App Insights'ta cloud role name) ile aynı tutulmalı. Opsiyonel
`config.readAppSettings: true` (Website Contributor gerekir) `TASKTROOPER_SERVICE` / `OTEL_SERVICE_NAME` app setting'ini
okuyup `service`'i ezer; yetki yoksa sessizce atlanır, başka hiçbir ayar saklanmaz. Detay: contracts §22,
`services/integrations/README.md` § Azure.

**Node.js Lambda — `@trooper/node`:**

```ts
import { init, wrapLambda } from '@trooper/node';
init({ service: 'orders-api' });                       // modül seviyesinde; DSN env'den (TROOPER_DSN)
export const handler = wrapLambda(async (event, context) => ({ statusCode: 200, body: 'ok' }));
```

`wrapLambda(handler, opts?)`: ilk çağrıda `faas.coldstart=true` (sonra `false`), `faas.invocation_id` = `awsRequestId`,
`faas.name` = `AWS_LAMBDA_FUNCTION_NAME`, `faas.trigger` event şeklinden (API Gateway / Function URL / ALB → `http`,
SQS/SNS/Kinesis/EventBridge event → `pubsub`, DynamoDB/S3 → `datasource`, EventBridge schedule → `timer`), `cloud.provider=aws`.
HTTP event'te SERVER span (`GET /users/:id`, `traceparent` devam eder), kuyruk batch'inde CONSUMER span. Dönmeden önce
flush eder (en fazla `flushTimeoutMs`, varsayılan 2 sn); async ve callback handler'ları destekler; telemetri hatası asla
handler'a yansımaz.

**Diğer diller — OTel / ADOT Lambda layer:** dil layer'ı (ör. `aws-otel-python-*`, `opentelemetry-nodejs-*`) + env:

```bash
AWS_LAMBDA_EXEC_WRAPPER=/opt/otel-instrument          # node/java: /opt/otel-handler
OTEL_SERVICE_NAME=orders-api
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.trooper.dev
OTEL_EXPORTER_OTLP_HEADERS=x-trooper-key=<ingest key>
OTEL_RESOURCE_ATTRIBUTES=deployment.environment.name=production
```

Collector layer'ı (ADOT) kullanılıyorsa SDK `localhost:4318`'e yollar; collector config'inde (`OPENTELEMETRY_COLLECTOR_CONFIG_URI`)
`otlphttp` exporter'ı `endpoint: https://ingest.trooper.dev`, `headers: {x-trooper-key: <ingest key>}` ile tanımlanır.
OTel semconv Lambda instrumentation'ı `faas.coldstart` / `faas.trigger` / `faas.invocation_id`'yi kendisi yazar.

**Azure Functions — OpenTelemetry modu:** `host.json`:

```json
{ "version": "2.0", "telemetryMode": "OpenTelemetry" }
```

App settings (Function App → Environment variables):

```bash
OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.trooper.dev
OTEL_EXPORTER_OTLP_PROTOCOL=http/protobuf
OTEL_EXPORTER_OTLP_HEADERS=x-trooper-key=<ingest key>
OTEL_SERVICE_NAME=billing-func                         # = Function App adı veya tag tasktrooper.service
```

Worker tarafında da OTel açılır (.NET isolated: `builder.Services.AddOpenTelemetry().UseFunctionsWorkerDefaults().UseOtlpExporter()`;
Node: `@azure/functions-opentelemetry-instrumentation` + OTLP exporter; Python: `PYTHON_ENABLE_OPENTELEMETRY=true`).
Host span'ları `faas.trigger`, `faas.invocation_id` (ve host destekliyorsa `faas.coldstart`) taşır; cold start sayısı api'de
`faas.coldstart='true'` entry span'larından hesaplanır.

## Wire formatı

- Backend: **OTLP/HTTP protobuf** → `POST /v1/traces`, `/v1/metrics`, `/v1/logs`. Header: `x-trooper-key`.
- Replay segment'leri → `POST /v1/replay` (contracts §23); profiller → `POST /v1/profiles` (pprof veya folded, §24).
- Browser/mobil: **RUM batch JSON** → `POST /v1/rum` (gzip). Şema `proto/rum/v1/rum.proto` içinde tanımlı, JSON mapping'i kullanılır (alan adları snake_case, enum'lar isim olarak).

RUM event tipleri: `view_start`, `view_end`, `vital`, `resource`, `error`, `action`, `custom`, `session_start`.
Ortak alanlar: `session_id`, `view_id`, `anonymous_id`, `user_id?`, `ts`, `service`, `version`, `env`, `device`, `connection`.

## Semantic convention'lar

OTel semantic conventions esas. `trooper.*` attribute'larının tam listesi ve writer'ın bunları nasıl okuduğu:
[`contracts.md` § 4](contracts.md#4-semantic-conventionlar-sdk--writer). Özet: `trooper.route`, `trooper.entry.route`
(isteğin tüm span'larında), `trooper.process`, `trooper.caller` (baggage `tt.svc`), `trooper.consumer.attempt`,
`trooper.consumer.dlq`, `trooper.sample_rate`.

## Versiyonlama

Semver. Wire protokolünde kırıcı değişiklik → yeni endpoint versiyonu (`/v2/rum`), eski versiyon en az 12 ay desteklenir.
