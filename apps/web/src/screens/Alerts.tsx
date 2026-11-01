import { useEffect, useState, type ReactNode } from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router';
import {
  Bell, BellOff, Bot, Check, CheckCircle2, ChevronRight, CircleDashed, Code2, ExternalLink, FileCode2, Lock, Pencil, Plus, Send, Sparkles, Target, Trash2, Wrench, X,
} from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CodeBlock, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, Section, Select, StatusBadge, Table, Tabs, useDrawer,
  type StatusKind,
} from '@/components/ui';
import {
  ApiError, kpis, incidents, monitors, mutate, qs, remote, seg, services,
  suggestions, timestamps, useData, type Incident, type Monitor, type RcaInfraCandidate, type RcaResult, type Suggestion, type TimeRange,
} from '@/data';
import {
  maintenanceAvailable, maintenanceWindows, monitorInput, resolvedIncidents, type ApiIncident, type ApiMonitor, type IncidentEvent, type MaintenanceWindow,
} from '@/data/api/ops';
import { can, denied } from '@/data/auth';
import { logsFor } from '@/data/logs';
import { LocalAICost } from '@/components/ui/LocalAICost';
import { LocalAIState } from '@/components/ui/LocalAIState';
import { targetLabel } from '@/data/slack';
import { WhyBlock, badLabel, badSeries, objectivePct } from './Kpis';
import { ago, clock, compact, dateTime, duration, ms, pct } from '@/lib/format';
import { CriticalityBadge } from './shared';
import { evaluate, formatRuleValue, metricMeta } from '@/lib/lagAlerts';
import { useInfra } from '@/lib/infra';

type Tab = 'monitors' | 'suggested' | 'history' | 'maintenance';

const KIND_LABEL: Record<Monitor['kind'], string> = {
  metric_threshold: 'Error rate', latency: 'Latency', throughput_change: 'Throughput change', anomaly: 'Anomaly',
  log_match: 'Log match', log_pattern_new: 'New log pattern', error_new: 'New error', consumer_lag: 'Consumer lag',
  job_missed: 'Missed job', rum_vital: 'Web vital', kpi_burn: 'KPI budget burn', composite: 'Composite',
};

// ---------- the metric behind each monitor ----------

interface MetricSpec {
  label: string;
  base: number;
  threshold: number;
  thresholdLabel: string;
  fmt: (v: number) => string;
}

/** KPI burn monitors chart the KPI's bad-event share; the line is the burn multiplier × allowed share. */
const kpiOfMonitor = (m: Monitor) => kpis.find((k) => k.id === (m as ApiMonitor).targetSpec?.kpiId || `KPI · ${k.name}` === m.target);
const slotOf = (m: Monitor) => (m.kind === 'metric_threshold' || m.kind === 'kpi_burn' ? 8 : 1);

function metricSeries(m: Monitor, range: TimeRange) {
  const k = m.kind === 'kpi_burn' ? kpiOfMonitor(m) : undefined;
  if (k) {
    const mult = Number(/> ?([\d.]+)×/.exec(m.condition)?.[1] ?? 1);
    const allowed = 1 - k.objective;
    const spec: MetricSpec = { label: badLabel(k), base: 0, threshold: mult * allowed, thresholdLabel: `${mult}× budget (${objectivePct(mult * allowed)})`, fmt: (v) => pct(v) };
    return { ts: timestamps(range), data: badSeries(k, range), spec };
  }
  if (m.queue) {
    const ev = evaluate(m.queue, range);
    const q = m.queue;
    const qspec: MetricSpec = { label: metricMeta(q.metric).label.replace(/ (above|within)$/, ''), base: 0, threshold: ev.th, thresholdLabel: formatRuleValue(q.metric, ev.th), fmt: (v) => formatRuleValue(q.metric, v) };
    return { ts: timestamps(range), data: ev.data, spec: qspec };
  }
  return apiMetricSeries(m as ApiMonitor, range);
}

// ---------- the series a saved monitor compares (GET /monitors/{id}/preview) ----------

interface Preview { timestamps: number[]; data: number[]; th: number; episodes: number; firingNow: boolean; now: number }
/** kinds the api can replay; others chart nothing rather than request an error */
const PREVIEWABLE = new Set(['metric_threshold', 'latency', 'error_rate', 'throughput']);

function apiMetricSeries(m: ApiMonitor, range: TimeRange) {
  const ts = timestamps(range);
  const kind = m.apiKind ?? m.kind;
  const metric = String(m.query?.metric ?? (kind === 'latency' ? m.query?.percentile ?? 'p95' : kind === 'throughput' ? 'rpm' : 'error_rate'));
  const ratio = metric === 'error_rate';
  const fmt = ratio ? (v: number) => pct(v) : metric.startsWith('p') ? ms : (v: number) => compact(v);
  const th = m.rule?.threshold ?? 0;
  const spec: MetricSpec = {
    label: ratio ? 'Error rate' : metric.startsWith('p') ? `${metric} latency` : metric === 'rpm' ? 'Requests / min' : KIND_LABEL[m.kind] ?? 'Value',
    base: 0, threshold: th, thresholdLabel: fmt(th), fmt,
  };
  if (!PREVIEWABLE.has(kind) || !m.targetSpec?.service) return { ts, data: ts.map(() => 0), spec };
  const p = remote<Preview | null>(`/monitors/${seg(m.id)}/preview${qs({ range })}`, null);
  const d = p?.data ?? [];
  const data = d.length >= ts.length ? d.slice(d.length - ts.length) : [...Array(ts.length - d.length).fill(0), ...d];
  return { ts, data, spec: { ...spec, threshold: p?.th ?? th, thresholdLabel: fmt(p?.th ?? th) } };
}

const MISSING_MONITOR: Monitor = { id: '', name: 'Deleted monitor', kind: 'metric_threshold', target: '—', condition: '—', severity: 'warning', state: 'ok', source: 'manual', lastChange: 0, channels: [] };
/** The incident's monitor; KPI incidents and incidents of deleted monitors get a stand-in. */
export const monitorOf = (i: Incident): Monitor => monitors.find((m) => m.id === i.monitorId)
  ?? (i.kpiId ? { ...MISSING_MONITOR, id: `kpi-${i.kpiId}`, name: i.title, kind: 'kpi_burn', target: `KPI · ${kpis.find((k) => k.id === i.kpiId)?.name ?? i.kpiId}`, targetSpec: { kpiId: i.kpiId } } as ApiMonitor
    : { ...MISSING_MONITOR, target: (i as ApiIncident).endpoint ? `${i.service} · ${(i as ApiIncident).endpoint}` : i.service });
export const serviceIdOf = (name: string) => services.find((s) => s.name === name || s.id === name)?.id;
export const sevTone = (s: Incident['severity']) => (s === 'critical' ? 'critical' : s === 'warning' ? 'warning' : 'accent');
const incidentStatus = (s: Incident['state']): StatusKind => s;
/** Notification targets for display: Slack App targets as `#name`. */
export const channelsLabel = (m: Monitor) => m.channels.map(targetLabel).join(', ');

