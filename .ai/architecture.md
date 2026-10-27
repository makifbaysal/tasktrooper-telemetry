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
`trooper-agent` (DB/broker, host, container, process, GPU ve Kubernetes metrikleri; OTLP ile ingest'e yollar).

### trooper-agent modları (ADR-019, contracts §20 + §22)

Tek binary, `TT_AGENT_MODES` ile birleştirilebilen üç mod (varsayılan `db` = eski davranış). Her mod kendi OTLP
resource'u ile ayrı bir emitter kullanır; bir kaynak okunamazsa ajan durmaz, `trooper.collector.up=0`
(`kind` = `host|kubelet|containers|k8s`) yazılır.

| Mod | Nerede | Kaynak | Aralık | Çıktı |
|---|---|---|---|---|
| `db` | Helm'de cluster Deployment'ı (`db.enabled`), VM / `docker run` | SQL, Kafka/RabbitMQ API; hedefler statik YAML (`db.targets` → ConfigMap) ve/veya api (`db.fromApi`) | config `interval` | §5 metrikleri |
| `host` | K8s'te Linux DaemonSet (hostPID, `/proc` `/sys` `/` salt-okunur) + opsiyonel Windows DaemonSet (HostProcess), K8s dışında `docker run` / systemd / Windows servisi | gopsutil; container'lar K8s'te kubelet `/stats/summary` + `/pods` (+ `/metrics/cadvisor` throttling), dışında sırayla Docker Engine API (Linux socket, Windows `//./pipe/docker_engine`) → Podman (Docker uyumlu socket) → cgroup v2 ağacı (Linux; containerd/nerdctl/diğerleri); aynı container id'de ilk kaynak kazanır. GPU: `nvidia-smi` varsa | 15 sn | `trooper.host.*`, `trooper.host.gpu.*`, `trooper.container.*`, `trooper.process.*` |
| `cluster` | Tek replika Deployment | K8s API: her kind için `list` + `watch` (resourceVersion, bookmark; 410 Gone → yeniden list; backoff ile yeniden bağlanma), bellekteki durum; client-go yok | 30 sn snapshot | `trooper.k8s.*` + Warning event (watch) → OTLP log (`service.name=k8s`) |

Cluster modu watch akışını düz `net/http` + streaming JSON ile okur; her kind (nodes, pods, replicasets, deployments,
statefulsets, daemonsets, jobs, cronjobs, Warning events) bir `Reflector`'dır. Nodes/pods ilk list'i bitmeden scrape
`up=0` döner; okunamayan (RBAC / API grubu yok) diğer kind'lar sadece kendi metriklerini düşürür (5 dk'da bir yeniden
denenir). GPU metrikleri `gpus_1m` tablosuna gider (writer yönlendirir, `deploy/clickhouse/004_gpu.sql`); `nvidia-smi`
yoksa sessizce atlanır. Windows build'i: `GOOS=windows` (process `fds` yok, `os.type=windows`).

Dağıtım: tek chart `deploy/helm/trooper-agent` — Linux host DaemonSet (`kubernetes.io/os: linux`), cluster Deployment
(`cluster` ve/veya `db`), `windows.enabled` → `kubernetes.io/os: windows` nodeSelector'lı HostProcess DaemonSet
(`services/collector/Dockerfile.windows`), RBAC, Secret, opsiyonel OBI eBPF DaemonSet. Eski
`services/collector/deploy/helm/trooper-agent` chart'ı kaldırıldı.
Sözleşme: contracts.md §20 + §22; detay ve env listesi `services/collector/README.md`. Demo (`make demo`) aynı metrikleri
`demo-eks` cluster'ı ve 4 Lambda fonksiyonu için simüle eder (`--infra-sim`).

## Lokal geliştirme (hedef)

`make dev` → docker compose ile altyapı + servisler `air` ile hot reload + `apps/web` Vite dev server.
Web dashboard şu an mock data ile bağımsız çalışır (`apps/web/src/data/mock.ts`); API hazır olunca `src/data/` altındaki
fonksiyonlar fetch'e çevrilecek — ekranlar veri kaynağını bilmez.

## Ölçek hedefleri (v1)

- Ingest: 200k span/sn, 500k log satırı/sn (tek bölge)
- Dashboard sorguları: p95 < 1 sn (son 24 saat, rollup)
- Alert değerlendirme gecikmesi: < 60 sn (periyodik), < 10 sn (stream)
