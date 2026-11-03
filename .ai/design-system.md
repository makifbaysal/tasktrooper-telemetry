# Design system

Source: `apps/web/src/styles/tokens.css` (tokens), `apps/web/src/components/ui` (components), `apps/web/src/components/charts` (charts).
Live showcase: the **`/design-system`** page in the dashboard (no link in the sidebar, open by URL).
Design record of the visual system (Impeccable): [`apps/web/DESIGN.md`](../apps/web/DESIGN.md) + `apps/web/.impeccable/design.json`; product context [`apps/web/PRODUCT.md`](../apps/web/PRODUCT.md). On conflict `tokens.css` and this file win; DESIGN.md is updated in the same PR.

**Theme source: TaskTrooper.** Colors, typography, radius and patterns follow the TaskTrooper app's theme
(`../tasktrooper-company/desktop/ui/src/styles/globals.css` and the ops console `components/operations/**`). If the theme changes
there, this file and `tokens.css` are updated in the same PR.

## Principles

1. **Data speaks loudly, the interface stays quiet.** Hue-265 cool neutrals, primary = TaskTrooper navy (`--accent`), brand mark `--brand-mark #15214b`, warm amber (`--accent-warm`) only for selection/emphasis. Color only for data and status.
2. **Dense but airy.** Body 14px (Inter), 4px grid, tight tables; 16px between cards. Labels (`.eyebrow`, table header, metric label) sentence case, 12–14px; uppercase only in sidebar group names.
3. **Status is never color alone.** Status is always icon + label (`StatusBadge`).
4. **Light and dark are equal citizens.** The dark theme uses separate steps picked from the same ramps, not automatic inversion. Theme: `system | light | dark` (from the topbar), via the `data-theme` attribute.
5. **Raw values are forbidden.** No hex, px font size or arbitrary spacing in components; use tokens.
6. **Motion lives in one layer and can be turned off.** Decorative motion (card reveal + ambient breathing, liquid cursor, sidebar collapse) only in `styles/motion.css` + `components/layout/motion.ts`; tokens `--dur-slow/--dur-reveal/--dur-ambient/--reveal-shift`. OS `prefers-reduced-motion` or **Reduce motion** in the account menu (`data-motion="reduced"`, localStorage `trooper-motion`) turns all of it off. No eyebrow/kicker above screen titles; no second accent color (purple primary, green signal) is added.

## Tokens

### Surfaces & ink
| Token | Usage |
|---|---|
| `--bg-page` | Page background |
| `--bg-surface` | Card, chart surface |
| `--bg-raised` | Input, popover, tooltip |
| `--bg-sunken` | Code block, segmented background, well |
| `--bg-hover` / `--bg-active` | Row/nav hover and selected |
| `--text-primary` / `--text-secondary` / `--text-muted` | Text hierarchy (muted = axis, meta) |
| `--border` / `--border-strong` / `--divider` | Edge, input edge, table line |
| `--accent`, `--accent-hover`, `--accent-soft`, `--accent-text` | Primary action (navy), selected tab, link |
| `--brand-mark`, `--accent-warm` | Logo box (fixed in both themes), selection color |
| `--scrollbar-thumb(-hover)`, `--scrollbar-track` | Thin, rounded, theme-aware scrollbar (8px) |

### Diff (diverging pair)
`--diff-increase` (orange, share grew) · `--diff-decrease` (blue, share shrank) — separate light + dark steps. Only in comparison
charts (profile diff flame graph, "Biggest changes" swatch). Status colors are **not used** for diff; intensity via
`color-mix(in srgb, var(--diff-*) N%, var(--bg-surface))` (mixing in oklch shifts the hue toward magenta). Unchanged = `--bg-sunken`.

### Status (TaskTrooper `.dark` steps in the dark theme; `-soft` background, `-text` text)
`--status-good` · `--status-warning` · `--status-serious` · `--status-critical` (+ `-soft` background, `-text` text)

Mapping: `ok/resolved → good`, `warning/pending → warning`, `acknowledged → serious`, `critical/firing → critical`, `muted → neutral`.

