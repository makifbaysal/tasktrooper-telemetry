# AGENTS.md — TaskTrooper Telemetry

Bu dosya projede çalışan tüm AI agent'ları (Claude Code, Cursor, Codex vb.) ve geliştiriciler için giriş noktasıdır.
Kod yazmadan önce ilgili `.ai/` dokümanını oku. Dokümanla çelişen bir karar alırsan, aynı PR'da dokümanı da güncelle.

## Proje özeti

TaskTrooper Telemetry (TaskTrooper ürün ailesinin parçası; repo `Task Trooper/tasktrooper-telemetry`); backend servislerine, worker/consumer'lara, web ve mobil uygulamalara eklenen **SDK'lar** üzerinden
veri toplayan, bu veriyi görselleştiren, üzerine **alert** kurduran ve **load test** koşturabilen bir observability platformudur.

Ayırt edici özellik: platform, müşterinin **kod tabanını okur** (repo bağlantısı), endpoint/consumer/sayfa envanterini çıkarır
ve bu envantere göre **alert'leri otomatik önerir/tanımlar**, load test senaryolarını otomatik üretir.

Dört ana yetenek:
1. **APM (backend):** endpoint bazlı throughput, error rate, latency (p50/p95/p99), trace, bağımlılık haritası; queue consumer metrikleri (lag, işlem süresi, retry/DLQ).
2. **RUM (frontend/mobil):** sayfa/ekran görüntüleme, kim geldi (anonim/identified user), oturum süresi, yükleme süreleri (Web Vitals, TTFB, app start), JS/crash hataları, network çağrıları.
3. **Logs & Alerting:** log ingestion + arama; metrik, log pattern ve anomali tabanlı alert'ler; kod-farkındalıklı otomatik alert önerileri.
4. **Load testing:** platform içinden senaryo tanımlama/koşturma, sonuçları aynı telemetri ile korelasyon.

## Doküman haritası (`.ai/`)

| Dosya | Ne zaman oku |
|---|---|
| [`.ai/README.md`](.ai/README.md) | Doküman indeksi, terimler sözlüğü |
| [`.ai/product.md`](.ai/product.md) | Persona, use-case, ekran listesi, kapsam |
| [`.ai/architecture.md`](.ai/architecture.md) | Sistem mimarisi, servisler, veri akışı, monorepo yapısı |
| [`.ai/sdk.md`](.ai/sdk.md) | SDK tasarımı, API yüzeyi, wire protokolü (OTLP) |
| [`.ai/data-model.md`](.ai/data-model.md) | ClickHouse/Postgres şemaları, retention, kardinalite kuralları |
| [`.ai/alerting.md`](.ai/alerting.md) | Alert motoru, kural tipleri, kod analizi ile otomatik alert |
| [`.ai/code-intelligence.md`](.ai/code-intelligence.md) | Repo okuma, envanter çıkarma, LLM kullanımı |
| [`.ai/load-testing.md`](.ai/load-testing.md) | Load test altyapısı, runner'lar, senaryo formatı |
| [`.ai/design-system.md`](.ai/design-system.md) | UI token'ları, komponentler, grafik kuralları |
| [`.ai/contracts.md`](.ai/contracts.md) | Servisler/SDK'lar arası sözleşme: portlar, auth, Kafka topic'leri, `trooper.*` attribute'ları, collector metrikleri |
| [`.ai/decisions.md`](.ai/decisions.md) | Mimari karar kayıtları (ADR) |
| [`.ai/roadmap.md`](.ai/roadmap.md) | Fazlar ve sıradaki işler |

## Monorepo yapısı

```
apps/web/               Dashboard (React 19 + Vite + TypeScript)
services/               Go servisleri: pkg, ingest (gw + writer), api, alerting (engine + notifier), collector, integrations (trooper-cloud), symbolicator, codeintel, loadtest (ctl + runner)
sdks/                   go, jvm, dotnet, js/packages (browser, react, vue, angular, react-native, node), android, ios, flutter
tools/                  demo (canlı demo dünyası, `make demo`), trooper-cli (sourcemap/dSYM/mapping upload)
proto/                  RUM wire şeması
deploy/                 docker-compose + ClickHouse/Postgres şemaları
.ai/                    Mimari ve ürün dokümanları (servisler arası sözleşme: .ai/contracts.md)
```

