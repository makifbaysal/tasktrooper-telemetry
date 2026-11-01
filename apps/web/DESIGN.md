---
name: TaskTrooper Telemetry
description: Observability dashboard in the TaskTrooper theme — a quiet console where data and status do the talking, fronted by a navy brand field on the public pages.
colors:
  trooper-navy: "oklch(0.264 0.079 268.5)"
  trooper-navy-hover: "oklch(0.32 0.09 268.5)"
  trooper-navy-text: "oklch(0.455 0.118 265)"
  trooper-navy-soft: "oklch(0.455 0.118 265 / 0.10)"
  brand-mark: "#15214b"
  signal-amber: "oklch(0.818 0.112 72.9)"
  signal-amber-soft: "oklch(0.818 0.112 72.9 / 0.16)"
  cold-slate-page: "oklch(0.962 0.007 265)"
  cold-slate-surface: "oklch(0.99 0.003 265)"
  cold-slate-raised: "oklch(0.995 0.002 265)"
  cold-slate-sunken: "oklch(0.945 0.01 265)"
  cold-slate-active: "oklch(0.935 0.014 265)"
  ink-primary: "oklch(0.145 0.02 265)"
  ink-secondary: "oklch(0.35 0.02 265)"
  ink-muted: "oklch(0.5 0.02 265)"
  ink-inverse: "oklch(0.985 0.002 247)"
  border: "oklch(0.895 0.012 265)"
  border-strong: "oklch(0.82 0.015 265)"
  divider: "oklch(0.915 0.01 265)"
  status-good: "oklch(0.55 0.15 145)"
  status-warning: "oklch(0.75 0.15 85)"
  status-serious: "oklch(0.68 0.15 50)"
  status-critical: "oklch(0.577 0.22 27)"
  series-1: "#2a78d6"
  series-2: "#eb6834"
  series-3: "#1baf7a"
  series-4: "#eda100"
  series-5: "#e87ba4"
  series-6: "#008300"
  series-7: "#6250d6"
  series-8: "#e34948"
  diff-increase: "oklch(0.65 0.17 52)"
  diff-decrease: "oklch(0.56 0.14 245)"
  field-navy: "oklch(0.22 0.07 268)"
  field-raised: "oklch(0.265 0.075 268)"
  field-line: "oklch(0.36 0.06 268)"
  field-line-strong: "oklch(0.47 0.06 268)"
  field-ink: "oklch(0.97 0.006 265)"
  field-ink-secondary: "oklch(0.85 0.025 265)"
  field-ink-muted: "oklch(0.74 0.03 265)"
  field-signal-ink: "oklch(0.89 0.075 75)"
typography:
  public-display:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(40px, 3.2vw + 20px, 68px)"
    fontWeight: 600
    lineHeight: 1.04
    letterSpacing: "-0.035em"
  public-headline:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(28px, 1.6vw + 18px, 40px)"
    fontWeight: 600
    lineHeight: 1.12
    letterSpacing: "-0.025em"
  public-lede:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "clamp(16px, 0.3vw + 15px, 19px)"
    fontWeight: 400
    lineHeight: 1.55
  display:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize: "28px"
    fontWeight: 600
    lineHeight: "36px"
    letterSpacing: "-0.02em"
    fontFeature: "tnum"
  headline:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "20px"
    fontWeight: 600
    lineHeight: "28px"
  title:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: "24px"
  body:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: "20px"
  caption:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: "16px"
  label:
    fontFamily: "'Inter Variable', Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "11px"
    fontWeight: 500
    lineHeight: "14px"
  mono:
    fontFamily: "'JetBrains Mono', ui-monospace, 'SF Mono', Menlo, Consolas, monospace"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: "16px"
rounded:
  xs: "4px"
  sm: "6px"
  md: "8px"
  lg: "14px"
  full: "999px"
