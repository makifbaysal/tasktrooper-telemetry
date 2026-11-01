/**
 * Public docs content: install + init per SDK, taken from each SDK's README (sdks/*). Keep these in step with the
 * READMEs; the full reference stays on GitHub (`readme`).
 */
export const REPO = 'https://github.com/makifbaysal/tasktrooper-telemetry';
/** Public site + dashboard (landing, /docs, /login and the app share this host). */
export const SITE = 'https://telemetry.tasktrooper.ai';
export const SITE_HOST = 'telemetry.tasktrooper.ai';
/** Production hosts (ADR-030, contracts §2a). */
export const API_HOST = 'https://api.telemetry.tasktrooper.ai';
export const INGEST_HOST = 'ingest.telemetry.tasktrooper.ai';
export const DSN_PLACEHOLDER = `https://<ingest-key>@${INGEST_HOST}/<project-id>`;

export type DocSdkGroup = 'Backend' | 'Web' | 'Mobile';

export interface DocSdk {
  id: string;
  label: string;
  group: DocSdkGroup;
  summary: string;
  install: { lang: string; code: string };
  init: { lang: string; code: string };
  notes?: string[];
  readme: string;
}

export const DOC_SDKS: DocSdk[] = [
  {
    id: 'go', label: 'Go', group: 'Backend', readme: 'sdks/go',
    summary: 'net/http, chi, gin, gRPC, database/sql, Kafka, Redis and more through optional integration modules.',
    install: { lang: 'bash', code: 'go get github.com/makifbaysal/tasktrooper-telemetry/sdks/go' },
    init: { lang: 'go', code: `import trooper "github.com/makifbaysal/tasktrooper-telemetry/sdks/go"

shutdown, err := trooper.Start(ctx, trooper.Config{
    DSN:     os.Getenv("TROOPER_DSN"),
    Service: "orders-api",
})
if err != nil {
    log.Printf("telemetry disabled: %v", err)
}
defer shutdown(context.Background())` },
  },
  {
    id: 'jvm', label: 'Java / Kotlin', group: 'Backend', readme: 'sdks/jvm',
    summary: 'Zero-code Java agent, or Spring Boot 3, Quarkus and Ktor modules from Maven Central.',
    install: { lang: 'kotlin', code: `// build.gradle.kts
repositories { mavenCentral() }
dependencies { implementation("ai.tasktrooper:trooper-spring-boot-starter:0.1.0") }` },
    init: { lang: 'bash', code: `# or without code changes: the Java agent
export TROOPER_DSN=${DSN_PLACEHOLDER}
export TROOPER_SERVICE=orders-api TROOPER_ENV=production
java -javaagent:trooper-javaagent.jar -jar app.jar` },
  },
  {
    id: 'dotnet', label: '.NET', group: 'Backend', readme: 'sdks/dotnet',
    summary: 'ASP.NET Core and worker services; route templates come from the matched endpoint.',
    install: { lang: 'bash', code: `dotnet add package TaskTrooper.Telemetry
dotnet add package TaskTrooper.Telemetry.AspNetCore` },
    init: { lang: 'csharp', code: `builder.Services
    .AddTaskTrooper(o => { o.Dsn = "${DSN_PLACEHOLDER}"; o.ServiceName = "orders-api"; })
    .AddAspNetCore();` },
  },
  {
    id: 'python', label: 'Python', group: 'Backend', readme: 'sdks/python',
    summary: 'FastAPI, Flask and Django; initialise before the app is created.',
    install: { lang: 'bash', code: 'pip install tasktrooper' },
    init: { lang: 'python', code: `import tasktrooper

tasktrooper.init(dsn="${DSN_PLACEHOLDER}", service="orders-api")
# … then create your FastAPI / Flask / Django app` },
  },
  {
    id: 'node', label: 'Node.js', group: 'Backend', readme: 'sdks/js/packages/node',
    summary: 'Express, Fastify, NestJS and the common clients; works with or without code.',
    install: { lang: 'bash', code: 'npm install @tasktrooper/node' },
    init: { lang: 'ts', code: `import { init } from '@tasktrooper/node';

init({ dsn: process.env.TROOPER_DSN, service: 'orders-api' });
// or without code: TROOPER_DSN=… node --import @tasktrooper/node/register app.mjs` },
  },
  {
    id: 'browser', label: 'Browser', group: 'Web', readme: 'sdks/js/packages/browser',
    summary: 'Page views, Web Vitals, JS errors, network calls and session replay for any web app.',
    install: { lang: 'bash', code: 'npm install @tasktrooper/browser' },
    init: { lang: 'ts', code: `import { init } from '@tasktrooper/browser';

init({
  dsn: '${DSN_PLACEHOLDER}',
  service: 'shop-web',
  tracePropagationTargets: [/api\\.example\\.com/],
});` },
    notes: ['Use a browser ingest key restricted to your origins (Settings → API keys).'],
  },
  {
    id: 'react', label: 'React', group: 'Web', readme: 'sdks/js/packages/react',
    summary: 'Provider, error boundary and router integrations; the view name is the route template.',
    install: { lang: 'bash', code: 'npm install @tasktrooper/react' },
    init: { lang: 'tsx', code: `import { TrooperProvider, ErrorBoundary } from '@tasktrooper/react';

<TrooperProvider options={{ dsn, app: 'shop-web', version }}>
  <ErrorBoundary name="root" fallback={({ resetError }) => <Crash onRetry={resetError} />}>
    <App />
  </ErrorBoundary>
</TrooperProvider>;` },
    notes: ['Vue (`@tasktrooper/vue`) and Angular (`@tasktrooper/angular`) wrap the same browser SDK.'],
  },
  {
    id: 'react-native', label: 'React Native', group: 'Mobile', readme: 'sdks/js/packages/react-native',
    summary: 'Screens, app start, JS errors and network calls; react-navigation route names become views.',
    install: { lang: 'bash', code: 'npm install @tasktrooper/react-native' },
    init: { lang: 'ts', code: `import { init, trooperNavigation } from '@tasktrooper/react-native';

init({ dsn, app: 'courier-app', environment: 'production' });
const nav = trooperNavigation(navigationRef);` },
  },
  {
    id: 'android', label: 'Android', group: 'Mobile', readme: 'sdks/android',
    summary: 'Screens, app start, crashes (including native), OkHttp calls, Compose and session replay.',
    install: { lang: 'kotlin', code: `dependencies {
    implementation("ai.tasktrooper:trooper-android:0.1.0")
    implementation("ai.tasktrooper:trooper-okhttp:0.1.0")   // optional
}` },
    init: { lang: 'kotlin', code: `class App : Application() {
    override fun onCreate() {
        super.onCreate()
        TaskTrooper.init(this, TrooperConfig(
            dsn = "${DSN_PLACEHOLDER}",
            app = "courier-app",
            version = BuildConfig.VERSION_NAME,
        ))
    }
}` },
  },
  {
    id: 'ios', label: 'iOS', group: 'Mobile', readme: 'sdks/ios',
    summary: 'UIKit and SwiftUI screens, app start, crashes, URLSession calls and session replay.',
    install: { lang: 'swift', code: `// Package.swift
.package(url: "${REPO}", from: "0.1.0"),
.product(name: "TaskTrooper", package: "tasktrooper-telemetry"),

// or CocoaPods
pod 'TaskTrooper', '~> 0.1'` },
    init: { lang: 'swift', code: `import TaskTrooper

TaskTrooper.start(TrooperConfig(
    dsn: "${DSN_PLACEHOLDER}",
    app: "courier-app"
))` },
    notes: ['Call it as early as possible: in `application(_:didFinishLaunchingWithOptions:)` or the SwiftUI `App` initialiser.'],
  },
  {
    id: 'flutter', label: 'Flutter', group: 'Mobile', readme: 'sdks/flutter',
    summary: 'Screens, errors from both zones, app start and HTTP calls (Dio optional).',
    install: { lang: 'yaml', code: `dependencies:
  tasktrooper_flutter: ^0.1.0
  tasktrooper_dio: ^0.1.0   # only if you use Dio` },
    init: { lang: 'dart', code: `void main() {
  TaskTrooper.init(
    const TrooperConfig(dsn: '${DSN_PLACEHOLDER}', app: 'courier-app'),
    appRunner: () => runApp(const MyApp()),
  );
}` },
  },
];

