# apps/web — Testing

What is verified for the dashboard today, and what new code is expected to add. Repo-wide testing rules:
[`../../../.ai/testing.md`](../../../.ai/testing.md). Coding rules: [`coding-standards.md`](coding-standards.md).

## Current state (honest)

- **No automated tests in `apps/web`.** No `*.test.ts(x)`, no vitest/jest dependency, no `test` script in `package.json`.
- **No lint step.** There is no ESLint/Prettier config; strict `tsc` is the only static check.
- **CI** (`.github/workflows/ci.yml`, job `web`) and **`make check`** run exactly:

```bash
cd apps/web
pnpm exec tsc -b       # typecheck (strict, noUnused*, verbatimModuleSyntax)
pnpm exec vite build   # production build
```

- Root `AGENTS.md` (rule 8): web → vitest for component logic, "to be added". Until it lands, the gates above
  plus a manual check in the browser are what a web change must pass.

## Before every commit / PR

1. `pnpm typecheck` in `apps/web` (fast loop while editing).
2. `make check` at the repo root; it must end with `check OK`. Don't skip or disable a step to get green.
3. Run the change in the browser against a real api (see below) — the build does not catch runtime or layout bugs.

## Manual verification

There is no mock backend (ADR-013, ADR-028), so UI is checked against real data:

```bash
docker compose -f deploy/docker-compose.yml up -d   # infra
make migrate && make build                           # schemas + services
make self                                            # api bootstrap + self-observability; prints web env
cd apps/web && pnpm dev                              # http://localhost:5173
```

Check for every touched screen:

- **States:** loading (skeleton), empty (`EmptyState`, `Table` `empty`), not found (bad route param), api error
  (stop the api — the screen must not crash or show invented numbers), mutation error (message next to the action).
- **Themes:** light and dark (`Settings` → theme), and `reduced` motion.
- **Widths:** narrow viewport — no horizontal page scroll; tables/charts stay readable.
- **Env / range:** change the topbar environment and time range; data refetches and charts redraw.
- **Console:** no React warnings (the app runs under `StrictMode`) and no unexpected `[data]` warnings.
- **Self-RUM:** with `VITE_TROOPER_DSN` set, errors from the change show up in the platform itself (ADR-027).

## Expected for new code

When vitest is added to `apps/web`, the first targets are pure modules with no React/DOM dependency.
Until then, keep new logic in that shape so it is testable without refactoring:

- Put non-trivial logic in **pure functions** in `src/lib/` or as exported helpers in a data module, not inline in JSX.
- Take inputs as arguments (including `now = Date.now()` — see `ago()` in `src/lib/format.ts`); no hidden reads of the
  remote cache or `localStorage` inside the function.
- Keep api→UI mapping in a separate `map*` function (`mapKpi` in `src/data/kpi.ts`), so it can be tested with raw JSON fixtures.

Priority candidates (pure, already isolated):

| Module | What to cover |
|---|---|
| `src/lib/rca.ts` (`findRootCause`) | single path, parallel siblings (critical path, not sum), own-code wins, leaf stop, no change → empty path, confidence product, `alternatives` ≥ 15 % share. Must match the server-side algorithm in `.ai/alerting.md`. |
| `src/lib/format.ts` | `compact`, `pct`, `ms`, `duration`, `ago`, `change` boundaries (0, negative, unit switch points). |
| `src/components/charts/scale.ts` | `niceTicks` (0, tiny, huge max), `seriesColor` wraps at 8. |
| `src/data/remote.ts` (`qs`, `seg`) | empty values dropped, `env` added only when `range` present and not overridden, encoding. |
| `map*` functions in `src/data/*` | `null` → `undefined`, unknown enum strings narrowed to the default. |
| `src/lib/lagAlerts.ts` (`evaluate`, `describeRule`, `recommend`) | threshold/episode counting, recommendation per broker kind. |

## When vitest is added (follow the JS SDK setup)

Mirror `sdks/js/packages/browser`, which already runs vitest in `make check`:

- `devDependencies`: `vitest`; `"test": "vitest run"` in `package.json`.
- `vitest.config.ts` resolving the `@/` alias like `vite.config.ts`; `environment: 'node'` for pure modules,
  `happy-dom` only when a test needs the DOM.
- Tests next to the code as `*.test.ts` (or a `test/` folder, as in the SDK), table-driven where cases repeat:

```ts
import { describe, expect, it } from 'vitest';
import { pct } from '@/lib/format';

describe('pct', () => {
  it.each([[0, '0%'], [0.000001, '<0.001%']])('%s → %s', (v, want) => {
    expect(pct(v)).toBe(want);
  });
});
```

- Add the web test step to the CI `web` job and to `make check` in the same PR, and update this file and
  `AGENTS.md` ("vitest … to be added").
- Component tests (rendering) come after pure-logic tests; no snapshot tests of whole screens.
- Never add mock api data to `src/` for tests — fixtures stay in test files (ADR-013/028 forbid demo data in the app).
