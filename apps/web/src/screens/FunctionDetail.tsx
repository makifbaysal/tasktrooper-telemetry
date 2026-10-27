import type { ReactNode } from 'react';
import { useNavigate, useParams } from 'react-router';
import { AlertOctagon, Bell, Gauge, SquareFunction } from 'lucide-react';
import { Columns, TimeSeries } from '@/components/charts';
import { Badge, Button, Callout, Card, EmptyState, KeyValues, PageHeader, Section, StatTile, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { integrationOf, isApi, services, timestamps, useData } from '@/data';
import { pick } from '@/data/api/apm';
import { FAAS_PROVIDER_LABEL, faasRank, serverlessFunction, serverlessFunctions, type FaasFunction, type FaasRecentError } from '@/data/serverless';
import { ago, compact, dateTime, ms, pct } from '@/lib/format';
import { AppInsightsBadge, FaasStatusBadge, TriggerBadge, Triggers, usd } from './Serverless';

export function FunctionDetail() {
  useData();
  const { functionId = '' } = useParams();
  const nav = useNavigate();
  const { range } = useApp();
  const d = serverlessFunction(functionId, range);
  const list = serverlessFunctions(range);
  const crumbs: { label: string; to?: string }[] = [{ label: 'Serverless', to: '/infrastructure/serverless' }];
  // Azure per-function row → breadcrumb through its Function App
  const parentId = d?.fn.parentId;
  if (parentId) {
    const parent = list.find((x) => x.id === parentId);
    crumbs.push({ label: parent?.name ?? d?.fn.app ?? 'Function app', to: `/infrastructure/serverless/${encodeURIComponent(parentId)}` });
  }
  const openFn = (x: FaasFunction) => nav(`/infrastructure/serverless/${encodeURIComponent(x.id)}`);

  if (!d) {
    return (
      <>
        <PageHeader title={functionId} crumbs={[...crumbs, { label: functionId }]} />
        <Card><EmptyState icon={<SquareFunction />} title={isApi ? 'Loading function…' : 'Function not found'} /></Card>
      </>
    );
  }

  const f = d.fn;
  const ts = d.invocations.timestamps.length ? d.invocations.timestamps : timestamps(range);
  const n = ts.length;
  const at = (set: typeof d.invocations, id: string) => pick(set, id, n);
  const inv = at(d.invocations, 'invocations');
  const errs = at(d.invocations, 'errors');
  const thr = at(d.invocations, 'throttles');
  const durTs = d.duration.timestamps.length === n ? d.duration.timestamps : ts;
  const conc = pick(d.concurrency, d.concurrency.series[0]?.id ?? 'concurrency', n);
  const cold = pick(d.coldStarts, d.coldStarts.series[0]?.id ?? 'coldStarts', n);
  const integration = d.integration ?? undefined;
  const svc = services.find((s) => s.id === f.service || s.name === f.service);
  const timeoutMs = f.timeoutS ? f.timeoutS * 1000 : undefined;
  const createAlert = () => nav(`/alerts/new?kind=faas&function=${encodeURIComponent(f.id)}&metric=${f.throttles > 0 ? 'throttles' : 'error_rate'}`);

  return (
    <>
      <PageHeader title={f.name} crumbs={[...crumbs, { label: f.name }]}
        subtitle={<span className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <FaasStatusBadge status={f.status} /><span>{FAAS_PROVIDER_LABEL[f.provider]}{f.parentId ? ' · function' : ''}</span><span className="muted">{f.runtime} · {f.region}</span>
          <AppInsightsBadge state={f.appInsights} />
        </span>}
        actions={<Button variant="primary" icon={<Bell size={14} />} onClick={createAlert}>Create alert</Button>} />

      {f.throttles > 0 && (
        <Callout icon={<AlertOctagon size={16} color="var(--status-critical)" />}>
          <strong>{compact(f.throttles)} invocations throttled</strong> in this range — concurrency peaked at {compact(f.concurrencyMax)}. Raise the reserved concurrency
          {f.triggers.some((t) => t.startsWith('sqs:')) ? ' or lower the SQS batch window / maximum concurrency of the event source mapping' : ' or the account limit'}; throttled async and queue events are retried and pile up.
        </Callout>
      )}
      {f.status === 'warning' && timeoutMs && f.p95Ms > timeoutMs * 0.8 && (
        <Callout icon={<Gauge size={16} color="var(--status-warning)" />}>p95 duration {ms(f.p95Ms)} is close to the {f.timeoutS} s timeout — slow invocations will start to time out.</Callout>
      )}
      {f.status === 'idle' && <Callout icon={<SquareFunction size={16} />}>No invocations in this range. Pick a longer time range or check its trigger ({f.triggers.join(', ') || 'none'}).</Callout>}

      <Section>
        <div className="grid grid-4">
          <StatTile label="Invocations" value={compact(f.invocations)} trend={inv.length > 1 ? inv : undefined} />
          <StatTile label="Error rate" value={f.invocations ? pct(f.errorRate) : '—'} />
          <StatTile label="p95 duration" value={f.invocations ? ms(f.p95Ms) : '—'} unit={timeoutMs ? `of ${f.timeoutS} s` : undefined} />
          {f.parentId && f.costUsd === 0
            ? <StatTile label="Cost" value="—" unit="billed on the app" />
            : <StatTile label="Cost" value={usd(f.costUsd)} unit="est." />}
        </div>
      </Section>

      <Section title="Activity">
        <div className="grid grid-2">
          <Card title="Invocations" subtitle="per interval">
            <TimeSeries timestamps={ts} height={200} area format={(v) => compact(v)} series={[{ id: 'inv', label: 'Invocations', slot: 1, data: inv }]} />
          </Card>
          <Card title="Errors and throttles" subtitle="per interval">
            <TimeSeries timestamps={ts} height={200} format={(v) => compact(v)} series={[
              { id: 'err', label: 'Errors', slot: 8, data: errs },
              ...(f.provider === 'aws' ? [{ id: 'thr', label: 'Throttles', slot: 2, data: thr }] : []),
            ]} />
          </Card>
          <Card title="Duration" subtitle={timeoutMs ? `ms · timeout ${f.timeoutS} s` : 'ms'}>
            <TimeSeries timestamps={durTs} height={200} format={ms}
              threshold={timeoutMs && Math.max(...at(d.duration, 'p99')) > timeoutMs * 0.5 ? { value: timeoutMs, label: 'timeout' } : undefined}
              series={[
                { id: 'avg', label: 'Average', slot: 1, data: at(d.duration, 'avg') },
                { id: 'p95', label: 'p95', slot: 2, data: at(d.duration, 'p95') },
                { id: 'p99', label: 'p99', slot: 3, data: at(d.duration, 'p99') },
              ]} />
          </Card>
          {f.provider === 'aws'
            ? <Card title="Concurrent executions" subtitle="max per interval">
                <TimeSeries timestamps={ts} height={200} area format={(v) => compact(v)} series={[{ id: 'conc', label: 'Concurrent executions', slot: 7, data: conc }]} />
              </Card>
            : null}
          <Card title="Cold starts" subtitle={f.coldStarts ? `${compact(f.coldStarts)} · ${pct(f.coldStartRate)} of invocations · from traces` : 'from traces (faas.coldstart)'}>
            {cold.some((v) => v > 0)
              ? <Columns timestamps={ts} height={200} format={(v) => compact(v)} series={[{ id: 'cold', label: 'Cold starts', data: cold, color: 'var(--series-5)' }]} />
              : <p className="muted" style={{ margin: 0 }}>No cold starts reported. They come from spans with <span className="mono">faas.coldstart=true</span> — instrument the function with OpenTelemetry or <span className="mono">wrapLambda</span>.</p>}
          </Card>
        </div>
      </Section>

      {d.children && d.children.length > 0 && (
        <Section title="Functions" subtitle={`${d.children.length} functions · per-function metrics from Application Insights; the charts above are app totals`}>
          <Card flush>
            <Table<FaasFunction> rows={d.children} rowKey={(x) => x.id} onRowClick={openFn} defaultSort={{ key: 'inv', dir: 'desc' }} columns={[
              { key: 'name', header: 'Function', sortValue: (x) => x.name, render: (x) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{x.name}</span> },
              { key: 'trg', header: 'Trigger', render: (x) => <Triggers triggers={x.triggers} /> },
              { key: 'status', header: 'Status', sortValue: (x) => faasRank(x.status), render: (x) => <FaasStatusBadge status={x.status} /> },
              { key: 'inv', header: 'Invocations', numeric: true, sortValue: (x) => x.invocations, render: (x) => compact(x.invocations) },
              { key: 'err', header: 'Error rate', numeric: true, sortValue: (x) => x.errorRate, render: (x) => (x.invocations ? pct(x.errorRate) : <span className="muted">—</span>) },
              { key: 'p95', header: 'p95', numeric: true, sortValue: (x) => x.p95Ms, render: (x) => (x.invocations ? ms(x.p95Ms) : <span className="muted">—</span>) },
            ]} />
          </Card>
        </Section>
      )}

      <Section title="Recent errors" subtitle="From the function's traces">
        <Card flush>
          <Table<FaasRecentError> rows={d.recentErrors} rowKey={(e) => `${e.ts}-${e.traceId}`} defaultSort={{ key: 'ts', dir: 'desc' }}
            empty={<EmptyState icon={<AlertOctagon />} title="No errors in this range" />} columns={[
              { key: 'ts', header: 'When', width: 140, sortValue: (e) => e.ts, render: (e) => <span className="muted" title={dateTime(e.ts)}>{ago(e.ts)}</span> },
              { key: 'msg', header: 'Message', render: (e) => <span className="mono" style={{ fontSize: 'var(--text-sm)', overflowWrap: 'anywhere' }}>{e.message}</span> },
              { key: 'trace', header: 'Trace', width: 220, render: (e) => svc
                ? <a href={`/services/${svc.id}?tab=logs`} onClick={(ev) => { ev.preventDefault(); nav(`/services/${svc.id}?tab=logs`); }} className="mono" style={{ fontSize: 'var(--text-xs)' }} title="Open the service's logs">{e.traceId.slice(0, 16)}…</a>
                : <span className="mono muted" style={{ fontSize: 'var(--text-xs)' }} title={e.traceId}>{e.traceId.slice(0, 16)}…</span> },
            ]} />
        </Card>
      </Section>

      <Section title="Configuration">
        <div className="grid grid-2">
          <Card>
            <KeyValues items={[
              ['Provider', FAAS_PROVIDER_LABEL[f.provider]],
              ...(f.app ? [['Function app', <span key="a" className="mono">{f.app}</span>] as [string, ReactNode]] : []),
              ['Runtime', <span key="r" className="mono">{f.runtime || '—'}</span>],
              ['Region', f.region || '—'],
              ['Memory', f.memoryMb ? `${f.memoryMb.toLocaleString('en-US')} MB` : '—'],
              ['Timeout', f.timeoutS ? `${f.timeoutS} s` : '—'],
              ['Architecture', f.arch || '—'],
              ['Triggers', <Triggers key="t" triggers={f.triggers} />],
            ]} />
          </Card>
          <Card>
            <KeyValues items={[
              ['Service', svc
                ? <a key="s" href={`/services/${svc.id}`} onClick={(e) => { e.preventDefault(); nav(`/services/${svc.id}`); }}>{svc.name}</a>
                : <span key="s" className="mono">{f.service || '—'}</span>],
              ['Max concurrency', f.provider === 'aws' ? compact(f.concurrencyMax) : <span className="muted">not reported by Azure</span>],
              ['Integration', integration
                ? <a key="i" href={`/settings/integrations?id=${encodeURIComponent(integration.id)}`} onClick={(e) => { e.preventDefault(); nav(`/settings/integrations?id=${encodeURIComponent(integration.id)}`); }}>
                    {integration.name}{integrationOf(integration.id)?.status === 'error' ? ' · sync failing' : ''}</a>
                : <span className="muted">—</span>],
              ['Slug', <span key="sl" className="mono" style={{ overflowWrap: 'anywhere' }}>{f.slug}</span>],
            ]} />
            {d.functions && d.functions.length > 0 && !(d.children && d.children.length > 0) && (
              <div style={{ marginTop: 'var(--space-3)' }}>
                <div className="field__label" style={{ marginBottom: 'var(--space-1)' }}>Functions in this app</div>
                <div className="list">
                  {d.functions.map((x) => (
                    <div key={x.name} className="list__row">
                      <span className="mono" style={{ flex: 1, minWidth: 0 }}>{x.name}</span>
                      {x.trigger && <TriggerBadge trigger={x.trigger} />}
                      {x.disabled && <Badge>disabled</Badge>}
                    </div>
                  ))}
                </div>
                <p className="muted" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>Azure Monitor reports metrics per app, so the charts above cover all of them.</p>
              </div>
            )}
          </Card>
        </div>
      </Section>
    </>
  );
}
