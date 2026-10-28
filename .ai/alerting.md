# Alerting

## Monitor tipleri

| Tip | Örnek | Değerlendirme |
|---|---|---|
| `metric_threshold` | `POST /checkout` error rate > %2, 5 dk | Periyodik (30–60 sn), `red_1m` |
| `latency` | p95 > 800 ms, 10 dk | Periyodik |
| `throughput_change` | Throughput son 1 haftanın aynı saatine göre %50 düştü/arttı | Periyodik, mevsimsel baseline |
| `anomaly` | Error rate'te istatistiksel sapma | Periyodik, robust z-score (MAD) + haftalık mevsimsellik |
| `log_match` | `level=error AND body:"payment declined"` sayısı > 20/5dk | Stream (ingest-writer'dan event) + periyodik doğrulama |
| `log_pattern_new` | Daha önce görülmemiş bir error log pattern'ı belirdi | Stream |
| `error_new` | Yeni error fingerprint (yeni issue) | Stream |
| `consumer_lag` | Broker verisi üzerinde, consumer group (+ opsiyonel topic) başına: lag > N, time behind (lag ÷ produce rate) > N s, herhangi partition > N, lag X dk boyunca artıyor, batch consumer N saatte drain olmadı, rebalance/saat > N. Editör son 24 saati replay eder ("would have fired N×") ve group'a göre eşik önerir (batch → not drained, geride kalan → growing, diğerleri → 7 günlük p99 time-behind × 2). Monitor `queue` alanı taşır | Periyodik (15 sn broker scrape) |
| `job_missed` | Cron job beklenen sürede koşmadı (heartbeat) | Periyodik |
| `rum_vital` | `/checkout` sayfasında p75 LCP > 2.5 sn | Periyodik, `rum_views_1m` |
| `composite` | A AND B (ör. error rate yüksek VE throughput normal) | Alt monitor'lerin durumları |
| `kpi_burn` | "Homepage loads fast" KPI'ının error budget'ı 1 saatte 6× hızlı yanıyor | Periyodik, `sli_1m`; multi-window burn rate (aşağıda) |
| `infra` | Pod 10 dk'da 3'ten fazla restart etti; node NotReady; host CPU > %90; workload'da eksik replika | Periyodik, `hosts_1m` / `containers_1m` / `k8s_*_1m` (ADR-019) |
| `faas` | Lambda/Azure fonksiyonunda error rate > %5, throttle, p95 timeout'un %80'ini aştı | Periyodik, `faas_1m` + cold start için `spans` (ADR-020) |
| `llm` | Saatlik tahmini LLM maliyeti > $20; `gpt-5` hata oranı > %5; Anthropic p95 > 20 sn; token patlaması | Periyodik, `llm_1m`/`llm_1h` + fiyat tablosu (ADR-023, contracts §25) |

## Kural şeması (alert-as-code ile aynı)

```yaml
name: Checkout error rate
kind: metric_threshold
query:
  source: red
  service: orders-api
  route: POST /checkout
  metric: error_rate
condition:
  op: '>'
  threshold: 0.02
  for: 5m
  min_requests: 50          # düşük trafikte false positive önler
severity: critical           # info | warning | critical
notify: [slack:#payments-alerts, pagerduty:payments]
labels: { team: payments }
```

Monitor'ler UI'dan veya repo'daki `.trooper/monitors/*.yaml` dosyalarından yönetilir (iki yönlü sync; repo kaynaklı monitor UI'da read-only).
Repo kaynaklı monitor'ler (`monitors.source='code'`, `code_ref` = `repo:path`) motor tarafında diğerleri gibi değerlendirilir;
bildirim payload'u `codeRef` taşır ve kanallar "Defined in code" olarak gösterir.

## Incident state machine

