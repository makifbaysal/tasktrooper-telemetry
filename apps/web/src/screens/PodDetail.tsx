import { useState } from 'react';
import { Link, useNavigate, useParams } from 'react-router';
import { Boxes, FileText, ListTree, Wrench } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Button, Card, EmptyState, KeyValues, MetricCard, PageHeader, Section, Segmented, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { endpointsFor, k8sPodDetails, podTraces, services, timestamps, useData, type K8sPodDetails } from '@/data';
import { int, pct } from '@/lib/format';
import { LINE_SLOTS } from './shared';
import { TraceTable } from './apm';
import { EventList, InfraStatusBadge, PodPhaseBadge, ServiceLink, bytes, clusterUrl, cores, hostUrl } from './k8sShared';

/** usage / request / limit — one unit per chart, fixed slots. */
const limitSeries = (s: K8sPodDetails['cpuSeries']) => [
  { id: 'usage', label: 'Usage', slot: LINE_SLOTS[0], data: s.usage },
  ...(s.request.some((v) => v > 0) ? [{ id: 'request', label: 'Request', slot: LINE_SLOTS[1], data: s.request }] : []),
  ...(s.limit.some((v) => v > 0) ? [{ id: 'limit', label: 'Limit', slot: LINE_SLOTS[2], data: s.limit }] : []),
];

/** A short reading of what is wrong with the pod and what usually fixes it. */
function diagnosis(p: K8sPodDetails): { title: string; fix: string } | undefined {
  const oom = p.reason === 'OOMKilled' || p.events.some((e) => /OOMKill/i.test(e.reason) || /OOMKilled/i.test(e.message));
  if (p.reason === 'CrashLoopBackOff' && oom) return {
    title: `Crash loop: the container is OOMKilled at its ${bytes(p.memoryLimit, 0)} memory limit`,
    fix: `Memory climbs to the limit and the kernel kills the process (${p.restartsInRange} restarts in range). Raise resources.limits.memory, or find the leak — compare this pod's memory chart with the deploy time.`,
  };
  if (oom) return { title: `OOMKilled at the ${bytes(p.memoryLimit, 0)} memory limit`, fix: 'Raise resources.limits.memory or lower the process\' memory use.' };
  if (p.reason === 'CrashLoopBackOff') return { title: 'Crash loop: the container keeps exiting', fix: 'Open the logs of the previous container run — the exit reason is usually in the last lines before the restart.' };
  if (p.reason === 'ImagePullBackOff' || p.reason === 'ErrImagePull') return { title: 'The image cannot be pulled', fix: 'Check the image tag and the registry credentials (imagePullSecrets).' };
  if (p.phase === 'Pending') {
    const sched = p.events.find((e) => e.reason === 'FailedScheduling');
    return { title: 'Pending: not scheduled on any node', fix: sched ? sched.message : 'No node has room for the pod\'s requests; lower the requests or add capacity.' };
  }
  if (p.phase === 'Running' && !p.ready) return { title: 'Running but not ready', fix: 'The readiness probe is failing; traffic is not routed to this pod.' };
  return undefined;
}

