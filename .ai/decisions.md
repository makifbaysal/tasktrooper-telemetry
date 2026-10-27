# Mimari kararlar (ADR)

Format: bağlam → karar → sonuç. Yeni karar en alta, numara artarak.

## ADR-001 — Wire protokolü olarak OTLP
**Bağlam:** Kendi protokolümüz SDK'ları sıfırdan yazmayı gerektirir; müşteriler zaten OTel kullanıyor olabilir.
**Karar:** Backend SDK'ları OTel SDK üzerine ince katman; ingest OTLP kabul eder. RUM için hafif özel JSON beacon.
**Sonuç:** Hızlı dil kapsamı, mevcut OTel kullanıcıları SDK'sız gelebilir. Browser bundle küçük kalır (< 15 KB gz hedef).

## ADR-002 — Telemetri deposu ClickHouse
**Bağlam:** Yüksek yazma hızı, kolon bazlı agregasyon, log full-text, uzun retention.
**Karar:** Metrik, span, log, RUM tek ClickHouse cluster'ında. Prometheus/Loki/Tempo ayrı stack'i değil.
**Sonuç:** Tek sorgu dili ile korelasyon (trace ↔ log ↔ RUM) kolay. Operasyon yükü tek sisteme iner.

## ADR-003 — Ingest buffer olarak Redpanda
**Bağlam:** ClickHouse yavaşlaması ingest kaybına yol açmamalı; stream alert'leri için event akışı lazım.
**Karar:** Redpanda (Kafka API uyumlu, JVM yok).
**Sonuç:** Replay, backpressure, stream consumer'lar (alert, pattern çıkarma).

## ADR-004 — Backend Go, frontend React + Vite
**Bağlam:** Ingest yolu yüksek throughput ister; ekip Go ve React'e hakim.
**Karar:** Tüm servisler Go. Dashboard React 19 + Vite SPA (SSR ihtiyacı yok, auth arkasında).
**Sonuç:** Tek binary deploy, düşük memory. Dashboard statik host edilebilir.

## ADR-005 — Grafikler kendi SVG komponentlerimiz
**Bağlam:** Observability grafikleri sınırlı sayıda form (time series, bar, histogram, heatmap, sparkline); design system'e tam uyum ve küçük bundle istiyoruz.
**Karar:** `apps/web/src/components/charts` altında hafif SVG komponentleri. Çok yoğun veri (>10k nokta) gerekirse uPlot'a geçiş değerlendirilecek.
**Sonuç:** Token'larla tam uyum, dark mode, hover katmanı elimizde.

## ADR-006 — Load test motoru k6
**Bağlam:** Kendi load generator'ımızı yazmak yerine olgun motor.
**Karar:** k6 Go kütüphanesi runner'a gömülü; declarative YAML → k6 script derleme.
**Sonuç:** Script modu ile ileri kullanıcıya tam güç; metrikler OTLP ile bize akar.

## ADR-007 — LLM yorumlama için, çıkarma için değil
**Bağlam:** Kod analizinde halüsinasyon riski.
**Karar:** Envanter tree-sitter extractor'ları ile deterministik çıkarılır; LLM sadece kritiklik, açıklama ve öneri üretir.
**Sonuç:** Envanter güvenilir; LLM kapatılabilir.

## ADR-008 — KPI = SLO, alert'ler burn rate üzerinden
**Bağlam:** Kullanıcı "anasayfa hızlı açılsın" gibi deneyim hedefleri tanımlamak istiyor; ham eşik alert'leri gürültülü.
**Karar:** KPI bir SLO'dur: hedef (sayfa/endpoint/journey/app start), good-event tanımı, objective, pencere (7d/28d).
Alert'ler multi-window burn rate (hızlı 1h+5m, yavaş 6h+30m) + opsiyonel "hedefin altında" kuralı.
**Sonuç:** Kısa sıçramalar sayfa açmaz; bütçe tükeniyorsa erkenden uyarır. Frontend ve backend KPI'ları aynı modelde.

