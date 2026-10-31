# MCP sunucusu (`services/mcp`, ADR-025)

Yerel kodlama agent'ları (OpenCode, Claude Code, Cursor …) için **salt okunur** Model Context Protocol
sunucusu. `services/api`'yi (`openapi.yaml`) sarar; ClickHouse/Postgres'e **dokunmaz**. ADR-025, sözleşme: contracts.md §26.

## Mimari

- Yeni Go modülü `services/mcp` (`cmd/tt-mcp`), `services/go.work`'e eklenir → CI modül matrisi otomatik genişler (ADR-026).
- Resmi Go MCP SDK (`github.com/modelcontextprotocol/go-sdk`); transport **stdio**. Sunucu bir process olarak
  agent tarafından başlatılır, tool çağrısı = JSON-RPC stdin/stdout.
- Uzatma hedefi (v1'de tasarlanmıyor): streamable-HTTP transport → uzak agent'ların paylaşılan bir sunucuya
  bağlanması. v1 stdio ile sınırlı; tool yüzeyi transport'tan bağımsız tasarlanır.
- API client: `net/http`, tek bir `apiBaseURL` + `Authorization: Bearer <TT_API_KEY>`; API'deki tüm tenant
  filtrelemesi (org/project) anahtarın kendisinden gelir (ADR-016, identity.go: api key → `(org, project)`,
  role `admin`).

## Yapılandırma (`services/pkg/config` ile TT_* env)

| Env | Zorunlu | Anlam |
|---|---|---|
| `TT_API_URL` | ✓ | API base (default `http://localhost:8080`) — collector ile aynı ad |
| `TT_API_KEY` | ✓ | Proje kapsamli api key (dev: `tt_dev_api_key`) |
| `TT_ORG_ID`, `TT_PROJECT_ID` | | Startup'ta `GET /me` ile doğrulanır; eşleşmezse sunucu **başlamaz** (yanlış anahtarla yanlış projeye okuma). API key auth'ta tenant anahtardan gelir, istek başlığından değil |
| `TT_LOG_LEVEL` | | `logx` (JSON slog) |

**Anahtar gizliliği:** `TT_API_KEY` sadece outbound `Authorization` header'ına yazılır. Tool tanımına,
tool çıktısına, hata mesajına ve log'a **asla** girmez; logx middleware'i `Authorization` header'ını loglamaz.

## Kapsam: v1 salt okunur

Write/akcióll tool **yok** (monitor create, incident ack/resolve, load test başlatma, repo analizi tetikleme,
hata status change dahil). Agent'lar okur, kullanıcı UI'dan veya CLI ile yazar. v2'de write tool'lar ayrı
ADR ile, açık kullanıcı onayı (ör. confirm parametresi) ile gelir.

## Tool yüzeyi (12 tool)

İsimler `tt_` önekli; tanımlar LLM'in tool **seçmesi** için yazılır (ne zaman çağrılan, ne zaman çağrılmayan).
Hepsi `{ range?, env? }` kabul eder (default `1h`, enum `15m|30m|1h|6h|24h|7d` — contracts §8); `env` verilmezse
tüm ortamlar. Sonuç şekilleri contracts.md'deki schema'lardır (api ile birebir passthrough).

| # | Tool | Sarılan uç (operationId) |
|---|---|---|
| 1 | `tt_list_services` | `GET /api/v1/services` (`getServices`) |
| 2 | `tt_get_service_kpis` | `GET /api/v1/services/{service}` (`getServicesService`) |
| 3 | `tt_list_endpoints` | `GET /api/v1/services/{service}/endpoints` (`getServicesServiceEndpoints`) |
| 4 | `tt_endpoint_rca` | `GET /api/v1/services/{service}/endpoints/{endpoint}/rca` (`getServicesServiceEndpointsEndpointRca`) |
| 5 | `tt_search_traces` | `GET /api/v1/traces` (`getTraces`) |
| 6 | `tt_get_trace` | `GET /api/v1/traces/{traceId}` (`getTracesTraceId`) |
| 7 | `tt_search_logs` | `GET /api/v1/logs` (`getLogs`) |
| 8 | `tt_list_monitors` | `GET /api/v1/monitors` (`getMonitors`) |
| 9 | `tt_list_incidents` | `GET /api/v1/incidents` (`getIncidents`) |
| 10 | `tt_get_incident` | `GET /api/v1/incidents/{id}` (`getIncidentsId`) + `GET /api/v1/incidents/{id}/events` (`getIncidentsIdEvents`) |
| 11 | `tt_list_loadtest_runs` | `GET /api/v1/loadtests/runs` (`getLoadtestsRuns`) |
| 12 | `tt_list_deploys` | `GET /api/v1/deploys` (`getDeploys`) |

**Tool detayları** (isim → LLM tanımı → input → sonuç):

1. **`tt_list_services`** — "List services in the project with throughput, error rate and latency
   summary; start here to discover service names." Input: `{ range?, env?, type? }` (`type` = `http|worker|web|mobile`,
   virgül listesi). Sonuç: `Service[]` (contracts §8; RED özeti + sparkline).
