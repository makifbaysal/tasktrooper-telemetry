# Load testing

## Hedef

Platform içinden load test tanımla, koştur, sonucu **aynı ekranda** hedef servisin gerçek telemetrisi (CPU, p95, error rate, DB span'ları) ile gör.

## Bileşenler (ADR-015, contracts §14 — `services/loadtest`)

- **`loadtest-ctl`** (:8097, `/healthz`, `/metrics`): api'nin `queued` bıraktığı run'ları `FOR UPDATE SKIP LOCKED` ile alır,
  kabul kontrolü yapar (doğrulanmış domain, plan limitleri, aylık VU-dakika), bölge başına bir runner başlatır, runner'ların
  shard satırlarını birleştirip `loadtest_runs.live` / `summary`'ye yazar, abort'u ve yetim run'ları yönetir, domain doğrulamasını yapar.
  Senaryo CRUD'u ve run kuyruğa alma api'dedir.
- **`loadtest-runner`**: kendi Go yük motorumuz (k6 gömülü **değil**, ADR-015). Bir run'ın bir bölge shard'ını koşturur,
  her tick'te (5 sn) `loadtest_run_shards` satırını günceller ve OTLP metrik yollar. Script senaryolarında PATH'teki `k6`'yı çağırır.
- **Executor**: `TT_LOADTEST_EXECUTOR=local` → alt süreç; `k8s` → bölge başına `batch/v1` Job (client-go yok, in-cluster SA token
  ile düz REST; `TT_LOADTEST_K8S_REGION_LABEL` ile nodeSelector, secret'lar `envFrom` ile).
- **Korelasyon**: runner her isteğe yeni `traceparent`, `baggage: tt.svc=loadtest:<run_id>`, `x-trooper-loadtest-run: <run_id>`,
  `user-agent: TaskTrooper-LoadTest/1` ekler → hedef serviste SDK'lar bunu `trooper.caller` olarak yazar; UI test trafiğini ayırır.

## Akış

```
api  POST /loadtests/scenarios/{id}/runs → loadtest_runs(state=queued)
ctl  claim (SKIP LOCKED) → kabul → state=running, started_at=now(), shard'lar (pending), VU-dakika rezerve
     └─ executor: bölge başına runner (share = 1/bölge sayısı; TT_LOADTEST_MAX_VUS bölgelere bölünür)
run  shard running → her tick: shard.live + kümülatif stats (birleştirilebilir log-bucket histogram) + OTLP
ctl  2 sn'de bir shard'ları birleştir → loadtest_runs.live + çalışırken summary
     abort_requested_at (veya api'nin doğrudan state=aborted yazması) → runner'ları durdur (runner da satırı 1 sn'de bir okur)
     bir bölgede gate abort → diğer bölgeler de durdurulur; sonuçsuz çıkan runner / deadline → shard error
     tüm shard'lar bitti → final summary, thresholds_passed, state passed|failed|aborted|error, kullanım gerçek değere düzeltilir
```

Yeniden başlatma: aynı `TT_LOADTEST_INSTANCE` ile açılan ctl, kendi `running` run'larını `error` ("loadtest-ctl restarted…")
yapar ve `abort_requested_at` koyar (runner'lar kendiliğinden durur). Başka instance'ların 3 dk güncellenmeyen run'ları da `error` olur.

## Şema (`deploy/postgres/006_loadtest.sql`)

- `loadtest_runs` + `region` (virgüllü bölge listesi), `live`, `abort_requested_at`, `error`, `created_at`, `requested_by`,
  `claimed_by`, `vu_minutes` (claim'de rezerve, bitince gerçek).
- `loadtest_domains (id, project_id, host, token, verified_at, method dns|http, created_at, verify_requested_at, last_checked_at, last_error)`.
- `loadtest_usage (project_id, month, vu_minutes)`.
- `loadtest_run_shards (run_id, region, share, state pending|running|done|aborted|error, live, stats, summary, error, abort_reason, …)`.

## Senaryo formatı

Runner, UI `ScenarioEditor`'ın kaydettiği spec'i (`apps/web/src/lib/loadtest.ts` `saveScenario`) **olduğu gibi** koşturur;
contracts §14'teki alternatif şekiller de kabul edilir:

| Alan | Anlam |
|---|---|
| `target`, `regions` | Base URL; bölge listesi (yoksa `TT_LOADTEST_REGIONS`, o da yoksa tek `local` shard) |
| `mode` | `vus` = kapalı model (VU'lar stage hedeflerini izler), `rps` = açık model (arrival rate, önceden ayrılmış VU havuzu; boş VU yoksa `dropped`) |
| `levels [{name, ramp, hold, target}]` | Yük profili; ramp 0 = sıçrama. Yoksa `levels [{durationS, target}]` veya `stages [{duration, target}]`. Hedefler arası lineer, 0'dan başlar |
| `steps ["METHOD /route"]`, `weights` | Ağırlıklar oran veya yüzde (normalize edilir). `flow: "sequential"` → her iterasyonda tüm adımlar sırayla (ağırlık yoksa default); aksi halde iterasyon başına ağırlıklı tek adım |
| `requests[key]` | `params` (`ad: değer` satırları → `:ad` path segmenti ya da query), `headers` (satır veya obje), `body` (JSON şablon metni) veya `json`, `checks` (`"status == 201"`, `"latency < 300ms"`; default status < 400), `pools [{name, values}]` |
| `thinkTime` | Saniye; kapalı modelde her istekten sonra |
| `pools {ad: [değerler]}`, `session {ad: şablon}` | Şablon verisi; havuz satırı obje olabilir (`{{pool.users.email}}`) |
| `thresholds` | `["http_req_failed rate < 1%", "http_req_duration p(95) < 500 ms"]` (p(99), avg, max, med da) veya `{p95Ms, p99Ms, errorRate}` |
| `gate` | `abortErrorRate`, `abortP95Ms`, `minRequests` (default 20) → 5 sn'lik pencerede aşılırsa otomatik abort |
| `maxVus`, `timeoutS`, `script` | rps havuzu; istek timeout'u (30 sn); ham k6 JS |

Şablonlar: `{{int a-b}}`, `{{uuid}}`, `{{pool.<ad>[.<alan>]}}` (aynı istekte aynı satır; `random.<ad>` eşanlamlı),
`{{weighted a:70 b:30}}`, `{{prev "KEY" .alan.0.alt}}` (aynı VU'da KEY adımının son JSON cevabı; sequential akışta iterasyon başında
sıfırlanır), `{{session.<ad>}}`, `{{optional 12% <ifade>}}` (alınmazsa JSON anahtarı düşer), `{{faker.words N|sentence|name|email|number}}`.
Tam olarak tek şablondan oluşan JSON string değeri tipini korur (sayı sayı kalır). Bilinmeyen şablon / boş havuz → yer tutucu değer +
`summary.warnings`.

Script modu: `spec.script` varsa runner `k6 run --summary-export` çalıştırır ve JSON özetini `summary`'ye çevirir (exit 99 = eşik ihlali);
`k6` yoksa run `error: "k6 not installed"`. Script senaryosunda da `target` zorunlu ve doğrulanmış olmalı (k6 başka host'a gitmekten
teknik olarak engellenemez — bilinen açık).

## Çıktı

- `live` = `{ts, vus, rps, p95, errorRate, progress}` (progress 0..1; çok bölgede vus/rps toplanır, p95 en kötü bölge).
- Final `summary`: `progress, rps, p95, errorRate, vus` (tepe), `thresholds [{name, passed, value}]`, `p50, p99, avgMs, maxMs,
  requests, errors, durationS, vuMinutes, dropped`, `steps [{key, count, errors, errorRate, rps, p50, p95, p99, avgMs}]`,
  `statusCodes {"200": n, "error": n}` (`error` = transport hatası), `checks`, `abortReason`, `warnings`, `regions` (çok bölgede).
  Çalışırken `summary` = `progress, rps, p95, errorRate, vus, thresholds, requests, errors` (api'nin run modeli bunları okur).
- Hata oranı = transport hatası **veya** başarısız check (check yoksa status ≥ 400).
- OTLP (delta, her tick): `trooper.loadtest.requests`, `.errors` (sum), `.duration_ms` (histogram, sınırlar 5…10000 ms),
  `.vus` (gauge); resource `service.name=loadtest`, nokta attr'ları `trooper.loadtest.run`, `.step`, `.region`.
  Kimlik: `TT_INTERNAL_TOKEN` → `x-trooper-internal` + `x-trooper-project`; yoksa `TT_DSN` içindeki key.

## Senaryo üretimi

- **Envanterden:** codeintel'in çıkardığı endpoint'ler + `loadtest_hint` (payload şeması) → seçilen endpoint'ler için taslak.
- **Gerçek trafikten:** son 7 günün route dağılımı ve RUM akışları (sayfa geçiş olasılıkları) → gerçekçi trafik karışımı. "Production trafiğinin 3 katı" gibi hedefler.
- Üretilen senaryo her zaman `draft`; kullanıcı onayı olmadan koşmaz.

## Güvenlik

- Hedef domain doğrulaması zorunlu: DNS TXT `trooper-verify=<token>` (host üzerinde) veya `https://<host>/.well-known/trooper-verify`
  gövdesi = token (başka host'a redirect izlenmez). api `POST /loadtests/domains/{id}/verify` → `verify_requested_at`; ctl worker'ı
  (10 sn) kontrol eder, `verified_at` + `method` ya da `last_error` yazar; DNS yayılması için istekten sonra 15 dk boyunca dakikada bir tekrar dener.
- Eşleşme: tam host, ya da **DNS ile** doğrulanmış domain'in alt alan adı. `TT_LOADTEST_ALLOW_LOCAL=true` iken `localhost`,
  `*.localhost`, loopback IP'ler muaf (dev).
- Limitler: `TT_LOADTEST_MAX_VUS` (rps modunda UI tahmini `ceil(rate × (0.2 + think))`; runner'da sert VU tavanı),
  `TT_LOADTEST_MAX_DURATION`, `TT_LOADTEST_MONTHLY_VU_MINUTES` (proje + UTC ay). Red → `state=error`, `error` kullanıcıya dönük mesaj.
- Acil durdurma: `abort_requested_at`; otomatik abort: `gate.abortErrorRate` / `gate.abortP95Ms`.

## Run UI

Canlı: VU sayısı, RPS, p95, error rate (runner tarafı) + hedef servisin RED ve kaynak metrikleri (SDK tarafı), aynı zaman ekseninde ama **ayrı grafiklerde** (dual-axis yasak). Bitince: eşik sonuçları, en yavaş endpoint'ler, önceki run ile karşılaştırma.

## Açık işler

- UI `progression: manual|ai` ve `gate.latencyX/cpu` (hedef servis telemetrisine bakan AI gate) runner'da uygulanmıyor; run timer ile (`auto`) ilerler.
- UI pool'ları sadece `{name, detail}` taşıyor; gerçek değerler `spec.pools` / `requests[k].pools[].values` ile verilmeli, yoksa sentetik değer.
- `{{session.*}}` için login akışı yok; `spec.session` şablonları VU başına bir kez üretilir.
- UI run state'leri `error`'u bilmiyor (api/UI tarafı).
- Bölge ağırlıkları (`eu-central: 70`) yok; yük bölgelere eşit bölünür. Local executor'da bölgeler aynı makinede ayrı süreçlerdir.
