import { useData } from '@/data/remote';
import { useState } from 'react';
import { useNavigate } from 'react-router';
import { ArrowRight, ChevronRight, Code2, Globe, Plus, Route, Server, Smartphone, Sparkles, Target } from 'lucide-react';
import { Badge, Button, Card, EmptyState, Meter, PageHeader, SearchInput, Section, Select, StatusBadge, Table } from '@/components/ui';
import { kpiSeries, kpiTree, kpiUnit, kpis, services, type Kpi, type RcaResult, type TimeRange } from '@/data';
import { findRootCause } from '@/lib/rca';

// ---------------------------------------------------------------- shared KPI helpers (also used by Alerts)
export const KPI_KIND_LABEL: Record<Kpi['target']['kind'], string> = {
  page: 'Page', endpoint: 'Endpoint', journey: 'User journey', app_start: 'App start',
};
export const SLI_LABEL: Record<Kpi['sli'], string> = { latency: 'Latency', availability: 'Availability', success_rate: 'Success rate' };
export const SOURCE_LABEL: Record<Kpi['source'], string> = { code: 'Suggested from code', template: 'Template', manual: 'Manual' };

/** Bad-event share over time. Charts start at 0, so plot the small bad share, not the ~100% good share. */
export const badSeries = (k: Kpi, range: TimeRange) => kpiSeries(k, range).map((g) => 1 - g);
export const badLabel = (k: Kpi) =>
  k.sli === 'latency'
    ? (k.target.kind === 'endpoint' ? 'Slow or failed requests' : k.target.kind === 'app_start' ? 'Slow or crashed starts' : 'Slow or failed page views')
    : k.sli === 'availability' ? 'Failed loads' : k.target.kind === 'journey' ? 'Failed checkouts' : 'Failed requests';

/** Share of good events, with enough decimals that 99.93% doesn't round to 99.9%. */
export const goodPct = (v: number) => `${(v * 100).toFixed(v >= 0.99 ? 2 : 1)}%`;
export const objectivePct = (v: number) => `${+(v * 100).toFixed(2)}%`;

export function kpiRca(k: Kpi): RcaResult | undefined {
  const tree = kpiTree(k);
  return tree ? findRootCause(tree, kpiUnit(k)) : undefined;
}

export function KpiStatusBadge({ k }: { k: Kpi }) {
  if (k.status === 'critical') return <StatusBadge status="critical" label="Breaching" />;
  if (k.status === 'warning') return <StatusBadge status="warning" label="Burning budget" />;
  return <StatusBadge status="ok" label="On track" />;
}

export function KpiTarget({ k }: { k: Kpi }) {
  const app = services.find((s) => s.id === k.target.serviceId);
  const Icon = k.target.kind === 'journey' ? Route : app?.type === 'mobile' ? Smartphone : app?.type === 'web' ? Globe : Server;
  return (
    <span className="row muted" style={{ gap: 6, minWidth: 0, fontSize: 'var(--text-sm)' }}>
      <Icon size={13} aria-hidden style={{ flexShrink: 0 }} />
      <span className="truncate">{app?.name ?? k.target.serviceId} · <span className="mono">{k.target.ref}</span></span>
    </span>
  );
}

/** "page → endpoint → … → cause" — the drill-down path as chips; the root cause chip is highlighted. */
export function RcaBreadcrumb({ result }: { result: RcaResult }) {
  return (
    <span className="why__path">
      {result.path.map((h, i) => {
        const last = i === result.path.length - 1;
        const label = `${h.node.kind === 'endpoint' && h.node.service ? `${h.node.service} ` : ''}${h.node.label}`;
        return (
          <span key={h.node.id} className="row" style={{ gap: 'var(--space-1)', minWidth: 0 }}>
            {i > 0 && <span className="why__sep"><ChevronRight size={12} aria-hidden /></span>}
            <span className={`why__hop${last ? ' why__hop--root' : ''}`} title={label}>{label}</span>
          </span>
        );
      })}
    </span>
  );
}

