---
name: TaskTrooper Telemetry
description: Observability dashboard in the TaskTrooper theme — a quiet console where data and status do the talking.
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
typography:
  display:
    fontFamily: "Inter, ui-sans-serif, system-ui, -apple-system, 'Segoe UI', sans-serif"
    fontSize: "28px"
    fontWeight: 600
    lineHeight: "36px"
    letterSpacing: "-0.02em"
    fontFeature: "tnum"
  headline:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "20px"
    fontWeight: 600
    lineHeight: "28px"
  title:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "16px"
    fontWeight: 600
    lineHeight: "24px"
  body:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "14px"
    fontWeight: 400
    lineHeight: "20px"
  caption:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "12px"
    fontWeight: 400
    lineHeight: "16px"
  label:
    fontFamily: "Inter, ui-sans-serif, system-ui, sans-serif"
    fontSize: "11px"
    fontWeight: 500
    lineHeight: "14px"
    letterSpacing: "0.06em"
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
    backgroundColor: "{colors.cold-slate-raised}"
    textColor: "{colors.ink-primary}"
    rounded: "{rounded.md}"
    height: "36px"
  card:
    backgroundColor: "{colors.cold-slate-surface}"
    rounded: "{rounded.lg}"
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
    textColor: "{colors.ink-muted}"
    typography: "{typography.label}"
    padding: "8px 12px"
  nav-item-active:
    backgroundColor: "{colors.cold-slate-active}"
    textColor: "{colors.ink-primary}"
    rounded: "{rounded.md}"
---

# Design System: TaskTrooper Telemetry

<!-- Source of truth for values: src/styles/tokens.css. Rules and component inventory: .ai/design-system.md.
     Theme source: the TaskTrooper app (tasktrooper-company/desktop/ui/src/styles/globals.css). -->

## Overview

**Creative North Star: "The Incident Console"**

The dashboard is the desk a backend developer sits down at when something is wrong. The chrome — sidebar, topbar,
cards, controls — is a quiet frame of cold slate neutrals and one navy voice; everything loud on screen is either data
(charts in a fixed categorical order) or status (always an icon with a label). Nothing decorates. If a pixel is coloured,
it is measuring something or saying something is broken.

Density is high but legible: 14px body on a 4px grid, tight tables, 16px between cards. Hierarchy comes from weight and
size steps, not from colour or ornament. The system follows the TaskTrooper app theme so the product family reads as one;
light and dark are equal citizens, with dark built from its own chosen steps rather than an inversion.

Motion is a quiet layer on top of state: cards settle in as they scroll into view and breathe faintly while on screen,
a liquid cursor follows the pointer and morphs onto controls, and every interaction answers in 120–320ms. All of it is
decorative, so all of it switches off with the OS "reduce motion" setting or the account menu's **Reduce motion**.

**Key Characteristics:**
- Cold slate neutrals (hue 265), one navy primary, warm amber only for selection and highlight.
- Colour is reserved for data and status; status is never colour alone.
- Inter for interface, JetBrains Mono for routes, logs, trace ids and code.
- Tabular numbers everywhere a value is compared.
- Flat-by-default surfaces with a single light card shadow; depth grows only for popovers and drawers.

## Colors

A cool, low-chroma slate canvas with a single deep navy voice and a warm amber reserved for "this is the one you picked".

### Primary
- **Trooper Navy** (`trooper-navy`): the one filled action per page (primary button), selected tab underline and link text
  (via `trooper-navy-text`), focus rings. Its soft tint (`trooper-navy-soft`) backs accent badges and avatars.
