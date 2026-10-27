import { useState } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import { Boxes, ChevronRight, X } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Badge, Button, Card, EmptyState, MetricCard, PageHeader, SearchInput, Section, Segmented, Select, Table, Tabs } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { k8sClusterDetails, k8sEvents, k8sPods, k8sWorkloads, timestamps, useData, type K8sPod, type K8sWorkload } from '@/data';
import { int } from '@/lib/format';
import { LINE_SLOTS } from './shared';
import {
  EventList, INFRA_RANK, InfraStatusBadge, LimitCell, PodPhaseBadge, ServiceLink, UsageCell, bytes, cores, hostUrl, lastSeenText, podUrl,
} from './k8sShared';

type Tab = 'pods' | 'workloads' | 'nodes' | 'namespaces' | 'events';
const TABS: Tab[] = ['pods', 'workloads', 'nodes', 'namespaces', 'events'];

/** used / requested / allocatable — one unit per chart, fixed slots. */
const capacitySeries = (s: Record<'used' | 'requested' | 'allocatable', number[]>) => [
  { id: 'used', label: 'Used', slot: LINE_SLOTS[0], data: s.used },
  { id: 'requested', label: 'Requested', slot: LINE_SLOTS[1], data: s.requested },
  { id: 'allocatable', label: 'Allocatable', slot: LINE_SLOTS[2], data: s.allocatable },
];

