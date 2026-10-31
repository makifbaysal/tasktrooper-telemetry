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
**Ek (GitLab / Bitbucket, contracts §18a):** OAuth uygulaması yerine repo başına token + repo başına webhook. Self-managed
GitLab adresi ayrı bir integration satırında değil `repositories.base_url`'de (repo başına tek alan; aynı projede farklı
GitLab'lar mümkün, sorgular basit kalır). Webhook secret'ını kullanıcı değil api üretir (zayıf secret riski yok) ve bir kez
gösterir. Bitbucket push payload'ı dosya listelemediği için her push `{*}` kuyruğa koyar; asıl daraltma git diff incremental'da.

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

## ADR-017 — Slack App ve GitHub App entegrasyonları
**Bağlam:** Alert'ler bugün Slack'e sadece incoming webhook ile gidiyor (kanal webhook'a sabit). Repo bağlamak token/kurulum id
elle girmeyi gerektiriyor.
**Karar:**
- **Slack App** (OAuth v2, bot scope'ları `chat:write`, `chat:write.public`, `channels:read`, `groups:read`, `channels:join`,
  `users:read.email`): org başına bir
  kurulum, bot token `secrets`'ta şifreli. Monitor/KPI kuralı bildirim hedefi `slack:<channel_id>` (görünen ad `#kanal`
  ayrıca saklanır); notifier `chat.postMessage` ile o kanala yazar, mesaj `ts`'ini saklar ve resolve/hatırlatma/ack aynı
  mesajın **thread**'ine gider (ilk mesaj `chat.update` ile durum rengini değiştirir). Eski `slack:` webhook kanalları çalışmaya
  devam eder.
- Mesaj içeriği: başlık, durum, değer/eşik, servis/endpoint, grafik linki, **olası kök neden + güven yüzdesi** (alert-engine'in
  RCA sonucu: `rootCause.summary`, `rootCause.confidence` → "Likely cause: … (78% confidence)"; confidence < 0.3 ise "low
  confidence" etiketi, RCA yoksa satır gösterilmez), deploy (son 1 saat), Ack / Open butonları (Slack interactivity →
  api `POST /slack/interactions`, imza `X-Slack-Signature` doğrulaması).
- **GitHub App**: kurulum akışı api'de (`/integrations/github/install` → GitHub → `/integrations/github/callback?installation_id=`),
  kurulumun repo listesi UI'da seçilir, seçilen repolar `repositories`'e eklenir ve analiz kuyruğa girer. App webhook'u
  (tek URL) codeintel'e gelir: `push` (default branch) → incremental analiz, `installation`/`installation_repositories` →
  repo ekle/kaldır senkronu, `pull_request` → ileride. Ortak GitHub App istemcisi `services/pkg/githubapp`.
**Sonuç:** Kanal seçimi UI'da listeden yapılır; webhook URL'si kopyalamak gerekmez.

## ADR-018 — AI kullanım kotası: kredi, org + üye limiti
**Bağlam:** LLM çağrıları (bugün codeintel zenginleştirme; ileride RCA, öneri) tek bir Anthropic anahtarıyla yapılıyor ve
tek sınır analiz başına öğe sayısı (`TT_CODEINTEL_LLM_MAX_ITEMS`). Paket bazında "bu ay şu kadar hakkın var" ve kişi bazında
tavan gerekiyor.
**Karar:**
- Birim **kredi**: 1 kredi = 0,01 USD model maliyeti. Token → kredi dönüşümü model fiyat tablosuyla (config; model id sabit
  kodlanmaz). Farklı modeller tek ölçüde toplanır.
- Dönem UTC takvim ayı; ayın 1'inde sıfırlanır (sayaç değil, `ai_usage` defterinden SUM).
- Limit katmanları: **paket varsayılanı** (`free` 100, `team` 5.000, `enterprise` sınırsız) → **org override**
  (`orgs.ai_monthly_credits`) → **üye tavanı** (`memberships.ai_monthly_credits`, sadece o kişinin başlattığı işler).
  Otomatik işler (webhook ile analiz) sadece org limitinden düşer.