`ok → pending (for süresi doluyor) → firing → acknowledged → resolved`
- Flapping koruması: resolve için `for` süresinin yarısı kadar temiz veri gerekir.
- Dedupe anahtarı: `monitor_id + group_by değerleri`.
- `pending` girişinde `pending` incident açılır, bildirim gitmez; `firing`'e geçince aynı incident yükseltilir ve bildirim gider. `pending`'den firing olmadan dönerse incident `quiet=true` ile resolve edilir (UI varsayılan listede gizler). Monitor başına tek açık incident.
- **Mute / bakım penceresi:** `monitors.muted_until` ve `maintenance_windows` (monitor/servis kapsamlı; KPI kuralları dahil). Değerlendirme ve state kaydı sürer, sadece teslimat kuyruğa girmez; event'ler `suppressed:true` + `muted` / `maintenance:<id>` sebebi taşır, ayrıca `suppressed` event'i yazılır. Resolve sadece firing bildirimini almış kanallara gider.
- **Hatırlatma:** `condition.renotifyMin`; n. hatırlatma incident başına bir kez (dedupe `incident:<id>:reminder-<n>:<channel>`). Acknowledged/pending incident'lara gitmez. PagerDuty/Opsgenie aynı key ile açık alert'e eklenir.
  KPI kuralları da aynı şekilde: `kpi_alert_rules.condition.renotifyMin` (`008_alerting_r3.sql`).
- Incident event kind'ları: `pending`, `firing`, `acknowledged`, `resolved`, `suppressed`, `reminder`.
- Outbox (`notifications.kind`): `firing`, `resolved`, `reminder`, `acknowledged` (sadece Slack App hedefleri; ack edilen
  incident'ın firing mesajının thread'ine yazılır).

### Değerlendirme detayları (alert-engine)

| Tip | Kaynak / algoritma |
|---|---|
| `anomaly` | Son 4 haftanın aynı minute-of-week'i ±1 saat, `red_1m` (tam saatlik pencerelerde `red_1h`); robust z-score |
| `composite` | Diğer monitor'lerin state'leri üzerinde `and`/`or`; diğer tüm monitor'lerden sonra değerlendirilir |
| `job_missed` | `consumer_1m` (process) veya `red_1m` (route); değer = son çalışmadan beri dakika |
| `error_new` | `errors_1h` geçmişi; stream yolunda +2 s / +6 s ClickHouse kontrolü |
| `consumer_lag` `oldest_age` | `broker_lag_1m.oldest_age_s`; messaging system `broker_clusters.kind`'den (sqs→`aws_sqs`, pubsub→`gcp_pubsub`); Kafka cluster'da `no_data` |
| `log_match` (stream) | `TT_ALERT_STREAM=true`; franz-go group `tt-alert-stream`, sadece shard 0'ın sahibi (stream leader) tüketir; bellekte sayım, ~1 s'de firing. Çoklu shard'da stream leader **tüm** shard'ların stream kurallarını değerlendirir; başka shard'ın monitor'ündeki eşleşme `alert_stream_hits` (unlogged) satırı + `NOTIFY tt_stream_hit` ile sahibine devredilir, sahibi `LISTEN` + 1 sn poll ile alıp (~100 ms) sadece yükseltir. Stream sadece yükseltir, resolve periyodik döngüde |
| `infra` | `query: {entity: host\|container\|pod\|node\|workload, metric, agg: avg (varsayılan)\|max, filter: {cluster, namespace, host, workload, service}}` (`target.service` filter.service yerine geçer). Her varlık için pencere değeri (dakikalık `max`, sonra `agg`); monitor değeri en kötü varlık (op `<` ise en küçük), `detail.entities` en kötü 5 (`id`, label'lar, `value`). `restarts` = pencere içi artış; `cpu_ratio`/`memory_ratio` = Σ kullanım / Σ limit (limiti olan container'lar); pod `not_ready` / node `not_ready` = son örneği ready=0 olan varlık **sayısı** (phase `Succeeded` ve pencerenin en yeni dakikasından 2 dk eski — silinmiş — varlıklar hariç); workload `unavailable` = en son `desired − available`. Eşleşen varlıkta hiç değer yoksa `no_data` |
| `faas` | `query: {functionId? \| filter: {provider, region, service}, metric, relativeToTimeout?}`. Fonksiyon başına: sayılar toplam, `error_rate` = errors/invocations, `duration_p95/p99` çağrı ağırlıklı ortalama (`relativeToTimeout` → ÷ timeout), `concurrency`/`iterator_age_ms` max, `cold_starts` = fonksiyonun servisinin `spans`'taki `faas.coldstart='true'` entry span'leri. `service` filtresi, cold start ve timeout Postgres `serverless_functions`'tan çözülür. `minRequests` altındaki fonksiyonlar oran/süre metriklerinde breach'e karar vermez. Raporlanmayan metrik (ör. Azure throttles) → `no_data` |

