# Product

<!-- impeccable:product-schema 1 -->

## Platform

web

## Users

**Primary: backend developer.** Usually reaching the dashboard mid-incident or right after a deploy, with one question:
is my endpoint / consumer slow or failing, since when, and is it my code or something I call? Design decisions favour
fast scanning and getting from "something is wrong" to the named cause in as few steps as possible.

Secondary audiences (from `.ai/product.md`, served without displacing the primary user):
- Frontend / mobile developer: which page or screen is slow, where crashes happen, where user flows drop.
- SRE / platform: alert coverage and noise, KPI error budgets, capacity limits across many services.
- Product / manager: users, pages, session length, overall health.

## Product Purpose

TaskTrooper Telemetry is an observability platform: SDKs added to backend services, workers/consumers, web and mobile
apps send telemetry (OTLP); the platform visualises it, alerts on it and runs load tests. It replaces the three or four
separate tools teams use today (APM, RUM, logs, load testing) and the habit of defining alerts by hand after an incident.

Success: a developer adds the SDK, their service appears (even with zero traffic), and when something breaks the
platform names the cause down the call tree ("homepage slow because storefront-bff → recommendation-api → reco-db
queries got 63× slower after migration 0042").

## Positioning

The platform reads the customer's code base (repo connection): it extracts the endpoint / consumer / page inventory and
uses it to propose or define alerts and generate load-test scenarios automatically. Alerts hang off KPIs (SLO error
budgets), and root-cause analysis walks the call tree to answer "your code or a dependency?" directly on the service page.

## Operating Context

- Delivered as SaaS at telemetry.tasktrooper.ai (landing, docs and the dashboard on one host); customers only add an SDK and a DSN. (The repo also runs fully locally for
  development: docker compose infra + Go services + Vite dashboard.)
- Part of the TaskTrooper product family; the dashboard follows the TaskTrooper app theme (`.ai/design-system.md` § Theme source).
- SDKs are published for Go, JVM, Android, .NET, Python, JS (browser, React, Vue, Angular, React Native, Node), Flutter
  and iOS (ADR-029). Users reach the dashboard from Slack alert messages, GitHub PR/push flows and direct navigation.
- Sign-in: OIDC (company account), dev e-mail login only in development. First sign-in without an org lands in onboarding
  (create org + first project → ingest key + SDK snippet).

## Capabilities and Constraints

- Screens: Overview, Services (backend, worker, web and mobile apps are all services), Service / Endpoint / Process
  detail (KPIs, alerts and logs live under their owning service), KPI detail, Load tests, Code insights, Infrastructure
  (databases, message queues), Integrations, session replay, profiling, AI/LLM monitoring.
- "One place" rule: each list and "New …" action exists in exactly one place (`.ai/product.md` § One place rule).
- Workers/consumers have processes, not endpoints; lag and alerts bind to the process.
- No mock, seed or demo data (ADR-028): every screen reads the real API; empty states carry setup guidance.
- Multi-tenant: every view is scoped to org + project.
- Terminology: service, endpoint (route template, e.g. `/users/:id`), process, KPI, monitor, incident, ingest key, DSN.

## Brand Commitments

- Name: TaskTrooper Telemetry (TaskTrooper product family).
- Theme follows the TaskTrooper app; visual rules are recorded in `.ai/design-system.md` and `apps/web/src/styles/tokens.css`.

## Evidence on Hand

None yet: early stage, no external customers, testimonials, case studies, benchmarks or pricing. The only real user is
the platform itself (self-observability, ADR-027). Future work must not invent customers, logos, quotes or numbers.

## Product Principles

1. From symptom to named cause: every screen shortens the path from "something is wrong" to "this is why".
2. Code-aware by default: inventory, alerts and load tests come from the customer's code, not manual setup.
3. Real data only: an honest empty state with setup guidance beats a plausible fake.
4. One place for everything: no duplicated lists or create actions across screens.
5. Status is never colour alone: always icon + label.