export function PodDetail() {
  useData();
  const { cluster = '', uid = '' } = useParams();
  const { range } = useApp();
  const nav = useNavigate();
  const [traceFilter, setTraceFilter] = useState<'all' | 'errors'>('all');
  const p = k8sPodDetails(uid, range);
  // hooks above; everything below may return early
  if (!p) return <EmptyState icon={<Boxes />} title="Pod not found">It may have been deleted, or it has not reported in the selected range.</EmptyState>;
  const ts = timestamps(range);
  const svc = services.find((s) => s.id === p.service || s.name === p.service);
  const d = diagnosis(p);
  const clusterName = p.cluster || cluster;
  const traces = podTraces({ name: p.name, namespace: p.namespace, service: p.service }, range, { errors: traceFilter === 'errors', limit: 20 });
  const eps = svc ? endpointsFor(svc.id) : [];
  const endpointUrl = (t: { method?: string; route?: string }) => {
    const ep = svc && eps.find((e) => !e.sub && e.method === t.method && e.route === t.route);
    return ep && svc ? `/services/${encodeURIComponent(svc.id)}/endpoints/${encodeURIComponent(ep.id)}` : undefined;
  };
  // server-side pod filter on the service's log tab (GET /logs?pod=&namespace=)
  const logsUrl = (extra: Record<string, string> = {}) => svc
    ? `/services/${encodeURIComponent(svc.id)}?${new URLSearchParams({ tab: 'logs', pod: p.name, ...(p.namespace ? { namespace: p.namespace } : {}), ...extra })}`
    : undefined;

  return (
    <>
      <PageHeader title={p.name}
        crumbs={[{ label: 'Kubernetes', to: '/infrastructure/kubernetes' }, { label: clusterName, to: clusterUrl(clusterName) }, { label: p.name }]}
        subtitle={<span className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <PodPhaseBadge pod={p} />
          <span className="secondary">{p.namespace} · {p.workloadKind} <Link to={`${clusterUrl(clusterName)}?ns=${encodeURIComponent(p.namespace)}&workload=${encodeURIComponent(p.workload)}`}>{p.workload}</Link></span>
          {p.node ? <Link to={hostUrl(p.node)} className="muted">node {p.node}</Link> : <span className="muted">not scheduled</span>}
        </span>}
        actions={svc && <>
          <Button icon={<FileText size={14} />} onClick={() => nav(logsUrl()!)}>Logs</Button>
          <Button icon={<ListTree size={14} />} onClick={() => document.getElementById('pod-traces')?.scrollIntoView({ behavior: 'smooth' })}>Traces</Button>
        </>} />

      {d && (
        <Card className={p.status === 'critical' ? 'card--rail-critical' : 'card--rail-warning'}
          title={<span className="row" style={{ gap: 'var(--space-2)' }}><InfraStatusBadge status={p.status} />{d.title}</span>}>
          <div className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-start' }}>
            <Wrench size={14} className="muted" style={{ marginTop: 'var(--space-1)', flexShrink: 0 }} aria-hidden />
            <span style={{ fontSize: 'var(--text-sm)' }}>{d.fix}</span>
          </div>
        </Card>
      )}

      <Section>
        <div className="grid grid-3">
          <MetricCard label="CPU · cores" value={cores(p.cpu)} unit={p.cpuLimit > 0 ? `/ ${cores(p.cpuLimit)} limit` : undefined}
            hint={`request ${cores(p.cpuRequest)}${p.cpuLimit > 0 ? ` · ${pct(p.cpu / p.cpuLimit, 0)} of limit` : ' · no limit'}`}>
            <TimeSeries timestamps={ts} height={150} format={(v) => cores(v)} series={limitSeries(p.cpuSeries)} />
          </MetricCard>
          <MetricCard label="Memory" value={bytes(p.memoryBytes)} unit={p.memoryLimit > 0 ? `/ ${bytes(p.memoryLimit, 0)} limit` : undefined}
            hint={`request ${bytes(p.memoryRequest, 0)}${p.memoryLimit > 0 ? ` · ${pct(p.memoryBytes / p.memoryLimit, 0)} of limit` : ' · no limit'}`}>
            <TimeSeries timestamps={ts} height={150} format={(v) => bytes(v, 0)} series={limitSeries(p.memorySeries)} />
          </MetricCard>
          <MetricCard label="Restarts" value={int(p.restartsInRange)} unit="in range" hint={`${int(p.restarts)} since the pod was created`}>
            <TimeSeries timestamps={ts} height={150} format={(v) => int(v)} yMax={Math.max(4, Math.ceil(Math.max(...p.restartSeries)))} series={[{ id: 'restarts', label: 'Restarts', slot: LINE_SLOTS[6], data: p.restartSeries }]} />
          </MetricCard>
        </div>
      </Section>

      <div id="pod-traces">
        <Section title="Traces" subtitle={`recent requests served by this pod${p.service ? ` · ${p.service}` : ''} (span resource k8s.pod.name)`}
          actions={<Segmented label="Traces" value={traceFilter} onChange={setTraceFilter} options={[{ id: 'all', label: 'All' }, { id: 'errors', label: 'Errors only' }]} />}>
          <Card flush>
            <TraceTable rows={traces} endpointUrl={endpointUrl}
              actions={svc ? (t) => <Link to={logsUrl({ traceId: t.id })!} onClick={(e) => e.stopPropagation()} style={{ fontSize: 'var(--text-sm)', whiteSpace: 'nowrap' }}>Logs</Link> : undefined}
              empty={<p className="muted" style={{ padding: 'var(--space-4)' }}>
                {traceFilter === 'errors' ? 'No failed requests on this pod in the selected range.'
                  : 'No traces from this pod. Set OTEL_RESOURCE_ATTRIBUTES=k8s.pod.name=$(POD_NAME),k8s.namespace.name=$(POD_NAMESPACE) on the container so spans carry the pod.'}
              </p>} />
          </Card>
        </Section>
      </div>

      <Section title="Containers">
        <Card flush>
          <Table rows={p.containers} rowKey={(c) => c.name} columns={[
            { key: 'name', header: 'Container', render: (c) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{c.name}</span> },
            { key: 'image', header: 'Image', render: (c) => <span className="mono muted" style={{ fontSize: 'var(--text-xs)', overflowWrap: 'anywhere' }}>{c.image}</span> },
            { key: 'cpu', header: 'CPU (cores)', numeric: true, render: (c) => cores(c.cpu) },
            { key: 'mem', header: 'Memory', numeric: true, render: (c) => bytes(c.memoryBytes) },
            { key: 'rst', header: 'Restarts', numeric: true, render: (c) => <span className={c.restarts > 3 ? 'delta--bad' : undefined}>{c.restarts}</span> },
          ]} />
        </Card>
      </Section>

      <div className="grid grid-3">
        <div className="span-2">
          <Section title="Events" subtitle="Kubernetes Warning events for this pod and its node">
            <Card><EventList events={p.events} empty="No warning events for this pod." /></Card>
          </Section>
        </div>
        <Section title="Details">
          <Card>
            <KeyValues items={[
              ['Service', <ServiceLink key="s" id={p.service} />],
              ['Phase', p.phase],
              ['Ready', p.ready ? 'yes' : 'no'],
              ['Node', p.node ? <Link key="n" to={hostUrl(p.node)}>{p.node}</Link> : 'not scheduled'],
              ['CPU request / limit', `${cores(p.cpuRequest)} / ${p.cpuLimit > 0 ? cores(p.cpuLimit) : '—'}`],
              ['Memory request / limit', `${bytes(p.memoryRequest, 0)} / ${p.memoryLimit > 0 ? bytes(p.memoryLimit, 0) : '—'}`],
              ['UID', <span key="u" className="mono" style={{ fontSize: 'var(--text-xs)', overflowWrap: 'anywhere' }}>{p.uid}</span>],
            ]} />
          </Card>
        </Section>
      </div>
    </>
  );
}