## ADR-009 — Kök neden: trace grafiğinde delta attribution
**Bağlam:** "Hata benden mi, çağırdığım servisten mi?" sorusuna otomatik cevap; ML modeli açıklanamaz ve eğitim verisi yok.
**Karar:** `edges_1m`'den çağrı ağacı kurulur; her çocuğun katkısı = `çağrı/istek × (şimdi − baseline)`; latency'de paralel
HTTP çağrılarında sadece kritik yol sayılır; açıklanmayan kalan "own code". En büyük payı takip ederek yaprağa / own code'a
iner; güven = yol boyunca payların çarpımı. Kanıt (deploy, migration, sorgu fingerprint, kaynak) kök düğüme iliştirilir.
**Sonuç:** Deterministik, açıklanabilir, her adımda "ne kadarını açıklıyor" gösterilebilir. Ortak bağımlılıkta korelasyonlu
düşüşleri ayırt edemez — kardeş ≥%15 ise alternatif olarak listelenir.

## ADR-010 — Telemetri deposu: Cassandra değil ClickHouse (yeniden değerlendirme)
**Bağlam:** Yüksek yazma yükü (hedef 200k span/sn + 500k log/sn tek bölge) için Cassandra / ScyllaDB önerildi.
**Değerlendirme:**
- Cassandra/Scylla: yazma throughput'u ve lineer ölçek çok iyi, multi-DC replikasyon olgun. Ama sorgu sadece partition key
  üzerinden; `GROUP BY`, percentile, ad-hoc filtre, full-text yok. Ürünün tüm ekranları agregasyon (servis/endpoint/zaman bazlı
  p95, top-N query, lag per group, log arama). Her görünüm yazarken ayrı tabloya önceden hesaplanmak zorunda kalır; yeni ekran =
  yeni tablo + backfill. Time-series + TTL'de tombstone/compaction yükü (TWCS şart), kolon sıkıştırması olmadığı için disk ~5–10×.
- ClickHouse: kolon bazlı, 10–20× sıkıştırma, node başına milyonlarca satır/sn batch insert, vektörize `quantilesTDigest`,
  materialized view ile rollup, `tokenbf_v1` ile log arama, tablo bazlı TTL + S3 tiering. Sharding (`Distributed`) +
  `ReplicatedMergeTree` + ClickHouse Keeper ile yatay ölçek. Sentry (Snuba), PostHog, SigNoz, Uptrace, Cloudflare aynı iş için kullanıyor.
