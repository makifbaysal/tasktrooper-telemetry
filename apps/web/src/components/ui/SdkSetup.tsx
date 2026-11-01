import { useState, type ReactNode } from 'react';
import { Link } from 'react-router';
import { KeyRound } from 'lucide-react';
import { CopyCode } from './Choice';
import { Segmented } from './Tabs';
import { EmptyState } from './misc';
import { projectDsn, useAuth } from '@/data/auth';

/**
 * SDK quick start (ADR-028): install + init snippet per SDK with the project's DSN. Used by onboarding (with the real
 * key, shown once) and by every "no data yet" empty state (key replaced by `<ingest-key>`).
 */
export type SdkId = 'go' | 'node' | 'browser' | 'python' | 'java' | 'dotnet';
export type SdkGroup = 'all' | 'backend' | 'frontend';

interface Sdk { label: string; frontend?: boolean; install: string; code: (dsn: string) => string; note?: string }
export const SDKS: Record<SdkId, Sdk> = {
  go: {
    label: 'Go',
    install: 'go get github.com/tasktrooper/telemetry-go',
    code: (dsn) => `import trooper "github.com/tasktrooper/telemetry-go"

shutdown, err := trooper.Start(ctx, trooper.Config{
    DSN:     "${dsn}", // or os.Getenv("TROOPER_DSN")
    Service: "orders-api",
})
if err != nil {
    log.Printf("telemetry disabled: %v", err)
}
defer shutdown(context.Background())`,
  },
  node: {
    label: 'Node.js',
    install: 'npm install @trooper/node',
    code: (dsn) => `import { init } from '@trooper/node';

init({ dsn: '${dsn}', service: 'orders-api' });
// or without code: TROOPER_DSN=… node --import @trooper/node/register app.mjs`,
  },
  browser: {
    label: 'Browser',
    frontend: true,
    install: 'npm install @trooper/browser',
    code: (dsn) => `import { init } from '@trooper/browser';

init({
  dsn: '${dsn}',
  service: 'shop-web',
  tracePropagationTargets: [/api\\.example\\.com/],
});`,
    note: 'Use a browser ingest key restricted to your origins (Settings → API keys). React, Vue, Angular and React Native packages wrap the same init.',
  },
  python: {
    label: 'Python',
    install: 'pip install tasktrooper',
    code: (dsn) => `import tasktrooper

tasktrooper.init(dsn="${dsn}", service="orders-api")
# … then create your FastAPI / Flask / Django app`,
  },
  java: {
    label: 'Java',
    install: '# download trooper-javaagent.jar (sdks/jvm)',
    code: (dsn) => `export TROOPER_DSN=${dsn}
export TROOPER_SERVICE=orders-api
java -javaagent:trooper-javaagent.jar -jar app.jar`,
    note: 'No code change: the agent instruments Spring, Quarkus, Ktor, JDBC, Kafka and RabbitMQ.',
  },
  dotnet: {
    label: '.NET',
    install: 'dotnet add package TaskTrooper.Telemetry.AspNetCore',
    code: (dsn) => `builder.Services
    .AddTaskTrooper(o => { o.Dsn = "${dsn}"; o.ServiceName = "orders-api"; })
    .AddAspNetCore();`,
  },
};
const ORDER: SdkId[] = ['go', 'node', 'browser', 'python', 'java', 'dotnet'];
const idsOf = (g: SdkGroup) => ORDER.filter((id) => g === 'all' || (g === 'frontend') === !!SDKS[id].frontend);

/** Tabs per SDK → install command + init snippet, both with copy buttons. */
export function SdkQuickstart({ dsn, group = 'all' }: { dsn: string; group?: SdkGroup }) {
  const ids = idsOf(group);
  const [sdk, setSdk] = useState<SdkId>(ids[0]);
  const s = SDKS[ids.includes(sdk) ? sdk : ids[0]];
  return (
    <div className="sdk-setup">
      {ids.length > 1 && <Segmented label="SDK" value={sdk} onChange={setSdk} options={ids.map((id) => ({ id, label: SDKS[id].label }))} />}
      <CopyCode>{s.install}</CopyCode>
      <CopyCode>{s.code(dsn)}</CopyCode>
      {s.note && <p className="sdk-setup__note">{s.note}</p>}
    </div>
  );
}

/**
 * Empty state for a screen without data: what will appear here + how to connect an SDK with this project's DSN.
 * `group` narrows the SDK tabs (frontend apps → Browser). `children` explains what the screen shows once data arrives.
 */
export function ConnectSdk({ icon, title, children, group = 'all', action, setup = true }: {
  icon: ReactNode; title: string; children?: ReactNode; group?: SdkGroup; action?: ReactNode;
  /** false: only the empty state text (e.g. a filter that matched nothing while other data exists) */
  setup?: boolean;
}) {
  const { project } = useAuth();
  return (
    <div className="connect-sdk">
      <EmptyState icon={icon} title={title} action={action}>{children}</EmptyState>
      {setup && (
        <div className="connect-sdk__setup">
          <SdkQuickstart dsn={projectDsn(project?.id)} group={group} />
          <p className="sdk-setup__note">
            <KeyRound size={12} aria-hidden /> Replace <span className="mono">&lt;ingest-key&gt;</span> with an ingest key from{' '}
            <Link to="/settings?tab=keys">Settings → API keys</Link>. Data shows up here within a minute.
          </p>
        </div>
      )}
    </div>
  );
}
