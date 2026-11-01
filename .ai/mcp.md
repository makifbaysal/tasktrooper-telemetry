# MCP sunucusu (`services/mcp`, ADR-025)

Yerel kodlama agent'ları (OpenCode, Claude Code, Cursor …) için Model Context Protocol sunucusu: varsayılan **salt okunur**, `TT_MCP_ALLOW_WRITE` ile onaylı write tool'lar (ADR-032).
`services/api`'yi (`openapi.yaml`) sarar; ClickHouse/Postgres'e **dokunmaz**. ADR-025, sözleşme: contracts.md §26.

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
| `TT_API_KEY` | ✓ | Proje kapsamli api key (seed key yok, ADR-028: dashboard Settings → API keys veya `POST /api/v1/api-keys`) |
| `TT_ORG_ID`, `TT_PROJECT_ID` | | Startup'ta `GET /me` ile doğrulanır; eşleşmezse sunucu **başlamaz** (yanlış anahtarla yanlış projeye okuma). API key auth'ta tenant anahtardan gelir, istek başlığından değil |
| `TT_MCP_ALLOW_WRITE` | | `true` → 11 write tool kaydedilir (default `false`, ADR-032). API key zaten `admin` rolünde: bu bayrak agent'ın yazabilmesinin tek kapısıdır |
| `TT_LOG_LEVEL` | | `logx` (JSON slog) |

**Anahtar gizliliği:** `TT_API_KEY` sadece outbound `Authorization` header'ına yazılır. Tool tanımına,
tool çıktısına, hata mesajına ve log'a **asla** girmez; logx middleware'i `Authorization` header'ını loglamaz.

## Kapsam: varsayılan salt okunur, opt-in write (ADR-032)

Varsayılan kurulum **salt okunur**: 27 tool, hepsi GET (tek istisna `tt_preview_monitor`: POST ama hiçbir şey kaydetmez).
`TT_MCP_ALLOW_WRITE=true` ile 11 write tool daha kaydedilir (toplam 38). Write tool'ların kuralları:

- **Onay:** her write tool `confirm` parametresi alır. `confirm` yoksa / `false` ise API'ye yazılmaz; sonuç
  `{dryRun: true, wouldSend: {method, path, body}, next}` — agent bunu kullanıcıya gösterir, kullanıcı onaylayınca
  aynı çağrıyı `confirm: true` ile tekrarlar. Tek istisna `tt_stop_loadtest` (yükü yalnız azaltır, acil durdurma).
- **Kaynak:** agent'ın oluşturduğu monitor, KPI ve load test senaryosu `source: "agent"` taşır (migration
  `014_agent_source.sql`); UI'da "Agent" rozeti. Kullanıcının verdiği `source` yok sayılır.
