/**
 * Screen-level pieces shared by Hosts, HostDetail, Kubernetes, ClusterDetail, PodDetail and the service
 * Infrastructure tab (ADR-019): status badges, usage meters, events list and the agent install empty state.
 */
import type { ReactNode } from 'react';
import { Link } from 'react-router';
import { AlertOctagon, AlertTriangle, CheckCircle2, CircleDashed, Clock, Radar, Server, XCircle } from 'lucide-react';
import { Badge, Card, CopyCode, EmptyState, Meter, StatusBadge, type Tone } from '@/components/ui';
import { POD_CRITICAL_REASONS, services, type InfraStatus, type K8sEvent, type K8sPod } from '@/data';
import { ago } from '@/lib/format';

// ---------------------------------------------------------------- formatting
export function bytes(n: number, digits = 1): string {
  if (!Number.isFinite(n) || n <= 0) return '0 B';
  const u = ['B', 'KiB', 'MiB', 'GiB', 'TiB'];
  const i = Math.min(u.length - 1, Math.floor(Math.log(n) / Math.log(1024)));
  const v = n / 1024 ** i;
  return `${v >= 100 || i === 0 ? Math.round(v) : v.toFixed(digits)} ${u[i]}`;
}
export const bytesRate = (n: number) => `${bytes(n)}/s`;
export function cores(n: number): string {
  if (!Number.isFinite(n) || n <= 0) return '0';
  return n < 1 ? `${Math.round(n * 1000)}m` : `${n.toFixed(n < 10 ? 2 : 1)}`;
}
export function uptime(s: number): string {
  if (s < 3600) return `${Math.round(s / 60)} min`;
  if (s < 86400) return `${Math.round(s / 3600)} h`;
  return `${Math.round(s / 86400)} d`;
}
export const meterTone = (v: number) => (v > 0.95 ? 'critical' : v > 0.85 ? 'warning' : undefined);

// ---------------------------------------------------------------- status
export const INFRA_RANK: Record<InfraStatus, number> = { ok: 0, stale: 1, warning: 2, critical: 3 };

/** Host / node / workload / cluster status. Stale = no data for > 5 min. Always icon + label. */
export function InfraStatusBadge({ status, label }: { status: InfraStatus; label?: string }) {
  if (status === 'stale') return <Badge tone="neutral" icon={<Clock aria-hidden />}>{label ?? 'Stale'}</Badge>;
  return <StatusBadge status={status} label={label} />;
}

/** Pod phase, or the container reason when there is one (CrashLoopBackOff, OOMKilled …). */
export function PodPhaseBadge({ pod }: { pod: Pick<K8sPod, 'phase' | 'reason' | 'ready'> }) {
  if (pod.reason) {
    const critical = POD_CRITICAL_REASONS.has(pod.reason);
    return <Badge tone={critical ? 'critical' : 'warning'} icon={critical ? <AlertOctagon aria-hidden /> : <AlertTriangle aria-hidden />}>{pod.reason}</Badge>;
  }
  const m: Record<string, { tone: Tone; Icon: typeof Clock; label?: string }> = {
    Running: pod.ready ? { tone: 'good', Icon: CheckCircle2 } : { tone: 'critical', Icon: AlertOctagon, label: 'Running · not ready' },
    Pending: { tone: 'warning', Icon: CircleDashed },
    Succeeded: { tone: 'neutral', Icon: CheckCircle2, label: 'Completed' },
    Failed: { tone: 'critical', Icon: XCircle },
  };
  const s = m[pod.phase] ?? { tone: 'neutral' as Tone, Icon: CircleDashed };
  return <Badge tone={s.tone} icon={<s.Icon aria-hidden />}>{s.label ?? pod.phase}</Badge>;
}

/** Service seen only through eBPF (OBI) spans — no SDK installed (ADR-019). */
export function EbpfBadge() {
  return <span title="Instrumented automatically with eBPF (no SDK): HTTP, gRPC and SQL spans only"><Badge tone="accent" icon={<Radar aria-hidden />}>eBPF</Badge></span>;
}

// ---------------------------------------------------------------- cells
/** 0..1 value as percent + meter. */
export function UsageCell({ value, label }: { value: number; label?: ReactNode }) {
  return (
    <div style={{ minWidth: 'calc(var(--space-12) * 2)' }}>
      <div className="row" style={{ justifyContent: 'space-between', gap: 'var(--space-2)', fontSize: 'var(--text-sm)' }}>
        <span className="num">{Math.round(value * 100)}%</span>{label && <span className="muted num" style={{ fontSize: 'var(--text-xs)', whiteSpace: 'nowrap' }}>{label}</span>}
      </div>
      <Meter value={value} tone={meterTone(value)} />
    </div>
  );
}