- Kontrol her LLM çağrısından **önce** (tahmini maliyetle), kayıt çağrıdan **sonra** (gerçek token). Org başına Postgres
  advisory lock ile sıralanır; limit "soft": en fazla bir çağrı kadar aşılabilir.
- Limit dolunca iş durmaz: LLM adımı atlanır, statik kurallarla devam edilir ve sonuçta `quota_exceeded` görünür.
- %80 ve %100 eşiklerinde org'un seçtiği bildirim hedeflerine (Slack kanalı vb.) tek mesaj (ay+eşik başına dedupe).
- Ortak mantık `services/pkg/aiquota` (bağımlılıksız; Postgres erişimi çağıranın verdiği arayüzle).
**Sonuç:** Faturalama/ödeme yok; paket `orgs.plan`'dan okunur. Kullanım ekranı Settings → Usage.

## ADR-019 — Host, Kubernetes ve container izleme (trooper-agent modları + opsiyonel eBPF)
**Bağlam:** Altyapı tarafında sadece DB ve broker izleniyor. Host, K8s cluster, pod, container ve process görünürlüğü yok.
New Relic bunu Pixie (eBPF) ile yapıyor.
**Karar:**
- Yeni ajan yazmıyoruz; `trooper-agent` (services/collector) **modlar** kazanır: `db` (bugünkü), `host` (host + container +
  process; K8s'te DaemonSet), `cluster` (K8s API'den nesne durumu + Warning event'leri; tek replika Deployment).
  `TT_AGENT_MODES=host,cluster` gibi birleştirilebilir.
- Host metrikleri gopsutil, container metrikleri K8s'te kubelet `/stats/summary` (+ `/pods` metadata), K8s dışında Docker Engine
  API (salt-okunur socket). Process'ler host başına CPU+bellek top 20; komut argümanları **toplanmaz** (PII).
- Cluster modu salt-okunur RBAC (get/list/watch); metrikler 30 sn'de bir snapshot, Warning event'leri OTLP log
  (`service.name=k8s`).
- **eBPF**: kendi eBPF kodumuzu yazmıyoruz. Helm chart opsiyonel olarak OpenTelemetry eBPF Instrumentation (OBI) DaemonSet'i
  kurar (`ebpf.enabled`, varsayılan kapalı); OBI enstrümante edilmemiş HTTP/gRPC/SQL servislerinden span + RED metriği üretip
  OTLP ile ingest-gw'ye yollar → mevcut trace/servis haritası yolu. UI bu servisleri "eBPF" rozetiyle gösterir.
- Veri tipli ClickHouse tablolarına (`hosts_1m`, `containers_1m`, `processes_1m`, `k8s_pods_1m`, `k8s_nodes_1m`,
  `k8s_workloads_1m`) yazılır; servis ↔ pod bağı pod label'ından (`tasktrooper.io/service` > `app.kubernetes.io/name` > `app`).
- Alert: yeni monitor kind `infra` (host/container/pod/node/workload); alert-engine RCA, servis incident'ında aynı servisin
  pod'larındaki OOMKilled/CrashLoop/restart ve host CPU doygunluğunu kök neden adayı yapar.
- Dağıtım: tek chart `deploy/helm/trooper-agent` (Linux DaemonSet + cluster Deployment + RBAC + opsiyonel OBI + opsiyonel
  Windows HostProcess DaemonSet + `db.enabled` ile DB/broker collector); K8s dışı host için `docker run` / systemd.
- Cluster modu list + watch kullanır; K8s dışı container'lar Docker → Podman → cgroup v2 sırasıyla, GPU `nvidia-smi` ile (§22).
**Sonuç:** Pixie'deki tam protokol ayrıştırma (MySQL/Kafka payload, flame graph) kapsam dışı; OBI'nin verdiği kadar.

## ADR-020 — Serverless: AWS Lambda ve Azure Functions
**Bağlam:** Serverless fonksiyonlar görünmüyor; AWS entegrasyonu sadece RDS/ElastiCache/MSK/SQS keşfediyor, Azure entegrasyonu yok.
**Karar:**
- **AWS Lambda** mevcut AWS entegrasyonunun yeni servisi (`config.services` içinde `lambda`): ListFunctions + tag + event source
  mapping keşfi (10 dk), CloudWatch `AWS/Lambda` metrikleri (60 sn): Invocations, Errors, Throttles, Duration
  (avg/max/p95/p99), ConcurrentExecutions, IteratorAge.
- **Azure** yeni sağlayıcı (`provider='azure'`): service principal (tenant/client id + secret), abonelik listesi. Function App'ler
  ARM'dan keşfedilir; Azure Monitor metrikleri **app** seviyesindedir (FunctionExecutionCount, FunctionExecutionUnits,
  Requests, Http5xx, HttpResponseTime, MemoryWorkingSet) → Azure'da bir "fonksiyon" satırı = bir Function App; içindeki
  fonksiyonlar `meta.functions`.