- **Sahiplik:** `tt_delete_monitor` / `tt_delete_kpi` yalnız `source = agent` kayıtları siler; diğerleri reddedilir
  (kullanıcı UI'dan siler ya da agent mute eder). Repo'dan yönetilen (`source = code`) monitor/KPI güncellenmez.
- **Load test:** `tt_run_loadtest` gerçek trafik üretir; senaryo `draft` ise reddedilir, hedef doğrulanmış domain
  olmalı (domain ekleme/doğrulama kullanıcıda). Agent oluşturduğu senaryoya `gate` yoksa
  `{abortErrorRate: 0.5, abortP95Ms: 10000}` ekler.
- Hâlâ sarılmayanlar: incident ack/resolve, repo analizi tetikleme, hata status değişikliği, kanal/entegrasyon
  ekleme, domain ekleme, API key işlemleri.

## Tool yüzeyi (27 okuma + 11 write)

İsimler `tt_` önekli; tanımlar LLM'in tool **seçmesi** için yazılır (ne zaman çağrılan, ne zaman çağrılmayan).
Zaman penceresi alan tool'lar `{ range?, env? }` kabul eder (default `1h`, enum `15m|30m|1h|6h|24h|7d` — contracts §8);
`env` verilmezse tüm ortamlar. Sonuç şekilleri contracts.md'deki schema'lardır (api ile birebir passthrough).

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
| 13 | `tt_list_errors` | `GET /api/v1/errors` (`getErrors`) — mesajlar `redact`'ten geçer |
| 14 | `tt_get_error` | `GET /api/v1/errors/{fingerprint}` (`getErrorsFingerprint`) |
| 15 | `tt_list_kpis` | `GET /api/v1/kpis` (`getKpis`) |
| 16 | `tt_list_alert_suggestions` | `GET /api/v1/suggestions` (`getSuggestions`) |
| 17 | `tt_list_inventory` | `GET /api/v1/inventory` (`getInventory`) |
| 18 | `tt_list_monitor_templates` | `GET /api/v1/monitor-templates` |
| 19 | `tt_list_notify_channels` | `GET /api/v1/integrations/slack` + `/integrations/slack/channels` (`refresh`) + `GET /api/v1/notification-channels` (yalnız ad + tip; config asla) |
| 20 | `tt_get_app_summary` | `GET /api/v1/apps/{app}/summary` |
| 21 | `tt_get_ai_overview` | `GET /api/v1/ai/overview` |
| 22 | `tt_list_hosts` | `GET /api/v1/infra/hosts` |
| 23 | `tt_list_databases` | `GET /api/v1/infra/databases` |
| 24 | `tt_list_consumer_groups` | `GET /api/v1/infra/groups` |
| 25 | `tt_preview_monitor` | `POST /api/v1/monitors/preview` (kaydetmez; seri son 60 bucket'a kırpılır) |
| 26 | `tt_list_loadtest_scenarios` | `GET /api/v1/loadtests/scenarios` (spec çıkarılır) + `GET /api/v1/loadtests/domains` |
| 27 | `tt_get_loadtest_run` | `GET /api/v1/loadtests/runs/{id}` |

**Write tool'lar** (`TT_MCP_ALLOW_WRITE=true`; hepsi `confirm` ile, bkz. Kapsam):

| Tool | Sarılan uç | Not |
|---|---|---|
| `tt_create_monitor` | `POST /api/v1/monitors` | `source: agent`; `channels` zorunlu (boş liste = yalnız incident) |
| `tt_update_monitor` | `GET` + `PATCH /api/v1/monitors/{id}` | name, severity, channels, status, condition, mutedUntil; `source = code` reddedilir |
| `tt_delete_monitor` | `GET` + `DELETE /api/v1/monitors/{id}` | yalnız `source = agent` |
| `tt_accept_suggestion` | `POST /api/v1/suggestions/{id}/accept` | threshold / severity / channels override |
| `tt_create_kpi` | `POST /api/v1/kpis` | `source: agent`; endpoint `ref` = `"METHOD /route"`, latency SLI'da `good.thresholdMs` zorunlu |
| `tt_update_kpi` | `GET` + `PUT /api/v1/kpis/{id}` | tam KPI; mevcut `source` korunur, `code` reddedilir |
| `tt_delete_kpi` | `GET` + `DELETE /api/v1/kpis/{id}` | yalnız `source = agent` |
| `tt_send_slack_test` | `POST /api/v1/integrations/slack/test` | `slack:<id>#ad` hedefini id'ye indirger; 502 mesajı (bot davetli değil vb.) agent'a geçer |
| `tt_create_loadtest_scenario` | `POST /api/v1/loadtests/scenarios` | `source: agent`, `state: ready`; `target` http(s) URL, `steps` + `levels` (veya `script`) zorunlu |
| `tt_run_loadtest` | `GET` scenario + `POST /api/v1/loadtests/scenarios/{id}/runs` | dry run hedef / tepe VU / süreyi gösterir; draft reddedilir |
| `tt_stop_loadtest` | `POST /api/v1/loadtests/runs/{id}/stop` | confirm yok |

Write hataları: 400/409 → API'nin `error` mesajı (≤ 300 karakter) geçer ("the API rejected the input (HTTP 400): …");
501/502 → API'nin okunur nedeni (Slack App yapılandırılmamış, Slack mesajı reddetti); 5xx gövdesi asla geçmez; timeout
→ "may or may not have been applied: check with a list call before retrying".

**Tool detayları** (v1 tool'ları; isim → LLM tanımı → input → sonuç):

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
9. **`tt_list_incidents`** — "Open or recent incidents with severity, state (pending, firing, acknowledged,
   resolved) and current value; start here for 'what is broken right now'. Omit state first: pending incidents
   are often the live problem." Input: `{ state?, service?, endpoint? ("METHOD /route"), monitorId?, kpiId?, since?, limit? }`
   (limit default 20, max 100). Sonuç: `Incident[]`.
10. **`tt_get_incident`** — "Everything about ONE incident: alert detail, root cause and timeline.
    rootCause is unstructured and its shape may vary; treat it as opaque JSON." Input: `{ id: string }`.
    Sonuç: `{ incident: Incident (+ rootCause), events: IncidentEvent[] }` (rootCause şemasız —
    bkz. "Bilinen bayraklar").
11. **`tt_list_loadtest_runs`** — "Load test runs with state, thresholds and summary; use to check a
    scenario's latest result." Input: `{ scenarioId? }`. Sonuç: `LoadTestRun[]`.
12. **`tt_list_deploys`** — "Deploy markers (newest versions first seen, Vercel/CI markers); use to
    correlate an incident or latency change with a deployment." Input: `{ range?, env?, service?, source? }`.
    Sonuç: `DeployMarker[]`.

**Hâlâ dışarıda:** profiling (`/profiles/*`), session replay, `POST /api/v1/ai/incident-summary` (LLM kotası).

## Kod intelligence ile örtüşme

`services/codeintel` (:8096) **sadece webhook + analiz** servisidir; sorgu API'si yoktur. Onun verisi
(envanter, öneri, repo durumu) zaten `services/api` uçlarında (`GET /repos`, `GET /inventory`,
`GET /suggestions`) okunur. Kural: MCP tool'ları codeintel'e **doğrudan** bağlanmaz; envanter / öneri tool'ları api üzerinden okur. Tek write
`tt_accept_suggestion` (ADR-032, onaylı); `POST /repos/{id}/analyze` ve `suggestions/{id}/reject` sarılmaz.

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
   değerleri `internal/redact` ile maskelenir (e-posta, IPv4/IPv6, `tt_api_`/`tt_ingest_`/`tt_dev_` (eski dev key'leri) ile
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
`command`'a mutlak yol yazılır). API ayakta olmalı (compose `--profile app` veya servisleri lokal çalıştır) ve bir proje + api key bulunmalı: onboarding
(UI'da "org + proje oluştur", contracts §11) veya `api bootstrap --org <ad> --project <ad> --owner <email>`, ardından
dashboard Settings → API keys. Platformun kendi verisi için `make self` (ADR-027/028). `tt-mcp`
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
        "TT_API_KEY": "<api-key>",
        "TT_ORG_ID": "<org-id>",
        "TT_PROJECT_ID": "<project-id>"
      },
      "enabled": true
    }
  }
}
```

**Claude Code** (CLI):

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=http://localhost:8080 -e TT_API_KEY=<api-key> \
  -e TT_ORG_ID=<org-id> -e TT_PROJECT_ID=<project-id> \
  -- tt-mcp
```

Alert / KPI / load test tanımlatmak için write açık (ADR-032; agent yine her değişiklikte onay ister). Demo API'si
`https://telemetry-api.tasktrooper.ai`; key dashboard Settings → API keys'ten, kullanıcı kendisi yapıştırır:

```bash
claude mcp add tasktrooper \
  -e TT_API_URL=https://telemetry-api.tasktrooper.ai -e TT_API_KEY=<api-key> \
  -e TT_MCP_ALLOW_WRITE=true \
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
        "TT_API_KEY": "<api-key>",
        "TT_ORG_ID": "<org-id>",
        "TT_PROJECT_ID": "<project-id>"
      }
    }
  }
}
```

## Agent Skills (`skills/`)

Tool'ları **hangi sırayla** kullanacağını anlatan Agent Skills (`skills/<name>/SKILL.md`, frontmatter `name` +
`description`; description agent'ın skill'i **ne zaman** yükleyeceğini söyler). Kanonik yer repo kökündeki
`skills/`; skill'ler sadece orada düzenlenir.

