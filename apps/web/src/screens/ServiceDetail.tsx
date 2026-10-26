import { ServiceLoadTests } from './ServiceLoadTests';
import { ServiceLag } from './ConsumerLag';
import { useState } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import { Bell, Play } from 'lucide-react';
import { BreakdownBar, Columns, Flow, Legend, Sparkline, TimeSeries, seriesColor } from '@/components/charts';
import {
  Badge, Button, Card, Drawer, DrawerSection, EmptyState, KeyValues, MetricCard, PageHeader, Section, Segmented, StatusBadge, Table, Tabs, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  TIME_CATEGORIES, breakdownSeries, callerSeries, dependenciesFor, endpointBreakdown, endpointErrorSeries, endpointTrafficSeries,
  endpointsFor, incidentShape, isClientApp, outgoingSeries, serviceBreakdown, serviceCallers, serviceErrorSeries, serviceTrafficSeries, services, timestamps,
  groupLag, groupsOfService, worst, groupHealth, deployEvents, deploysOf, DEPLOY_SOURCE_LABEL, type DeployMarker,
  type CallerTraffic, type Dependency, type Endpoint, type Service,
} from '@/data';
import { ago, compact, dateTime, ms, pct } from '@/lib/format';
import { CallerDrawer, DependencyDrawer, ErrorCell, NameCell, epLabel, sourceLabel, latencyFromP95, latencyHint, latencySeriesFor, statusSplitFor } from './apm';
import { CALLER_KIND_LABEL, CriticalityBadge, DEP_KIND_LABEL, LINE_SLOTS, ServiceType, callerColorFor, categoryOf } from './shared';
import { RootCauseCard, diagnose } from './rca';
import { LogExplorer } from './Logs';
import { AlertCount, AlertsBody, OpenIncidents } from './Alerts';
import { KpiCount, KpisAtRisk, ServiceKpis } from './Kpis';
import { FrontendApp } from './FrontendApp';
import { DeployList } from './Integrations';

type Tab = 'endpoints' | 'lag' | 'traffic' | 'time' | 'deploys' | 'loadtests' | 'kpis' | 'alerts' | 'logs';
const TABS: Tab[] = ['endpoints', 'lag', 'traffic', 'time', 'deploys', 'loadtests', 'kpis', 'alerts', 'logs'];

/** Color slot = entity's fixed position in its list, not its rank, so it matches across charts. */
const slotAt = (i: number) => LINE_SLOTS[i % LINE_SLOTS.length];

