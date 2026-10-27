# Roadmap

## Faz 0 — Temel ✅
- [x] Mimari ve ürün dokümanları
- [x] Design system (token + temel komponentler)
- [x] Dashboard ekranları mock data ile
- [x] `deploy/docker-compose.yml` (ClickHouse, Postgres, Redpanda, Redis) + şemalar (`deploy/clickhouse`, `deploy/postgres`)
- [x] `services/pkg` ortak paketler, `proto/rum/v1`, servisler arası sözleşme (`contracts.md`)

## Faz 1 — Backend APM MVP ✅

Durum: servisler (`ingest`, `api`, `alerting`, `collector`, `integrations` = `trooper-cloud`, `symbolicator`) ve SDK'lar (Go, JVM,
.NET, JS, Android, iOS, Flutter, React Native) hazır; testler ve uçtan uca akış (SDK → gw → Redpanda → writer → ClickHouse → api → UI)
çalışıyor. Dashboard `VITE_DATA=api` ile gerçek veriyi gösterir (ADR-013). `make demo` mock dünyayı gerçek pipeline üzerinden canlı üretir.

- [x] UI `src/data/*` → api (APM, infra, alerting, KPI, load test, entegrasyonlar)
- [x] `consumer_1m.messaging_system`, `broker_topics_1m`, `broker_lag_1m.oldest_age_s`; db metadata (version/zone/instance class/disk)
- [x] KPI headline `metric` + seri, `degradedSince`; tüm monitor tipleri için preview; incident `pending` + `quiet`
- [x] ingest: Vercel drain, GeoIP, tail sampling, Prometheus `/metrics`, RUM backfill (≤ 24 saat) düzeltmesi
- [x] SDK: Android NDK crash + ApplicationExitInfo + R8 upload plugin, iOS tüm thread stack + dSYM script, Flutter plugin, RN `TrooperRum`
  native module, JVM `tt-attempt` + tracestate oranı + `trooper.sdk.errors`
- [x] collector: MongoDB, Kafka topic metadata, MSK IAM / OAUTHBEARER / KIP-848, broker node metrikleri, MySQL prepared statements
- [x] Cloud entegrasyonları (ADR-012): AWS (RDS, ElastiCache, MSK, SQS), GCP (Cloud SQL, Memorystore, Pub/Sub), Neon, Vercel
  (deploy + drain) + Integrations ekranı
- [x] Symbolication: sourcemap, R8/ProGuard, dSYM, NDK, Dart + `trooper-cli`
- [ ] Paket yayınlama (Maven Central, NuGet, npm, pub.dev, CocoaPods) — hesap gerektirir
- [x] Quarkus `tt-attempt` otomatik okuma; `@trooper/node`
- [x] ingest-gw internal servis credential'ı (`TT_INTERNAL_TOKEN`, contracts §2)
- [ ] Gerçek AWS/GCP/Neon/Vercel hesaplarıyla doğrulama (şu an fake sunucularla test edildi)
- [x] Auth (OIDC + dev login, ADR-016), org/proje/üye/API key yönetimi + audit log
- [x] api: 1 saatten kısa range'ler (`15m`, `30m`)

## Faz 2 — RUM + Logs ✅
- [x] `@trooper/browser`, `/v1/rum`, `rum_events` + MV'ler, frontend ekranları gerçek veriye
- [x] Log ingestion + pattern + Logs ekranı
- [x] `@trooper/react-native`
- [x] Vercel log kaynağı (`vercel.source`) Logs ekranında filtre

## Faz 3 — Alerting ✅
- [x] `alert-engine` (threshold, latency, log_match, error_new, anomaly, composite, job_missed, consumer_lag/oldest_age, kpi_burn) +
  `notifier` (Slack, PagerDuty, Opsgenie, e-posta, webhook, Teams); stream kuralları; mute, bakım pencereleri, hatırlatma
- [x] KPI'lar (SLO) + burn-rate alert'leri
- [x] Kök neden analizi (`rca` endpoint, incident `rootCause`)
- [x] Alert-as-code (`.trooper/monitors`, codeintel senkronlar)
- [x] AlertEditor'da anomaly/composite/job_missed/error_new formları
- [x] Slack App: alert mesajı kök neden + confidence ile kanala, thread reply, ack/mute butonları (ADR-017)
- [x] KPI kurallarında hatırlatma; çoklu shard'da stream kuralları

