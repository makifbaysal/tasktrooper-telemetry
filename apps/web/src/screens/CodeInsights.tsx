import { useState } from 'react';
import { useNavigate } from 'react-router';
import { Bell, Check, ChevronRight, CircleDashed, Eye, EyeOff, FileCode2, FolderGit2, GitBranch, Gauge, Plus, RefreshCw, ShieldCheck } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import {
  Badge, Button, Card, CodeBlock, Drawer, DrawerSection, EmptyState, KeyValues, Meter, MetricCard, PageHeader, Section, Segmented, StatusBadge, Table, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { inventory, monitors, repos, series, services, suggestions, timestamps, type InventoryItem, type TimeRange } from '@/data';
import { ago, compact } from '@/lib/format';
import { CriticalityBadge } from './shared';

type Filter = 'all' | 'unobserved' | 'monitored';

const KIND_LABEL: Record<InventoryItem['kind'], string> = {
  endpoint: 'Endpoint', consumer: 'Consumer', cron: 'Scheduled job', view: 'View', dependency: 'Dependency',
};

/** Traffic seen in production but with no matching handler in any connected repo. */
const UNKNOWN_IN_CODE = [{ key: 'GET /v0/orders/legacy', service: 'orders-api', rpm: 14 }];

const monitorFor = (i: InventoryItem) => monitors.find((m) => m.target.includes(i.key));
const suggestionFor = (i: InventoryItem) => suggestions.find((s) => s.target.endsWith(i.key));

function whyCritical(i: InventoryItem): string {
  const s = suggestionFor(i);
  if (s) return s.rationale;
  if (i.criticality === 'critical') return 'Writes money or order state; a failure here is customer-visible and not retried automatically.';
  if (i.criticality === 'important') return 'Part of a user-facing flow, but failures are retried or degrade gracefully.';
  return 'Read-only or internal path; failures have no lasting side effects.';
}

function handlerSnippet(i: InventoryItem): string {
  const go = i.file.endsWith('.go');
  const py = i.file.endsWith('.py');
  if (i.kind === 'consumer') {
    return `// ${i.file}:${i.line}
consumer.wrap('${i.source}', { process: '${i.key}' }, async (msg) => {
  const order = await orders.get(msg.orderId);
  await shipments.create(order);         // side effect
}, { retries: 3, dlq: '${(i.source ?? '').replace('.dlq', '')}.dlq' });`;
  }
  if (i.kind === 'cron') {
    return `# ${i.file}:${i.line}
@schedule("${i.source}")
def send_invoices():
    for account in billing.due_today():
        mailer.send_invoice(account)      # external call`;
  }
  if (i.kind === 'dependency') {
    return `// ${i.file}:${i.line}
client := iyzico.New(cfg.APIKey, iyzico.WithTimeout(5*time.Second))
res, err := client.CreatePayment(ctx, req)  // synchronous, on the charge path`;
  }
  if (go) {
    const [method, route] = i.key.split(' ');
    return `// ${i.file}:${i.line}
r.${method[0] + method.slice(1).toLowerCase()}("${route}", func(w http.ResponseWriter, r *http.Request) {
    tx, _ := db.BeginTx(r.Context(), nil)
    defer tx.Rollback()
    // …
})`;
  }
  if (py) return `# ${i.file}:${i.line}\n@app.route("${i.key.split(' ')[1]}")\ndef handler(): ...`;
  const [method, route] = i.key.split(' ');
  return `// ${i.file}:${i.line}
@${method[0] + method.slice(1).toLowerCase()}('${route}')
async handle(@Body() dto: Dto) {
  return this.db.transaction(async (tx) => {
    // …
  });
}`;
}

function trafficFor(i: InventoryItem, range: TimeRange) {
  const base = i.kind === 'cron' ? 0.4 : i.criticality === 'critical' ? 220 : i.criticality === 'important' ? 90 : 25;
  return series(`inv-${i.id}`, range, { base, noise: 0.18, daily: 0.35, min: 0 });
}

export function CodeInsights() {
  const nav = useNavigate();
  const { range } = useApp();
  const drawer = useDrawer<InventoryItem>();
  const [filter, setFilter] = useState<Filter>('all');
  const [accepted, setAccepted] = useState<string[]>([]);

  const scoped = inventory.filter((i) => i.criticality !== 'low');
  const covered = scoped.filter((i) => i.monitored).length;
  const coverage = scoped.length ? covered / scoped.length : 0;
  const gaps = scoped.filter((i) => !i.monitored)
    .sort((a, b) => (a.criticality === 'critical' ? 0 : 1) - (b.criticality === 'critical' ? 0 : 1));
  const criticalGaps = gaps.filter((i) => i.criticality === 'critical').length;
  const unobserved = inventory.filter((i) => !i.observed);
  const rows = inventory.filter((i) => filter === 'all' || (filter === 'unobserved' ? !i.observed : i.monitored));

  const item = drawer.item;
  const sug = item && suggestionFor(item);
  const mon = item && monitorFor(item);

  return (
    <>
      <PageHeader title="Code insights" subtitle="What your code exposes, what runs in production, and what a monitor protects"
        actions={<Button variant="primary" icon={<Plus size={14} />}>Connect repository</Button>} />

      {/* 1 — the headline: how much of what matters is protected */}
      <div className="grid grid-3">
        <MetricCard className="metric--hero span-2" label="Alert coverage"
          value={scoped.length ? `${Math.round(coverage * 100)}%` : "—"}
          hint={<>{covered} of {scoped.length} critical &amp; important code paths have a monitor · <strong>{criticalGaps} critical path{criticalGaps === 1 ? '' : 's'} without one</strong></>}
          actions={<span className="muted" style={{ fontSize: 'var(--text-sm)' }}>{suggestions.length} suggested monitors · review them in the inventory below or under each service's Alerts tab</span>}>
          <div style={{ paddingTop: 'var(--space-2)' }}><Meter value={coverage} tone={coverage < 0.8 ? 'warning' : undefined} /></div>
        </MetricCard>

        <Card title="Drift" subtitle="Code vs. production traffic, last 30 days">
          <div className="list" style={{ margin: 'calc(-1 * var(--space-2)) 0' }}>
            <button className="list__row list__row--clickable" style={{ all: 'unset', display: 'flex', alignItems: 'center', gap: 'var(--space-3)', padding: 'var(--space-2) 0', cursor: 'pointer', width: '100%' }}
              onClick={() => { setFilter('unobserved'); document.getElementById('inventory')?.scrollIntoView({ behavior: 'smooth' }); }}>
              <EyeOff size={16} className="muted" aria-hidden />
              <span className="list__main"><span className="list__title">In code, no traffic</span><span className="list__meta">Dead code or a missing SDK</span></span>
              <span className="num" style={{ fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-semibold)' }}>{unobserved.length}</span>
              <ChevronRight size={14} className="muted" aria-hidden />
            </button>
            <div className="list__row" style={{ padding: 'var(--space-2) 0' }}>
              <Eye size={16} className="muted" aria-hidden />
              <span className="list__main"><span className="list__title">Traffic, not in code</span><span className="list__meta mono">{UNKNOWN_IN_CODE[0].key} · {UNKNOWN_IN_CODE[0].rpm} rpm</span></span>
              <span className="num" style={{ fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-semibold)' }}>{UNKNOWN_IN_CODE.length}</span>
            </div>
          </div>
        </Card>
      </div>

      {/* 2 — what to do: the unprotected paths */}
      <Section title="Coverage gaps" subtitle="Critical and important code paths with no monitor — click for code, traffic and a suggested alert">
        {gaps.length === 0 ? (
          <Card><EmptyState icon={<ShieldCheck size={20} />} title="Every important path is monitored" /></Card>
        ) : (
          <Card flush>
            <div className="list">
              {gaps.map((i) => {
                const s = suggestionFor(i);
                return (
                  <div key={i.id} className="list__row list__row--clickable" onClick={() => drawer.open(i)}>
                    <CriticalityBadge c={i.criticality} />
                    <div className="list__main">
                      <span className="list__title mono">{i.key}{i.source && <span className="muted"> · {i.source}</span>}</span>
                      <span className="list__meta">{KIND_LABEL[i.kind]} · {i.service} · {i.observed ? 'receiving traffic' : 'no traffic in 30d'}</span>
                    </div>
                    {s ? <Badge tone="accent" icon={<Bell />}>Suggestion ready</Badge> : <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>No suggestion yet</span>}
                    <ChevronRight size={14} className="muted" aria-hidden />
                  </div>
                );
              })}
            </div>
          </Card>
        )}
      </Section>

      {/* 3 — reference: full inventory */}
      <div id="inventory">
        <Section title="Inventory" subtitle={`${inventory.length} code paths found in ${repos.length} repositories`}
          actions={<Segmented label="Filter" value={filter} onChange={setFilter} options={[
            { id: 'all', label: 'All' }, { id: 'monitored', label: 'Monitored' }, { id: 'unobserved', label: 'No traffic' },
          ]} />}>
          <Card flush>
            <Table<InventoryItem> rows={rows} rowKey={(i) => i.id} onRowClick={drawer.open}
              columns={[
                { key: 'key', header: 'Code path', sortValue: (i) => i.key, render: (i) => <span className="mono">{i.key}{i.source && <span className="muted"> · {i.source}</span>}</span> },
                { key: 'kind', header: 'Kind', sortValue: (i) => i.kind, render: (i) => <span className="secondary">{KIND_LABEL[i.kind]}</span> },
                { key: 'svc', header: 'Service', sortValue: (i) => i.service, render: (i) => <span className="secondary">{i.service}</span> },
                { key: 'crit', header: 'Criticality', sortValue: (i) => ['critical', 'important', 'low'].indexOf(i.criticality), render: (i) => <CriticalityBadge c={i.criticality} /> },
                { key: 'obs', header: 'Traffic', render: (i) => (i.observed ? <span className="secondary">Observed</span> : <span className="row muted" style={{ gap: 4 }}><EyeOff size={12} aria-hidden />None (30d)</span>) },
                { key: 'mon', header: 'Monitor', render: (i) => (i.monitored ? <Badge tone="good" icon={<Bell />}>Active</Badge> : <span className="muted">—</span>) },
                { key: 'go', header: '', render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
              ]} />
          </Card>
        </Section>
      </div>

      {/* 4 — secondary: where the inventory comes from */}
      <Section title="Repositories">
        <Card flush>
          <div className="list">
            {repos.map((r) => (
              <div key={r.id} className="list__row">
                <FolderGit2 size={16} className="muted" aria-hidden />
                <div className="list__main">
                  <span className="list__title">{r.name}</span>
                  <span className="list__meta row" style={{ gap: 6 }}><GitBranch size={12} aria-hidden />{r.branch} · <span className="mono">{r.sha}</span> · {r.language} · {r.services.join(', ')}</span>
                </div>
                {r.status === 'analyzing'
                  ? <Badge tone="accent" icon={<CircleDashed />}>Analyzing…</Badge>
                  : <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>Analyzed {ago(r.analyzedAt)}</span>}
                <Button size="sm" variant="ghost" iconOnly aria-label="Re-analyze" icon={<RefreshCw size={14} />} />
              </div>
            ))}
          </div>
        </Card>
      </Section>

      <Drawer open={!!item} onClose={drawer.close}
        title={item ? <span className="mono">{item.key}</span> : ''}
        subtitle={item && <>{KIND_LABEL[item.kind]} · {item.service}</>}
        actions={item?.kind === 'endpoint' && <Button size="sm" icon={<Gauge size={14} />} onClick={() => nav('/load-tests')}>Load test this</Button>}>
        {item && (
          <>
            <DrawerSection title="Protection">
              {mon ? (
                <div className="row"><StatusBadge status={mon.state} /><span className="list__title">{mon.name}</span><span className="muted">{mon.condition}</span>
                  <span className="spacer" /><Button size="sm" variant="ghost" onClick={() => nav(`/services/${services.find((s) => s.name === item.service)?.id ?? ''}?tab=alerts`)}>Open</Button></div>
              ) : item.monitored ? (
                <Badge tone="good" icon={<Bell />}>Monitored</Badge>
              ) : sug ? (
                <div className="stack" style={{ gap: 'var(--space-2)' }}>
                  <div className="row"><Badge tone="accent" icon={<Bell />}>Suggested</Badge><span className="list__title">{sug.title}</span><span className="spacer" /><span className="muted">{Math.round(sug.confidence * 100)}% confidence</span></div>
                  <div className="mono secondary">{sug.condition}</div>
                  <div className="row">
                    {accepted.includes(sug.id)
                      ? <StatusBadge status="ok" label="Monitor created" />
                      : <Button size="sm" variant="primary" icon={<Check size={14} />} onClick={() => setAccepted((a) => [...a, sug.id])}>Accept suggestion</Button>}
                    <Button size="sm" variant="ghost" onClick={() => nav('/alerts/new')}>Customize…</Button>
                  </div>
                </div>
              ) : (
                <div className="row"><span className="muted">No monitor and no suggestion yet.</span><span className="spacer" /><Button size="sm" icon={<Plus size={14} />} onClick={() => nav('/alerts/new')}>Create monitor</Button></div>
              )}
            </DrawerSection>

            <DrawerSection title="Why it matters" aside={<CriticalityBadge c={item.criticality} />}>
              <p className="secondary" style={{ margin: 0 }}>{whyCritical(item)}</p>
            </DrawerSection>

            <DrawerSection title="Production traffic" aside={item.observed ? `${compact(trafficFor(item, range).at(-1) ?? 0)} / min now` : undefined}>
              {item.observed
                ? <TimeSeries timestamps={timestamps(range)} height={140} area format={(v) => compact(v)}
                    series={[{ id: 't', label: 'Requests / min', slot: 1, data: trafficFor(item, range) }]} />
                : <EmptyState icon={<EyeOff size={20} />} title="No traffic in 30 days">Dead code, a feature flag that is off, or the SDK is not installed on this service.</EmptyState>}
            </DrawerSection>

            <DrawerSection title="Source" aside={<span className="row mono" style={{ gap: 4 }}><FileCode2 size={12} aria-hidden />{item.file}:{item.line}</span>}>
              <CodeBlock>{handlerSnippet(item)}</CodeBlock>
            </DrawerSection>

            <DrawerSection title="Details">
              <KeyValues items={[
                ['Service', item.service],
                ['Repository', repos.find((r) => r.services.includes(item.service))?.name ?? '—'],
                ['Kind', KIND_LABEL[item.kind]],
                ['Criticality', item.criticality],
              ]} />
            </DrawerSection>
          </>
        )}
      </Drawer>
    </>
  );
}