**Karar:** ADR-002 geçerli: telemetri ClickHouse. Yüksek yük için: Redpanda buffer (ingest ClickHouse'tan bağımsız), writer'da
büyük batch (≥50k satır / 1 sn), `async_insert` kapalı (batch'i biz yapıyoruz), shard key `cityHash64(org_id, project_id, service)`,
hot/cold tiering (7 gün NVMe → S3). Metadata Postgres, rate limit/cache Redis, blob'lar (sourcemap, crash dump, replay) S3.
**Sonuç:** Cassandra eklenmiyor. İleride çok yüksek QPS'li key-value ihtiyacı (ör. trace-by-id cache, session state) çıkarsa
ScyllaDB değerlendirilir; şu an trace-by-id `trace_index` tablosuyla ClickHouse'ta çözülüyor.

## ADR-011 — SDK stratejisi: OpenTelemetry dağıtımı + RUM JSON
**Bağlam:** Go (gin, fiber), Java (Spring, Quarkus), Kotlin, .NET backend; Android, iOS, Flutter, React Native mobil; web JS/TS.
**Karar:**
- **Backend SDK'ları = OTel distro'su.** Kendi exporter'ımızı yazmıyoruz; OTLP/HTTP protobuf ile `ingest-gw`'ye. Distro'nun
  işi: tek satır init, DSN, resource (service/env/version), `trooper.*` attribute'ları (route template, entry route, consumer
  group/topic, caller), PII varsayılanları, sampling. Framework auto-instrumentation'ı OTel'in kendi kütüphanelerinden gelir;
  sadece eksik olan için kod yazarız (ör. route template, consumer group, Fiber/Gin route).
  - Go: `trooper-go` + `troopergin`, `trooperfiber`, `trooperecho`, `trooperchi`, `trooperhttp`, `trooperkafka` (franz-go, sarama),
    `trooperamqp`, `troopersql`, `trooperredis`, `trooperslog`.
  - JVM (Java, Kotlin): OTel Java agent **extension** (`trooper-javaagent.jar` — agent + extension tek jar) → Spring Boot, Quarkus
    (JVM mode), Ktor, Kafka, RabbitMQ, JDBC hepsi agent'tan. Agent kullanamayanlar için `trooper-spring-boot-starter` ve
    `trooper-quarkus` (Quarkus'un `quarkus-opentelemetry` extension'ını yapılandırır, native image dahil), `trooper-ktor` plugin.
  - .NET: `TaskTrooper.Telemetry` NuGet — `AddTaskTrooper()` (ASP.NET Core, HttpClient, EF Core/SqlClient, Confluent.Kafka, RabbitMQ).
- **Client SDK'ları (web, RN, Flutter, Android, iOS) = RUM JSON** (`POST /v1/rum`, şema `proto/rum/v1/rum.proto`). OTel client
  SDK'ları ağır ve RUM kavramlarını (session, view, vital, crash, app start) kapsamıyor. Network istekleri `traceparent`
  header'ı ile backend trace'ine bağlanır.
  - Web: `@trooper/browser` framework'süz çalışır (History API ile SPA route, window error, fetch/XHR, Web Vitals). Framework'ün
    hataları yuttuğu yerler için ince paketler: `@trooper/react` (ErrorBoundary, React 19 root error callback'leri),
    `@trooper/vue` (`app.config.errorHandler`), `@trooper/angular` (`ErrorHandler` provider). Svelte, Solid, Next.js, Nuxt ek paket istemez.
  - `@trooper/react-native` (JS; react-navigation ekran takibi, ErrorUtils, fetch/XHR, AppState, offline kuyruk).
  - `trooper_flutter` (Dart), `TaskTrooper` Android (Kotlin, AAR), `TaskTrooper` iOS (Swift Package).
**Sonuç:** Backend dil kapsamı ucuz ve OTel kullanan müşteri SDK'sız gelebilir. Client tarafı tek şema → tek writer yolu.

## ADR-012 — Cloud entegrasyonları: AWS, Google Cloud, Vercel, Neon
**Bağlam:** Müşterinin DB/queue/deploy verisinin bir kısmı sadece bulut sağlayıcının API'sinde var (RDS/ElastiCache CPU-disk,
MSK broker metrikleri, SQS/Pub/Sub kuyrukları, Cloud SQL, Neon compute, Vercel deploy + log + Web Vitals). SQL ile alınamıyor.
**Karar:**
- Yeni servis `services/integrations` (`cmd/trooper-cloud`): Postgres `integrations` tablosundaki her bağlantı için
  **keşif** (kaynakları `databases` / `broker_clusters` satırı olarak upsert, `collector = 'cloud'`, `integration_id`) ve
  **metrik çekme** (60 sn) yapar. Metrikleri collector ile **aynı OTLP isimleriyle** ingest-gw'ye yollar (contracts §5) —
  writer ve api değişmeden "Cloud integration" kaynaklı veriyi gösterir.
  - AWS: STS AssumeRole (external id ile, önerilen) veya access key. CloudWatch `GetMetricData`: RDS/Aurora, ElastiCache,
    MSK (broker node + `SumOffsetLag`), SQS. Keşif: `rds:DescribeDBInstances`, `elasticache:DescribeReplicationGroups`,
    `kafka:ListClustersV2`, `sqs:ListQueues`.
  - Google Cloud: service account JSON (veya workload identity). Cloud Monitoring `timeSeries.list`: Cloud SQL, Memorystore Redis,
    Pub/Sub (`num_undelivered_messages`, `oldest_unacked_message_age`). Keşif: Cloud SQL Admin, Memorystore, Pub/Sub API.
  - Neon: API key. Projeler/branch'ler/compute endpoint'leri → `databases` (engine postgres); compute durumu, CU, active time,
    storage → `trooper.db.*`; `pg_stat_statements` için mevcut collector'ın direct modu (Neon rolü ile) kullanılır.
  - Vercel: access token (+ team id). REST API ile projeler ve **deploy'lar** → `deploy_markers` (servis eşlemesi: Vercel proje
    adı = servis adı, değiştirilebilir). **Drains**: Vercel log / trace / speed-insights / web-analytics drain'leri
    ingest-gw'deki `POST /v1/drains/vercel`'e gelir (`x-vercel-signature` HMAC-SHA1 doğrulaması) ve log, span, RUM vital /
    view'a çevrilir.