- **Brand Mark** (`brand-mark`, #15214b): the logo square only; identical in both themes.

### Secondary
- **Signal Amber** (`signal-amber`): selection and highlight — the click ring in session replay, the current selection in
  a chart or list. Never a status, never a button fill.

### Tertiary
- **Categorical series** (`series-1` … `series-8`, blue → orange → aqua → yellow → magenta → green → violet → red): chart
  lines and bars in this fixed order. `series-8` (red) doubles as the error series in charts.
- **Diff pair** (`diff-increase` warm orange, `diff-decrease` blue): profile and comparison diffs only.

### Neutral
- **Cold Slate Page** (`cold-slate-page`): the page and sidebar background.
- **Cold Slate Surface** (`cold-slate-surface`): cards and chart surfaces.
- **Cold Slate Raised** (`cold-slate-raised`): inputs, popovers, tooltips.
- **Cold Slate Sunken** (`cold-slate-sunken`): wells, code blocks, segmented-control track, table header tint.
- **Cold Slate Active** (`cold-slate-active`): selected nav item and pressed rows; hover uses the same tone at 60%.
- **Ink** (`ink-primary`, `ink-secondary`, `ink-muted`): text hierarchy; muted is for axes, meta and placeholders.
- **Border / Border Strong / Divider**: card and control edges, input edges and hover underlines, table rules.

### Status
`status-good`, `status-warning`, `status-serious`, `status-critical` — each with a `-soft` background and a `-text`
foreground in `tokens.css`. Mapping: ok/resolved → good, warning/pending → warning, acknowledged → serious,
critical/firing → critical, muted → neutral.

### Named Rules
**The Measuring-or-Broken Rule.** Colour appears only when it encodes data or status. Chrome stays slate and navy.

**The Icon-Plus-Label Rule.** A status colour never stands alone: every status renders through `StatusBadge` (icon + word).

**The Fixed Order Rule.** Categorical series are assigned `series-1..8` in order; status colours are never reused as series
or as diff colours.

## Typography

**Body Font:** Inter (with ui-sans-serif, system-ui, -apple-system, Segoe UI)
**Label/Mono Font:** JetBrains Mono (with ui-monospace, SF Mono, Menlo, Consolas)

**Character:** Inter carries the interface at small sizes without fuss; JetBrains Mono marks anything a developer would
copy — a route, a trace id, a log line — so it reads as machine text at a glance.

### Hierarchy
- **Display** (600, 28/36px, -0.02em, tabular): stat and metric values. One `hero` number (`--text-hero`, 34/40px) per page at most.
- **Headline** (600, 20/28px): page title (h1).
- **Title** (600, 16/24px): section and card titles.
- **Body** (400, 14/20px): default for everything, including table cells and metric labels on Overview.
- **Caption** (400, 12/16px): secondary text and meta.
- **Label** (500, 11/14px, 0.06em, uppercase): table headers, sidebar section labels, badges, chart ticks.
- **Mono** (400, 12–14px): routes, logs, code, trace ids, path chips.

### Named Rules
**The Tabular Rule.** Every number that sits next to another number uses `tabular-nums`; large values tighten to -0.02em.

**The Mono-Means-Machine Rule.** Monospace is only for text the user would copy or match against code.

## Layout

App shell: a 240px sticky sidebar, a 56px topbar and content capped at 1440px. On wide screens the topbar toggle
collapses the sidebar (remembered). Below 900px the sidebar is an overlay drawer over a `--scrim`, opened by the same
toggle and closed by navigation, Escape or a tap on the scrim; below 640px the topbar wraps so the time range takes its
own full-width row and the page never scrolls sideways. Spacing follows a 4px grid
(`--space-1..12`); cards sit 16px apart, card bodies pad 16px. Controls are 36px (`md`) or 30px (`sm`) tall.

Pages read top to bottom: `PageHeader` (breadcrumb, title, subtitle, actions on the right), then sections of cards.
Tables run edge to edge inside a flush card. Service pages split into tabs; KPIs, alerts and logs live under their owning
service, never duplicated elsewhere.

## Elevation & Depth

Flat by default. Cards carry one light shadow (`--shadow-sm`) and a 1px border; that is the only resting depth. Popovers
and menus lift with `--shadow-lg` on the raised surface; drawers sit above the page with the same shadow. Dark theme uses
deeper, softer shadows from its own steps.

### Shadow Vocabulary
- **Raised** (`--shadow-sm`: `0 1px 2px 0 rgb(0 0 0 / 0.06), 0 1px 3px 0 rgb(0 0 0 / 0.08)`): cards, selected segmented item.
- **Floating** (`--shadow-md`: `0 4px 16px -4px rgb(0 0 0 / 0.12), 0 1px 3px 0 rgb(0 0 0 / 0.06)`): tooltips, hovered interactive cards.
- **Overlay** (`--shadow-lg`: `0 8px 24px -4px rgb(0 0 0 / 0.18), 0 2px 8px -2px rgb(0 0 0 / 0.12)`): menus, popovers, drawers.

### Named Rules
**The Opt-Out Motion Rule.** Decorative motion lives in one place (`styles/motion.css` + `components/layout/motion.ts`)
and is driven by `data-*` attributes that are only set when motion is allowed: card reveal (`--dur-reveal`, travel
`--reveal-shift`, staggered 75ms), ambient card pulse (`--dur-ambient`, `--shadow-sm` ↔ `--shadow-md`, border toward
`--accent`), liquid cursor (accent tints, never hides the system cursor, off for touch and text inputs) and sidebar
collapse (`--dur-slow`). `prefers-reduced-motion` or `:root[data-motion='reduced']` turns all of it off; state
transitions (hover, focus, press) stay within 120–200ms on `--ease`. Never add motion outside this layer.

**The Alarm Rail Rule.** A card in an alarm state gets a 4px left status rail (critical or warning) instead of a coloured fill.

## Shapes

Gently rounded, never pill-shaped except where a shape names a count or state. Bar ends 4px, chips and segmented items
6px, buttons, inputs and nav items 8px, cards 14px (the TaskTrooper `rounded-xl`), badges fully round. Edges are 1px
hairlines in `border`; stronger edges (`border-strong`) mark inputs and hover states.

## Components

### Buttons
Precise and quiet: one filled navy button per page, everything else neutral.
- **Shape:** gently rounded (8px), 36px tall (30px small), 16px horizontal padding (12px small).
- **Primary:** Trooper Navy fill, inverse text; hover deepens to `trooper-navy-hover`. Use once per page.
- **Secondary (default):** page-coloured fill with a 1px border; hover fills with `cold-slate-active`.
- **Ghost:** no fill or border, secondary ink; hover darkens the ink.
- **Danger:** critical fill, inverse text — destructive confirmations only.
- **Press:** scales to 0.98 on active. **Disabled:** 50% opacity, not-allowed cursor; when a role blocks the action, say why in `title`.
- **Icon-only:** square at control height and always carries an `aria-label`.

### Badges & Status
- **Badge:** 22px, fully round, 11px medium label. Neutral tone sits on `cold-slate-sunken` with a border; toned badges
  (`accent`, `good`, `warning`, `serious`, `critical`) use the `-soft` background and `-text` foreground, no border.
- **StatusBadge:** the only way to show status — icon + label in the status tone.

### Cards / Containers
- **Corner Style:** 14px.
- **Background:** `cold-slate-surface` with a 1px `border`.
- **Shadow Strategy:** Raised at rest (see Elevation).
- **Internal Padding:** 16px; header 48px minimum with title, subtitle and actions; `flush` variant for tables and lists.
- **Alarm state:** 4px left status rail.

### Inputs / Fields
- **Style:** `cold-slate-raised` fill, 1px `border-strong`, 8px radius, 36px tall; muted placeholder.
- **Focus:** border turns Trooper Navy with a 3px `trooper-navy-soft` halo.
- **Family:** `Field`, `Input`, `Textarea` (mono variant for JSON/headers), `SearchInput` (leading icon), `Select`, `Toggle`.

### Tabs & Segmented
- **Tabs:** page sections. Muted label, 2px underline; selected tab turns navy text with a navy underline. Arrow keys, Home and End move between tabs; when the row overflows, the clipped edge fades so hidden tabs are discoverable.
- **Segmented:** filters and time ranges. Sunken 3px track, 6px items; the selected item lifts onto the page colour with the Raised shadow.
- **ChoiceChips:** the segmented look with multiple `aria-pressed` items, wrapping on narrow screens.

### Tables
Dense and scannable. Headers in the uppercase label style on a lightly sunken band, sticky; cells pad 10px × 12px with
divider rules; numeric columns right-aligned and tabular; rows highlight on hover and may open detail on click; child rows indent under their parent. A table wider than its card scrolls inside it, with edge shadows marking the clipped side.

### Navigation
Sidebar items are 8px-rounded rows with an icon and label; hover uses the hover tone, the current page fills with
`cold-slate-active` and primary ink. Section labels use the uppercase label style. The topbar holds the sidebar toggle,
the quick jump (services and pages; `/` or ⌘K focuses it, arrows + Enter open a result), the env and time-range scope
(only on screens that read them) and the theme switch (`system | light | dark`). The account menu sits at the bottom of the sidebar.

### Root Cause Card (signature)
Sits above the golden signals on service and endpoint pages, only when there is a clear regression. An origin label
("Your code" / "Your dependency" / "Not your code · downstream"), one sentence naming the cause, a single-line path of
crumbs through the call tree and the affected KPIs; it opens a drawer with the step-by-step chain (before → now,
multiplier, "explains %" bar, evidence at the root).

## Do's and Don'ts

### Do:
- **Do** reference tokens from `src/styles/tokens.css` for every colour, size, space and radius.
- **Do** render status through `StatusBadge` (icon + label) and alarm cards through the 4px status rail.
- **Do** assign chart series `series-1..8` in order and keep one y-axis per chart.
- **Do** use JetBrains Mono for routes, logs, trace ids and code; tabular numbers for compared values.
- **Do** keep exactly one primary (navy) button per page.
- **Do** build dark theme from its own token steps; check both themes.

### Don't:
- **Don't** write raw hex, raw px font sizes or ad-hoc spacing in components.
- **Don't** use status colours for decoration, series or diffs, or colour as the only status signal.
- **Don't** use dual y-axes.
- **Don't** add decorative motion outside `motion.css` / `motion.ts`, or any motion that ignores the reduce-motion switches.
- **Don't** introduce a second accent hue for chrome (violet primary, green "signal" nav): the chrome voice is Trooper Navy.
- **Don't** use provider brand colours or logos for third-party identity; use the neutral `ProviderMark`.
