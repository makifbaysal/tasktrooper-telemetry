import { Link, useNavigate, useParams } from 'react-router';
import { Server } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Badge, Card, EmptyState, MetricCard, PageHeader, Section, StatTile, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { hostDetails, k8sPods, timestamps, useData, type HostProcess, type InfraContainer } from '@/data';
import { int, pct } from '@/lib/format';
import { InfraStatusBadge, LimitCell, ServiceLink, UsageCell, bytes, bytesRate, cores, lastSeenText, uptime } from './k8sShared';

export function HostDetail() {
  useData();
  const { hostId = '' } = useParams();
  const { range } = useApp();
  const nav = useNavigate();
  const host = hostDetails(hostId, range);
  // pods on a node: container → pod detail link
  const pods = host?.cluster ? k8sPods({ cluster: host.cluster, node: host.node }, range) : [];
  if (!host) return <EmptyState icon={<Server />} title="Host not found">It has not reported in the selected range, or the name is wrong.</EmptyState>;
  const ts = timestamps(range);
  const podUrl = (c: InfraContainer) => {
    const p = pods.find((x) => x.uid === c.podUid || x.name === c.pod);
    return p ? `/infrastructure/kubernetes/${encodeURIComponent(p.cluster)}/pods/${encodeURIComponent(p.uid)}` : undefined;
  };
  const containerOf = (id: string) => host.containerList.find((c) => c.id === id);

  return (
    <>
      <PageHeader title={host.id}
        crumbs={[{ label: 'Hosts', to: '/infrastructure/hosts' }, { label: host.id }]}
        subtitle={<span className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <InfraStatusBadge status={host.status} />
          <span className="secondary">{host.os} · {host.arch} · {host.cpuCores} cores · {bytes(host.memoryBytes, 0)}</span>
          <span className="muted">up {uptime(host.uptimeS)} · {lastSeenText(host.lastSeen)}</span>
          {host.cluster
            ? <Link to={`/infrastructure/kubernetes/${encodeURIComponent(host.cluster)}?tab=nodes`}>{host.cluster} · node</Link>
            : <Badge>outside Kubernetes</Badge>}
        </span>} />

      <Section>
        <div className="grid grid-3">
          <MetricCard label="CPU" value={pct(host.cpu, 0)} hint={`${cores(host.cpu * host.cpuCores)} of ${host.cpuCores} cores · load1 ${host.load1.toFixed(2)}`}>
            <TimeSeries timestamps={ts} height={150} yMax={1} format={(v) => pct(v, 0)} threshold={{ value: 0.9, label: '90%' }}
              series={[{ id: 'cpu', label: 'CPU', slot: 1, data: host.cpuSeries }]} />
          </MetricCard>
          <MetricCard label="Memory" value={pct(host.memory, 0)} hint={`${bytes(host.memory * host.memoryBytes)} of ${bytes(host.memoryBytes, 0)}`}>
            <TimeSeries timestamps={ts} height={150} yMax={1} format={(v) => pct(v, 0)}
              series={[{ id: 'memory', label: 'Memory', slot: 2, data: host.memorySeries }]} />
          </MetricCard>
          <MetricCard label="Disk · fullest filesystem" value={pct(host.disk, 0)}>
            <TimeSeries timestamps={ts} height={150} yMax={1} format={(v) => pct(v, 0)} threshold={{ value: 0.9, label: '90%' }}
              series={[{ id: 'disk', label: 'Disk', slot: 3, data: host.diskSeries }]} />
          </MetricCard>
        </div>
        <div className="grid grid-3">
          <MetricCard className="span-2" label="Network" value={bytesRate(host.netIn + host.netOut)} hint={`in ${bytesRate(host.netIn)} · out ${bytesRate(host.netOut)}`}>
            <TimeSeries timestamps={ts} height={150} format={bytesRate}
              series={[{ id: 'in', label: 'In', slot: 1, data: host.netInSeries }, { id: 'out', label: 'Out', slot: 2, data: host.netOutSeries }]} />
          </MetricCard>
          <StatTile label="Containers" value={int(host.containerList.length || host.containers)} unit={host.cluster ? 'via kubelet' : 'via Docker'} />
        </div>
      </Section>

      {host.gpus.length > 0 && (
        <Section title="GPUs" subtitle={`${host.gpus.length} × ${[...new Set(host.gpus.map((g) => g.model))].join(', ')} · nvidia-smi, last minute`}>
          <div className="grid grid-3">
            {host.gpus.map((g) => (
              <Card key={g.index}>
                <div className="row" style={{ justifyContent: 'space-between', gap: 'var(--space-2)', marginBottom: 'var(--space-3)', minWidth: 0 }}>
                  <strong>GPU {g.index}</strong>
                  <span className="muted truncate" style={{ fontSize: 'var(--text-xs)' }} title={g.uuid}>{g.model}</span>
                </div>
                <div className="stack" style={{ gap: 'var(--space-3)' }}>
                  <div>
                    <div className="muted" style={{ fontSize: 'var(--text-xs)', marginBottom: 'var(--space-1)' }}>Utilization</div>
                    <UsageCell value={g.utilization} />
                  </div>
                  <div>
                    <div className="muted" style={{ fontSize: 'var(--text-xs)', marginBottom: 'var(--space-1)' }}>Memory</div>
                    <UsageCell value={g.memory} label={g.memory > 0 ? `${bytes(g.memoryBytes, 0)} / ${bytes(g.memoryBytes / g.memory, 0)}` : bytes(g.memoryBytes, 0)} />
                  </div>
                  <div className="row secondary num" style={{ gap: 'var(--space-4)', fontSize: 'var(--text-sm)' }}>
                    <span>{Math.round(g.temperatureC)} °C</span>
                    <span>{Math.round(g.powerW)} W</span>
                  </div>
                </div>
              </Card>
            ))}
          </div>
          <MetricCard label="GPU utilization" value={pct(host.gpus.reduce((a, g) => a + g.utilization, 0) / host.gpus.length, 0)} hint="average of all GPUs, now">
            <TimeSeries timestamps={ts} height={170} yMax={1} format={(v) => pct(v, 0)}
              series={host.gpuSeries.map((s, i) => ({ id: s.id, label: s.label, slot: i + 1, data: s.data }))} />
          </MetricCard>
        </Section>
      )}

      <Section title="Services on this host" subtitle="from container labels (tasktrooper.io/service, app.kubernetes.io/name, app)">
        <Card>
          {host.services.length === 0
            ? <p className="muted" style={{ margin: 0 }}>No service label on any container here.</p>
            : <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>{host.services.map((s) => <Badge key={s}><ServiceLink id={s} /></Badge>)}</div>}
        </Card>
      </Section>

      <Section title="Containers" subtitle={`${host.containerList.length} running`}>
        <Card flush>
          <Table rows={host.containerList} rowKey={(c) => c.id} defaultSort={{ key: 'cpu', dir: 'desc' }}
            onRowClick={(c) => { const u = podUrl(c); if (u) nav(u); }}
            empty={<p className="muted" style={{ padding: 'var(--space-4)' }}>No containers reported. Without access to the Docker socket the agent only reports the host.</p>}
            columns={[
              { key: 'name', header: 'Container', sortValue: (c) => c.name, render: (c) => (
                <div style={{ minWidth: 0, maxWidth: 'calc(var(--space-12) * 6)' }}>
                  <div style={{ fontWeight: 'var(--weight-medium)' }}>{c.name}</div>
                  <div className="muted mono truncate" style={{ fontSize: 'var(--text-xs)' }} title={c.image}>{c.image}</div>
                </div>) },
              { key: 'pod', header: 'Pod', render: (c) => c.pod
                ? <span style={{ fontSize: 'var(--text-sm)' }}><span className="muted">{c.namespace}/</span>{c.pod}</span>
                : <span className="muted">—</span> },
              { key: 'svc', header: 'Service', render: (c) => <ServiceLink id={c.service} /> },
              { key: 'cpu', header: 'CPU (cores)', sortValue: (c) => c.cpu, render: (c) => <LimitCell used={c.cpu} limit={c.cpuLimit} format={cores} /> },
              { key: 'mem', header: 'Memory', sortValue: (c) => c.memoryBytes, render: (c) => <LimitCell used={c.memoryBytes} limit={c.memoryLimit} format={(n) => bytes(n, 0)} /> },
              { key: 'thr', header: 'Throttled', numeric: true, sortValue: (c) => c.throttled, render: (c) => (c.throttled > 0 ? pct(c.throttled, 0) : <span className="muted">—</span>) },
              { key: 'rst', header: 'Restarts', numeric: true, sortValue: (c) => c.restarts, render: (c) => <span className={c.restarts > 3 ? 'delta--bad' : undefined}>{c.restarts}</span> },
              { key: 'st', header: 'Status', render: (c) => <InfraStatusBadge status={c.status} /> },
            ]} />
        </Card>
      </Section>

      <Section title="Top processes" subtitle="last minute, by CPU · only the executable is collected, never arguments">
        <Card flush>
          <Table<HostProcess> rows={host.processes} rowKey={(p) => `${p.pid}`} defaultSort={{ key: 'cpu', dir: 'desc' }}
            empty={<p className="muted" style={{ padding: 'var(--space-4)' }}>No process data (the agent needs the host's /proc mounted).</p>}
            columns={[
              { key: 'pid', header: 'PID', numeric: true, sortValue: (p) => p.pid, render: (p) => <span className="mono">{p.pid}</span> },
              { key: 'exe', header: 'Executable', sortValue: (p) => p.exe, render: (p) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{p.exe}</span> },
              { key: 'cmd', header: 'Command', render: (p) => <span className="mono muted truncate" style={{ display: 'block', maxWidth: 'calc(var(--space-12) * 6)', fontSize: 'var(--text-xs)' }} title={p.command}>{p.command}</span> },
              { key: 'ctr', header: 'Container', render: (p) => { const c = p.containerId ? containerOf(p.containerId) : undefined; return c ? <span style={{ fontSize: 'var(--text-sm)' }}>{c.name}</span> : <span className="muted">host</span>; } },
              { key: 'cpu', header: 'CPU (cores)', numeric: true, sortValue: (p) => p.cpu, render: (p) => cores(p.cpu) },
              { key: 'mem', header: 'Memory', numeric: true, sortValue: (p) => p.memoryBytes, render: (p) => bytes(p.memoryBytes) },
              { key: 'thr', header: 'Threads', numeric: true, sortValue: (p) => p.threads, render: (p) => p.threads },
              { key: 'fds', header: 'FDs', numeric: true, sortValue: (p) => p.fds, render: (p) => p.fds },
            ]} />
        </Card>
      </Section>
    </>
  );
}