export function ServiceDetail() {
  const { serviceId = '' } = useParams();
  const nav = useNavigate();
  const { range } = useApp();
  const [params, setParams] = useSearchParams();
  const raw = params.get('tab') as Tab | null;
  const tab: Tab = raw && TABS.includes(raw) ? raw : 'endpoints';
  const setTab = (t: Tab) => setParams(t === 'endpoints' ? {} : { tab: t }, { replace: true });

  const svc = services.find((s) => s.id === serviceId);
  if (!svc) return <EmptyState icon={<Bell />} title="Service not found" />;
  const ts = timestamps(range);
  const eps = endpointsFor(svc.id);
  const isConsumer = svc.type === 'consumer' || svc.type === 'worker';
  const unit = isConsumer ? 'msg/min' : 'rpm';
  const critical = svc.status === 'critical';

  if (isClientApp(svc)) {
    return (
      <>
        <PageHeader title={svc.name}
          crumbs={[{ label: categoryOf(svc.type).label, to: `/services?type=${categoryOf(svc.type).id}` }, { label: svc.name }]}
          subtitle={<span className="row" style={{ gap: 12 }}><StatusBadge status={svc.status} /><ServiceType type={svc.type} /><span className="muted">{svc.language} · {svc.version}</span></span>}
          />
        <OpenIncidents serviceId={svc.id} />
        <KpisAtRisk serviceId={svc.id} />
        <FrontendApp key={svc.id} app={svc} />
      </>
    );
  }

  return (
    <>
      <PageHeader title={svc.name}
        crumbs={[{ label: categoryOf(svc.type).label, to: `/services?type=${categoryOf(svc.type).id}` }, { label: svc.name }]}
        subtitle={<span className="row" style={{ gap: 12 }}><StatusBadge status={svc.status} /><ServiceType type={svc.type} /><span className="muted">{svc.language} · {svc.version} · {svc.instances} instances</span></span>}
        actions={<>
          <Button icon={<Play size={14} />} onClick={() => nav('/load-tests')}>Load test</Button>
        </>} />

      <OpenIncidents serviceId={svc.id} />
      {diagnose(svc.id).map((d) => <RootCauseCard key={d.metric} d={d} scope={svc.name} />)}
      <KpisAtRisk serviceId={svc.id} />

      <HealthSection svc={svc} ts={ts} unit={unit} isConsumer={isConsumer} critical={critical} />

      <Section>
        <Tabs value={tab} onChange={setTab} tabs={[
          { id: 'endpoints', label: <>{isConsumer ? 'Processes' : 'Endpoints'} <Badge>{eps.length}</Badge></> },
          ...(groupsOfService(svc.id).length ? [{ id: 'lag' as const, label: <>Lag <LagBadge serviceId={svc.id} /></> }] : []),
          { id: 'traffic', label: 'Traffic flow' },
          { id: 'time', label: 'Time spent' },
          { id: 'deploys', label: 'Deploys' },
          ...(svc.type === 'http' ? [{ id: 'loadtests' as const, label: 'Load tests' }] : []),
          { id: 'kpis', label: <>KPIs <KpiCount serviceId={svc.id} /></> },
          { id: 'alerts', label: <>Alerts <AlertCount serviceId={svc.id} /></> },
          { id: 'logs', label: 'Logs' },
        ]} />
        {tab === 'endpoints' && <EndpointsTab eps={eps} unit={unit} isConsumer={isConsumer} onOpen={(e) => nav(`/services/${svc.id}/endpoints/${e.id}`)} />}
        {tab === 'lag' && <ServiceLag serviceId={svc.id} />}
        {tab === 'traffic' && <TrafficTab svc={svc} eps={eps} ts={ts} unit={unit} isConsumer={isConsumer} />}
        {tab === 'time' && <TimeTab svc={svc} eps={eps} ts={ts} critical={critical} onOpen={(e) => nav(`/services/${svc.id}/endpoints/${e.id}?tab=time`)} />}
        {tab === 'deploys' && <DeploysTab svc={svc} />}
        {tab === 'loadtests' && <ServiceLoadTests key={svc.id} serviceId={svc.id} />}
        {tab === 'kpis' && <ServiceKpis serviceId={svc.id} />}
        {tab === 'alerts' && <AlertsBody key={svc.id} serviceId={svc.id} />}
        {tab === 'logs' && <LogExplorer key={svc.id} serviceId={svc.id} />}
      </Section>
    </>
  );
}

/** Total broker lag of a consumer; tone follows its worst group. */
function LagBadge({ serviceId }: { serviceId: string }) {
  const gs = groupsOfService(serviceId);
  const h = worst(gs.map(groupHealth));
  return <Badge tone={h === 'critical' ? 'critical' : h === 'warning' ? 'warning' : 'neutral'}>{compact(gs.reduce((a, g) => a + groupLag(g), 0))}</Badge>;
}

// ---------------------------------------------------------------- KPIs measured on this app / service
/** KPIs targeting this service; degraded ones show their root cause right here. */
// ---------------------------------------------------------------- Health (always visible)
/**
 * The golden signals, each shown exactly once. Traffic is the hero: its stacked
 * status columns show volume AND failures; error rate and latency sit beside it.
 */
