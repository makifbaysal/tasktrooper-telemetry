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
| `trooper-quarkus` | `sdks/jvm` | Quarkus 3 | quarkus-opentelemetry config'i, JAX-RS route template, reactive messaging |
| `trooper-ktor` + `trooper-kotlin` | `sdks/jvm` | Kotlin / Ktor 3 | server + client plugin, coroutine context |
| `TaskTrooper.Telemetry` (+ `.AspNetCore`, `.Kafka`, `.RabbitMQ`, `.MassTransit`, `.EntityFrameworkCore`, `.Redis`) | `sdks/dotnet` | .NET 8+ | ASP.NET Core, HttpClient, SqlClient/EF Core, Npgsql, Redis, gRPC, Confluent.Kafka, RabbitMQ, MassTransit |
| `@trooper/core` + `@trooper/browser` | `sdks/js/packages` | Web | page view, SPA route, Web Vitals, fetch/XHR + traceparent, JS error, click (rage/dead), sendBeacon, offline kuyruk |
| `@trooper/react`, `@trooper/vue`, `@trooper/angular` | `sdks/js/packages` | Web framework'leri | ErrorBoundary/errorHandler, router entegrasyonu (react-router, Next.js, TanStack Router, vue-router, Angular Router) → route template view adı |
| `@trooper/react-native` | `sdks/js/packages` | React Native | react-navigation ekranları, app start, JS hataları, network, AppState flush |
| `trooper-android` (+ `-okhttp`, `-compose`) | `sdks/android` | Android | Activity/Fragment/Compose ekranları, app start, crash, ANR, frozen frame, OkHttp |
| `TaskTrooper` (SPM + CocoaPods) | `sdks/ios` | iOS | UIKit/SwiftUI ekranları, app start, crash (signal + NSException), hang, URLSession |
| `trooper_flutter` (+ `trooper_dio`) | `sdks/flutter` | Flutter | Navigator/go_router, Flutter/Dart hataları, http/dio, frame timing, app start |
| `@trooper/node` (faz 2) | — | Node.js | OTel Node auto-instrumentation üzerine preset |
| `trooper-python` (faz 2) | — | Python | django, fastapi, flask, celery, sqlalchemy, requests |

Kütüphane başına özel kod sadece OTel'in vermediği bilgi için yazılır: route template (`trooper.route`), consumer process adı,
çağıran servis (`tt.svc` baggage). Geri kalan her şey OTel instrumentation'ından gelir.

## API yüzeyi

### Node

```ts
import { init } from '@trooper/node';

init({
  dsn: process.env.TROOPER_DSN,           // https://<ingest-key>@ingest.trooper.dev/<project>
  service: 'orders-api',
  environment: 'production',
  version: process.env.GIT_SHA,           // deploy marker
  sampleRate: 1.0,
  logs: { capture: ['pino'] },
});
```

Worker/consumer için ek API:

```ts
import { consumer } from '@trooper/node';

consumer.wrap('orders.created', async (msg) => { ... });   // süre, sonuç, retry, lag
consumer.reportLag({ topic, partition, lag });             // kütüphane lag vermiyorsa
```

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