| Skill | Ne zaman |
|---|---|
| [`trooper-diagnose`](../skills/trooper-diagnose/SKILL.md) | "X'te bir sorun var, neden?" — incident → KPI → endpoint RCA → trace → log → deploy; kanıtsız neden yok, zayıf kanıtta "unknown" |
| [`trooper-query`](../skills/trooper-query/SKILL.md) | Telemetri sorusu; scope (`range`/`service`/`limit`), log sayfalama (`before`/`nextBefore`), `truncated` okuma |
| [`trooper-alerts`](../skills/trooper-alerts/SKILL.md) | "Bu proje için alert / KPI kur" — envanter + mevcut monitor/KPI/öneri → veriden eşik → `tt_preview_monitor` → liste halinde onay → `confirm: true`; veri yetersizse load test önerir (onayla senaryo + run) |
| [`trooper-contribute`](../skills/trooper-contribute/SKILL.md) | Bu repoda kod değişikliği; AGENTS.md + `.ai/` kurallarına işaret eden kontrol listesi |

`trooper-diagnose`, `trooper-query` ve `trooper-alerts` yukarıdaki § Bağlanma ile kayıtlı `tasktrooper` MCP sunucusunu gerektirir. `trooper-alerts`
write adımları için `TT_MCP_ALLOW_WRITE=true` ister; kapalıysa önerileri liste olarak verir, kullanıcı UI'dan oluşturur.

