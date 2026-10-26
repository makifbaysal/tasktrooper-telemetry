# Mimari

## Genel görünüm

```
 ┌──────────── Müşteri tarafı ────────────┐
 │ Backend SDK  Worker SDK  Browser SDK    │
 │ (node/go/py) (consumer)  Mobile SDK     │
 └──────┬──────────┬───────────┬───────────┘
        │ OTLP/HTTP (protobuf, gzip) + RUM JSON beacon
        ▼
 ┌──────────────┐   ┌───────────┐   ┌──────────────────┐
 │ ingest-gw    │──▶│ Redpanda  │──▶│ ingest-writer    │──▶ ClickHouse
 │ (auth, rate  │   │ topics:   │   │ (batch insert,   │     (metrics, spans,
 │ limit, valid)│   │ spans,    │   │  RED rollup,     │      logs, rum_events)
 └──────────────┘   │ logs,rum, │   │  route normalize)│
                    │ metrics   │   └──────────────────┘
                    └─────┬─────┘
                          │ (stream tüketimi)
                          ▼
                   ┌──────────────┐      ┌──────────────┐
                   │ alert-engine │─────▶│ notifier     │──▶ Slack, PagerDuty,
                   │ (eval loop + │      │ (dedupe,     │    e-posta, webhook
                   │  stream rule)│      │  routing)    │
                   └──────┬───────┘      └──────────────┘
                          │
 ┌──────────┐   ┌─────────┴────┐   ┌──────────────┐   ┌──────────────────┐
 │ apps/web │──▶│ api (query + │──▶│ PostgreSQL   │   │ codeintel        │
 │ dashboard│   │ control)     │   │ (metadata)   │◀──│ (repo sync, AST, │
 └──────────┘   └──────┬───────┘   └──────────────┘   │  LLM, öneri)     │
                       │                              └──────────────────┘
                       ▼
                ┌──────────────┐    ┌─────────────────┐
                │ loadtest-ctl │───▶│ k6 runner'lar    │──▶ müşteri hedefi
                │ (orkestrasyon│    │ (K8s Job, bölge) │
                └──────────────┘    └─────────────────┘
```

## Servisler

| Servis | Dil | Sorumluluk | Ölçek |
|---|---|---|---|
| `ingest-gw` | Go | OTLP/HTTP + gRPC, RUM beacon; API key doğrulama, rate limit (Redis), şema validasyonu, Redpanda'ya yazma. Stateless. | Yatay, en yüksek trafik |
| `ingest-writer` | Go | Redpanda tüketir, ClickHouse'a batch insert (≥10k satır veya 1 sn). Route normalizasyonu, error fingerprint, geo-ip, user-agent parse. | Partition sayısı kadar |
| `api` | Go | Dashboard'un tek backend'i. Query (ClickHouse), control plane CRUD (Postgres), auth (OIDC + session), tenant izolasyonu. | Yatay |
| `alert-engine` | Go | Monitor'leri periyodik değerlendirir (ClickHouse sorgusu) + düşük gecikmeli kurallar için stream değerlendirme. Incident state machine. KPI burn-rate değerlendirmesi ve `rca` paketi (çağrı ağacında delta attribution ile kök neden; bkz. alerting.md § Kök neden analizi). | Leader election + shard (monitor id hash) |
| `notifier` | Go | Bildirim kanalları, dedupe, gruplama, escalation. | Küçük |
| `codeintel` | Go + tree-sitter | Repo sync (GitHub/GitLab App), AST ile envanter çıkarma, LLM ile zenginleştirme, alert/senaryo önerisi. | Job tabanlı kuyruk |
| `loadtest-ctl` | Go | Senaryo saklama, run orkestrasyonu, runner'lara iş dağıtma, sonuç toplama. | Küçük |
| `loadtest-runner` | Go (k6 embed) | Senaryoyu koşar, metrikleri OTLP olarak kendi ingest'imize yollar. | K8s Job, çok bölge |

Tüm servisler kendi SDK'mız ile enstrümante edilir (dogfooding).

## Veri akışı kararları

- **Wire protokolü OTLP.** SDK'lar OpenTelemetry üzerine ince katman. Müşteri mevcut OTel collector'ını da bize yönlendirebilir. RUM için ek olarak hafif JSON beacon (`navigator.sendBeacon`) — OTel JS browser paketleri ağır.
- **Buffer olarak Redpanda.** ClickHouse yavaşlarsa ingest düşmez; replay mümkün. Topic başına retention 24 saat.
- **Ön-agregasyon.** `ingest-writer` span'lardan dakikalık RED rollup üretir (`red_1m`), ClickHouse materialized view ile saatliğe (`red_1h`) iner. Dashboard'lar rollup'tan okur, ham span sadece trace/drill-down için.
- **Sampling.** Head sampling SDK'da (default %100, ayarlanabilir); hata ve yavaş istekler her zaman tutulur (tail sampling ingest-writer'da). RED metrikleri sampling'den **önce** sayılır, bu yüzden doğruluk kaybolmaz.

## Multi-tenancy & güvenlik

- Her satırda `org_id`, `project_id`. ClickHouse'da partition key'in parçası değil, ORDER BY'ın başında.
- API key tipleri: `ingest` (sadece yazma, browser'da açık olabilir), `api` (okuma/yazma, gizli).
- Browser ingest key'i domain allowlist ile korunur.
- Repo erişimi GitHub App ile, minimum yetki (contents:read, metadata:read, pull_requests:write — alert-as-code PR için).
- Kaynak kod kalıcı saklanmaz: analiz sonrası sadece envanter + özetler saklanır.

## Monorepo

Güncel yerleşim ve modül sahipliği: [`contracts.md` § 1](contracts.md#1-repo-yerleşimi-ve-sahiplik). Özet:

```
apps/web/                         React dashboard
services/                         Go 1.26, go.work
  pkg/  ingest/  api/  alerting/  collector/      (codeintel, loadtest sonra)
sdks/
  go/  jvm/  dotnet/  js/packages/  android/  ios/  flutter/
proto/rum/v1/                     RUM wire şeması
deploy/
  docker-compose.yml  clickhouse/*.sql  postgres/*.sql
.ai/                              dokümanlar
```

Servis ↔ binary: `ingest` → `ingest-gw`, `ingest-writer`; `alerting` → `alert-engine`, `notifier`; `collector` →
`trooper-agent` (DB + broker metrikleri, OTLP ile ingest'e yollar).

## Lokal geliştirme (hedef)

`make dev` → docker compose ile altyapı + servisler `air` ile hot reload + `apps/web` Vite dev server.
Web dashboard şu an mock data ile bağımsız çalışır (`apps/web/src/data/mock.ts`); API hazır olunca `src/data/` altındaki
fonksiyonlar fetch'e çevrilecek — ekranlar veri kaynağını bilmez.

## Ölçek hedefleri (v1)

- Ingest: 200k span/sn, 500k log satırı/sn (tek bölge)
- Dashboard sorguları: p95 < 1 sn (son 24 saat, rollup)
- Alert değerlendirme gecikmesi: < 60 sn (periyodik), < 10 sn (stream)
