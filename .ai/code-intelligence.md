# Code intelligence

Müşterinin kod tabanını okuyup **envanter** çıkaran ve bu envanterden alert + load test önerileri üreten servis (`codeintel`).

## Bağlantı

- GitHub App / GitLab App kurulumu → repo seçimi.
- Monorepo desteği: `.trooper/config.yaml` ile `path → service.name` eşlemesi; yoksa SDK `init({ service })` çağrılarından otomatik tespit.
- Tetikleyiciler: ilk kurulum (full scan), default branch'e push (incremental, sadece diff), manuel "yeniden analiz".

## Pipeline

```
clone (shallow, sparse) → dil/framework tespiti → AST parse (tree-sitter)
  → extractor'lar → ham envanter
  → LLM zenginleştirme (kritiklik, açıklama, gruplama)
  → gözlem verisi ile eşleştirme (endpoints.source = code|observed|both)
  → öneri üretimi (alert, load test senaryosu)
  → kaynak kod silinir, envanter saklanır
```

## Extractor'lar (framework bazlı, deterministik)

| Dil | Framework | Ne çıkarılır |
|---|---|---|
| TS/JS | express, fastify, nest, koa, hono, Next.js (app/pages router) | route + method + handler dosya:satır, middleware zinciri |
| TS/JS | kafkajs, bullmq, amqplib, SQS | topic/queue + handler |
| TS/JS | react-router, Next.js, react-navigation | view/sayfa listesi |
| Go | net/http, chi, gin, echo, grpc | route/method, handler |
| Go | sarama, franz-go, asynq | consumer |
| Python | django, fastapi, flask, celery | route, task |
| Hepsi | cron tanımları (node-cron, k8s CronJob yaml, celery beat) | schedule |
| Hepsi | HTTP client çağrıları, DB driver kullanımı | dış bağımlılık |

Extractor'lar deterministik kalır; LLM **çıkarma** için değil, **yorumlama** için kullanılır (halüsinasyon riski düşük tutulur).

## LLM kullanımı

- Model: Claude (model ID config'ten). Çağrılar batch'lenir, prompt caching kullanılır (repo özeti sabit prefix).
- Girdi: envanter öğesi + handler fonksiyonu kaynağı (token limitli) + çağırdığı fonksiyonların imzaları.
- Çıktı (structured JSON): `criticality`, `business_domain`, `summary`, `failure_modes[]`, `suggested_monitors[]` (şablon id + gerekçe), `loadtest_hint` (örnek payload şeması, idempotent mi).
- Sonuçlar commit sha + dosya hash ile cache'lenir; değişmeyen handler tekrar analiz edilmez.
- Kaynak kod LLM sağlayıcısında saklanmaz (zero data retention ayarı); müşteri isterse LLM adımı kapatılabilir → sadece statik kurallar.

## Çıktılar

- **Envanter** (Code insights ekranı): endpoint/consumer/view/cron/bağımlılık listesi, kod linki, gözlemlendi mi, monitor'ü var mı.
- **Ölü kod sinyali:** kodda var ama 30 gündür trafik yok.
- **Gölge endpoint:** trafik var ama kodda bulunamadı (proxy, eski versiyon).
- **Alert önerileri** → [alerting.md](alerting.md#kod-farkındalıklı-otomatik-alert)
- **Load test senaryo taslağı** → [load-testing.md](load-testing.md#senaryo-üretimi)
