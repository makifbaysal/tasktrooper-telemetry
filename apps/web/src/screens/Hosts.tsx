import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { ChevronRight } from 'lucide-react';
import { Sparkline } from '@/components/charts';
import { Card, PageHeader, SearchInput, Section, Select, Table } from '@/components/ui';
import { hosts, k8sClusters, noAgentData, useData, type Host } from '@/data';
import { AgentSetup, INFRA_RANK, InfraStatusBadge, UsageCell, bytes, bytesRate, lastSeenText } from './k8sShared';

const NO_CLUSTER = '__none';

/** Why a host needs attention, in one line. */
function hostProblem(x: Host): string {
  if (x.status === 'stale') return `No data for ${Math.round((Date.now() - x.lastSeen) / 60_000)} min — agent stopped or host down`;
  const worst = [['CPU', x.cpu], ['Memory', x.memory], ['Disk', x.disk]].sort((a, b) => (b[1] as number) - (a[1] as number))[0];
  return `${worst[0]} at ${Math.round((worst[1] as number) * 100)}%`;
}

export function Hosts() {
  useData();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const cluster = params.get('cluster') ?? '';
  const [q, setQ] = useState('');
  const setCluster = (c: string) => setParams(c ? { cluster: c } : {}, { replace: true });
  const open = (x: Host) => nav(`/infrastructure/hosts/${encodeURIComponent(x.id)}`);

  if (noAgentData()) {
    return (
      <>
        <PageHeader title="Hosts" subtitle="VMs and Kubernetes nodes reporting through trooper-agent" />
        <AgentSetup focus="hosts" />
      </>
    );
  }

  const clusterNames = [...new Set([...k8sClusters.map((c) => c.name), ...hosts.map((x) => x.cluster).filter(Boolean)])].sort();
  const rows = hosts.filter((x) => (!cluster || (cluster === NO_CLUSTER ? !x.cluster : x.cluster === cluster))
    && (!q || `${x.id} ${x.node} ${x.os}`.toLowerCase().includes(q.toLowerCase())));
  const bad = hosts.filter((x) => x.status !== 'ok').sort((a, b) => INFRA_RANK[b.status] - INFRA_RANK[a.status]);
  const outside = hosts.filter((x) => !x.cluster).length;

  return (
    <>
      <PageHeader title="Hosts"
        subtitle={`${hosts.length} hosts · ${hosts.length - outside} Kubernetes nodes · ${outside} outside Kubernetes`} />

      {bad.length > 0 && (
        <Section title="Needs attention">
          <div className="grid grid-3">
            {bad.map((x) => (
              <Card key={x.id} onClick={() => open(x)} className={x.status === 'critical' ? 'card--rail-critical' : x.status === 'warning' ? 'card--rail-warning' : undefined}>
                <div className="row" style={{ gap: 'var(--space-2)', marginBottom: 'var(--space-2)', minWidth: 0 }}>
                  <InfraStatusBadge status={x.status} /><strong className="truncate">{x.id}</strong>
                </div>
                <div style={{ fontWeight: 'var(--weight-medium)', marginBottom: 'var(--space-1)' }}>{hostProblem(x)}</div>
                <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>
                  {x.cluster ? `${x.cluster} node` : 'outside Kubernetes'} · {x.containers} containers · {lastSeenText(x.lastSeen)}
                </div>
              </Card>
            ))}
          </div>
        </Section>
      )}

      <Section title="All hosts" subtitle={`${rows.length} of ${hosts.length}`}
        actions={<>
          <Select aria-label="Cluster" value={cluster} onChange={(e) => setCluster(e.target.value)}
            options={[{ value: '', label: 'All clusters' }, ...clusterNames.map((c) => ({ value: c, label: c })), { value: NO_CLUSTER, label: 'Outside Kubernetes' }]} />
          <div style={{ width: 'calc(var(--space-12) * 5)' }}><SearchInput placeholder="Filter by host" value={q} onChange={(e) => setQ(e.target.value)} /></div>
        </>}>
        <Card flush>
          <Table rows={rows} rowKey={(x) => x.id} onRowClick={open} defaultSort={{ key: 'status', dir: 'desc' }}
            empty={<p className="muted" style={{ padding: 'var(--space-4)' }}>No host matches the filter.</p>}
            columns={[
              { key: 'name', header: 'Host', sortValue: (x) => x.id, render: (x) => (
                <div style={{ minWidth: 0, maxWidth: 'calc(var(--space-12) * 6)' }}>
                  <div className="truncate" style={{ fontWeight: 'var(--weight-medium)' }} title={x.id}>{x.id}</div>
                  <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{x.os} · {x.arch} · {x.cpuCores} cores · {bytes(x.memoryBytes, 0)}</div>
                </div>) },
              { key: 'status', header: 'Status', sortValue: (x) => INFRA_RANK[x.status], render: (x) => <InfraStatusBadge status={x.status} /> },
              { key: 'cpu', header: 'CPU', sortValue: (x) => x.cpu, render: (x) => (
                <div className="row" style={{ gap: 'var(--space-2)' }}><UsageCell value={x.cpu} /><Sparkline data={x.spark.cpu} width={56} height={22} /></div>) },
              { key: 'mem', header: 'Memory', sortValue: (x) => x.memory, render: (x) => <UsageCell value={x.memory} /> },
              { key: 'disk', header: 'Disk', sortValue: (x) => x.disk, render: (x) => <UsageCell value={x.disk} /> },
              { key: 'net', header: 'Net in / out', numeric: true, sortValue: (x) => x.netIn + x.netOut, render: (x) => (
                <span style={{ whiteSpace: 'nowrap', fontSize: 'var(--text-sm)' }}>{bytesRate(x.netIn)} <span className="muted">/</span> {bytesRate(x.netOut)}</span>) },
              { key: 'ctr', header: 'Containers', numeric: true, sortValue: (x) => x.containers, render: (x) => x.containers },
              { key: 'where', header: 'Cluster', sortValue: (x) => x.cluster, render: (x) => x.cluster
                ? <span style={{ fontSize: 'var(--text-sm)' }}>{x.cluster}</span>
                : <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>—</span> },
              { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
            ]} />
        </Card>
      </Section>
    </>
  );
}
