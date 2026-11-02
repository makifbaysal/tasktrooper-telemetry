# AI: local development to production

## Run the dashboard without credits

From `services/api`, start the offline playground:

```powershell
go run ./cmd/ai-playground
```

In a second terminal, from `apps/web`:

```powershell
pnpm install
pnpm dev
```

Open `http://localhost:5173`. Vite proxies `/local-ai/*` to the playground on `127.0.0.1:8099`. The contextual buttons in Incidents and Code Insights **always** use this offline mock. The Overview "Ask telemetry" drawer is gone: free-text questions go to the Assistant screen (`/assistant`, ADR-033), which runs in the authenticated api — offline mock by default, an OpenAI-compatible model (Meta Muse Spark) with `TT_ASSISTANT_API_KEY` + `TT_ASSISTANT_MODEL` (+ `TT_ASSISTANT_API_URL`) (see `.ai/assistant.md`). The playground explicitly constructs `ai.NewMock()` and ignores the model environment variables. Its actions show an estimated 0 credits and unlimited mock allowance; this is not an estimate of a future paid call. Check `http://127.0.0.1:8099/healthz` if a button cannot reach it.

To test the authenticated API's incident-summary implementation without a database, run `go run ./cmd/ai-demo` from `services/api` with `TT_ASSISTANT_API_KEY` or `TT_ASSISTANT_MODEL` unset. The result should report `provider: "mock"` and zero token usage.

## Configure the model on the server

Every LLM call of the platform — the assistant, the authenticated `POST /api/v1/ai/incident-summary` route and codeintel enrichment — goes through **one OpenAI-compatible Chat Completions endpoint** (ADR-034):

| Env | Default | |
| --- | --- | --- |
| `TT_ASSISTANT_API_URL` | `https://api.meta.ai/v1` | Base URL (`…/v1`) or the full `…/chat/completions` URL. Default: Meta Muse Spark; any OpenAI-compatible gateway works |
| `TT_ASSISTANT_API_KEY` | unset | Server-side secret, sent as `Authorization: Bearer` |
| `TT_ASSISTANT_MODEL` | unset | Model id (never hard-coded in the code) |

Key **and** model set → real model; otherwise the incident summary and the assistant use the offline mock and codeintel uses static rules. Keep the key in the server environment or secret manager; never put it in `apps/web`, a `VITE_*` variable, a committed file, or a browser request. Test the endpoint and model against your account before enabling traffic. Example PowerShell session for an isolated server-side smoke test:

```powershell
$env:TT_ASSISTANT_API_KEY = '<secret from your secret manager>'
$env:TT_ASSISTANT_MODEL = '<a Muse Spark model id>'
go run ./cmd/ai-demo # this makes one paid request
```

Clear those session variables after testing.

## Spending controls

- The incident-summary route allows at most 4,000 bytes of title and evidence and requests at most 150 output tokens. Its HTTP client times out after 15 seconds and does not retry automatically.
- With a real model, every incident-summary request reserves AI credits before the call (organization and member monthly limits, `services/pkg/aiquota`) and records the real token usage afterwards (`ai_usage.feature = rca`, `ref = incident-summary`), also when the answer is unusable. When the month's quota is used up the route answers HTTP 429 without calling the model. The assistant (`feature = assistant`) and codeintel enrichment (`feature = codeintel.enrich`) use the same quota.
- Set a provider-side budget or alert as a second limit, and monitor actual billing. Keep the dashboard's `0 credits` label only while it calls `/local-ai/*`.

To return to free development, unset `TT_ASSISTANT_API_KEY` (or `TT_ASSISTANT_MODEL`) and restart the server. The contextual dashboard buttons remain offline mocks regardless of this setting.
