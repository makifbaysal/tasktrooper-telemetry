import { useNavigate } from 'react-router';
import { ChevronRight } from 'lucide-react';
import { Card, KeyValues, PageHeader, Section, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { k8sClusters, k8sEvents, k8sPods, noAgentData, useData, type K8sCluster } from '@/data';
import { int } from '@/lib/format';
import { AgentSetup, EventList, INFRA_RANK, InfraStatusBadge, PodPhaseBadge, ServiceLink, UsageCell, bytes, clusterUrl, cores, lastSeenText, podUrl } from './k8sShared';

function ClusterCard({ c }: { c: K8sCluster }) {
  const nav = useNavigate();
  const cpu = c.cpuAllocatable > 0 ? c.cpuUsed / c.cpuAllocatable : 0;
  const mem = c.memoryAllocatable > 0 ? c.memoryUsed / c.memoryAllocatable : 0;
  const rail = c.status === 'critical' ? 'card--rail-critical' : c.status === 'warning' ? 'card--rail-warning' : undefined;
  return (
    <Card onClick={() => nav(clusterUrl(c.name))} className={rail}
      title={<span className="row" style={{ gap: 'var(--space-2)' }}>{c.name}<InfraStatusBadge status={c.status} /></span>}
      subtitle={`${c.namespaces} namespaces · ${c.workloads} workloads · ${lastSeenText(c.lastSeen)}`}
      actions={<ChevronRight size={16} className="muted" aria-hidden />}>
      <KeyValues items={[
        ['Nodes ready', <span key="n" className={c.nodesReady < c.nodes ? 'delta--bad num' : 'num'}>{c.nodesReady} / {c.nodes}</span>],
        ['Pods running', <span key="p" className="num">{c.podsRunning} / {c.pods}</span>],
        ['Pods failing', <span key="f" className={c.podsFailing > 0 ? 'delta--bad num' : 'num'}>{c.podsFailing}</span>],
        ['Warning events', <span key="w" className="num">{int(c.warningEvents)}</span>],
        ['CPU', <UsageCell key="c" value={cpu} label={`${cores(c.cpuUsed)} / ${cores(c.cpuAllocatable)} cores`} />],
        ['Memory', <UsageCell key="m" value={mem} label={`${bytes(c.memoryUsed, 0)} / ${bytes(c.memoryAllocatable, 0)}`} />],
      ]} />
    </Card>
  );
}

export function Kubernetes() {
  useData();
  const nav = useNavigate();
  const { range } = useApp();
  if (noAgentData() || (k8sClusters.length === 0)) {
    return (
      <>
        <PageHeader title="Kubernetes" subtitle="Clusters, nodes, workloads and pods reporting through trooper-agent" />
        <AgentSetup focus="k8s" />
      </>
    );
  }
  const clusters = [...k8sClusters].sort((a, b) => INFRA_RANK[b.status] - INFRA_RANK[a.status] || a.name.localeCompare(b.name));
  const failing = k8sPods({}, range).filter((p) => p.status === 'critical');
  const events = k8sEvents({ limit: 8 });

  return (
    <>
      <PageHeader title="Kubernetes"
        subtitle={`${clusters.length} cluster${clusters.length === 1 ? '' : 's'} · ${clusters.reduce((a, c) => a + c.nodes, 0)} nodes · ${clusters.reduce((a, c) => a + c.pods, 0)} pods`} />

      {failing.length > 0 && (
        <Section title="Failing pods" subtitle={`${failing.length} across all clusters`}>
          <Card flush>
            <Table rows={failing} rowKey={(p) => p.uid} onRowClick={(p) => nav(podUrl(p))} columns={[
              { key: 'pod', header: 'Pod', render: (p) => (
                <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{p.name}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{p.cluster} · {p.namespace}</div></div>) },
              { key: 'phase', header: 'State', render: (p) => <PodPhaseBadge pod={p} /> },
              { key: 'rst', header: 'Restarts in range', numeric: true, render: (p) => <span className={p.restartsInRange > 0 ? 'delta--bad' : undefined}>{p.restartsInRange}</span> },
              { key: 'svc', header: 'Service', render: (p) => <ServiceLink id={p.service} /> },
              { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
            ]} />
          </Card>
        </Section>
      )}

      <Section title="Clusters">
        <div className="grid grid-2">{clusters.map((c) => <ClusterCard key={c.name} c={c} />)}</div>
      </Section>

      <Section title="Recent warning events" subtitle="all clusters">
        <Card><EventList events={events} /></Card>
      </Section>
    </>
  );
}
