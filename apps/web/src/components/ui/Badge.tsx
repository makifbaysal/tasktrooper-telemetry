import type { ReactNode } from 'react';
import { AlertOctagon, AlertTriangle, CheckCircle2, CircleDashed, BellOff, Info } from 'lucide-react';

export type Tone = 'neutral' | 'accent' | 'good' | 'warning' | 'serious' | 'critical';

export function Badge({ tone = 'neutral', icon, children }: { tone?: Tone; icon?: ReactNode; children: ReactNode }) {
  return (
    <span className={`badge badge--${tone}`}>
      {icon}
      {children}
    </span>
  );
}

/** Status is never color-alone: icon + label always ship together. */
export type StatusKind = 'ok' | 'warning' | 'critical' | 'firing' | 'pending' | 'muted' | 'acknowledged' | 'resolved' | 'info';

const STATUS: Record<StatusKind, { tone: Tone; label: string; Icon: typeof Info }> = {
  ok: { tone: 'good', label: 'Healthy', Icon: CheckCircle2 },
  resolved: { tone: 'good', label: 'Resolved', Icon: CheckCircle2 },
  warning: { tone: 'warning', label: 'Warning', Icon: AlertTriangle },
  pending: { tone: 'warning', label: 'Pending', Icon: CircleDashed },
  acknowledged: { tone: 'serious', label: 'Acknowledged', Icon: AlertTriangle },
  critical: { tone: 'critical', label: 'Critical', Icon: AlertOctagon },
  firing: { tone: 'critical', label: 'Firing', Icon: AlertOctagon },
  muted: { tone: 'neutral', label: 'Muted', Icon: BellOff },
  info: { tone: 'accent', label: 'Info', Icon: Info },
};

export function StatusBadge({ status, label }: { status: StatusKind; label?: string }) {
  const s = STATUS[status];
  return <Badge tone={s.tone} icon={<s.Icon aria-hidden />}>{label ?? s.label}</Badge>;
}

export function MethodBadge({ method }: { method: string }) {
  return <span className="mono" style={{ color: 'var(--text-secondary)', fontWeight: 600, minWidth: 44, display: 'inline-block' }}>{method}</span>;
}
