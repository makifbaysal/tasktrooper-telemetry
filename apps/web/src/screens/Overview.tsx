import { useNavigate } from 'react-router';
import { AlertOctagon, ArrowRight, Sparkles } from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Card, Drawer, DrawerSection, KeyValues, Meter, MetricCard, PageHeader, Section, StatusBadge, Table, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  deployMarkers, endpointsFor, incidents, inventory, isApi, kpiSeries, kpis, monitors, redSeries, series, serviceErrorSeries, serviceTrafficSeries, services, suggestions, timestamps,
  type Endpoint, type Incident,
} from '@/data';
import { ago, compact, ms, pct } from '@/lib/format';
import { ErrorCell, latencyHint, latencySeriesFor, type Latency } from './apm';
import { ServiceType } from './shared';
import { PathCrumbs, incidentCause } from './rca';
import { ruleSeries } from '@/lib/lagAlerts';

const LAT: Latency = { p50: 88, avg: 104, p95: 214, p99: 470 };

type Attention = Endpoint & { service: string; reason: 'errors' | 'slow'; score: number };

export function Overview() {
  const { range, env } = useApp();
  const nav = useNavigate();
  const inc = useDrawer<Incident>();
  const ts = timestamps(range);
  const backend = services.filter((s) => s.type === 'http' || s.type === 'consumer' || s.type === 'worker');
  const rpm = backend.reduce((a, s) => a + s.rpm, 0);
  const prevRpm = backend.reduce((a, s) => a + s.prevRpm, 0);
  const err = backend.reduce((a, s) => a + s.errorRate * s.rpm, 0) / rpm;
  const prevErr = backend.reduce((a, s) => a + s.prevErrorRate * s.prevRpm, 0) / prevRpm;
  // api: project-wide RED series (`/overview/series`); mock: synthesized curves (unchanged)
  const red = isApi ? redSeries(range) : undefined;
  const errSeries = red ? red.errorRate : series('err-all', range, { base: err, noise: 0.25, spikeAt: 0.82, spikeMul: 3.4, spikeWidth: 5 });
  const rpmSeries = red ? red.rpm : series('t-rpm', range, { base: rpm });
  const p95Now = red ? (red.p95.filter((v) => v > 0).at(-1) ?? 0) : 214;
  // the newest bucket is still filling, so take the last non-empty one
  const lastNonZero = (xs: number[]) => xs.filter((v) => v > 0).at(-1) ?? 0;
  const latNow: Latency = red ? { p50: lastNonZero(red.p50), avg: 0, p95: p95Now, p99: lastNonZero(red.p99) } : LAT;
  const heroEvents = isApi
    ? deployMarkers.filter((d) => d.ts >= ts[0] && d.state !== 'error' && d.state !== 'canceled').map((d) => ({ ts: d.ts, label: `deploy ${d.service} ${d.version}`, detail: d.commitMessage }))
    : [{ ts: ts[Math.round(ts.length * 0.78)], label: 'deploy payments-api v1.18.3' }];

  // One list instead of "slowest" + "most failing": an endpoint shows up once, with the reason it's here.
  const attention: Attention[] = backend
    .flatMap((s) => endpointsFor(s.id).map((e) => ({ ...e, service: s.name })))
    .map((e) => {
      const errScore = e.errorRate >= 0.005 ? e.errorRate * e.rpm : 0;
      const slowScore = e.p95 >= 300 ? (e.p95 / 300) * 10 : 0;
      return { ...e, reason: errScore >= slowScore ? 'errors' as const : 'slow' as const, score: Math.max(errScore, slowScore) };
    })
    .filter((e) => e.score > 0)
    .sort((a, b) => b.score - a.score)
    .slice(0, 6);

  const critical = inventory.filter((i) => i.criticality === 'critical');
  const covered = critical.filter((i) => i.monitored).length;
  const coverage = critical.length ? covered / critical.length : 0;
  const open = incidents.filter((i) => i.state !== 'resolved');
  const ordered = [...backend].sort((a, b) => ['critical', 'warning', 'ok'].indexOf(a.status) - ['critical', 'warning', 'ok'].indexOf(b.status) || b.rpm - a.rpm);

  return (
    <>
      <PageHeader title="Overview" subtitle={`shop-platform · ${env} · last ${range}`} />

      {open.length > 0 && (
        <Card flush title={<span className="row"><AlertOctagon size={16} color="var(--status-critical)" aria-hidden />{open.length} open incidents</span>}
          subtitle="each service also lists its own under its Alerts tab"
          actions={<Button size="sm" variant="ghost" onClick={() => nav('/incidents')}>All incidents <ArrowRight size={14} /></Button>}>
          <div className="list">
            {open.map((i) => {
              const cause = incidentCause(i);
              return (
                <div key={i.id} className="list__row list__row--clickable" onClick={() => inc.open(i)}>
                  <StatusBadge status={i.state === 'acknowledged' ? 'acknowledged' : i.state === 'pending' ? 'pending' : 'firing'} />
                  <div className="list__main">
                    <span className="list__title">{i.title}</span>
                    <span className="list__meta">{i.service} · now {i.value}</span>
                    {cause && <span className="secondary" style={{ fontSize: 'var(--text-sm)', whiteSpace: 'normal' }}><strong>Why:</strong> {cause.summary}</span>}
                  </div>
                  <span className="muted">{ago(i.startedAt)}</span>
                </div>
              );
            })}
          </div>
        </Card>
      )}

      <div className="grid grid-3">
        <MetricCard className="span-2 metric--hero" label="Error rate · all backend services" value={pct(err)} curr={err} prev={prevErr} upIsGood={false}
          hint={`${compact(rpm * err)} failed requests / min · SLO 1%`}>
          <TimeSeries timestamps={ts} series={[{ id: 'err', label: 'Error rate', slot: 8, data: errSeries }]} area
            format={(v) => pct(v)} height={300} threshold={{ value: 0.01, label: 'SLO 1%' }}
            events={heroEvents} />
        </MetricCard>
        <div className="stack">
          <MetricCard label="Throughput" value={compact(rpm)} unit="rpm" curr={rpm} prev={prevRpm}>
            <TimeSeries timestamps={ts} height={96} area format={(v) => compact(v)}
              series={[{ id: 'rpm', label: 'Throughput', slot: 1, data: rpmSeries }]} />
          </MetricCard>
          <MetricCard label="Latency · p95 · all endpoints" value={String(Math.round(p95Now))} unit="ms" curr={p95Now} prev={red ? p95Now : 198} upIsGood={false}
            hint={red ? `p50 ${ms(latNow.p50)} · p99 ${ms(latNow.p99)}` : latencyHint(LAT, 'p95')}>
            <TimeSeries timestamps={ts} height={120} format={ms} series={latencySeriesFor('all', range, LAT, { spikeAt: 0.82, spikeMul: 1.6, spikeWidth: 5 })} />
          </MetricCard>
        </div>
      </div>

      <Section title="Services" subtitle="unhealthy first · click for details"
        actions={<Button size="sm" variant="ghost" onClick={() => nav('/services')}>All services <ArrowRight size={14} /></Button>}>
        <Card flush>
          <Table rows={ordered} rowKey={(s) => s.id} onRowClick={(s) => nav(`/services/${s.id}`)} columns={[
            { key: 'name', header: 'Service', render: (s) => <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{s.name}</div><div style={{ fontSize: 'var(--text-xs)' }}><ServiceType type={s.type} /></div></div> },
            { key: 'status', header: 'Health', render: (s) => <StatusBadge status={s.status} /> },
            { key: 'rpm', header: 'Traffic', numeric: true, render: (s) => `${compact(s.rpm)} ${s.type === 'http' ? 'rpm' : 'msg/min'}` },
            { key: 'rpm-t', header: '', render: (s) => <Sparkline data={serviceTrafficSeries(s.id, range).slice(-24)} width={80} height={24} /> },
            { key: 'err', header: 'Error rate', numeric: true, render: (s) => <ErrorCell rate={s.errorRate} /> },
            { key: 'err-t', header: '', render: (s) => <Sparkline data={serviceErrorSeries(s.id, range).slice(-24)} width={80} height={24} /> },
            { key: 'p95', header: 'p95', numeric: true, render: (s) => ms(s.p95) },
          ]} />
        </Card>
      </Section>

      <div className="grid grid-3">
        <Card className="span-2" flush title="Endpoints needing attention" subtitle="Failing or slow, worst first · click to open">
          <Table rows={attention} rowKey={(e) => e.serviceId + e.id} onRowClick={(e) => nav(`/services/${e.serviceId}/endpoints/${e.id}`)} columns={[
            { key: 'ep', header: 'Endpoint', render: (e) => <div style={{ minWidth: 0 }}><div className="truncate" style={{ maxWidth: 280 }}><span className="mono muted">{e.method}</span> {e.route}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{e.service}</div></div> },
            { key: 'why', header: 'Why', render: (e) => <Badge>{e.reason === 'errors' ? 'Failing' : 'Slow'}</Badge> },
            { key: 'rpm', header: 'rpm', numeric: true, render: (e) => compact(e.rpm) },
            { key: 'err', header: 'Error rate', numeric: true, render: (e) => <ErrorCell rate={e.errorRate} /> },
            { key: 'p95', header: 'p95', numeric: true, render: (e) => ms(e.p95) },
            { key: 'mon', header: 'Monitor', render: (e) => e.monitored ? <Badge tone="good">Active</Badge> : <span className="muted">None</span> },
          ]} />
        </Card>
        <div className="stack">
          <Card title={<span className="row"><Sparkles size={14} color="var(--accent)" aria-hidden />Alert coverage</span>} subtitle="Critical code paths with a monitor">
            {critical.length ? <>
              <div className="row" style={{ alignItems: 'baseline', marginBottom: 8 }}>
                <span className="stat__value">{Math.round(coverage * 100)}%</span>
                <span className="muted">{covered} of {critical.length}</span>
              </div>
              <Meter value={coverage} tone={coverage < 0.6 ? 'warning' : undefined} />
              <Button size="sm" variant="primary" style={{ marginTop: 12 }} onClick={() => nav('/code')}>
                Review {suggestions.length} suggestions
              </Button>
            </> : <>
              <p className="muted">Connect a repository to see which critical code paths have no monitor.</p>
              <Button size="sm" variant="primary" style={{ marginTop: 12 }} onClick={() => nav('/code')}>Open code insights</Button>
            </>}
          </Card>
        </div>
      </div>

      {inc.item && <IncidentDrawer incident={inc.item} onClose={inc.close} />}
    </>
  );
}

