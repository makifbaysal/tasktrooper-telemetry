import type { ReactNode } from 'react';
import { useNavigate } from 'react-router';
import { ArrowRight, FileText, GitBranch, Gauge } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Badge, Button, StatusBadge, Table, type StatusKind } from '@/components/ui';
import {
  endpointsFor, incidents, kpis, monitors, runs, scenarios, services, useData, type ChartUnit, type Widget,
} from '@/data';
import { ago, compact, ms, pct } from '@/lib/format';
import { bytes } from './k8sShared';

/** Only dashboard paths are followed; anything else from the model is dropped. */
export const safePath = (p?: string) => (p && p.startsWith('/') && !p.startsWith('//') ? p : undefined);

const findService = (s?: string) => (s ? services.find((x) => x.id === s || x.name === s) : undefined);

/** One widget from the assistant (`show_widget`): entity cards read the dashboard data layer, charts/tables carry data. */
export function AssistantWidget({ widget: w }: { widget: Widget }) {
  useData();
  switch (w.kind) {
    case 'service': {
      const s = findService(w.service);
      return (
        <EntityCard kind="Service" title={w.title ?? s?.name ?? w.service} to={`/services/${encodeURIComponent(s?.id ?? w.service)}`}
          status={s?.status} facts={s ? [`${compact(s.rpm)} ${s.type === 'http' ? 'rpm' : 'msg/min'}`, `${pct(s.errorRate)} errors`, `p95 ${ms(s.p95)}`] : ['No data in this range']} />
      );
    }
    case 'endpoint': {
      const s = findService(w.service);
      const ep = s ? endpointsFor(s.id).find((e) => e.id === w.endpoint || `${e.method} ${e.route}` === w.endpoint || e.route === w.endpoint) : undefined;
      const to = s && ep ? `/services/${encodeURIComponent(s.id)}/endpoints/${encodeURIComponent(ep.id)}` : `/services/${encodeURIComponent(s?.id ?? w.service)}`;
      return (
        <EntityCard kind={`Endpoint · ${s?.name ?? w.service}`} title={<span className="mono">{w.title ?? w.endpoint}</span>} to={to}
          facts={ep ? [`${compact(ep.rpm)} rpm`, `${pct(ep.errorRate)} errors`, `p95 ${ms(ep.p95)}`] : ['No data in this range']} />
      );
    }
    case 'incident': {
      const i = incidents.find((x) => x.id === w.id);
      const st: StatusKind | undefined = i ? (i.state === 'acknowledged' ? 'acknowledged' : i.state === 'pending' ? 'pending' : i.state === 'resolved' ? 'resolved' : 'firing') : undefined;
      return (
        <EntityCard kind="Incident" title={w.title ?? i?.title ?? `Incident ${w.id}`} to={`/incidents?id=${encodeURIComponent(w.id)}`}
          statusKind={st} facts={i ? [i.service, `now ${i.value}`, `started ${ago(i.startedAt)}`] : []} />
      );
    }
    case 'kpi': {
      const k = kpis.find((x) => x.id === w.id);
      return (
        <EntityCard kind="KPI" title={w.title ?? k?.name ?? `KPI ${w.id}`} to={`/kpis/${encodeURIComponent(w.id)}`} status={k?.status}
          facts={k ? [`objective ${pct(k.objective, 1)}`, `${pct(k.budgetRemaining, 0)} budget left`, `burn ${k.burnRate1h.toFixed(1)}× (1h)`] : []} />
      );
    }
    case 'monitor': {
      const m = monitors.find((x) => x.id === w.id);
      const s = findService(w.service ?? m?.target);
      const st: StatusKind | undefined = m ? (m.state === 'firing' ? 'firing' : m.state === 'pending' ? 'pending' : m.state === 'muted' ? 'muted' : 'ok') : undefined;
      return (
        <EntityCard kind="Monitor" title={w.title ?? m?.name ?? `Monitor ${w.id}`} to={s ? `/services/${encodeURIComponent(s.id)}?tab=alerts` : '/incidents'}
          statusKind={st} facts={m ? [m.target, m.condition, m.severity] : []} />
      );
    }
    case 'trace':
      return <EntityCard kind="Trace" icon={<GitBranch size={14} aria-hidden />} title={<span className="mono">{w.title ?? w.traceId}</span>} to={`/traces/${encodeURIComponent(w.traceId)}`} facts={[]} />;
    case 'loadtest_run': {
      const r = runs.find((x) => x.id === w.id);
      const sc = r ? scenarios.find((x) => x.id === r.scenarioId) : undefined;
      const st: StatusKind | undefined = r ? (r.state === 'passed' ? 'ok' : r.state === 'failed' ? 'critical' : r.state === 'aborted' ? 'muted' : 'pending') : undefined;
      return (
        <EntityCard kind="Load test run" icon={<Gauge size={14} aria-hidden />} title={w.title ?? sc?.name ?? `Run ${w.id}`} to={`/load-tests/runs/${encodeURIComponent(w.id)}`}
          statusKind={st} statusLabel={r?.state} facts={r ? [`${compact(r.rps)} rps`, `p95 ${ms(r.p95)}`, `${pct(r.errorRate)} errors`, `${r.vus} VUs`] : []} />
      );
    }
    case 'logs': {
      const s = findService(w.service);
      const q = new URLSearchParams({ tab: 'logs' });
      if (w.query) q.set('q', w.query);
      return (
        <EntityCard kind="Logs" icon={<FileText size={14} aria-hidden />} title={w.title ?? (w.query ? `“${w.query}”` : 'Logs')} to={s ? `/services/${encodeURIComponent(s.id)}?${q}` : '/services'}
          facts={[s?.name ?? w.service ?? 'all services', w.level ?? ''].filter(Boolean)} />
      );
    }
    case 'chart': return <ChartWidget w={w} />;
    case 'table': return <TableWidget w={w} />;
    case 'stats': return <StatsWidget w={w} />;
  }
}

