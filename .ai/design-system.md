# Design system

Kaynak: `apps/web/src/styles/tokens.css` (token'lar), `apps/web/src/components/ui` (komponentler), `apps/web/src/components/charts` (grafikler).
Canlı showcase: dashboard'da **`/design-system`** sayfası.

**Tema kaynağı: TaskTrooper.** Renkler, tipografi, radius ve kalıplar TaskTrooper uygulamasının temasını izler
(`../tasktrooper-company/desktop/ui/src/styles/globals.css` ve ops konsolu `components/operations/**`). Orada tema
değişirse bu dosya ve `tokens.css` aynı PR'da güncellenir.

## İlkeler

1. **Veri yüksek sesle konuşur, arayüz sessiz kalır.** Hue-265 soğuk nötrler, primary = TaskTrooper laciverti (`--accent`), marka işareti `--brand-mark #15214b`, sıcak amber (`--accent-warm`) sadece seçim/vurgu. Renk sadece veri ve durum için.
2. **Yoğun ama ferah.** Body 14px (Inter), 4px grid, tablolar sıkı; kartlar arası 16px. Micro etiketler (`.eyebrow`, tablo başlığı, metric label) 11px uppercase + letter-spacing.
3. **Durum asla tek başına renk değildir.** Status her zaman ikon + etiket (`StatusBadge`).
4. **Light ve dark eşit vatandaş.** Dark tema aynı rampalardan seçilmiş ayrı adımlar, otomatik ters çevirme değil. Tema: `system | light | dark` (topbar'dan), `data-theme` attribute'u ile.
5. **Ham değer yasak.** Komponentlerde hex, px font size, rastgele spacing yazılmaz; token kullanılır.

## Token'lar

### Yüzeyler & mürekkep
| Token | Kullanım |
|---|---|
| `--bg-page` | Sayfa zemini |
| `--bg-surface` | Kart, grafik yüzeyi |
| `--bg-raised` | Input, popover, tooltip |
| `--bg-sunken` | Kod bloğu, segmented zemin, well |
| `--bg-hover` / `--bg-active` | Satır/nav hover ve seçili |
| `--text-primary` / `--text-secondary` / `--text-muted` | Metin hiyerarşisi (muted = eksen, meta) |
| `--border` / `--border-strong` / `--divider` | Kenar, input kenarı, tablo çizgisi |
| `--accent`, `--accent-hover`, `--accent-soft`, `--accent-text` | Primary aksiyon (lacivert), seçili tab, link |
| `--brand-mark`, `--accent-warm` | Logo kutusu (iki temada sabit), seçim rengi |
| `--scrollbar-thumb(-hover)`, `--scrollbar-track` | İnce, yuvarlak, temaya göre scrollbar (8px) |

### Diff (diverging çift)
`--diff-increase` (turuncu, pay büyüdü) · `--diff-decrease` (mavi, pay küçüldü) — light + dark ayrı adımlar. Sadece karşılaştırma
grafiklerinde (profil diff flame graph, "Biggest changes" swatch'ı). Status renkleri diff için **kullanılmaz**; şiddet
`color-mix(in srgb, var(--diff-*) N%, var(--bg-surface))` ile (oklch'de karıştırma hue'yu magentaya kaydırır). Değişmeyen = `--bg-sunken`.

### Status (dark temada TaskTrooper `.dark` adımları; `-soft` arka plan, `-text` metin)
`--status-good` · `--status-warning` · `--status-serious` · `--status-critical` (+ `-soft` arka plan, `-text` metin)

Eşleme: `ok/resolved → good`, `warning/pending → warning`, `acknowledged → serious`, `critical/firing → critical`, `muted → neutral`.

### Tipografi
| Token | px / lh | Kullanım |
|---|---|---|
| `--text-xs` | 11/14 | micro: eksen tick, badge, uppercase etiketler |
| `--text-sm` | 12/16 | caption: ikincil metin, meta |
| `--text-md` | 14/20 | **body default** |
| `--text-lg` | 16/24 | heading: section ve kart başlığı |
| `--text-xl` | 20/28 | title: sayfa başlığı (h1) |
| `--text-2xl` | 28/36 | display: stat / metric değeri |
| `--text-hero` | 44/48 | Sayfa başına en fazla bir hero sayı |

Font: `--font-sans` (Inter), `--font-mono` (JetBrains Mono — route, log, kod, trace id, yol chip'leri). Sayılar `tabular-nums`, büyük değerler `letter-spacing: -0.02em`.

### Spacing, radius, layout
- Spacing: `--space-1..12` (4px grid).
- Radius: `--radius-xs 4` (bar ucu) · `sm 6` (chip, segmented item) · `md 8` (buton, input, nav item) · `lg 14` (kart, rounded-xl) · `full` (badge).
- Kontrol yükseklikleri: `--control-h-sm 30`, `--control-h-md 36`.
- Layout: sidebar 240px, topbar 56px (yarı saydam + blur), içerik max 1440px. 900px altı sidebar üstte yatay.
- Kartlar: `--shadow-sm` (TaskTrooper shadow-raised). Alarm durumundaki kart: sol 4px status rail (`.card--rail-critical` / `.card--rail-warning`).

## Komponentler (`components/ui`)

| Komponent | Not |
|---|---|
| `Button` | `primary` (sayfada tek ana aksiyon), `secondary` (default), `ghost`, `danger`; `sm`/`md`; `iconOnly` için `aria-label` zorunlu |
| `Badge` | `neutral, accent, good, warning, serious, critical` tonları |
| `StatusBadge` | Durum için tek doğru yol — ikon + etiket |
| `Card` | `title`, `subtitle`, `actions`, `flush` (tablo/list için padding'siz) |
| `StatTile` | label · value(+unit) · delta (yön × `upIsGood`) · opsiyonel sparkline |
| `Table` | Kolon tanımlı, `numeric` sağa hizalı tabular, `sortValue` ile sıralanabilir, `onRowClick`; `childRows` ile iç içe satırlar (ebeveynin hemen altında girintili, aynı sıralama — ör. Azure Function App'in fonksiyonları) |
| `Tabs` / `Segmented` | Tabs = sayfa içi bölüm; Segmented = filtre/zaman aralığı |
| `Field`, `Input`, `Textarea`, `SearchInput`, `Select`, `Toggle` | Form elemanları (`Textarea mono` JSON/header şablonları için) |
| `PageHeader` | breadcrumb + başlık + alt başlık + aksiyonlar |
| `EmptyState`, `Meter`, `CodeBlock`, `Callout` | Yardımcılar |
| `ChoiceChips` | Çoklu seçim (region, servis, drain tipi): `Segmented` görünümü, birden çok `aria-pressed`, dar ekranda satır kırar |
| `CopyCode` | Kopyala butonlu `CodeBlock` (IAM policy, CloudFormation/Terraform, drain URL); `wrap` tek satırlık uzun değeri (URL, ARN) kırar |
| `Menu`, `MenuItem`, `MenuLabel`, `MenuDivider` | Trigger'a bağlı popover menü (kullanıcı menüsü, org/proje seçici). `trigger({open, toggle})` kendi butonunu çizer; Esc / dış tık / seçim kapatır; `align` start/end, `placement` below/above, `block` tam genişlik. `MenuItem selected` → check ikonu, `danger`, `hint` ikinci satır. Stil `styles/account.css` (`--bg-raised`, `--shadow-lg`) |
| `Avatar` | Baş harfler `--accent-soft` disk üzerinde veya IdP resmi; `sm` / `md` (= `--control-h-sm`) / `lg` |

Entegrasyon kalıpları (`screens/Integrations.tsx`): `ProviderMark` (sağlayıcı kimliği — marka rengi/logosu yok, nötr kutu + kısaltma),
`IntegrationStatusBadge` (ok → Connected, error → Error, pending → Syncing…, disabled → Disabled; hep ikon + etiket),
`IntegrationSourceBadge` (keşfedilen DB/cluster satırında "AWS · aws-prod", tıkla → entegrasyon çekmecesi; sync hatalıysa critical ton),
`DeployList` (deploy marker listesi: versiyon, commit mesajı, yazar, kaynak, link, build durumu).

Code insights / load test kalıpları: `SuggestionCard` (`screens/CodeInsights.tsx` — öneri kartı: kritiklik + "From code" + calibrated/not calibrated badge,
hedef, koşul mono, gerekçe, `dosya:satır` GitHub linki, Reject / Customize / Accept; kabul sonrası "Monitor created" + monitöre link),
`ConnectRepoDrawer` (`screens/ConnectRepo.tsx` — birincil "Connect GitHub" (api'ye tam sayfa yönlendirme), dönüşte `?github=<id>` ile repo seçici:
arama, Private/Added badge'leri, çoklu seçim; token / local path "Advanced" altında), repo satırı durumları (Analyzing… / Analyzing push… +
`pending_paths`, Disconnected warning, Analysis failed + hata metni, "Webhook active"), `RunBlockedCallout` + `ScenarioSource`
(`screens/LoadTests.tsx` — draft / domain / limit / api nedenli "çalışamaz" uyarısı, domain için Domains linki; Draft · needs review warning badge'i),
`LoadTestDomains` (`screens/LoadTestDomains.tsx` — TXT kaydı ve `/.well-known/trooper-verify` talimatları `CopyCode` ile, Verify / Checking… durumu),
run ekranı `TargetCard` (hedef servis RED: "All traffic" slot 1 vs "Load test" slot 2, ölçü başına ayrı grafik).

Hesap / bildirim kalıpları: `Login` (`screens/Login.tsx`, router dışında, `.login__card`), `NotifyPicker` (`screens/NotifyPicker.tsx` — chip'li hedef girişi: Slack kanalı `#ad`, private kilit ikonu, serbest hedef mono; arama listesi `Menu` stilinde), `MonitorSourceBadge` (`code` → accent + kilit "Managed in repository", `suggestion` → accent + ✨), `LikelyCause` (özet + güven badge'i; < %30 warning tonunda "low confidence"). Rol gereği kapalı aksiyonlar gizlenir ya da `disabled` + `title` ile nedeni söylenir (`denied()`); salt okunur formlar `<fieldset disabled>`.

Altyapı / K8s kalıpları (`screens/k8sShared.tsx`): `InfraStatusBadge` (ok/warning/critical + `stale` → nötr saat ikonu "Stale"), `PodPhaseBadge` (reason varsa onu gösterir — CrashLoopBackOff/OOMKilled/ImagePullBackOff critical, diğerleri warning; yoksa phase), `UsageCell` / `LimitCell` (0..1 meter; kullanılan / limit, limit yoksa "no limit"), `EventList` (K8s Warning event akışı), `AgentSetup` (veri yokken Helm + `docker run` talimatı, eBPF opsiyonel notu), `EbpfBadge` (accent, SDK'sız eBPF servisi). Kapasite grafikleri birim başına tek grafik: used / requested / allocatable (veya usage / request / limit) `LINE_SLOTS` 1-2-3.

AI kullanım / serverless kalıpları: `AiQuotaBanner` (`screens/Usage.tsx` — sayfa üstü `Callout`, warning/critical ton badge + metin + View usage + kapat; ay ve eşik başına kapatılır), `AiCapEditor` (üye tavanı: `Meter` + "used / cap" veya "No cap", kalem → inline sayı input'u, Enter kaydeder / Esc vazgeçer; Usage ve Members tablosunda), kredi formatı `credits()` (100 altı 2 ondalık). `FaasStatusBadge` (`screens/Serverless.tsx` — ok/warning/critical `StatusBadge`, `idle` / `missing` nötr "Idle" / "Missing"), maliyet her yerde `usd()` + "est." etiketi; fonksiyon detayında birim başına ayrı grafik (sayılar, ms, concurrency, cold start kolonları), hata serisi `--series-8`.

AI monitoring kalıpları (`screens/AiMonitoring.tsx`, ADR-023 — "AI usage" (kota) ile karıştırma): `AiMonitoringBody` (`/ai` ve servis **AI** sekmesi aynı gövde, `service` kapsamı), `ProviderBadge` (sağlayıcı kimliği nötr `Badge` — marka/status rengi yok), maliyet hücresi (`usd()` / tek çağrı için `aiUsd()` 4 ondalık, her yerde "est."; fiyat yoksa muted **price unknown** → `AiPricesDrawer`, toplamlar "partial"), model renkleri sayfadaki alfabetik model listesine göre `LINE_SLOTS` (maliyet `Columns` stacked, "Other" `--text-muted`), dört `MetricCard` (maliyet, token input/output, çağrı + hata `--series-8`, p95 + TTFT hint'te). `AiCallDrawer` (meta, token/maliyet, hata `Callout`, OTel GenAI mesajları rol bazlı `.llm-msg` kartları — tool call / tool result `CodeBlock`; içerik yoksa "Content capture is off" `EmptyState` + Node/Go snippet; "Open trace" → `/traces/:id?span=`), `AiPricesDrawer` (fiyat tablosu USD / 1M token, Default / "Org override" badge, admin+ düzenle / sıfırla, viewer salt okunur `Callout`), `AiSetup` (LLM verisi yokken SDK başına `Segmented` + `CopyCode`), `LlmSpanSection` (trace span panelinde model, token, maliyet, TTFT + prompt çekmecesi). Stil `styles/aimon.css`.

Session replay (`components/replay`, ADR-021): `ReplayPlayer` (web, `@rrweb/replay` 2.2.0 Replayer, kayıt boyutunu sığdıracak şekilde
ölçeklenen sahne; segment'ler geldikçe `addEvent`; imleç tıklama halkası `--accent-warm`) ve `WireframePlayer` (mobil, SVG: düğüm türüne göre
kutu, kayıttaki bg/fg/radius/border; maskeli etiket = kelime başına çubuk, maskeli input = nokta, görsel = yer tutucu kutu + glif; dokunma
`--accent-warm` halka + iz, klavye nötr panel; cihaz çerçevesi `--border-strong`). İkisi de aynı `PlaybackProps`'u alır (playing, speed,
skipInactive, seek, onTime, onEnded). Ekran (`screens/ReplayDetail.tsx`): oynatıcı + kontroller (play/pause, 1×/2×/4×/8× `Segmented`, "Skip
inactivity" `Toggle`) + scrubber (oynatılan `--accent`, yüklenen `--bg-active`, boş/inaktif aralık taralı; işaretler: view `--series-1`,
tıklama `--series-3`, hata `--series-8`, başarısız istek `--series-2`, legend ile) + sağda olay listesi (aktif satır `--accent-soft` + sol
ray, gelecektekiler soluk; hata → stack çekmecesi, `traceId` → trace linki). Başlıkta kilit ikonlu "Text and inputs are masked".

Profiling ekranı (`screens/Profiling.tsx`): tip `Segmented`, host / versiyon `Select`, Profile / Compare; host başına `TimeSeries` (core),
`FlameGraph`, Top functions `Table` (satıra tıkla → flame graph'ta arama), Compare: versiyon↔versiyon veya zaman penceresi, diff flame graph +
"Biggest changes" (swatch + `±x pp`). Profil yoksa `ProfilingSetup` (`TROOPER_PROFILING=true` `CopyCode`), endpoint sayfasında kompakt `Callout`.

Ekran seviyesi kalıplar (`screens/rca.tsx`):

| Kalıp | Not |
|---|---|
| `RootCauseCard` | Servis/endpoint sayfasında golden signals'ın üstünde; sadece belirgin artışta (≥%30 ve mutlak eşik). Origin etiketi: "Your code" / "Your dependency" / "Not your code · downstream". Cümle + `PathCrumbs` + etkilenen KPI'lar; tıkla → `RcaDrawer` |
| `RcaChain` | Dikey stepper: her düğümde önce → şimdi, çarpan, "explains %" barı, kök düğümde badge + kanıtlar |
| `PathCrumbs` | Tek satır yol özeti (liste/incident satırlarında "Why:" bloğu) |
| `KpiStrip` | Client app servis sayfasında app'in KPI kartları; bozuksa kök neden cümlesi |

Yeni komponent: `components/ui/` altına ekle, `index.ts`'ten export et, `/design-system` sayfasına örnek koy, bu tabloyu güncelle.

## Data viz kuralları

Grafikler `components/charts` altında, saf SVG: `TimeSeries`, `Columns` (stacked), `Sparkline`, `BarList`, `PropBar`, `Legend`, `PartitionBars`.
`PartitionBars`: partition başına lag kolonları (tek seri, tek renk); en yüksek kolon direkt etiketli, hover'da değer. Amaç skew'i göstermek (bir uzun kolon = hot partition).
`TimeSeries` x etiketi varsayılan saat; 5 dk'dan kısa aralıkta saniyeli; göreli zaman için `formatX` (ör. planlanan yük `+2:00`). Nokta sayısı tick sayısından azsa etiket tekrar basılmaz.

- **Zaman aralıkları (topbar):** `15m` (15×1 dk), `30m` (30×1 dk), `1h` (60×1 dk), `6h` (72×5 dk), `24h` (96×15 dk), `7d` (84×2 sa) — `RANGE_POINTS` (`data/mock.ts`); api `timerange` paketi aynı bucket'ları döndürmeli, yoksa UI mock timestamp'lerine düşer.
- **Form seçimi:** zaman → `TimeSeries` (line, tek seri ise opsiyonel `area`); zaman içinde parça-bütün → `Columns`; sıralı karşılaştırma → `BarList`; tek oran → `PropBar`; tek sayı → `StatTile` (grafik değil).
- **Kategorik renk:** `--series-1..8`, sabit sıra (blue, orange, aqua, yellow, magenta, green, violet, red). Renk **entity'ye** bağlı, sıraya değil — servisler için `serviceSlot(id)` kullan. 8'den fazla seri → "Other" veya small multiples, asla üretilmiş renk.
- **Scatter/çok-çiftli formlar:** en fazla ilk 3 slot.
- **Çok çizgili grafiklerde ve endpoint stack'lerinde** `LINE_SLOTS` (`1,2,3,7,5,6,8`) sırası kullanılır — sarı (4) atlanır, çünkü turuncu↔sarı çizgiler özellikle dark modda ayırt edilemiyor. Sıra validator'dan geçti (light'ta kırmızı↔yeşil CVD ΔE 7.2: stack'lerde 2px boşluk + legend + tooltip ile telafi edilir). Servis renkleri `serviceSlot(id)`, endpoint renkleri servis içindeki sabit sıraya göre.
- **Ölçekler çok farklıysa small multiples.** Ör. Overview'da her servis kendi kartında, kendi ölçeğinde trafik + error rate (ayrı iki grafik). Tek grafikte büyük seri küçükleri sıfıra yapıştırıyorsa ayır.
- **Tablo satırında trend:** `Sparkline` (her satır kendi ölçeği), değer kolonunun yanında.
- **Tek y-ekseni.** Dual-axis yasak; farklı ölçekler ayrı grafik (ör. load test: VU, RPS, p95 ayrı kartlar).
- **Mark spec:** çizgi 2px, marker r=4 + 2px yüzey halkası, bar ≤ 24px kalınlık, 4px yuvarlak veri ucu / kare taban, stacked segmentler arası 2px boşluk, area %10 opaklık, grid 1px solid.
- **Legend:** ≥ 2 seride her zaman; tek seride yok (başlık söyler).
- **Metin seri rengi giymez.** Değer/etiket ink token'larında; kimlik yanındaki renkli işaretten gelir.
- **Hover default:** line/area → crosshair + tooltip; columns → kolon bazlı tooltip.
- **Eşik çizgisi:** `--status-critical` kesikli + etiket (`threshold` prop). Deploy/olay: `events` prop (`{ ts, label, detail? }` — dikey kesikli çizgi + etiket; `detail` hover tooltip'inde, ör. deploy commit mesajı · yazar · kaynak). Deploy marker'ları `deployEvents(service, from)` ile verilir.
- **Time spent kategorileri** (`TIME_CATEGORIES`): App code 1, Database 2, Cache 3, External HTTP 7, Queue publish 5, AI / LLM 6 (dep_kind `ai`, ADR-023).
- **FlameGraph** (`components/charts/FlameGraph.tsx`, ADR-022): icicle (kök üstte), DOM blokları, satır 20px. Renk paket/modül başına sabit
  (`framePackage()` → hash → `--series-1..8`, metin okunsun diye `color-mix` ile %32 tint; `all` / `[pruned]` / `(root)` gibi özel frame'ler
  `--bg-sunken`). Tıkla → zoom (atalar üstte soluk tam genişlik satır), breadcrumb + Reset, arama (eşleşmeyen soluk, eşleşen `--accent-warm`
  çerçeve, "x% of total matches"), hover tooltip: total / self değeri + toplam payı (+ zoom'lu düğüm payı). Diff modu (`baseTotal` + düğümlerde
  `baseValue`): renk = toplamdaki pay farkı, `--diff-*` çifti, `FlameDiffLegend` ile. Snapshot tipleri (heap, goroutine) profil başına gösterilir.
- **Status renkleri seri olarak kullanılmaz.** 5xx/error serisi için `--series-8` (red) kullanılır, `--status-critical` değil.
- Durum eşikleri (Web Vitals vb.) tabloda `StatusBadge` ile gösterilir; iyi değer düz sayı.

## Ekran kalıpları

- **Liste ekranı:** PageHeader → filtre satırı (Segmented + Search) → `Card flush` içinde `Table`.
- **Detay ekranı:** breadcrumb'lı PageHeader (sağda en fazla 1 primary) → 4'lü StatTile → 2–3'lü grafik grid → Tabs → tablo/list.
- **Öneri kartı (kod kaynaklı):** başlık + kritiklik + tip badge'leri → hedef → koşul (mono) → gerekçe → `dosya:satır · confidence` → Dismiss / Edit / Accept.
- Kod kaynaklı şeyler `Sparkles`/`Code2` ikonu + `accent` badge ile işaretlenir.

## Sayfa kompozisyonu (zorunlu)

1. **Önem sırası = dikey sıra = alan.** Her sayfa `Section`'lardan oluşan bir yığındır:
   1. *Şu an ne bozuk* (firing incident, failing dependency, running test) — varsa en üstte, yoksa hiç render edilmez.
   2. *Ana metrikler* — `MetricCard` ile, büyük; en önemli olan `span-2` / `metric--hero`.
   3. *Kırılımlar* — ana liste/tablo (endpoint'ler, sayfalar, monitor'ler).
   4. *İkincil bilgi* — küçük kartlar, en altta.
2. **Bir bilgi bir kez.** Aynı metrik sayfada iki yerde görünmez: `StatTile` + ayrı grafik kartı yasak → `MetricCard` (değer + delta + grafik tek kartta). Tablo kolonu ile yanındaki liste aynı sayıyı tekrar etmez.
3. **Özet sayfada, detay tıklayınca.** Liste satırı / kart tıklanınca `Drawer` (sağ panel) açılır: zaman grafikleri, kırılımlar, ilişkili kayıtlar orada. Ayrı tam sayfası olan varlıklar (servis, endpoint, run) için drawer başlığında "Open" aksiyonu.
4. Tıklanabilir satır/kart görsel ipucu taşır (`card--interactive`, satır hover, chevron).
5. Sayfa bölümleri arası boşluk `--space-6`, bölüm içi `--space-3/4`.

| Komponent | Kullanım |
|---|---|
| `Section` | Başlık + alt başlık + aksiyon; ilgili kartları gruplar |
| `MetricCard` | Değer + delta + hint + grafik. `className="metric--hero span-2"` ana metrik için |
| `Delta` | Tek başına delta etiketi |
| `Drawer`, `useDrawer`, `DrawerSection`, `KeyValues` | Tıklayınca detay paneli |