/** "Why" block: the root-cause sentence plus the path that led there. */
export function WhyBlock({ result, onOpen }: { result: RcaResult; onOpen?: () => void }) {
  return (
    <div className="why">
      <div className="why__head">
        <Sparkles size={13} color="var(--accent-text)" aria-hidden />
        <span className="eyebrow">Root cause</span>
        <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{Math.round(result.confidence * 100)}% confidence</span>
        {onOpen && <><span className="spacer" /><button className="btn btn--ghost btn--sm" onClick={(e) => { e.stopPropagation(); onOpen(); }}>Details <ArrowRight size={12} /></button></>}
      </div>
      <div className="why__summary">{result.summary}</div>
      <RcaBreadcrumb result={result} />
    </div>
  );
}

/** KPI table; worst first (lowest budget). Used on the KPIs page and every service's KPIs tab. */
export function KpiTable({ rows, hideTarget }: { rows: Kpi[]; hideTarget?: boolean }) {
  const nav = useNavigate();
  return (
    <Table rows={rows} rowKey={(k) => k.id} onRowClick={(k) => nav(`/kpis/${k.id}`)} defaultSort={{ key: 'budget', dir: 'asc' }}
            empty="No KPI matches the filters." columns={[
            { key: 'status', header: 'Status', width: 120, sortValue: (k) => ['critical', 'warning', 'ok'].indexOf(k.status), render: (k) => <KpiStatusBadge k={k} /> },
            { key: 'name', header: 'KPI', sortValue: (k) => k.name, render: (k) => (
              <div style={{ minWidth: 0, maxWidth: hideTarget ? 190 : 320 }}>
                <div className="row" style={{ gap: 6, fontWeight: 500 }}>{k.name}{k.source === 'code' && <Code2 size={12} color="var(--accent)" aria-label="Suggested from code" />}</div>
                {hideTarget ? <span className="mono muted truncate" style={{ fontSize: 'var(--text-xs)', display: 'block' }} title={k.target.ref}>{k.target.ref}</span> : <KpiTarget k={k} />}
              </div>
            ) },
            ...(hideTarget ? [] : [{ key: 'sli', header: 'Measures', render: (k: Kpi) => <span className="secondary" style={{ whiteSpace: 'nowrap' }}>{SLI_LABEL[k.sli]} · {KPI_KIND_LABEL[k.target.kind]}</span> }]),
            { key: 'obj', header: 'Objective', numeric: true, sortValue: (k) => k.objective, render: (k) => <span style={{ whiteSpace: 'nowrap' }}>{objectivePct(k.objective)} <span className="muted">/ {k.window}</span></span> },
            { key: 'now', header: 'Good now', numeric: true, sortValue: (k) => k.current - k.objective, render: (k) => (
              <span style={{ color: k.current < k.objective ? 'var(--status-critical)' : undefined, fontWeight: k.current < k.objective ? 600 : undefined }}>{goodPct(k.current)}</span>) },
            { key: 'budget', header: 'Budget left', sortValue: (k) => k.budgetRemaining, render: (k) => (
              <div className="row" style={{ gap: 8, minWidth: 120 }}>
                <div style={{ flex: 1 }}><Meter value={k.budgetRemaining} tone={k.budgetRemaining < 0.25 ? 'critical' : k.budgetRemaining < 0.5 ? 'warning' : undefined} /></div>
                <span className="num muted" style={{ width: 34, textAlign: 'right' }}>{Math.round(k.budgetRemaining * 100)}%</span>
              </div>) },
            { key: 'burn', header: 'Burn 1h', numeric: true, sortValue: (k) => k.burnRate1h, render: (k) => `${k.burnRate1h.toFixed(1)}×` },
            { key: 'alerts', header: 'Alerts', render: (k) => {
              const firing = k.alerts.filter((a) => a.state === 'firing').length;
              return firing ? <StatusBadge status="firing" label={`${firing} firing`} /> : <span className="muted" style={{ whiteSpace: 'nowrap' }}>{k.alerts.length} rule{k.alerts.length === 1 ? '' : 's'}</span>;
            } },
            ...(hideTarget ? [] : [{ key: 'owner', header: 'Owner', render: (k: Kpi) => <span className="secondary">{k.owner}</span> }]),
            ...(hideTarget ? [] : [{ key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> }]),
          ]} />
  );
}