function EntityCard({ kind, title, to, status, statusKind, statusLabel, facts, icon }: {
  kind: string; title: ReactNode; to: string; status?: 'ok' | 'warning' | 'critical'; statusKind?: StatusKind; statusLabel?: string; facts: string[]; icon?: ReactNode;
}) {
  const nav = useNavigate();
  const st = statusKind ?? status;
  return (
    <div className="aw aw--entity">
      <div className="aw__main">
        <span className="aw__kind">{icon}{kind}</span>
        <span className="aw__title truncate">{title}</span>
        {facts.length > 0 && <span className="aw__facts">{facts.join(' · ')}</span>}
      </div>
      {st && <StatusBadge status={st} label={statusLabel} />}
      <Button size="sm" variant="ghost" onClick={() => nav(to)}>Open <ArrowRight size={14} aria-hidden /></Button>
    </div>
  );
}

const unitFormat = (u: ChartUnit, ratio: boolean): ((v: number) => string) => {
  switch (u) {
    case 'ms': return ms;
    case 'percent': return (v) => pct(ratio ? v : v / 100);
    case 'bytes': return (v) => bytes(v);
    case 'rpm': return (v) => `${compact(v)} rpm`;
    default: return (v) => compact(v);
  }
};

function WidgetFrame({ title, link, children }: { title?: string; link?: string; children: ReactNode }) {
  const nav = useNavigate();
  const to = safePath(link);
  return (
    <div className="aw">
      {(title || to) && (
        <div className="aw__head">
          {title && <span className="aw__title">{title}</span>}
          {to && <Button size="sm" variant="ghost" onClick={() => nav(to)}>Open <ArrowRight size={14} aria-hidden /></Button>}
        </div>
      )}
      {children}
    </div>
  );
}

function ChartWidget({ w }: { w: Extract<Widget, { kind: 'chart' }> }) {
  const series = (w.series ?? []).slice(0, 4).map((s) => ({ ...s, points: (s.points ?? []).slice(0, 500) }));
  const ts = [...new Set(series.flatMap((s) => s.points.map((p) => p[0])))].sort((a, b) => a - b);
  if (ts.length === 0) return <WidgetFrame title={w.title} link={w.link}><p className="muted">No data points.</p></WidgetFrame>;
  const all = series.flatMap((s) => s.points.map((p) => p[1]));
  // percent values may come as ratios (0..1) or as 0..100
  const ratio = all.every((v) => v <= 1);
  const data = series.map((s, i) => {
    const byTs = new Map(s.points.map((p) => [p[0], p[1]]));
    return { id: `s${i}`, label: s.label, slot: i + 1, data: ts.map((t) => byTs.get(t) ?? 0) };
  });
  return (
    <WidgetFrame title={w.title} link={w.link}>
      <TimeSeries timestamps={ts} series={data} height={180} format={unitFormat(w.unit, ratio)} />
    </WidgetFrame>
  );
}

type Row = { i: number; cells: string[]; to?: string };

function TableWidget({ w }: { w: Extract<Widget, { kind: 'table' }> }) {
  const nav = useNavigate();
  const cols = w.columns ?? [];
  const rows = (w.rows ?? []).slice(0, 50).map((cells, i) => ({ i, cells, to: safePath(w.links?.[i]) }));
  const clickable = rows.some((r) => r.to);
  return (
    <WidgetFrame title={w.title}>
      <Table<Row> rows={rows} rowKey={(r) => String(r.i)} empty="No rows."
        onRowClick={clickable ? (r) => { if (r.to) nav(r.to); } : undefined}
        columns={cols.map((c, j) => ({ key: String(j), header: c, render: (r) => r.cells[j] ?? '' }))} />
    </WidgetFrame>
  );
}

function StatsWidget({ w }: { w: Extract<Widget, { kind: 'stats' }> }) {
  return (
    <WidgetFrame title={w.title} link={w.link}>
      <div className="aw__stats">
        {(w.items ?? []).slice(0, 6).map((it, i) => (
          <div key={i} className="aw__stat">
            <span className="aw__stat-label">{it.label}</span>
            <span className="aw__stat-value num">{it.value}</span>
            {it.status ? <StatusBadge status={it.status} /> : null}
          </div>
        ))}
      </div>
    </WidgetFrame>
  );
}

/** Small "referenced" chip used in the composer and in sent user messages. */
export function RefChip({ label, kind, onRemove }: { label: string; kind: string; onRemove?: () => void }) {
  return (
    <span className="aref">
      <Badge tone="accent">{kind.replace('_', ' ')}</Badge>
      <span className="aref__label truncate">{label}</span>
      {onRemove && <button type="button" className="aref__x" aria-label={`Remove ${label}`} onClick={onRemove}>×</button>}
    </span>
  );
}
