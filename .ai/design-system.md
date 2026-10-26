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
| `Table` | Kolon tanımlı, `numeric` sağa hizalı tabular, `sortValue` ile sıralanabilir, `onRowClick` |
| `Tabs` / `Segmented` | Tabs = sayfa içi bölüm; Segmented = filtre/zaman aralığı |
| `Field`, `Input`, `Textarea`, `SearchInput`, `Select`, `Toggle` | Form elemanları (`Textarea mono` JSON/header şablonları için) |
| `PageHeader` | breadcrumb + başlık + alt başlık + aksiyonlar |
| `EmptyState`, `Meter`, `CodeBlock`, `Callout` | Yardımcılar |
| `ChoiceChips` | Çoklu seçim (region, servis, drain tipi): `Segmented` görünümü, birden çok `aria-pressed`, dar ekranda satır kırar |
| `CopyCode` | Kopyala butonlu `CodeBlock` (IAM policy, CloudFormation/Terraform, drain URL); `wrap` tek satırlık uzun değeri (URL, ARN) kırar |

Entegrasyon kalıpları (`screens/Integrations.tsx`): `ProviderMark` (sağlayıcı kimliği — marka rengi/logosu yok, nötr kutu + kısaltma),
`IntegrationStatusBadge` (ok → Connected, error → Error, pending → Syncing…, disabled → Disabled; hep ikon + etiket),
`IntegrationSourceBadge` (keşfedilen DB/cluster satırında "AWS · aws-prod", tıkla → entegrasyon çekmecesi; sync hatalıysa critical ton),
`DeployList` (deploy marker listesi: versiyon, commit mesajı, yazar, kaynak, link, build durumu).

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