/** Where a monitor is defined: alert-as-code (read-only), an accepted code suggestion, a template or by hand. */
export function MonitorSourceBadge({ m }: { m: Monitor }) {
  if (m.source === 'code') return <Badge tone="accent" icon={<Lock />}>Managed in repository</Badge>;
  if (m.source === 'suggestion') return <Badge tone="accent" icon={<Sparkles />}>From code suggestion</Badge>;
  if (m.source === 'agent') return <Badge tone="accent" icon={<Bot />}>Created by an agent (MCP)</Badge>;
  return <Badge>{m.source}</Badge>;
}

/** "Likely cause" line from the engine's RCA (ADR-017): summary + confidence; < 30% is flagged as low confidence. */
export function LikelyCause({ summary, confidence }: { summary: string; confidence?: number }) {
  const c = confidence !== undefined && Number.isFinite(confidence) ? Math.round(confidence * 100) : undefined;
  const low = confidence !== undefined && confidence < 0.3;
  return (
    <div className="row" style={{ gap: 'var(--space-2)', alignItems: 'baseline', flexWrap: 'wrap' }}>
      <strong>Likely cause:</strong>
      <span className="secondary" style={{ flex: 1, minWidth: 0 }}>{summary}</span>
      {c !== undefined && <Badge tone={low ? 'warning' : 'neutral'}>{c}% confidence{low ? ' · low confidence' : ''}</Badge>}
    </div>
  );
}

/** Why an incident is happening: the alert engine stores its root-cause analysis on the incident (RcaResult without children). */
function incidentRca(i: Incident): RcaResult | undefined {
  const r = (i as ApiIncident).rootCause as RcaResult | undefined;
  return r && Array.isArray(r.path) && r.path.length && r.root ? r : undefined;
}

// ---------- history ----------

export interface PastIncident extends Incident { resolvedAt: number; ackBy?: string }
/** Resolved incidents of the last 7 days (hydrated in place from the api). */
export const pastIncidents: PastIncident[] = resolvedIncidents;
/** When it was acknowledged (the api's ackedAt). */
export const ackedAtOf = (i: Incident) => (i as ApiIncident).ackedAt;

// ---------- acknowledgements (shared across screens for the session) ----------

const ackedIds = new Set<string>();
const resolvedIds = new Set<string>();
const ackListeners = new Set<() => void>();
const notifyAcks = () => ackListeners.forEach((f) => f());

/** POST, then refresh the lists; the local mark shows the change until they land. */
function incidentAction(id: string, action: 'ack' | 'resolve', mark: Set<string>) {
  mark.add(id);
  notifyAcks();
  mutate('POST', `/incidents/${seg(id)}/${action}`, undefined, ['/incidents'])
    .catch((e: unknown) => { console.warn(`[data] ${action} incident`, e); mark.delete(id); notifyAcks(); });
}

/**
 * Acknowledge / resolve state shared by the Incidents page, service Alerts tabs and the sidebar badge.
 * POST /incidents/{id}/ack | resolve.
 */
export function useAcks() {
  const [, bump] = useState(0);
  useData();
  useEffect(() => {
    const f = () => bump((n) => n + 1);
    ackListeners.add(f);
    return () => { ackListeners.delete(f); };
  }, []);
  const ack = (id: string) => incidentAction(id, 'ack', ackedIds);
  const resolve = (id: string) => incidentAction(id, 'resolve', resolvedIds);
  const stateOf = (i: Incident): Incident['state'] =>
    (i as PastIncident).resolvedAt || resolvedIds.has(i.id) ? 'resolved' : ackedIds.has(i.id) && i.state === 'firing' ? 'acknowledged' : i.state;
  return { ack, resolve, stateOf };
}

export const INCIDENT_ORDER: Record<Incident['state'], number> = { firing: 0, acknowledged: 1, pending: 2, resolved: 3 };

// ---------- screen ----------

/** Which service a monitor watches: "svc · …", "svc logs", or a KPI's target service. */
export function monitorServiceId(m: Monitor): string | undefined {
  const kpi = m.kind === 'kpi_burn' ? kpiOfMonitor(m) : undefined;
  if (kpi) return kpi.target.serviceId;
  const name = m.target.split(' · ')[0].replace(/ logs$/, '');
  return serviceIdOf(name);
}

/** Tab badge: firing count in red, otherwise number of monitors. */
export function AlertCount({ serviceId, endpointRef }: { serviceId: string; endpointRef?: string }) {
  const ms = monitors.filter((m) => monitorServiceId(m) === serviceId && (!endpointRef || m.target.split(' · ')[1] === endpointRef
    || (m.kind === 'kpi_burn' && kpiOfMonitor(m)?.target.ref === endpointRef)));
  const firing = incidents.filter((i) => i.state === 'firing' && ms.some((m) => m.id === i.monitorId)).length;
  return firing ? <Badge tone="critical">{firing} firing</Badge> : <Badge>{ms.length}</Badge>;
}

/**
 * Open incidents of a service / endpoint — shown at the top of its page (what is broken first),
 * not inside the Alerts tab. Renders nothing when all is clear.
 */
export function OpenIncidents({ serviceId, endpointRef }: { serviceId: string; endpointRef?: string }) {
  const nav = useNavigate();
  const { ack, resolve, stateOf } = useAcks();
  const incDrawer = useDrawer<Incident>();
  const mine = (m: Monitor) => {
    if (monitorServiceId(m) !== serviceId) return false;
    if (!endpointRef) return true;
    const kpi = m.kind === 'kpi_burn' ? kpiOfMonitor(m) : undefined;
    return kpi ? kpi.target.kind === 'endpoint' && kpi.target.ref === endpointRef : m.target.split(' · ')[1] === endpointRef;
  };
  const open = incidents
    .filter((i) => mine(monitorOf(i)) || (!endpointRef && serviceIdOf(i.service) === serviceId))
    .filter((i) => stateOf(i) !== 'resolved')
    .sort((a, b) => INCIDENT_ORDER[stateOf(a)] - INCIDENT_ORDER[stateOf(b)] || (a.severity === 'critical' ? -1 : 1));
  if (open.length === 0) return null;
  const firing = open.filter((i) => stateOf(i) === 'firing').length;
  return (
    <Section title="Open incidents" subtitle={firing ? `${firing} firing · ${open.length - firing} acknowledged or pending` : `${open.length} acknowledged or pending`}
      actions={<Button size="sm" variant="ghost" onClick={() => nav('/incidents')}>All incidents →</Button>}>
      <div className="grid grid-3">
        {open.map((i) => <IncidentCard key={i.id} incident={i} state={stateOf(i)} onOpen={() => incDrawer.open(i)} onAck={() => ack(i.id)} />)}
      </div>
      {incDrawer.item && (
        <IncidentDrawer incident={incDrawer.item} state={stateOf(incDrawer.item)}
          onClose={incDrawer.close} onAck={() => ack(incDrawer.item!.id)} onResolve={() => resolve(incDrawer.item!.id)}
          onOpenService={(id) => nav(`/services/${id}`)} />
      )}
    </Section>
  );
}