- Trace/cold start: müşteri fonksiyonu OTel ile enstrümante eder (AWS: OTel/ADOT Lambda layer veya `@trooper/node`
  `wrapLambda`; Azure: Functions OpenTelemetry modu) ve OTLP'yi ingest-gw'ye yollar. Cold start sayısı span'lardaki
  `faas.coldstart=true`'dan hesaplanır (CloudWatch'ta yok).
- Maliyet tahmini api'de: GB-saniye + istek fiyatı (sabitler config'te), UI'da "tahmini" etiketiyle.
- Metadata Postgres `serverless_functions`, metrikler ClickHouse `faas_1m`; alert için monitor kind `faas`.
**Sonuç:** Google Cloud Functions/Run, Vercel Functions ileride aynı tabloya eklenebilir.

## ADR-021 — Session replay: web için rrweb, mobil için wireframe
**Bağlam:** RUM oturumları sayı ve olay listesi olarak görünüyor; kullanıcının ekranda ne yaşadığı görünmüyor. New Relic web
ve mobil session replay sunuyor. "Kapsam dışı (v1)" listesindeydi.
**Karar:**
- **Web:** kayıt `rrweb` (`@rrweb/record` 2.2.0) ile DOM snapshot + mutation; ayrı paket `@trooper/replay` (browser
  SDK'yı şişirmemek için, `@trooper/browser`'a integration olarak takılır). Oynatma UI'da `@rrweb/replay` 2.2.0.
- **Mobil (Android, iOS):** piksel/ekran görüntüsü **toplanmaz**. View hiyerarşisinden **wireframe** (dikdörtgen, tür, renk,
  maskeli metin) kare kare çıkarılır, sadece değişince ve en fazla 2 kare/sn; dokunmalar ayrı event. Oynatıcı bizim (SVG).
  Böylece görüntü içinde PII sızması yapısal olarak imkânsız.
- **React Native:** RN native view'larla çizer → native SDK'nın replay'i (Android `trooper-replay`, iOS `TaskTrooperReplay`)
  aynen çalışır; `@trooper/react-native` `TrooperRum` native module'ü üzerinden başlatır, oturum JS'ten `setContext` ile gelir.
- **Flutter:** tek bir `FlutterView` yüzeyine çizdiği için native traversal işe yaramaz; `trooper_flutter` Dart tarafında
  **Element ağacından** wireframe çıkarır (widget tipi → kind, `Material`/`ColoredBox`/`DecoratedBox` → renk/radius,
  `RenderBox` → global rect), aynı wire formatı.
- **Gizlilik default:** web'de tüm metin ve input maskeli (`maskAllText`, `maskAllInputs` true), password her zaman,
  `data-trooper-block` öğesi yer tutucu, canvas / cross-origin iframe / network gövdesi kaydedilmez. Mobilde metin maskeli,
  görseller sadece kutu, input her zaman maskeli. Unmask açıkça (`data-trooper-unmask` / `trooperUnmask()`).