### Typography
| Token | px / lh | Usage |
|---|---|---|
| `--text-xs` | 11/14 | micro: axis tick, badge, uppercase labels |
| `--text-sm` | 12/16 | caption: secondary text, meta |
| `--text-md` | 14/20 | **body default** |
| `--text-lg` | 16/24 | heading: section and card title |
| `--text-xl` | 20/28 | title: drawer / secondary title |
| `--text-2xl` | 28/36 | page title (PageHeader h1, `-0.022em`), stat / metric value |
| `--text-hero` | 44/48 | At most one hero number per page |

Fluid steps for public pages (landing, docs, login): `--text-display` (40→68px, hero h1), `--text-headline` (28→40px, section title), `--text-lede` (16→19px); body measure `--measure` 68ch. Not used inside the app shell.

Labels are sentence case: metric label (`--text-md`, secondary), table header (`--text-sm`, secondary) and `.eyebrow` (`--text-sm`) carry no uppercase/tracking; uppercase only in sidebar group names. Kicker/eyebrow above titles is forbidden.

Font: `--font-sans` (Inter), `--font-mono` (JetBrains Mono — route, log, code, trace id, path chips). Numbers `tabular-nums`, large values `letter-spacing: -0.02em`.

### Spacing, radius, layout
- Spacing: `--space-1..12` (4px grid) + `--space-16/24/32` for public pages.
- Radius: `--radius-xs 4` (bar end) · `sm 6` (chip, segmented item) · `md 8` (button, input, nav item) · `lg 14` (card, rounded-xl) · `full` (badge).
- Control heights: `--control-h-sm 30`, `--control-h-md 36`.
- Layout: sidebar 240px (collapsed by the button in the topbar, localStorage `trooper-sidebar`), topbar 56px (semi-transparent + blur), content max 1440px. Below 900px the sidebar is an overlay drawer above `--scrim` (opened by the same button; navigation, Escape or tapping the scrim closes it); below 640px the topbar wraps onto two rows, the time range on its own row. Quick switch in the topbar (`GlobalSearch`: service + page, `/` or ⌘K). The env selector and time range in the topbar are visible only on screens that read them (`scopeFor`, `AppShell.tsx`): Overview, service/endpoint/trace, infrastructure, KPI detail, AI monitoring; hidden on Incidents, Settings, Load tests, Code insights. Screen layout (Overview, Incidents, Services) is in `styles/screens.css`, structure only.
- Cards are flat: no shadow, `1px --divider` border, `--radius-md`; on hover (`card--interactive`) only `--border-strong`. A card inside a card is not a box, it becomes a region of its parent (no border, hairline between them). Card in alarm state (`.card--rail-*`, `.rca`): border mixed with the status color + 2px inset line on top; colored left/right edge (>1px) is forbidden.
- Content rhythm: `.content` padding `--space-8`, `--space-8` between sections, `--space-4` within a section.
- Public pages: `--public-max` 1200px column, `--public-nav-h` 72px; nav, hero and sections aligned to the same inner edge (`--inline`). Brand background `--field-*` (navy, same in both themes): hero, code band, code blocks, login panel. Hero animation `--ease-out-expo`; reduced motion shows the last frame. The root cause finding (`.finding`) is the chart's last lane: in the wide layout it sits absolutely below the SVG's `FINDING_Y` line (fixed ratio, the stage does not resize), connected by a dashed line coming down from reco-db; no card/shadow, a thin band in the signal tone. Four steps: symptom → path → cause → suggested fix; in the narrow layout (≤720px) it drops below the chart.

## Components (`components/ui`)

