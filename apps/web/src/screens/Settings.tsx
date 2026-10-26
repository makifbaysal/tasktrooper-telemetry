import { useState } from 'react';
import { useNavigate } from 'react-router';
import { ChevronRight, Copy, KeyRound, Plug, RotateCw, Trash2 } from 'lucide-react';
import { Columns } from '@/components/charts';
import { Badge, Button, Callout, Card, CodeBlock, Drawer, DrawerSection, KeyValues, PageHeader, Section, Segmented, StatusBadge, Table, useDrawer } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { NOW, PROVIDER_LABEL, integrations, series, timestamps, useData } from '@/data';
import { IntegrationStatusBadge, ProviderMark } from './Integrations';
import { ago, compact, dateTime } from '@/lib/format';

type Lang = 'node' | 'browser' | 'react-native' | 'go';

const SNIPPETS: Record<Lang, { install: string; code: string }> = {
  node: {
    install: 'pnpm add @trooper/node',
    code: `import { init } from '@trooper/node';

init({
  dsn: process.env.TROOPER_DSN,
  service: 'orders-api',
  environment: 'production',
  version: process.env.GIT_SHA,
});

// Worker / consumer
import { consumer } from '@trooper/node';
consumer.wrap('orders.created', async (msg) => handle(msg));`,
  },
  browser: {
    install: 'pnpm add @trooper/browser',
    code: `import { init, identify, track } from '@trooper/browser';

init({
  dsn: 'https://pk_live_3f9a@ingest.trooper.dev/shop-platform',
  service: 'shop-web',
  tracePropagationTargets: [/api\\.shop\\.com/],
});

identify({ id: user.id, plan: user.plan });
track('checkout_started', { cart_value: 120 });`,
  },
  'react-native': {
    install: 'pnpm add @trooper/react-native',
    code: `import { init, withNavigation } from '@trooper/react-native';

init({ dsn: TROOPER_DSN, service: 'shop-mobile' });
export default withNavigation(App);`,
  },
  go: {
    install: 'go get github.com/trooper-dev/trooper-go',
    code: `shutdown := trooper.Init(trooper.Config{
    DSN:         os.Getenv("TROOPER_DSN"),
    Service:     "payments-api",
    Environment: "production",
})
defer shutdown(ctx)

r := chi.NewRouter()
r.Use(trooperhttp.Middleware())`,
  },
};

interface ApiKey {
  id: string;
  name: string;
  kind: 'ingest' | 'api';
  prefix: string;
  origins: string[];
  scopes: string[];
  createdAt: number;
  createdBy: string;
  lastUsed: number;
  /** events (ingest) or requests (api) per minute, for the usage chart */
  base: number;
}

const KEYS: ApiKey[] = [
  { id: 'k1', name: 'Browser ingest', kind: 'ingest', prefix: 'pk_live_3f9a', origins: ['shop.com', '*.shop.com'], scopes: ['ingest:rum'], createdAt: NOW - 140 * 86400_000, createdBy: 'platform team', lastUsed: NOW - 2_000, base: 5200 },
  { id: 'k2', name: 'Backend ingest', kind: 'ingest', prefix: 'sk_ing_91c0', origins: [], scopes: ['ingest:traces', 'ingest:metrics', 'ingest:logs'], createdAt: NOW - 140 * 86400_000, createdBy: 'platform team', lastUsed: NOW - 1_000, base: 18400 },
  { id: 'k3', name: 'CI / Terraform', kind: 'api', prefix: 'sk_api_77ab', origins: [], scopes: ['monitors:write', 'loadtests:run'], createdAt: NOW - 41 * 86400_000, createdBy: 'devops', lastUsed: NOW - 3 * 86400_000, base: 0.6 },
];

const CHANNELS = [
  { id: 'slack', name: 'Slack', detail: '#payments-alerts, #orders-oncall, #web-team', monitors: 6 },
  { id: 'pd', name: 'PagerDuty', detail: 'Services: payments, orders', monitors: 2 },
  { id: 'hook', name: 'Webhook', detail: 'https://hooks.shop.com/trooper', monitors: 0 },
];

