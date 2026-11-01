import type { ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { ArrowRight } from 'lucide-react';
import { TimeSeries, type Series } from '@/components/charts';
import { Badge, Button, Drawer, DrawerSection, KeyValues, StatusBadge, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  callerSeries, depErrorSeries, depLatencySeries, endpointCallers, endpointCalls, endpointsFor, outgoingSeries, services, timestamps,
  type CallerTraffic, type Dependency, type Endpoint, type TimeRange,
  databaseFor, topicForDep, redSeries,
} from '@/data';
import { ago, compact, ms, pct } from '@/lib/format';
import { CALLER_KIND_LABEL, DEP_KIND_LABEL, errorTone, traceUrl } from './shared';

/** A row of a trace list (GET /traces). */
export interface TraceRow { id: string; ts: number; dur: number; status: number; service?: string; method?: string; route?: string; isError?: boolean }

/**
 * The trace list used on endpoint and pod pages: trace id, status, duration, age. Rows open the trace page
 * (`/traces/:traceId`) unless `onRowClick` overrides it. With `endpointUrl` it adds the endpoint column (method + route,
 * linked when the endpoint is known); `actions` renders a trailing per-row cell.
 */
export function TraceTable<T extends TraceRow>({ rows, onRowClick, endpointUrl, actions, empty }: {
  rows: T[]; onRowClick?: (t: T) => void; endpointUrl?: (t: T) => string | undefined; actions?: (t: T) => ReactNode; empty?: ReactNode;
}) {
  const nav = useNavigate();
  const failed = (t: T) => t.isError || t.status >= 500;
  return (
    <Table<T> rowKey={(t) => t.id} rows={rows} onRowClick={onRowClick ?? ((t) => nav(traceUrl(t.id)))} empty={empty} columns={[
      { key: 'id', header: 'Trace', render: (t) => <Link to={traceUrl(t.id)} className="mono" onClick={(e) => e.stopPropagation()} title={t.id}>{t.id.slice(0, 12)}</Link> },
      ...(endpointUrl ? [{ key: 'ep', header: 'Endpoint', render: (t: T) => {
        const label = <><span className="mono muted">{t.method}</span> {t.route || '—'}</>;
        const url = endpointUrl(t);
        return <span style={{ fontSize: 'var(--text-sm)', whiteSpace: 'nowrap' }}>{url ? <Link to={url} onClick={(e) => e.stopPropagation()}>{label}</Link> : label}</span>;
      } }] : []),
      { key: 's', header: 'Status', render: (t) => failed(t) ? <StatusBadge status="critical" label={t.status ? String(t.status) : 'Error'} /> : <Badge>{t.status || 'OK'}</Badge> },
      { key: 'd', header: 'Duration', numeric: true, render: (t) => ms(t.dur) },
      { key: 't', header: 'When', numeric: true, render: (t) => <span className="muted">{ago(t.ts)}</span> },
      ...(actions ? [{ key: 'a', header: '', render: actions }] : []),
    ]} />
  );
}

/** Display name: consumers/workers are named by process, HTTP endpoints by method + route. */
export const epLabel = (e: Endpoint) => e.sub ? e.sub.process : `${e.method} ${e.route}`;
/** Ref used by monitors (`svc · ref`): the process (consumer group) for subscriptions, method + route for HTTP. */
export const epRef = (e: Endpoint) => e.sub ? e.sub.process : `${e.method} ${e.route}`;
const SOURCE_LABEL = { kafka: 'Kafka', rabbitmq: 'RabbitMQ', sqs: 'SQS', cron: 'Cron' } as const;
export const sourceLabel = (e: Endpoint) => e.sub ? SOURCE_LABEL[e.sub.source] : e.method;

/** Error-rate cell: plain number when healthy, status badge when breaching. */
export function ErrorCell({ rate }: { rate: number }) {
  const t = errorTone(rate);
  return t ? <StatusBadge status={t} label={pct(rate)} /> : <>{pct(rate)}</>;
}

/** Name + secondary line, used as first cell in compact tables. */
export function NameCell({ color, name, sub, mono }: { color?: string; name: ReactNode; sub?: ReactNode; mono?: boolean }) {
  return (
    <span className="row" style={{ minWidth: 0 }}>
      {color && <span className="legend__swatch" style={{ background: color }} />}
      <span style={{ minWidth: 0 }}>
        <div className={mono ? 'mono truncate' : 'truncate'}>{name}</div>
        {sub && <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{sub}</div>}
      </span>
    </span>
  );
}

const small = { fontSize: 'var(--text-xs)', marginBottom: 4 } as const;

// ---------------------------------------------------------------- Dependency drawer
export function DependencyDrawer({ dep, serviceRpm, onClose }: { dep: Dependency | null; serviceRpm: number; onClose: () => void }) {
  const { range } = useApp();
  const nav = useNavigate();
  if (!dep) return null;
  const ts = timestamps(range);
  const target = dep.kind === 'http' ? services.find((s) => dep.name.endsWith(s.name)) : undefined;
  const db = databaseFor(dep);
  const topic = topicForDep(dep);
  const usedBy = endpointsFor(dep.serviceId)
    .map((e) => ({ e, call: endpointCalls(e).find((c) => c.dep.id === dep.id) }))
    .filter((x): x is { e: Endpoint; call: NonNullable<typeof x.call> } => !!x.call)
    .map((x) => ({ ...x, callsPerMin: x.e.rpm * x.call.callsPerReq }))
    .sort((a, b) => b.callsPerMin - a.callsPerMin);

  return (
    <Drawer open onClose={onClose}
      title={<span className="mono">{dep.name}</span>}
      subtitle={<><Badge>{DEP_KIND_LABEL[dep.kind]}</Badge>{errorTone(dep.errorRate) && <StatusBadge status={errorTone(dep.errorRate)!} label={`${pct(dep.errorRate)} failing`} />}</>}
      actions={<>
        {target && <Button size="sm" onClick={() => nav(`/services/${target.id}`)}>Open {target.name} <ArrowRight size={14} /></Button>}
        {db && <Button size="sm" onClick={() => nav(`/infrastructure/databases/${db.id}`)}>Open {db.name} · CPU {pct(db.cpu, 0)} <ArrowRight size={14} /></Button>}
        {topic && <Button size="sm" onClick={() => nav(`/infrastructure/queues/${topic.clusterId}?tab=topics&topic=${encodeURIComponent(`${topic.clusterId}/${topic.name}`)}`)}>Open topic <ArrowRight size={14} /></Button>}
      </>}>
      <KeyValues items={[
        ['Calls / min', compact(dep.callsPerMin)],
        ['Calls per request', `${(dep.callsPerMin / serviceRpm).toFixed(2)}×`],
        ['Adds per request (avg)', ms((dep.callsPerMin * dep.avgMs) / serviceRpm)],
        ['Error rate', <ErrorCell key="e" rate={dep.errorRate} />],
        ['Failed calls / min', compact(dep.callsPerMin * dep.errorRate)],
      ]} />
      <DrawerSection title="Response time" aside={`avg ${ms(dep.avgMs)} · p95 ${ms(dep.p95)}`}>
        <TimeSeries timestamps={ts} height={190} format={ms} series={[
          { id: 'avg', label: 'avg', slot: 2, data: depLatencySeries(dep, range, 'avg') },
          { id: 'p95', label: 'p95', slot: 1, data: depLatencySeries(dep, range, 'p95') },
        ]} />
      </DrawerSection>
      <DrawerSection title="Error rate">
        {dep.errorRate === 0
          ? <div className="empty" style={{ padding: 'var(--space-6) var(--space-2)', fontSize: 'var(--text-sm)' }}>No failed calls in this range</div>
          : <TimeSeries timestamps={ts} height={150} area format={(v) => pct(v)} series={[{ id: 'err', label: 'Error rate', slot: 8, data: depErrorSeries(dep, range) }]} />}
      </DrawerSection>
      <DrawerSection title="Calls / min">
        <TimeSeries timestamps={ts} height={130} area format={(v) => compact(v)} series={[{ id: 'calls', label: 'Calls', slot: 2, data: outgoingSeries(dep, range) }]} />
      </DrawerSection>
      <DrawerSection title="Called by" aside={`${usedBy.length} endpoint${usedBy.length === 1 ? '' : 's'}`}>
        <div className="card" style={{ overflow: 'hidden' }}>
          <Table rows={usedBy} rowKey={(r) => r.e.id} onRowClick={(r) => nav(`/services/${r.e.serviceId}/endpoints/${r.e.id}`)} columns={[
            { key: 'ep', header: 'Endpoint', render: (r) => <span className="truncate" style={{ display: 'block', maxWidth: 260 }}><span className="mono muted">{r.e.method}</span> {r.e.route}</span> },
            { key: 'n', header: 'Per request', numeric: true, render: (r) => `${r.call.callsPerReq}×` },
            { key: 'c', header: 'Calls / min', numeric: true, render: (r) => compact(r.callsPerMin) },
            { key: 't', header: 'Time / request', numeric: true, render: (r) => ms(r.call.msPerReq) },
          ]} />
        </div>
      </DrawerSection>
    </Drawer>
  );
}

// ---------------------------------------------------------------- Caller drawer
/** `scope` = the service or endpoint being called; for a service we also list which endpoints the caller hits. */
export function CallerDrawer({ caller, targetKey, serviceId, color, onClose }: {
  caller: CallerTraffic | null; targetKey: string; serviceId?: string; color: string; onClose: () => void;
}) {
  const { range } = useApp();
  const nav = useNavigate();
  if (!caller) return null;
  const ts = timestamps(range);
  const svc = services.find((s) => s.id === caller.caller.id);
  const hits = serviceId
    ? endpointsFor(serviceId)
        .map((e) => ({ e, c: endpointCallers(e).find((x) => x.caller.id === caller.caller.id) }))
        .filter((x): x is { e: Endpoint; c: CallerTraffic } => !!x.c)
        .sort((a, b) => b.c.rpm - a.c.rpm)
    : [];

  return (
    <Drawer open onClose={onClose} title={caller.caller.name}
      subtitle={<>{caller.caller.kind === 'unknown' ? <StatusBadge status="warning" label="Unknown caller" /> : <Badge>{CALLER_KIND_LABEL[caller.caller.kind]}</Badge>}<span className="muted">{caller.caller.via}</span></>}
      actions={svc && <Button size="sm" onClick={() => nav(`/services/${svc.id}`)}>Open {svc.name} <ArrowRight size={14} /></Button>}>
      <KeyValues items={[
        ['Requests / min', compact(caller.rpm)],
        ['Share of traffic', pct(caller.share, 1)],
        ['Error rate', <ErrorCell key="e" rate={caller.errorRate} />],
        ['p95 latency', ms(caller.p95)],
      ]} />
      {caller.caller.kind === 'unknown' && (
        <div className="muted" style={{ fontSize: 'var(--text-sm)' }}>
          Requests without trace context, API key or a known user-agent. Install the SDK in the calling app or issue it an API key to identify it.
        </div>
      )}
      <DrawerSection title="Requests / min">
        <TimeSeries timestamps={ts} height={170} area format={(v) => compact(v)}
          series={[{ id: 'rpm', label: caller.caller.name, color, data: callerSeries(targetKey, caller, range) }]} />
      </DrawerSection>
      {hits.length > 0 && (
        <DrawerSection title="Endpoints it calls" aside={`${hits.length}`}>
          <div className="card" style={{ overflow: 'hidden' }}>
            <Table rows={hits} rowKey={(r) => r.e.id} onRowClick={(r) => nav(`/services/${r.e.serviceId}/endpoints/${r.e.id}`)} columns={[
              { key: 'ep', header: 'Endpoint', render: (r) => <span className="truncate" style={{ display: 'block', maxWidth: 260 }}><span className="mono muted">{r.e.method}</span> {r.e.route}</span> },
              { key: 'rpm', header: 'rpm', numeric: true, render: (r) => compact(r.c.rpm) },
              { key: 'err', header: 'Errors', numeric: true, render: (r) => <ErrorCell rate={r.c.errorRate} /> },
              { key: 'p95', header: 'p95', numeric: true, render: (r) => ms(r.c.p95) },
            ]} />
          </div>
        </DrawerSection>
      )}
    </Drawer>
  );
}

/** Small "label above chart" for multi-chart cards. */
export function ChartLabel({ children }: { children: ReactNode }) {
  return <div className="muted" style={small}>{children}</div>;
}

// ---------------------------------------------------------------- Latency (avg + percentiles)
export type LatencyStat = 'p50' | 'avg' | 'p95' | 'p99';
export type Latency = Record<LatencyStat, number>;

/** Fixed order + slot per statistic, so p95 is the same color on every latency chart. */
export const LATENCY_STATS: { id: LatencyStat; slot: number }[] = [
  { id: 'p50', slot: 3 },
  { id: 'avg', slot: 2 },
  { id: 'p95', slot: 1 },
  { id: 'p99', slot: 7 },
];

/**
 * Latency lines (p50 / p95 / p99) of the project (no serviceId), a service or an endpoint from `redSeries`
 * (avg isn't served per bucket).
 */
export function latencySeriesFor(range: TimeRange, serviceId?: string, endpointId?: string): Series[] {
  const red = redSeries(range, serviceId, endpointId);
  return LATENCY_STATS.filter((s) => s.id !== 'avg').map((s) => ({ id: s.id, label: s.id, slot: s.slot, data: red[s.id as 'p50' | 'p95' | 'p99'] }));
}

/** Status-class columns from `redSeries` (s2xx/s3xx/s4xx/s5xx; consumers: processed / retried / failed). */
export function statusSplitFor(range: TimeRange, serviceId: string | undefined, isConsumer: boolean, endpointId?: string) {
  const red = redSeries(range, serviceId, endpointId);
  return [
    { id: 'ok', label: isConsumer ? 'Processed' : '2xx–3xx', data: red.s2xx.map((v, i) => v + (red.s3xx[i] ?? 0)), color: 'var(--series-1)' },
    { id: '4xx', label: isConsumer ? 'Retried' : '4xx', data: red.s4xx, color: 'var(--series-4)' },
    { id: '5xx', label: isConsumer ? 'Failed' : '5xx', data: red.s5xx, color: 'var(--series-8)' },
  ];
}

/** "p50 88 ms · p99 470 ms" — the known stats not shown as the headline value (unknown / zero ones are skipped). */
export function latencyHint(l: Partial<Latency>, headline?: LatencyStat) {
  return LATENCY_STATS.filter((s) => s.id !== headline && (l[s.id] ?? 0) > 0).map((s) => `${s.id} ${ms(l[s.id]!)}`).join(' · ');
}
