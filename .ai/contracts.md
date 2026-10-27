# Servisler ve SDK'lar arası sözleşmeler

Bu dosya backend servisleri ve SDK'ların birbirine dokunduğu her şeyi tanımlar. Paralel geliştirme bu sözleşmeye dayanır:
burada yazanı değiştirmek gerekirse **önce bu dosya güncellenir**, sonra kod. Mimari: `architecture.md`, kararlar: `decisions.md`
(özellikle ADR-010, ADR-011).

## 1. Repo yerleşimi ve sahiplik

```
services/                      Go 1.26, go.work ile çok modül
  go.work
  pkg/                         github.com/tasktrooper/telemetry/services/pkg   (ortak; harici bağımlılık yok)
  ingest/                      .../services/ingest    cmd/ingest-gw, cmd/ingest-writer
  api/                         .../services/api       cmd/api   (+ openapi.yaml)
  alerting/                    .../services/alerting  cmd/alert-engine, cmd/notifier
  collector/                   .../services/collector cmd/trooper-agent (DB + broker collector)
  integrations/                .../services/integrations cmd/trooper-cloud (AWS, GCP, Neon, Vercel — ADR-012)
tools/
  demo/                        github.com/tasktrooper/telemetry/tools/demo — demo trafiği (SDK'larla gerçek veri üretir)
deploy/
  docker-compose.yml           clickhouse, postgres, redpanda, redis (+ servisler profile "app" ile)
  clickhouse/*.sql             ClickHouse şeması (sıralı, idempotent: CREATE ... IF NOT EXISTS)
  postgres/*.sql               Postgres şeması
proto/rum/v1/rum.proto         RUM wire şeması (JSON mapping kullanılır)
sdks/
  go/                          github.com/tasktrooper/telemetry-go (ayrı modül, go.work'e dahil değil)
  jvm/                         Gradle multi-project: javaagent extension, spring-boot-starter, quarkus, ktor
  dotnet/                      TaskTrooper.Telemetry (+ tests)
  js/packages/                 browser, react, vue, angular, react-native (pnpm workspace)
  android/                     Gradle library (Kotlin)
  ios/                         Swift Package
  flutter/                     Dart/Flutter package
```

Her modül/klasör tek bir sahibe (agent/geliştirici) aittir; başka modülün `go.mod`'una dokunulmaz. `services/pkg`'ye ekleme
gerekirse küçük tutulur (tek harici bağımlılık `google.golang.org/protobuf`) ve bu dosyaya not düşülür.