| Component | Note |
|---|---|
| `Button` | `primary` (single main action on the page), `secondary` (default), `ghost`, `danger`; `sm`/`md`; `aria-label` required for `iconOnly` |
| `Badge` | `neutral, accent, good, warning, serious, critical` tones |
| `StatusBadge` | The one correct way for status — icon + label |
| `Card` | `title`, `subtitle`, `actions`, `flush` (no padding, for table/list) |
| `StatTile` | label · value(+unit) · delta (direction × `upIsGood`) · optional sparkline |
| `Table` | Column-defined, `numeric` right-aligned tabular, sortable with `sortValue`, `onRowClick`; nested rows with `childRows` (indented right below the parent, same sort — e.g. an Azure Function App's functions). Scrolls inside if wider than the card, shadow on the clipped edge; if `empty` is a string, a padded `.table__empty` |
| `Tabs` / `Segmented` | Tabs = in-page section (navigation with arrow keys / Home / End, the clipped edge fades on overflow — `data-fade`); Segmented = filter/time range |
| `Field`, `Input`, `Textarea`, `SearchInput`, `Select`, `Toggle` | Form elements (`Textarea mono` for JSON/header templates) |
| `PageHeader` | breadcrumb + title + subtitle + actions |
| `EmptyState`, `Meter`, `CodeBlock`, `Callout` | Helpers |
| `Skeleton`, `SkeletonText`, `SkeletonRows`, `SkeletonPage` | Loading state: shimmer blocks in the shape of the content (`.skel`, `prefers-reduced-motion` → flat). No "Loading…" text or spinner in the middle; spinner only for in-button actions and replay buffering. One `role="status"` + `label` per group. Boot screen (`main.tsx` `renderLoading`): app shell skeleton if there is a session hint (`trooper-signed-in`), otherwise navy field background |
| `ChoiceChips` | Multi-select (region, service, drain type): `Segmented` look, multiple `aria-pressed`, wraps on narrow screens |
| `CopyCode` | `CodeBlock` with a copy button (IAM policy, CloudFormation/Terraform, drain URL); `wrap` breaks a long single-line value (URL, ARN) |
| `Menu`, `MenuItem`, `MenuLabel`, `MenuDivider` | Popover menu attached to a trigger (user menu, org/project selector). `trigger({open, toggle})` draws its own button; Esc / outside click / selection closes it; `align` start/end, `placement` below/above, `block` full width. `MenuItem selected` → check icon, `danger`, `hint` second line. Style `styles/account.css` (`--bg-raised`, `--shadow-lg`) |
| `Markdown` | Safe markdown subset for model output (assistant replies): headings, paragraphs, lists, quotes, fenced code, pipe tables, inline code / bold / italic / links. React elements only (no HTML strings); `/…` links → router `Link`, http(s) → new tab, other schemes → plain text. Style `.md*` in `styles/assistant.css` |
| `Avatar` | Initials on an `--accent-soft` disc or IdP picture; `sm` / `md` (= `--control-h-sm`) / `lg` |
| `SdkQuickstart` | `Segmented` per SDK (Go, Node.js, Browser, Python, Java, .NET; `group` = all / backend / frontend) → install command + `init` snippet, both `CopyCode`; the `dsn` prop is embedded in the snippet (`components/ui/SdkSetup.tsx`) |
| `ConnectSdk` | No-data empty state (ADR-028): `EmptyState` (icon, title, what the screen will show once data arrives) + `SdkQuickstart` with the project's DSN (`<ingest-key>` instead of the key, `projectDsn()`) + "Settings → API keys" link. `setup={false}` → text only (empty filter result). The "no data yet" state of the main screens is drawn with this |

Integration patterns (`screens/Integrations.tsx`): `ProviderMark` (provider identity — no brand color/logo, neutral box + abbreviation),
`IntegrationStatusBadge` (ok → Connected, error → Error, pending → Syncing…, disabled → Disabled; always icon + label),
`IntegrationSourceBadge` ("AWS · aws-prod" on a discovered DB/cluster row, click → integration drawer; critical tone if sync failed),
`DeployList` (deploy marker list: version, commit message, author, source, link, build status).

Code insights / load test patterns: `SuggestionCard` (`screens/CodeInsights.tsx` — suggestion card: criticality + "From code" + calibrated/not calibrated badge,
target service (link to its Alerts tab, "Not reporting yet" badge when the service sends no telemetry) + endpoint, condition mono, rationale,
`file:line` GitHub link, Reject / Customize / Accept…; Accept… opens `AcceptSuggestionDrawer` (`screens/Alerts.tsx`, also used by the service
Alerts → Suggested tab: where it goes (service, endpoint, "Shown under"), proposed rule, editable name + severity, why, code ref; "Create monitor"
accepts and opens the service's Alerts tab); after accept "Monitor created" + link to the monitor),
`ConnectRepoDrawer` (`screens/ConnectRepo.tsx` — primary "Connect GitHub" (full-page redirect to the api), on return a repo picker via `?github=<id>`:
search, Private/Added badges, multi-select; token / local path under "Advanced"), repo row states (Analyzing… / Analyzing push… +
`pending_paths`, Disconnected warning, Analysis failed + error text, "Webhook active"), `RunBlockedCallout` + `ScenarioSource`
(`screens/LoadTests.tsx` — "cannot run" warning with draft / domain / limit / api reasons, Domains link for domain; Draft · needs review warning badge),
`LoadTestDomains` (`screens/LoadTestDomains.tsx` — TXT record and `/.well-known/trooper-verify` instructions with `CopyCode`, Verify / Checking… state),
run screen `TargetCard` (target service RED: "All traffic" slot 1 vs "Load test" slot 2, a separate chart per measure).

Account / notification patterns: `Login` (`screens/Login.tsx`, `/login` page: `--field-*` brand panel on the left + form on the right, `.signin*` classes in `public.css`; also rendered outside the router when the session drops), `Onboarding` (`screens/Onboarding.tsx`, outside the router, `.login__card`; step 1 org + first project form → `POST /orgs`, step 2 wide card `.onboarding__card`: DSN and ingest key once via `CopyCode` + warning `Callout` + `SdkQuickstart` + "Continue to the dashboard"), `NotifyPicker` (`screens/NotifyPicker.tsx` — target input with chips: Slack channel `#name`, private lock icon, free target mono; search list in `Menu` style), `MonitorSourceBadge` (`code` → accent + lock "Managed in repository", `suggestion` → accent + ✨), `LikelyCause` (summary + confidence badge; < 30% "low confidence" in warning tone). Actions disabled by role are hidden or `disabled` + `title` states the reason (`denied()`); read-only forms `<fieldset disabled>`.

Infrastructure / K8s patterns (`screens/k8sShared.tsx`): `InfraStatusBadge` (ok/warning/critical + `stale` → neutral clock icon "Stale"), `PodPhaseBadge` (shows the reason if any — CrashLoopBackOff/OOMKilled/ImagePullBackOff critical, others warning; otherwise phase), `UsageCell` / `LimitCell` (0..1 meter; used / limit, "no limit" if there is no limit), `EventList` (K8s Warning event stream), `AgentSetup` (Helm + `docker run` instructions when there is no data, note on optional eBPF), `EbpfBadge` (accent, eBPF service without SDK). Capacity charts are one chart per unit: used / requested / allocatable (or usage / request / limit) `LINE_SLOTS` 1-2-3.

AI usage / serverless patterns: `AiQuotaBanner` (`screens/Usage.tsx` — page-top `Callout`, warning/critical tone badge + text + View usage + close; dismissed per month and threshold), `AiCapEditor` (member cap: `Meter` + "used / cap" or "No cap", pencil → inline number input, Enter saves / Esc cancels; in the Usage and Members tables), credit format `credits()` (2 decimals below 100). `FaasStatusBadge` (`screens/Serverless.tsx` — ok/warning/critical `StatusBadge`, `idle` / `missing` neutral "Idle" / "Missing"), cost everywhere `usd()` + "est." label; in function detail a separate chart per unit (counts, ms, concurrency, cold start columns), error series `--series-8`.

AI monitoring patterns (`screens/AiMonitoring.tsx`, ADR-023 — do not confuse with "AI usage" (quota)): `AiMonitoringBody` (`/ai` and the service **AI** tab share the same body, `service` scope), `ProviderBadge` (provider identity neutral `Badge` — no brand/status color), cost cell (`usd()` / `aiUsd()` with 4 decimals for a single call, "est." everywhere; if no price, muted **price unknown** → `AiPricesDrawer`, totals "partial"), model colors by `LINE_SLOTS` following the page's alphabetical model list (cost `Columns` stacked, "Other" `--text-muted`), four `MetricCard`s (cost, token input/output, calls + errors `--series-8`, p95 + TTFT in hint). `AiCallDrawer` (meta, token/cost, error `Callout`, OTel GenAI messages as role-based `.llm-msg` cards — tool call / tool result `CodeBlock`; if no content, "Content capture is off" `EmptyState` + Node/Go snippet; "Open trace" → `/traces/:id?span=`), `AiPricesDrawer` (price table USD / 1M tokens, Default / "Org override" badge, admin+ edit / reset, viewer read-only `Callout`), `AiSetup` (`Segmented` + `CopyCode` per SDK when there is no LLM data), `LlmSpanSection` (model, token, cost, TTFT + prompt drawer in the trace span panel). Style `styles/aimon.css`.

Service map (`screens/ServiceMapScreen.tsx`, style `styles/servicemap.css`): reuses the landing's `smap` classes (public.css); inside the app `.svcmap` maps the `--field-*` tokens to the theme (canvas `--bg-sunken`, node `--bg-surface` + `--text-primary`, lines `--border*`) — the navy field is only on the landing. Node width fits both the name and the metric row. Tier lanes are separated by a light background color (Clients blue, Services neutral, Data aqua, External magenta; the status family and nearby tones are not used), with a dashed divider between them. The viewport refits only when the layout changes (node set / size); the 60 s data refresh keeps zoom/pan. Status only via `--status-*` and always icon (node top right) + label (incident tag, tooltip, panel `StatusBadge`). Layout on the client: tier lanes, services in rows by call depth, at most 6 nodes per row (wraps), neighbor barycenter ordering; wide node spacing (horizontal 56, 150 units between lanes). Viewport via viewBox: scroll zooms around the cursor, drag pans, +/−/Fit (Fit does not enlarge a small map beyond 1100 units wide). The detail panel is an overlay on the left of the canvas (slide-in, does not shrink the map); the selected node is centered in the area outside the panel. Problem nodes in a card carousel on top (`.svcmap__card`: status line on the top edge + `StatusBadge`, scroll-snap, arrow buttons). The service detail's Map tab embeds the same component with `serviceId` (`.svcmap--embedded`, `.svcmap__bar` instead of PageHeader). Reduced motion: no traffic dots.

Session replay (`components/replay`, ADR-021): `ReplayPlayer` (web, `@rrweb/replay` 2.2.0 Replayer, stage scaled to fit the recording
size; `addEvent` as segments arrive; cursor click ring `--accent-warm`) and `WireframePlayer` (mobile, SVG: box by node
type, bg/fg/radius/border from the recording; masked label = a bar per word, masked input = dots, image = placeholder box + glyph; touch
`--accent-warm` ring + trail, keyboard neutral panel; device frame `--border-strong`). Both take the same `PlaybackProps` (playing, speed,
skipInactive, seek, onTime, onEnded). Screen (`screens/ReplayDetail.tsx`): player + controls (play/pause, 1×/2×/4×/8× `Segmented`, "Skip
inactivity" `Toggle`) + scrubber (played `--accent`, loaded `--bg-active`, empty/inactive range hatched; markers: view `--series-1`,
click `--series-3`, error `--series-8`, failed request `--series-2`, with legend) + event list on the right (active row `--accent-soft` + left
rail, future ones faded; error → stack drawer, `traceId` → trace link). "Text and inputs are masked" with a lock icon in the header.

Profiling screen (`screens/Profiling.tsx`): type `Segmented`, host / version `Select`, Profile / Compare; `TimeSeries` (core) per host,
`FlameGraph`, Top functions `Table` (click a row → search in the flame graph), Compare: version↔version or time window, diff flame graph +
"Biggest changes" (swatch + `±x pp`). If there is no profile, `ProfilingSetup` (`TROOPER_PROFILING=true` `CopyCode`), a compact `Callout` on the endpoint page.

Screen-level patterns (`screens/rca.tsx`):

| Pattern | Note |
|---|---|
| `RootCauseCard` | On the service/endpoint page above golden signals; only on a significant increase (≥30% and absolute threshold). Origin label: "Your code" / "Your dependency" / "Not your code · downstream". Sentence + `PathCrumbs` + affected KPIs; click → `RcaDrawer` |
| `RcaChain` | Vertical stepper: at each node before → now, multiplier, "explains %" bar, badge + evidence on the root node |
| `PathCrumbs` | Single-line path summary ("Why:" block in list/incident rows) |
| `KpiStrip` | The app's KPI cards on the client app service page; root cause sentence if broken |

New component: add under `components/ui/`, export from `index.ts`, put an example on the `/design-system` page, update this table.

Assistant patterns (`screens/Assistant.tsx`, `screens/assistantWidgets.tsx`, ADR-033, `.ai/assistant.md`): two columns — thread history (`--sidebar-w`, New chat, inline rename, two-step delete) and the chat card (header with provider badge: `warning` **Offline mock** / `accent` model id). User messages are `--accent-soft` bubbles on the right with mention chips (`RefChip`: `accent` kind badge + label) and image thumbnails; assistant replies are left-aligned with an avatar disc and render blocks in order: `Markdown` text, tool rows (collapsible `<details>`, spinner while running, `--status-good` check / `--status-critical` triangle + "Used/Failed" text), **approval cards** (`--status-warning` border + soft background while pending, scalar input fields as a `dl`, full request JSON collapsible, Approve = `primary` / Reject = `secondary`; neutral with an Approved/Rejected badge once decided) and widgets. `AssistantWidget`: entity cards (service, endpoint, incident, KPI, monitor, trace, load test run, logs — kind eyebrow, title, facts line, `StatusBadge`, ghost **Open →**) read the dashboard data layer; `chart` = `TimeSeries` (single axis, series slots 1..4 in order), `table` = `Table` (row links), `stats` = label/value grid with `StatusBadge`. Only `/…` links are followed. Composer: auto-growing textarea in a bordered box (focus ring), `@` opens a listbox picker above it (arrow keys / Enter / Tab / Esc), image attach button + paste + drop (64px thumbnails with upload spinner / error state), Send (`primary` icon) ↔ Stop while streaming. Loading a thread uses `SkeletonText`. Style `styles/assistant.css`.

## Data viz rules

Charts live under `components/charts`, pure SVG: `TimeSeries`, `Columns` (stacked), `Sparkline`, `BarList`, `PropBar`, `Legend`, `PartitionBars`.
`PartitionBars`: lag columns per partition (single series, single color); the tallest column is directly labeled, value on hover. The goal is to show skew (one tall column = hot partition).
`TimeSeries` x label defaults to hours; with seconds for ranges shorter than 5 min; `formatX` for relative time (e.g. planned load `+2:00`). If there are fewer points than ticks, labels are not printed twice.

- **Time ranges (topbar):** `15m` (15×1 min), `30m` (30×1 min), `1h` (60×1 min), `6h` (72×5 min), `24h` (96×15 min), `7d` (84×2 h) — `RANGE_POINTS` (`data/apm.ts`); the api `timerange` package must return the same buckets, otherwise the UI draws an empty axis aligned to now (no data, zero series).
- **Form choice:** time → `TimeSeries` (line, optional `area` if single series); part-to-whole over time → `Columns`; ranked comparison → `BarList`; single ratio → `PropBar`; single number → `StatTile` (not a chart).
- **Categorical color:** `--series-1..8`, fixed order (blue, orange, aqua, yellow, magenta, green, violet, red). Color is bound to the **entity**, not the order — use `serviceSlot(id)` for services. More than 8 series → "Other" or small multiples, never a generated color.
- **Scatter/multi-pair forms:** at most the first 3 slots.
- **In multi-line charts and endpoint stacks** the `LINE_SLOTS` (`1,2,3,7,5,6,8`) order is used — yellow (4) is skipped, because orange↔yellow lines cannot be told apart, especially in dark mode. The order passed the validator (in light, red↔green CVD ΔE 7.2: compensated in stacks with 2px gap + legend + tooltip). Service colors `serviceSlot(id)`, endpoint colors by the fixed order within the service.
- **Small multiples if scales differ a lot.** E.g. on Overview each service in its own card, at its own scale, traffic + error rate (two separate charts). If a large series flattens small ones to zero in one chart, split them.
- **Trend in a table row:** `Sparkline` (each row its own scale), next to the value column.
- **Single y-axis.** Dual-axis is forbidden; different scales go in separate charts (e.g. load test: VU, RPS, p95 separate cards).
- **Mark spec:** line 2px, marker r=4 + 2px surface ring, bar ≤ 24px thick, 4px rounded data end / square base, 2px gap between stacked segments, area 10% opacity, grid 1px solid.
- **Legend:** always with ≥ 2 series; none with a single series (the title says it).
- **Text does not wear the series color.** Values/labels in ink tokens; identity comes from the colored mark next to them.
- **Hover default:** line/area → crosshair + tooltip; columns → per-column tooltip.
- **Threshold line:** `--status-critical` dashed + label (`threshold` prop). Deploy/event: `events` prop (`{ ts, label, detail? }` — vertical dashed line + label; `detail` in the hover tooltip, e.g. deploy commit message · author · source). Deploy markers are provided via `deployEvents(service, from)`.
- **Time spent categories** (`TIME_CATEGORIES`): App code 1, Database 2, Cache 3, External HTTP 7, Queue publish 5, AI / LLM 6 (dep_kind `ai`, ADR-023).
- **FlameGraph** (`components/charts/FlameGraph.tsx`, ADR-022): icicle (root on top), DOM blocks, row 20px. Color fixed per package/module
  (`framePackage()` → hash → `--series-1..8`, 32% tint via `color-mix` so text stays readable; special frames like `all` / `[pruned]` / `(root)`
  `--bg-sunken`). Click → zoom (ancestors as faded full-width rows on top), breadcrumb + Reset, search (non-matching faded, matching `--accent-warm`
  border, "x% of total matches"), hover tooltip: total / self value + share of total (+ share of the zoomed node). Diff mode (`baseTotal` + `baseValue`
  on nodes): color = difference in share of total, the `--diff-*` pair, with `FlameDiffLegend`. Snapshot types (heap, goroutine) are shown per profile.
- **Status colors are not used as series.** For the 5xx/error series use `--series-8` (red), not `--status-critical`.
- Status thresholds (Web Vitals etc.) are shown in tables with `StatusBadge`; a good value is a plain number.

## Screen patterns

- **List screen:** PageHeader → filter row (Segmented + Search) → `Table` inside `Card flush`.
- **Detail screen:** PageHeader with breadcrumb (at most 1 primary on the right) → 4 StatTiles → grid of 2–3 charts → Tabs → table/list.
- **Suggestion card (from code):** title + criticality + type badges → target → condition (mono) → rationale → `file:line · confidence` → Dismiss / Edit / Accept… (Accept… opens `AcceptSuggestionDrawer`; the monitor is created from there).
- Things that come from code are marked with a `Sparkles`/`Code2` icon + `accent` badge.

## Page composition (mandatory)

1. **Order of importance = vertical order = area.** Every page is a stack of `Section`s:
   1. *What is broken right now* (firing incident, failing dependency, running test) — at the top if any, otherwise not rendered at all.
   2. *Main metrics* — with `MetricCard`, large; the most important one `span-2` / `metric--hero`.
   3. *Breakdowns* — the main list/table (endpoints, pages, monitors).
   4. *Secondary information* — small cards, at the bottom.
2. **One piece of information once.** The same metric does not appear in two places on a page: `StatTile` + separate chart card is forbidden → `MetricCard` (value + delta + chart in one card). A table column and the list next to it do not repeat the same number.
3. **Summary on the page, detail on click.** Clicking a list row / card opens a `Drawer` (right panel): time charts, breakdowns, related records live there. For entities with their own full page (service, endpoint, run), an "Open" action in the drawer header.
4. Clickable rows/cards carry a visual hint (`card--interactive`, row hover, chevron).
5. Spacing between page sections `--space-8`, within a section `--space-4`.

| Component | Usage |
|---|---|
| `Section` | Title + subtitle + action; groups related cards |
| `MetricCard` | Value + delta + hint + chart. `className="metric--hero span-2"` for the main metric |
| `Delta` | Standalone delta label |
| `Drawer`, `useDrawer`, `DrawerSection`, `KeyValues` | Detail panel on click |