/** Monitors + code suggestions + history; open incidents sit at the top of the page (OpenIncidents). With `serviceId` everything is scoped to that service. */
/** Alerts of something that is not a service (a host, database or Kubernetes cluster): which monitors belong to it and where "New monitor" goes. */
export interface AlertScope {
  /** shown in the empty state: "No monitors on <label> yet" */
  label: string;
  match: (m: ApiMonitor) => boolean;
  /** editor link, prefilled for this item */
  newHref: string;
  /** already inside an "Alerts" tab: no section title */
  inTab?: boolean;
}

export function AlertsBody({ serviceId, endpointRef, scope }: { serviceId?: string; endpointRef?: string; scope?: AlertScope }) {
  useInfra();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  // on a service page the outer tab lives in ?tab=, so the inner one uses ?atab=
  const loc = useLocation();
  const key = serviceId || scope ? 'atab' : 'tab';
  const raw = params.get(key) as Tab | null;
  const tab: Tab = (raw === 'suggested' || raw === 'maintenance') && !scope ? raw : raw === 'history' ? raw : 'monitors';
  const setTab = (t: Tab) => setParams((p) => { const n = new URLSearchParams(p); if (t === 'monitors') n.delete(key); else n.set(key, t); return n; }, { replace: true });
  const svc = serviceId ? services.find((s) => s.id === serviceId) : undefined;
  const onEndpoint = (target: string) => target.split(' · ')[1] === endpointRef;
  const mine = (m: Monitor) => {
    if (scope) return scope.match(m as ApiMonitor);
    if (serviceId && monitorServiceId(m) !== serviceId) return false;
    if (!endpointRef) return true;
    const kpi = m.kind === 'kpi_burn' ? kpiOfMonitor(m) : undefined;
    return kpi ? kpi.target.kind === 'endpoint' && kpi.target.ref === endpointRef : onEndpoint(m.target);
  };
  const scopedMonitors = monitors.filter(mine);
  const scopedSuggestions = suggestions.filter((s) => (!svc || s.target.split(' · ')[0].replace(/ logs$/, '') === svc.name) && (!endpointRef || onEndpoint(s.target)));
  const scopedHistory = pastIncidents.filter((h) => {
    const m = monitors.find((x) => x.id === h.monitorId);
    return endpointRef || scope ? !!m && mine(m) : !serviceId || serviceIdOf(h.service) === serviceId || (!!m && mine(m));
  });

  const { ack, resolve, stateOf } = useAcks();
  const isMuted = (m: Monitor) => m.state === 'muted';
  const toggleMute = (m: Monitor) => void setMonitorMute(m as ApiMonitor, !isMuted(m));
  const scopedWindows = maintenanceWindows.filter((w) => !serviceId || !w.scope.services?.length || w.scope.services.includes(serviceId));
  const [decided, setDecided] = useState<Record<string, 'accepted' | 'rejected'>>({});
  const pending = scopedSuggestions.filter((s) => !decided[s.id]);

  const incDrawer = useDrawer<Incident>();
  const newHref = scope ? scope.newHref : `/alerts/new${serviceId ? `?service=${serviceId}${endpointRef ? `&endpoint=${encodeURIComponent(endpointRef)}` : ''}` : ''}`;
  const monDrawer = useDrawer<Monitor>();
  const sugDrawer = useDrawer<Suggestion>();


  return (
    <>
      {/* 2 · configuration */}
      <Section title={scope && !scope.inTab ? 'Alerts' : undefined} actions={(serviceId || scope) && can('write') ? <>
          <Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => nav(newHref)}>New monitor</Button>
        </> : undefined}>
        <Tabs value={tab} onChange={setTab} tabs={[
          { id: 'monitors', label: <>Monitors <Badge>{scopedMonitors.length}</Badge></> },
          ...(scope ? [] : [{ id: 'suggested' as const, label: <><Sparkles size={14} color="var(--accent)" />Suggested from code <Badge tone="accent">{pending.length}</Badge></> }]),
          { id: 'history', label: <>History <Badge>{scopedHistory.length}</Badge></> },
          ...(scope ? [] : [{ id: 'maintenance' as const, label: <>Maintenance <Badge tone={scopedWindows.some(activeWindow) ? 'warning' : undefined}>{scopedWindows.length}</Badge></> }]),
        ]} />

        {tab === 'monitors' && scopedMonitors.length === 0 && (
          <Card>
            <EmptyState icon={<Bell />} title={`No monitors on ${scope?.label ?? endpointRef ?? svc?.name ?? 'this project'} yet`}
              action={can('write') ? <Button variant="primary" icon={<Plus size={14} />} onClick={() => nav(newHref)}>Create a monitor</Button> : undefined}>
              {scope ? 'Alert on memory, disk, CPU and more. Pick a template or set your own threshold, then choose where it notifies (Slack, email, webhook).' : pending.length > 0
                ? <><span>{pending.length} suggested from code are waiting. </span><a href="#" onClick={(e) => { e.preventDefault(); setTab('suggested'); }}>Review suggestions</a></>
                : 'Create one, or define a KPI so alerts fire on what users feel.'}
            </EmptyState>
          </Card>
        )}
        {tab === 'monitors' && scopedMonitors.length > 0 && (
          <Card flush>
            <Table rows={scopedMonitors} rowKey={(m) => m.id} onRowClick={monDrawer.open} columns={[
              { key: 'state', header: 'State', width: 130, render: (m) => isMuted(m) ? <StatusBadge status="muted" /> : <StatusBadge status={m.state} label={m.state === 'ok' ? 'OK' : undefined} /> },
              { key: 'name', header: 'Monitor', render: (m) => (
                <div style={{ minWidth: 0 }}>
                  <div className="row" style={{ gap: 6, fontWeight: 500 }}>{m.name}{m.source === 'code' && <Lock size={12} color="var(--accent)" aria-label="Managed in repository" />}{m.source === 'suggestion' && <Sparkles size={12} color="var(--accent)" aria-label="From code suggestion" />}{m.source === 'agent' && <Bot size={12} color="var(--accent)" aria-label="Created by an agent (MCP)" />}</div>
                  <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{m.target}</div>
                </div>
              ) },
              { key: 'cond', header: 'Condition', render: (m) => <span className="mono secondary" style={{ fontSize: 'var(--text-xs)', whiteSpace: 'nowrap' }}>{m.condition}</span> },
              { key: 'trend', header: 'Last 24h', render: (m) => <Sparkline data={metricSeries(m, '24h').data} width={120} height={28} /> },
              { key: 'chg', header: 'Last change', numeric: true, sortValue: (m) => m.lastChange, render: (m) => <span className="muted" style={{ whiteSpace: 'nowrap' }}>{ago(m.lastChange)}</span> },
              { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
            ]} />
          </Card>
        )}

        {tab === 'suggested' && (
          <>
            <Card>
              <div className="row" style={{ gap: 'var(--space-4)', flexWrap: 'wrap' }}>
                <Sparkles size={20} color="var(--accent)" aria-hidden />
                <div style={{ flex: 1, minWidth: 240 }}>
                  <div style={{ fontWeight: 600 }}>{pending.length} suggestions from {serviceId ? `${svc?.name}'s repository` : 'connected repositories'}</div>
                  <div className="secondary">Scored by business impact; thresholds set from 14 days of real traffic. Click one for the reasoning.</div>
                </div>
                <Button onClick={() => nav('/code')}>View inventory</Button>
                <Button variant="primary" icon={<Check size={14} />} disabled={!pending.length || !can('write')} title={can('write') ? undefined : denied('write')}
                  onClick={() => setDecided(Object.fromEntries(scopedSuggestions.map((s) => [s.id, decided[s.id] ?? ('accepted' as const)])))}>Accept all {pending.length}</Button>
              </div>
            </Card>
            {pending.length === 0
              ? <Card><EmptyState icon={<Check />} title="All caught up">New suggestions appear when code changes land on main.</EmptyState></Card>
              : (
                <div className="grid grid-2">
                  {pending.map((s) => (
                    <Card key={s.id} onClick={() => sugDrawer.open(s)} className="card--interactive">
                      <div className="stack" style={{ gap: 'var(--space-2)' }}>
                        <div className="row" style={{ gap: 'var(--space-2)' }}>
                          <h3 className="truncate" style={{ flex: 1 }}>{s.title}</h3>
                          <CriticalityBadge c={s.criticality} />
                        </div>
                        <div className="muted truncate" style={{ fontSize: 'var(--text-sm)' }}>{s.target}</div>
                        <div className="row" style={{ gap: 'var(--space-2)', marginTop: 'var(--space-1)' }}>
                          <code className="mono truncate" style={{ flex: 1, fontSize: 'var(--text-xs)', background: 'var(--bg-sunken)', padding: '2px 6px', borderRadius: 'var(--radius-sm)' }}>{s.condition}</code>
                          <Button size="sm" variant="ghost" icon={<X size={12} />} disabled={!can('write')}
                            onClick={(e) => { e.stopPropagation(); setDecided({ ...decided, [s.id]: 'rejected' }); }}>Dismiss</Button>
                          <Button size="sm" variant="primary" icon={<Check size={12} />} disabled={!can('write')}
                            onClick={(e) => { e.stopPropagation(); setDecided({ ...decided, [s.id]: 'accepted' }); }}>Accept</Button>
                        </div>
                      </div>
                    </Card>
                  ))}
                </div>
              )}
          </>
        )}

        {tab === 'maintenance' && <MaintenanceWindows serviceId={serviceId} />}

        {tab === 'history' && (
          <Card flush>
            <Table rows={scopedHistory} rowKey={(h) => h.id} empty="No resolved incidents in the last 7 days." onRowClick={incDrawer.open} defaultSort={{ key: 'start', dir: 'desc' }} columns={[
              { key: 'state', header: 'State', width: 130, render: () => <StatusBadge status="resolved" /> },
              { key: 'title', header: 'Incident', render: (h) => <div><div style={{ fontWeight: 500 }}>{h.title}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{h.service}</div></div> },
              { key: 'sev', header: 'Severity', render: (h) => <Badge tone={sevTone(h.severity)}>{h.severity}</Badge> },
              { key: 'peak', header: 'Peak', numeric: true, render: (h) => h.value },
              { key: 'dur', header: 'Duration', numeric: true, sortValue: (h) => h.resolvedAt - h.startedAt, render: (h) => duration((h.resolvedAt - h.startedAt) / 1000) },
              { key: 'start', header: 'Started', numeric: true, sortValue: (h) => h.startedAt, render: (h) => <span className="muted" style={{ whiteSpace: 'nowrap' }}>{ago(h.startedAt)}</span> },
            ]} />
          </Card>
        )}
      </Section>

      {incDrawer.item && (
        <IncidentDrawer incident={incDrawer.item} state={stateOf(incDrawer.item)}
          onClose={incDrawer.close} onAck={() => ack(incDrawer.item!.id)} onResolve={() => resolve(incDrawer.item!.id)}
          onOpenService={(id) => nav(`/services/${id}`)} />
      )}
      {monDrawer.item && (
        <MonitorDrawer monitor={monitors.find((x) => x.id === monDrawer.item!.id) ?? monDrawer.item} muted={isMuted(monDrawer.item)} onClose={monDrawer.close}
          onMute={() => toggleMute(monDrawer.item!)}
          onDelete={() => { void deleteMonitor(monDrawer.item!); monDrawer.close(); }}
          onEdit={() => nav(`/alerts/new?kind=${monDrawer.item!.kind}${serviceId ? `&service=${serviceId}` : ''}&edit=${monDrawer.item!.id}${scope ? `&back=${encodeURIComponent(loc.pathname + loc.search)}` : ''}`)} />
      )}
      {sugDrawer.item && (
        <SuggestionDrawer s={sugDrawer.item} onClose={sugDrawer.close} onEdit={() => nav(`/alerts/new?kind=${sugDrawer.item!.kind}`)}
          onDecide={(d) => { setDecided({ ...decided, [sugDrawer.item!.id]: d }); sugDrawer.close(); }} />
      )}
    </>
  );
}

