/**
 * Public docs content: install + init per SDK, taken from each SDK's README (sdks/*). Keep these in step with the
 * READMEs; the full reference stays on GitHub (`readme`).
 */
export const REPO = 'https://github.com/makifbaysal/tasktrooper-telemetry';
/** Public site + dashboard (landing, /docs, /login and the app share this host). */
export const SITE = 'https://telemetry.tasktrooper.ai';
export const SITE_HOST = 'telemetry.tasktrooper.ai';
/** Live hosts, one level under the zone (ADR-031: the free certificate does not cover api.telemetry.…; ADR-030's hosts are
 *  not served yet). */
export const API_HOST = 'https://telemetry-api.tasktrooper.ai';
export const INGEST_HOST = 'telemetry-ingest.tasktrooper.ai';
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
    .AddTaskTrooper(o =>
    {
        o.Dsn = Environment.GetEnvironmentVariable("TROOPER_DSN");
        o.ServiceName = "orders-api";
    })
    .AddAspNetCore();` },
  },
  {
    id: 'python', label: 'Python', group: 'Backend', readme: 'sdks/python',
    summary: 'FastAPI, Flask and Django; initialise before the app is created.',
    install: { lang: 'bash', code: 'pip install tasktrooper' },
    init: { lang: 'python', code: `import os
import tasktrooper

tasktrooper.init(
    dsn=os.environ["TROOPER_DSN"],
    service="orders-api",
)
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
  { name: 'tt_list_errors · tt_get_error', what: 'Error groups by fingerprint, and one with its latest stack.' },
  { name: 'tt_list_incidents · tt_get_incident', what: 'Open and recent incidents; one with root cause and timeline.' },
  { name: 'tt_list_monitors · tt_list_kpis', what: 'Alert rules and SLOs with their state, burn rate and budget.' },
  { name: 'tt_preview_monitor', what: 'Replays an unsaved alert rule over past data: how often would it have fired? Saves nothing.' },
  { name: 'tt_list_alert_suggestions · tt_list_inventory', what: 'Alerts suggested from code analysis, and the routes, crons and consumers found in the repository.' },
  { name: 'tt_list_monitor_templates', what: 'Ready-made infrastructure, serverless and LLM alert rules.' },
  { name: 'tt_list_notify_channels', what: 'Where alerts can go: Slack channels the bot sees and configured channels, each with its exact target string.' },
  { name: 'tt_get_app_summary · tt_get_ai_overview', what: 'Web / mobile app health, and LLM calls, tokens and cost.' },
  { name: 'tt_list_hosts · tt_list_databases · tt_list_consumer_groups', what: 'Hosts, databases and queue consumer groups with their health.' },
  { name: 'tt_list_deploys', what: 'Deploy markers, to line an incident up with a release.' },
  { name: 'tt_list_loadtest_scenarios · tt_list_loadtest_runs · tt_get_loadtest_run', what: 'Load test scenarios, verified target domains and run results.' },
];

/** Registered only with TT_MCP_ALLOW_WRITE=true; each one returns a dry run until called with confirm: true. */
export const MCP_WRITE_TOOLS: DocTool[] = [
  { name: 'tt_create_monitor · tt_update_monitor · tt_delete_monitor', what: 'Alert rules. Deletes only rules an agent created.' },
  { name: 'tt_accept_suggestion', what: 'Turns a code-analysis suggestion into an alert rule.' },
  { name: 'tt_create_kpi · tt_update_kpi · tt_delete_kpi', what: 'SLOs with fast and slow burn-rate alerts.' },
  { name: 'tt_send_slack_test', what: 'Posts a test message to check the bot can reach a channel.' },
  { name: 'tt_create_loadtest_scenario · tt_run_loadtest · tt_stop_loadtest', what: 'A baseline load test against a verified domain when there is too little traffic to set thresholds. Stopping needs no confirmation.' },
];

/** One command: finds the coding tools on the machine, asks which ones and global vs. project, installs tt-mcp + skills. */
export const INSTALL_CMD = `curl -fsSL ${SITE}/install-agent.sh | bash`;

export const INSTALL_TOOLS = ['Claude Code', 'Muse Code', 'Cursor', 'Antigravity', 'Codex CLI', 'OpenCode', 'Gemini CLI'];

export const INSTALL_STEPS: string[] = [
  'Finds the coding tools installed on this machine and asks which ones to set up.',
  'Asks whether to install globally (every project) or only for the current project.',
  'Asks for an API key (Settings → API keys), checks it against the API and saves it in a file only you can read: ~/.config/tasktrooper/mcp.env, or .tasktrooper/mcp.env in the project (added to .gitignore). Tool configs never contain the key.',
  'Asks whether agents may create alert rules, SLOs and load tests after your approval (off by default).',
  'Downloads tt-mcp for your OS and checks its checksum, copies the trooper-* skills, and registers the server in each tool.',
];