spacing:
  "1": "4px"
  "2": "8px"
  "3": "12px"
  "4": "16px"
  "5": "20px"
  "6": "24px"
  "8": "32px"
  "10": "40px"
  "12": "48px"
  "16": "64px"
  "24": "96px"
  "32": "128px"
components:
  button-primary:
    backgroundColor: "{colors.trooper-navy}"
    textColor: "{colors.ink-inverse}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    padding: "0 16px"
    height: "36px"
  button-primary-hover:
    backgroundColor: "{colors.trooper-navy-hover}"
  button-secondary:
    backgroundColor: "{colors.cold-slate-page}"
    textColor: "{colors.ink-primary}"
    rounded: "{rounded.md}"
    padding: "0 16px"
    height: "36px"
  button-secondary-hover:
    backgroundColor: "{colors.cold-slate-active}"
  button-ghost:
    textColor: "{colors.ink-secondary}"
    rounded: "{rounded.md}"
    height: "36px"
  button-danger:
    backgroundColor: "{colors.status-critical}"
    textColor: "{colors.ink-inverse}"
    rounded: "{rounded.md}"
    height: "36px"
  button-sm:
    typography: "{typography.caption}"
    padding: "0 12px"
    height: "30px"
  input:
    backgroundColor: "{colors.cold-slate-page}"
    textColor: "{colors.ink-primary}"
    rounded: "{rounded.md}"
    height: "36px"
  card:
    backgroundColor: "{colors.cold-slate-surface}"
    rounded: "{rounded.md}"
    padding: "16px"
  badge:
    backgroundColor: "{colors.cold-slate-sunken}"
    textColor: "{colors.ink-secondary}"
    typography: "{typography.label}"
    rounded: "{rounded.full}"
    padding: "0 10px"
    height: "22px"
  badge-critical:
    textColor: "{colors.status-critical}"
    rounded: "{rounded.full}"
  segmented:
    backgroundColor: "{colors.cold-slate-sunken}"
    rounded: "{rounded.md}"
    padding: "3px"
  segmented-item-selected:
    backgroundColor: "{colors.cold-slate-page}"
    textColor: "{colors.ink-primary}"
    rounded: "{rounded.sm}"
  table-header:
    textColor: "{colors.ink-secondary}"
    typography: "{typography.caption}"
    padding: "8px 12px"
  nav-item-active:
    backgroundColor: "{colors.cold-slate-active}"
    textColor: "{colors.ink-primary}"
    rounded: "{rounded.md}"
  skeleton:
    backgroundColor: "{colors.cold-slate-sunken}"
    rounded: "{rounded.xs}"
    height: "12px"
  public-button-light:
    backgroundColor: "{colors.field-ink}"
    textColor: "{colors.field-navy}"
    typography: "{typography.body}"
    rounded: "{rounded.md}"
    padding: "0 20px"
    height: "44px"
  public-button-outline:
    textColor: "{colors.field-ink}"
    rounded: "{rounded.md}"
    padding: "0 20px"
    height: "44px"
  public-button-outline-hover:
    backgroundColor: "{colors.field-raised}"
  public-code-block:
    backgroundColor: "{colors.field-navy}"
    textColor: "{colors.field-ink-secondary}"
    typography: "{typography.mono}"
    rounded: "{rounded.lg}"
    padding: "16px"
---

# Design System: TaskTrooper Telemetry

<!-- Source of truth for values: src/styles/tokens.css. Rules and component inventory: .ai/design-system.md.
     Theme source: the TaskTrooper app (tasktrooper-company/desktop/ui/src/styles/globals.css).
     App shell: styles/components.css. Public pages (/, /docs, /login): styles/public.css. -->

## Overview

**Creative North Star: "The Incident Console"**

The dashboard is the desk a backend developer sits down at when something is wrong. The chrome — sidebar, topbar,
cards, controls — is a quiet frame of cold slate neutrals and one navy voice; everything loud on screen is either data
(charts in a fixed categorical order) or status (always an icon with a label). Nothing decorates. If a pixel is coloured,
it is measuring something or saying something is broken.

