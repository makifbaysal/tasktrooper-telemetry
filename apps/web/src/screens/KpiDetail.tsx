import { useNavigate, useParams } from 'react-router';
import { Bell, Pencil, Play, Target } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Badge, Button, Card, EmptyState, KeyValues, Meter, MetricCard, PageHeader, Section, StatusBadge, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { kpiMetricSeries, kpiTree, kpiUnit, kpis, services, timestamps, type Kpi, type KpiAlertRule } from '@/data';
import { ago, ms, pct } from '@/lib/format';
import { findRootCause } from '@/lib/rca';
import { RcaChain, fmtFor, impactedKpis } from './rca';
import { badLabel, badSeries, goodPct } from './Kpis';

const TARGET_KIND: Record<Kpi['target']['kind'], string> = { page: 'Page', endpoint: 'Endpoint', journey: 'User journey', app_start: 'App start' };
const SLI_LABEL: Record<Kpi['sli'], string> = { latency: 'Latency', availability: 'Availability', success_rate: 'Success rate' };
const SOURCE_LABEL: Record<Kpi['source'], string> = { code: 'Suggested from code', template: 'Template', manual: 'Manual' };

export function KpiDetail() {
  const { kpiId = '' } = useParams();
  const { range } = useApp();
  const nav = useNavigate();
  const k = kpis.find((x) => x.id === kpiId);
  if (!k) return <EmptyState icon={<Target />} title="KPI not found" />;

  const ts = timestamps(range);
  const app = services.find((s) => s.id === k.target.serviceId);
  const tree = kpiTree(k);
  const result = tree ? findRootCause(tree, kpiUnit(k)) : undefined;
  const fmt = fmtFor(kpiUnit(k));
  const degradedTs = k.degradedSince && k.degradedSince >= ts[0] ? [{ ts: k.degradedSince, label: 'degraded' }] : undefined;
  const others = result ? impactedKpis(result.root.service ?? '').filter((x) => x.id !== k.id) : [];
  const daysLeft = k.burnRate1h > 0 ? ((k.window === '7d' ? 7 : 28) * k.budgetRemaining) / k.burnRate1h : Infinity;

  return (
    <>
      <PageHeader title={k.name}
        crumbs={[{ label: app?.name ?? k.target.serviceId, to: `/services/${k.target.serviceId}?tab=kpis` }, { label: k.name }]}
        subtitle={<span className="row" style={{ gap: 12, flexWrap: 'wrap' }}>
          <StatusBadge status={k.status} label={k.status === 'ok' ? 'On target' : k.status === 'warning' ? 'At risk' : 'Breaching'} />
          <span className="secondary">{TARGET_KIND[k.target.kind]} · <span className="mono">{k.target.ref}</span> on <a href={`/services/${k.target.serviceId}`} onClick={(e) => { e.preventDefault(); nav(`/services/${k.target.serviceId}`); }}>{app?.name ?? k.target.serviceId}</a></span>
          <span className="muted">owner {k.owner}</span>
        </span>}
        actions={<>
          {k.target.kind === 'endpoint' && <Button icon={<Play size={14} />} onClick={() => nav('/load-tests')}>Load test</Button>}
          <Button icon={<Pencil size={14} />} onClick={() => nav(`/kpis/new?edit=${k.id}`)}>Edit</Button>
        </>} />

      {/* 1 · why it's broken — the most important thing on the page when degraded */}
      {tree && result && result.path.length > 0 && (
        <Section title="Why it's degrading" subtitle={k.degradedSince ? `since ${ago(k.degradedSince)} · confidence ${Math.round(result.confidence * 100)}%` : undefined}>
          <div className="grid grid-3">
            <Card className="span-2 rca" title={<span className="rca__summary">{result.summary}</span>}
              subtitle={`${k.metric?.label ?? SLI_LABEL[k.sli]} ${fmt(tree.baseline)} → ${fmt(tree.current)} — walked down the call tree to the deepest cause`}>
              <RcaChain tree={tree} result={result} />
            </Card>
            <div className="stack">
              <Card title="Impact">
                <KeyValues items={[
                  ['Good events now', <strong key="g">{goodPct(k.current)}</strong>],
                  ['Objective', goodPct(k.objective)],
                  ['Budget burn (1h)', `${k.burnRate1h.toFixed(1)}×`],
                  ['Budget gone in', Number.isFinite(daysLeft) ? (daysLeft < 1 ? `${Math.round(daysLeft * 24)} h` : `${daysLeft.toFixed(1)} d`) : '—'],
                ]} />
              </Card>
              {others.length > 0 && (
                <Card flush title="Same cause also hits">
                  <div className="list">
                    {others.map((o) => (
                      <div key={o.id} className="list__row list__row--clickable" onClick={() => nav(`/kpis/${o.id}`)}>
                        <StatusBadge status={o.status} label={goodPct(o.current)} />
                        <div className="list__main"><span className="list__title">{o.name}</span><span className="list__meta">{app?.name} · {o.target.ref}</span></div>
                      </div>
                    ))}
                  </div>
                </Card>
              )}
            </div>
          </div>
        </Section>
      )}

      {/* 2 · objective */}
      <div className="grid grid-3">
        <MetricCard className="span-2 metric--hero" label={`Good ${k.target.kind === 'endpoint' ? 'requests' : k.target.kind === 'journey' ? 'checkouts' : 'events'} · last hour`}
          value={goodPct(k.current)} curr={k.current} prev={k.windowGood}
          badge={k.status !== 'ok' ? <StatusBadge status={k.status} label={`objective ${goodPct(k.objective)}`} /> : undefined}
          hint={<>Good = {k.goodWhen.toLowerCase()} · {goodPct(k.windowGood)} over {k.window} · chart: {badLabel(k).toLowerCase()} vs the allowed share</>}>
          <TimeSeries timestamps={ts} height={260} area format={(v) => pct(v, 1)}
            threshold={{ value: 1 - k.objective, label: `allowed ${pct(1 - k.objective, 1)}` }} events={degradedTs}
            series={[{ id: 'bad', label: badLabel(k), slot: 8, data: badSeries(k, range) }]} />
        </MetricCard>
        <div className="stack">
          <MetricCard label={`Error budget left · ${k.window}`} value={pct(k.budgetRemaining, 0)}
            hint={`${goodPct(1 - k.objective)} of events may be bad in ${k.window}`}>
            <Meter value={k.budgetRemaining} tone={k.budgetRemaining < 0.25 ? 'critical' : k.budgetRemaining < 0.5 ? 'warning' : undefined} />
          </MetricCard>
          {k.metric && (
            <MetricCard label={k.metric.label} value={fmt(k.metric.current)} curr={k.metric.current} prev={k.metric.baseline} upIsGood={false}
              hint={k.threshold ? `good ≤ ${k.threshold.metric === 'LCP' || k.metric.unit === 'ms' ? ms(k.threshold.value) : k.threshold.value}` : `usually ${fmt(k.metric.baseline)}`}>
              <TimeSeries timestamps={ts} height={90} format={fmt}
                threshold={k.threshold && k.metric.unit === 'ms' ? { value: k.threshold.value, label: ms(k.threshold.value) } : undefined}
                series={[{ id: 'm', label: k.metric.label, slot: 1, data: kpiMetricSeries(k, range) }]} />
            </MetricCard>
          )}
        </div>
      </div>

      {/* 3 · alerts defined on this KPI */}
      <Section title="Alerts on this KPI" subtitle="fire on error-budget burn, so short blips don't page anyone"
        actions={<Button size="sm" icon={<Bell size={14} />} onClick={() => nav(`/kpis/new?edit=${k.id}#alerts`)}>Add rule</Button>}>
        <Card flush>
          <Table<KpiAlertRule> rows={k.alerts} rowKey={(r) => r.id} columns={[
            { key: 'state', header: 'State', render: (r) => <StatusBadge status={r.state === 'ok' ? 'ok' : r.state} /> },
            { key: 'cond', header: 'Condition', render: (r) => r.condition },
            { key: 'kind', header: 'Type', render: (r) => <Badge>{r.kind === 'burn_rate' ? 'Burn rate' : 'Below target'}</Badge> },
            { key: 'sev', header: 'Severity', render: (r) => <StatusBadge status={r.severity === 'critical' ? 'critical' : 'warning'} label={r.severity} /> },
            { key: 'ch', header: 'Notifies', render: (r) => <span className="secondary">{r.channels.join(', ')}</span> },
          ]} />
        </Card>
        <div className="muted" style={{ fontSize: 'var(--text-sm)' }}>
          Every alert from this KPI carries the root cause above, so the page says <em>where</em> it broke, not just that it did.
        </div>
      </Section>

      {/* 4 · definition (secondary) */}
      <Section title="Definition">
        <Card>
          <KeyValues items={[
            ['Target', <span key="t">{TARGET_KIND[k.target.kind]} · <span className="mono">{k.target.ref}</span></span>],
            ['Measured on', app?.name ?? k.target.serviceId],
            ['SLI', SLI_LABEL[k.sli]],
            ['Good event', k.goodWhen],
            ['Objective', `${goodPct(k.objective)} over ${k.window}`],
            ['Source', SOURCE_LABEL[k.source]],
            ['Description', k.description],
            ['Owner', k.owner],
          ]} />
        </Card>
      </Section>
    </>
  );
}
