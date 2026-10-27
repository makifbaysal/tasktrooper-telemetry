# Code intelligence

Müşterinin kod tabanını okuyup **envanter** çıkaran ve bu envanterden alert + load test önerileri üreten servis
(`services/codeintel`, ADR-014; sözleşme: [contracts.md §13](contracts.md#13-code-intelligence-servicescodeintel-adr-014),
GitHub App: [§18](contracts.md#18-github-app-adr-017)). Kurulum/env: `services/codeintel/README.md`.

## Bağlantı

- **GitHub App** (ADR-017): api install akışı + repo seçici → `repositories(provider='github', installation_id)`.
  codeintel clone için installation token alır (`services/pkg/githubapp`: RS256 JWT, token cache).
- Alternatifler: `credentials_ref` (PAT; `secret:<id>|env:|file:`), public repo (token yok), `provider='local'` +
  `local_path` (dev/demo, yerinde okunur). GitLab/Bitbucket henüz yok (analiz `error` ile biter).
- Monorepo: `.trooper/config.yaml` (`services: [{path, name}]`); yoksa SDK init çağrıları (`TROOPER_SERVICE`,
  `service: "..."` + trooper), sonra modül kökü (go.mod/package.json/pyproject), en son repo adı.
- Tetikleyiciler: ilk bağlantı (full scan), default branch'e push (webhook → incremental), manuel "yeniden analiz"
  (api `analyze_requested_at = now()`; tam tarama için `pending_paths = '{*}'`).

## Pipeline

```
runner (15 sn tick) → due repo (hiç analiz yok | analyze_requested_at > analyzed_at) → debounce (30 sn)
  → Postgres claim (status=analyzing, analysis_started_at; replica'lar arası tek analiz, bayat claim 2×timeout sonra geri alınır)
  → fetch: git clone --depth 1 --single-branch (token sadece GIT_CONFIG_* env'de http.extraHeader; argv/URL/log'da asla)
  → extract (deterministik; Go: go/ast, JS/TS: tokenizer, Python: regex+decorator) → ham envanter
  → incremental ise: sadece değişen dosyaların öğeleri yenilenir, diğerleri DB'den aynen kalır
  → alert-as-code senkronu (.trooper/monitors/*.yaml → monitors source='code')
  → gözlem eşleştirme (ClickHouse red_1m/consumer_1m, read-only) → observed / dead / shadow + istatistik
  → kritiklik: statik kurallar → LLM (cache: aynı kimlik + code_hash) → trafik bump (≥100 rpm medium→high)
  → monitored işareti (aktif/muted monitor'ler) → inventory_items upsert (id'ler kimlikle korunur)
  → alert_suggestions (proposed; item+kind tekil) → load test taslakları (generated/draft)
  → clone silinir (kaynak kod saklanmaz)
```

- Kimlik: `(kind, service, key)`; key formatı contracts §13 (endpoint `METHOD /route/:param`, writer normalizasyonu).
- Timeout `TT_CODEINTEL_ANALYZE_TIMEOUT` (10 dk), panic → `status='error'`; hata sonrası 5 dk backoff (yeni istek gelirse hemen).
- Başarılı analiz: `status='ready'`, `last_analyzed_sha`, `languages`, `services`, `analyzed_at` (analiz sürerken yeni istek
  geldiyse `analyzed_at = başlangıç` → repo due kalır). Hata: `status='error'`, `error`, `pending_paths` geri yazılır.

## GitHub webhook (`POST /v1/webhooks/github`)

- İmza: `X-Hub-Signature-256`, App secret'ı (`TT_GITHUB_WEBHOOK_SECRET`) her event için; repo `webhook_secret_ref`
  sadece o repo'nun `push`'unu yetkilendirir. İmzasız/yanlış → 401.
- Idempotency: `X-GitHub-Delivery` → `codeintel_webhook_deliveries` (7 gün); tekrar → 200 "duplicate"; işleme hatası →
  kayıt silinir (GitHub redelivery işlenir).
- `push` default branch: `analyze_requested_at = now()`, `pending_paths ∪= added+modified+removed`. 20+ commit (GitHub listeyi
  keser), commit'siz push veya >200 dosya → `{*}` (full scan). Branch silme / diğer branch'ler yok sayılır.
- Debounce: son istekten 30 sn geçmeden analiz başlamaz (push patlamaları tek analizde birleşir); patlama 10× debounce'tan
  uzun sürerse yine de koşar. Aynı repo için asla iki analiz aynı anda koşmaz.
- `installation_repositories` removed → repo `status='disconnected'` (runner atlar); added → `pending` + full scan.
- `installation` deleted/suspend → github integration `status='error'` + repo'lar disconnected; unsuspend → geri.

## Extractor'lar (framework bazlı, deterministik)

| Dil | Framework | Ne çıkarılır |
|---|---|---|
| Go | net/http (1.22 pattern'ları dahil), chi (Route/Group/Mount), gin/echo/fiber (group), gorilla/mux (PathPrefix/Subrouter/Methods), gRPC (`*_ServiceDesc`) | route + method + handler dosya:satır, handler kaynağı + çağrılan fonksiyon imzaları |
| Go | sarama, franz-go, kafka-go, asynq | consumer (process = group/queue, source = topic) |
| Go | robfig/cron | cron |
| TS/JS | express, fastify (route objeleri, register prefix), koa, hono, Nest (Controller/Get…/setGlobalPrefix), Next.js (app `route.ts` / pages `api`) | route + method + handler; `use`/`register` mount'ları dosyalar arası |
| TS/JS | kafkajs, bullmq, amqplib, sqs-consumer / ReceiveMessage, Nest EventPattern/Processor | consumer |
| TS/JS | node-cron, `cron` CronJob, Nest `@Cron` | cron |
| TS/JS | react-router (obje + `<Route>`), Next.js page/app router, react-navigation | view |
| Python | FastAPI (APIRouter prefix, include_router), Flask (Blueprint, register_blueprint), Django (path/re_path/include) | route |
| Python | Celery task + beat_schedule (crontab → cron), kafka-python, confluent-kafka | consumer / cron |
| YAML | k8s CronJob (+ `TROOPER_SERVICE` env), helm (regex fallback) | cron |
| Hepsi | HTTP client URL literal'leri, DB/cache/queue driver import'ları | dependency (`<kind> · <name>`) |

Extractor'lar deterministik kalır; LLM **çıkarma** için değil, **yorumlama** için kullanılır. Route parametreleri tek biçime
normalize edilir (`{id}`, `<int:id>`, `[id]`, `:id(\d+)` → `:id`; wildcard → `:wildcard`). Limitler: 50.000 dosya, dosya başına 1 MB,
`node_modules`/`vendor`/`dist`/gizli dizinler atlanır.

## LLM kullanımı

- Sadece `ANTHROPIC_API_KEY` **ve** `TT_CODEINTEL_MODEL` birlikte set ise açık (model ID asla kodda yok); yoksa statik kurallar.
- Resmi Go SDK (`anthropic-sdk-go`), structured output (JSON schema), 15 öğelik batch'ler; repo özeti (dil, servisler, envanter
  satırları) `cache_control` ile cache'lenen sabit system prefix.
- Girdi: öğe + handler kaynağı (≤6000 karakter) + çağrılan fonksiyon imzaları. Çıktı: `criticality`, `businessDomain`, `summary`,
  `failureModes[]`, `suggestedMonitors[]` (şablon + gerekçe), `loadtestHint` (örnek payload, idempotent, not) → `inventory_items.llm`.
- Cache: aynı kimlik + `code_hash` (key + snippet sha256) → önceki sonuç yeniden kullanılır; incremental push'ta değişmeyen
  handler LLM'e gitmez. Analiz başına üst sınır `TT_CODEINTEL_LLM_MAX_ITEMS` (300; kritik olanlar önce). Dependency/view gönderilmez.
- Batch hatası analizi düşürmez (uyarı + statik kritiklik).

## Statik kritiklik kuralları

`critical | high | medium | low`: ödeme/checkout/auth/order gibi kritik kelime + yazma → critical, kritik + okuma → high, yazma → medium,
okuma/health → low; `ANY` method'da DB yazma ifadesi yazma sayılır; consumer DLQ/kritik → high, diğer → medium; cron kritik → high;
ödeme sağlayıcı bağımlılıkları → high, DB/queue → medium. Trafik ≥100 rpm → medium → high.

## Çıktılar

- **Envanter** (`inventory_items`, Code insights ekranı): öğe, servis, dosya:satır, kritiklik, `observed`, `monitored`, `dead`
  (kodda var, 30 gün trafik yok; servis hiç enstrümante değilse işaretlenmez), `shadow` (trafik var, kodda yok; `repository_id` NULL),
  `summary`, `llm`, `meta.stats` (7 gün count/errors/p95/rpm/errorRate).
- **Alert önerileri** (`alert_suggestions`, `state='proposed'`, `(inventory_item_id, kind)` tekil; accepted/rejected satırlara
  dokunulmaz): critical/high, monitor'ü olmayan, ölü/gölge olmayan öğeler için. Endpoint → error rate (`metric_threshold`,
  eşik ≥500 istek varsa gözlemden kalibre: max(2×oran, %1), üst sınır %50), critical ise ayrıca latency (p95×1,5) ve error_new;
  consumer → tek uygun broker cluster varsa `consumer_lag`, yoksa `job_missed` ("consumer stalled"); cron → `job_missed`
  (aralık cron ifadesinden). `monitor_draft` = api `POST /monitors` gövdesi. Detay: [alerting.md](alerting.md#kod-farkındalıklı-otomatik-alert).
- **Load test taslakları** (`loadtest_scenarios`, `source='generated'`, `state='draft'`, servis başına "<svc> · critical paths
  (generated)"): critical+high HTTP endpoint'leri, ağırlıklar üretim trafiğinden, seviye 1×/2×/3× üretim RPS (trafik yoksa VU),
  LLM payload örneği, idempotent olmayanlara not, thresholds `{p95Ms, errorRate}`. `ready`'e alınmış taslak güncellenmez;
  artık üretilmeyen ve hiç koşmamış taslaklar silinir. Detay: [load-testing.md](load-testing.md#senaryo-üretimi).
- **Alert-as-code**: `.trooper/monitors/*.yaml` (alerting.md § Kural şeması; tek doküman, liste, `monitors:` veya `---`) →
  `monitors(source='code', code_ref='<repo>:<path>')`, anahtar code_ref + name; dosyadan silinen monitor silinir, parse hatalı
  dosyanın monitor'leri korunur (uyarı). api bu satırları read-only gösterir (409 "managed in repository").