## Faz 4 — Code intelligence ✅
- [x] GitHub App kurulum, repo seçimi, push webhook → incremental analiz (30 sn debounce)
- [x] TS/JS + Go extractor'ları
- [x] LLM zenginleştirme (opsiyonel), kritiklik skoru, alert önerileri, kapsama metriği
- [x] Webhook path'leriyle incremental analiz
- [ ] `last_analyzed_sha` git-diff tabanlı incremental (manuel istek şu an full scan)
- [ ] GitLab / Bitbucket kaynakları

## Faz 5 — Load testing ✅
- [x] Senaryo/run CRUD (api + UI)
- [x] `loadtest-ctl` + `loadtest-runner` (kendi Go motoru, ADR-015)
- [x] Domain doğrulama, limitler
- [x] Envanter/trafikten senaryo üretimi, canlı run ekranı

## Faz 6 — AI kotası, altyapı ve serverless ✅
- [x] AI kullanım kotası: kredi defteri, paket varsayılanları, org + üye limiti, %80/%100 bildirimi, Usage ekranı (ADR-018)
- [x] trooper-agent `host` / `cluster` modları: host, container, process, K8s node/pod/workload, Warning event'leri (ADR-019)
- [x] Helm chart (DaemonSet + Deployment + RBAC) + opsiyonel eBPF (OBI)
- [x] Monitor kind `infra`, RCA'da pod/host sinyalleri
- [x] AWS Lambda keşif + CloudWatch metrikleri, Azure Functions sağlayıcısı, `faas` monitor'ları, maliyet tahmini (ADR-020)
- [x] `@trooper/node` `wrapLambda`
- [x] UI: Hosts, Kubernetes, Serverless ekranları; servis detayında altyapı sekmesi
- [x] K8s cluster modunda list yerine watch (binlerce pod üstü için)
- [x] containerd/Podman host'ları, GPU ve Windows node metrikleri
- [x] Azure: fonksiyon bazında metrik (App Insights), app setting'den servis eşlemesi
- [x] Pod bazında trace/log filtresi, trace detay sayfası; iki Helm chart'ın birleştirilmesi (`services/collector/deploy/helm` eski DB chart'ı)
- [x] Gerçek K8s doğrulaması: kind cluster (host + cluster modu, OOMKilled pod, watch); Lambda keşfi LocalStack ile
- [ ] Gerçek EKS/AKS, Lambda CloudWatch metrikleri ve Azure hesabıyla doğrulama (hesap gerektirir)
- [ ] NVIDIA dışı GPU'lar, cgroup v1 host'larında Docker/Podman'sız container metrikleri, gerçek Windows node testi

## Faz 7 — Session replay, continuous profiling, AI monitoring (New Relic parity) ✅
- [x] Sözleşme: ADR-021/022/023, contracts §23–25, ClickHouse `005_replay_profiles_llm.sql`, Postgres `012_llm_prices.sql`
- [x] ingest: `/v1/replay` → `tt.replay` → S3 + `replay_segments`; `/v1/profiles` → `tt.profiles` → `profile_stacks`/`profiles`;
  GenAI span'ları → `dep_kind=ai`, `llm_1m`, `llm_content` (OpenLLMetry ve Vercel AI SDK adları dahil)
- [x] api: `/replays*`, `/profiles/*`, `/ai/*` + fiyat tablosu; sessions `hasReplay`, error `replay`, trace span `llm`
- [x] SDK: `@trooper/replay` (rrweb), Android/iOS wireframe replay; Go/JVM (JFR)/Node profiling; Node/Go/JVM/.NET LLM span'ları
- [x] UI: Replays sekmesi + oynatıcı, Profiling sekmesi + flame graph/diff, AI monitoring ekranı + servis AI sekmesi
- [x] Demo: replay (web rrweb + mobil wireframe), profil (iki versiyon, diff hikâyeleri), LLM trafiği (Anthropic/OpenAI/Gemini)
- [ ] Gerçek cihazda mobil replay doğrulaması (main thread maliyeti, Compose/SwiftUI çıktısı)

## Faz 7b — Tamamlayıcılar
- [x] Flutter (Dart Element ağacı) ve React Native (native SDK köprüsü) replay
- [x] .NET (`TaskTrooper.Telemetry.Profiling`, EventPipe) ve Python profiling
- [x] Python SDK `tasktrooper` (ADR-024): OTel dağıtımı + profiling + LLM
- [x] LLM alert'leri (monitor kind `llm`, şablonlar, editör), fiyat tablosu `services/pkg/llmprice`
- [x] OTLP profiles sinyali (`/v1development/profiles`, gRPC)
- [x] Web replay: buffer sekmesi `storage` event'i
- [x] Mobil replay emülatör/simulator doğrulaması
