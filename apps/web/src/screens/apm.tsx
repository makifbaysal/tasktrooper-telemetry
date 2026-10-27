import type { ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { ArrowRight } from 'lucide-react';
import { TimeSeries, type Series } from '@/components/charts';
import { Badge, Button, Drawer, DrawerSection, KeyValues, StatusBadge, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  callerSeries, depErrorSeries, endpointCallers, endpointCalls, endpointsFor, outgoingSeries, series, services, timestamps,
  type CallerTraffic, type Dependency, type Endpoint, type TimeRange,
  databaseFor, topicForDep, isApi, redSeries,
} from '@/data';
import { ago, compact, ms, pct } from '@/lib/format';
import { CALLER_KIND_LABEL, DEP_KIND_LABEL, errorTone } from './shared';

/** A row of a trace list (GET /traces). */
export interface TraceRow { id: string; ts: number; dur: number; status: number; service?: string; method?: string; route?: string; isError?: boolean }

/**
 * The trace list used on endpoint and pod pages: trace id, status, duration, age. With `endpointUrl` it adds the
 * endpoint column (method + route, linked when the endpoint is known); `actions` renders a trailing per-row cell.
 */
export function TraceTable<T extends TraceRow>({ rows, onRowClick, endpointUrl, actions, empty }: {
  rows: T[]; onRowClick?: (t: T) => void; endpointUrl?: (t: T) => string | undefined; actions?: (t: T) => ReactNode; empty?: ReactNode;
}) {
  const failed = (t: T) => t.isError || t.status >= 500;
  return (
    <Table<T> rowKey={(t) => t.id} rows={rows} onRowClick={onRowClick} empty={empty} columns={[
      { key: 'id', header: 'Trace', render: (t) => <span className="mono">{t.id.slice(0, 12)}</span> },
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

/**
 * Splits a traffic series into ok / client error / server error, so ONE stacked chart
 * shows volume and failures together (replaces separate throughput + status-code charts).
 */
export function statusSplit(total: number[], errRate: number[], isConsumer: boolean) {
  const s5 = total.map((v, i) => v * errRate[i]);
  const s4 = total.map((v, i) => v * 0.012 * (1 + ((i * 7) % 5) / 10));
  return [
    { id: 'ok', label: isConsumer ? 'Processed' : '2xx–3xx', data: total.map((v, i) => Math.max(0, v - s5[i] - s4[i])), color: 'var(--series-1)' },
    { id: '4xx', label: isConsumer ? 'Retried' : '4xx', data: s4, color: 'var(--series-4)' },
    { id: '5xx', label: isConsumer ? 'Failed' : '5xx', data: s5, color: 'var(--series-8)' },
  ];
}

const small = { fontSize: 'var(--text-xs)', marginBottom: 4 } as const;

// ---------------------------------------------------------------- Dependency drawer
export function DependencyDrawer({ dep, serviceRpm, onClose }: { dep: Dependency | null; serviceRpm: number; onClose: () => void }) {
  const { range } = useApp();
  const nav = useNavigate();
  if (!dep) return null;
  const depLatency: Latency = { avg: dep.avgMs, p50: dep.avgMs * 0.75, p95: dep.p95, p99: dep.p95 * 2 };
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
      <DrawerSection title="Response time" aside={latencyHint(depLatency)}>
        <TimeSeries timestamps={ts} height={190} format={ms} series={latencySeries(`dep-${dep.serviceId}-${dep.id}`, range, depLatency, dep.errorRate > 0.01 ? { spikeAt: 0.8, spikeMul: 2.6, spikeWidth: 6 } : {})} />
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

/** Typical right-skewed ratios when only p95 is known (mock data). */
export const latencyFromP95 = (p95: number, avg?: number): Latency => ({ p50: p95 * 0.42, avg: avg ?? p95 * 0.5, p95, p99: p95 * 2.1 });

type Spike = { spikeAt?: number; spikeMul?: number; spikeWidth?: number };

/** One line per statistic. Keys `p50-…`/`p95-…` stay stable so existing charts keep their shape. */
export function latencySeries(key: string, range: TimeRange, l: Latency, spike: Spike = {}): Series[] {
  return LATENCY_STATS.map((s) => ({ id: s.id, label: s.id, slot: s.slot, data: series(`${s.id}-${key}`, range, { base: l[s.id], ...spike }) }));
}

/**
 * Latency lines of the project (no serviceId), a service or an endpoint. api mode: p50 / p95 / p99 from
 * `redSeries` (avg isn't served); mock: the synthesized `latencySeries` (unchanged).
 */
export function latencySeriesFor(key: string, range: TimeRange, l: Latency, spike: Spike = {}, serviceId?: string, endpointId?: string): Series[] {
  if (!isApi) return latencySeries(key, range, l, spike);
  const red = redSeries(range, serviceId, endpointId);
  return LATENCY_STATS.filter((s) => s.id !== 'avg').map((s) => ({ id: s.id, label: s.id, slot: s.slot, data: red[s.id as 'p50' | 'p95' | 'p99'] }));
}

/** Status-class columns: api mode reads s2xx/s3xx/s4xx/s5xx from `redSeries`; mock splits total × error rate. */
export function statusSplitFor(range: TimeRange, serviceId: string | undefined, total: number[], errRate: number[], isConsumer: boolean, endpointId?: string) {
  if (!isApi) return statusSplit(total, errRate, isConsumer);
  const red = redSeries(range, serviceId, endpointId);
  return [
    { id: 'ok', label: isConsumer ? 'Processed' : '2xx–3xx', data: red.s2xx.map((v, i) => v + (red.s3xx[i] ?? 0)), color: 'var(--series-1)' },
    { id: '4xx', label: isConsumer ? 'Retried' : '4xx', data: red.s4xx, color: 'var(--series-4)' },
    { id: '5xx', label: isConsumer ? 'Failed' : '5xx', data: red.s5xx, color: 'var(--series-8)' },
  ];
}

/** Derives the other statistics from an existing p95 series (e.g. a load-test run). */
export function latencyFromP95Series(p95: number[]): Series[] {
  const ratio: Record<LatencyStat, number> = { p50: 0.42, avg: 0.5, p95: 1, p99: 2.1 };
  return LATENCY_STATS.map((s) => ({
    id: s.id, label: s.id, slot: s.slot,
    data: s.id === 'p95' ? p95 : p95.map((v, i) => v * ratio[s.id] * (1 + (((i * 13 + s.slot * 7) % 9) - 4) / 100)),
  }));
}

/** "avg 104 ms · p50 88 ms · p99 470 ms" — the stats not shown as the headline value. */
export function latencyHint(l: Latency, headline?: LatencyStat) {
  return LATENCY_STATS.filter((s) => s.id !== headline).map((s) => `${s.id} ${ms(l[s.id])}`).join(' · ');
}