export function K8sClusterDetail() {
  useData();
  const { cluster = '' } = useParams();
  const { range } = useApp();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const rawTab = params.get('tab') as Tab | null;
  const tab: Tab = rawTab && TABS.includes(rawTab) ? rawTab : 'pods';
  const namespace = params.get('ns') ?? '';
  const workload = params.get('workload') ?? '';
  const node = params.get('node') ?? '';
  const [q, setQ] = useState('');
  const [only, setOnly] = useState<'all' | 'problems'>('all');
  const update = (patch: Record<string, string>) => {
    const next = new URLSearchParams(params);
    for (const [k, v] of Object.entries(patch)) if (v) next.set(k, v); else next.delete(k);
    if (next.get('tab') === 'pods') next.delete('tab');
    setParams(next, { replace: true });
  };

  const c = k8sClusterDetails(cluster, range);
  const pods = k8sPods({ cluster, namespace: namespace || undefined }, range);
  const workloads = k8sWorkloads({ cluster, namespace: namespace || undefined }, range);
  const events = k8sEvents({ cluster, namespace: namespace || undefined, limit: 200 });
  if (!c) return <EmptyState icon={<Boxes />} title="Cluster not found">No data from a cluster called “{cluster}”.</EmptyState>;
  const ts = timestamps(range);

  const match = (s: string) => !q || s.toLowerCase().includes(q.toLowerCase());
  const podRows = pods.filter((p) => (!workload || p.workload === workload) && (!node || p.node === node) && match(`${p.name} ${p.service} ${p.node}`)
    && (only === 'all' || p.status !== 'ok'));
  const workloadRows = workloads.filter((w) => match(`${w.name} ${w.service}`) && (only === 'all' || w.status !== 'ok'));
  const nodeRows = c.nodeList.filter((n) => match(n.name));
  const failing = pods.filter((p) => p.status === 'critical');
  const namespaces = c.namespaceList.map((n) => n.name);
  const lastCpu = c.cpu.requested[c.cpu.requested.length - 1] ?? 0;
  const lastMem = c.memory.requested[c.memory.requested.length - 1] ?? 0;

  return (
    <>
      <PageHeader title={c.name}
        crumbs={[{ label: 'Kubernetes', to: '/infrastructure/kubernetes' }, { label: c.name }]}
        subtitle={<span className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <InfraStatusBadge status={c.status} />
          <span className="secondary">{c.nodesReady}/{c.nodes} nodes ready · {c.podsRunning}/{c.pods} pods running · {c.namespaces} namespaces</span>
          <span className="muted">{lastSeenText(c.lastSeen)}</span>
        </span>} />

      {failing.length > 0 && (
        <Section title="Failing pods">
          <div className="grid grid-3">
            {failing.map((p) => (
              <Card key={p.uid} className="card--rail-critical" onClick={() => nav(podUrl(p))}>
                <div className="row" style={{ gap: 'var(--space-2)', marginBottom: 'var(--space-2)', flexWrap: 'wrap' }}>
                  <PodPhaseBadge pod={p} /><strong className="truncate">{p.name}</strong>
                </div>
                <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>
                  {p.namespace} · {p.workloadKind} {p.workload} · {p.restartsInRange} restarts in range · memory {bytes(p.memoryBytes, 0)}{p.memoryLimit > 0 && ` of ${bytes(p.memoryLimit, 0)} limit`}
                </div>
              </Card>
            ))}
          </div>
        </Section>
      )}

      <Section>
        <div className="grid grid-2">
          <MetricCard label="CPU · cores" value={cores(c.cpuUsed)} unit={`/ ${cores(c.cpuAllocatable)} allocatable`}
            hint={`${cores(lastCpu)} requested by pods`}>
            <TimeSeries timestamps={ts} height={170} format={(v) => cores(v)} series={capacitySeries(c.cpu)} />
          </MetricCard>
          <MetricCard label="Memory" value={bytes(c.memoryUsed)} unit={`/ ${bytes(c.memoryAllocatable, 0)} allocatable`}
            hint={`${bytes(lastMem)} requested by pods`}>
            <TimeSeries timestamps={ts} height={170} format={(v) => bytes(v, 0)} series={capacitySeries(c.memory)} />
          </MetricCard>
        </div>
      </Section>

      <Section>
        <Tabs value={tab} onChange={(t) => update({ tab: t })} tabs={[
          { id: 'pods', label: <>Pods <Badge tone={failing.length ? 'critical' : 'neutral'}>{failing.length ? `${failing.length} failing` : pods.length}</Badge></> },
          { id: 'workloads', label: <>Workloads <Badge>{workloads.length}</Badge></> },
          { id: 'nodes', label: <>Nodes <Badge tone={c.nodesReady < c.nodes ? 'critical' : 'neutral'}>{c.nodesReady}/{c.nodes}</Badge></> },
          { id: 'namespaces', label: <>Namespaces <Badge>{c.namespaceList.length}</Badge></> },
          { id: 'events', label: <>Events <Badge tone={events.length ? 'warning' : 'neutral'}>{events.length}</Badge></> },
        ]} />

        {tab !== 'namespaces' && (
          <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
            <Select aria-label="Namespace" value={namespace} onChange={(e) => update({ ns: e.target.value, workload: '' })}
              options={[{ value: '', label: 'All namespaces' }, ...namespaces.map((n) => ({ value: n, label: n }))]} />
            {(tab === 'pods' || tab === 'workloads') && (
              <Segmented label="Show" value={only} onChange={setOnly} options={[{ id: 'all', label: 'All' }, { id: 'problems', label: 'Problems only' }]} />
            )}
            {tab !== 'events' && <div style={{ width: 'calc(var(--space-12) * 5)' }}><SearchInput placeholder="Filter by name" value={q} onChange={(e) => setQ(e.target.value)} /></div>}
            {tab === 'pods' && workload && <Button size="sm" variant="ghost" icon={<X size={14} />} onClick={() => update({ workload: '' })}>workload: {workload}</Button>}
            {tab === 'pods' && node && <Button size="sm" variant="ghost" icon={<X size={14} />} onClick={() => update({ node: '' })}>node: {node}</Button>}
          </div>
        )}

        {tab === 'pods' && <PodTable rows={podRows} onOpen={(p) => nav(podUrl(p))} />}

        {tab === 'workloads' && (
          <Card flush>
            <Table<K8sWorkload> rows={workloadRows} rowKey={(w) => `${w.namespace}/${w.kind}/${w.name}`} defaultSort={{ key: 'status', dir: 'desc' }}
              onRowClick={(w) => update({ tab: 'pods', ns: w.namespace, workload: w.name })}
              empty={<p className="muted" style={{ padding: 'var(--space-4)' }}>No workload matches.</p>}
              columns={[
                { key: 'name', header: 'Workload', sortValue: (w) => w.name, render: (w) => (
                  <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{w.name}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{w.kind} · {w.namespace}</div></div>) },
                { key: 'status', header: 'Status', sortValue: (w) => INFRA_RANK[w.status], render: (w) => <InfraStatusBadge status={w.status} /> },
                { key: 'desired', header: 'Desired', numeric: true, sortValue: (w) => w.desired, render: (w) => w.desired },
                { key: 'ready', header: 'Ready', numeric: true, sortValue: (w) => w.ready, render: (w) => <span className={w.ready < w.desired ? 'delta--bad' : undefined}>{w.ready}</span> },
                { key: 'avail', header: 'Available', numeric: true, sortValue: (w) => w.available, render: (w) => <span className={w.available < w.desired ? 'delta--bad' : undefined}>{w.available}</span> },
                { key: 'upd', header: 'Up to date', numeric: true, sortValue: (w) => w.updated, render: (w) => w.updated },
                { key: 'rst', header: 'Restarts in range', numeric: true, sortValue: (w) => w.restarts, render: (w) => <span className={w.restarts > 0 ? 'delta--bad' : undefined}>{w.restarts}</span> },
                { key: 'cpu', header: 'CPU (cores)', numeric: true, sortValue: (w) => w.cpu, render: (w) => cores(w.cpu) },
                { key: 'mem', header: 'Memory', numeric: true, sortValue: (w) => w.memoryBytes, render: (w) => bytes(w.memoryBytes, 0) },
                { key: 'svc', header: 'Service', render: (w) => <ServiceLink id={w.service} /> },
                { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
              ]} />
          </Card>
        )}

        {tab === 'nodes' && (
          <Card flush>
            <Table rows={nodeRows} rowKey={(n) => n.name} defaultSort={{ key: 'cpu', dir: 'desc' }} onRowClick={(n) => nav(hostUrl(n.host || n.name))} columns={[
              { key: 'name', header: 'Node', sortValue: (n) => n.name, render: (n) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{n.name}</span> },
              { key: 'ready', header: 'Status', sortValue: (n) => INFRA_RANK[n.status ?? 'ok'], render: (n) => n.ready
                ? <InfraStatusBadge status={n.status ?? 'ok'} label={n.status === 'ok' || !n.status ? 'Ready' : undefined} />
                : <InfraStatusBadge status="critical" label="NotReady" /> },
              { key: 'cpu', header: 'CPU', sortValue: (n) => (n.cpuAllocatable ? n.cpu / n.cpuAllocatable : 0), render: (n) => (
                <UsageCell value={n.cpuAllocatable ? n.cpu / n.cpuAllocatable : 0} label={`${cores(n.cpu)} / ${cores(n.cpuAllocatable)}`} />) },
              { key: 'mem', header: 'Memory', sortValue: (n) => (n.memoryAllocatable ? n.memoryBytes / n.memoryAllocatable : 0), render: (n) => (
                <UsageCell value={n.memoryAllocatable ? n.memoryBytes / n.memoryAllocatable : 0} label={`${bytes(n.memoryBytes, 0)} / ${bytes(n.memoryAllocatable, 0)}`} />) },
              { key: 'pods', header: 'Pods', numeric: true, sortValue: (n) => n.pods, render: (n) => (
                <Button size="sm" variant="ghost" onClick={(e) => { e.stopPropagation(); update({ tab: 'pods', node: n.name }); }}>{n.pods}</Button>) },
              { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
            ]} />
          </Card>
        )}

        {tab === 'namespaces' && (
          <Card flush>
            <Table rows={c.namespaceList} rowKey={(n) => n.name} defaultSort={{ key: 'failing', dir: 'desc' }} onRowClick={(n) => update({ tab: 'pods', ns: n.name })} columns={[
              { key: 'name', header: 'Namespace', sortValue: (n) => n.name, render: (n) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{n.name}</span> },
              { key: 'wl', header: 'Workloads', numeric: true, sortValue: (n) => n.workloads, render: (n) => n.workloads },
              { key: 'pods', header: 'Pods', numeric: true, sortValue: (n) => n.pods, render: (n) => n.pods },
              { key: 'failing', header: 'Failing pods', numeric: true, sortValue: (n) => n.failing, render: (n) => <span className={n.failing > 0 ? 'delta--bad' : undefined}>{int(n.failing)}</span> },
              { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
            ]} />
          </Card>
        )}

        {tab === 'events' && <Card><EventList events={events} /></Card>}
      </Section>
    </>
  );
}

/** Pod table (cluster page, service Infrastructure tab). */
export function PodTable({ rows, onOpen, showCluster }: { rows: K8sPod[]; onOpen: (p: K8sPod) => void; showCluster?: boolean }) {
  return (
    <Card flush>
      <Table rows={rows} rowKey={(p) => p.uid} onRowClick={onOpen} defaultSort={{ key: 'status', dir: 'desc' }}
        empty={<p className="muted" style={{ padding: 'var(--space-4)' }}>No pod matches.</p>}
        columns={[
          { key: 'name', header: 'Pod', sortValue: (p) => p.name, render: (p) => (
            <div style={{ minWidth: 0, maxWidth: 'calc(var(--space-12) * 7)' }}>
              <div className="truncate" style={{ fontWeight: 'var(--weight-medium)' }} title={p.name}>{p.name}</div>
              <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{showCluster && `${p.cluster} · `}{p.namespace} · {p.workloadKind} {p.workload} · {p.node ? <span title={p.node}>{p.node.split('.')[0]}</span> : 'unscheduled'}</div>
            </div>) },
          { key: 'status', header: 'State', sortValue: (p) => INFRA_RANK[p.status], render: (p) => <PodPhaseBadge pod={p} /> },
          { key: 'rst', header: 'Restarts', numeric: true, sortValue: (p) => p.restartsInRange * 1000 + p.restarts, render: (p) => (
            <span title="in range · total"><span className={p.restartsInRange > 0 ? 'delta--bad' : undefined}>{p.restartsInRange}</span> <span className="muted">· {p.restarts}</span></span>) },
          { key: 'cpu', header: 'CPU vs limit', sortValue: (p) => (p.cpuLimit ? p.cpu / p.cpuLimit : 0), render: (p) => <LimitCell used={p.cpu} limit={p.cpuLimit} format={cores} /> },
          { key: 'mem', header: 'Memory vs limit', sortValue: (p) => (p.memoryLimit ? p.memoryBytes / p.memoryLimit : 0), render: (p) => <LimitCell used={p.memoryBytes} limit={p.memoryLimit} format={(n) => bytes(n, 0)} /> },
          { key: 'svc', header: 'Service', render: (p) => <ServiceLink id={p.service} /> },
          { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
        ]} />
    </Card>
  );
}
