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
| `trooper-ktor` + `trooper-kotlin` | `sdks/jvm` | Kotlin / Ktor 3 | server + client plugin, coroutine context |
| `TaskTrooper.Telemetry` (+ `.AspNetCore`, `.Kafka`, `.RabbitMQ`, `.MassTransit`, `.EntityFrameworkCore`, `.Redis`) | `sdks/dotnet` | .NET 8+ | ASP.NET Core, HttpClient, SqlClient/EF Core, Npgsql, Redis, gRPC, Confluent.Kafka, RabbitMQ, MassTransit |
| `@trooper/node` | `sdks/js/packages/node` | Node.js ≥ 22.15 | `@opentelemetry/sdk-node` + OTLP/HTTP protobuf (traces/metrics/logs); http, undici, express, koa, NestJS, kafkajs, amqplib, pg, mysql2, mongodb, ioredis/redis, pino/winston log bridge; route template helper'ları (express, fastify, koa, Hono, NestJS interceptor), BullMQ processor wrapper'ı, `tt-attempt` okuma/yazma |
| `@trooper/core` + `@trooper/browser` | `sdks/js/packages` | Web | page view, SPA route, Web Vitals, fetch/XHR + traceparent, JS error, click (rage/dead), sendBeacon, offline kuyruk |
| `@trooper/react`, `@trooper/vue`, `@trooper/angular` | `sdks/js/packages` | Web framework'leri | ErrorBoundary/errorHandler, router entegrasyonu (react-router, Next.js, TanStack Router, vue-router, Angular Router) → route template view adı |
| `@trooper/react-native` | `sdks/js/packages` | React Native | react-navigation ekranları, app start, JS hataları, network, AppState flush |
| `trooper-android` (+ `-okhttp`, `-compose`) | `sdks/android` | Android | Activity/Fragment/Compose ekranları, app start, crash, ANR, frozen frame, OkHttp |
| `TaskTrooper` (SPM + CocoaPods) | `sdks/ios` | iOS | UIKit/SwiftUI ekranları, app start, crash (signal + NSException), hang, URLSession |
| `trooper_flutter` (+ `trooper_dio`) | `sdks/flutter` | Flutter | Navigator/go_router, Flutter/Dart hataları, http/dio, frame timing, app start |
| `trooper-python` (faz 2) | — | Python | django, fastapi, flask, celery, sqlalchemy, requests |

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

## Wire formatı

- Backend: **OTLP/HTTP protobuf** → `POST /v1/traces`, `/v1/metrics`, `/v1/logs`. Header: `x-trooper-key`.
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