// ---------- incident card ----------

export function IncidentCard({ incident: i, state, onOpen, onAck }: { incident: Incident; state: Incident['state']; onOpen: () => void; onAck: () => void }) {
  const m = monitorOf(i);
  const { ts, data, spec } = metricSeries(m, '1h');
  const firing = state === 'firing';
  const pending = state === 'pending';
  const rca = incidentRca(i);
  const longValue = i.value.length > 10;
  const forMin = (m as ApiMonitor).rule?.forMin ?? Number(/for (\d+)m/.exec(m.condition)?.[1] ?? 5);
  return (
    <section className={`card card--interactive ${firing ? 'card--rail-critical' : 'card--rail-warning'}`} onClick={onOpen} aria-label={pending ? `Pending: ${i.title}` : undefined}
      style={{ gap: 'var(--space-3)', padding: 'var(--space-4) var(--space-5)' }}>
      <div className="row" style={{ gap: 'var(--space-2)' }}>
        <StatusBadge status={incidentStatus(state)} />
        <Badge tone={sevTone(i.severity)}>{i.severity}</Badge>
        <span className="spacer" />
        <span className="muted num" style={{ fontSize: 'var(--text-sm)', whiteSpace: 'nowrap' }}>for {duration((Date.now() - i.startedAt) / 1000)}</span>
      </div>
      <div>
        <h3 className="kpi-card__title" style={{ marginBottom: 2 }}>{i.title}</h3>
        <div className="row muted" style={{ fontSize: 'var(--text-sm)', gap: 6 }}>
          {i.kpiId && <Badge tone="accent" icon={<Target />}>KPI</Badge>}
          <span className="truncate">{i.service} · {m.target.split(' · ')[1] ?? m.target}</span>
        </div>
      </div>
      <div className="row" style={{ alignItems: 'baseline', gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        <span className="num" style={{ fontSize: longValue ? 'var(--text-lg)' : 'var(--text-2xl)', fontWeight: 600 }}>{i.value}</span>
        <span className="muted">threshold {spec.thresholdLabel}</span>
      </div>
      <TimeSeries timestamps={ts} height={90} area format={spec.fmt}
        threshold={{ value: spec.threshold, label: spec.thresholdLabel }}
        series={[{ id: 'v', label: spec.label, slot: slotOf(m), data }]} />
      {rca && <WhyBlock result={rca} />}
      <div className="row" style={{ gap: 'var(--space-2)' }}>
        <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{pending
          ? <>Condition breached · fires if it holds {forMin} min · nobody notified yet</>
          : <>Notified {channelsLabel(m) || 'no channel'}</>}</span>
        <span className="spacer" />
        {firing && can('write') && <Button size="sm" icon={<Check size={12} />} onClick={(e) => { e.stopPropagation(); onAck(); }}>Acknowledge</Button>}
      </div>
    </section>
  );
}

// ---------- drawers ----------

export function IncidentDrawer({ incident: i, state, onClose, onAck, onResolve, onOpenService }: {
  incident: Incident; state: Incident['state']; onClose: () => void; onAck: () => void; onResolve?: () => void; onOpenService: (id: string) => void;
}) {
  const m = monitorOf(i);
  const past = (i as PastIncident).resolvedAt;
  const { ts, data, spec } = metricSeries(m, past ? '7d' : '6h');
  const sid = serviceIdOf(i.service);
  const forMin = (m as ApiMonitor).rule?.forMin ?? Number(/for (\d+)m/.exec(m.condition)?.[1] ?? 5);
  const ackedAt = ackedAtOf(i);

  const timeline: { ts: number; icon: ReactNode; text: ReactNode }[] = [
    { ts: i.startedAt - forMin * 60_000, icon: <CircleDashed size={14} color="var(--status-warning)" />, text: <>Pending — condition first breached</> },
  ];
  if (state !== 'pending') {
    timeline.push({ ts: i.startedAt, icon: <Bell size={14} color="var(--status-critical)" />, text: <>Firing — held for {forMin} minutes</> });
    timeline.push({ ts: i.startedAt + 8_000, icon: <Send size={14} color="var(--text-muted)" />, text: <>Notified {channelsLabel(m)}</> });
  }
  if (ackedAt) {
    timeline.push({ ts: ackedAt, icon: <Check size={14} color="var(--status-serious)" />, text: <>Acknowledged{(i as PastIncident).ackBy ? <> by {(i as PastIncident).ackBy}</> : null}</> });
  }
  if (past) timeline.push({ ts: past, icon: <CheckCircle2 size={14} color="var(--status-good)" />, text: <>Resolved — back under threshold</> });
  // the real timeline (pending, fired, notified, reminder, suppressed, acked, resolved) once the api answers
  const apiEvents = remote<IncidentEvent[]>(`/incidents/${seg(i.id)}/events`, []);
  if (apiEvents.length) timeline.splice(0, timeline.length, ...[...apiEvents].sort((a, b) => a.ts - b.ts).map((e) => eventRow(e, m)));

  const events = timeline.filter((t) => t.ts >= ts[0]).slice(0, 3).map((t, k) => ({ ts: t.ts, label: ['Pending', 'Firing', 'Notified'][k] }));
  const rca = past ? undefined : incidentRca(i);
  // the engine's stored RCA: one-line summary + confidence
  const stored = (i as ApiIncident).rootCause as { summary?: string; confidence?: number; infra?: RcaInfraCandidate[] | null } | undefined;
  const infraCauses = Array.isArray(stored?.infra) ? [...stored.infra].sort((a, b) => b.confidence - a.confidence) : [];
  const likely = stored?.summary ? { summary: stored.summary, confidence: typeof stored.confidence === 'number' ? stored.confidence : undefined }
    : rca ? { summary: rca.summary, confidence: rca.confidence } : undefined;
  const nav = useNavigate();
  const related = sid ? logsFor(sid).filter((l) => l.level === 'error' || l.level === 'warn').slice(0, 6) : [];
  const [explanation, setExplanation] = useState<{ summary: string; provider: string; usage: { inputTokens: number; outputTokens: number } } | null>(null);
  const [explainError, setExplainError] = useState('');
  const [explaining, setExplaining] = useState(false);
  const [nextSteps, setNextSteps] = useState<string[]>([]);
  const [stepsError, setStepsError] = useState('');
  const [suggesting, setSuggesting] = useState(false);
  const [report, setReport] = useState('');
  const [reportError, setReportError] = useState('');
  const [reporting, setReporting] = useState(false);
  const [triage, setTriage] = useState<{ groups: { label: string; count: number }[] } | null>(null);
  const [triageError, setTriageError] = useState('');
  const [triaging, setTriaging] = useState(false);
  useEffect(() => {
    setExplanation(null); setExplainError(''); setNextSteps([]); setStepsError('');
    setReport(''); setReportError(''); setTriage(null); setTriageError('');
  }, [i.id]);

  const incidentEvidence = [
    `Service: ${i.service}; severity: ${i.severity}; observed value: ${i.value}`,
    `Monitor: ${m.name}; target: ${m.target}; condition: ${m.condition}`,
    ...related.slice(0, 4).map((l) => `${l.level}: ${l.message}`),
  ].map((s) => s.slice(0, 500));

  const explain = async () => {
    setExplaining(true);
    setExplainError('');
    try {
      const response = await fetch('/local-ai/incident-summary', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: i.title, evidence: incidentEvidence }),
      });
      if (!response.ok) throw new Error(`Local mock server returned HTTP ${response.status}`);
      setExplanation(await response.json());
    } catch (cause) {
      setExplainError(cause instanceof Error ? cause.message : 'Could not reach the local mock server.');
    } finally {
      setExplaining(false);
    }
  };

  const suggestNextSteps = async () => {
    setSuggesting(true);
    setStepsError('');
    try {
      const response = await fetch('/local-ai/incident-next-steps', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: i.title, evidence: incidentEvidence }),
      });
      if (!response.ok) throw new Error(`Local mock server returned HTTP ${response.status}`);
      const result = await response.json() as { steps: string[] };
      setNextSteps(result.steps);
    } catch (cause) {
      setStepsError(cause instanceof Error ? cause.message : 'Could not reach the local mock server.');
    } finally {
      setSuggesting(false);
    }
  };

  const draftReport = async () => {
    setReporting(true);
    setReportError('');
    try {
      const response = await fetch('/local-ai/incident-report', {
        method: 'POST', headers: { 'Content-Type': 'application/json' }, body: JSON.stringify({ title: i.title, evidence: incidentEvidence }),
      });
      if (!response.ok) throw new Error(`Local mock server returned HTTP ${response.status}`);
      const result = await response.json() as { report: string };
      setReport(result.report);
    } catch (cause) {
      setReportError(cause instanceof Error ? cause.message : 'Could not reach the local mock server.');
    } finally {
      setReporting(false);
    }
  };

  const triageLogs = async () => {
    setTriaging(true);
    setTriageError('');
    try {
      const response = await fetch('/local-ai/triage-logs', {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ title: i.title, logs: related.map((l) => l.message.slice(0, 500)) }),
      });
      if (!response.ok) throw new Error(`Local mock server returned HTTP ${response.status}`);
      setTriage(await response.json());
    } catch (cause) {
      setTriageError(cause instanceof Error ? cause.message : 'Could not reach the local mock server.');
    } finally {
      setTriaging(false);
    }
  };

  return (
    <Drawer open onClose={onClose} width={720}
      title={i.title}
      subtitle={<span className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status={incidentStatus(state)} /><Badge tone={sevTone(i.severity)}>{i.severity}</Badge><span className="muted">started {dateTime(i.startedAt)}</span></span>}
      actions={<>
        <Button size="sm" icon={<Sparkles size={12} />} disabled={explaining} onClick={() => void explain()}>{explaining ? 'Explaining…' : 'Explain'}</Button>
        <LocalAICost />
        {i.kpiId && <Button size="sm" icon={<Target size={12} />} onClick={() => nav(`/kpis/${i.kpiId}`)}>Open KPI</Button>}
        {sid && <Button size="sm" icon={<ExternalLink size={12} />} onClick={() => onOpenService(sid)}>Open {i.service}</Button>}
        {state === 'firing' && can('write') && <Button size="sm" variant="primary" icon={<Check size={12} />} onClick={onAck}>Acknowledge</Button>}
        {onResolve && can('write') && !past && state !== 'resolved' && <Button size="sm" icon={<CheckCircle2 size={12} />} onClick={onResolve}>Resolve</Button>}
      </>}>
      {(explanation || explainError || explaining) && <DrawerSection title="AI explanation" aside={explanation?.provider === 'mock' ? 'Offline mock · 0 tokens' : undefined}>
        {explanation && <p className="secondary" style={{ margin: 0 }}>{explanation.summary}</p>}
        <LocalAIState busy={explaining} error={explainError} onRetry={() => void explain()} />
      </DrawerSection>}
      <DrawerSection title="AI follow-up" aside="Offline mock · 0 tokens">
        <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
          <Button size="sm" icon={<Sparkles size={12} />} disabled={suggesting} onClick={() => void suggestNextSteps()}>
            {suggesting ? 'Drafting…' : 'Suggest next steps'}
          </Button>
          <LocalAICost />
          <Button size="sm" icon={<FileCode2 size={12} />} disabled={reporting} onClick={() => void draftReport()}>
            {reporting ? 'Drafting…' : 'Draft incident report'}
          </Button>
          <LocalAICost />
        </div>
        {nextSteps.length > 0 && <ol className="stack" style={{ gap: 'var(--space-2)', marginTop: 'var(--space-3)' }}>
          {nextSteps.map((step) => <li key={step}>{step}</li>)}
        </ol>}
        <LocalAIState busy={suggesting} error={stepsError} onRetry={() => void suggestNextSteps()} />
        {report && <div style={{ marginTop: 'var(--space-3)' }}><CodeBlock>{report}</CodeBlock><p className="muted">Draft only; no report was saved.</p></div>}
        <LocalAIState busy={reporting} error={reportError} onRetry={() => void draftReport()} />
      </DrawerSection>
      {state === 'pending' && (
        <Callout icon={<CircleDashed size={16} />}>
          <strong>Pending.</strong> <span className="secondary">The condition is breached but has not held for {forMin} minutes yet. It fires and notifies {channelsLabel(m) || 'its channels'} if it holds; if it clears first, it closes quietly.</span>
        </Callout>
      )}
      {likely && !rca && <Callout icon={<Sparkles size={16} color="var(--accent)" />}><LikelyCause summary={likely.summary} confidence={likely.confidence} /></Callout>}
      {infraCauses.length > 0 && (
        <DrawerSection title="Infrastructure candidates" aside="pods and hosts of this service">
          <div className="list">
            {infraCauses.map((c, k) => (
              <div key={k} className="list__row">
                <div className="list__main">
                  <span className="list__title">{c.title}</span>
                  <span className="list__meta">{c.kind === 'pod' ? 'Pod' : 'Host'} <span className="mono">{c.entity}</span>{c.cluster ? ` · ${c.cluster}` : ''}{c.node ? ` · node ${c.node}` : ''}{c.restarts ? ` · ${c.restarts} restarts` : ''}</span>
                </div>
                <Badge tone={c.confidence < 0.3 ? 'warning' : 'neutral'}>{Math.round(c.confidence * 100)}% confidence</Badge>
              </div>
            ))}
          </div>
        </DrawerSection>
      )}
      {rca && (
        <DrawerSection title="Why it's happening" aside={<>{rca.root.service ? `root cause in ${rca.root.service}` : ''}{rca.confidence < 0.3 && <> <Badge tone="warning">low confidence</Badge></>}</>}>
          <WhyBlock result={rca} />
          {rca.root.evidence && rca.root.evidence.length > 0 && (
            <ul className="stack" style={{ gap: 'var(--space-2)', listStyle: 'none', margin: 0, padding: 0 }}>
              {rca.root.evidence.map((e, k) => (
                <li key={k} style={{ fontSize: 'var(--text-sm)' }}>
                  <div className={e.kind === 'query' || e.kind === 'log' ? 'mono' : undefined} style={{ fontWeight: 500, overflowWrap: 'anywhere' }}>{e.title}</div>
                  {(e.detail || e.at) && <div className="muted">{e.at ? `${clock(e.at)} · ` : ''}{e.detail}</div>}
                </li>
              ))}
            </ul>
          )}
          {i.kpiId && <div><Button size="sm" onClick={() => nav(`/kpis/${i.kpiId}#rca`)}>See the full drill-down</Button></div>}
        </DrawerSection>
      )}
      <DrawerSection title={spec.label} aside={past ? 'Last 7 days' : 'Last 6 hours'}>
        <TimeSeries timestamps={ts} height={220} area format={spec.fmt}
          threshold={{ value: spec.threshold, label: spec.thresholdLabel }} events={past ? undefined : events}
          series={[{ id: 'v', label: spec.label, slot: slotOf(m), data }]} />
      </DrawerSection>

      <DrawerSection title="Timeline">
        <ol className="stack" style={{ gap: 'var(--space-2)', listStyle: 'none', margin: 0, padding: 0 }}>
          {timeline.map((t, k) => (
            <li key={k} className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="mono muted" style={{ fontSize: 'var(--text-xs)', width: 64 }}>{clock(t.ts)}</span>
              {t.icon}
              <span>{t.text}</span>
            </li>
          ))}
        </ol>
      </DrawerSection>

      <DrawerSection title="Monitor" aside={m.name}>
        <KeyValues items={[
          ['Condition', <span className="mono">{m.condition}</span>],
          ['Target', m.target],
          ['Type', KIND_LABEL[m.kind]],
          ['Notifies', channelsLabel(m) || '—'],
          ['Source', <MonitorSourceBadge m={m} />],
          ['Peak value', i.value],
        ]} />
      </DrawerSection>

      <DrawerSection title="Related logs" aside={`${i.service} · warn & error`}>
        <div style={{ marginBottom: 'var(--space-3)' }}>
          <Button size="sm" icon={<Sparkles size={12} />} disabled={triaging || related.length === 0} onClick={() => void triageLogs()}>
            {triaging ? 'Triage in progress…' : 'Triage logs'}
          </Button>
          <LocalAICost />
        </div>
        {triage && <div className="stack" style={{ gap: 'var(--space-1)', marginBottom: 'var(--space-3)' }}>
          {triage.groups.map((group) => <div key={group.label}>{group.label}: {group.count}</div>)}
          <span className="muted">Offline mock · 0 tokens · labels are not saved.</span>
        </div>}
        <LocalAIState busy={triaging} error={triageError} onRetry={() => void triageLogs()} />
        {related.length === 0 ? <span className="muted">No warning or error logs from this service in the window.</span> : (
          <div className="stack" style={{ gap: 'var(--space-1)' }}>
            {related.map((l) => (
              <div key={l.id} className="row mono" style={{ gap: 'var(--space-3)', fontSize: 'var(--text-xs)', alignItems: 'flex-start' }}>
                <span className="muted" style={{ flexShrink: 0 }}>{clock(l.ts, true)}</span>
                <span style={{ flexShrink: 0, width: 40, color: l.level === 'error' ? 'var(--status-critical)' : 'var(--status-warning)' }}>{l.level}</span>
                <span className="secondary" style={{ overflowWrap: 'anywhere' }}>{l.message}</span>
              </div>
            ))}
          </div>
        )}
      </DrawerSection>
    </Drawer>
  );
}