/** Count badge for a KPIs tab: at-risk count in status colour, otherwise total. */
export function KpiCount({ serviceId }: { serviceId: string }) {
  const list = kpis.filter((k) => k.target.serviceId === serviceId);
  const bad = list.filter((k) => k.status !== 'ok');
  return bad.length
    ? <Badge tone={bad.some((k) => k.status === 'critical') ? 'critical' : 'warning'}>{bad.length} at risk</Badge>
    : <Badge>{list.length}</Badge>;
}

/** Only the KPIs that are in trouble, shown at the top of a service page. Renders nothing when all are on track. */
const riskOrder = (a: Kpi, b: Kpi) => (a.status === 'critical' ? -1 : 1) - (b.status === 'critical' ? -1 : 1) || a.budgetRemaining - b.budgetRemaining;

export function KpisAtRisk({ serviceId }: { serviceId: string }) {
  const all = kpis.filter((k) => k.target.serviceId === serviceId);
  const list = all.filter((k) => k.status !== 'ok').sort(riskOrder);
  if (!list.length) return null;
  return (
    <Section title="KPIs at risk" subtitle={`${list.length} of ${all.length} KPIs on this ${services.find((s) => s.id === serviceId)?.type === 'http' ? 'service' : 'app'} are burning error budget`}>
      <KpiRiskGrid list={list} />
    </Section>
  );
}

/** Cards for KPIs in trouble: value, budget, burn and the root-cause sentence. */
export function KpiRiskGrid({ list, showTarget }: { list: Kpi[]; showTarget?: boolean }) {
  const nav = useNavigate();
  return (
      <div className="grid grid-3">
        {list.map((k) => {
          const rca = kpiRca(k);
          return (
            <Card key={k.id} flush onClick={() => nav(`/kpis/${k.id}`)} className={k.status === 'critical' ? 'card--rail-critical' : 'card--rail-warning'}>
              <div className="kpi-card" style={{ gap: 'var(--space-3)', padding: 'var(--space-4)' }}>
                <div className="kpi-card__head">
                  <div style={{ flex: 1, minWidth: 0 }}>
                    <h3 className="kpi-card__title">{k.name}</h3>
                    {showTarget ? <KpiTarget k={k} /> : <span className="mono muted truncate" style={{ fontSize: 'var(--text-xs)', display: 'block' }} title={k.target.ref}>{k.target.ref}</span>}
                  </div>
                  <KpiStatusBadge k={k} />
                </div>
                <div className="row" style={{ alignItems: 'baseline', gap: 'var(--space-2)' }}>
                  <span className="kpi-card__big" style={{ color: 'var(--status-critical-text)' }}>{goodPct(k.current)}</span>
                  <span className="kpi-card__sub">objective {objectivePct(k.objective)} · burn {k.burnRate1h.toFixed(1)}×</span>
                </div>
                <div className="row" style={{ gap: 'var(--space-2)' }}>
                  <span className="eyebrow">Budget</span>
                  <div style={{ flex: 1 }}><Meter value={k.budgetRemaining} tone={k.budgetRemaining < 0.25 ? 'critical' : k.budgetRemaining < 0.5 ? 'warning' : undefined} /></div>
                  <span className="num muted" style={{ fontSize: 'var(--text-sm)' }}>{Math.round(k.budgetRemaining * 100)}%</span>
                </div>
                {rca && rca.path.length > 0 && <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{rca.summary}</div>}
              </div>
            </Card>
          );
        })}
      </div>
  );
}

