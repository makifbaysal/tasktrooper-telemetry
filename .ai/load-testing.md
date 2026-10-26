# Load testing

## Hedef

Platform içinden load test tanımla, koştur, sonucu **aynı ekranda** hedef servisin gerçek telemetrisi (CPU, p95, error rate, DB span'ları) ile gör.

## Bileşenler

- **`loadtest-ctl`**: senaryo CRUD, run kuyruğu, runner'lara iş dağıtımı, eşik değerlendirme, sonuç özeti.
- **`loadtest-runner`**: k6 Go kütüphanesi gömülü binary. K8s Job olarak birden çok bölgede (eu-central, us-east, ...) çalışır. Metrikleri OTLP ile kendi ingest'imize yollar (`service = loadtest:<run_id>`).
- **Korelasyon**: runner her isteğe `traceparent` + `x-trooper-loadtest-run: <run_id>` header'ı ekler → hedef serviste SDK bu span'ları etiketler; UI run sırasında yalnızca test trafiğini veya tüm trafiği ayırabilir.

## Senaryo formatı

İki mod:
1. **Declarative (YAML/UI builder)** — çoğu kullanıcı için:
```yaml
name: Checkout flow
target: https://staging.shop.com
stages:
  - { duration: 2m, target: 50 }     # ramp-up (VU)
  - { duration: 5m, target: 200 }
  - { duration: 1m, target: 0 }
regions: [eu-central: 70, us-east: 30]
steps:
  - request: { method: GET, url: /api/products }
  - request: { method: POST, url: /api/cart, json: { productId: '{{random.product}}' } }
  - think: 1s
  - request: { method: POST, url: /api/checkout }
    checks: [status == 200]
thresholds:
  http_req_failed: rate < 0.01
  http_req_duration: p(95) < 500
data: { products: csv://products.csv }
```
2. **Script** — ham k6 JavaScript (ileri kullanıcı).

Declarative senaryo, runner'da k6 script'ine derlenir.

## Senaryo üretimi

- **Envanterden:** codeintel'in çıkardığı endpoint'ler + `loadtest_hint` (payload şeması) → seçilen endpoint'ler için taslak.
- **Gerçek trafikten:** son 7 günün route dağılımı ve RUM akışları (sayfa geçiş olasılıkları) → gerçekçi trafik karışımı. "Production trafiğinin 3 katı" gibi hedefler.
- Üretilen senaryo her zaman `draft`; kullanıcı onayı olmadan koşmaz.

## Güvenlik

- Hedef domain doğrulaması zorunlu (DNS TXT kaydı veya `/.well-known/trooper-verify` dosyası) — üçüncü taraflara yük testi atılamaz.
- Plan bazlı limit: max VU, max süre, aylık VU-dakika.
- Run sırasında acil durdurma; eşik ihlalinde (ör. error rate > %20) otomatik abort opsiyonu.

## Run UI

Canlı: VU sayısı, RPS, p95, error rate (runner tarafı) + hedef servisin RED ve kaynak metrikleri (SDK tarafı), aynı zaman ekseninde ama **ayrı grafiklerde** (dual-axis yasak). Bitince: eşik sonuçları, en yavaş endpoint'ler, önceki run ile karşılaştırma.