**Şablonlar** (`services/alerting/internal/eval/templates.go`, `source='template'`): pod CrashLoop (`restarts > 3`, 10 dk
pencere), node NotReady (`not_ready > 0`, 5 dk), host CPU > 0.9 (avg, 10 dk), host disk > 0.9, workload `unavailable > 0`
(10 dk), fonksiyon error rate > 0.05 (5 dk, minRequests 20), throttles > 0 (5 dk), p95 > timeout × 0.8 (`relativeToTimeout`).

**Altyapı kök neden adayları (ADR-019):** servisi olan her incident'ta (monitor ve KPI) çağrı ağacı RCA'sına ek olarak son 15 dk
içinde aynı `service`'in pod'ları (`k8s_pods_1m`) ve servisi çalıştıran host'lar (`containers_1m` host'ları + pod node'ları,
`hosts_1m`) taranır: `OOMKilled` (0.8) / `CrashLoopBackOff` (0.75) / sadece restart artışı (0.6), restart ≥ 3 ise +0.05, birden
fazla pod etkilenmişse +0.05; host ortalama CPU > 0.9 → "Host <h> CPU saturated" (0.6 + (cpu − 0.9) × 2.5). Güven 0.6–0.85
aralığına sıkıştırılır, en fazla 5 aday. Çağrı ağacı yoksa, servisin kendi kodunda bitiyorsa, hiçbir şey açıklamıyorsa veya
güveni en iyi adaydan düşükse özet/güven en iyi adayınki olur ("Pod api-1 restarted (OOMKilled).", Slack "Likely cause" satırı
bunu gösterir); ağacın cevabı `treeSummary`/`treeConfidence`'ta kalır. Adaylar `root_cause.infra[]` ve kök düğümün
`evidence`'ında (`kind: resource`) yer alır.

KPI kurallarının state'i `kpi_alert_rules.state/state_since/last_value/last_eval_at`'te tutulur, açılışta yüklenir; KPI incident'ları `kpi_rule_id` taşır. Firing bildirimi enqueue edilmeden önce aynı transaction'da
`incidents.root_cause` (RCA sonucu, `confidence` dahil) yazılır; payload `rootCause {summary, confidence}` ve servisin son 1 saatteki
deploy'unu (`deploy`) taşır. Query JSON detayları: `services/alerting/README.md`.

## Kod-farkındalıklı otomatik alert

Akış (detay: [code-intelligence.md](code-intelligence.md)):

1. `codeintel` repo'dan envanter çıkarır: endpoint, consumer, cron, dış bağımlılık, kritik iş akışları (ödeme, auth, sipariş).
2. Her envanter öğesi için **kritiklik skoru** hesaplanır:
   - Statik sinyaller: path/isimde `payment|checkout|auth|login|order` vb., DB write yapıyor mu, dış ödeme sağlayıcısı çağırıyor mu, transaction kullanıyor mu, auth middleware'i var mı.
   - LLM sinyali: handler kodu + çevresi verilerek "iş etkisi" sınıflandırması (`critical | important | low`) ve gerekçe.
   - Gözlemsel sinyaller (varsa): gerçek trafik hacmi, mevcut error rate, latency profili.
3. **Şablon seçimi** kritikliğe ve tipe göre:
   - HTTP endpoint (critical): error rate + p95 latency + throughput düşüşü
   - HTTP endpoint (important): error rate
   - Consumer: lag + failure rate + DLQ > 0
   - Cron: job_missed
   - Dış bağımlılık: client span error rate
   - Log: koddaki `logger.error(...)` çağrılarından anlamlı mesajlar → `log_match` önerisi
4. **Eşikler** gözlemsel veriden türetilir: 14 günlük baseline varsa `p95 * 1.5`, error rate için `max(baseline_p99, %1)`. Veri yoksa şablon default'u + "kalibrasyon" etiketi; 7 gün sonra otomatik yeniden önerilir.
5. Öneriler `alert_suggestions` tablosuna `proposed` olarak yazılır. Kullanıcı UI'da toplu kabul eder veya proje ayarında **auto-accept** açıksa doğrudan `active` monitor olur.
6. Opsiyonel: kabul edilen monitor'ler için repo'ya `.trooper/monitors/` PR'ı açılır.
7. Yeni commit'te diff analizi: yeni endpoint → yeni öneri; silinen endpoint → monitor'ü arşivleme önerisi.