function MonitorDrawer({ monitor: m, muted, onClose, onMute, onEdit, onDelete }: { monitor: Monitor; muted: boolean; onClose: () => void; onMute: () => void; onEdit: () => void; onDelete?: () => void }) {
  const { ts, data, spec } = metricSeries(m, '24h');
  const managed = m.source === 'code';
  const mutedUntil = (m as ApiMonitor).mutedUntil;
  const changes = [{ ts: m.lastChange, state: m.state }];
  return (
    <Drawer open onClose={onClose} title={m.name}
      subtitle={<span className="row" style={{ gap: 'var(--space-2)' }}>{muted ? <StatusBadge status="muted" /> : <StatusBadge status={m.state} label={m.state === 'ok' ? 'OK' : undefined} />}<span className="muted">{m.target}</span></span>}
      actions={can('write') && <>
        <Button size="sm" icon={muted ? <Bell size={12} /> : <BellOff size={12} />} onClick={onMute}>{muted ? 'Unmute' : 'Mute 1h'}</Button>
        <Button size="sm" icon={<Pencil size={12} />} onClick={onEdit} disabled={managed} title={managed ? 'Managed in repository — edit the YAML' : undefined}>Edit</Button>
        {onDelete && <Button size="sm" variant="ghost" icon={<Trash2 size={12} />} disabled={managed} title={managed ? 'Managed in repository — remove the YAML file' : undefined}
          onClick={() => { if (window.confirm(`Delete monitor “${m.name}”?`)) onDelete(); }}>Delete</Button>}
      </>}>
      {managed && (
        <Callout icon={<Code2 size={16} color="var(--accent)" />}>
          <span className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}><MonitorSourceBadge m={m} /><span className="mono secondary" style={{ fontSize: 'var(--text-sm)' }}>{m.codeRef ?? '.trooper/monitors/*.yaml'}</span></span>
          <div className="secondary" style={{ marginTop: 'var(--space-1)' }}>Defined in the repository; change the YAML and push to update it.</div>
        </Callout>
      )}
      {muted && mutedUntil && mutedUntil > Date.now() && <Callout icon={<BellOff size={16} />}>Muted until {dateTime(mutedUntil)} — it is evaluated but nobody is notified.</Callout>}
      <DrawerSection title={spec.label} aside="Last 24 hours">
        <TimeSeries timestamps={ts} height={200} area format={spec.fmt}
          threshold={{ value: spec.threshold, label: spec.thresholdLabel }}
          series={[{ id: 'v', label: spec.label, slot: slotOf(m), data }]} />
      </DrawerSection>
      <DrawerSection title="Configuration">
        <KeyValues items={[
          ['Condition', <span className="mono">{m.condition}</span>],
          ['Type', KIND_LABEL[m.kind]],
          ['Severity', <Badge tone={sevTone(m.severity)}>{m.severity}</Badge>],
          ['Notifies', channelsLabel(m) || '—'],
          ['Source', <MonitorSourceBadge m={m} />],
          ...(m.codeRef ? [['Defined in', <span className="mono">{m.codeRef}</span>] as [string, ReactNode]] : []),
          ['Target', m.target],
        ]} />
      </DrawerSection>
      <DrawerSection title="Recent state changes">
        <ol className="stack" style={{ gap: 'var(--space-2)', listStyle: 'none', margin: 0, padding: 0 }}>
          {changes.map((c, k) => (
            <li key={k} className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="muted num" style={{ fontSize: 'var(--text-xs)', width: 120 }}>{dateTime(c.ts)}</span>
              <StatusBadge status={c.state} label={c.state === 'ok' ? 'OK' : undefined} />
            </li>
          ))}
        </ol>
      </DrawerSection>
    </Drawer>
  );
}