export interface DocGuide { id: string; title: string; body: string[] }

export const DOC_GUIDES: DocGuide[] = [
  {
    id: 'code-insights', title: 'Code insights',
    body: [
      'Install the TaskTrooper GitHub App and pick repositories. TaskTrooper reads the routes, consumers and jobs in your code and matches them with production traffic.',
      'For each critical path it proposes a KPI and the monitors behind it. You accept, edit or dismiss each suggestion; pushes to the default branch update the inventory incrementally.',
    ],
  },
  {
    id: 'kpis-alerts', title: 'KPIs and alerts',
    body: [
      'A KPI is an objective on a service, endpoint or process, for example 99.5% availability over 30 days. Alerts fire on error-budget burn rate, so a short spike and a slow leak both page at the right time.',
      'When an incident opens, root-cause analysis walks the call tree and tells you whether it is your code or something you call. Alerts go to Slack with the cause attached; acknowledge and mute from the thread.',
    ],
  },
  {
    id: 'load-tests', title: 'Load tests',
    body: [
      'Verify a staging host once (DNS TXT record or a well-known file), then define load levels and a traffic mix, or generate a scenario from real production traffic.',
      'Runs report through the same telemetry, so the services under test show their latency and errors next to the load curve.',
    ],
  },
];

// ---------------------------------------------------------------- AI agents: MCP server + Agent Skills (.ai/mcp.md)