- **Örnekleme:** oturum başına karar: `sampleRate` (default 0.1) kaydeder; `onErrorSampleRate` (default 1.0) bellekte son 60 sn'yi
  tutar, hata olunca flush edip kayda devam eder. Karar oturum boyunca sabit (web: localStorage, sekmeler arası ortak).
- **Taşıma:** segment'ler (≤ 5 sn veya ≤ 256 KB) `POST /v1/replay` → `tt.replay` → writer blob'u **S3**'e (`tt-replays` bucket),
  indeksi ClickHouse `replay_segments`'e yazar. Retention 30 gün. Oturum meta verisi (kullanıcı, cihaz, hatalar, view'lar)
  `rum_events`'ten gelir; replay ayrı bir oturum kavramı getirmez (`session_id` ortak).
- **Korelasyon:** oynatıcının zaman çizelgesi `rum_events`'ten (view, action, error, resource); resource event'lerindeki
  `trace_id` ile replay'den backend trace'ine (`/traces/:id`) geçilir. Hata detayından "Watch replay" ilgili saniyeye atlar.
**Sonuç:** Mobilde gerçek görüntü yok (bilinçli); web'de canvas/WebGL içerik boş görünür. Detay: contracts.md §23.
Web'de karar/sayaç localStorage'da sekmeler arası ortak; buffer modundaki sekme `storage` event'iyle `session`'a geçer.

## ADR-022 — Continuous profiling: pprof + folded stack, ClickHouse'ta stack satırları
**Bağlam:** Endpoint yavaşladığında "where time goes" bağımlılık kırılımı veriyor ama kendi kodumuzun (`app` kategorisi) içinde
hangi fonksiyonun CPU/bellek yaktığı görünmüyor. New Relic thread profiler ve flame graph sunuyor.
**Karar:**
- SDK'lar sürekli, düşük overhead'li profil alır (varsayılan: her 60 sn'de 10 sn CPU, 60 sn'de bir heap/goroutine) ve
  `POST /v1/profiles`'a yollar. İki wire formatı: **pprof** (Go'nun yerel formatı) ve **folded stack** (`a;b;c 123`, JVM/Node
  için basit; bağımlılık gerektirmez). OTLP profiles sinyali henüz stabil değil — stabil olunca gw'de kabul edilir.