/** Every KPI defined on one service or app — the service page's KPIs tab — plus KPIs suggested from its code. */
export function ServiceKpis({ serviceId }: { serviceId: string }) {
  const nav = useNavigate();
  const [dismissed, setDismissed] = useState<Record<string, boolean>>({});
  const name = services.find((s) => s.id === serviceId)?.name ?? serviceId;
  const list = kpis.filter((k) => k.target.serviceId === serviceId);
  const sugs = SUGGESTED.filter((x) => x.target.split(' · ')[0] === name && !dismissed[x.id]);
  const create = () => nav(`/kpis/new?app=${serviceId}`);
  return (
    <div className="stack">
      {list.length === 0 ? (
        <Card>
          <EmptyState icon={<Target />} title="No KPIs yet">
            Define what users should experience here — alerts then fire on error-budget burn.
            <div style={{ marginTop: 'var(--space-3)' }}><Button variant="primary" icon={<Plus size={14} />} onClick={create}>New KPI</Button></div>
          </EmptyState>
        </Card>
      ) : (
        <Card flush title={`${list.length} KPI${list.length === 1 ? '' : 's'}`} subtitle="sorted by error budget left — worst first"
          actions={<Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={create}>New KPI</Button>}>
          <KpiTable rows={list} hideTarget />
        </Card>
      )}
      {sugs.length > 0 && (
        <Card flush title={<span className="row" style={{ gap: 6 }}><Sparkles size={14} color="var(--accent-text)" aria-hidden />Suggested from code</span>}
          subtitle="critical flows in this repository without a KPI">
          <div className="list">
            {sugs.map((x) => (
              <div key={x.id} className="list__row">
                <div className="list__main">
                  <span className="list__title">{x.name} <span className="muted mono" style={{ fontWeight: 400, fontSize: 'var(--text-xs)' }}>{x.target.split(' · ')[1]}</span></span>
                  <span className="list__meta">{x.why}</span>
                </div>
                <Button size="sm" variant="ghost" onClick={() => setDismissed({ ...dismissed, [x.id]: true })}>Dismiss</Button>
                <Button size="sm" onClick={() => nav(`/kpis/new?${x.q}&name=${encodeURIComponent(x.name)}`)}>Review &amp; add</Button>
              </div>
            ))}
          </div>
        </Card>
      )}
    </div>
  );
}

// ---------------------------------------------------------------- code suggestions
const SUGGESTED = [
  { id: 'search-page', name: 'Product search answers fast', target: 'shop-web · /search', why: 'Search is the second most visited page and feeds 31% of add-to-cart events.', q: 'kind=page&app=shop-web&page=search' },
  { id: 'login', name: 'Login succeeds', target: 'auth-api · POST /v1/sessions', why: 'Every checkout needs a session; failures here block all purchases.', q: 'kind=endpoint&service=auth-api' },
  { id: 'mobile-start', name: 'Shop app opens fast', target: 'shop-mobile · Cold start', why: 'Cold start p75 grew 12% over the last 3 releases.', q: 'kind=app_start&app=shop-mobile' },
];


// ---------------------------------------------------------------- all KPIs (sidebar)
/**
 * Every KPI across services. At-risk ones are cards (with the root cause), the rest a table —
 * each KPI appears once. Creating a KPI happens on the owning service's KPIs tab.
 */
export function Kpis() {
  useData();
  const [q, setQ] = useState('');
  const [app, setApp] = useState('all');
  const targets = [...new Set(kpis.map((k) => k.target.serviceId))];
  const match = (k: Kpi) => (app === 'all' || k.target.serviceId === app)
    && (!q || `${k.name} ${k.target.ref} ${k.owner} ${k.target.serviceId}`.toLowerCase().includes(q.toLowerCase()));
  const atRisk = kpis.filter((k) => k.status !== 'ok' && match(k)).sort(riskOrder);
  const onTrack = kpis.filter((k) => k.status === 'ok' && match(k));
  return (
    <>
      <PageHeader title="KPIs" subtitle={`${kpis.length} KPIs across ${targets.length} services and apps · create one from a service's KPIs tab`}
        actions={<>
          <Select aria-label="Filter by app or service" value={app} onChange={(e) => setApp(e.target.value)}
            options={[{ value: 'all', label: 'All apps & services' }, ...targets.map((t) => ({ value: t, label: services.find((s) => s.id === t)?.name ?? t }))]} />
          <div style={{ width: 220 }}><SearchInput placeholder="Search KPIs" value={q} onChange={(e) => setQ(e.target.value)} /></div>
        </>} />
      <Section title="At risk" subtitle={atRisk.length ? `${atRisk.length} burning error budget` : 'Nothing at risk'}>
        {atRisk.length ? <KpiRiskGrid list={atRisk} showTarget /> : <Card><EmptyState icon={<Target />} title="All on track" /></Card>}
      </Section>
      <Section title="On track" subtitle={`${onTrack.length} KPI${onTrack.length === 1 ? '' : 's'}`}>
        <Card flush><KpiTable rows={onTrack} /></Card>
      </Section>
    </>
  );
}