Density is high but legible: 14px body on a 4px grid, flat hairline cards, sentence-case labels, 32px between page
regions. Hierarchy comes from weight and size steps, not from colour, shadow or ornament. The system follows the
TaskTrooper app theme so the product family reads as one; light and dark are equal citizens, with dark built from its own
chosen steps rather than an inversion.

Signed out, the same console is introduced on **the Field**: a deep navy ground (`--field-*`, identical in both themes)
with a faint dot measurement grid, on which a layered service map draws itself, turns hot along one path and settles on
a root cause. The Field is the brand ground of the public pages only — hero, code band, code blocks, sign-in panel;
everything else on those pages sits on the app's own page colour and follows the theme. Inside the app shell the Field
never appears.

Motion is a quiet layer on top of state. All decorative motion switches off with the OS "reduce motion" setting or the
account menu's **Reduce motion**; the public hero then shows its final frame.

**Key Characteristics:**
- Cold slate neutrals (hue 265), one navy primary, warm amber only for selection, highlight and the Field's signal.
- Colour is reserved for data and status; status is never colour alone.
- Inter for interface, JetBrains Mono for routes, logs, trace ids and code.
- Tabular numbers everywhere a value is compared.
- Flat surfaces: cards are a 1px divider hairline on the surface tone, no resting shadow; shadows only for popovers, menus and drawers.
- Loading is the shape of the content (shimmer skeletons), never a centred spinner or "Loading…".

## Colors

A cool, low-chroma slate canvas with a single deep navy voice, a warm amber reserved for "this is the one you picked",
and a darker navy field that carries the brand on the public pages.

### Primary
- **Trooper Navy** (`trooper-navy`): the one filled action per page (primary button), selected tab underline and link text
  (via `trooper-navy-text`), focus rings. Its soft tint (`trooper-navy-soft`) backs accent badges, callouts and avatars.