- **Go:** `runtime/pprof`; istek goroutine'ine pprof label `trooper.route` / `trooper.method` konur → flame graph endpoint
  bazında filtrelenebilir. **JVM:** JFR (`jdk.ExecutionSample`, `jdk.ObjectAllocationSample`, `jdk.JavaMonitorEnter`), native
  agent yok. **Node:** `node:inspector` CPU profiler + sampling heap profiler. **.NET:** ayrı paket `TaskTrooper.Telemetry.Profiling`,
  süreç içi EventPipe oturumu (`DiagnosticsClient` kendi PID'ine) + TraceEvent ile parse: SampleProfiler → `cpu`/`wall`,
  `GCAllocationTick` → `alloc_space`, contention → `lock`; Activity → thread eşlemesiyle route etiketi.
  **Python:** `tasktrooper` paketi (ADR-024) — `sys._current_frames()` örnekleyici thread (100 Hz) → `cpu`/`wall`, opsiyonel
  `tracemalloc` → `inuse_space`.
- **OTLP profiles:** gw `POST /v1development/profiles` (OTLP/HTTP protobuf + JSON) ve gRPC `ProfilesService/Export`
  kabul eder (OTel Collector / eBPF profiler'ları doğrudan bağlanır); writer pprof yoluyla aynı tablolara açar.
- Writer profilleri stack satırlarına açar: ClickHouse `profile_stacks` (stack = frame dizisi, root önce; değer) ve
  `profiles` (profil başına özet). Flame graph, top functions ve diff api'de `GROUP BY stack` ile hesaplanır.
- **Diff:** iki pencere/versiyon karşılaştırması pay (değer / toplam) üzerinden — deploy öncesi/sonrası "hangi fonksiyon
  büyüdü" sorusu. Retention 14 gün.
- Kardinalite: frame adı = fonksiyon (dosya/satır yok, Node'da `fn (file)`), stack derinliği ≤ 128, profil başına ≤ 10k benzersiz
  stack (küçükler `[other]`'a birleşir).
**Sonuç:** Thread dump / anlık "thread profiler" yok; sürekli profil zaten aynı soruyu cevaplar. Detay: contracts.md §24.

## ADR-023 — AI / LLM monitoring: OTel GenAI semantic conventions
**Bağlam:** Müşteriler uygulamalarında LLM çağırıyor (OpenAI, Anthropic, Gemini, Bedrock, Azure OpenAI…). Bugün bu çağrılar
sadece `HTTP · api.openai.com` bağımlılığı olarak görünüyor: model, token, maliyet ve prompt yok. New Relic "AI monitoring"
sunuyor. (Not: ADR-018 bizim kendi LLM kotamızdır; bu ADR müşterinin LLM kullanımını izler — UI'da "AI monitoring".)
**Karar:**
- Wire = normal OTLP span'ı, **OTel GenAI semantic conventions** (`gen_ai.provider.name`, `gen_ai.operation.name`,
  `gen_ai.request.model`, `gen_ai.response.model`, `gen_ai.usage.input_tokens|output_tokens`, …). Yeni endpoint yok;
  OTel'in kendi GenAI instrumentation'larından (Spring AI, Microsoft.Extensions.AI, OpenLLMetry, Vercel AI SDK) gelen span'lar
  da aynen çalışır.
- SDK'lar sıfır-kod sarmalayıcı verir: **Node** `instrumentOpenAI(client)` / `instrumentAnthropic(client)` + Vercel AI SDK
  telemetry'si; **Go** `llm.Start(ctx, …)` yardımcıları + OpenAI/Anthropic HTTP `RoundTripper`'ı (stream dahil token'ları
  cevaptan okur); **JVM** Spring AI / LangChain4j span'larını OTel'den alır + Anthropic/OpenAI Java client sarmalayıcısı;
  **.NET** `Microsoft.Extensions.AI` ActivitySource'u varsayılan açık.
- **Prompt / completion içeriği varsayılan toplanmaz** (PII). `captureContent: true` ile açılır; writer içeriği span
  attribute'larından ayırıp `llm_content` tablosuna yazar (alan başına ≤ 32 KB, retention 7 gün), span'da sadece token/meta kalır.
- Writer: GenAI client span'ı bağımlılık kategorisi **`ai`** (`dep = "<provider> · <model>"`) olur → "Time spent" ve RCA'da
  görünür; ayrıca `llm_1m` rollup'ı (provider, model, operation, servis, entry route; çağrı, hata, token'lar, süre digest'i).
- **Maliyet** api'de hesaplanır: model başına fiyat tablosu (1M token başına input/output/cache read) kod içi varsayılan +
  org override (Postgres `llm_prices`). Fiyatlar değişir → UI'da "est." etiketi, fiyat bilinmeyen modelde maliyet boş.
- UI: sidebar'da **AI monitoring** (provider/model/servis/endpoint bazında çağrı, token, maliyet, hata, p95; son çağrılar +
  prompt çekmecesi), servis detayında **AI** sekmesi (LLM çağrısı varsa), trace detayında GenAI span paneli.
**Sonuç:** Python için `tasktrooper` paketi (ADR-024) `instrument_openai` / `instrument_anthropic` verir; OpenLLMetry de çalışır.
LLM alert'leri monitor kind `llm` (contracts §25, alerting.md). Detay: contracts.md §25.

## ADR-024 — Python SDK (`tasktrooper`): OTel dağıtımı + profiling + LLM
**Bağlam:** LLM uygulamalarının çoğu Python. Python SDK roadmap'te "faz 2" idi; AI monitoring ve profiling için gerekli.
**Karar:**
- PyPI paketi `tasktrooper` (`sdks/python`, Python ≥ 3.9): ADR-011'deki gibi ince OTel dağıtımı — `opentelemetry-sdk` +
  OTLP/HTTP protobuf exporter, DSN, resource (`telemetry.distro.name=trooper-python`), ParentBased(TraceIdRatio) +
  `tt=r:` tracestate, `trooper.entry.route/method`, `trooper.caller` (baggage `tt.svc`), `trooper.sample_rate`; config adları
  contracts §2 (`TROOPER_DSN`, …). Framework instrumentation'ı OTel contrib paketlerinden (extras: `tasktrooper[django]`,
  `[fastapi]`, `[flask]`, `[celery]`, `[requests]`, `[sqlalchemy]`…); route template FastAPI/Flask/Django router'ından.
- `tasktrooper.init(...)` asla throw etmez; `tasktrooper-run` launcher + `import tasktrooper.auto`.
- Profiling (§24) ve LLM (`instrument_openai`, `instrument_anthropic`, sync + async + stream; §25) aynı pakette, ek bağımlılık yok.
**Sonuç:** Paket yayını diğer SDK'larla birlikte (hesap gerektirir).

## ADR-025 — MCP sunucusu: `services/mcp`, salt okunur, stdio, api'yi sarar
**Bağlam:** Yerel kodlama agent'ları (OpenCode, Claude Code) telemetriye doğal dilde sorgu sunsun; her ekip
üçüncü parti observability MCP'si yazmasın. API'de 150+ uç var, agent tool seti bunların aynası olamaz.
**Karar:**
- Yeni Go modülü `services/mcp` (`cmd/tt-mcp`), `services/go.work`'e eklenir; resmi Go MCP SDK, **stdio**
  transport (streamable-HTTP uzatma hedefi, v1'de tasarlanmıyor).
- Tool'lar `services/api`'yi (`openapi.yaml`) sarar; ClickHouse/Postgres'e doğrudan bağlanılmaz (contracts §26).
  Tenant (org/project) api key'den gelir; ek olarak `TT_ORG_ID`/`TT_PROJECT_ID` startup'ta `/me` ile doğrulanır.
- v1 **salt okunur**: 12 tool (servis KPI, endpoint, endpoint RCA, trace, log, monitor, incident + timeline,
  load test run, deploy marker). Write tool yok. Her tool input doğrulama, sonuç cap'i (64 KB; kesilirse
  `truncated`/`total`/`hint` işaretleri) ve secret/stack içermeyen hata döner. Detay: `.ai/mcp.md`.
**Sonuç:** Tool seti bilinçli olarak dar; api'ye yeni uç eklenince MCP de senkron büyür. Write tool'ları
v2'de ayrı ADR ile gelir.

## ADR-026 — CI sözleşmesi: Make hedefleri + go.work modül matrisi
**Bağlam:** PR'lerde her Go modülünün derlenmesi/vet + unit testi, web typecheck + build, JS SDK build + vitest
ve workflow lint doğrulanmalı; modül listesi iki yerde tutulamaz.
**Karar:**
- `.github/workflows/ci.yml` modül matrisini `services/go.work`'ten türetir; `make check` aynı kapıları lokal
  koşar — CI sözleşmesi = Makefile hedefleri.
- PR CI unit-only'dir; integration/e2e (`make e2e-ingest`) local'dedir ve ertelenmiştir, CI'ya girmez.
- Imaj build'leri ci.yml'de değil, `.github/workflows/docker.yml`'dedir (release/registry akışı).
**Sonuç:** `go.work`'e modül eklemek CI'ı otomatik genişletir; matrix senkron derdi yok.