Detay: [`.ai/architecture.md`](.ai/architecture.md#monorepo).

## Stack

- **Frontend:** React 19, Vite, TypeScript (strict), react-router, plain CSS + design token'ları (CSS custom properties). Grafikler: kendi SVG chart komponentlerimiz (`apps/web/src/components/charts`).
- **Backend:** Go 1.26. HTTP için `net/http` + chi, gRPC/OTLP için `go.opentelemetry.io/collector` bileşenleri.
- **Veri:** ClickHouse (telemetri: metrik, span, log, RUM event), PostgreSQL (metadata: org, proje, alert, kullanıcı), Redpanda (Kafka API, ingest buffer), Redis (rate limit, cache).
- **Load test motoru:** kendi Go motorumuz (ADR-015), runner'lar local process veya Kubernetes Job.
- **LLM:** Claude API (kod analizi ve alert önerisi). Model ID'leri sabit kodlanmaz, config'ten gelir.
- **Paket yöneticisi:** pnpm (JS), Go modules.

## Komutlar

```bash
# Web dashboard
cd apps/web
pnpm install
pnpm dev          # http://localhost:5173
pnpm build        # typecheck + production build
pnpm typecheck
```

Backend: `docker compose -f deploy/docker-compose.yml up -d` (altyapı), `make migrate`, `make build`, `make test`. Detay: `services/*/README.md`, `sdks/*/README.md`.

## Çalışma kuralları

1. **Önce doküman.** Yeni servis, tablo, SDK API'si veya alert tipi → önce ilgili `.ai/*.md` güncellenir, sonra kod.
2. **Design system dışına çıkma.** Tema TaskTrooper'ı izler (bkz. design-system.md § Tema kaynağı). UI'da ham hex, ham px spacing, inline font yazma; `apps/web/src/styles/tokens.css` token'larını kullan. Yeni komponent → `apps/web/src/components/ui/` altına, design-system.md'ye ekle.
3. **Grafik kuralları:** tek y-ekseni (dual axis yasak), kategorik renkler sabit sırada (`--series-1..8`), status renkleri (`--status-*`) sadece durum için ve her zaman ikon + etiketle. Detay: design-system.md § Data viz.
4. **Kardinalite disiplini.** Metrik label'larına user id, request id, ham URL koyma. Endpoint'ler route template olarak (`/users/:id`) normalize edilir. Bkz. data-model.md.
5. **SDK'lar host uygulamayı asla çökertmez.** Tüm SDK hataları yutulur ve internal metriğe yazılır; ağ çağrıları async + batch + backoff.
6. **PII:** SDK default olarak header/body/query param toplamaz; maskeleme kuralları SDK tarafında uygulanır.
7. **Multi-tenancy:** Her sorgu `org_id` + `project_id` ile filtrelenir. Query servisinde tenant filtresi olmayan sorgu yazılmaz.
8. **Testler:** Go → table-driven test; web → komponent mantığı için vitest (eklenecek). Ingest yolu için benchmark zorunlu.
9. **Commit:** Conventional Commits (`feat(web): ...`, `fix(ingest): ...`).
10. **Dil:** Kod, identifier, commit İngilizce. `.ai/` dokümanları Türkçe (teknik terimler İngilizce).

## Mevcut durum

- [x] Mimari dokümanlar (`.ai/`)
- [x] Design system token'ları + temel komponentler (`apps/web`)
- [x] İlk ekranlar (mock data): Overview, Services (web/mobil app'ler dahil), Service/Endpoint detail (KPI'lar, alert'ler ve loglar servis/endpoint altında), KPI detail, Load tests, Code insights, Infrastructure (Databases, Message queues — Kafka/RabbitMQ lag)
- [x] KPI (SLO) + burn-rate alert'leri ve kök neden analizi UI'ı (mock, `src/lib/rca.ts`)
- [x] Altyapı (compose), ClickHouse/Postgres şemaları, RUM proto, sözleşme (`contracts.md`)
- [x] Backend servisleri: ingest (gw + writer), api (90 endpoint, `services/api/openapi.yaml`), alerting (engine + notifier), collector (`trooper-agent`)
- [x] SDK'lar: Go, JVM (agent, Spring Boot, Quarkus, Ktor), .NET, JS (browser/react/vue/angular/RN/node), Android, iOS, Flutter
- [x] UI'ın `src/data/*` fonksiyonları api'ye bağlı (`VITE_DATA=api`; default mock) — ADR-013
- [x] Cloud entegrasyonları: AWS, Google Cloud, Neon, Vercel (`services/integrations`, Integrations ekranı) — ADR-012
- [x] Symbolication servisi + `trooper-cli`; canlı demo (`make demo`)
- [x] OIDC auth + takım yönetimi (ADR-016), code intelligence (Faz 4, ADR-014), load test runner (Faz 5, ADR-015)
- [x] Slack App (alert + kök neden/confidence, ack/mute) ve GitHub App (repo ekleme, push webhook → incremental analiz) — ADR-017
- [x] AI kullanım kotası (ADR-018), host/K8s/container izleme + opsiyonel eBPF (ADR-019), AWS Lambda + Azure Functions (ADR-020)
- [x] Session replay (web rrweb + Android/iOS wireframe), continuous profiling (Go/JVM/Node, flame graph + diff), AI/LLM monitoring (GenAI span'ları, token/maliyet) — ADR-021/022/023
- [ ] Paket yayınlama

Sıradaki işler: [`.ai/roadmap.md`](.ai/roadmap.md).