function IncidentDrawer({ incident, onClose }: { incident: Incident; onClose: () => void }) {
  const nav = useNavigate();
  const { range } = useApp();
  const ts = timestamps(range);
  const monitor = monitors.find((m) => m.id === incident.monitorId);
  const svc = services.find((s) => s.name === incident.service);
  const isLag = monitor?.kind === 'consumer_lag';
  const isLatency = monitor?.kind === 'latency';
  const kpi = incident.kpiId ? kpis.find((k) => k.id === incident.kpiId) : undefined;
  const cause = incidentCause(incident);
  const data = kpi ? kpiSeries(kpi, range) : svc && isLag
    ? (isApi && monitor?.queue ? ruleSeries({ ...monitor.queue, metric: 'lag' }, range) : series(`lag-${svc.id}`, range, { base: (svc.lag ?? 1000) * 0.4, spikeAt: 0.95, spikeMul: 2.5, spikeWidth: 4 }))
    : svc && isLatency
      ? (isApi ? redSeries(range, svc.id).p95 : series(`p95-${svc.id}`, range, { base: svc.p95, spikeAt: 0.8, spikeMul: 2.6, spikeWidth: 6 }))
      : svc ? serviceErrorSeries(svc.id, range) : [];
  const fmt = kpi ? (v: number) => pct(v, 1) : isLag ? (v: number) => compact(v) : isLatency ? ms : (v: number) => pct(v);
  const threshold = kpi ? { value: kpi.objective, label: `objective ${pct(kpi.objective, 1)}` } : isLag ? { value: 10_000, label: '10K' } : isLatency ? { value: 600, label: '600 ms' } : { value: 0.02, label: '2%' };

  return (
    <Drawer open onClose={onClose} title={incident.title}
      subtitle={<><StatusBadge status={incident.state === 'acknowledged' ? 'acknowledged' : incident.state === 'pending' ? 'pending' : 'firing'} /><span className="muted">started {ago(incident.startedAt)}</span></>}
      actions={kpi ? <Button size="sm" onClick={() => nav(`/kpis/${kpi.id}`)}>Open KPI <ArrowRight size={14} /></Button> : svc && <Button size="sm" onClick={() => nav(`/services/${svc.id}`)}>Open {svc.name} <ArrowRight size={14} /></Button>}>
      {cause && (
        <section className="card rca" style={{ padding: 'var(--space-4)' }}>
          <div className="stack" style={{ gap: 8 }}>
            <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>Why</span>
            <span className="rca__summary">{cause.summary}</span>
            <PathCrumbs result={cause} />
          </div>
        </section>
      )}
      <KeyValues items={[
        ['Current value', <strong key="v">{incident.value}</strong>],
        ['Condition', monitor?.condition ?? '—'],
        ['Target', monitor?.target ?? incident.service],
        ['Severity', <StatusBadge key="s" status={incident.severity === 'critical' ? 'critical' : 'warning'} label={incident.severity} />],
        ['Notified', monitor?.channels.join(', ') ?? '—'],
        ['Monitor source', monitor?.source === 'code' ? 'Suggested from code' : monitor?.source ?? '—'],
      ]} />
      {data.length > 0 && (
        <DrawerSection title={kpi ? 'Good events' : isLag ? 'Consumer lag' : isLatency ? 'p95 latency' : 'Error rate'}>
          <TimeSeries timestamps={ts} height={180} area format={fmt} threshold={threshold}
            series={[{ id: 'v', label: monitor?.name ?? 'Value', slot: isLatency || isLag ? 1 : 8, data }]} />
        </DrawerSection>
      )}
      <div className="row">
        <Button variant="primary" size="sm" onClick={() => nav(`/services/${svc?.id ?? ''}?tab=alerts`)}>Service alerts</Button>
        <Button size="sm" onClick={() => nav(`/services/${svc?.id ?? ''}?tab=logs`)}>Related logs</Button>
      </div>
    </Drawer>
  );
}