`services/pkg` içeriği: `topics` (topic/header adları), `tenant` (Tenant + context), `config` (TT_* env okuma),
`logx` (JSON slog), `secrets` (AES-GCM + credentials ref çözümü), `rumpb` (`proto/rum/v1`'den buf ile üretilir: `cd proto && go run github.com/bufbuild/buf/cmd/buf@v1.57.0 generate`).

**Ortam değişkenleri** (default'lar host'tan docker-compose'a göre): `TT_POSTGRES_URL`, `TT_CLICKHOUSE_URL`
(`clickhouse://trooper:trooper@localhost:9000/telemetry`), `TT_KAFKA_BROKERS` (`localhost:19092`; container içinden `redpanda:9092`),
`TT_REDIS_URL`, `TT_LOG_LEVEL`. Servise özel değişkenler `TT_<SERVIS>_*` önekiyle (ör. `TT_INGEST_HTTP_ADDR`).

## 2. Ağ uçları ve kimlik doğrulama

| Servis | Port | Not |
|---|---|---|
| ingest-gw HTTP | 4318 | `POST /v1/traces`, `/v1/metrics`, `/v1/logs` (OTLP/HTTP protobuf **ve** JSON; `Content-Encoding: gzip` desteklenir), `POST /v1/rum` (RUM JSON, gzip opsiyonel), `GET /healthz` |
| ingest-gw gRPC | 4317 | OTLP gRPC (TraceService, MetricsService, LogsService) |
| api | 8080 | `/api/v1/...` (bkz. `services/api/openapi.yaml`), `GET /healthz` |
| alert-engine | 8090 | sadece `/healthz`, `/metrics` |
| ingest-gw metrics | 4318 (veya `TT_INGEST_METRICS_ADDR`) | Prometheus `GET /metrics` |
| ingest-writer metrics | 9464 (`TT_INGEST_WRITER_METRICS_ADDR`) | Prometheus `GET /metrics` |

**Internal credential:** servis içi gönderenler (trooper-cloud, loadtest-runner) `x-trooper-internal: <TT_INTERNAL_TOKEN>` +
`x-trooper-project: <id>` gönderir (OTLP HTTP/gRPC metadata, `/v1/rum`); sabit zamanlı karşılaştırma, `Origin` varsa 403, token
ayarlı değilse 401. Org Postgres'ten (60 sn cache).
**Ingest auth:** `x-trooper-key: <ingest key>` header'ı, ya da `Authorization: Bearer <key>`. Browser `sendBeacon` header
koyamadığı için `?key=` query parametresi de kabul edilir (sadece `/v1/rum`). Key Postgres `api_keys` tablosunda sha256 hash'i ile
durur; ingest-gw 60 sn TTL'li in-memory cache tutar. Key → `(org_id, project_id)`. Browser key'lerinde `allowed_origins` kontrolü.
**DSN:** `https://<key>@<host>[:port]/<project_id>` — SDK'lar DSN'den endpoint ve key çıkarır. Lokal: `http://tt_dev_ingest_key@localhost:4318/1`.
**Dev seed:** org 1, project 1, ingest key `tt_dev_ingest_key`, api key `tt_dev_api_key` (postgres/002_seed.sql).

**`/v1/rum` content type'ları:** `application/json`, `application/x-protobuf` ve `text/plain;charset=UTF-8` (gövde JSON —
`sendBeacon` preflight'sız sadece bunu gönderebilir). **CORS:** `OPTIONS` preflight `content-type`, `x-trooper-key`,
`content-encoding` header'larına izin verir; `Access-Control-Allow-Origin` key'in `allowed_origins`'ine göre (boşsa istek origin'i).
Müşterinin cross-origin API'leri `traceparent` (ve `baggage`) header'ına CORS izni vermelidir — SDK dokümanı + onboarding bunu söyler.

Cevaplar: 200 (OTLP: boş `Export*ServiceResponse`), 400 (parse), 401 (key), 413 (> 5 MB gövde), 429 (rate limit; `Retry-After`).
SDK'lar 429/5xx'te exponential backoff (1s→60s, jitter), 4xx'te (429 hariç) batch'i atar. OTel exporter'larını kullanan
backend SDK'ları upstream retry politikasıyla yetinebilir (429/502/503/504, en fazla 5 deneme).

**Ortak SDK config adları** (tüm dillerde aynı; verilmeyenler OTel default'una düşer, açıkça verilen `OTEL_*`/`otel.*` kazanır):
`TROOPER_DSN`, `TROOPER_SERVICE`, `TROOPER_ENV`, `TROOPER_VERSION`, `TROOPER_SAMPLE_RATE`, `TROOPER_ENABLED`
(property dosyalarında `trooper.dsn`, `trooper.sample-rate`, `trooper.enabled` …). Resource'ta `telemetry.distro.name`
(`trooper-go`, `tasktrooper-javaagent`, `trooper-dotnet` …).

## 3. Kafka (Redpanda) topic'leri — ingest-gw → ingest-writer

| Topic | Değer | Key |
|---|---|---|
| `tt.otlp.traces` | `ExportTraceServiceRequest` protobuf (JSON gelse de protobuf'a çevrilir) | `project_id` (string) |
| `tt.otlp.metrics` | `ExportMetricsServiceRequest` protobuf | `project_id` |
| `tt.otlp.logs` | `ExportLogsServiceRequest` protobuf | `project_id` |
| `tt.rum` | `RumBatch` protobuf (`proto/rum/v1`) | `project_id` |

Header'lar (hepsi string): `org_id`, `project_id`, `received_at` (unix ms), `client_ip` (sadece geo için, writer sonrası atılır),
`user_agent` (sadece RUM). Topic'ler 24 saat retention, 24 partition (dev: 3). Writer consumer group: `tt-ingest-writer`.

## 4. Semantic convention'lar (SDK → writer)

OTel semantic conventions esas. Writer şu attribute'ları okur (span veya resource):

| Attribute | Kim yazar | Anlam |
|---|---|---|
| `service.name`, `service.version`, `deployment.environment.name` | resource | servis, versiyon (deploy marker), ortam |
| `trooper.route` | SDK (server span) | normalize route template (`/v1/orders/:id`). Yoksa writer `http.route`'u, o da yoksa `url.path`'i normalize eder (sayı/uuid/hex → `:id`) |
| `trooper.entry.route` | SDK (aynı istek içindeki **tüm** span'lar) | bulunduğu entry span'ın route'u (consumer'da process adı) → `edges_1m` |
| `trooper.entry.method` | SDK (`trooper.entry.route` ile birlikte) | entry span'ın HTTP method'u; consumer'da `CONSUME`, gRPC'de `GRPC` |
| `trooper.process` | SDK (consumer span) | consumer process adı (yoksa `messaging.consumer.group.name`, o da yoksa — RabbitMQ / SmallRye — `messaging.destination.name`) |
| `messaging.system`, `messaging.destination.name`, `messaging.consumer.group.name`, `messaging.destination.partition.id` | OTel | consumer: kaynak, topic/queue, group |
| `trooper.consumer.attempt` (int), `trooper.consumer.dlq` (bool) | SDK | retry sayısı, DLQ'ya gönderildi |
| `trooper.caller` | SDK (server span) | çağıran servis adı; çağıran SDK W3C baggage'a `tt.svc=<service>` koyar, karşı taraf okur. Yoksa writer user-agent'tan sınıflar (`web`, `mobile`, `probe`, `unknown`) |
| `peer.service` / `db.system` + `server.address` / `messaging.system` | OTel (client span) | bağımlılık adı: writer `dep` = `"<db.system> · <db.namespace veya server.address>"`, HTTP için `"HTTP · <peer.service veya server.address>"`, queue için `"<messaging.system> · <destination>"` |
| `db.query.text` | OTel | writer normalize eder (literal → `?`), `cityHash64` ile `query_hash` |

**Route normalizasyonu (tüm SDK'lar + writer):** `{id}`, `{id:regex}`, `<id>`, `:id` → `:id`; catch-all `{p...}`, `*p`, `{*p}` → `*p`;
gRPC route = tam method adı (`/pkg.Service/Method`). Ham sayı/uuid/hex/ulid segmentleri → `:id`.

**Writer tamamlamaları:** entry olmayan span'larda `route = entry_route`; SDK `trooper.entry.route` yazmadıysa writer aynı
batch'teki en yakın entry-span atasından doldurur. Webhook çağıranlar `caller=<vendor>` (ör. `Stripe`), `caller_kind=webhook`.
Exception type/message `attrs` içine kopyalanır (`errors_1h` okur).

Entry span = `kind ∈ {server, consumer}` ve parent'ı başka servisten (veya yok). RED/consumer rollup'ları sadece entry span'lardan.
**Sampling:** RED metrikleri sampling'den önce sayılmalı → SDK'lar sampling yapsa bile entry span'ları `trooper.sample_rate` (float)
ile işaretler, writer sayımları `1/sample_rate` ile çarpar. Parent-based sampling'de gerçekte geçerli olan oran upstream'in
oranıdır: kök span'ı başlatan SDK oranı W3C `tracestate`'e `tt=r:<rate>` olarak yazar; remote parent'lı entry span'lar
`trooper.sample_rate`'i oradan alır (yoksa kendi config oranını yazar).

**Retry sayısı taşıma:** retry topic'leri / yeniden publish arasında deneme sayısı mesaj header'ı `tt-attempt` (ondalık string)
ile taşınır; consumer SDK'sı bunu `trooper.consumer.attempt`'e yazar (RabbitMQ'da `x-delivery-count`/`x-death` de okunur,
Spring Kafka'da `kafka_deliveryAttempt`).

**SDK iç metriği:** her backend SDK'sı yuttuğu hataları `trooper.sdk.errors` counter'ında (meter `TaskTrooper.Telemetry`,
attr `component`) sayar.

## 5. Collector metrikleri (trooper-agent → OTLP metrics → writer)

Collector DB ve broker verisini **OTLP gauge/sum** olarak ingest-gw'ye yollar; writer bu isimleri tipli tablolara yönlendirir.

DB (`db_1m`): `trooper.db.cpu` (0..1), `trooper.db.memory` (0..1), `trooper.db.disk` (0..1), `trooper.db.connections`,
`trooper.db.max_connections`, `trooper.db.ops` (/sn), `trooper.db.latency.p95` (ms), `trooper.db.slow` (/dk),
`trooper.db.hit_ratio` (0..1), `trooper.db.evictions` (/dk), `trooper.db.replication_lag` (ms).
Attribute'lar: `trooper.db.id`, `trooper.db.name`, `db.system` (postgresql|mysql|redis|mongodb), `trooper.db.node`, `trooper.db.role`.

DB sorguları (`db_queries_1m`): `trooper.db.query.calls`, `trooper.db.query.total_ms`, `trooper.db.query.p95_ms`, `trooper.db.query.rows`
+ `trooper.db.id`, `trooper.db.query.hash`, `db.query.text` (normalize edilmiş).

Broker lag (`broker_lag_1m`): `trooper.broker.lag`, `trooper.broker.end_offset`, `trooper.broker.committed_offset`,
`trooper.broker.consume_rate`, `trooper.broker.produce_rate`, RabbitMQ için `trooper.broker.unacked`.
Attribute'lar: `trooper.broker.cluster`, `messaging.system` (kafka|rabbitmq), `messaging.destination.name`,
`messaging.consumer.group.name`, `messaging.destination.partition.id` (RabbitMQ: `0`).
Group durumu (`broker_groups_1m`): `trooper.broker.group.members`, `trooper.broker.group.rebalances` (+ `trooper.broker.group.state` attr).
Broker node (`broker_nodes_1m`): `trooper.broker.node.cpu|memory|disk|net_in` + `trooper.broker.node` (Kafka'da sadece log-dir'den
gelen node adı `broker-<id>`, `nodes[].brokerId` ile değişir). RabbitMQ ek gauge'ları `trooper.broker.node.fd` ve `.proc`
(0..1 kullanım oranı) → generic `metrics` tablosu.

Topic / kuyruk (`broker_topics_1m`): `trooper.broker.topic.partitions`, `.replication`, `.retention_ms`, `.avg_msg_bytes`,
`.in_rate` (msg/sn), `.bytes_in_rate` (byte/sn), `.size_bytes` + `trooper.broker.cluster`, `messaging.destination.name`.
`.retention_ms = -1` → sınırsız (UI "∞" gösterir). Kafka `size_bytes` disk üzerindeki (sıkıştırılmış) boyuttur.
Kuyruk yaşı: `trooper.broker.oldest_age_s` → `broker_lag_1m.oldest_age_s` (SQS `ApproximateAgeOfOldestMessage`, Pub/Sub
`oldest_unacked_message_age`, RabbitMQ head message timestamp). `messaging.system` ∈ `kafka | rabbitmq | aws_sqs | gcp_pubsub`.
Broker node `net_in` birimi **MB/sn**.

Kaynak: cloud entegrasyonundan gelen metrikler resource'ta `service.name=trooper-cloud`, `trooper.integration.id`,
`cloud.provider` (`aws|gcp|neon|vercel`), `cloud.region` taşır; isimler yukarıdakiyle aynıdır.

Diğer tüm metrikler `metrics` tablosuna (custom).

**Raporlanmayan değer:** collector tablolarında eksik `max` kolonları `-Inf`, `hit_ratio` (min) `+Inf` yazılır (NaN, ClickHouse
max/min'de satır sırasına bağlı sonuç verir). Okuyanlar (api, alerting) sonsuz değeri "veri yok" sayar (`isFinite(x)`).

Ek kurallar:
- `trooper.db.query.calls|total_ms|rows` **delta Sum**'dır (writer dakikalık toplar); `p95_ms` ve diğer tüm `trooper.db.*` /
  `trooper.broker.*` değerleri gauge (son snapshot).
- `trooper.db.query.hash` motorun kendi id'sidir (ondalık uint64: pg `queryid`, MySQL digest'in ilk 64 biti, Redis komut adı FNV).
  Span'lardaki `db_query_hash` (cityHash64) ile eşleşmez; sorgu ↔ endpoint eşlemesi normalize metin üzerinden yapılır.
- `trooper.db.id` ve `trooper.broker.cluster` = Postgres'teki `slug`.
- Collector sağlığı: `trooper.collector.up` (0/1) + `trooper.collector.target`, `trooper.collector.kind` → `metrics` tablosu.
- `GET /api/v1/internal/collector-config` JSON şekli: `services/collector/README.md` (DB kullanıcı adı `databases.username`,
  boşsa `trooper_monitor`).

## 6. ClickHouse

Şema: `deploy/clickhouse/001_init.sql`. Tek düğüm dev için `MergeTree`; prod'da aynı DDL `ON CLUSTER` + `Replicated*` + `Distributed`
(migration aracı ortam değişkeni ile seçer). Writer kurallar:
- Batch: tablo başına ≥ 50k satır veya 1 sn, hangisi önce. Insert başarısızsa Kafka offset commit edilmez (at-least-once).
- Rollup'lar (`red_1m`, `edges_1m`, `callers_1m`, `consumer_1m`, `rum_views_1m`, `rum_vitals_1m`, `trace_index`) materialized view;
  writer sadece ham tablolara yazar (+ collector tipli tabloları).
- Sorgu tarafı (api, alerting) rollup'larda `sum(...)`, quantile için `quantilesTDigestWeightedMerge(...)` (`red_1m/1h`,
  `consumer_1m`, `edges_1m`, `callers_1m` weighted digest tutar), RUM rollup'larında `quantilesTDigestMerge(...)` kullanır.
- Monitor `query` JSON şekilleri (kind başına): `services/alerting/README.md`. Monitor güncellemesi `updated_at`'i artırır
  (trigger, `deploy/postgres/003_alerting.sql`); alert-engine monitor cache'i buna bakar.

## 7. Postgres

Şema: `deploy/postgres/001_init.sql`, seed `002_seed.sql`. Sahibi `api`; alerting `monitors`, `incidents`, `notification_channels`,
`kpis`, `kpi_alert_rules`'ı okur/yazar. Collector `databases` ve `broker_clusters`'ı api'den okur (`GET /api/v1/internal/collector-config`,
api key ile), DB'ye doğrudan bağlanmaz.

## 8. API (dashboard'un backend'i)

`services/api/openapi.yaml` tek kaynak. Kurallar: JSON, camelCase alanlar; zaman `ts` unix ms; süreler ms; oranlar 0..1;
`?range=1h|6h|24h|7d` (UI'daki `TimeRange`) ve `?env=`. Seriler `{ timestamps: number[], series: [{ id, label, data: number[] }] }`.
Cevap şekilleri `apps/web/src/data/types.ts` (+ `infra.ts`) ile birebir — UI'da mock'tan fetch'e geçiş sadece `src/data/*`
fonksiyonlarını değiştirir. Auth: `Authorization: Bearer <api key>` (dev) — OIDC session sonra.

## 9. RUM

`proto/rum/v1/rum.proto`. JSON'da alan adları proto'daki snake_case adlarıdır (`protojson` `UseProtoNames`); int64'ler sayı
olarak gönderilebilir. Bir batch tek resource (app/cihaz) + N event. Client SDK kuralları: batch ≤ 100 event veya 5 sn,
sayfa/app arka plana geçince flush (web: `sendBeacon`/`fetch keepalive`), offline'da diske/IndexedDB'ye kuyruk (≤ 5 MB, FIFO drop).
Network span'ı için `traceparent` (W3C) üretilir ve `resource` event'ine `trace_id`/`span_id` yazılır.

**Görünüm yükleme süresi:** `loading_time_ms` **VIEW_START** event'inde taşınır (writer bunu `duration_ms`'e yazar,
`rum_views_1m.load` sadece view_start'tan hesaplanır). Bu yüzden SDK'lar VIEW_START'ı ekranın ilk frame'ine / sayfanın
load'una kadar geciktirebilir; VIEW_END `time_spent_ms` taşır. `loading_time_ms` yok/0 = bilinmiyor (sayfa load'dan önce
gizlendi); rollup bunları `duration_ms > 0` koşuluyla dışlar.

**Bilinen ek attribute'lar** (`RumEvent.attributes`): ACTION → `click_count`; ERROR → `component_stack`, `error_boundary`,
`component`, `vue_info`; APP_START → `start_reference`; frame vital'ları → `total_frames`.

**Frame vital'ları:** `SLOW_FRAMES` ve `FROZEN_FRAMES` view başına **sayı** olarak gönderilir ve `attributes.total_frames`
eklenir; oranı (slow / total) backend hesaplar. Eşikler: slow > 16 ms (display-link jitter'ı için ≤ 17.5 ms tolerans kabul), frozen > 700 ms.

**APP_START:** `attributes.start_reference` = `process` (işletim sistemi process başlangıcından) | `sdk_init` (native SDK
process zamanını alamıyorsa, ör. Android API < 24 ya da iOS pre-warm) | `foreground` (hot start: arka plandan dönüş) | `dart_init` / `js_init` (framework init'inden). `process` dışındakiler
daha kısa çıkar, UI bunu işaretler.

**`connection`:** `wifi | cellular | 4g | 3g | offline | unknown`; ethernet/VPN/diğer → `unknown`.

**Donma / ANR:** Android ANR ve iOS hang aynı modelle: ERROR, `source=anr`, `type` = `ANR` | `AppHang`.
MetricKit'ten gelen crash/hang'ler `attributes.metrickit=true` taşır; aynı crash SDK raporuyla da gelebilir, server dedupe eder
(session + zaman penceresi + fingerprint).

**Hibrit uygulamalar (Flutter / React Native + native SDK):** native SDK varsa oturum ve crash'ler ortak olur. Kanal adı
`dev.tasktrooper.telemetry/rum` (Flutter MethodChannel — `trooper_flutter` plugin'i, iOS'ta native SDK için `trooper_flutter/Native` subspec; RN'de native module adı `TrooperRum` (TurboModule + eski mimari), JS önce `TrooperRum`, sonra bu kanal adını arar; RN ≥ 0.74, iOS'ta native SDK için `TROOPER_RN_NATIVE_SDK=1 pod install`):
- `setContext({session_id, anonymous_id, view_id, view_name, user_id})` — JS/Dart → native; native crash raporu bu bağlamla gönderilir.
- `getContext()` → aynı alanlar — native → JS/Dart (native önce başladıysa onun oturumu kullanılır).
- `getProcessStartTime()` → unix ms — APP_START `start_reference=process` için.
Native handler yoksa çağrılar sessizce başarısız olur. Oturumun sahibi Dart/JS katmanıdır: native SDK `setContext` ile
oturumu devraldığında kendi SESSION_START'ını göndermez.

**Ingest davranış ayarları (env):** `TT_TAIL_SAMPLE_RATE` (default 1; error/yavaş entry span'lı trace'ler tam tutulur, diğerleri
deterministik trace-id hash ile, tutulan span'larda `sample_weight *= 1/r`), `TT_TAIL_SLOW_MS` (default 1000), `TT_GEOIP_DB` (MaxMind
mmdb; `rum_events.country` client IP'den, IP saklanmaz; drain'den gelen `geo.country` önceliklidir).
**RUM saat düzeltmesi:** batch sadece en yeni event'i received_at'ten > 5 dk ileride veya > 24 saat geride ise kaydırılır;
offline kuyruktan gelen ≤ 24 saatlik event'ler kendi zamanlarında kalır.

## 10. Cloud entegrasyonları (ADR-012)

- Postgres: `integrations`, `secrets`, `deploy_markers` (`deploy/postgres/004_integrations.sql`). `databases` / `broker_clusters`
  satırlarını keşif upsert eder: `slug` = `<provider>-<kaynak adı>` (ör. `aws-orders-db`), `collector='cloud'`, `integration_id`,
  `source` ('AWS RDS · eu-central-1'), `version`, `zone`, `instance_class`, `disk_gb`, `meta`. Kullanıcının elle eklediği satıra
  dokunulmaz (integration_id NULL ise). Kaynak silinirse satır silinmez, `meta.missing=true`.
- `trooper-cloud` Postgres'e doğrudan bağlanır (servis içi; api ile aynı DB), `integrations.status/error/last_sync_at/resources`
  günceller. Secret çözümü: `secret:<id>` → `secrets` tablosu, AES-256-GCM, anahtar `TT_SECRETS_KEY` (base64 32 byte;
  dev default `deploy/dev-secrets.key` yoksa sabit dev anahtarı + uyarı). Şifreleme formatı: `nonce(12) | ciphertext+tag`,
  AAD = `project_id` (ondalık string). api yazar, trooper-cloud okur — ikisi aynı küçük paketi kullanır: `services/pkg/secrets`.
- Metrik/keşif aralığı: keşif 10 dk, metrik 60 sn (CloudWatch/Monitoring 1 dk çözünürlük; son tam dakika okunur).
- **Vercel:** deploy'lar REST API'den (`GET /v6/deployments`) 60 sn'de bir → `deploy_markers` (source `vercel`, servis =
  `config.projectMap[project] ?? project name`). Drain'ler ingest-gw `POST /v1/drains/vercel?key=<ingest key>&integration=<id>`:
  imza `x-vercel-signature` = hex(HMAC-SHA1(drainSecret, rawBody)); drain secret'ı ingest-gw `integrations.config.drainSecretRef`
  ile çözer. Gövde JSON dizi veya NDJSON. Eşleme: `logs` → `telemetry.logs` (service = proje adı/projectMap, severity level'den,
  `trace_id` varsa), `traces` (OTLP JSON) → mevcut trace yolu, `speed_insights` → RUM VITAL (LCP/INP/CLS/FCP/TTFB, view_name =
  route), `web_analytics` → RUM VIEW_START (path → view_name, session/anonim id hash'lenir). Vercel'in doğrulama isteği
  (`x-vercel-verify`) için cevap header'ı `x-vercel-verify: <config.verifyToken>`.
  Drain config anahtarları: `drainSecretRef`, `verifyToken`, `speedInsightsUnit`, `projectMap` (anahtar proje adı **veya** id).
  Cevaplar: secret yok → 401, imza yanlış → 403, başka projenin entegrasyonu → 403, bilinmeyen entegrasyon → 404. Gzip gövdede imza
  hem açılmış hem ham byte üzerinden denenir. Client IP, user agent, JA3/JA4 saklanmaz. Integration + secret 60 sn cache'lenir.
- **Neon:** `GET /api/v2/projects`, `/projects/{id}/branches`, `/projects/{id}/endpoints` → her compute endpoint bir `databases`
  satırı (engine postgres, `instance_class` = "0.25–2 CU", `source` = 'Neon · aws-eu-central-1'); metrikler: compute durumu
  (active/idle → `trooper.db.ops` değil, `meta.state`), `trooper.db.cpu`/`memory` Neon'un consumption/metrics API'sinden
  (yoksa boş), `disk` = logical size / plan limit. Sorgu istatistikleri collector'ın direct modu ile.
- **AWS / GCP metrik eşlemesi** `services/integrations/README.md`'de tablo olarak tutulur (CloudWatch/Monitoring metrik →
  `trooper.*`).
- trooper-cloud'un gönderdiği OTLP'de ek resource attribute `trooper.integration.name`. Kuyruk ve aggregate lag (SQS, Pub/Sub,
  MSK group toplamı) `messaging.destination.partition.id = "0"` ile yazılır (`broker_lag_1m.partition` UInt32; `-1` kullanılmaz).
  Neon tüketim sayaçları custom metrik `trooper.cloud.neon.*` (`compute_time_seconds`, `active_time_seconds`,
  `cpu_used_seconds`, `written_data_bytes`, `data_transfer_bytes`, `synthetic_storage_bytes`, `logical_size_bytes`,
  `compute_active`, `compute_cu_max`) → generic `metrics` tablosu. Datapoint zamanı sağlayıcının kendi dakikasıdır.
- **trooper-cloud ingest auth:** `TT_INTERNAL_TOKEN` ayarlıysa internal credential (§2); değilse/401'de proje başına ingest key
  `TT_CLOUD_INGEST_KEYS=projectId:key,...` (geri uyumlu). Vercel `createDrain` gerçek ingest key ister (drain URL'si public).
- Bilinen boşluklar: Neon CPU/memory/connection metriği yok; RDS memory instance-class tablosundan (bilinmeyen sınıf →
  `config.instanceMemoryGb`); CloudWatch/GCP'den `max_connections` ve p95 yok; MSK Serverless'ta sadece lag.

## 11. Dashboard veri modu (ADR-013)

- `apps/web`: `VITE_DATA=mock|api` (default mock), `VITE_API_URL` (default `http://localhost:8080`), `VITE_API_KEY` (dev:
  `tt_dev_api_key`). Altyapı `src/data/remote.ts`: `api()`, `remote(path, fallback, map?)`, `hydrateList(arr, path, map?)`,
  `registerHydrator(fn)`, `invalidate(prefix)`, `mutate(method, path, body, invalidates[])`, `useData()`, `remoteStatus()`.
- Her `src/data/*.ts` dosyası kendi hydrator'ını kaydeder ve fonksiyonlarını `isApi ? remote(...) : mock(...)` biçiminde yazar.
  Mock implementasyonu silinmez.

## 12. Symbolication (`services/symbolicator`)

- Blob store: S3 uyumlu (dev: SeaweedFS S3 `http://localhost:9002`, bucket `tt-artifacts` (servis yoksa oluşturur), `trooper` / `trooper-secret`, path-style; env `TT_S3_ENDPOINT`,
  `TT_S3_BUCKET`, `TT_S3_ACCESS_KEY`, `TT_S3_SECRET_KEY`). Anahtar: `<project>/<kind>/<app>/<release>/<dosya>`; release başına
  `index.json` (Postgres tablosu yok). İki replica aynı release'e eşzamanlı yüklerse index girdisi düşebilir — yeniden yükleme düzeltir.
- `symbolicator` :8095, api key ile (`Authorization: Bearer`, key cache 60 sn, Postgres yoksa 503):
  - `POST /v1/artifacts` (multipart: `kind` = `sourcemap | dsym | r8 | proguard | dart | ndk`, `app`, `release` (= RUM
    `resource.version` / `service.version`), dosya part adı `file` (birden çok olabilir)). Limit: debug dosyası 200 MB (zip içi dahil),
    map/mapping 50 MB.
  - `GET /v1/artifacts?app=&release=`, `DELETE /v1/artifacts?app=&release=[&kind=]`.
  - `POST /v1/symbolicate` `{ app, release, platform, stack }` → `{ stack, frames: [{ fn, file, line, col, inApp }], symbolicated,
    missing: [debugId] }` (`missing` = yüklemesi olmayan debug id'ler). Bulunamazsa ham stack döner (`symbolicated: false`). Sonuç
    Redis'te 24 saat cache'lenir; cache key release'in yükleme versiyonunu içerir (yeni yükleme eski sonucu gizlemez).
- **Stack formatları:**
  - JS: Chrome/V8 ve Firefox/Safari. Orijinal fonksiyon adları minified `.js` map ile birlikte yüklenince kesin (CLI default).
    Desteklenmez: dosyaya gömülü `data:` URL map'ler.
  - Android Java/Kotlin: R8/ProGuard mapping (inline frame, `sourceFile`, `rewriteFrame`). R8 outline desteklenmez.
  - Android NDK: tombstone satırları `#NN pc <rel hex>  <path> (BuildId: <hex>)`; frame ≥ 1 SDK tarafından zaten düzeltilmiş
    (−4 arm64 / −2 thumb / −1 x86), symbolicator `pc`'yi olduğu gibi kullanır; build-id ile `kind=ndk` (unstripped `.so`) aranır.
    `platform=android` + tombstone frame'leri → native yol; karışık stack'te sadece native kısım çözülür.
  - iOS: Apple formatı, tüm thread bölümleri + `Binary Images`; release `"<short> (<build>)"` (ör. `1.4.0 (212)`), dSYM zip/bundle/ham,
    fat dosyanın iki slice'ı. Swift demangle host'ta `swift-demangle` varsa (container imajında yok → mangled kalır).
  - Flutter: `kind=dart` split-debug-info sembolleri (Android ELF, iOS Mach-O). Dart obfuscation map desteklenmez.
- api hata detayında (`/errors/{fingerprint}`, RUM app errors) stack'i symbolicator'a sorar (`TT_SYMBOLICATOR_URL`, timeout 2 sn),
  ulaşamazsa ham stack döner.
- CLI: `tools/trooper-cli` → `trooper-cli upload-sourcemaps|upload-dsym|upload-mapping|upload-flutter-symbols|upload-ndk-symbols|delete
  --app --release <path>` (`TROOPER_API_URL`, `TROOPER_API_KEY`). Exit kodları: 0 ok, 1 sunucu/ağ, 2 kullanım, 3 yetkisiz, 4 bazı
  dosyalar reddedildi. Android Gradle plugin'i (`com.tasktrooper.mapping-upload`) sadece `r8` yükler; NDK `.so` için CLI
  `upload-ndk-symbols`. iOS: `sdks/ios/Scripts/upload-dsyms.sh`.

## 13. Code intelligence (`services/codeintel`, ADR-014)

- Port :8096 (`/healthz`, `/metrics`, `POST /v1/webhooks/github`). Migration `deploy/postgres/005_codeintel.sql` (codeintel sahibi):
  - `repositories` + `installation_id bigint`, `credentials_ref text`, `local_path text` (provider `local` eklenir),
    `analyze_requested_at timestamptz`, `analyzed_at timestamptz`, `error text`, `languages text[]`, `services text[]`,
    `webhook_secret_ref text`.
  - `inventory_items` + `service text`, `criticality text` (critical|high|medium|low), `observed bool`, `monitored bool`,
    `code_hash text`, `summary text`, `dead bool` (kodda var, 30 gün trafik yok), `shadow bool` (trafik var, kodda yok →
    `repository_id` NULL olabilir), `llm jsonb` (failure_modes, loadtest_hint).
  - `alert_suggestions` + `title text`, `kind text`, `target text`, `condition text`, `criticality text`, `code_ref text`,
    `calibrated bool` (eşik gözlem verisiyle ayarlandı), `created_at`. `monitor_draft` = api `POST /monitors` gövdesiyle aynı şekil.
  - `monitors` + `source text` ('manual'|'code'|'template'|'suggestion'), `code_ref text` (alert-as-code; read-only).
  - `loadtest_scenarios` + `state text` ('draft'|'ready', default 'ready'); codeintel `source='generated', state='draft'` yazar.
- `.trooper/config.yaml`: `services: [{ path: services/orders, name: orders-api }]`; yoksa SDK init çağrılarından tespit.
- `.trooper/monitors/*.yaml`: alerting.md § Kural şeması formatı (name, kind, query, condition, severity, notify, labels).
- Envanter `key` formatı: endpoint `"METHOD /route/:param"` (writer normalizasyonu ile aynı), consumer = process adı (`source` =
  topic/queue), cron = job adı (`source` = schedule), view = route, dependency = `"<kind> · <name>"`.
- Gözlem eşleştirme: `red_1m` (son 7 gün) route'ları ve `consumer_1m` process'leri ile; `observed`, `dead`, `shadow`.

## 14. Load testing (`services/loadtest`, ADR-015)

- `loadtest-ctl` :8097 (`/healthz`, `/metrics`). Migration `deploy/postgres/006_loadtest.sql` (loadtest sahibi):
  - `loadtest_runs` + `region text`, `live jsonb` (`{ ts, vus, rps, p95, errorRate, progress }`), `abort_requested_at`,
    `error text`, `created_at`, `requested_by bigint`.
  - `loadtest_domains (id, project_id, host, token, verified_at, method 'dns'|'http', created_at, UNIQUE(project_id, host))`.
  - `loadtest_usage (project_id, month date, vu_minutes numeric, PRIMARY KEY(project_id, month))`.
- Senaryo `spec` (jsonb) = UI `ScenarioEditor` modeli: `{ target, mode: 'vus'|'rps', levels: [{durationS, target}], steps: [key],
  weights: {key: pct}, requests: {key: {method, path, headers?, json?, checks?}}, thinkTime, regions, gate: {...},
  thresholds: {p95Ms, errorRate}, script? }`. Şablonlar: `{{int a-b}}`, `{{pool.<name>}}`, `{{weighted a:70 b:30}}`,
  `{{prev "KEY" .field}}`, `{{uuid}}`.
- Runner metrikleri (OTLP, `service.name=loadtest`, attr `trooper.loadtest.run`, `trooper.loadtest.step`):
  `trooper.loadtest.requests` (sum), `trooper.loadtest.errors` (sum), `trooper.loadtest.duration_ms` (histogram),
  `trooper.loadtest.vus` (gauge) → generic `metrics` tablosu.
- İstek header'ları: `traceparent`, `baggage: tt.svc=loadtest:<run_id>`, `x-trooper-loadtest-run`, `user-agent: TaskTrooper-LoadTest/1`.

## 15. Kimlik, org ve yönetim API'si (ADR-016)

- Migration `deploy/postgres/007_auth.sql` (api sahibi): `sessions (id text PK, user_id, created_at, expires_at, last_seen_at,
  user_agent)`, `org_members.role` ∈ owner|admin|member|viewer, `audit_log (id, org_id, project_id, user_id, api_key_id,
  action, target, meta jsonb, at)`, `users.oidc_subject`, `api_keys.created_by`, `api_keys.last_used_at`.
- Uçlar (`/api/v1`):
  - `GET /auth/config` (anonim) → `{ oidc: bool, devLogin: bool }`; `GET /auth/login?redirect=` → OIDC'ye yönlendirir;
    `GET /auth/callback`; `POST /auth/dev-login {email}`; `POST /auth/logout`; `GET /me` → `{ user, orgs: [{id, name, role,
    projects: [{id, name}]}], currentProjectId }`.
  - `GET/POST /orgs/{org}/members`, `PATCH/DELETE /orgs/{org}/members/{user}` (rol), `GET/POST /projects`,
    `GET/POST /api-keys` (POST cevabı key'i **bir kez** döner), `DELETE /api-keys/{id}`, `GET /audit-log?limit=`.
- CORS: cookie'li istekler için `Access-Control-Allow-Credentials: true` ve tam origin (wildcard değil).

## 16. Faz 4–5 api uçları ve diğer açık işler

- Code intelligence (api, Postgres'ten okur/yazar):
  - `GET /repos` → UI `Repo[]`; `POST /repos {provider, fullName, defaultBranch?, token? | installationId? | localPath?}`;
    `DELETE /repos/{id}`; `POST /repos/{id}/analyze` (→ `analyze_requested_at`).
  - `GET /inventory?repoId=&kind=&service=` → UI `InventoryItem[]` (+ `dead`, `shadow`, `summary`).
  - `GET /suggestions?state=proposed` → UI `Suggestion[]`; `POST /suggestions/{id}/accept` (monitor oluşturur, `source=suggestion`)
    ; `POST /suggestions/{id}/reject`.
  - `PUT/PATCH/DELETE /monitors/{id}` `source='code'` ise 409 "managed in repository".
- Load test (api): mevcut uçlar + `GET /loadtests/runs/{id}/series?step=5s` (`{ ts[], vus[], rps[], p95[], errorRate[] }`,
  `metrics` tablosundan) ve `GET /loadtests/runs/{id}/target?service=` (hedef servisin aynı penceredeki RED'i, caller
  `loadtest:<id>` ayrımı ile); `GET/POST /loadtests/domains`, `POST /loadtests/domains/{id}/verify`, `DELETE /loadtests/domains/{id}`.
- Range: `15m`, `30m` eklenir (`timerange` + UI `TimeRange`).
- Logs: `GET /logs?source=` → `attrs['vercel.source']` filtresi (build|static|external|edge|lambda|redirect|firewall).
- Ingest internal credential: `x-trooper-internal: <TT_INTERNAL_TOKEN>` + `x-trooper-project: <id>` başlıkları; ingest-gw
  token eşleşirse key aramaz. trooper-cloud ve loadtest-runner bunu kullanır (`TT_CLOUD_INGEST_KEYS` geri uyumlu kalır).
- KPI kuralları: `kpi_alert_rules.condition jsonb` (`renotifyMin`) — migration `008_alerting_r3.sql` (alerting sahibi).

## 17. Slack App (ADR-017)

- Env (api + notifier): `TT_SLACK_CLIENT_ID`, `TT_SLACK_CLIENT_SECRET`, `TT_SLACK_SIGNING_SECRET`, `TT_PUBLIC_API_URL`
  (OAuth redirect + interactivity URL tabanı), `TT_PUBLIC_APP_URL` (mesajlardaki linkler).
- Postgres (`009_slack_github.sql`, api sahibi): `integrations.provider` += `slack`, `github`; Slack kurulumu
  `integrations(provider='slack', config={teamId, teamName, botUserId}, credentials_ref=secret:<bot token>)`, org başına bir
  tane (project_id = kurulumu yapan proje; okumada org geneli). `notification_messages (id, incident_id, channel, provider,
  external_id /*slack ts*/, external_channel, created_at)`.
- api uçları: `GET /integrations/slack/install` → Slack OAuth'a yönlendirir (state imzalı); `GET /integrations/slack/callback`;
  `GET /integrations/slack/channels?q=` → `[{id, name, isPrivate, isMember}]` (cache 5 dk); `DELETE /integrations/slack`;
  `POST /slack/interactions` (Ack / Mute 1h butonları; imza doğrulaması; Slack kullanıcısını e-posta ile `users`'a eşler).
- Bildirim kind'ları: `firing | resolved | reminder | acknowledged`; ack bildirimini **alert-engine** kuyruğa koyar (api koymaz;
  dedupe `incident:<id>:acknowledged:<channel>`). Buton action id'leri: `tt_ack:<incidentId>`, `tt_mute1h:<monitorId>`.
  `not_in_channel` → `conversations.join` (scope `channels:join`) + bir kez tekrar; private kanal → kalıcı "uygulamayı davet et"
  hatası. Test için `TT_SLACK_API_URL`. Alert-engine çoklu shard: stream kuralı isabetleri `alert_stream_hits` (unlogged) +
  `NOTIFY tt_stream_hit` ile monitor'ün sahibi shard'a devredilir.
- Bildirim hedefi formatı: `slack:<channel_id>#<channel_name>` (örn. `slack:C0123ABCD#payments-alerts`). notifier: bot token ile
  `chat.postMessage`; ilk mesajın `ts`'i `notification_messages`'e; `resolved`/`reminder`/`acknowledged` → `thread_ts` ile
  thread'e cevap + ilk mesaj `chat.update` (renk/durum). `slack:https://hooks.slack.com/...` webhook hedefleri eskisi gibi.
- Mesaj Block Kit: header (durum emoji + başlık), alanlar (servis, değer vs eşik, süre, severity), **"Likely cause"** bölümü
  (`incidents.root_cause` → `summary` + `round(confidence*100)%`; `< 30%` → "low confidence"), deploy context, butonlar
  (Open incident, Ack, Mute 1h).

## 18. GitHub App (ADR-017)

- Env (api + codeintel): `TT_GITHUB_APP_ID`, `TT_GITHUB_APP_SLUG`, `TT_GITHUB_APP_PRIVATE_KEY` (env:/file: ref),
  `TT_GITHUB_WEBHOOK_SECRET`. Ortak istemci `services/pkg/githubapp` (JWT, installation token cache, repo listesi, sayfalama).
- Kurulum: `integrations(provider='github', config={installationId, account, accountType}, project_id)`; `repositories.installation_id`.
- api uçları: `GET /integrations/github/install` → `https://github.com/apps/<slug>/installations/new?state=<imzalı>`;
  `GET /integrations/github/callback?installation_id&setup_action&state` → integration kaydı, UI'a `/settings/integrations?github=<id>`
  ile döner; `GET /integrations/github/{id}/repos?q=` → `[{fullName, defaultBranch, private, language, added}]`;
  `POST /repos {provider:'github', fullName, installationId}` (mevcut uç; token gerekmez).
- Webhook (codeintel `POST /v1/webhooks/github`, App webhook URL'si): `push` default branch → `repositories.analyze_requested_at
  = now()` + değişen dosya listesi (`pending_paths text[]`) → incremental analiz; `installation_repositories` removed → repo
  `status='disconnected'`; `installation` deleted → integration `status='error'`. Teslim id'si (`X-GitHub-Delivery`) ile idempotent.

## 19. AI kullanım kotası (ADR-018)

**Postgres `deploy/postgres/010_ai_quota.sql`** (sahibi: aiquota sahibi; idempotent):
```sql
ALTER TABLE orgs        ADD COLUMN IF NOT EXISTS ai_monthly_credits integer;          -- NULL = paket varsayılanı, -1 = sınırsız
ALTER TABLE orgs        ADD COLUMN IF NOT EXISTS ai_notify text[] NOT NULL DEFAULT '{}'; -- monitor channels formatı (slack:<id>#ad, email:..)
ALTER TABLE memberships ADD COLUMN IF NOT EXISTS ai_monthly_credits integer;          -- NULL = üye tavanı yok
ALTER TABLE repositories ADD COLUMN IF NOT EXISTS analyze_requested_by bigint REFERENCES users(id) ON DELETE SET NULL;
CREATE TABLE IF NOT EXISTS ai_usage (
  id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  org_id bigint NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
  project_id bigint REFERENCES projects(id) ON DELETE SET NULL,
  user_id bigint REFERENCES users(id) ON DELETE SET NULL,      -- NULL = otomatik iş
  feature text NOT NULL,                                       -- codeintel.enrich | codeintel.suggest | rca | other
  model text NOT NULL, input_tokens integer NOT NULL, output_tokens integer NOT NULL,
  credits numeric(12,4) NOT NULL, ref text NOT NULL DEFAULT '',  -- ör. 'repo:12'
  created_at timestamptz NOT NULL DEFAULT now());
-- index: (org_id, created_at), (org_id, user_id, created_at)
CREATE TABLE IF NOT EXISTS ai_quota_alerts (org_id bigint, period date, threshold smallint CHECK (threshold IN (80,100)),
  sent_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY (org_id, period, threshold));
-- notifications.kind CHECK: ('firing','resolved','reminder','acknowledged','ai_quota'); incident_id zaten NULL olabilir
```

**Kredi hesabı** (`services/pkg/aiquota`, harici bağımlılık yok):
- `credits = (inTok × inPrice + outTok × outPrice) / 1e6 / 0.01` — fiyatlar USD / milyon token.
- `TT_AI_PRICES="<model>=<in>/<out>,..."`, bilinmeyen model → `TT_AI_DEFAULT_PRICE` (varsayılan `3/15`).
- `TT_AI_PLAN_CREDITS="free=100,team=5000,enterprise=-1"` (varsayılan bu). Bilinmeyen plan → `free`.
- Etkin org limiti: `orgs.ai_monthly_credits` ?? plan varsayılanı; `-1` sınırsız. Üye limiti: `memberships.ai_monthly_credits`.
- `(*Config).Reserve(ctx, tx, Request{OrgID, ProjectID, UserID *int64, Feature, EstimateCredits})` → `Decision{Allowed, Reason
  ("org_limit"|"user_limit"), OrgUsed, OrgLimit, UserUsed, UserLimit}`. Transaction içinde
  `pg_advisory_xact_lock(7140, org_id)`; kullanım = bu ayın `ai_usage` SUM'ı. `Record(ctx, db, Usage{...})` gerçek token ile yazar.
  Postgres erişimi küçük bir arayüzle (`Exec`, `QueryRow(...).Scan`); pgx adaptörünü çağıran yazar.
- Tahmin: `ceil(promptChars/4) × in + maxTokens × out`.

**codeintel:** her LLM batch'inden önce `Reserve`; red → kalan öğeler LLM'siz (statik), analiz özetine
`repositories.analysis` (jsonb, 010) yazılır: `{sha, incremental, items, llmItems, llm: {status: "ok"|"disabled"|
"quota_exceeded", reason?: "org_limit"|"user_limit"|"check_failed", credits, skipped?}}` (api `inventory.analysis.llm` olarak
döner; `check_failed` = kota kontrolü hata verdi, LLM fail-closed atlanır).
`UserID` = `repositories.analyze_requested_by` (manuel analizde api doldurur; webhook/otomatik isteklerde codeintel NULL'lar).

**Eşik bildirimi:** alert-engine 5 dk'da bir: org kullanımı ≥ %80 / ≥ %100 ve `ai_quota_alerts`'te o ay+eşik yoksa
`orgs.ai_notify` hedeflerine bildirim kuyruğa (notification kind `ai_quota`, incident'sız; payload `{orgId, period, threshold,
used, limit}`). Notifier Slack/e-posta metni: "AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1."

**API** (api sahibi):
- `GET /usage/ai?month=YYYY-MM` (member+, varsayılan bu ay) →
  `{ period, resetsAt, plan, org: {limit: number|null, used, remaining: number|null}, me: {limit: number|null, used},
     byUser: [{userId, email, name, limit: number|null, used}] (sadece admin+; diğerlerine []),
     byFeature: [{feature, credits, calls}], byProject: [{projectId, name, credits}],
     daily: SeriesSet (id 'credits'), aiQuota?: {monthlyCredits: number|null, notify: string[]} (sadece admin+) }` —
  `limit: null` = sınırsız. Krediler 2 ondalık.
- `PUT /orgs/{org}/ai-quota {monthlyCredits: number|null, notify: string[]}` (admin+, audit `ai_quota.update`).
- `PUT /orgs/{org}/members/{user}/ai-quota {monthlyCredits: number|null}` (admin+, audit `ai_quota.member`).
- `GET /me` → ek alan `ai: {period, limit, used, userLimit, userUsed}`; `GET /orgs/{org}/members` satırları `aiCredits: {limit, used}`.
- `POST /repos/{id}/analyze` → `analyze_requested_by` = oturum kullanıcısı (api key ile NULL).

## 20. Host, container, process ve Kubernetes (ADR-019)

**trooper-agent modları:** `TT_AGENT_MODES` (virgüllü; varsayılan `db`) — `db`, `host`, `cluster`. `TT_K8S_CLUSTER` (cluster adı;
host modunda K8s içindeyse de set edilir), `NODE_NAME`/`NODE_IP` (downward API), `TT_AGENT_DOCKER_SOCKET`
(varsayılan `/var/run/docker.sock`, yoksa container toplanmaz), `TT_AGENT_PROCFS` (`/host/proc`), `TT_AGENT_TOP_PROCESSES` (20).
Aralık: host 15 sn, cluster 30 sn. Gönderim bugünkü gibi OTLP metrics, `TT_DSN` ingest key.

Ortak resource attr'ları: `service.name=trooper-agent`, `host.name`, `host.id` (machine-id), `os.type`, `host.arch`,
`k8s.cluster.name` (varsa), `k8s.node.name` (varsa), `cloud.provider`/`cloud.region` (varsa).

| Metrik (gauge) | Birim | Attr'lar | Tablo.kolon |
|---|---|---|---|
| `trooper.host.cpu` | 0..1 | — | `hosts_1m.cpu` |
| `trooper.host.memory` / `.memory_bytes` | 0..1 / byte | | `memory` / `memory_bytes` |
| `trooper.host.disk` | 0..1 (en dolu fs) | | `disk` |
| `trooper.host.disk_read` / `.disk_write` | byte/sn | | `disk_read` / `disk_write` |
| `trooper.host.net_in` / `.net_out` | byte/sn | | `net_in` / `net_out` |
| `trooper.host.load1`, `trooper.host.uptime_s`, `trooper.host.cpu_cores` | | | `load1`, `uptime_s`, `cpu_cores` |
| `trooper.container.cpu` / `.cpu_limit` | core | `container.id`, `container.name`, `container.image.name`, `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `trooper.k8s.workload.kind`, `trooper.k8s.workload.name`, `trooper.service` | `containers_1m.cpu` / `cpu_limit` |
| `trooper.container.memory_bytes` / `.memory_limit_bytes` | byte | aynı | `memory_bytes` / `memory_limit` |
| `trooper.container.restarts` | toplam sayaç | aynı | `restarts` |
| `trooper.container.net_in` / `.net_out` | byte/sn | aynı | `net_in` / `net_out` |
| `trooper.container.throttled` | 0..1 | aynı | `throttled` |
| `trooper.process.cpu` (core), `.memory_bytes`, `.threads`, `.fds` | | `process.pid`, `process.executable.name`, `process.command` (sadece ilk token, ≤ 128 karakter), `container.id` | `processes_1m` |
| `trooper.k8s.node.ready` (0/1), `.allocatable_cpu` (core), `.allocatable_memory` (byte), `.pods` | | `k8s.node.name` | `k8s_nodes_1m` |
| `trooper.k8s.pod.ready` (0/1), `.restarts`, `.cpu_request`, `.cpu_limit`, `.memory_request`, `.memory_limit` | | `k8s.namespace.name`, `k8s.pod.name`, `k8s.pod.uid`, `k8s.node.name`, `k8s.pod.phase`, `trooper.k8s.pod.reason`, `trooper.k8s.workload.kind`, `trooper.k8s.workload.name`, `trooper.service` | `k8s_pods_1m` |
| `trooper.k8s.workload.desired` / `.ready` / `.available` / `.updated` | | `k8s.namespace.name`, `trooper.k8s.workload.kind` (Deployment\|StatefulSet\|DaemonSet\|Job\|CronJob), `trooper.k8s.workload.name` | `k8s_workloads_1m` |

- `trooper.k8s.pod.reason`: container waiting/terminated reason (`CrashLoopBackOff`, `OOMKilled`, `ImagePullBackOff`,
  `ErrImagePull`, `CreateContainerConfigError`, `Evicted`, `Error`) yoksa boş. Workload = ReplicaSet → Deployment'a çözülür.
- `trooper.service` = pod label `tasktrooper.io/service` > `app.kubernetes.io/name` > `app`; K8s dışı container'da label
  `tasktrooper.io/service` veya container adı.
- **Event'ler** (cluster modu): sadece `type=Warning` → OTLP log, resource `service.name=k8s`, `k8s.cluster.name`; log attr'ları
  `k8s.event.reason`, `k8s.event.type`, `k8s.object.kind`, `k8s.object.name`, `k8s.namespace.name`, `k8s.event.count`;
  body = message; severity `warn`. Aynı event (uid+count) bir kez.
- SDK/servis tarafı: K8s'te uygulama `OTEL_RESOURCE_ATTRIBUTES=k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE),k8s.node.name=$(NODE_NAME)`
  set ederse span'lar pod'a bağlanır (sdk.md'de örnek).
- **eBPF (OBI):** Helm `ebpf.enabled=true` → OBI DaemonSet; `OTEL_EXPORTER_OTLP_ENDPOINT=<ingest-gw>`, header `x-trooper-key`.
  OBI span'larının `telemetry.sdk.name` değeri `opentelemetry-ebpf-instrumentation` (veya `beyla`); api servis listesinde
  `source: 'ebpf'` döner.

**trooper-agent uygulama notları:** `process.pid` ve `k8s.event.count` OTLP int. K8s'te pod ağı ortak olduğu için `net_in/out`
sadece pod'un ilk container'ında. `trooper.k8s.pod.reason` son `lastState.terminated` sebebini 15 dk tutar (OOM sonrası
CrashLoopBackOff → `OOMKilled`). Job: desired = completions, available = updated = succeeded (çalışan Job `unavailable`
sayılmaz — alerting ve api Job'u hariç tutar); CronJob: desired 1 (askıdaysa 0), son run başarısızsa available 0. Ek env:
`TT_KUBELET_INSECURE`, `TT_KUBELET_URL`, `TT_K8S_API_URL`, `TT_K8S_TOKEN`, `TT_K8S_INSECURE`, `TT_AGENT_ROOTFS`, `TT_AGENT_SYSFS`,
`TT_AGENT_HOSTNAME`, `TT_CLOUD_PROVIDER`, `TT_CLOUD_REGION`.

**ClickHouse `deploy/clickhouse/003_infra_k8s_faas.sql`** (AggregatingMergeTree, gauge'lar `SimpleAggregateFunction(max, Float64)`,
string durum kolonları `SimpleAggregateFunction(anyLast, String)`; TTL 1m tablolar 30 gün, processes 7 gün; raporlanmayan değer
§5 kuralı: max kolonu `-Inf`):
- `hosts_1m (org_id, project_id, cluster, host, host_id, os, arch, node, minute | cpu, cpu_cores, memory, memory_bytes, disk,
  disk_read, disk_write, net_in, net_out, load1, uptime_s)` ORDER BY (org_id, project_id, host, minute).
- `containers_1m (org_id, project_id, cluster, host, namespace, pod, pod_uid, container, container_id, image, workload_kind,
  workload, service, minute | cpu, cpu_limit, memory_bytes, memory_limit, restarts, net_in, net_out, throttled)`
  ORDER BY (org_id, project_id, cluster, namespace, pod, container, minute).
- `processes_1m (org_id, project_id, host, container_id, pid UInt32, exe, command, minute | cpu, memory_bytes, threads, fds)`.
- `k8s_nodes_1m (org_id, project_id, cluster, node, minute | ready, allocatable_cpu, allocatable_memory, pods)`.
- `k8s_pods_1m (org_id, project_id, cluster, namespace, pod, pod_uid, node, workload_kind, workload, service, minute |
  phase anyLast, reason anyLast, ready, restarts, cpu_request, cpu_limit, memory_request, memory_limit)`.
- `k8s_workloads_1m (org_id, project_id, cluster, namespace, kind, name, minute | desired, ready, available, updated)`.
Boş `cluster` = K8s dışı. Writer `trooper.host.*`, `trooper.container.*`, `trooper.process.*`, `trooper.k8s.*`, `trooper.faas.*`
öneklerini bu tablolara yönlendirir.

**Monitor kind `infra`** — `query: {entity: 'host'|'container'|'pod'|'node'|'workload', metric, filter: {cluster?, namespace?,
host?, workload?, service?}, agg: 'max'|'avg'}`; metrikler: host `cpu|memory|disk|load1`, container/pod `cpu_ratio`
(cpu/limit) `|memory_ratio|restarts` (pencere içi artış) `|throttled`, pod `not_ready` (ready=0 olan pod sayısı), node
`not_ready` (sayı), workload `unavailable` (desired − available). Değer = eşleşen varlıklardaki en kötü değer; incident
`detail.entities` en kötü 5 varlık. Şablonlar: pod CrashLoop (`restarts` > 3 / 10 dk), node NotReady (`not_ready` > 0, 5 dk),
host CPU > 0.9 (10 dk), host disk > 0.9, workload `unavailable` > 0 (10 dk).
**RCA:** servis incident'ında aynı `service`'in pod'larında pencere içinde `reason ∈ {OOMKilled, CrashLoopBackOff}` veya restart
artışı → aday "Pod <pod> restarted (OOMKilled)" (confidence 0.6–0.85); pod'un host'unda cpu > 0.9 → "Host <h> CPU saturated".

**API** (`?range=` her yerde; değerler seçili aralığın son tam dakikası, seriler aralık bucket'ları):
- `GET /infra/hosts?cluster=&q=` → `[{id (host.name), hostId, os, arch, cluster, node, cpu, cpuCores, memory, memoryBytes,
  disk, netIn, netOut, load1, uptimeS, containers, lastSeen, status, spark: {cpu: number[], memory: number[]}}]`.
- `GET /infra/hosts/{host}` → host alanları (anlık değerler, liste ile aynı) + `series: {cpu, memory, disk (SeriesSet, 0..1),
  network (SeriesSet: `in`, `out` byte/sn)}`, `containerList: Container[]`, `processes: [{pid, exe, command, containerId, cpu, memoryBytes, threads, fds}]` (son
  dakika, cpu'ya göre), `services: string[]`.
- `GET /infra/containers?host=&service=` → `Container[] = [{id, name, image, host, cluster, namespace, pod, podUid, service,
  cpu, cpuLimit, memoryBytes, memoryLimit, restarts, netIn, netOut, throttled, status}]`.
- `GET /infra/k8s/clusters` → `[{name, nodes, nodesReady, pods, podsRunning, podsFailing, namespaces, workloads,
  cpuUsed, cpuAllocatable, memoryUsed, memoryAllocatable, warningEvents, lastSeen, status}]`.
- `GET /infra/k8s/clusters/{cluster}` → cluster alanları (sayılar, liste ile aynı) + `nodeList: [{name, ready, status, cpu,
  cpuAllocatable, memoryBytes, memoryAllocatable, pods, host}]`, `namespaceList: [{name, pods, workloads, failing}]`,
  `series: {cpu (SeriesSet core: `used`, `requested`, `allocatable`), memory (SeriesSet byte, aynı id'ler)}`.
- `GET /infra/k8s/workloads?cluster=&namespace=` → `[{cluster, namespace, kind, name, desired, ready, available, updated,
  restarts (aralıkta artış), cpu, memoryBytes, service, status}]`.
- `GET /infra/k8s/pods?cluster=&namespace=&workload=&node=&service=` → `Pod[] = [{uid, name, namespace, cluster, node, phase,
  reason, ready, restarts, restartsInRange, cpu, cpuRequest, cpuLimit, memoryBytes, memoryRequest, memoryLimit, workloadKind,
  workload, service, status, containers: [{name, image, cpu, memoryBytes, restarts}]}]`.
- `GET /infra/k8s/pods/{uid}` → Pod (anlık değerler) + `series: {cpu (SeriesSet core: `usage`, `request`, `limit`), memory
  (byte, aynı id'ler), restarts (SeriesSet, bucket başına restart artışı)}`, `events: K8sEvent[]`.
- `GET /infra/k8s/events?cluster=&namespace=&kind=&name=&limit=` → `K8sEvent[] = [{ts, type, reason, message, objectKind,
  objectName, namespace, cluster, count}]` (logs tablosu, `service='k8s'`).
- `GET /services/{svc}/infra` → `{pods: Pod[], hosts: [{id, cpu, memory}], containers: Container[], functions:
  [{id, name, provider}]}`.
- Durum kuralları (`status`: `ok|warning|critical|stale`): host/node `critical` cpu|memory|disk > 0.95 veya NotReady,
  `warning` > 0.85, `stale` son veri > 5 dk. Pod `critical`: phase Failed, reason ∈ {CrashLoopBackOff, OOMKilled,
  ImagePullBackOff, ErrImagePull} veya Running+ready=0; `warning`: restartsInRange > 0 veya Pending. Workload `critical`
  available=0 & desired>0, `warning` available < desired. Cluster: en kötü node/pod durumu.
  Kural: detay uçları liste satırının alanlarını **aynen** taşır; seriler `series` altında, listeler `*List` adıyla (alan adı
  çakışması olmaz).

## 21. Serverless (ADR-020)

**Postgres `deploy/postgres/011_serverless.sql`** (sahibi: integrations):
```sql
-- integrations.provider CHECK'ine 'azure' eklenir; monitors.kind serbest text (kontrol yoksa dokunulmaz)
CREATE TABLE IF NOT EXISTS serverless_functions (
  id bigint GENERATED BY DEFAULT AS IDENTITY PRIMARY KEY,
  project_id bigint NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
  integration_id bigint REFERENCES integrations(id) ON DELETE SET NULL,
  provider text NOT NULL CHECK (provider IN ('aws','azure')),
  slug text NOT NULL,                 -- aws-<region>-<name> | azure-<app>
  name text NOT NULL, app text,       -- Azure: function app adı (name = app)
  region text NOT NULL DEFAULT '', runtime text NOT NULL DEFAULT '', memory_mb integer, timeout_s integer,
  arch text NOT NULL DEFAULT '', triggers text[] NOT NULL DEFAULT '{}',   -- 'api', 'sqs:orders', 'schedule', 'http', 'kinesis:..'
  service text NOT NULL,              -- span service.name eşlemesi (varsayılan name; tag/app setting 'tasktrooper.service')
  url text, meta jsonb NOT NULL DEFAULT '{}',   -- arn, plan (Consumption|Flex|Premium), functions[] (Azure), tags
  last_seen_at timestamptz NOT NULL DEFAULT now(), missing boolean NOT NULL DEFAULT false,
  created_at timestamptz NOT NULL DEFAULT now(), UNIQUE (project_id, slug));
```

**trooper-cloud:**
- AWS `config.services` içinde `lambda` (yeni entegrasyonlarda varsayılan açık). IAM: `lambda:ListFunctions`, `lambda:ListTags`,
  `lambda:ListEventSourceMappings`, `lambda:GetFunctionConcurrency`, `cloudwatch:GetMetricData`.
- Azure: `provider='azure'`, `config: {tenantId, clientId, subscriptionIds: [], resourceGroups?: [], env?}`,
  `credentials_ref` → client secret (düz sealed secret). Token: `https://login.microsoftonline.com/{tenant}/oauth2/v2.0/token`
  (client_credentials, scope `https://management.azure.com/.default`); `TT_AZURE_LOGIN_URL`, `TT_AZURE_ARM_URL` test için
  override. Keşif `Microsoft.Web/sites` (kind `functionapp`), fonksiyonlar `{site}/functions`; metrikler
  `{site}/providers/microsoft.insights/metrics` PT1M. Rol: Reader + Monitoring Reader.
- OTLP metrikler (gauge, sağlayıcı dakikası; resource `service.name=trooper-cloud`, `cloud.provider`, `cloud.region`;
  datapoint attr'ları `trooper.faas.id` = slug, `faas.name`):

| Metrik | AWS kaynağı | Azure kaynağı | `faas_1m` kolonu |
|---|---|---|---|
| `trooper.faas.invocations` | Invocations Sum | FunctionExecutionCount Total | `invocations` |
| `trooper.faas.errors` | Errors Sum | Http5xx Total | `errors` |
| `trooper.faas.throttles` | Throttles Sum | — | `throttles` |
| `trooper.faas.duration.avg` / `.max` / `.p95` / `.p99` (ms) | Duration Average/Maximum/p95/p99 | HttpResponseTime Average/Maximum (s→ms) | `duration_avg` / `duration_max` / `duration_p95` / `duration_p99` |
| `trooper.faas.concurrency` | ConcurrentExecutions Maximum | — | `concurrency` |
| `trooper.faas.iterator_age_ms` | IteratorAge Maximum | — | `iterator_age_ms` |
| `trooper.faas.memory_bytes` | — | MemoryWorkingSet Average | `memory_bytes` |
| `trooper.faas.execution_units` (MB·ms) | — | FunctionExecutionUnits Total | `execution_units` |

**ClickHouse** `faas_1m (org_id, project_id, function_id, provider, region, minute | invocations, errors, throttles, duration_avg,
duration_max, duration_p95, duration_p99, concurrency, iterator_age_ms, memory_bytes, execution_units)` — §20 dosyasında,
`ORDER BY (org_id, project_id, function_id, minute)`. Dakika değerleri gauge `max`; aralık toplamı = dakikaların `sum`'ı.

**Trace / cold start:** fonksiyon span'ları `faas.name`, `faas.coldstart` (bool), `faas.trigger`, `cloud.provider` taşır
(OTel semconv). `@trooper/node` `wrapLambda(handler, opts?)`: ilk çağrıda `faas.coldstart=true`, `faas.invocation_id`,
`faas.trigger` set eder, dönmeden önce flush eder. Cold start sayısı api'de `spans` (`attrs['faas.coldstart']='true'`,
`service` = fonksiyonun `service`'i, `is_entry=1`).

**Maliyet tahmini** (api, USD; env ile override): Lambda `invocations × 0.20/1e6 + GB-s × 0.0000166667` (arm64 `0.0000133334`),
GB-s = Σ(invocations × duration_avg ms / 1000) × memory_mb / 1024. Azure Consumption `execution_units / 1024 / 1000 × 0.000016
+ invocations × 0.20/1e6`. Free tier düşülmez.

**Monitor kind `faas`** — `query: {functionId? | filter: {provider?, region?, service?}, metric: 'invocations'|'errors'|
'error_rate'|'throttles'|'duration_p95'|'duration_p99'|'cold_starts'|'concurrency'|'iterator_age_ms'}`. Şablonlar: error rate
> 0.05 (5 dk), throttles > 0 (5 dk), p95 > timeout × 0.8.

**API:**
- `GET /serverless/summary` → `{functions, invocations, errors, errorRate, throttles, coldStarts, costUsd,
  byProvider: [{provider, functions, invocations, costUsd}]}`.
- `GET /serverless/functions?provider=&q=` → `[{id, slug, name, provider, app, region, runtime, memoryMb, timeoutS, arch,
  triggers, service, invocations, errors, errorRate, throttles, p95Ms, p99Ms, coldStarts, coldStartRate, concurrencyMax, costUsd,
  status: 'ok'|'warning'|'critical'|'idle'|'missing', spark: number[]}]` (spark = invocations bucket'ları). `idle` =
  aralıkta çağrı yok; `critical` errorRate > 0.05 veya throttles > 0; `warning` errorRate > 0.01 veya p95 > timeout × 0.8.
- `GET /serverless/functions/{id}` → function (anlık/aralık değerleri, liste ile aynı) + `series: {invocations (SeriesSet:
  `invocations`, `errors`, `throttles`), duration (SeriesSet ms: `avg`, `p95`, `p99`), concurrency (SeriesSet), coldStarts
  (SeriesSet)}`, `recentErrors: [{ts, traceId,
  message}]` (span'lardan), `functions?: string[]` (Azure), `integration: {id, name}`.
- `POST /integrations` `provider:'azure'` → `config` yukarıdaki gibi, client secret aws/gcp ile aynı `secret` alanında.

**Monitor şablonları:** kaynak `services/alerting/internal/eval/templates.go` (`eval.Templates`); api aynı listeyi
`GET /monitor-templates` → `[{id, name, description, kind, severity, query, condition}]` ile döner. `faas` "p95 > timeout × 0.8"
şablonu `query.relativeToTimeout: true` + eşik 0.8. Incident `rootCause` ek alanları: `infra: [{kind: pod|host, entity,
cluster?, node?, reason, restarts?, value?, title, confidence}]`, `treeSummary`, `treeConfidence`.

**Faz 6 api notları (uygulamadan):** repo seviyesi analiz özeti `Repo.analysis` (`analysis.llm`); inventory satırlarının
`analysis`'i öğe seviyesidir. `Pod.lastSeen` (ms), `K8sNode.status` ek alan. Container durumu: `critical` memory > 0.95 × limit;
`warning` cpu|memory > 0.85, aralıkta restart veya throttled > 0.25; `stale` 5 dk veri yok. eBPF tespiti son 6 saat; servisin tüm
span'ları OBI/beyla ise `source: 'ebpf'`. `GET /serverless/functions/{id}` numeric id veya slug kabul eder; satırlarda `costUsd`.
Faas `relativeToTimeout` sadece `duration_p95|duration_p99` ile geçerli.

## 22. Faz 6 tamamlayıcıları

**trooper-agent:**
- Cluster modu **watch** kullanır: her kaynak için ilk `list` + `watch?resourceVersion=` (allowWatchBookmarks); 410 Gone → yeniden
  list. Snapshot metrikleri yine 30 sn'de bir bellekteki durumdan üretilir. Event'ler de watch ile.
- Container kaynakları sırası (host modu, K8s dışı): Docker Engine API → Podman (Docker uyumlu socket; `TT_AGENT_PODMAN_SOCKET`,
  varsayılan `/run/podman/podman.sock`, rootless `$XDG_RUNTIME_DIR/podman/podman.sock`) → **cgroup v2 fallback** (Linux,
  `/sys/fs/cgroup`; containerd/nerdctl/diğerleri: `cpu.stat`, `memory.current`, `memory.max`, `cpu.max`; container id cgroup yolundan,
  ad/imaj bilinmiyorsa id'nin ilk 12 karakteri). Aynı container iki kaynaktan gelirse ilki kazanır.
- **GPU** (host modu, `nvidia-smi` PATH'te varsa veya `TT_AGENT_NVIDIA_SMI`): `trooper.host.gpu.utilization` (0..1),
  `.memory` (0..1), `.memory_bytes`, `.temperature_c`, `.power_w`; attr `gpu.index`, `gpu.model`, `gpu.uuid`. ClickHouse
  `gpus_1m (org_id, project_id, cluster, host, gpu_index UInt8, gpu_uuid, model, minute | utilization, memory, memory_bytes,
  temperature_c, power_w)` (`deploy/clickhouse/004_gpu.sql`). Host'a ek alan: `hosts_1m` değişmez; api host satırına
  `gpus: [{index, model, utilization, memory}]` ekler (son dakika).
- **Windows** node/host: agent `GOOS=windows` build edilir (host + docker npipe `//./pipe/docker_engine`; process `fds` yok);
  Helm `windows.enabled` → `kubernetes.io/os: windows` nodeSelector'lı ayrı DaemonSet (HostProcess container). `os.type=windows`.
- **Tek Helm chart:** `deploy/helm/trooper-agent` tek kaynak; eski `services/collector/deploy/helm/trooper-agent` silinir.
  DB/broker hedefleri yeni chart'ta `db.enabled` + `db.targets` (statik YAML, eski chart'ın configmap formatı) ve/veya
  `db.fromApi` (`TT_API_URL`/`TT_API_KEY`) ile cluster Deployment'ında çalışır.

**Azure fonksiyon bazında metrik (Application Insights):**
- Function App'e bağlı App Insights bileşeni ARM'dan bulunur: `Microsoft.Insights/components` kaynaklarının
  `hidden-link:<siteId>` tag'i (Reader yeterli). Sorgu ARM proxy'si üzerinden: `POST {componentId}/query?api-version=2018-04-20`
  (`{"query": KQL, "timespan": "PT15M"}`), aynı ARM token'ı.
- KQL: `requests | where timestamp > ago(15m) | summarize invocations=count(), errors=countif(success == false),
  avg=avg(duration), p95=percentile(duration,95), p99=percentile(duration,99), max=max(duration) by bin(timestamp, 1m),
  operation_Name, cloud_RoleName`. Cold start: `customMetrics`/trace yoksa boş.
- Her fonksiyon ayrı `serverless_functions` satırı: slug `azure-<app>-<function>`, `name` = fonksiyon, `app` = app adı,
  `meta.parent` = app slug'ı, `meta.source = 'appinsights'`, `triggers` = fonksiyonun tetikleyicisi. App satırı kalır (app
  seviyesi metrikler). `service` = `cloud_RoleName` (yoksa tag, yoksa app). Metrik attr'ı `trooper.faas.id` = fonksiyon slug'ı.
- App Insights yoksa veya erişim yoksa sadece app satırı (bugünkü davranış), `meta.appInsights = 'unavailable'`.
- Opsiyonel `config.readAppSettings: true` → `POST {site}/config/appsettings/list` (Website Contributor gerekir) ile
  `TASKTROOPER_SERVICE` / `OTEL_SERVICE_NAME` okunur; yetki yoksa sessizce atlanır.
- api: `GET /serverless/functions` satırlarına `parentId` (app satırının id'si, sadece Azure fonksiyon satırlarında); liste varsayılan
  olarak hepsini döner; `?parent=<id>` filtresi. Function detail'da Azure app için `children: Function[]`.

**Pod bazında trace ve log:**
- `GET /traces` yeni filtreler: `pod`, `namespace`, `node`, `host` (span `resource_attrs['k8s.pod.name']`,
  `['k8s.namespace.name']`, `['k8s.node.name']`, `['host.name']`).
- `GET /logs` yeni filtreler: `pod`, `namespace` (`attrs['k8s.pod.name']`, `attrs['k8s.namespace.name']`).
- Pod sayfası: "Traces" bölümü `/traces?service=&pod=` listesini gösterir; "Logs" linki `/logs?pod=&namespace=` ile sunucu tarafı filtre.