export const INSTALL_FLAGS: DocCode = { label: 'Without questions (CI, dotfiles)', lang: 'bash', code: `TT_API_KEY=<api-key> bash -c "$(curl -fsSL ${SITE}/install-agent.sh)" -- \\
  --yes --tools claude,cursor --scope global --read-only` };

export const MCP_SETUP: DocCode[] = [
  { label: 'Manual: binary and key file', lang: 'bash', code: `# tt-mcp_<os>_<arch>.tar.gz from ${REPO}/releases (tags mcp/v*), or build:
git clone ${REPO} && cd tasktrooper-telemetry/services/mcp
GOWORK=off go build -o ~/.local/bin/tt-mcp ./cmd/tt-mcp

# settings: environment variables, or this file (mode 600)
mkdir -p ~/.config/tasktrooper && umask 077 && cat > ~/.config/tasktrooper/mcp.env <<'EOF'
TT_API_URL=${API_HOST}
TT_API_KEY=<api-key>
TT_MCP_ALLOW_WRITE=false
EOF` },
  { label: 'Claude Code', lang: 'bash', code: `claude mcp add --scope user tasktrooper -- ~/.local/bin/tt-mcp` },
  { label: 'Cursor (~/.cursor/mcp.json) · Antigravity (~/.gemini/config/mcp_config.json)', lang: 'json', code: `{
  "mcpServers": {
    "tasktrooper": { "type": "stdio", "command": "/Users/<you>/.local/bin/tt-mcp", "args": [] }
  }
}` },
  { label: 'OpenCode (~/.config/opencode/opencode.json)', lang: 'json', code: `{
  "mcp": {
    "tasktrooper": { "type": "local", "command": ["/Users/<you>/.local/bin/tt-mcp"], "enabled": true }
  }
}` },
  { label: 'Codex CLI', lang: 'bash', code: `codex mcp add tasktrooper -- ~/.local/bin/tt-mcp` },
  { label: 'Muse Code (~/.config/muse/settings.json)', lang: 'json', code: `{
  "schema_version": 1,
  "mcp_servers": {
    "tasktrooper": { "transport": "stdio", "command": "/Users/<you>/.local/bin/tt-mcp", "args": [], "mode": "optional", "enabled": true }
  }
}` },
];

export const MCP_NOTES: string[] = [
  'Read-only by default. With TT_MCP_ALLOW_WRITE=true an agent can also create alert rules, SLOs and load tests, but every change is shown as a dry run first and is only written after you approve it. Everything it creates is labelled “Agent” in the dashboard.',
  'The API key decides the organisation and project. Create one under Settings → API keys. tt-mcp reads it from the environment or from the first env file it finds: $TT_MCP_ENV_FILE, .tasktrooper/mcp.env in the project, then ~/.config/tasktrooper/mcp.env. Optional TT_ORG_ID and TT_PROJECT_ID are checked at startup.',
  'The key only travels in the Authorization header. It never appears in tool output, errors or logs.',
  'Log messages and span attributes are masked before they reach the agent: e-mail addresses, IPs, bearer tokens, TaskTrooper keys and card-like numbers.',
  'Results are capped at 64 KB. A trimmed result says so and suggests how to narrow the query.',
];

export interface DocSkill { name: string; when: string; needsMcp: boolean }

export const SKILLS: DocSkill[] = [
  { name: 'trooper-diagnose', needsMcp: true, when: 'Something is slow, failing or alerting. Walks incidents → KPIs → endpoint root cause → traces → logs → deploys, cites every claim and says “unknown” when the evidence is thin.' },
  { name: 'trooper-alerts', needsMcp: true, when: 'Set up alerting for a project: reads services, traffic, existing rules and channels, derives every threshold from your data (“p95 over the last 5 minutes > 800 ms”), previews how often each would have fired, and creates the approved list. Suggests a short load test when there is too little traffic.' },
  { name: 'trooper-query', needsMcp: true, when: 'A question about production data: slowest endpoints, error logs for a trace, what deployed today. Teaches scoping, pagination and how to read truncated results.' },
  { name: 'trooper-contribute', needsMcp: false, when: 'For contributors to the TaskTrooper repository itself, not for using the product. A checklist pointing at AGENTS.md and the design docs: tenancy, cardinality, PII, tokens, make check.' },
];

export const SKILLS_SETUP: DocCode[] = [
  { label: 'Manual', lang: 'bash', code: `# global: Cursor, Codex, OpenCode, Gemini CLI and Muse Code read ~/.agents/skills
cp -r tasktrooper-telemetry/skills/trooper-{diagnose,query,alerts} ~/.agents/skills/
ln -s ~/.agents/skills/trooper-* ~/.claude/skills/          # Claude Code
ln -s ~/.agents/skills/trooper-* ~/.gemini/config/skills/   # Antigravity

# one project: .agents/skills (and .claude/skills for Claude Code)` },
]
