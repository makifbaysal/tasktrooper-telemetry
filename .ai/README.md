# .ai — Project knowledge base

Source document set for agents and developers. Entry point: [`../AGENTS.md`](../AGENTS.md).

## Reading order

1. [product.md](product.md) — what we build, and for whom
2. [architecture.md](architecture.md) — how we build it
3. By topic: [sdk.md](sdk.md), [data-model.md](data-model.md), [alerting.md](alerting.md), [code-intelligence.md](code-intelligence.md), [load-testing.md](load-testing.md), [mcp.md](mcp.md) (+ Agent Skills: [`../skills/`](../skills/), setup in mcp.md § Agent Skills)
4. UI work: [design-system.md](design-system.md)
5. Why it is this way: [decisions.md](decisions.md) · What's next: [roadmap.md](roadmap.md)

## Glossary

| Term | Meaning |
|---|---|
| **Org** | Customer company (tenant). Billing and user boundary. |
| **Project** | A product/system under an org. API keys are per project. |
| **Service** | A single deploy unit reported by the SDK (`service.name`). Type: `http`, `worker`, `consumer`, `web`, `mobile`. |
| **Environment** | `production`, `staging` etc. (`deployment.environment`). |
| **Endpoint / Operation** | HTTP route template (`GET /orders/:id`), gRPC method, or consumer topic+handler. |
| **RED** | Rate (throughput), Errors (error rate), Duration (latency). The core metric set for backends. |
| **RUM** | Real User Monitoring. Real user sessions, page views, Web Vitals. |
| **View** | A page (web) or screen (mobile) in the frontend. |
| **Session** | A user's uninterrupted usage period (30 min inactivity → new session). |
| **Inventory** | The endpoint/consumer/view/external dependency list produced by code analysis. |
| **Monitor** | An alert rule. Source: `manual`, `suggested` (code analysis), `template`. |
| **Incident** | An alert state that has fired and is not yet closed. |
| **Load test run** | A single run of a scenario against a given target. |
