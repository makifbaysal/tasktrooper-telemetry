import { Link, useNavigate } from 'react-router';
import { Server } from 'lucide-react';
import { Badge, Card, EmptyState, Section, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { serviceInfra, useData } from '@/data';
import { PodTable } from './ClusterDetail';
import { InfraStatusBadge, LimitCell, UsageCell, bytes, cores, hostUrl, podUrl } from './k8sShared';

/** Service page → Infrastructure tab: where the service runs (pods, hosts, containers, serverless functions). */
export function ServiceInfraTab({ serviceId }: { serviceId: string }) {
  useData();
  const { range } = useApp();
  const nav = useNavigate();
  const infra = serviceInfra(serviceId, range);
  // containers that are not a pod's container (Docker on a VM) — pods already cover the K8s ones
  const loose = infra.containers.filter((c) => !c.pod);
  const empty = infra.pods.length === 0 && infra.hosts.length === 0 && infra.containers.length === 0 && infra.functions.length === 0;

  if (empty) {
    return (
      <Card>
        <EmptyState icon={<Server />} title="No infrastructure linked to this service">
          Pods and containers are matched by the label <span className="mono">tasktrooper.io/service</span> (or <span className="mono">app.kubernetes.io/name</span>, <span className="mono">app</span>);
          run trooper-agent in <span className="mono">host</span> / <span className="mono">cluster</span> mode — see <Link to="/infrastructure/hosts">Hosts</Link> for the install command.
        </EmptyState>
      </Card>
    );
  }

  return (
    <div className="stack">
      {infra.pods.length > 0 && (
        <Section title="Pods" subtitle={`${infra.pods.length} pods · ${infra.pods.filter((p) => p.status === 'critical').length} failing`}>
          <PodTable rows={infra.pods} onOpen={(p) => nav(podUrl(p))} showCluster />
        </Section>
      )}

      {infra.hosts.length > 0 && (
        <Section title="Hosts" subtitle="where its pods and containers run">
          <Card flush>
            <Table rows={infra.hosts} rowKey={(x) => x.id} onRowClick={(x) => nav(hostUrl(x.id))} defaultSort={{ key: 'cpu', dir: 'desc' }} columns={[
              { key: 'id', header: 'Host', sortValue: (x) => x.id, render: (x) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{x.id}</span> },
              { key: 'cpu', header: 'CPU', sortValue: (x) => x.cpu, render: (x) => <UsageCell value={x.cpu} /> },
              { key: 'mem', header: 'Memory', sortValue: (x) => x.memory, render: (x) => <UsageCell value={x.memory} /> },
            ]} />
          </Card>
        </Section>
      )}

      {loose.length > 0 && (
        <Section title="Containers" subtitle="outside Kubernetes (Docker)">
          <Card flush>
            <Table rows={loose} rowKey={(c) => c.id} onRowClick={(c) => nav(hostUrl(c.host))} columns={[
              { key: 'name', header: 'Container', render: (c) => (
                <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{c.name}</div><div className="muted mono" style={{ fontSize: 'var(--text-xs)' }}>{c.image}</div></div>) },
              { key: 'host', header: 'Host', render: (c) => c.host },
              { key: 'cpu', header: 'CPU (cores)', render: (c) => <LimitCell used={c.cpu} limit={c.cpuLimit} format={cores} /> },
              { key: 'mem', header: 'Memory', render: (c) => <LimitCell used={c.memoryBytes} limit={c.memoryLimit} format={(n) => bytes(n, 0)} /> },
              { key: 'rst', header: 'Restarts', numeric: true, render: (c) => c.restarts },
              { key: 'st', header: 'Status', render: (c) => <InfraStatusBadge status={c.status} /> },
            ]} />
          </Card>
        </Section>
      )}

      {infra.functions.length > 0 && (
        <Section title="Serverless functions">
          <Card>
            <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
              {infra.functions.map((f) => (
                <Link key={f.id} to={`/infrastructure/serverless/${encodeURIComponent(f.id)}`}><Badge>{f.name} · {f.provider}</Badge></Link>
              ))}
            </div>
          </Card>
        </Section>
      )}
    </div>
  );
}