export interface DocCode { label: string; lang: string; code: string }
export interface DocTool { name: string; what: string }

export const MCP_TOOLS: DocTool[] = [
  { name: 'tt_list_services', what: 'Services with throughput, error rate and latency. Start here.' },
  { name: 'tt_get_service_kpis', what: 'Health, RED metrics, dependencies and versions of one service.' },
  { name: 'tt_list_endpoints', what: 'Routes and consumers of a service, with ids for root-cause analysis.' },
  { name: 'tt_endpoint_rca', what: 'Why one endpoint got slower or errorier, per dependency against a baseline.' },
  { name: 'tt_search_traces', what: 'Recent traces by service, endpoint, duration or error.' },
  { name: 'tt_get_trace', what: 'Full span tree of one trace.' },
  { name: 'tt_search_logs', what: 'Log lines by text, level, service, route or trace id, paginated.' },
  { name: 'tt_list_monitors', what: 'Alert rules with kind, query and state.' },
  { name: 'tt_list_incidents', what: 'Open and recent incidents with severity and state.' },
  { name: 'tt_get_incident', what: 'One incident: alert detail, root cause and timeline.' },
  { name: 'tt_list_loadtest_runs', what: 'Load test runs with thresholds and summary.' },
  { name: 'tt_list_deploys', what: 'Deploy markers, to line an incident up with a release.' },
];

export const MCP_SETUP: DocCode[] = [
  { label: 'Build', lang: 'bash', code: `git clone ${REPO}
cd tasktrooper-telemetry/services/mcp
go install ./cmd/tt-mcp        # $(go env GOPATH)/bin must be on PATH` },
  { label: 'Claude Code', lang: 'bash', code: `claude mcp add tasktrooper \\
  -e TT_API_URL=${API_HOST} -e TT_API_KEY=<api-key> \\
  -- tt-mcp` },
  { label: '.mcp.json', lang: 'json', code: `{
  "mcpServers": {
    "tasktrooper": {
      "command": "tt-mcp",
      "env": { "TT_API_URL": "${API_HOST}", "TT_API_KEY": "<api-key>" }
    }
  }
}` },
  { label: 'OpenCode', lang: 'json', code: `{
  "mcp": {
    "tasktrooper": {
      "type": "local",
      "command": ["tt-mcp"],
      "environment": { "TT_API_URL": "${API_HOST}", "TT_API_KEY": "<api-key>" },
      "enabled": true
    }
  }
}` },
];

export const MCP_NOTES: string[] = [
  'Read-only: agents can look, never change monitors, incidents or load tests.',
  'The API key decides the organisation and project. Create one under Settings → API keys. Optional TT_ORG_ID and TT_PROJECT_ID are checked at startup, and the server refuses to start on a mismatch.',
  'The key only travels in the Authorization header. It never appears in tool output, errors or logs.',
  'Log messages and span attributes are masked before they reach the agent: e-mail addresses, IPs, bearer tokens, TaskTrooper keys and card-like numbers.',
  'Results are capped at 64 KB. A trimmed result says so and suggests how to narrow the query.',
];

export interface DocSkill { name: string; when: string; needsMcp: boolean }

export const SKILLS: DocSkill[] = [
  { name: 'trooper-diagnose', needsMcp: true, when: 'Something is slow, failing or alerting. Walks incidents → KPIs → endpoint root cause → traces → logs → deploys, cites every claim and says “unknown” when the evidence is thin.' },
  { name: 'trooper-query', needsMcp: true, when: 'A question about production data: slowest endpoints, error logs for a trace, what deployed today. Teaches scoping, pagination and how to read truncated results.' },
  { name: 'trooper-contribute', needsMcp: false, when: 'For contributors to the TaskTrooper repository itself, not for using the product. A checklist pointing at AGENTS.md and the design docs: tenancy, cardinality, PII, tokens, make check.' },
];

export const SKILLS_SETUP: DocCode[] = [
  { label: 'Claude Code', lang: 'bash', code: `# inside a clone of the repo the skills are already wired up:
#   .claude/skills -> ../skills
claude -p "list skills starting with trooper-"

# elsewhere: copy them into your project
cp -r tasktrooper-telemetry/skills/trooper-* <your-project>/.claude/skills/` },
  { label: 'OpenCode', lang: 'json', code: `// opencode.json
{ "skills": ["./skills"] }` },
];
