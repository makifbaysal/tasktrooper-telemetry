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