export function SettingsScreen() {
  useData();
  const { range } = useApp();
  const nav = useNavigate();
  const [lang, setLang] = useState<Lang>('node');
  const [revoked, setRevoked] = useState<string[]>([]);
  const [rotated, setRotated] = useState<string[]>([]);
  const drawer = useDrawer<ApiKey>();
  const s = SNIPPETS[lang];
  const k = drawer.item;
  const usage = k ? series(`key-${k.id}`, range, { base: k.base, noise: 0.25, daily: 0.4, min: 0 }) : [];

  return (
    <>
      <PageHeader title="Settings & SDK" subtitle="Project shop-platform" />

      <Section title="Install the SDK" subtitle="Data appears within a minute of the first request">
        <Card actions={<Segmented label="Platform" value={lang} onChange={setLang} options={[
          { id: 'node', label: 'Node.js' }, { id: 'browser', label: 'Browser' }, { id: 'react-native', label: 'React Native' }, { id: 'go', label: 'Go' },
        ]} />} title="1. Install, 2. initialize">
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <div className="row"><div style={{ flex: 1 }}><CodeBlock>{s.install}</CodeBlock></div><Button iconOnly aria-label="Copy install command" icon={<Copy size={14} />} onClick={() => navigator.clipboard?.writeText(s.install)} /></div>
            <CodeBlock>{s.code}</CodeBlock>
          </div>
        </Card>
      </Section>

      <div className="grid grid-2">
        <Section title="API keys" subtitle="Click a key for usage and actions" actions={<Button size="sm" icon={<KeyRound size={12} />}>Create key</Button>}>
          <Card flush>
            <Table<ApiKey> rowKey={(x) => x.id} rows={KEYS} onRowClick={drawer.open} columns={[
              { key: 'n', header: 'Name', render: (x) => <div><div>{x.name}</div><div className="mono muted" style={{ fontSize: 'var(--text-xs)' }}>{x.prefix}…</div></div> },
              { key: 'k', header: 'Type', render: (x) => <Badge tone={x.kind === 'api' ? 'warning' : 'neutral'}>{x.kind}</Badge> },
              { key: 'u', header: 'Last used', numeric: true, sortValue: (x) => x.lastUsed, render: (x) => revoked.includes(x.id) ? <StatusBadge status="muted" label="Revoked" /> : <span className="muted">{ago(x.lastUsed)}</span> },
              { key: 'go', header: '', render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
            ]} />
          </Card>
        </Section>

        <Section title="Notification channels" subtitle="Where monitors send alerts">
          <Card flush>
            <div className="list">
              {CHANNELS.map((c) => (
                <div key={c.id} className="list__row">
                  <div className="list__main"><span className="list__title">{c.name}</span><span className="list__meta">{c.detail}</span></div>
                  <span className="muted" style={{ fontSize: 'var(--text-xs)', whiteSpace: 'nowrap' }}>{c.monitors} monitor{c.monitors === 1 ? '' : 's'}</span>
                  <StatusBadge status="ok" label="Connected" />
                </div>
              ))}
            </div>
          </Card>
        </Section>
      </div>

      <Section title="Cloud integrations" subtitle="AWS, Google Cloud, Vercel and Neon — managed databases, queues, deploys and drains"
        actions={<Button size="sm" icon={<Plug size={12} />} onClick={() => nav('/settings/integrations')}>Manage</Button>}>
        <Card flush>
          <div className="list">
            {integrations.map((i) => (
              <div key={i.id} className="list__row list__row--clickable" onClick={() => nav(`/settings/integrations?id=${encodeURIComponent(i.id)}`)}>
                <ProviderMark provider={i.provider} size="sm" />
                <div className="list__main"><span className="list__title">{i.name}</span><span className="list__meta">{PROVIDER_LABEL[i.provider]} · {i.resources.length} resources{i.lastSyncAt ? ` · synced ${ago(i.lastSyncAt)}` : ''}</span></div>
                <IntegrationStatusBadge status={i.status} />
                <ChevronRight size={14} className="muted" aria-hidden />
              </div>
            ))}
            {integrations.length === 0 && (
              <div className="list__row list__row--clickable" onClick={() => nav('/settings/integrations')}>
                <Plug size={16} className="muted" aria-hidden />
                <div className="list__main"><span className="list__title">Connect a cloud account</span><span className="list__meta">Read-only; discovered resources join the Databases and Message queues pages</span></div>
                <ChevronRight size={14} className="muted" aria-hidden />
              </div>
            )}
          </div>
        </Card>
      </Section>

      <Drawer open={!!k} onClose={drawer.close} width={560}
        title={k?.name} subtitle={k && <span className="mono">{k.prefix}…</span>}
        actions={k && !revoked.includes(k.id) && <>
          <Button size="sm" icon={<RotateCw size={14} />} onClick={() => setRotated((r) => [...r, k.id])}>Rotate</Button>
          <Button size="sm" variant="danger" icon={<Trash2 size={14} />} onClick={() => setRevoked((r) => [...r, k.id])}>Revoke</Button>
        </>}>
        {k && (
          <>
            {revoked.includes(k.id) && <Callout icon={<Trash2 size={16} />}>This key is revoked. SDKs using it are rejected with 401.</Callout>}
            {rotated.includes(k.id) && !revoked.includes(k.id) && <Callout icon={<RotateCw size={16} />}>New key issued. The old key keeps working for 24 h so you can roll it out.</Callout>}
            <DrawerSection title={k.kind === 'ingest' ? 'Events received' : 'API requests'} aside={`${compact(usage.at(-1) ?? 0)} / min now`}>
              <Columns timestamps={timestamps(range)} height={160} format={(v) => compact(v)}
                series={[{ id: 'u', label: k.kind === 'ingest' ? 'Events / min' : 'Requests / min', data: usage, color: 'var(--series-1)' }]} />
            </DrawerSection>
            <DrawerSection title="Access">
              <KeyValues items={[
                ['Type', k.kind === 'ingest' ? 'Ingest (write-only)' : 'Management API'],
                ['Scopes', <span className="row" style={{ gap: 4, flexWrap: 'wrap' }}>{k.scopes.map((x) => <Badge key={x}>{x}</Badge>)}</span>],
                ['Allowed origins', k.origins.length ? <span className="mono">{k.origins.join(', ')}</span> : <span className="muted">Any (server-side key)</span>],
              ]} />
            </DrawerSection>
            <DrawerSection title="History">
              <KeyValues items={[
                ['Created', `${dateTime(k.createdAt)} by ${k.createdBy}`],
                ['Last used', ago(k.lastUsed)],
              ]} />
            </DrawerSection>
          </>
        )}
      </Drawer>
    </>
  );
}