function SuggestionDrawer({ s, onClose, onEdit, onDecide }: { s: Suggestion; onClose: () => void; onEdit: () => void; onDecide: (d: 'accepted' | 'rejected') => void }) {
  return (
    <Drawer open onClose={onClose} title={s.title}
      subtitle={<span className="row" style={{ gap: 'var(--space-2)' }}><CriticalityBadge c={s.criticality} /><span className="muted">{s.target}</span></span>}
      actions={<>
        <Button size="sm" icon={<X size={12} />} disabled={!can('write')} onClick={() => onDecide('rejected')}>Dismiss</Button>
        <Button size="sm" icon={<Pencil size={12} />} onClick={onEdit}>Edit</Button>
        <Button size="sm" variant="primary" icon={<Check size={12} />} disabled={!can('write')} onClick={() => onDecide('accepted')}>Accept</Button>
      </>}>
      <DrawerSection title="Why this alert">
        <p className="secondary" style={{ margin: 0 }}>{s.rationale}</p>
      </DrawerSection>
      <DrawerSection title="Proposed monitor">
        <KeyValues items={[
          ['Condition', <span className="mono">{s.condition}</span>],
          ['Type', KIND_LABEL[s.kind]],
          ['Confidence', `${Math.round(s.confidence * 100)}%`],
          ['Threshold', s.calibrated ? 'Calibrated on 14 days of traffic' : <Badge tone="warning">Calibrating — no baseline yet</Badge>],
        ]} />
      </DrawerSection>
      <DrawerSection title="Found in code">
        <span className="row mono" style={{ gap: 'var(--space-2)', fontSize: 'var(--text-sm)' }}><FileCode2 size={14} className="muted" />{s.codeRef}</span>
      </DrawerSection>
    </Drawer>
  );
}