2. **`tt_get_service_kpis`** — "Health, RED metrics, dependencies and version info of ONE service;
   use after tt_list_services, not for discovery." Input: `{ service: string, range?, env? }`.
   Sonuç: `ServiceDetail` (+ `hasAi`).
3. **`tt_list_endpoints`** — "Endpoints (routes, consumers) of one service with error rate and p95.
   Returns each endpoint's id (slug, for tt_endpoint_rca) and method + route ("POST /v1/charges", for the
   endpoint filter of tt_search_traces, tt_list_incidents, tt_list_monitors)." Input: `{ service: string, range?, env? }`.
   Sonuç: `Endpoint[]` (id = slug, ör. `get-orders-id`; `method` + `route`).
4. **`tt_endpoint_rca`** — "Explain WHY one endpoint got slower or errorier: per-dependency delta vs a
   baseline window (root-cause attribution). Requires an endpoint id from tt_list_endpoints." Input:
   `{ service, endpoint, range?, baseline?: prev|1d|7d }` (default `prev`). Sonuç: `EndpointRca` (ADR-009).
5. **`tt_search_traces`** — "Find recent traces by service/endpoint/duration/error; returns trace ids
   for tt_get_trace." Input: `{ range?, env?, service?, endpoint? ("METHOD /route" veya process adı — slug değil),
   errorsOnly?: bool, minMs?: number,
   sort?: string, limit? }` (limit default 20, max 100). Sonuç: `TraceSummary[]`.
6. **`tt_get_trace`** — "Full span tree of ONE trace (waterfall, attributes, dependencies). Use a trace
   id from tt_search_traces or an error issue." Input: `{ traceId: string }` (32 hex; format doğrulanır).
   Sonuç: `Trace` (span sayısı > 200 ise span listesi kesilir + `truncated`/`total`/`hint`, kural 3).
7. **`tt_search_logs`** — "Search log lines (newest first) by query text, level, service, route or trace
   id; cursor-paginate with `before`/`nextBefore`." Input: `{ range?, env?, service?, level?, q?, traceId?,
   route?, before?, limit? }` (limit default 50, max 200). Sonuç: `LogPage { lines, nextBefore, ... }`.
8. **`tt_list_monitors`** — "Alert rules (monitors) with kind, query and state; use to find what is
   alerting or what is configured for a service." Input: `{ service?, endpoint? ("METHOD /route"), kind?, clusterId?, group? }`.
   Sonuç: `Monitor[]`.
9. **`tt_list_incidents`** — "Open or recent incidents with severity and current value; start here for
   'what is broken right now'." Input: `{ state?, service?, endpoint? ("METHOD /route"), monitorId?, kpiId?, since?, limit? }`
   (limit default 20, max 100). Sonuç: `Incident[]`.
10. **`tt_get_incident`** — "Everything about ONE incident: alert detail, root cause and timeline.
    rootCause is unstructured and its shape may vary; treat it as opaque JSON." Input: `{ id: string }`.
    Sonuç: `{ incident: Incident (+ rootCause), events: IncidentEvent[] }` (rootCause şemasız —
    bkz. "Bilinen bayraklar").
11. **`tt_list_loadtest_runs`** — "Load test runs with state, thresholds and summary; use to check a
    scenario's latest result, not to start tests." Input: `{ scenarioId? }`. Sonuç: `LoadTestRun[]`.
12. **`tt_list_deploys`** — "Deploy markers (newest versions first seen, Vercel/CI markers); use to
    correlate an incident or latency change with a deployment." Input: `{ range?, env?, service?, source? }`.
    Sonuç: `DeployMarker[]`.

**Bilinçli olarak dışarıda bırakılanlar** (v2 adayları, ayrı tool sayısı/kardinalite gerekçesiyle):
`tt_list_errors`/`tt_get_error` (`getErrors`, `getErrorsFingerprint`), SLO KPI listesi (`getKpis`),
RUM (`/apps/*`), infra/K8s (`/infra/*`), profiling (`/profiles/*`), AI monitoring (`/ai/*`), codeintel
verisi (`/inventory`, `/suggestions` — bkz. aşağıdaki örtüşme notu).

## Kod intelligence ile örtüşme

`services/codeintel` (:8096) **sadece webhook + analiz** servisidir; sorgu API'si yoktur. Onun verisi
(envanter, öneri, repo durumu) zaten `services/api` uçlarında (`GET /repos`, `GET /inventory`,
`GET /suggestions`) okunur. Kural: MCP tool'ları codeintel'e **doğrudan** bağlanmaz; v2'de envanter/
öneri tool'ları eklenirse yine api üzerinden ve salt okunur (`POST /repos/{id}/analyze`,
`suggestions/{id}/accept|reject` gibi write'lar asla sarılmaz) tasarlanır.

## Kesişen kurallar (her tool için)

1. **Tenant (AGENTS.md kural 7):** org/project anahtardan gelir; MCP ek olarak startup'ta
   `TT_ORG_ID`/`TT_PROJECT_ID` ↔ `/me` eşleşmesini zorlar. Hiçbir tool org/project parametresi almaz.
2. **Input doğrulama:** enum'lar (`range`, `baseline`, `type`, `level`), hex trace id, sayısal `limit`/`minMs`
   cap'leri; yanlış input → tool hatası (API'ye gitmeden).
3. **Sonuç cap'i:** tool çıktısı en fazla **64 KB** JSON (trace > 200 span ayrıca kesilir). Aşarsa dizi
   kesilir ve sonuca `truncated: true`, kesilen kaynak toplamı `total` ve agent'ın sorguyu nasıl daraltacağına
   dair `hint` eklenir. Listelerde `limit` clamp edilir: trace ≤ 100 (default 20), log ≤ 200 (default 50),
   incident ≤ 100 (default 20); log'lar `nextBefore` cursor'ı ile sayfalanır.
4. **PII:** MCP ek veri toplamaz; header/body toplayan tool yok. Dönen log mesajı ve trace attribute
   değerleri `internal/redact` ile maskelenir (e-posta, IPv4/IPv6, `tt_api_`/`tt_ingest_`/`tt_dev_` ile
   başlayan anahtarlar — tool adları (`tt_list_*`) kasıtlı olarak match edilmez — `Authorization: Bearer`
   değeri, 16 haneli kart benzeri sayı). `q` filtresi ve header'lar log'a yazılmaz. Maskeler dashboard'daki
   SDK maskeleme değerleriyle **çakışmaz**: API'nin toplamadığı veriyi MCP de toplamaz, sadece gelen
   yüzeyde maskeler.
5. **Temiz hatalar:** tool hatası kısa ve aksiyon odaklıdır; upstream gövdesi, stack trace,
   `Authorization` değeri ve iç URL asla taşınmaz. 401/403 → "check TT_API_KEY", 404 (geçerli kaynak
   id'si gerektiren tool'larda) → "call tt_list_endpoints for valid endpoint ids", 5xx/timeout → API
   kullanılamıyor. Başlangıçtaki `/me` kontrolü (config) en fazla 512 karakter gövde gösterebilir — bu
   stderr'e yazılır, tool çıktısı değildir.

## Bağlanma

Binary: `cd services/mcp && go install ./cmd/tt-mcp` (`$(go env GOPATH)/bin` PATH'te olmalı; değilse aşağıdaki
`command`'a mutlak yol yazılır). API ayakta olmalı (`make demo` veya compose `--profile app`): `tt-mcp`
startup'ta `GET /me` çağırır, API yoksa başlamaz.

**OpenCode** (`~/.config/opencode/opencode.json` veya proje `opencode.json`):

```json
{
  "mcp": {
    "tasktrooper": {
      "type": "local",
      "command": ["tt-mcp"],
      "environment": {
        "TT_API_URL": "http://localhost:8080",
        "TT_API_KEY": "tt_dev_api_key",
        "TT_ORG_ID": "1",
        "TT_PROJECT_ID": "1"
      },
      "enabled": true
    }
  }
}
```

**Claude Code** (CLI):

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=http://localhost:8080 -e TT_API_KEY=tt_dev_api_key \
  -e TT_ORG_ID=1 -e TT_PROJECT_ID=1 \
  -- tt-mcp
```

veya proje kökünde `.mcp.json`:

```json
{
  "mcpServers": {
    "tasktrooper": {
      "command": "tt-mcp",
      "env": {
        "TT_API_URL": "http://localhost:8080",
        "TT_API_KEY": "tt_dev_api_key",
        "TT_ORG_ID": "1",
        "TT_PROJECT_ID": "1"
      }
    }
  }
}
```

## Agent Skills (`skills/`)

Tool'ları **hangi sırayla** kullanacağını anlatan Agent Skills (`<name>/SKILL.md`, frontmatter `name` +
`description`). Kanonik yer repo kökündeki `skills/`; detay ve tetikleme tablosu: [`skills/README.md`](../skills/README.md).

| Skill | Ne zaman |
|---|---|
| `trooper-diagnose` | "X'te bir sorun var, neden?" — incident → KPI → endpoint RCA → trace → log → deploy; kanıtsız neden yok, zayıf kanıtta "unknown" |
| `trooper-query` | Telemetri sorusu; scope (`range`/`service`/`limit`), log sayfalama (`before`/`nextBefore`), `truncated` okuma |
| `trooper-contribute` | Bu repoda kod değişikliği; AGENTS.md + `.ai/` kurallarına işaret eden kontrol listesi |

**Keşif:** tek bağlantı commit'li `.claude/skills -> ../skills` symlink'i. Claude Code proje skill'lerini
`.claude/skills/`'ten okur; OpenCode (≥ 2.0) `.opencode/skill(s)/` yanında uyumluluk için `.claude/skills/` ve
`.agents/skills/`'i de tarar (symlink takip edilir). Kopya yok; symlink desteklenmeyen checkout'ta OpenCode için
`opencode.json` → `"skills": ["./skills"]`. Skill metni tool adlarını birebir (`tt_*`) kullanır; tool eklenir/
yeniden adlandırılırsa skill'ler aynı PR'da güncellenir.

## Bilinen bayraklar / boşluklar

- `POST /api/v1/ai/incident-summary` (`postAIIncidentSummary`) main'e **yeni** düştü (0764ad4, "offline mock
  integrations"); default sağlayıcı **mock** ve ücretli model çağrısı. v1'de sarılmıyor (POST + LLM kotası);
  gerçek sağlayıcı doğrulanınca v2 adayı.
- `Incident.rootCause` openapi'de `rootCause: {}` — **şeması belgelenmemiş**, passthrough opaque JSON.
- `endpoint` path paramı route değil **slug id** (`get-orders-id`); agent'ların önce `tt_list_endpoints`
  çağırması şart (tool tanımında böyle yazıyor).
- **İki farklı endpoint kimliği** (demo verisiyle doğrulandı, 2026-10-31): `tt_endpoint_rca`'nın `endpoint`'i
  (path param, `ResolveEndpoint`) **slug**'tır; `tt_search_traces`, `tt_list_incidents`, `tt_list_monitors`'ın
  `endpoint` filtresi (query param, api `refParam` / `i.endpoint = $?`) ise **`"METHOD /route"`** (consumer'da process
  adı). Slug bu filtrelerde hata değil **boş liste** döner — agent "olay yok" sanır. Tool input tanımları bunu açıkça
  söyler; `Incident.endpoint` de route biçimindedir (`POST /v1/orders`). API tek biçime çekilirse tanımlar güncellenir.
- Auth today: `Authorization: Bearer <api key>` (dev akışı). OIDC session akışına geçilirse MCP için anahtar
  kalır; `X-Trooper-Project` header'ı sadece cookie akışında anlamlı.