- **Brand Mark** (`brand-mark`, #15214b): the logo square only; identical in both themes.

### Secondary
- **Signal Amber** (`signal-amber`): selection and highlight — text selection, the click ring in session replay, the
  current selection in a chart or list, the selected public SDK tab. On the Field it is the signal (`--field-signal`):
  the hot path, the cause node, the finding band. Never a status, never an app button fill.

### Tertiary
- **Categorical series** (`series-1` … `series-8`, blue → orange → aqua → yellow → magenta → green → violet → red): chart
  lines and bars in this fixed order. `series-8` (red) doubles as the error series in charts.
- **Diff pair** (`diff-increase` warm orange, `diff-decrease` blue): profile and comparison diffs only.

### Neutral
- **Cold Slate Page** (`cold-slate-page`): the page and sidebar background; also the fill of inputs and secondary buttons.
- **Cold Slate Surface** (`cold-slate-surface`): cards and chart surfaces.
- **Cold Slate Raised** (`cold-slate-raised`): popovers, menus, tooltips, evidence rows.
- **Cold Slate Sunken** (`cold-slate-sunken`): wells, code blocks, segmented-control track, table header tint, skeletons.
- **Cold Slate Active** (`cold-slate-active`): selected nav item and pressed rows; hover uses the same tone at 60%.
- **Ink** (`ink-primary`, `ink-secondary`, `ink-muted`): text hierarchy; muted is for axes, meta and placeholders.
- **Divider / Border / Border Strong**: divider is the card edge and table rule; border edges controls and the shell;
  border-strong is hover and emphasis.

### The Field (public pages)
- **Field Navy** (`field-navy`): hero, code band, public code blocks, sign-in panel, signed-out boot screen.
- **Field Raised** (`field-raised`): service-map node boxes, outline-button hover.
- **Field Line / Line Strong**: the dot grid, tier separators, code-band rules / edges, node strokes, outline buttons.
- **Field Ink / Secondary / Muted**: headline, lede, meta on the navy ground.
- **Field Signal Ink** (`field-signal-ink`): amber text on navy (finding label, hot metrics, matched code lines).

### Status
`status-good`, `status-warning`, `status-serious`, `status-critical` — each with a `-soft` background and a `-text`
foreground in `tokens.css`. Mapping: ok/resolved → good, warning/pending → warning, acknowledged → serious,
critical/firing → critical, muted → neutral.

### Named Rules
**The Measuring-or-Broken Rule.** Colour appears only when it encodes data or status. Chrome stays slate and navy.

**The Icon-Plus-Label Rule.** A status colour never stands alone: every status renders through `StatusBadge` (icon + word).

**The Fixed Order Rule.** Categorical series are assigned `series-1..8` in order; status colours are never reused as series
or as diff colours.

**The Field Stays Outside Rule.** `--field-*` and the fluid public type steps belong to the public pages and the signed-out
boot screen; the app shell never uses them.

## Typography

**Body Font:** Inter Variable / Inter (with ui-sans-serif, system-ui, -apple-system, Segoe UI)
**Label/Mono Font:** JetBrains Mono (with ui-monospace, SF Mono, Menlo, Consolas)

**Character:** Inter carries the interface at small sizes without fuss and the public headlines at large ones with tight
tracking; JetBrains Mono marks anything a developer would copy — a route, a trace id, a log line, a service-map node name
— so it reads as machine text at a glance.

### Hierarchy
- **Display** (600, 28/36px, -0.02em, tabular): page title (PageHeader h1, -0.022em) and stat / metric values. One
  `hero` number (`--text-hero`, 44/48px) per page at most.
- **Headline** (600, 20/28px): default h1 outside the PageHeader; public surface column headings.
- **Title** (600, 16/24px): section, card, drawer and KPI-card titles.
- **Body** (400, 14/20px): default for everything, including table cells and metric labels (medium, secondary ink).
- **Caption** (400, 12/16px): secondary text, meta, table headers (medium, secondary ink) and the small `eyebrow` label.
- **Label** (500, 11/14px): badges, key/value terms, chart ticks.
- **Mono** (400, 12–14px): routes, logs, code, trace ids, path chips.

Public pages only:
- **Public Display** (600, 40→68px fluid, 1.04, -0.035em): the landing hero h1.
- **Public Headline** (600, 28→40px fluid, 1.12, -0.025em): section headings, docs h1, the sign-in statement.
- **Public Lede** (400, 16→19px fluid, 1.55): hero and docs intro paragraphs; long copy capped at `--measure` (68ch).

### Named Rules
**The Tabular Rule.** Every number that sits next to another number uses `tabular-nums`; large values tighten to -0.02em.

**The Mono-Means-Machine Rule.** Monospace is only for text the user would copy or match against code.

**The Sentence-Case Rule.** Labels — metric labels, table headers, the `eyebrow` small label — are sentence case with no
tracking. Uppercase with 0.06em tracking is kept only for sidebar group names. A small label names a field or a block of
content; it never sits as a kicker above a heading.

## Layout

App shell: a 240px sticky sidebar, a 56px topbar and content capped at 1440px, padded 32px with 32px between page
regions (16px padding below 900px). On wide screens the topbar toggle collapses the sidebar (remembered). Below 900px the
sidebar is an overlay drawer over a `--scrim`, opened by the same toggle and closed by navigation, Escape or a tap on the
scrim; below 640px the topbar wraps so the time range takes its own full-width row and the page never scrolls sideways.
Spacing follows a 4px grid (`--space-1..12`); cards in a grid sit 16px apart, card bodies pad 16px. Controls are 36px
(`md`) or 30px (`sm`) tall. Grids step 4 → 2 columns at 1100px and to one at 720px.

Pages read top to bottom: `PageHeader` (breadcrumb, title, subtitle, actions on the right), then sections. A top-level
card whose body is a table is a page region, not a box: no fill or side walls, the table sits on the page background
(`--table-cover: --bg-page`) aligned with the section heading. Service pages split into tabs; KPIs, alerts and logs live
under their owning service, never duplicated elsewhere.

Public pages (`publicRouter.tsx`): one column capped at `--public-max` (1200px) with a fluid gutter (16→64px); nav (72px,
`--public-nav-h`), hero and every section align to the same inline edge. Sections pad 96px vertically (64px below 720px),
separated by divider hairlines; section heads are a 5:6 two-column grid (heading | supporting copy). The hero is a 4:7
copy | service-map grid, stacking at 1080px. Docs is a 14rem sticky rail + a 48rem body; at 1080px the rail becomes a
horizontal sticky strip. Sign-in is a 5:6 split — Field panel left, form (24rem) right — collapsing to one column at
1080px with the graph hidden. Content groups on public pages are rows and rules (signals list, surfaces columns with a
2px ink top rule, a timeline), not card grids.

## Elevation & Depth

Flat. Cards carry no resting shadow: depth comes from tone (page → surface) and a 1px `divider` hairline. A card inside
a card is a region of its parent — no fill, no border, a divider hairline between siblings. Shadows are reserved for
things that float above the page: menus, popovers, the quick-jump list and drawers (`--shadow-lg`), and the selected
segmented item and toggle knob (`--shadow-sm`). Dark theme uses deeper, softer shadows from its own steps. On the Field,
glow (`drop-shadow` in signal amber) marks only the hot path and traffic dots of the service map.

### Shadow Vocabulary
- **Raised** (`--shadow-sm`: `0 1px 2px 0 rgb(0 0 0 / 0.06), 0 1px 3px 0 rgb(0 0 0 / 0.08)`): selected segmented item, toggle knob.
- **Floating** (`--shadow-md`: `0 4px 16px -4px rgb(0 0 0 / 0.12), 0 1px 3px 0 rgb(0 0 0 / 0.06)`): tooltips; peak of the motion layer's ambient card pulse.
- **Overlay** (`--shadow-lg`: `0 8px 24px -4px rgb(0 0 0 / 0.18), 0 2px 8px -2px rgb(0 0 0 / 0.12)`): menus, popovers, quick-jump list, drawers, the mobile nav drawer.

### Named Rules
**The Flat Card Rule.** Cards rest flat: 1px `divider`, `radius-md`, no shadow. Hover on an interactive card changes only
the border (to `border-strong`).

**The Opt-Out Motion Rule.** Decorative app motion lives in one place (`styles/motion.css` + `components/layout/motion.ts`)
and is driven by `data-*` attributes that are only set when motion is allowed: card reveal (`--dur-reveal`, travel
`--reveal-shift`, staggered 75ms), ambient card pulse, liquid cursor (accent tints, never hides the system cursor, off for
touch and text inputs) and sidebar collapse (`--dur-slow`). The public service map runs on its own scene clock with
`--ease-out-expo` and renders its final frame (`[data-static]`) when motion is reduced. `prefers-reduced-motion` or
`:root[data-motion='reduced']` turns all of it off, skeleton shimmer included; state transitions (hover, focus, press)
stay within 120–200ms on `--ease`.

**The Alarm Line Rule.** A card in an alarm state (`card--rail-*`, the root-cause card) keeps its hairline frame, tints the
border toward the status colour (45%) and draws a 2px inset line along its top edge. No coloured fill and no thick side stripe.

## Shapes

Gently rounded, never pill-shaped except where a shape names a count or state. Skeleton bars and bar ends 4px, chips and
segmented items 6px, buttons, inputs, nav items and cards 8px; the boot status card and public code blocks 14px; badges,
meters and toggles fully round. Edges are 1px hairlines (`divider` on cards and rules, `border` on controls);
`border-strong` marks hover. On the Field, nodes are rounded rectangles with a 1px strong-line stroke and the dot grid is
32px.

## Components

### Buttons
Precise and quiet: one filled navy button per page, everything else neutral.
- **Shape:** gently rounded (8px), 36px tall (30px small), 16px horizontal padding (12px small).
- **Primary:** Trooper Navy fill, inverse text; hover deepens to `trooper-navy-hover`. Use once per page.
- **Secondary (default):** page-coloured fill with a 1px border; hover fills with `cold-slate-active`.
- **Ghost:** no fill or border, secondary ink; hover darkens the ink.
- **Danger:** critical fill, inverse text — destructive confirmations only.
- **Press:** scales to 0.98 on active. **Disabled:** 50% opacity, not-allowed cursor; when a role blocks the action, say why in `title`.
- **Busy:** an in-button spinning icon replaces the leading icon while the action runs.
- **Icon-only:** square at control height and always carries an `aria-label`.
- **Public buttons:** 44px tall, 20px padding, 8px radius. On the Field: **light** (field-ink fill, navy text; hover mixes
  toward amber) and **outline** (field-line-strong edge; hover fills field-raised). On the page: **solid** (navy) and
  **ghost** (border-strong edge). The nav's sign-in is a 36px **quiet** outline.

### Badges & Status
- **Badge:** 22px, fully round, 11px medium label. Neutral tone sits on `cold-slate-sunken` with a border; toned badges
  (`accent`, `good`, `warning`, `serious`, `critical`) use the `-soft` background and `-text` foreground, no border.
- **StatusBadge:** the only way to show status — icon + label in the status tone.

### Cards / Containers
- **Corner Style:** 8px (`radius-md`).
- **Background:** `cold-slate-surface` with a 1px `divider` border.
- **Shadow Strategy:** none at rest (see the Flat Card Rule); interactive cards change border only on hover.
- **Nesting:** a nested card drops its fill, border and radius and becomes a region, divider hairline between siblings.
- **Internal Padding:** 16px; header 48px minimum with title, subtitle and actions; `flush` variant for tables and lists.
- **Alarm state:** status-tinted border + 2px inset top line (the Alarm Line Rule).

### Loading
- **Skeleton family** (`Skeleton`, `SkeletonText`, `SkeletonRows`, `SkeletonPage`): blocks in the shape of the content,
  `cold-slate-sunken` with a soft 1.6s shimmer, 4px radius (6/8px/full variants). One `role="status"` with a label per group.
- **Boot:** signed in → an app-shell skeleton (sidebar, topbar, page tiles); signed out → the bare Field navy so nothing flashes.
- **Spinners** only inside a busy button and over session-replay buffering.

### Inputs / Fields
- **Style:** page-coloured fill, 1px `border`, 8px radius, 36px tall; muted placeholder.
- **Focus:** border turns Trooper Navy with a 3px `trooper-navy-soft` halo.
- **Family:** `Field` (12px medium secondary label), `Input`, `Textarea` (mono variant for JSON/headers), `SearchInput`
  (leading icon), `Select`, `Toggle`.

### Tabs & Segmented
- **Tabs:** page sections. Muted label, 2px underline; selected tab turns navy text with a navy underline. Arrow keys, Home and End move between tabs; when the row overflows, the clipped edge fades so hidden tabs are discoverable.
- **Segmented:** filters and time ranges. Sunken 3px track, 6px items; the selected item lifts onto the page colour with the Raised shadow.
- **ChoiceChips:** the segmented look with multiple `aria-pressed` items, wrapping on narrow screens.
- **Public tabs** (SDK install): secondary ink, selected tab primary ink with a 2px amber underline.

### Tables
Dense and scannable. Headers are 12px medium secondary ink in sentence case on a lightly sunken band, ruled top and bottom,
sticky; cells pad 12px with divider rules; numeric columns right-aligned and tabular; rows highlight on hover and may open
detail on click; child rows indent under their parent. A table wider than its container scrolls inside it, with edge
shadows marking the clipped side. Top-level table cards open onto the page background (see Layout).

### Navigation
Sidebar items are 8px-rounded 36px rows with an icon and label; hover uses the hover tone, the current page fills with
`cold-slate-active` and primary ink. Group names are the one uppercase label. The topbar (translucent surface, blurred)
holds the sidebar toggle, the quick jump (services and pages; `/` or ⌘K focuses it, arrows + Enter open a result), the
env and time-range scope (only on screens that read them) and the theme switch (`system | light | dark`). The account
menu sits at the bottom of the sidebar.

Public nav: 72px, brand mark + name, text links, sign-in on the right. Over the hero it is transparent on the Field
(field inks); on docs it is sticky on a 92% page tint with blur and a divider. Below 720px only the Docs link stays.

### Root Cause Card (signature)
Sits above the golden signals on service and endpoint pages, only when there is a clear regression. An origin label
("Your code" / "Your dependency" / "Not your code · downstream"), one sentence naming the cause, a single-line path of
crumbs through the call tree and the affected KPIs; it opens a drawer with the step-by-step chain (before → now,
multiplier, "explains %" bar, evidence at the root). Framed by the Alarm Line Rule.

### Service Map (public signature)
The landing hero's scene (`components/public/ServiceMap.tsx`): a layered graph (clients → edge → services → data) on the
Field with lane labels and dashed tier separators. Nodes are field-raised boxes with a lucide icon, a mono name and a mono
tabular metric. Beats: edges draw tier by tier, traffic dots flow, a deploy pin drops, the slow path turns amber bottom-up,
a comet scan walks from the user to the database, the cause node ripples and is tagged. Then the **finding** settles in:
on wide screens laid over the graph's last lane below the map and fed by a dashed leader from the cause node; at 1080px and
below, the same band under the graph. It is a faint amber band with a top rule, not a card — an amber label, a confidence
note and three numbered steps (mono numerals in amber rings). The sign-in panel reuses the narrow graph (no finding); hovering a node dims everything it does not touch.

### Code Blocks (public)
Always on the Field: navy fill, 1px field-line edge, 14px radius, a top bar with the file label and a quiet copy button;
mono 12px at 1.7 line height, ligatures off. The code → monitors band highlights matched lines with a 14% amber wash.

## Do's and Don'ts

### Do:
- **Do** reference tokens from `src/styles/tokens.css` for every colour, size, space and radius.
- **Do** render status through `StatusBadge` (icon + label) and alarm cards through the tinted border + 2px top line.
- **Do** keep cards flat: 1px `divider`, 8px radius, border-only hover; flatten cards nested in cards.
- **Do** write labels in sentence case; keep uppercase for sidebar group names only.
- **Do** show loading as skeletons in the shape of the content; keep spinners inside busy buttons and replay buffering.
- **Do** assign chart series `series-1..8` in order and keep one y-axis per chart.
- **Do** use JetBrains Mono for routes, logs, trace ids and code; tabular numbers for compared values.
- **Do** keep exactly one primary (navy) button per page.
- **Do** build dark theme from its own token steps; check both themes.
- **Do** keep the Field (`--field-*`, `--text-display/headline/lede`, `--space-16+`) on the public pages, aligned to the shared `--inline` edge.

### Don't:
- **Don't** write raw hex, raw px font sizes or ad-hoc spacing in components.
- **Don't** use status colours for decoration, series or diffs, or colour as the only status signal.
- **Don't** use dual y-axes.
- **Don't** give cards a resting shadow, a thick coloured side stripe or a coloured fill for alarm state.
- **Don't** put a kicker or eyebrow label above a heading.
- **Don't** show a centred spinner or "Loading…" text for page or panel loading.
- **Don't** add decorative motion outside `motion.css` / `motion.ts` (app) or the service map's scene clock (public), or any motion that ignores the reduce-motion switches.
- **Don't** introduce a second accent hue for chrome (violet primary, green "signal" nav): the chrome voice is Trooper Navy.
- **Don't** use provider brand colours or logos for third-party identity; use the neutral `ProviderMark`.