**Kapsama metriği:** kritik envanter öğelerinin yüzde kaçının aktif monitor'ü var → Code insights ekranında gösterilir.

## KPI'lar (SLO)

KPI, kullanıcının gördüğü bir sonucun hedefidir: *"anasayfa hızlı yüklensin"*, *"checkout başarılı olsun"*,
*"POST /v1/orders 5xx dönmesin"*. Teknik olarak bir SLO'dur:

| Alan | Örnek |
|---|---|
| Hedef (target) | `page` (app + path), `endpoint` (servis + route), `journey` (sıralı adımlar), `app_start` (mobil cold start) |
| SLI tipi | `latency` (eşik altı + hatasız), `availability` (sayfa render oldu / 2xx), `success_rate` (5xx değil, journey tamamlandı) |
| Good event | `LCP ≤ 2.5 s ve JS error yok`, `duration ≤ 300 ms ve status < 500` |
| Objective + pencere | `%95`, `28d` (veya `7d`) |
| Error budget | `1 − objective` kadar kötü event hakkı; kalan oran gösterilir |

**Alert'lar KPI üzerinde tanımlanır** ve error budget burn rate'ine bakar (kısa dalgalanmalar sayfa açmaz):

- *Fast burn*: 1 saat **ve** 5 dk pencerede burn rate > N× (ör. 6×) → page (PagerDuty).
- *Slow burn*: 6 saat **ve** 30 dk pencerede burn rate > M× (ör. 3×) → ticket / Slack.
- *SLI below*: basit eşik (ör. "%97 altında 15 dk") — opsiyonel.

Burn rate = (1 − good_ratio) / (1 − objective). 6× = 28 günlük bütçe ~4.7 günde biter.
Her KPI alert'ı (`kpi_burn` monitor) bildirime **kök neden cümlesini** ekler.

Koddan öneri: code intelligence, router'da landing / checkout gibi kritik sayfaları ve kritik endpoint'leri bulunca KPI
önerir (ör. `/` → "Homepage loads fast", checkout akışı → "Checkout succeeds").

## Kök neden analizi

Amaç: "error rate arttı" demek yerine **"bu sorun senden değil; payments-api → iyzico çağrıları timeout oluyor"** ya da
**"anasayfa yavaş çünkü recommendation-api'nin reco-db sorguları 63× yavaşladı"** demek. Hem KPI'lar hem de servis /
endpoint sayfaları aynı motoru kullanır (`alert-engine` içinde `rca` paketi; UI tarafı `apps/web/src/lib/rca.ts` ile aynı algoritma).

**Girdi — çağrı ağacı.** Span'lardan (parent/child + `peer.service` / `db.system`) dakikalık kenar rollup'ı:
`edges_1m(caller_service, caller_endpoint, callee_kind, callee_service, callee_endpoint|dependency, calls, errors, duration_sum)`.
Buradan bir düğüm için ağaç kurulur: sayfa → API endpoint → downstream servis endpoint'i → DB / cache / kuyruk / harici API.
Her düğüm için metrik iki pencerede hesaplanır: **şimdi** (son 10–15 dk) ve **baseline** (değişim başlamadan önceki 1 saat /
geçen haftanın aynı saati). Metrik: latency → istek başına ortalama ms, hatalar → başarısızlık oranı.

**Algoritma (delta attribution):**
1. Düğümün açıklanacak değişimi: `current − baseline`.
2. Her çocuğun katkısı: `çağrı/istek × (child.current − child.baseline)`. Paralel çağrılar (aynı istekte eşzamanlı HTTP)
   için sadece en yavaş dal kritik yoldadır.
3. Çocukların açıklamadığı kalan → düğümün **kendi kodu**.
4. Pozitif katkılar normalize edilir (pay 0..1); en büyük paya sahip aday takip edilir, ≥ %15 olan diğerleri "also" olarak gösterilir.
5. Durma: kendi kod kazanırsa, yaprak düğüme (DB, cache, harici API) gelinirse ya da değişim yoksa.
6. Güven = adımlardaki payların çarpımı.

