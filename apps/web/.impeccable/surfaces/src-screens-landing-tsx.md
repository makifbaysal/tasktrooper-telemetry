---
version: 1
slug: "src-screens-landing-tsx"
primary_target: "src/screens/Landing.tsx"
related_targets: ["src/screens/Login.tsx","src/screens/Docs.tsx"]
---

# Landing (public) — surface brief

Scope: public marketing page at `/` for signed-out visitors, plus the public `/docs` (SDK install) and the separate `/login`.
Mode: Persuade. Audience: backend developers first (frontend/mobile devs, SRE, managers skim). Job: understand what TaskTrooper
Telemetry does and why it differs, then read the docs or sign in. No sign-up, no waitlist; Sign in is a nav link to `/login`.
Proof: no customers, logos, quotes, prices or benchmarks exist (PRODUCT.md). Demonstrations use synthetic example systems,
labelled "Synthetic example". Constraints: TaskTrooper theme (navy, amber accent-warm, cold slate, Inter, JetBrains Mono),
tokens only, works in light and dark, reduced motion respected.

## Direction contract

THESIS: The page owns one idea — the product names the cause, not just the symptom. It refuses the category default of a
centred headline over a product screenshot and a row of feature cards.

OWN-WORLD: A deep Trooper Navy field (dark, in both themes) carrying a synthetic service graph drawn in thin slate lines; one
call path lit in Signal Amber; mono service names; a navy-raised "cause" panel with an amber kicker. Below the fold the page
turns to the app's light/dark page colour with sections separated by hairlines and type, not cards.

STORY: Visitor sees a system map with one path lit and reads the sentence that explains it; scrolls through what the product
does (APM, RUM, logs & alerts, load tests, code-aware alerts, root cause), how it gets in (SDKs, three lines), and goes to Docs
or signs in.

FIRST VIEWPORT: Full-bleed navy map. Nav at top (mark + name left; Product, SDKs, Docs; Sign in link right). Headline top-left
at ~64px over a one-paragraph promise and two actions (See how it works → scrolls; Read the docs → /docs). Graph occupies the
right two-thirds; storefront-bff → recommendation-api → reco-db → migration 0042 lit amber with p95 and 63× annotations.
The cause panel sits bottom-left. "Synthetic example" label bottom-right.

FORM: Service map with lit path; dealt structure 7 of my ranked list (index 2 on the page). seed:aa10b1c368ffa149
Signature interaction: the lit path draws itself hop by hop on load (stroke-dashoffset), each hop's annotation fades in as the
line reaches it, then the cause sentence settles; hovering a node highlights its edges. Reduced motion shows the final state.

FINISH: unreviewed and undocumented is unfinished; this build ends with the finish review, the verdict, DESIGN.md, and every shipping raster carrying its provenance