- SQS ve Pub/Sub, RabbitMQ gibi **kuyruk** modelidir: `broker_clusters.kind ∈ {kafka, rabbitmq, sqs, pubsub}`; lag = görünür /
  teslim edilmemiş mesaj, ek olarak en eski mesaj yaşı (`oldest_age_s`).
- Kimlik bilgileri: UI'dan girilen secret'lar Postgres `secrets` tablosunda AES-256-GCM ile şifreli (`TT_SECRETS_KEY`, 32 byte
  base64); `credentials_ref = 'secret:<id>'`. Diğer referanslar `env:` / `file:` (collector ile aynı). Secret'lar API
  cevaplarında asla dönmez.
**Sonuç:** Bulut verisi ayrı bir pipeline istemiyor; sadece yeni bir "kaynak". Lambda / Cloud Run / Vercel Functions için
RED metrikleri span'lardan gelir (OTel/Vercel trace drain); bulut metriklerinden servis RED'i türetilmez.

## ADR-013 — Dashboard veri modu: senkron API + uzak cache
**Bağlam:** Ekranlar `src/data/*` fonksiyonlarını senkron çağırıyor (`services`, `endpointSeries(...)`). Hepsini async'e çevirmek
27 ekranı yeniden yazmak demek.
**Karar:** Fonksiyon imzaları aynı kalır. `VITE_DATA=api` iken (default `mock`):
- Liste verisi (`services`, `monitors`, `incidents`, `kpis`, `databases`, `clusters`, `topics`, `consumerGroups`, senaryolar,
  entegrasyonlar …) uygulama açılırken `hydrate()` ile api'den çekilir ve mevcut export edilen dizilerin **içi değiştirilir**
  (referans aynı kalır). 60 sn'de bir ve mutasyon sonrası yenilenir.
- Detay/seri fonksiyonları `remote(path, fallback)` kullanır: cache'te varsa döner, yoksa isteği başlatır ve boş/sıfır
  fallback döner; cevap gelince veri versiyonu artar ve `useApp()`/`useData()` kullanan ekranlar yeniden render olur.
- Mutasyonlar (`addDatabase`, `addLagMonitor`, ack/resolve …) api'ye POST eder, sonra ilgili cache'i geçersiz kılar.
**Sonuç:** Ekran kodu değişmez; mock mod geliştirme/demo için korunur. Yükleme sırasında grafikler kısa süre sıfır görünür
(kabul edildi; kritik ekranlarda `remoteStatus()` ile ince yükleme çubuğu).

## ADR-014 — Code intelligence servisi (`services/codeintel`)
**Bağlam:** Faz 4: repo okuyup envanter, alert önerisi ve load test taslağı üretmek. Tablolar (`repositories`,
`inventory_items`, `alert_suggestions`) 001'de var, servis yok.
**Karar:**
- Ayrı Go servisi `codeintel` (:8096). api ile aynı Postgres'i kullanır; ClickHouse'u sadece okur (gözlem eşleştirme).
  api tetikler (`repositories.analyze_requested_at`), codeintel 15 sn'de bir yoklar (trooper-cloud `syncRequestedAt` deseni).