**Çıktı:** yol (her adımda önce → şimdi, açıklama payı), kök düğüm, tek cümlelik özet ve **kanıtlar**: kök düğüm ile
zamanca örtüşen deploy'lar / migration'lar (deploy marker + repo diff), en çok değişen sorgu fingerprint'i
(`db.statement` normalize), kaynak metrikleri (DB CPU, bağlantı havuzu), ilgili log pattern'leri, harici sağlayıcı hataları.

**Servis sayfasındaki etiket:** kök neden bakılan servisin kendi kodu ise **"Your code"**, servisin doğrudan çağırdığı
bir bağımlılık (kendi DB'si, harici API) ise **"Your dependency"**, başka bir servisin altındaysa
**"Not your code · downstream"**. Ayrıca kök nedenin etkilediği KPI'lar ("users feel it in") listelenir.

Kart sadece metrik belirgin değiştiğinde gösterilir: göreli değişim ≥ %30 ve mutlak değişim ≥ 0.1 puan (hata) / 20 ms (latency).

## Bildirim

Kanallar: Slack, PagerDuty, Opsgenie, e-posta, webhook, Microsoft Teams (Adaptive Card; Incoming Webhook ve Workflows URL'leri, `teams:`/`msteams:` prefix).
Bildirim içeriği: grafik snapshot'ı, ilgili deploy (son 1 saat), örnek trace/log linki, kod referansı (dosya:satır, envanterden).

**Slack App (ADR-017, contracts §17):** hedef `slack:<channel_id>#<ad>` → org'un bot token'ı ile `chat.postMessage`; ilk mesajın
`ts`'i `notification_messages`'e yazılır, `resolved` / `reminder` / `acknowledged` aynı mesajın thread'ine cevap olarak gider ve ilk
mesaj `chat.update` ile güncellenir (durum rengi, butonlar). Mesaj: header (durum emoji + başlık), alanlar (servis, değer vs eşik,
süre, severity), **"Likely cause: … (NN% confidence)"** (`< %30` → "low confidence"; RCA yoksa satır yok), deploy / kod bağlamı,
butonlar *Open incident* (`TT_PUBLIC_APP_URL`), *Ack* (`tt_ack:<incidentId>`), *Mute 1h* (`tt_mute1h:<monitorId>`) — api
`POST /slack/interactions` işler. Hatalar: `not_in_channel` → `conversations.join` + tekrar (private kanal → kalıcı hata, "app'i
kanala davet et"), `channel_not_found` / `invalid_auth` → kalıcı, 429 → `Retry-After`. `slack:https://hooks.slack.com/…` webhook
hedefleri eskisi gibi çalışır.

**AI kota bildirimi (ADR-018, contracts §19):** alert-engine her `TT_AI_QUOTA_CHECK_INTERVAL` (5 dk) bu ayın (UTC)
`ai_usage` toplamını org bazında hesaplar; etkin limit (`orgs.ai_monthly_credits` ?? paket, `-1` sınırsız) %80 / %100'e ulaştıysa
ve org'un `ai_notify` hedefi varsa `notifications` kuyruğuna kind `ai_quota` satırı yazar (incident yok; payload `{orgId, projectId,
period, threshold, used, limit, resetsAt, link}`). Dedupe `ai_quota_alerts (org_id, period, threshold)` ile — ay+eşik başına tek
mesaj; doğrudan %100'e sıçrayan org'a sadece %100 gider (%80 de işaretlenir). Satır PK'sı sayesinde her replika kontrolü
çalıştırabilir (leader gerekmez). `projectId` = org'un en küçük project id'si (kanal adı / Slack kurulumu çözümü için).
Notifier metni tüm kanallarda tek satır: "AI usage at 80% of this month's credits (4,012 / 5,000). Resets Nov 1." (%100'de
"AI enrichment falls back to static rules until then." eklenir). Slack App hedefi → düz `chat.postMessage` (thread / buton /
`notification_messages` yok); Slack webhook → `{text}`; e-posta → düz metin (konu "AI usage at 80% of this month's credits");
Teams → tek TextBlock'lu Adaptive Card; webhook → `{event:"ai_quota", …payload, text}` (`X-Trooper-Event: ai_quota`, imza aynı);
PagerDuty/Opsgenie → warning/P4, dedup `tt-ai-quota-<org>-<period>-<threshold>`. `email:<adres>[,<adres>]` hedefleri
`notification_channels` satırı olmadan da çözülür.
