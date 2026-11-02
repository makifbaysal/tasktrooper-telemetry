import { useNavigate } from 'react-router';
import { Activity, ArrowRight, MessagesSquare } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import {
  Badge, Button, Card, ConnectSdk, Drawer, DrawerSection, KeyValues, Meter, Section, StatusBadge, Table, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  DEPLOY_SOURCE_LABEL, deployMarkers, endpointsFor, incidents, inventory, kpiSeries, kpis, monitors, redSeries, serviceErrorSeries, serviceMap,
  services, timestamps, worst, type Endpoint, type Health, type Incident, type Service, type TimeRange,
} from '@/data';
import { ago, compact, ms, pct } from '@/lib/format';
import { ErrorCell } from './apm';
import { ServiceType } from './shared';
import { PathCrumbs, incidentCause } from './rca';
import { ruleSeries } from '@/lib/lagAlerts';

type Attention = Endpoint & { service: string; reason: 'errors' | 'slow'; score: number };

export function Overview() {
  const { range, env } = useApp();
  const nav = useNavigate();
  const inc = useDrawer<Incident>();
  const ts = timestamps(range);
  const backend = services.filter((s) => s.type === 'http' || s.type === 'consumer' || s.type === 'worker');

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

  const open = incidents.filter((i) => i.state !== 'resolved');
  const ordered = [...backend].sort((a, b) => ['critical', 'warning', 'ok'].indexOf(a.status) - ['critical', 'warning', 'ok'].indexOf(b.status) || b.rpm - a.rpm);

  return (
    <>
      <header className="overview-heading" aria-labelledby="overview-title">
        <div className="overview-heading__copy">
          <h1 id="overview-title">{env ? `${env[0].toUpperCase()}${env.slice(1)} overview` : 'Overview'}</h1>
          <p>{services.length ? `${backend.length} backend services monitored over the last ${range}. Start with the active incidents.` : 'Waiting for the first data from an SDK.'}</p>
        </div>
      </header>

      {services.length === 0 ? (
        <Card>
          <ConnectSdk icon={<Activity />} title="No telemetry yet">
            Connect an SDK to your first service or app. Throughput, error rate, latency, incidents and the endpoints that need
            attention show up here as soon as data arrives.
          </ConnectSdk>
        </Card>
      ) : <>

      <div className="overview-primary">
      {open.length > 0 && (
        <Card flush className="overview-incidents" title={`${open.length} open incident${open.length === 1 ? '' : 's'}`} subtitle="Newest first · click for the cause"
          actions={<Button size="sm" variant="ghost" onClick={() => nav('/incidents')}>View all <ArrowRight size={14} /></Button>}>
          <div className="list">
            {open.slice(0, 4).map((i) => (
              <div key={i.id} className="list__row list__row--clickable" onClick={() => inc.open(i)}>
                <StatusBadge status={i.state === 'acknowledged' ? 'acknowledged' : i.state === 'pending' ? 'pending' : 'firing'} />
                <div className="list__main"><span className="list__title">{i.title}</span><span className="list__meta">{i.service} · now {i.value}</span></div>
                <span className="muted">{ago(i.startedAt)}</span>
              </div>
            ))}
          </div>
        </Card>
      )}

        <HealthPanel range={range} backend={backend} />
      </div>
      <div className="overview-secondary">
        <RecentDeploys since={ts[0]} />
        <KpisAtRisk />
      </div>

      <Section title="Services" subtitle="unhealthy first · click for details"
        actions={<Button size="sm" variant="ghost" onClick={() => nav('/services')}>All services <ArrowRight size={14} /></Button>}>
        <Card flush>
          <Table rows={ordered} rowKey={(s) => s.id} onRowClick={(s) => nav(`/services/${s.id}`)} columns={[
            { key: 'name', header: 'Service', render: (s) => <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{s.name}</div><div style={{ fontSize: 'var(--text-xs)' }}><ServiceType type={s.type} /></div></div> },
            { key: 'status', header: 'Health', render: (s) => <StatusBadge status={s.status} /> },
            { key: 'rpm', header: 'Traffic', numeric: true, render: (s) => `${compact(s.rpm)} ${s.type === 'http' ? 'rpm' : 'msg/min'}` },
            { key: 'err', header: 'Error rate', numeric: true, render: (s) => <ErrorCell rate={s.errorRate} /> },
            { key: 'p95', header: 'p95', numeric: true, render: (s) => ms(s.p95) },
          ]} />
        </Card>
      </Section>

      <div className="overview-detail">
        <Card flush title="Endpoints needing attention" subtitle="Failing or slow, worst first · click to open">
          <Table rows={attention} rowKey={(e) => e.serviceId + e.id} onRowClick={(e) => nav(`/services/${e.serviceId}/endpoints/${e.id}`)} columns={[
            { key: 'ep', header: 'Endpoint', render: (e) => <div style={{ minWidth: 0 }}><div className="truncate" style={{ maxWidth: 280 }}><span className="mono muted">{e.method}</span> {e.route}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{e.service}</div></div> },
            { key: 'why', header: 'Why', render: (e) => <Badge>{e.reason === 'errors' ? 'Failing' : 'Slow'}</Badge> },
            { key: 'rpm', header: 'rpm', numeric: true, render: (e) => compact(e.rpm) },
            { key: 'err', header: 'Error rate', numeric: true, render: (e) => <ErrorCell rate={e.errorRate} /> },
            { key: 'p95', header: 'p95', numeric: true, render: (e) => ms(e.p95) },
            { key: 'mon', header: 'Monitor', render: (e) => e.monitored ? <Badge tone="good">Active</Badge> : <span className="muted">None</span> },
          ]} />
        </Card>
      </div>

      </>}

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
    ? (monitor?.queue ? ruleSeries({ ...monitor.queue, metric: 'lag' }, range) : [])
    : svc && isLatency
      ? redSeries(range, svc.id).p95
      : svc ? serviceErrorSeries(svc.id, range) : [];
  const fmt = kpi ? (v: number) => pct(v, 1) : isLag ? (v: number) => compact(v) : isLatency ? ms : (v: number) => pct(v);
  const threshold = kpi ? { value: kpi.objective, label: `objective ${pct(kpi.objective, 1)}` }
    : isLag && monitor?.queue?.metric === 'lag' ? { value: monitor.queue.threshold, label: compact(monitor.queue.threshold) } : undefined;

  return (
    <Drawer open onClose={onClose} title={incident.title}
      subtitle={<><StatusBadge status={incident.state === 'acknowledged' ? 'acknowledged' : incident.state === 'pending' ? 'pending' : 'firing'} /><span className="muted">started {ago(incident.startedAt)}</span></>}
      actions={<>
        <Button size="sm" variant="ghost" icon={<MessagesSquare size={14} />} onClick={() => nav(`/assistant?ref=incident:${encodeURIComponent(incident.id)}`)}>Ask assistant</Button>
        <Button size="sm" onClick={() => nav(`/incidents?id=${encodeURIComponent(incident.id)}`)}>Open incident <ArrowRight size={14} /></Button>
      </>}>
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

const STATUS_ORDER: Health[] = ['critical', 'warning', 'ok'];
const STATUS_LABEL: Record<Health, string> = { critical: 'Critical', warning: 'Warning', ok: 'Healthy' };

/** What needs a look right now, in counts that lead somewhere: services by health, failing dependencies, KPIs, coverage. */
function HealthPanel({ range, backend }: { range: TimeRange; backend: Service[] }) {
  const nav = useNavigate();
  const map = serviceMap(range);
  const counts = STATUS_ORDER.map((st) => ({ st, n: backend.filter((s) => s.status === st).length }));
  const deps = (map?.nodes ?? []).filter((n) => (n.tier === 'data' || n.tier === 'external') && n.status !== 'ok');
  const atRisk = kpis.filter((k) => k.status !== 'ok');
  const critical = inventory.filter((i) => i.criticality === 'critical');
  const covered = critical.filter((i) => i.monitored).length;
  const lines: { label: string; value: string; status?: Health; to: string }[] = [
    { label: 'Dependencies with errors', value: map ? String(deps.length) : '…', status: deps.length ? worst(deps.map((d) => d.status)) : undefined, to: '/service-map' },
    { label: 'KPIs at risk', value: `${atRisk.length} of ${kpis.length}`, status: atRisk.length ? worst(atRisk.map((k) => k.status)) : undefined, to: '/kpis' },
    { label: 'Critical code paths with a monitor', value: critical.length ? `${covered} of ${critical.length}` : 'No repository', to: '/code' },
  ];
  return (
    <Card className="overview-health" title="System health" subtitle={`${backend.length} backend services · last ${range}`}
      actions={<Button size="sm" variant="ghost" onClick={() => nav('/service-map')}>Service map <ArrowRight size={14} /></Button>}>
      <div className="overview-health__bar" role="img" aria-label={counts.map((c) => `${c.n} ${STATUS_LABEL[c.st].toLowerCase()}`).join(', ')}>
        {counts.filter((c) => c.n > 0).map((c) => <span key={c.st} className={`is-${c.st}`} style={{ flexGrow: c.n }} />)}
      </div>
      <div className="overview-health__counts">
        {counts.map((c) => (
          <button key={c.st} type="button" className="overview-health__count" onClick={() => nav('/services')}>
            <span className="overview-health__num">{c.n}</span>
            <StatusBadge status={c.st} label={STATUS_LABEL[c.st]} />
          </button>
        ))}
      </div>
      <ul className="overview-health__lines">
        {lines.map((l) => (
          <li key={l.label}>
            <button type="button" onClick={() => nav(l.to)}>
              <span>{l.label}</span>
              <span className="overview-health__val">{l.status && <StatusBadge status={l.status} label={STATUS_LABEL[l.status]} />}{l.value}</span>
            </button>
          </li>
        ))}
      </ul>
    </Card>
  );
}

/** Deploys in the range with the service's health now: the first thing to check after "what changed?". */
function RecentDeploys({ since }: { since: number }) {
  const nav = useNavigate();
  const list = deployMarkers.filter((d) => d.ts >= since && d.state !== 'canceled').sort((a, b) => b.ts - a.ts).slice(0, 5);
  return (
    <Card flush title="Recent deploys" subtitle="With each service's health now">
      {list.length === 0 ? <p className="muted overview-empty">No deploys in this range.</p> : (
        <div className="list">
          {list.map((d) => {
            const svc = services.find((s) => s.name === d.service || s.id === d.service);
            return (
              <div key={`${d.service}-${d.version}-${d.ts}`} className="list__row list__row--clickable" onClick={() => svc && nav(`/services/${svc.id}`)}>
                {d.state === 'error' ? <StatusBadge status="critical" label="Failed" /> : svc ? <StatusBadge status={svc.status} label={STATUS_LABEL[svc.status]} /> : <Badge>Deploy</Badge>}
                <div className="list__main">
                  <span className="list__title"><span className="mono">{d.service}</span> {d.version}</span>
                  <span className="list__meta truncate">{d.commitMessage ?? DEPLOY_SOURCE_LABEL[d.source]}</span>
                </div>
                <span className="muted">{ago(d.ts)}</span>
              </div>
            );
          })}
        </div>
      )}
    </Card>
  );
}

/** KPIs burning their error budget, worst first. */
function KpisAtRisk() {
  const nav = useNavigate();
  const list = kpis.filter((k) => k.status !== 'ok').sort((a, b) => a.budgetRemaining - b.budgetRemaining).slice(0, 5);
  return (
    <Card flush title="KPIs at risk" subtitle="Error budget left and burn rate"
      actions={<Button size="sm" variant="ghost" onClick={() => nav('/kpis')}>All KPIs <ArrowRight size={14} /></Button>}>
      {list.length === 0 ? <p className="muted overview-empty">{kpis.length ? `All ${kpis.length} KPIs are on track.` : 'No KPIs yet. Define one on a service.'}</p> : (
        <div className="list">
          {list.map((k) => (
            <div key={k.id} className="list__row list__row--clickable" onClick={() => nav(`/kpis/${k.id}`)}>
              <StatusBadge status={k.status} label={STATUS_LABEL[k.status]} />
              <div className="list__main">
                <span className="list__title">{k.name}</span>
                <span className="list__meta">{pct(k.budgetRemaining, 0)} budget left · burning {k.burnRate1h.toFixed(1)}× (1h)</span>
              </div>
              <Meter value={Math.max(0, k.budgetRemaining)} tone={k.status === 'critical' ? 'critical' : 'warning'} />
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}