function HealthSection({ svc, ts, unit, isConsumer, critical }: { svc: Service; ts: number[]; unit: string; isConsumer: boolean; critical: boolean }) {
  const { range } = useApp();
  const spike = incidentShape(svc.id, svc.prevP95 / svc.p95);
  const total = serviceTrafficSeries(svc.id, range);
  const err = serviceErrorSeries(svc.id, range);
  const lat = latencyFromP95(svc.p95, serviceBreakdown(svc.id).avgMs);
  return (
    <div className="grid grid-3">
      <MetricCard className="span-2 metric--hero" label={isConsumer ? 'Messages processed' : 'Requests'}
        value={compact(svc.rpm)} unit={unit} curr={svc.rpm} prev={svc.prevRpm}
        hint={<>{compact(svc.rpm * svc.errorRate)} failed / min{svc.lag !== undefined && <> · consumer lag <strong className="secondary">{compact(svc.lag)}</strong> msgs</>}</>}>
        <Columns timestamps={ts} height={300} format={(v) => compact(v)} series={statusSplitFor(range, svc.id, total, err, isConsumer)} />
      </MetricCard>
      <div className="stack">
        <MetricCard label="Error rate" value={pct(svc.errorRate)} curr={svc.errorRate} prev={svc.prevErrorRate} upIsGood={false}
          badge={critical ? <StatusBadge status="firing" label="Alert firing" /> : undefined}>
          <TimeSeries timestamps={ts} format={(v) => pct(v)} height={104} area events={deployEvents(svc.id, ts[0])}
            threshold={critical ? { value: 0.02, label: '2%' } : undefined}
            series={[{ id: 'err', label: 'Error rate', slot: 8, data: err }]} />
        </MetricCard>
        <MetricCard label={isConsumer ? 'Processing time · p95' : 'Latency · p95'} value={ms(svc.p95)} curr={svc.p95} prev={svc.prevP95} upIsGood={false}
          hint={latencyHint(lat, 'p95')}>
          <TimeSeries timestamps={ts} format={ms} height={140} events={deployEvents(svc.id, ts[0])} series={latencySeriesFor(svc.id, range, lat, spike, svc.id)} />
        </MetricCard>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------- Endpoints / processes
function EndpointsTab({ eps, unit, isConsumer, onOpen }: { eps: Endpoint[]; unit: string; isConsumer: boolean; onOpen: (e: Endpoint) => void }) {
  const { range } = useApp();
  if (isConsumer && eps.some((e) => e.sub)) return <ProcessesTab eps={eps} unit={unit} onOpen={onOpen} />;
  return (
    <Card flush>
      <Table rows={eps} rowKey={(e) => e.id} defaultSort={{ key: 'rpm', dir: 'desc' }} onRowClick={onOpen} columns={[
        { key: 'route', header: 'Endpoint', sortValue: (e) => e.route, render: (e) => <span style={{ whiteSpace: 'nowrap' }}><span className="mono muted" style={{ display: 'inline-block', width: 48 }}>{e.method}</span>{e.route}</span> },
        { key: 'crit', header: 'Criticality', sortValue: (e) => ['critical', 'important', 'low'].indexOf(e.criticality), render: (e) => <CriticalityBadge c={e.criticality} /> },
        { key: 'rpm', header: unit, numeric: true, sortValue: (e) => e.rpm, render: (e) => compact(e.rpm) },
        { key: 'rpm-t', header: '', render: (e) => <Sparkline data={endpointTrafficSeries(e, range).slice(-24)} width={72} height={24} /> },
        { key: 'err', header: 'Error rate', numeric: true, sortValue: (e) => e.errorRate, render: (e) => <ErrorCell rate={e.errorRate} /> },
        { key: 'err-t', header: '', render: (e) => <Sparkline data={endpointErrorSeries(e, range).slice(-24)} width={72} height={24} /> },
        { key: 'p95', header: 'p95', numeric: true, sortValue: (e) => e.p95, render: (e) => ms(e.p95) },
        { key: 'mon', header: 'Monitor', render: (e) => e.monitored ? <Badge tone="good" icon={<Bell />}>Active</Badge> : <span className="muted">None</span> },
      ]} />
    </Card>
  );
}

/**
 * Consumers/workers have no endpoints: each row is a process (one consumer group)
 * and the topic/queue/schedule it reads is a separate column — several processes
 * can read the same topic.
 */
function ProcessesTab({ eps, unit, onOpen }: { eps: Endpoint[]; unit: string; onOpen: (e: Endpoint) => void }) {
  const readers = (e: Endpoint) => eps.filter((x) => x.sub && e.sub && x.sub.topic === e.sub.topic).length;
  return (
    <Card flush>
      <Table rows={eps} rowKey={(e) => e.id} defaultSort={{ key: 'rpm', dir: 'desc' }} onRowClick={onOpen} columns={[
        { key: 'proc', header: 'Process', sortValue: (e) => epLabel(e), render: (e) => (
          <div style={{ whiteSpace: 'nowrap' }}>
            <div className="row" style={{ gap: 6, fontWeight: 500 }}>{epLabel(e)}{e.criticality === 'critical' && <CriticalityBadge c={e.criticality} />}{e.monitored && <span title="Monitor active" style={{ display: 'inline-flex', color: 'var(--status-good)' }}><Bell size={12} aria-label="Monitor active" /></span>}</div>
            <div className="muted mono" style={{ fontSize: 'var(--text-xs)' }}>{e.sub?.schedule ? `schedule ${e.sub.schedule}` : e.sub?.group ? `group ${e.sub.group}` : ''}</div>
          </div>) },
        { key: 'topic', header: 'Reads from', sortValue: (e) => e.sub?.topic ?? e.route, render: (e) => (
          <div style={{ whiteSpace: 'nowrap' }}>
            <div className="mono">{e.sub?.topic ?? e.route}</div>
            <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{sourceLabel(e)}{readers(e) > 1 && <> · +{readers(e) - 1} more reader{readers(e) > 2 ? 's' : ''}</>}</div>
          </div>) },
        { key: 'rpm', header: unit, numeric: true, sortValue: (e) => e.rpm, render: (e) => compact(e.rpm) },
        { key: 'lag', header: 'Lag', numeric: true, sortValue: (e) => e.sub?.lag ?? 0, render: (e) => e.sub?.source === 'cron' ? <span className="muted">—</span> : compact(e.sub?.lag ?? 0) },
        { key: 'err', header: 'Error rate', numeric: true, sortValue: (e) => e.errorRate, render: (e) => <ErrorCell rate={e.errorRate} /> },
        { key: 'p95', header: 'p95', numeric: true, sortValue: (e) => e.p95, render: (e) => ms(e.p95) },
        { key: 'dlq', header: 'Retry / DLQ', numeric: true, sortValue: (e) => e.sub?.dlqPerMin ?? 0, render: (e) => <span>{compact(e.sub?.retriesPerMin ?? 0)} <span className="muted">/</span> {compact(e.sub?.dlqPerMin ?? 0)}</span> },
      ]} />
    </Card>
  );
}

// ---------------------------------------------------------------- Traffic flow
function TrafficTab({ svc, eps, ts, unit, isConsumer }: { svc: Service; eps: Endpoint[]; ts: number[]; unit: string; isConsumer: boolean }) {
  const { range } = useApp();
  const [by, setBy] = useState<'endpoint' | 'caller'>('caller');
  const depDrawer = useDrawer<Dependency>();
  const callerDrawer = useDrawer<CallerTraffic>();
  const deps = dependenciesFor(svc.id);
  const callers = serviceCallers(svc.id);
  const callerColor = callerColorFor(callers.map((c) => c.caller.id));
  const depColor = (d: Dependency) => seriesColor(slotAt(deps.indexOf(d)));
  const outPerMin = deps.reduce((a, d) => a + d.callsPerMin, 0);
  const outErr = deps.reduce((a, d) => a + d.errorRate * d.callsPerMin, 0) / (outPerMin || 1);

  return (
    <>
      <Card title={isConsumer ? 'What it reads, what it calls' : 'Who calls it, what it calls'}
        subtitle={`${compact(svc.rpm)} ${unit} in · ${compact(outPerMin)} calls/min out · ${(outPerMin / svc.rpm).toFixed(1)} calls per ${isConsumer ? 'message' : 'request'} · ${pct(outErr)} of outgoing calls fail`}>
        <Flow leftTitle={isConsumer ? 'Reads from' : undefined}
          center={{ label: svc.name, sub: `${compact(svc.rpm)} ${unit}` }}
          left={callers.map((c) => ({ id: `in-${c.caller.id}`, label: c.caller.name, sub: CALLER_KIND_LABEL[c.caller.kind], value: c.rpm, display: compact(c.rpm), color: callerColor(c.caller.id) }))}
          right={deps.map((d) => ({ id: `out-${d.id}`, label: d.name, sub: DEP_KIND_LABEL[d.kind], value: d.callsPerMin, display: compact(d.callsPerMin), color: depColor(d) }))} />
      </Card>

      <div className="grid grid-2">
        <Card title="Incoming" subtitle={unit} flush
          actions={<Segmented label="Split incoming by" value={by} onChange={setBy} options={[{ id: 'caller', label: isConsumer ? 'By source' : 'By caller' }, { id: 'endpoint', label: isConsumer ? 'By process' : 'By endpoint' }]} />}>
          <div style={{ padding: '0 var(--space-4) var(--space-3)' }}>
            {by === 'caller'
              ? <Columns timestamps={ts} height={200} format={(v) => compact(v)}
                  series={callers.map((c) => ({ id: c.caller.id, label: c.caller.name, color: callerColor(c.caller.id), data: callerSeries(svc.id, c, range) }))} />
              : <Columns timestamps={ts} height={200} format={(v) => compact(v)}
                  series={eps.map((e, i) => ({ id: e.id, label: epLabel(e), color: seriesColor(slotAt(i)), data: endpointTrafficSeries(e, range) }))} />}
          </div>
          {by === 'caller' && (
            <Table rows={callers} rowKey={(c) => c.caller.id} defaultSort={{ key: 'rpm', dir: 'desc' }} onRowClick={callerDrawer.open} columns={[
              { key: 'name', header: isConsumer ? 'Source' : 'Caller', sortValue: (c) => c.caller.name, render: (c) => (
                <NameCell color={callerColor(c.caller.id)} name={c.caller.name}
                  sub={c.caller.kind === 'unknown' ? 'Unidentified' : CALLER_KIND_LABEL[c.caller.kind]} />) },
              { key: 'rpm', header: unit, numeric: true, sortValue: (c) => c.rpm, render: (c) => compact(c.rpm) },
              { key: 'share', header: 'Share', numeric: true, sortValue: (c) => c.share, render: (c) => pct(c.share, 1) },
              { key: 'err', header: 'Errors', numeric: true, sortValue: (c) => c.errorRate, render: (c) => <ErrorCell rate={c.errorRate} /> },
            ]} />
          )}
        </Card>

        <Card title="Outgoing" subtitle="calls/min · click a dependency for response time & errors over time" flush>
          <div style={{ padding: '0 var(--space-4) var(--space-3)' }}>
            <Columns timestamps={ts} height={200} format={(v) => compact(v)}
              series={deps.map((d) => ({ id: d.id, label: d.name, color: depColor(d), data: outgoingSeries(d, range) }))} />
          </div>
          <Table<Dependency> rows={deps} rowKey={(d) => d.id} defaultSort={{ key: 'err', dir: 'desc' }} onRowClick={depDrawer.open} columns={[
            { key: 'name', header: 'Dependency', sortValue: (d) => d.name, render: (d) => <NameCell mono color={depColor(d)} name={d.name} sub={DEP_KIND_LABEL[d.kind]} /> },
            { key: 'calls', header: 'Calls/min', numeric: true, sortValue: (d) => d.callsPerMin, render: (d) => compact(d.callsPerMin) },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (d) => d.p95, render: (d) => ms(d.p95) },
            { key: 'err', header: 'Errors', numeric: true, sortValue: (d) => d.errorRate, render: (d) => <ErrorCell rate={d.errorRate} /> },
          ]} />
        </Card>
      </div>

      <DependencyDrawer dep={depDrawer.item} serviceRpm={svc.rpm} onClose={depDrawer.close} />
      <CallerDrawer caller={callerDrawer.item} targetKey={svc.id} serviceId={svc.id}
        color={callerDrawer.item ? callerColor(callerDrawer.item.caller.id) : ''} onClose={callerDrawer.close} />
    </>
  );
}

// ---------------------------------------------------------------- Time spent
function TimeTab({ svc, eps, ts, critical, onOpen }: { svc: Service; eps: Endpoint[]; ts: number[]; critical: boolean; onOpen: (e: Endpoint) => void }) {
  const { range } = useApp();
  const depDrawer = useDrawer<Dependency>();
  const total = serviceBreakdown(svc.id);
  const perCat = breakdownSeries(svc.id, total, range, critical);
  const deps = dependenciesFor(svc.id);
  const rows = eps.map((e) => ({ e, b: endpointBreakdown(e) }));
  const maxAvg = Math.max(...rows.map((r) => r.b.avgMs));
  const totalWork = rows.reduce((a, r) => a + r.e.rpm * r.b.avgMs, 0);
  const segments = (b: ReturnType<typeof endpointBreakdown>) =>
    TIME_CATEGORIES.map((c) => ({ id: c.id, label: c.label, value: b.parts[c.id], color: seriesColor(c.slot) }));
  const depMs = (d: Dependency) => (d.callsPerMin * d.avgMs) / svc.rpm;
  const isConsumer = svc.type === 'consumer' || svc.type === 'worker';

  return (
    <>
      <div className="grid grid-3">
        <Card className="span-2" title="Where request time goes" subtitle={`Avg ${ms(total.avgMs)} per request, by kind of work`}>
          <Columns timestamps={ts} height={260} format={ms}
            series={TIME_CATEGORIES.map((c) => ({ id: c.id, label: c.label, color: seriesColor(c.slot), data: perCat[c.id] }))} />
        </Card>
        <Card title="Per request, now" subtitle="Click a dependency for details" flush>
          <div className="list">
            {TIME_CATEGORIES.map((c) => {
              const inCat = deps.filter((d) => d.kind === c.id).sort((a, b) => depMs(b) - depMs(a));
              return (
                <div key={c.id} style={{ paddingBottom: inCat.length ? 'var(--space-2)' : undefined, borderTop: '1px solid var(--divider)' }}>
                  <div className="row" style={{ padding: 'var(--space-3) var(--space-4) var(--space-1)' }}>
                    <span className="legend__swatch" style={{ background: seriesColor(c.slot) }} />
                    <span className="list__title" style={{ flex: 1 }}>{c.label}</span>
                    <span className="num">{ms(total.parts[c.id])}</span>
                    <span className="num muted" style={{ width: 36, textAlign: 'right' }}>{Math.round((total.parts[c.id] / total.avgMs) * 100)}%</span>
                  </div>
                  {inCat.map((d) => (
                    <div key={d.id} className="row list__row--clickable" onClick={() => depDrawer.open(d)}
                      style={{ padding: '2px var(--space-4) 2px calc(var(--space-4) + 20px)', fontSize: 'var(--text-sm)' }}>
                      <span className="mono secondary truncate" style={{ flex: 1 }}>{d.name.split(' · ')[1] ?? d.name}</span>
                      <span className="num secondary">{ms(depMs(d))}</span>
                      <span style={{ width: 36 }} />
                    </div>
                  ))}
                </div>
              );
            })}
          </div>
        </Card>
      </div>

      <Card title={`Time per ${isConsumer ? 'process' : 'endpoint'}`} subtitle="Bar = avg request time, colored by where it goes · click a row for details" flush
        actions={<Legend kind="swatch" items={TIME_CATEGORIES.map((c) => ({ label: c.label, color: seriesColor(c.slot) }))} />}>
        <Table rows={rows} rowKey={(r) => r.e.id} defaultSort={{ key: 'share', dir: 'desc' }} onRowClick={(r) => onOpen(r.e)} columns={[
          { key: 'ep', header: isConsumer ? 'Process' : 'Endpoint', sortValue: (r) => epLabel(r.e), render: (r) => r.e.sub
            ? <span className="truncate" style={{ display: 'block', maxWidth: 240 }}>{r.e.sub.process} <span className="mono muted">· {r.e.sub.topic}</span></span>
            : <span className="truncate" style={{ display: 'block', maxWidth: 240 }}><span className="mono muted">{r.e.method}</span> {r.e.route}</span> },
          { key: 'avg', header: 'Avg', numeric: true, sortValue: (r) => r.b.avgMs, render: (r) => ms(r.b.avgMs) },
          { key: 'bar', header: 'Breakdown', width: '40%', render: (r) => <BreakdownBar segments={segments(r.b)} max={maxAvg} format={ms} /> },
          { key: 'share', header: 'Share of service time', numeric: true, sortValue: (r) => r.e.rpm * r.b.avgMs, render: (r) => pct((r.e.rpm * r.b.avgMs) / totalWork, 1) },
        ]} />
      </Card>

      <DependencyDrawer dep={depDrawer.item} serviceRpm={svc.rpm} onClose={depDrawer.close} />
    </>
  );
}

// ---------------------------------------------------------------- Deploys
/** Deploys from span versions, Vercel and CI webhooks (data/integrations.ts); click → before / after. */
function DeploysTab({ svc }: { svc: Service }) {
  const nav = useNavigate();
  const { range } = useApp();
  const d = useDrawer<DeployMarker>();
  const deploys = deploysOf(svc.id);
  const ts = timestamps(range);
  const latest = !!d.item && d.item === deploys[0];
  return (
    <>
      <DeployList deploys={deploys} onOpen={d.open} />
      {d.item && (
        <Drawer open onClose={d.close} title={<span className="mono">{d.item.version}</span>} subtitle={d.item.commitMessage}
          actions={<Button size="sm" onClick={() => nav('/code')}>Code insights</Button>}>
          <KeyValues items={[
            ['Deployed', `${dateTime(d.item.ts)} · ${ago(d.item.ts)}`], ['Author', d.item.author ?? '—'],
            ['Source', DEPLOY_SOURCE_LABEL[d.item.source]], ['Environment', d.item.env],
            ['Error rate before', pct(latest ? svc.prevErrorRate : svc.errorRate)], ['Error rate after', <ErrorCell key="a" rate={svc.errorRate} />],
            ['p95 before', ms(latest ? svc.prevP95 : svc.p95)], ['p95 after', ms(svc.p95)],
            ...(d.item.url ? [['Link', <a key="u" href={d.item.url} target="_blank" rel="noreferrer">{d.item.url.replace(/^https?:\/\//, '')}</a>] as [string, React.ReactNode]] : []),
          ]} />
          {d.item.ts >= ts[0] && (
            <DrawerSection title="Error rate around the deploy">
              <TimeSeries timestamps={ts} height={170} area format={(v) => pct(v)}
                events={deployEvents(svc.id, ts[0]).filter((e) => e.ts === d.item!.ts)}
                series={[{ id: 'err', label: 'Error rate', slot: 8, data: serviceErrorSeries(svc.id, range) }]} />
            </DrawerSection>
          )}
        </Drawer>
      )}
    </>
  );
}

