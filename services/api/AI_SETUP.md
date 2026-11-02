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

Open `http://localhost:5173`. Vite proxies `/local-ai/*` to the playground on `127.0.0.1:8099`. The contextual buttons in Incidents and Code Insights **always** use this offline mock. The Overview "Ask telemetry" drawer is gone: free-text questions go to the Assistant screen (`/assistant`, ADR-033), which runs in the authenticated api — offline mock by default, Claude with `ANTHROPIC_API_KEY` + `TT_ASSISTANT_MODEL` (see `.ai/assistant.md`). The playground explicitly constructs `ai.NewMock()` and ignores paid-provider environment variables. Its actions show an estimated 0 credits and unlimited mock allowance; this is not an estimate of a future paid call. Check `http://127.0.0.1:8099/healthz` if a button cannot reach it.

To test the authenticated API's incident-summary implementation without a database, run `go run ./cmd/ai-demo` from `services/api` with `TT_AI_PROVIDER` unset or set to `mock`. The result should report `provider: "mock"` and zero token usage.

## Configure a provider on the server

The authenticated `POST /api/v1/ai/incident-summary` route is separate from the dashboard playground. It defaults to `mock`. A paid call requires **all** of: an explicit provider, `TT_AI_ALLOW_PAID=true`, and that provider's key. Keep keys in the server environment or secret manager; never put them in `apps/web`, a `VITE_*` variable, a committed file, or a browser request.

| Provider | `TT_AI_PROVIDER` | Server-side secret |
| --- | --- | --- |
| Meta | `meta` | `MODEL_API_KEY` |
| Baseten | `baseten` | `BASETEN_API_KEY` |

`TT_AI_MODEL` optionally overrides the configured default model. Test the provider and model against your account before enabling traffic. Example PowerShell session for an isolated server-side smoke test:

```powershell
$env:TT_AI_PROVIDER = 'meta' # or 'baseten'
$env:TT_AI_ALLOW_PAID = 'true'
$env:MODEL_API_KEY = '<secret from your secret manager>' # use BASETEN_API_KEY for Baseten
go run ./cmd/ai-demo # this makes one paid request
```

Clear those session variables after testing. Merely setting a key does not enable paid calls; the opt-in must be exactly `true`.

## Spending controls and production gate

- The incident-summary route allows at most 4,000 bytes of title and evidence and requests at most 150 output tokens. Its HTTP client times out after 15 seconds and does not retry automatically.
- These are **per-request** limits. They do not cap the number of calls or total spend. A manual Retry in the dashboard starts another request, although the current dashboard routes are mocks.
- The existing organization/member AI quota and Usage page cover code-intelligence enrichment. The standalone incident-summary route does **not** reserve credits or record usage in that ledger yet.
- Before routing dashboard buttons to a paid provider, add a server-side preflight estimate, atomic quota reservation, actual-usage settlement, and a response containing the remaining allowance. Block calls when the allowance is exhausted. Show the estimate as a range if actual token use can vary.
- Set a provider-side budget or alert as a second limit, and monitor actual billing. Keep the dashboard's `0 credits` label only while it calls `/local-ai/*`.

To return to free development, set `TT_AI_PROVIDER=mock` or unset it and remove `TT_AI_ALLOW_PAID`; restart the server. The contextual dashboard buttons remain offline mocks regardless of this setting.