/** Used against a limit (cores or bytes). Without a limit only the value is shown. */
export function LimitCell({ used, limit, format }: { used: number; limit: number; format: (n: number) => string }) {
  if (!(limit > 0)) return <span className="num" style={{ fontSize: 'var(--text-sm)' }}>{format(used)} <span className="muted">· no limit</span></span>;
  return <UsageCell value={used / limit} label={`${format(used)} / ${format(limit)}`} />;
}

/** Service name → link when it is a known service, plain text otherwise. */
export function ServiceLink({ id }: { id: string }) {
  if (!id) return <span className="muted">—</span>;
  const svc = services.find((s) => s.id === id || s.name === id);
  return svc ? <Link to={`/services/${svc.id}`} onClick={(e) => e.stopPropagation()}>{svc.name}</Link> : <span className="secondary">{id}</span>;
}

// ---------------------------------------------------------------- events
/** Warning events, newest first (Kubernetes only reports Warning events through the agent). */
export function EventList({ events, empty = 'No warning events in this range.' }: { events: K8sEvent[]; empty?: string }) {
  if (events.length === 0) return <p className="muted" style={{ margin: 0 }}>{empty}</p>;
  return (
    <div className="list">
      {events.map((e, i) => (
        <div key={`${e.ts}-${e.objectName}-${e.reason}-${i}`} className="list__row" style={{ alignItems: 'flex-start' }}>
          <div className="list__main" style={{ minWidth: 0 }}>
            <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
              <Badge tone="warning" icon={<AlertTriangle aria-hidden />}>{e.reason}</Badge>
              <span className="mono" style={{ fontSize: 'var(--text-sm)' }}>{e.objectKind}/{e.objectName}</span>
              {e.namespace && <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{e.namespace}</span>}
              {e.count > 1 && <span className="muted num" style={{ fontSize: 'var(--text-xs)' }}>×{e.count}</span>}
            </div>
            <div className="secondary" style={{ fontSize: 'var(--text-sm)', marginTop: 'var(--space-1)', overflowWrap: 'anywhere' }}>{e.message}</div>
          </div>
          <span className="list__meta muted" style={{ whiteSpace: 'nowrap', fontSize: 'var(--text-xs)' }}>{ago(e.ts)}</span>
        </div>
      ))}
    </div>
  );
}

// ---------------------------------------------------------------- empty state
const HELM = `helm install trooper-agent deploy/helm/trooper-agent \\
  --namespace trooper --create-namespace \\
  --set clusterName=<cluster-name> \\
  --set dsn=<ingest-dsn>
# optional: auto-instrument services without an SDK (eBPF)
#   --set ebpf.enabled=true`;
const DOCKER = `docker run -d --name trooper-agent --restart unless-stopped \\
  --pid host --network host \\
  -v /proc:/host/proc:ro \\
  -v /var/run/docker.sock:/var/run/docker.sock:ro \\
  -e TT_AGENT_MODES=host \\
  -e TT_AGENT_PROCFS=/host/proc \\
  -e TT_DSN=<ingest-dsn> \\
  ghcr.io/tasktrooper/trooper-agent:latest`;

/** No host or cluster has reported yet: how to install trooper-agent. */
export function AgentSetup({ focus = 'both' }: { focus?: 'hosts' | 'k8s' | 'both' }) {
  return (
    <Card>
      <EmptyState icon={<Server />} title={focus === 'k8s' ? 'No Kubernetes cluster is reporting yet' : 'No host is reporting yet'}>
        Install trooper-agent: in Kubernetes it runs as a DaemonSet (hosts, containers, processes) plus one Deployment
        (cluster objects and Warning events); on a VM it runs as a single container.
      </EmptyState>
      <div className="stack" style={{ maxWidth: 'calc(var(--space-12) * 16)', margin: '0 auto' }}>
        {focus !== 'hosts' && <><div className="eyebrow">Kubernetes · Helm</div><CopyCode>{HELM}</CopyCode></>}
        {focus !== 'k8s' && <><div className="eyebrow">VM or bare-metal host · Docker</div><CopyCode>{DOCKER}</CopyCode></>}
        <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>
          eBPF is optional: <span className="mono">--set ebpf.enabled=true</span> adds an OpenTelemetry eBPF Instrumentation DaemonSet that traces
          HTTP, gRPC and SQL calls of services without an SDK — they show up under Services with an “eBPF” badge. The DSN is an ingest key
          from Settings &amp; SDK.
        </p>
      </div>
    </Card>
  );
}

export const podUrl = (p: Pick<K8sPod, 'cluster' | 'uid'>) => `/infrastructure/kubernetes/${encodeURIComponent(p.cluster)}/pods/${encodeURIComponent(p.uid)}`;
export const clusterUrl = (name: string) => `/infrastructure/kubernetes/${encodeURIComponent(name)}`;
export const hostUrl = (id: string) => `/infrastructure/hosts/${encodeURIComponent(id)}`;
export const lastSeenText = (ts: number) => `last data ${ago(ts)}`;
