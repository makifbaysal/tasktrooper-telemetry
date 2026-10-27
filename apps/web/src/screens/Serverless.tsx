import { useState } from 'react';
import { useNavigate } from 'react-router';
import { Plug, SquareFunction } from 'lucide-react';
import { Sparkline } from '@/components/charts';
import { Badge, Button, Card, EmptyState, PageHeader, SearchInput, Section, Segmented, StatTile, StatusBadge, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { useData } from '@/data';
import {
  FAAS_PROVIDER_LABEL, faasRank, serverlessFunctions, serverlessSummary, type FaasFunction, type FaasProvider, type FaasStatus,
} from '@/data/serverless';
import { compact, ms, pct } from '@/lib/format';

/** Status → StatusBadge (always icon + label). idle / missing are neutral. */
export function FaasStatusBadge({ status }: { status: FaasStatus }) {
  switch (status) {
    case 'ok': return <StatusBadge status="ok" />;
    case 'warning': return <StatusBadge status="warning" />;
    case 'critical': return <StatusBadge status="critical" />;
    case 'idle': return <StatusBadge status="muted" label="Idle" />;
    case 'missing': return <StatusBadge status="muted" label="Missing" />;
  }
}

/** USD estimate: cents under $1, two decimals under $100, whole dollars above. */
export const usd = (v: number) => (v === 0 ? '$0' : v < 0.01 ? '<$0.01' : v < 100 ? `$${v.toFixed(2)}` : `$${Math.round(v).toLocaleString('en-US')}`);
export const providerShort = (p: FaasProvider) => (p === 'aws' ? 'Lambda' : 'Azure');

/** One-line reason for a function in "Needs attention". */
function problemOf(f: FaasFunction): string {
  if (f.throttles > 0) return `${compact(f.throttles)} throttled invocations — concurrency hit its limit (max ${compact(f.concurrencyMax)})`;
  if (f.errorRate > 0.01) return `Error rate ${pct(f.errorRate)} (${compact(f.errors)} errors)`;
  if (f.timeoutS && f.p95Ms > f.timeoutS * 800) return `p95 ${ms(f.p95Ms)} is ${Math.round((f.p95Ms / (f.timeoutS * 1000)) * 100)}% of the ${f.timeoutS} s timeout`;
  return 'Degraded';
}

/** Free-form trigger "kind:target" (sqs:orders, schedule:nightly, servicebus:q, api, blob…): kind as label, target as text. */
export function TriggerBadge({ trigger }: { trigger: string }) {
  const i = trigger.indexOf(':');
  const kind = i > 0 ? trigger.slice(0, i) : trigger;
  const rest = i > 0 ? trigger.slice(i + 1) : '';
  return <Badge><span style={{ fontWeight: 'var(--weight-medium)' }}>{kind}</span>{rest && <span className="mono">&nbsp;{rest}</span>}</Badge>;
}
export function Triggers({ triggers }: { triggers: string[] }) {
  if (!triggers.length) return <span className="muted">—</span>;
  return <span className="row" style={{ gap: 'var(--space-1)', flexWrap: 'wrap' }}>{triggers.map((t) => <TriggerBadge key={t} trigger={t} />)}</span>;
}

export function Serverless() {
  useData();
  const nav = useNavigate();
  const { range } = useApp();
  const [provider, setProvider] = useState<'all' | FaasProvider>('all');
  const [q, setQ] = useState('');
  const all = serverlessFunctions(range);
  const sum = serverlessSummary(range);
  const open = (f: FaasFunction) => nav(`/infrastructure/serverless/${encodeURIComponent(f.id)}`);
  const bad = all.filter((f) => f.status === 'critical' || f.status === 'warning').sort((a, b) => faasRank(b.status) - faasRank(a.status));
  const needle = q.trim().toLowerCase();
  const rows = all.filter((f) => (provider === 'all' || f.provider === provider)
    && (!needle || `${f.name} ${f.service} ${f.region} ${f.runtime} ${f.triggers.join(' ')}`.toLowerCase().includes(needle)));
  const providers = [...new Set(all.map((f) => f.provider))];

  const header = <PageHeader title="Serverless" subtitle="AWS Lambda functions and Azure Function Apps — invocations, errors, duration, cold starts and estimated cost" />;
  if (all.length === 0) {
    return (
      <>
        {header}
        <Card>
          <EmptyState icon={<SquareFunction />} title="No serverless functions yet"
            action={<Button variant="primary" icon={<Plug size={14} />} onClick={() => nav('/settings/integrations')}>Connect AWS or Azure</Button>}>
            Turn on Lambda in an AWS integration or connect an Azure subscription. Functions are discovered every 10 minutes and their metrics read every minute.
            Instrument them with OpenTelemetry (or <span className="mono">wrapLambda</span> from <span className="mono">@trooper/node</span>) to get traces and cold starts.
          </EmptyState>
        </Card>
      </>
    );
  }

  return (
    <>
      {header}

      {bad.length > 0 && (
        <Section title="Needs attention">
          <div className="grid grid-3">
            {bad.map((f) => (
              <Card key={f.id} onClick={() => open(f)} className={f.status === 'critical' ? 'card--rail-critical' : 'card--rail-warning'}>
                <div className="row" style={{ gap: 'var(--space-2)', marginBottom: 'var(--space-2)' }}>
                  <FaasStatusBadge status={f.status} /><strong>{f.name}</strong><span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{providerShort(f.provider)} · {f.region}</span>
                </div>
                <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{problemOf(f)}</div>
              </Card>
            ))}
          </div>
        </Section>
      )}

      <Section>
        <div className="grid grid-3">
          <StatTile label="Invocations" value={compact(sum.invocations)} />
          <StatTile label="Error rate" value={pct(sum.errorRate)} />
          <StatTile label="Estimated cost" value={usd(sum.costUsd)} unit="est." />
          <StatTile label="Throttles" value={compact(sum.throttles)} />
          <StatTile label="Cold starts" value={compact(sum.coldStarts)} />
          <StatTile label={`Functions${sum.byProvider.some((p) => p.functions > 0) ? ` · ${sum.byProvider.filter((p) => p.functions > 0).map((p) => `${p.functions} ${providerShort(p.provider)}`).join(', ')}` : ''}`}
            value={String(sum.functions || all.length)} />
        </div>
      </Section>

      <Section title="Functions" subtitle={`${rows.length} of ${all.length} · cost is an estimate from duration × memory and request price, free tier not deducted`}
        actions={<div className="row" style={{ gap: 'var(--space-2)' }}>
          {providers.length > 1 && <Segmented label="Provider" value={provider} onChange={setProvider}
            options={[{ id: 'all', label: 'All' }, ...providers.map((p) => ({ id: p, label: FAAS_PROVIDER_LABEL[p] }))]} />}
          <div style={{ width: 240 }}><SearchInput placeholder="Name, runtime, trigger…" value={q} onChange={(e) => setQ(e.target.value)} /></div>
        </div>}>
        <Card flush>
          <Table<FaasFunction> rows={rows} rowKey={(f) => f.id} onRowClick={open} defaultSort={{ key: 'status', dir: 'desc' }}
            empty={<EmptyState icon={<SquareFunction />} title="No function matches" />} columns={[
              { key: 'name', header: 'Function', sortValue: (f) => f.name, render: (f) => (
                <div style={{ whiteSpace: 'nowrap' }}>
                  <div className="row" style={{ gap: 'var(--space-2)', fontWeight: 'var(--weight-medium)' }}>{f.name}<Badge>{providerShort(f.provider)}</Badge></div>
                  <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{f.runtime} · {f.region}{f.triggers.length ? ` · ${f.triggers.join(', ')}` : ''}</span>
                </div>) },
              { key: 'status', header: 'Status', sortValue: (f) => faasRank(f.status), render: (f) => <FaasStatusBadge status={f.status} /> },
              { key: 'inv', header: 'Invocations', numeric: true, sortValue: (f) => f.invocations, render: (f) => (
                <div className="row" style={{ gap: 'var(--space-2)', justifyContent: 'flex-end' }}>
                  {f.spark.length > 1 && <Sparkline data={f.spark.slice(-24)} width={56} height={22} />}<span>{compact(f.invocations)}</span>
                </div>) },
              { key: 'err', header: 'Error rate', numeric: true, sortValue: (f) => f.errorRate, render: (f) => (f.invocations ? pct(f.errorRate) : <span className="muted">—</span>) },
              { key: 'p95', header: 'p95', numeric: true, sortValue: (f) => f.p95Ms, render: (f) => (f.invocations ? ms(f.p95Ms) : <span className="muted">—</span>) },
              { key: 'thr', header: 'Throttles', numeric: true, sortValue: (f) => f.throttles, render: (f) => (f.throttles ? compact(f.throttles) : <span className="muted">0</span>) },
              { key: 'cold', header: 'Cold starts', numeric: true, sortValue: (f) => f.coldStarts, render: (f) => (f.coldStarts
                ? <span title={`${pct(f.coldStartRate)} of invocations`}>{compact(f.coldStarts)}</span> : <span className="muted">0</span>) },
              { key: 'cost', header: 'Cost (est.)', numeric: true, sortValue: (f) => f.costUsd, render: (f) => usd(f.costUsd) },
            ]} />
        </Card>
      </Section>
    </>
  );
}