// ---------- monitor actions (api mode) ----------

/** Mute for an hour / unmute. PATCH {mutedUntil}; until the api has it, PUT the monitor with status muted|active. */
async function setMonitorMute(m: ApiMonitor, mute: boolean) {
  const prev = m.state;
  m.state = mute ? 'muted' : 'ok';
  try {
    await mutate('PATCH', `/monitors/${seg(m.id)}`, { mutedUntil: mute ? Date.now() + 3600_000 : null }, ['/monitors']);
  } catch (e) {
    if (!(e instanceof ApiError) || (e.status !== 404 && e.status !== 405)) { m.state = prev; console.warn('[data] mute monitor', e); return; }
    try {
      await mutate('PUT', `/monitors/${seg(m.id)}`, monitorInput(m, { status: mute ? 'muted' : 'active' }), ['/monitors']);
    } catch (e2) { m.state = prev; console.warn('[data] mute monitor', e2); }
  }
}

async function deleteMonitor(m: Monitor) {
  try { await mutate('DELETE', `/monitors/${seg(m.id)}`, undefined, ['/monitors']); } catch (e) { console.warn('[data] delete monitor', e); }
}

// ---------- maintenance windows ----------

const activeWindow = (w: MaintenanceWindow) => w.startsAt <= Date.now() && w.endsAt > Date.now();
const DURATIONS = [{ value: '30', label: '30 minutes' }, { value: '60', label: '1 hour' }, { value: '120', label: '2 hours' }, { value: '240', label: '4 hours' }, { value: '480', label: '8 hours' }];
const localInput = (t: number) => { const d = new Date(t - new Date(t).getTimezoneOffset() * 60_000); return d.toISOString().slice(0, 16); };

