# Code intelligence

Müşterinin kod tabanını okuyup **envanter** çıkaran ve bu envanterden alert + load test önerileri üreten servis
(`services/codeintel`, ADR-014; sözleşme: [contracts.md §13](contracts.md#13-code-intelligence-servicescodeintel-adr-014),
GitHub App: [§18](contracts.md#18-github-app-adr-017), GitLab/Bitbucket: [§18a](contracts.md#18a-gitlab-ve-bitbucket-kaynakları)).
Kurulum/env: `services/codeintel/README.md`.

## Bağlantı

- **GitHub App** (ADR-017): api install akışı + repo seçici → `repositories(provider='github', installation_id)`.
  codeintel clone için installation token alır (`services/pkg/githubapp`: RS256 JWT, token cache).
- Alternatifler: `credentials_ref` (PAT; `secret:<id>|env:|file:`), public repo (token yok), `provider='local'` +
  `local_path` (dev/demo, yerinde okunur).
- **GitLab** (`provider='gitlab'`): gitlab.com veya self-managed. `full_name` = `group/subgroup/project` (2+ segment),
  self-managed için `repositories.base_url` (`https://gitlab.example.com[/yol]`; sadece https, userinfo/query/fragment yok;
  NULL → `https://gitlab.com`). Token: personal / project / group access token (`read_repository` scope'u yeter) →
  `credentials_ref`; git'e `Authorization: Basic base64("oauth2:<token>")` olarak gider.
- **Bitbucket Cloud** (`provider='bitbucket'`): `full_name` = `workspace/repo` (tam 2 segment), host sabit
  `https://bitbucket.org` (`base_url` kabul edilmez). Token: repository/project/workspace access token →
  `Basic base64("x-token-auth:<token>")`; app password / API token `kullanıcı:parola` biçiminde girilir →
  `Basic base64(kullanıcı:parola)`. Bitbucket Server / Data Center desteklenmez.
- Ortak kurallar `services/pkg/vcs`'te (api + codeintel): full name biçimi, base URL doğrulaması, clone URL'si, auth header'ı.
  Tüm sağlayıcılarda token sadece `GIT_CONFIG_*` env'inde (`http.extraHeader`); argv/URL/log/`repositories.error`'da asla
  (hata mesajında ham token, parola ve base64 biçimi `***` olur). Token yoksa public clone.
- Webhook secret'ı: GitLab, Bitbucket ve App'siz GitHub repo'larında api bağlantıda rastgele secret üretir, `secrets`'a
  mühürler (`webhook_secret_ref`) ve webhook URL'si ile **bir kez** döner (`POST /repos/{id}/webhook-secret` yeniler).
  İlk doğrulanmış teslim `repositories.webhook_received_at`'i yazar → `webhookActive` (UI "webhook aktif").
- Monorepo: `.trooper/config.yaml` (`services: [{path, name}]`); yoksa SDK init çağrıları (`TROOPER_SERVICE`,
  `service: "..."` + trooper), sonra modül kökü (go.mod/package.json/pyproject), en son repo adı.
- Tetikleyiciler: ilk bağlantı (full scan), default branch'e push (webhook dosya listesi → incremental), manuel "yeniden
  analiz" (api `analyze_requested_at = now()`, `pending_paths = '{*}'`). `{*}` artık "liste bilinmiyor" demektir: daha önce
  analiz edilmiş repo'da değişen dosyalar `git diff last_analyzed_sha..HEAD` ile bulunur → yine incremental (bkz.
  [Incremental analiz](#incremental-analiz)). Zorla full scan: `codeintel -once <id>` (ya da `last_analyzed_sha = NULL`).

## Pipeline

```
runner (15 sn tick) → due repo (hiç analiz yok | analyze_requested_at > analyzed_at) → debounce (30 sn)
  → Postgres claim (status=analyzing, analysis_started_at; replica'lar arası tek analiz, bayat claim 2×timeout sonra geri alınır)
  → fetch: git clone --depth 1 --single-branch (token sadece GIT_CONFIG_* env'de http.extraHeader; argv/URL/log'da asla)
  → değişen dosyalar: webhook listesi | yoksa git diff last_analyzed_sha..HEAD | yoksa full scan (aşağıda)
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

### Incremental analiz

Değişen dosya kümesi (`analyze.changedFiles`) şu sırayla belirlenir:

1. Repo hiç analiz edilmemiş (`analyzed_at` / `last_analyzed_sha` yok) → **full scan**.
2. `pending_paths` kullanılabilir bir liste (`*` yok, ≤ 200 dosya) → webhook listesi (`diff_source=webhook`).
3. Aksi halde (manuel istek `{*}`, webhook taşması `{*}`, boş liste) → **git diff** (`diff_source=git`,
   `source.Fetcher.ChangedSince`): shallow clone'a eski commit `git fetch --depth=1 --no-tags origin <last_analyzed_sha>`
   ile çekilir (clone'la aynı `GIT_CONFIG_*` kimlik bilgisi; GitHub/GitLab/Bitbucket protocol v2'de commit id ile fetch'e
   izin verir), sonra `git diff --name-status -z --no-renames <last_sha> HEAD`. Diff ağaçtan ağaca olduğu için force push
   sonrası da doğrudur (eski commit sunucuda duruyorsa). Rename = eski + yeni yol; silinen dosyaların öğeleri
   webhook yolundaki gibi envanterden düşer (değişen dosya, extract'ta artık öğe vermez).
4. HEAD == `last_analyzed_sha` → sıfır dosyalı incremental: extract öğe yenilemez ama alert-as-code, trafik eşleştirme,
   monitored işareti, öneriler ve load test taslakları yine yenilenir.

Full scan'e düşüş (sebep `full scan` log'unda `reason` olarak yazılır):

- eski commit alınamıyor (`source.ErrNoBase`: geçmiş yeniden yazılmış + GC, geçersiz sha, fetch/auth hatası);
- git diff 200 dosyadan (`store.MaxPendingPaths`) fazla; başka git hatası;
- servis eşlemesini değiştirebilecek değişiklik: `.trooper/config.yaml|yml` (her iki kaynakta), git diff'te eklenen/silinen
  modül manifest'i (`go.mod`, `package.json`, `pyproject.toml`, `requirements.txt`, `setup.py`) — değişmeyen dosyaların
  öğeleri DB'deki servisle kalacağı için;
- `provider='local'`: yerinde okunur, commit edilmemiş değişiklikler de dahil → commit diff'i önceki analizin gördüğünü
  tarif etmez; her `{*}` full scan.

Not: extractor'lar geliştiğinde değişmeyen dosyalar incremental analizde yeniden çıkarılmaz; tüm envanteri yenilemek için
full scan zorlanmalı (yukarıda).

## GitHub webhook (`POST /v1/webhooks/github`)

- İmza: `X-Hub-Signature-256`, App secret'ı (`TT_GITHUB_WEBHOOK_SECRET`) her event için; repo `webhook_secret_ref`
  sadece o repo'nun `push`'unu yetkilendirir. İmzasız/yanlış → 401.
- Idempotency: `X-GitHub-Delivery` → `codeintel_webhook_deliveries` (7 gün); tekrar → 200 "duplicate"; işleme hatası →
  kayıt silinir (GitHub redelivery işlenir).
- `push` default branch: `analyze_requested_at = now()`, `pending_paths ∪= added+modified+removed`. 20+ commit (GitHub listeyi
  keser), commit'siz push veya >200 dosya → `{*}` (git diff, olmazsa full scan). Branch silme / diğer branch'ler yok sayılır.
- Debounce: son istekten 30 sn geçmeden analiz başlamaz (push patlamaları tek analizde birleşir); patlama 10× debounce'tan
  uzun sürerse yine de koşar. Aynı repo için asla iki analiz aynı anda koşmaz.
- `installation_repositories` removed → repo `status='disconnected'` (runner atlar); added → `pending` + full scan.
- `installation` deleted/suspend → github integration `status='error'` + repo'lar disconnected; unsuspend → geri.

## GitLab / Bitbucket webhook (`POST /v1/webhooks/gitlab`, `POST /v1/webhooks/bitbucket`)

Repo başına webhook (App yok), codeintel'in public adresinde; api URL'yi `TT_PUBLIC_WEBHOOK_URL` tabanıyla verir. Aday
repo'lar `provider` + `lower(full_name)` ile bulunur (tüm projeler); sadece secret'ı tutan repo'lar etkilenir, hiçbiri
tutmazsa 401.

| | GitLab | Bitbucket Cloud |
|---|---|---|
| Doğrulama | `X-Gitlab-Token` = repo'nun webhook secret'ı (sabit zamanlı karşılaştırma) | `X-Hub-Signature: sha256=<hex>` = HMAC-SHA256(gövde, secret) |
| Event | `X-Gitlab-Event: Push Hook`; diğerleri 200 "ignored" | `X-Event-Key: repo:push`; diğerleri 200 "ignored" |
| Repo | `project.path_with_namespace` | `repository.full_name` |
| Idempotency | `Idempotency-Key`, yoksa `X-Gitlab-Event-UUID` (`X-Gitlab-Webhook-UUID` ile birlikte) | `X-Request-UUID` (`X-Hook-UUID` ile birlikte) |
| Değişen dosyalar | `commits[].added/modified/removed`; GitLab en çok 20 commit listeler → `total_commits_count > len(commits)`, commit'siz push veya >200 dosya → `{*}` | push payload'ı dosya listelemez → her zaman `{*}` |
| Branch silme | `after` sıfır / `checkout_sha` null → yok sayılır | `changes[].new == null` → yok sayılır |

- Default branch push'u (repo'nun `default_branch`'i; boşsa payload'daki): `analyze_requested_at = now()`,
  `analyze_requested_by = NULL`, `pending_paths ∪= dosyalar`; diğer branch'ler "ignored". `{*}` incremental pipeline'da
  `last_analyzed_sha` diff'ine çevrilir (bkz. Pipeline).
- Teslim id'leri `codeintel_webhook_deliveries`'e sağlayıcı ön ekiyle yazılır (`gitlab:<hook>:<id>`, `bitbucket:<hook>:<id>`;
  GitHub id'leriyle ve aynı repo'yu bağlamış başka projelerin hook'larıyla çakışmaz). Tekrar → 200 "duplicate"; işleme
  hatası → kayıt silinir (sağlayıcının yeniden denemesi işlenir). Gövde sınırı 25 MB. Id header'ı yoksa idempotency yok.
- Doğrulanmış her teslim (diğer branch dahil) `webhook_received_at = now()` yazar.
- Metrik: `codeintel_webhooks_total{event="gitlab:push"|"gitlab:other"|"bitbucket:repo:push"|"bitbucket:other", result}` (event etiketi sınırlı; doğrulanmamış header değerleri kardinalite üretmez).

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

### AI kotası (ADR-018, contracts §19)

- Her LLM batch'inden **önce** `services/pkg/aiquota` `Reserve` (org başına `pg_advisory_xact_lock(7140, org_id)`, kısa transaction):
  tahmin = `ceil(promptKarakter/4) × in + min(16000, 600 × öğe) × out` kredi (çıktı tahmini öğe başına 600 token; `max_tokens`
  ile tahmin limitin çok altında reddederdi). `used + tahmin > limit` → red. Çağrıdan **sonra** gerçek token'la `ai_usage` satırı
  (`feature=codeintel.enrich`, `model` = `TT_CODEINTEL_MODEL`, `ref=repo:<id>`, `input_tokens` cache okuma/yazma dahil; kredi
  cache yazma 1,25×, cache okuma 0,1× input fiyatıyla). Cevap gelip kullanılamasa da (decode hatası, max_tokens) kayıt yazılır.
- Kullanıcı = `repositories.analyze_requested_by` (api manuel analizde doldurur). Webhook (push, installation) ve diğer
  otomatik istekler bu kolonu `NULL`'lar → sadece org limiti uygulanır; üye tavanı (`memberships.ai_monthly_credits`) sadece
  üyenin başlattığı analizde.
- Red → kalan öğeler LLM'siz (statik kritiklik, önceki cache sonuçları korunur), analiz başarılı biter; red analiz boyunca
  kalıcıdır (tekrar sorulmaz). Kota kontrolü hata verirse (DB) fail-closed: LLM atlanır, `reason: check_failed`.
- Analiz özeti `repositories.analysis` (jsonb, `FinishOK`): `{sha, incremental, items, llmItems, llm: {status: ok|disabled|quota_exceeded,
  reason?: org_limit|user_limit|check_failed, credits, skipped?}}`. `disabled` = LLM yapılandırılmamış; `credits` bu analizin
  harcadığı kredi (4 ondalık). api bunu `inventory.analysis.llm` olarak geçirir.
- Fiyatlar `TT_AI_PRICES` (`<model>=<in>/<out>`, USD / milyon token; tam id, yoksa en uzun prefix), `TT_AI_DEFAULT_PRICE` (`3/15`),
  paketler `TT_AI_PLAN_CREDITS` (`free=100,team=5000,enterprise=-1`).
- Öneri üretimi (`suggest`) LLM çağırmaz; `codeintel.suggest` feature'ı ileride LLM'li öneri için ayrılmıştır.

## Statik kritiklik kuralları

`critical | high | medium | low`: ödeme/checkout/auth/order gibi kritik kelime + yazma → critical, kritik + okuma → high, yazma → medium,
okuma/health → low; `ANY` method'da DB yazma ifadesi yazma sayılır; consumer DLQ/kritik → high, diğer → medium; cron kritik → high;
ödeme sağlayıcı bağımlılıkları → high, DB/queue → medium. Trafik ≥100 rpm → medium → high.

## Çıktılar

Yerel geliştirme kopyasında Code insights çekmecesi envanter zenginleştirme, monitor önerisi ve yük testi taslağı için `/local-ai/*` mock uç noktalarını çağırır. Bu çağrılar `services/api/cmd/ai-playground` ile sunulur, kredi harcamaz ve gerçek codeintel analizinden bağımsızdır. Üretim akışındaki codeintel kotası yukarıdaki `aiquota` kurallarıyla yönetilir.

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
