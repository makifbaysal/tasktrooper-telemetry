# .ai — Proje bilgi tabanı

Agent'lar ve geliştiriciler için kaynak doküman seti. Giriş noktası: [`../AGENTS.md`](../AGENTS.md).

## Okuma sırası

1. [product.md](product.md) — ne yapıyoruz, kim için
2. [architecture.md](architecture.md) — nasıl yapıyoruz
3. Konuya göre: [sdk.md](sdk.md), [data-model.md](data-model.md), [alerting.md](alerting.md), [code-intelligence.md](code-intelligence.md), [load-testing.md](load-testing.md), [mcp.md](mcp.md)
4. UI işi: [design-system.md](design-system.md)
5. Neden böyle: [decisions.md](decisions.md) · Sırada ne var: [roadmap.md](roadmap.md)

## Sözlük

| Terim | Anlam |
|---|---|
| **Org** | Müşteri şirket (tenant). Faturalama ve kullanıcı sınırı. |
| **Project** | Org altında bir ürün/sistem. API key'ler proje bazlıdır. |
| **Service** | SDK'nın raporladığı tek bir deploy birimi (`service.name`). Tipi: `http`, `worker`, `consumer`, `web`, `mobile`. |
| **Environment** | `production`, `staging` vb. (`deployment.environment`). |
| **Endpoint / Operation** | HTTP route template (`GET /orders/:id`), gRPC metodu veya consumer topic+handler. |
| **RED** | Rate (throughput), Errors (error rate), Duration (latency). Backend için temel metrik seti. |
| **RUM** | Real User Monitoring. Gerçek kullanıcı oturumları, sayfa görüntülemeleri, Web Vitals. |
| **View** | Frontend'de sayfa (web) veya ekran (mobil). |
| **Session** | Bir kullanıcının kesintisiz kullanım periyodu (30 dk inaktivite → yeni session). |
| **Inventory** | Kod analizinden çıkan endpoint/consumer/view/dış bağımlılık listesi. |
| **Monitor** | Bir alert kuralı. Kaynak: `manual`, `suggested` (kod analizi), `template`. |
| **Incident** | Tetiklenmiş ve henüz kapanmamış alert durumu. |
| **Load test run** | Bir senaryonun belirli bir hedefe karşı tek koşusu. |