**Keşif:** tek bağlantı commit'li `.claude/skills -> ../skills` symlink'i, kopya yok.
- Claude Code proje skill'lerini `.claude/skills/<name>/SKILL.md`'den okur.
- OpenCode (2.0.x) `.opencode/skill(s)/` yanında uyumluluk için `.claude/skills/` ve `.agents/skills/`'i de
  tarar (cwd'den yukarı, symlink takip edilir). Tarama asenkron: background service açıksa dosya değişikliği
  birkaç saniye sonra görünür.
- **`skills/` altına skill olmayan `.md` koyma:** OpenCode `{*.md,**/SKILL.md}` deseniyle tarar; ör. bir
  `skills/README.md` "README" adlı bir skill olarak listelenir. Bu yüzden kurulum notu burada, `skills/`'te değil.
- Symlink'siz checkout (`core.symlinks=false`): OpenCode için `opencode.json` → `"skills": ["./skills"]`; Claude
  Code için `rm .claude/skills && cp -r skills .claude/skills` (kopya commit'lenmez).
- Kontrol: `claude -p "list skills starting with trooper-"`; OpenCode'da `/skills` komutu veya
  `opencode api skill.list --param "location[directory]=$PWD"`.

Skill metni tool adlarını birebir (`tt_*`) kullanır; tool eklenir/yeniden adlandırılırsa skill'ler aynı PR'da
güncellenir.

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
- **Doğrulama ↔ API uyumsuzlukları** (demo ile doğrulandı, 2026-10-31; kod düzeltmesi açık iş, şimdilik tool tanımları
  çalışan değerlere yönlendirir): `tt_list_incidents.state` MCP'de `firing|resolved|acked` ile doğrulanıyor, API ise
  `open|pending|firing|acknowledged|resolved` bekliyor → `acked` API'den 400 döner, `pending`/`open`/`acknowledged`
  MCP'de reddedilir. `tt_search_logs.level` API'de **tam eşleşme** (virgül listesi `warn,error` kabul eder), "minimum"
  değil; MCP tek değer kabul ediyor (virgül listesi reddedilir).
- Auth today: `Authorization: Bearer <api key>` (dev akışı). OIDC session akışına geçilirse MCP için anahtar
  kalır; `X-Trooper-Project` header'ı sadece cookie akışında anlamlı.
