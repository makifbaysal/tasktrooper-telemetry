# Ürün

## Problem

Ekipler observability için üç-dört farklı araç kullanıyor (APM, RUM, log, load test) ve alert'leri elle, genelde bir
incident yaşandıktan sonra tanımlıyor. Yeni endpoint eklenince alert eklenmiyor; load test ayrı bir dünyada koşuyor ve
sonuçları üretim telemetrisiyle karşılaştırılamıyor.

## Çözüm

Tek SDK ailesi + tek platform:
- SDK'yı ekle → backend/worker/consumer/web/mobil verisi otomatik akar.
- Repo'yu bağla → platform endpoint ve consumer envanterini çıkarır, kritik yolları belirler, alert'leri önerir.
- Aynı envanterden load test senaryosu üret, koştur, sonucu canlı telemetri ile yan yana gör.
- **KPI tanımla** ("anasayfa hızlı yüklensin", "checkout başarılı olsun") → alert'ler KPI'ın error budget'ı üzerinden kurulur.
- **Kök neden analizi:** KPI bozulduğunda ya da bir servisin error rate / latency'si arttığında platform çağrı ağacında
  en alt katmana kadar iner ve sorunu adıyla söyler: *"anasayfa yavaş çünkü storefront-bff → recommendation-api →
  reco-db sorguları 63× yavaşladı (migration 0042 index'i düşürdü)"*. Servis sayfasında "senin kodun mu, çağırdığın bir
  servis mi?" sorusuna doğrudan cevap verir. Detay: [`alerting.md` § KPI'lar](alerting.md#kpilar-slo) ve
  [§ Kök neden analizi](alerting.md#kök-neden-analizi).

### Tek yer kuralı

Aynı öğe (liste, "New …" butonu) arayüzde tek bir yerde bulunur. KPI'lar, alert'ler ve loglar sahibi olan servisin
sekmelerinde yaşar; "New KPI" sadece KPIs sekmesinde, "New monitor" sadece Alerts sekmesinde. Overview servisler arası
özet gösterir (açık incident'lar, golden signals, servis tablosu); KPI'ların toplu görünümü sadece `/kpis`'te.

### Frontend = servis

Web ve mobil uygulamalar ayrı bir "Frontend" bölümünde değil, **servis listesinde** yer alır (tip: `web` / `mobile`).
Bir organizasyonda istenen sayıda client app olabilir (storefront, backoffice, seller portal, kurye app, landing...).
Client app'in servis sayfası RUM görünümüdür: KPI'lar, kullanıcılar, vitals, sayfalar/ekranlar, oturumlar, hatalar ve
**API calls** (app'in çağırdığı backend endpoint'leri — frontend'den backend'e geçiş buradan).

## Personalar

| Persona | İhtiyaç |
|---|---|
| **Backend geliştirici** | Endpoint'im yavaş mı, hata veriyor mu, hangi deploy'dan sonra bozuldu? |
| **Frontend/mobil geliştirici** | Hangi sayfa yavaş açılıyor, hangi ekranda crash var, kullanıcı akışı nerede kopuyor? |
| **SRE / platform** | Alert gürültüsü düşük, kapsama yüksek; kapasite sınırları biliniyor. |
| **Ürün / yönetici** | Kaç kullanıcı, hangi sayfalar, oturum süreleri, genel sağlık. |

## Toplanan veri

### Backend (HTTP servis)
- İstek sayısı, durum kodu dağılımı, latency histogramı — route template bazlı
- Trace/span (DB, cache, dış HTTP, queue publish)
- Exception (stack trace, fingerprint)
- Runtime: CPU, memory, GC, goroutine/event-loop lag, pool kullanımı
- Deploy marker (version, commit sha)

### Worker / Consumer
- Mesaj işleme hızı, işlem süresi, başarı/hata, retry, DLQ'ya düşen
- Consumer lag (topic/partition), queue derinliği
- Cron/job: son başarılı koşu, süre, kaçırılan koşu
- **Model: endpoint yok, process var.** Worker/consumer HTTP isteği almaz; bir veri kaynağını (Kafka topic, RabbitMQ/SQS queue, cron schedule) dinler. Birim = **process** (her biri kendi consumer group'u); okuduğu topic ayrı alan. Aynı topic'i birden fazla process okuyabilir (ör. `orders.created` → `order-created-handler`, `order-search-indexer`).
- Servis sayfası: **Processes** tab'ı (Process + group/schedule · Reads from (kaynak + topic, "+N more reader") · msg/min · lag · error · p95 · retry/DLQ). Traffic flow'da gelen taraf "Reads from" = topic'ler (üreten servis `via` olarak), servis değil.
- Lag, alert ve monitor **process'e** bağlıdır (`svc · process`), topic'e değil — lag consumer group başınadır.
- Consumer sayfasında **Lag** tab'ı: servisin okuduğu her (topic, consumer group) için lag zaman serisi, partition başına lag, üretim/tüketim hızı, catch-up ("not catching up" / "drains in X" / "≈ X s behind") ve öneriler (hot partition, members < partitions, rebalance, SDK'sız consumer). Lag broker'dan okunur (Infrastructure → Message queues).

### Web (browser SDK)
- Page view, route değişimi (SPA), referrer, UTM
- Web Vitals: LCP, INP, CLS, FCP, TTFB
- Resource/XHR/fetch zamanlamaları → backend trace ile bağlanır (traceparent)
- JS hataları (source map ile çözülmüş), unhandled rejection
- Kullanıcı: anonim id, `identify()` ile user id/e-posta (opt-in), cihaz, tarayıcı, ülke
- Session: süre, sayfa sayısı, bounce, rage/dead click
- Custom event (`track('checkout_started')`)

### Mobil (React Native, sonra native iOS/Android)
- Screen view, app start (cold/warm), frame drop, ANR, crash
- Network çağrıları, offline kuyruk
- App version, OS, cihaz modeli

### Loglar
- SDK log bridge (pino, winston, zap, slog, console) veya OTLP logs
- Otomatik `trace_id` / `span_id` korelasyonu

## Ekranlar (v1)

| Ekran | Route | İçerik |
|---|---|---|
| Overview | `/` | Açık incident'lar (her birinde kök neden cümlesi; tıkla → drawer: grafik, "Why" bloğu, monitor, kanallar), hero error rate (deploy işaretli) + throughput + latency, servis tablosu (sağlıksız önce, sparkline), "dikkat isteyen endpoint'ler", risk altındaki KPI'lar, alert coverage |
| KPIs | `/kpis` (sidebar, Services'in altında "Objectives") | Tüm servis/app'lerin KPI'ları tek bakışta: risk altındakiler kart (değer, budget, burn, kök neden cümlesi), yoldakiler tablo — her KPI bir kez. App/servis filtresi + arama. "New KPI" yok (yaratma servisin KPIs sekmesinde) |
| KPIs (servis altında) | `/services/:id?tab=kpis` | Servisin/app'in tüm KPI'ları (budget'a göre en kötü önce) + o repodan koddan önerilen KPI'lar. Risk altındakiler servis sayfasının en üstünde. **"New KPI" yalnızca burada** (sayfa drawer'ındaki "Track as KPI" sayfa önseçili kısayol) |
| KPI detail | `/kpis/:id` | Bozuksa en üstte kök neden zinciri (sayfa → API → servis → DB/harici/kod, her adımda önce→şimdi ve açıklama payı, kanıtlar: deploy, migration, sorgu, kaynak), etki (budget, burn, aynı nedenden etkilenen diğer KPI'lar); good-event grafiği + hedef çizgisi, error budget, başlık metriği; KPI'a bağlı alert kuralları; tanım |
| KPI editor | `/kpis/new` | Hedef (sayfa / endpoint / journey / app start), good event tanımı (eşik + hatasız), objective + pencere, burn-rate alert kuralları, canlı önizleme (son 7 gün kaç alert) |
| Services | `/services`, `/services?type=api\|worker\|consumer\|web\|mobile` | Sidebar: Overview · Incidents · **Services** (kategoriler) · **Infrastructure** (Databases, Message queues) · **Objectives** (KPIs) · **Tools** (Load tests, Code insights); ayrı Alerts ve Logs girişi yok — servisin altında. "Services" başlığı altında kategori başına bir giriş (APIs, Workers, Consumers, Web apps, Mobile apps; sayı + sağlıksız rozeti; servis/endpoint sayfası kendi kategorisini aktif gösterir). "Needs attention" kartları (neden sağlıksız), tip bazlı servis tabloları |
| Service detail (client app) | `/services/:id` (web/mobile) | En üstte **sadece risk altındaki KPI'lar** (bozuksa kök neden cümlesi; hepsi yoldaysa bölüm görünmez), aktif kullanıcı + JS error / crash rate, vitals (web: LCP/INP/CLS/TTFB, mobil: cold/warm start, slow frames, API p75), sekmeler: Pages/Screens, Sessions, Errors, **API calls** (→ endpoint sayfası), **Deploys** (Vercel deploy marker'ları: commit, yazar, build durumu, link; ana grafiklerde deploy çizgisi), **KPIs** (app'in tüm KPI'ları, budget'a göre en kötü önce; sekme rozeti risk altındaki sayısı), **Alerts**, **Logs**; sayfa drawer'ından "Track as KPI" |
| Service detail | `/services/:id` | Error rate / latency belirgin arttıysa en üstte **kök neden kartı**: "Your code" / "Your dependency" / "Not your code · downstream" etiketi, cümle, yol, etkilenen KPI'lar; tıkla → zincir + kanıt drawer'ı. Altında golden signals: hero istek grafiği (2xx/4xx/5xx stacked), error rate, latency. Sekmeler: **Endpoints** (varsayılan, sparkline'lı tablo), **Traffic flow** (caller → servis → bağımlılık akışı; gelen trafik caller/endpoint toggle; giden trafik; caller ve bağımlılık satırı → drawer: zaman bazlı response time, error rate, çağrı hacmi, kim çağırıyor), **Time spent** (kategori bazlı süre grafiği + kategori altında bağımlılık kırılımı + endpoint breakdown), **Deploys** (span versiyonları + Vercel/CI deploy marker'ları; tıkla → öncesi/sonrası; error rate ve latency grafiklerinde deploy çizgisi, hover'da commit · yazar), **KPIs** (servisin KPI'ları; risk altındakiler ayrıca kök neden kartının altında en üstte), **Alerts**, **Logs** (ayrı Logs ekranı yok — loglar servisin altında: arama, level facet, lines/patterns, satır → drawer) |
| Endpoint detail | `/services/:id/endpoints/:eid` | Kök neden kartı (servis sayfasıyla aynı mantık, endpoint kapsamında), golden signals (hero status stacked istek grafiği + error rate + latency), "where time goes" (zaman grafiği + istek başına çağrı listesi → bağımlılık drawer), caller grafiği + liste (→ drawer), hatalar (→ stack trace drawer), örnek trace'ler (→ span waterfall drawer) |
| Alerts (servis altında) | `/services/:id?tab=alerts`, endpoint sayfasında "Alerts on this endpoint" bölümü | Ayrı Alerts ekranı yok (`/alerts` → Overview). **Açık incident'lar sekmede değil, servis/endpoint/app sayfasının en üstünde** ("Open incidents" kartları, ack, drawer; yoksa bölüm hiç görünmez). Servisin **Alerts** sekmesi: monitor'leri (servise ait KPI burn monitor'ları dahil), koddan önerilen monitor'ler, geçmiş; "New monitor" / "New KPI" servis önseçili. Endpoint sayfası aynı içeriği endpoint kapsamında gösterir. Tüm açık incident'lar Overview'da |
| Alert editor | `/alerts/new` | Kural oluşturma: metrik/log/anomali, eşik, önizleme |
| Incidents | `/incidents` (sidebar, Overview'ın altında; firing sayısı rozeti) | Tüm servis/app'lerin incident'ları: özet (firing, ack/pending, MTTA, MTTR), açık incident kartları, son 7 günün çözülenleri. Servis/severity filtresi + arama; `?id=` drawer'ı açar. Ack her yerde ortak (sidebar rozeti anında düşer). Servisin Alerts sekmesi aynı incident'ları servis kapsamında gösterir |
| Load tests | `/load-tests` | Tüm senaryolar (Run/Edit), canlı run kartı (aktif seviye + geçiş durumu: "AI holding — Peak not safe"), son koşular. Run kendi kendine ilerler/biter; Stop → aborted |
| Load test run | `/load-tests/runs/:id` | Canlı metrikler, hedef servis CPU/DB, endpoint tablosu. **Level control**: seviye adımları, Step back / Hold here / Continue / Go to next (AI "güvenli değil" derse "anyway"), AI gate kontrolleri (şimdi / bir sonraki seviye için tahmin / limit: run p95 ve hata, servis CPU, her bağımlılığın p95 ve hatası), sorunlu sistemi adıyla söyleyen öneri (önce third-party, sonra kendi servislerimiz) ve karar günlüğü (AI / You) |
| Load test senaryo editörü | `/load-tests/new` (`?from=traffic`, `?service=`), `/load-tests/scenarios/:id/edit` | **Seviyeler** (ad, ramp, hold, hedef; VU veya req/s modeli; preset: ramp & hold, 1×→2×→3×, spike, soak; planlanan yük grafiği) · **Traffic mix** (servis(ler)in endpoint'leri; ağırlık % ya da peak'te req/s elle; "Match production", "Spread evenly") · **Request data** (endpoint başına params, header, JSON body şablonu; `{{pool.x}}`, `{{weighted}}`, `{{prev "…" .id}}`) · "Generate from production": AI production isteklerinden dağılımları ve şablonları çıkarır, PII/secret alanlar staging havuzlarıyla değişir · **Level transitions**: AI-guided (her seviye sonunda API ve çağırdığı sistemler sağlıklıysa ve sonraki seviye limitlerde kalacaksa artırır; değilse bekler ya da geri iner, nedenini ve ne yapılacağını söyler; gate: bağımlılık p95 ≤ N× production, servis CPU ≤ %), Manual (kullanıcı geçirir, aynı kontroller öneri olarak), Timer · threshold'lar · YAML önizleme |
| Load tests (API altında) | `/services/:id?tab=loadtests` (yalnız `http` servisler) | O API'ye vuran senaryolar (buradan Run / Edit / New / Generate), koşuları ve **endpoint bazlı karşılaştırma**: en fazla 4 run seçilir, en eskisi baseline; her endpoint için p95, hata, istek sayısı ve baseline'a göre Δ |
| Databases | `/infrastructure/databases`, `/infrastructure/databases/:id` | Servislerin çağırdığı DB ve cache'ler (PostgreSQL, MySQL, Redis, MongoDB). Liste: "Needs attention" (sorun + neden), CPU / memory / connections (used/max) / throughput / p95 / kullanan servisler. Detay: sorun kartı (neden + çözüm), CPU (node başına), connections, query p95, throughput, hit ratio, replication lag; node tablosu; **top queries / commands** (normalize edilmiş, çağıran servis + endpoint, calls/min, avg/p95, rows, DB zamanındaki pay; plan notu); "Used by" (servis gözünden calls, p95, hata). Cloud entegrasyonundan gelenlerde sağlayıcı rozeti (→ entegrasyon), sync hatasında "values from X ago" kartı; Neon'da compute kartı (proje, branch, endpoint, active/idle, CU aralığı, storage). **Connect database**: engine, toplama yöntemi (Agent / Cloud — RDS/Cloud SQL/ElastiCache/Memorystore/Neon için Integrations'a yönlendirir / Direct read-only user), monitoring user SQL'i, test connection. Servis bağımlılık çekmecesinden "Open orders-db" ile gelinir |
| Message queues | `/infrastructure/queues`, `/infrastructure/queues/:clusterId` (`?tab=topics\|nodes`, `?topic=cluster/name`, `?group=cluster/id`) | Kafka, RabbitMQ, Amazon SQS ve Google Pub/Sub (SQS/Pub/Sub cloud entegrasyonundan; broker/partition/rebalance yok, terimler `brokerTerms`, en eski mesaj yaşı kolon/kartta; entegrasyon rozeti → Integrations). Liste: cluster kartları (broker, topic, group sayısı, toplam lag, geride kalanlar) + tüm cluster'larda **Consumer groups** ve **Topics & queues** tabloları (filtre). Topic çekmecesi: gelen msg/s, producer'lar, group başına lag (çizgi) ve okuyan group'lar; okuyan yoksa uyarı. Group çekmecesi: consumer (servis/process ya da SDK'sız dış sistem), members, state/rebalance, okuduğu her topic için lag grafiği, partition başına lag, öneriler; "Open <servis>" → servisin Lag tab'ı. Cluster sayfası: aynı tablolar + Brokers/Nodes (CPU, memory, disk, leaders, under-replicated). **Connect broker**: Kafka (bootstrap, SASL/mTLS/IAM, Describe ACL'leri) veya RabbitMQ (management API, vhost, `monitoring` tag'li user). RabbitMQ'da topic = queue, lag = ready mesaj. **Lag alerts**: cluster sayfası ve liste sayfasında "Lag alerts" tab'ı, group çekmecesinde liste + "New lag alert", consumer Lag tab'ında kart başına "Alert on lag". Editör: cluster / group / topic, koşul (lag, time behind, partition, growing, not drained, rebalances), eşik, süre, severity, notify, öneri ve 24 saatlik önizleme. SDK'lı consumer'ın lag alert'i servisin Alerts tab'ında da görünür |
| Code insights | `/code` | Bağlı repo'lar, envanter, kapsama (alert'i olmayan endpoint'ler) |
| Settings | `/settings` | Proje, API key, SDK kurulum, bildirim kanalları (Slack, PagerDuty, webhook), cloud entegrasyon özeti (→ Integrations) |
| Integrations | `/settings/integrations` (`?id=` çekmeceyi açar; sidebar alt kısmında, sync hatası rozeti) | Cloud entegrasyonları (ADR-012): sync hatası olanlar en üstte (Reconnect), bağlı liste (durum ikon + etiket, kaynak sayısı, son sync), sağlayıcı kartları (AWS, Google Cloud, Vercel, Neon + yetenekler). Çekmece: keşfedilen kaynaklar (→ Database / Cluster / servis sayfası), config, Vercel'de proje → servis eşlemesi + drain URL + son deploy'lar; Sync now, Disable/Enable, Reconnect, Delete (onaylı). **Connect** çekmeceleri: AWS (IAM role + external id önerilen — CloudFormation / Terraform / policy JSON, seçili servislere göre; alternatif access key; region ve servis seçimi), Google Cloud (service account JSON yükle/yapıştır, gcloud komutları, proje id'leri, servisler), Vercel (token + team, proje → servis eşleme tablosu, drain tipleri, drain secret, drain URL), Neon (API key, org, proje seçimi). Test connection + Connect |

## Kapsam dışı (v1)

- Session replay (video benzeri) — v2
- Profiling (continuous) — v2
- Synthetic monitoring (uptime check) — v1.5, load test runner altyapısını kullanacak
- Self-hosted kurulum — sonra