/** Planned quiet periods: monitors in scope keep evaluating but notify nobody. */
export function MaintenanceWindows({ serviceId }: { serviceId?: string }) {
  useData();
  const [, bump] = useState(0);
  const [name, setName] = useState('');
  const [scope, setScope] = useState(serviceId ?? '');
  const [start, setStart] = useState(() => localInput(Date.now()));
  const [minutes, setMinutes] = useState('60');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const rows = maintenanceWindows.filter((w) => !serviceId || !w.scope.services?.length || w.scope.services.includes(serviceId))
    .sort((a, b) => b.startsAt - a.startsAt);
  const svcName = (id: string) => services.find((s) => s.id === id)?.name ?? id;

  if (!maintenanceAvailable) {
    return <Card><Callout icon={<Wrench size={16} />}>Maintenance windows need the api's <span className="mono">/maintenance-windows</span> endpoint, which this server does not have yet.</Callout></Card>;
  }

  const create = async () => {
    const startsAt = new Date(start).getTime() || Date.now();
    const w = { name: name.trim() || `Maintenance${scope ? ` · ${svcName(scope)}` : ''}`, startsAt, endsAt: startsAt + Number(minutes) * 60_000, scope: scope ? { services: [scope] } : {} };
    setBusy(true); setError(undefined);
    try {
      await mutate('POST', '/maintenance-windows', w, ['/maintenance-windows']);
      setName('');
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false); bump((n) => n + 1);
  };
  const remove = async (w: MaintenanceWindow) => {
    try {
      await mutate('DELETE', `/maintenance-windows/${seg(w.id)}`, undefined, ['/maintenance-windows']);
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    bump((n) => n + 1);
  };

  return (
    <div className="stack">
      <Card title="Schedule a maintenance window" subtitle="Monitors in scope keep evaluating; incidents open but nobody is notified">
        <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap', alignItems: 'end' }}>
          <div style={{ flex: 2, minWidth: 180 }}><Field label="Name"><Input value={name} placeholder="Postgres upgrade" onChange={(e) => setName(e.target.value)} /></Field></div>
          <div style={{ flex: 1, minWidth: 160 }}><Field label="Scope"><Select value={scope} onChange={(e) => setScope(e.target.value)}
            options={[{ value: '', label: 'Whole project' }, ...services.map((s) => ({ value: s.id, label: s.name }))]} /></Field></div>
          <div style={{ flex: 1, minWidth: 180 }}><Field label="Starts"><Input type="datetime-local" value={start} onChange={(e) => setStart(e.target.value)} /></Field></div>
          <div style={{ flex: 1, minWidth: 130 }}><Field label="For"><Select value={minutes} onChange={(e) => setMinutes(e.target.value)} options={DURATIONS} /></Field></div>
          <Button variant="primary" icon={<Plus size={14} />} disabled={busy || !can('write')} title={can('write') ? undefined : denied('write')} onClick={() => void create()}>Schedule</Button>
        </div>
        {error && <div style={{ color: 'var(--status-critical)', fontSize: 'var(--text-sm)' }}>{error}</div>}
      </Card>
      <Card flush>
        <Table<MaintenanceWindow> rows={rows} rowKey={(w) => w.id} empty="No maintenance windows." columns={[
          { key: 'state', header: 'State', width: 120, render: (w) => activeWindow(w) ? <Badge tone="warning" icon={<Wrench />}>Active</Badge> : w.startsAt > Date.now() ? <Badge>Scheduled</Badge> : <Badge tone="neutral">Ended</Badge> },
          { key: 'name', header: 'Window', render: (w) => <div><div style={{ fontWeight: 500 }}>{w.name}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{w.scope.services?.length ? w.scope.services.map(svcName).join(', ') : w.scope.monitorIds?.length ? `${w.scope.monitorIds.length} monitors` : 'Whole project'}</div></div> },
          { key: 'start', header: 'Starts', numeric: true, sortValue: (w) => w.startsAt, render: (w) => <span className="muted" style={{ whiteSpace: 'nowrap' }}>{dateTime(w.startsAt)}</span> },
          { key: 'dur', header: 'Duration', numeric: true, render: (w) => duration((w.endsAt - w.startsAt) / 1000) },
          { key: 'del', header: '', width: 40, render: (w) => can('write') ? <Button size="sm" variant="ghost" iconOnly icon={<Trash2 size={14} />} aria-label={`Delete ${w.name}`} onClick={() => void remove(w)} /> : null },
        ]} />
      </Card>
    </div>
  );
}

/** One api incident event as a timeline row. */
function eventRow(e: IncidentEvent, m: Monitor): { ts: number; icon: ReactNode; text: ReactNode } {
  const d = (e.data ?? {}) as Record<string, unknown>;
  const ch = typeof d.channel === 'string' ? d.channel : undefined;
  switch (e.kind) {
    case 'pending': return { ts: e.ts, icon: <CircleDashed size={14} color="var(--status-warning)" />, text: <>Pending — condition first breached</> };
    case 'fired': return { ts: e.ts, icon: <Bell size={14} color="var(--status-critical)" />, text: <>Firing{typeof d.display === 'string' ? <> at {d.display}</> : null}</> };
    case 'notified': return { ts: e.ts, icon: <Send size={14} color="var(--text-muted)" />, text: <>Notified {ch ? targetLabel(ch) : channelsLabel(m)}</> };
    case 'reminder': return { ts: e.ts, icon: <Send size={14} color="var(--text-muted)" />, text: <>Reminder sent{ch ? <> to {ch}</> : null} — still firing</> };
    case 'suppressed': return { ts: e.ts, icon: <BellOff size={14} color="var(--text-muted)" />, text: <>Notification suppressed{typeof d.reason === 'string' ? <> — {d.reason}</> : ' (muted or in a maintenance window)'}</> };
    case 'acked': return { ts: e.ts, icon: <Check size={14} color="var(--status-serious)" />, text: <>Acknowledged{typeof d.by === 'string' && d.by !== 'api' ? <> by {d.by}</> : null}</> };
    case 'resolved': return { ts: e.ts, icon: <CheckCircle2 size={14} color="var(--status-good)" />, text: <>Resolved{d.quiet ? ' quietly — cleared while pending' : ''}</> };
    default: return { ts: e.ts, icon: <CircleDashed size={14} color="var(--text-muted)" />, text: <>{e.kind}</> };
  }
}
