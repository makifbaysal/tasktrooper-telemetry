import { Cpu, Globe, Inbox, Server, Smartphone, Workflow } from 'lucide-react';
import { Badge, StatusBadge } from '@/components/ui';
import { CALLERS, services, type Criticality, type ServiceType } from '@/data';

export const TYPE_META: Record<ServiceType, { label: string; Icon: typeof Server }> = {
  http: { label: 'HTTP service', Icon: Server },
  worker: { label: 'Worker', Icon: Cpu },
  consumer: { label: 'Consumer', Icon: Inbox },
  web: { label: 'Web app', Icon: Globe },
  mobile: { label: 'Mobile app', Icon: Smartphone },
};

/** Service categories — one sidebar entry each, `/services?type=<id>`. */
export const SERVICE_CATEGORIES: { id: string; label: string; types: ServiceType[]; Icon: typeof Server }[] = [
  { id: 'api', label: 'APIs', types: ['http'], Icon: Server },
  { id: 'worker', label: 'Workers', types: ['worker'], Icon: Cpu },
  { id: 'consumer', label: 'Consumers', types: ['consumer'], Icon: Inbox },
  { id: 'web', label: 'Web apps', types: ['web'], Icon: Globe },
  { id: 'mobile', label: 'Mobile apps', types: ['mobile'], Icon: Smartphone },
];
export const categoryOf = (t: ServiceType) => SERVICE_CATEGORIES.find((c) => c.types.includes(t))!;

export function ServiceType({ type }: { type: ServiceType }) {
  const m = TYPE_META[type] ?? { label: type, Icon: Workflow };
  return <span className="row secondary" style={{ gap: 6 }}><m.Icon size={14} aria-hidden />{m.label}</span>;
}

/** Fixed categorical slot per service — color follows the entity, not its rank. */
export const serviceSlot = (id: string) => LINE_SLOTS[Math.max(0, services.findIndex((s) => s.id === id)) % LINE_SLOTS.length];

export function CriticalityBadge({ c }: { c: Criticality }) {
  if (c === 'critical') return <StatusBadge status="critical" label="Critical path" />;
  if (c === 'important') return <Badge tone="accent">Important</Badge>;
  return <Badge>Low</Badge>;
}

/** Error-rate cell: status icon only when breaching. */
export function errorTone(rate: number) {
  return rate >= 0.02 ? 'critical' : rate >= 0.005 ? 'warning' : null;
}

/**
 * Slot order for multi-line charts: skips yellow (4) because orange/yellow
 * lines are hard to tell apart, especially in dark mode.
 */
export const LINE_SLOTS = [1, 2, 3, 7, 5, 6, 8];

export const DEP_KIND_LABEL: Record<'db' | 'cache' | 'http' | 'queue', string> = {
  db: 'Database', cache: 'Cache', http: 'HTTP', queue: 'Queue',
};

export const CALLER_KIND_LABEL: Record<import('@/data').CallerKind, string> = {
  service: 'Service', web: 'Web app', mobile: 'Mobile app', partner: 'Partner API', webhook: 'Webhook', probe: 'Health probe', unknown: 'Unknown',
  topic: 'Kafka topic', queue: 'Queue', schedule: 'Schedule',
};

/**
 * Caller colors: callers present on this page, in registry order → LINE_SLOTS.
 * Same caller keeps its color on every chart of the page, regardless of rank.
 */
export function callerColorFor(ids: string[]) {
  // registry callers first (stable colors), then data sources (topics/queues) in page order
  const ordered = [...CALLERS.map((c) => c.id).filter((id) => ids.includes(id)), ...ids.filter((id) => !CALLERS.some((c) => c.id === id))];
  return (id: string) => `var(--series-${LINE_SLOTS[Math.max(0, ordered.indexOf(id)) % LINE_SLOTS.length]})`;
}
