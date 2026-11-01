# apps/web — Coding standards

Rules for the dashboard (`apps/web`), derived from what the code does today. Repo-wide rules
(commits, language, review) live in [`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md);
visual rules (tokens, components, data viz, page composition) live in
[`../../../.ai/design-system.md`](../../../.ai/design-system.md). This file does not restate them.

## Toolchain

- React 19, react-router 7 (`createBrowserRouter`), Vite 8, TypeScript 5.9, pnpm workspace. Icons: `lucide-react`.
- No ESLint / Prettier config. The only gates are `tsc -b` and `vite build` (CI `web` job, `make check`).
- Env: `VITE_API_URL` (default `http://localhost:8080`), `VITE_TROOPER_DSN` (self-RUM, ADR-027). Local overrides in `.env.local` (git-ignored); template in `.env.example`.

```bash
cd apps/web
pnpm install
pnpm dev          # http://localhost:5173, needs the api running
pnpm typecheck    # tsc -b
pnpm build        # tsc -b && vite build
make check        # repo root, before every commit/PR — must end with "check OK"
```

## Layout

```
src/main.tsx            boot: auth → onboarding / public router / app; global CSS imports
src/router.tsx          app routes (inside AppShell); publicRouter.tsx = signed-out routes
src/screens/            one file per route screen (PascalCase.tsx); shared screen pieces in lowercase files (shared.tsx, apm.tsx, rca.tsx, k8sShared.tsx)
src/components/ui/      design-system primitives, re-exported from index.ts
src/components/charts/  own SVG charts, re-exported from index.ts
src/components/layout/  AppShell, AppProvider/useApp (context.tsx)
src/components/{public,replay}/  feature component groups
src/data/               api access + UI types; screens import from '@/data' (index.ts)
src/lib/                pure helpers (format.ts, rca.ts) and feature logic
src/styles/             global CSS files imported once in main.tsx
```

- New route → add the screen in `src/screens/`, register it in `src/router.tsx`.
- New reusable UI → `src/components/ui/`, export from `index.ts`, document in design-system.md (root rule 2).

## TypeScript

`tsconfig.json` is `strict` + `noUnusedLocals` + `noUnusedParameters` + `verbatimModuleSyntax` + `isolatedModules`.

- Type-only imports must use `type`: `import { kpis, type Kpi } from '@/data';` (`verbatimModuleSyntax` fails the build otherwise).
- Prefer the `@/` alias (`@/components/ui`, `@/data`, `@/lib/format`) over deep relative paths.
- UI types live in `src/data/types.ts`. Api shapes that differ get a local `Raw*` type and a `map*` function in the data module (see `RawKpi` / `mapKpi` in `src/data/kpi.ts`); `null` from the api becomes `undefined`, unknown enum strings are narrowed to a known value.
- Use string-literal unions, not `enum`: `type StatusKind = 'ok' | 'warning' | ...` (`src/components/ui/Badge.tsx`).
- Labels per union value: `Record<Union, string>` so a new value is a compile error (`KPI_KIND_LABEL` in `src/screens/Kpis.tsx`).
- No `any`. Use `unknown` and narrow; in `catch`: `e instanceof Error ? e.message : String(e)`.
- Non-null `!` only where the DOM guarantees it (`document.getElementById('root')!` in `main.tsx`).

## Components

- Function components with named exports; no default exports. Props typed inline for small components, a `Props` type otherwise (`Button.tsx`, `TimeSeries.tsx`).
- Primitives forward native props: `type Props = ButtonHTMLAttributes<HTMLButtonElement> & {...}` and spread `...rest` (`Button.tsx`).
- Build screens from `@/components/ui` (`PageHeader`, `Section`, `Card`, `Table`, `EmptyState`, `StatusBadge`, `Skeleton*`, …) — don't re-create them with raw markup.
- Short JSDoc on exported components/functions saying *what it shows* or *why* (e.g. `/** KPI table; worst first (lowest budget). */`).

Do / Don't:

```tsx
<Button variant="primary" disabled={saving} onClick={() => void create()}>Create KPI</Button>   // do
<button className="btn btn--primary" onClick={create}>Create KPI</button>                      // don't
```

## Data layer (`src/data`)

The dashboard reads only real api data (ADR-013, ADR-028): no mock, seed or demo values, ever.
Reference: `src/data/remote.ts`.

- **Reads are synchronous.** `remote<T>(path, fallback, map?)` returns the cached value or `fallback` (an empty value — `null`, `[]`, `0`), starts the request on a miss and re-renders consumers when it lands. TTL 60 s, stale-while-revalidate.
- **Lists are hydrated in place.** Exported arrays (`export const kpis: Kpi[] = []`) are filled by `registerHydrator(() => hydrateList(kpis, '/kpis', map))`; they refresh at boot, every 60 s and after each mutation. Keep the array reference — never reassign.
- **Writes** go through `mutate(method, path, body, invalidatesPrefixes)` and return the api result (`createKpi` in `src/data/kpi.ts`).
- **Paths:** build query strings with `qs({ range, ... })` (drops empty values, adds the global `env` filter) and segments with `seg(id)`. Never concatenate raw ids/params.
- Raw `fetch` only inside `remote.ts` / `auth.ts`; everything else calls `api`, `remote`, `prefetch` or `mutate`.
- Each data function gets a one-line JSDoc naming its endpoint: `/** KPIs of the project (`GET /kpis`, hydrated in place). */`.
- Screens import from `@/data` (the barrel), not from `@/data/<module>`; exception: `@/data/remote` for `useData`.
- Tenant scoping is handled by the transport (`X-Trooper-Project` header, session cookie) — don't add org/project params by hand.

## State and hooks

- Global UI state (time range, env filter, theme, motion) is in `AppProvider`; read it with `useApp()` (`src/components/layout/context.tsx`). Components that don't call `useApp()` but read data call `useData()` to re-render on new data.
- Route params: `const { kpiId = '' } = useParams();` then look the entity up; render `EmptyState` when not found (`src/screens/KpiDetail.tsx`).
- Local form state with `useState`; async actions follow the `saving` + `error` pattern:

```tsx
const [saving, setSaving] = useState(false);
const [error, setError] = useState<string>();
async function create() {
  setSaving(true); setError(undefined);
  try { nav(`/kpis/${await createKpi(draft)}`); }
  catch (e) { setError(e instanceof Error ? e.message : String(e)); }
  finally { setSaving(false); }
}
```

- `localStorage` access is always wrapped in `try/catch` (private mode); keys are prefixed `trooper-` (`context.tsx`, `main.tsx`).
- No data-fetching in `useEffect`; use the data layer. `useEffect` is for DOM/side effects (theme attribute, players, observers).

## Loading, empty and error states

- Loading: `Skeleton`, `SkeletonText`, `SkeletonRows`, `SkeletonPage` with a `role="status"` + `aria-label` (`src/components/ui/Skeleton.tsx`, `ReplayDetail.tsx`).
- Empty / not found: `EmptyState` with an icon and a plain title; `Table` takes an `empty` message.
- Failed reads: `remoteStatus().errors` filtered by path prefix (`readError` in `src/screens/Settings.tsx`). Failed writes: show the caught message next to the action.
- Never fill a gap with invented numbers; show the empty state.

## Styling

Full rules: [design-system.md](../../../.ai/design-system.md). In code:

- Global CSS in `src/styles/*.css`, imported once from `main.tsx`; no CSS modules, no CSS-in-JS library. A new feature area may add a file (`servicemap.css`, `trace.css`) and import it in `main.tsx`.
- Class names are BEM-style: `block`, `block__element`, `block--modifier`, state as `is-*` (`why__hop why__hop--root`, `rev__item is-active`).
- Values come from `tokens.css` custom properties: `var(--space-2)`, `var(--text-sm)`, `var(--radius-md)`, `var(--status-critical)`, `var(--font-mono)`. No raw hex, px spacing or font stacks.
- Inline `style` only for computed/one-off layout, and still with tokens: `style={{ gap: 'var(--space-2)' }}`, not `style={{ gap: 6 }}`.
- Utility classes in use: `row`, `spacer`, `muted`, `secondary`, `mono`, `num`, `truncate`, `eyebrow`.

## Charts

Use `@/components/charts` (`TimeSeries`, `Columns`, `Sparkline`, `BarList`, `Legend`, `Flow`, `PartitionBars`, `FlameGraph`). Rules: [design-system.md § Data viz rules](../../../.ai/design-system.md).

- Series colors via `seriesColor(slot)` → `var(--series-N)`; pass a stable `slot` per entity so colors don't shift between charts.
- One y-axis per chart; use `threshold` / `events` props for reference lines and deploy markers instead of a second axis.
- Pass a `format` function from `@/lib/format` (`ms`, `pct`, `compact`, …) instead of formatting inside the chart.

## Accessibility

- Status is never color-only: use `StatusBadge` (icon + label) (`Badge.tsx`).
- Decorative icons get `aria-hidden`; meaningful icon-only elements get `aria-label` (`<Code2 aria-label="Suggested from code" />`).
- Loading regions: `role="status"` + `aria-label`; inline errors: `role="alert"` (`LocalAIState.tsx`).
- Interactive elements are `<button>`/`<a>` (via `Button`, `Link`), not clickable `<div>`s; `Button` defaults to `type="button"`.
- Respect motion: `prefers-reduced-motion` and the `data-motion="reduced"` user setting (`styles/motion.css`).

## Naming

- Files: components and screens `PascalCase.tsx`; data/lib modules `camelCase.ts`.
- Components `PascalCase`; hooks `useX`; constants `UPPER_SNAKE` (`TTL_MS`, `SLI_LABEL`); data mutations as verbs (`createKpi`, `invalidate`).
- Route paths kebab-case (`/load-tests/runs/:runId`, `/service-map`); route params `camelCase` + `Id`.
- User-facing copy: sentence case, English, no jargon where a plain word exists ("Burning budget", "KPI not found").