- Repo erişimi: GitHub App (app id + private key secret'ı, installation token) **veya** token (`secret:`), dev/demo için
  `local` provider (`file://` yol). Clone shallow; analiz bitince kaynak silinir.
- Parse: tree-sitter yerine **Go'da yazılmış deterministik extractor'lar** (Go için `go/ast`; TS/JS ve Python için hafif
  tokenizer + framework kalıpları). Gerekçe: CGO'suz tek binary, extractor'lar zaten kalıp tabanlı. Kapsam code-intelligence.md
  tablosu.
- LLM (Claude): sadece **yorumlama** (kritiklik, özet, failure mode, monitor önerisi, loadtest_hint). Model ID `TT_CODEINTEL_MODEL`,
  anahtar `ANTHROPIC_API_KEY`; ikisinden biri yoksa LLM adımı kapalı → statik kurallar. Sonuçlar dosya hash'i ile cache'lenir.
- Alert-as-code: scan sırasında `.trooper/monitors/*.yaml` okunur → `monitors` tablosuna `source='code'` upsert
  (`code_ref` = repo:path). UI ve api bu monitor'leri read-only tutar. Repo'dan silinen dosyanın monitor'ü silinir.
- Webhook: `POST /v1/webhooks/github` (HMAC `X-Hub-Signature-256`) → default branch push'unda incremental analiz.
**Sonuç:** Ek dil/framework yeni extractor dosyası demek. Tree-sitter doğruluğu gerekirse sonradan eklenebilir.

## ADR-015 — Load test kontrolcüsü ve runner (`services/loadtest`)
**Bağlam:** Faz 5: api senaryo/run CRUD'u var, runner yok; run'lar `queued` kalıyor.
**Karar:**
- Modül `services/loadtest`: `loadtest-ctl` (:8097) + `loadtest-runner`. ctl `loadtest_runs.state='queued'` satırlarını
  `FOR UPDATE SKIP LOCKED` ile alır; domain doğrulaması ve plan limitlerini kontrol eder; runner'ı başlatır
  (`TT_LOADTEST_EXECUTOR=local` alt süreç, `k8s` → Job manifesti, bölge başına bir Job).
- Yük motoru: k6 gömmek yerine **kendi Go motorumuz** (VU ve arrival-rate modları, stage'ler, check'ler, threshold'lar,
  think time, şablon değişkenleri). Gerekçe: k6'yı kütüphane olarak gömmek desteklenen bir API değil ve bağımlılık ağacı ağır;
  declarative senaryo formatımız zaten sınırlı. Ham k6 script modu: runner `k6` binary'si PATH'te varsa onu çağırır.
- Korelasyon: runner her isteğe `traceparent` + `baggage: tt.svc=loadtest:<run_id>` koyar → hedef servisin SDK'ları bunu
  `trooper.caller` olarak yazar; UI test trafiğini caller ile ayırır. Ek header `x-trooper-loadtest-run: <run_id>`.
- Runner metrikleri OTLP ile kendi ingest'imize (`service.name = loadtest`, attr `trooper.loadtest.run`), canlı özet her 5 sn
  `loadtest_runs.live` (jsonb) kolonuna. Bitince `summary`, `thresholds_passed`, `state ∈ passed|failed|aborted|error`.
- Güvenlik: hedef host `loadtest_domains`'de doğrulanmış olmalı (DNS TXT `trooper-verify=<token>` veya
  `https://<host>/.well-known/trooper-verify` içeriği = token); `localhost`/`*.localhost`/`127.0.0.1` dev'de
  (`TT_LOADTEST_ALLOW_LOCAL=true`) muaf. Limitler: `TT_LOADTEST_MAX_VUS`, `TT_LOADTEST_MAX_DURATION`, aylık VU-dakika.
  Abort: api `POST /loadtests/runs/{id}/stop` → `abort_requested_at`; ctl runner'ı durdurur. Opsiyonel otomatik abort
  (`spec.gate.abortErrorRate`).
**Sonuç:** k6 ekosisteminin tamamı yok; declarative kapsam bilinçli olarak sınırlı.

## ADR-016 — Kullanıcı kimliği: OIDC + oturum, API key'ler korunur
**Bağlam:** Dashboard şu an tek bir dev api key'i ile çalışıyor; kullanıcı, org, rol yok.
**Karar:**
- api OIDC Authorization Code + PKCE akışını yürütür (`TT_OIDC_ISSUER`, `TT_OIDC_CLIENT_ID`, `TT_OIDC_CLIENT_SECRET`,
  `TT_OIDC_REDIRECT_URL`). Başarılı girişte `users` upsert, httpOnly `tt_session` cookie (Postgres `sessions`, 7 gün, kaydırmalı).
- İstekte kimlik sırası: `Authorization: Bearer <api key>` (otomasyon, CLI, SDK upload) → `tt_session` cookie. Cookie ile
  gelen isteklerde proje `X-Trooper-Project` header'ından (yoksa kullanıcının ilk projesi).
- Roller org seviyesinde: `owner | admin | member | viewer`. viewer sadece GET; admin+ üye/anahtar/entegrasyon yönetir.
- OIDC yapılandırılmamışsa (`TT_OIDC_ISSUER` boş) **dev login**: `POST /auth/dev-login {email}` sadece `TT_AUTH_DEV=true`
  iken; UI bu durumda e-posta ile giriş gösterir. Mevcut `VITE_API_KEY` akışı dev için çalışmaya devam eder.
- `audit_log` tablosu: kim, ne zaman, hangi mutasyon.
**Sonuç:** SAML/SCIM sonraki iş. API key'ler proje kapsamlı kalır.
