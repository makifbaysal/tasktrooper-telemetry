import { useData } from '@/data/remote';
import { useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { Activity, Bug, Clock, Code2, FileText, Gauge, GitMerge, Lock, Server, Sparkles, SquareFunction, TrendingUp } from 'lucide-react';
import { Columns, TimeSeries } from '@/components/charts';
import { Badge, Button, Callout, Card, Field, Input, MetricCard, PageHeader, Segmented, Select, StatusBadge, Toggle } from '@/components/ui';
import { clusters, endpointsFor, groupsOfService, isApi, monitors, mutate, qs, seg, series, services, timestamps, type Monitor } from '@/data';
import { postRemote, type ApiMonitor } from '@/data/api/ops';
import { can, denied } from '@/data/auth';
import { serverlessFunctions } from '@/data/serverless';
import { monitorTemplates, type MonitorTemplate } from '@/data/monitorTemplates';
import { LagAlertEditor } from './LagAlerts';
import { NotifyPicker } from './NotifyPicker';
import { compact, ms, pct } from '@/lib/format';

type Kind = 'metric_threshold' | 'anomaly' | 'composite' | 'log_match' | 'error_new' | 'consumer_lag' | 'job_missed' | 'infra' | 'faas';
const KINDS: { id: Kind; label: string; desc: string; Icon: typeof Activity }[] = [
  { id: 'metric_threshold', label: 'Metric threshold', desc: 'Error rate, throughput, latency above/below a value', Icon: Activity },
  { id: 'anomaly', label: 'Anomaly', desc: 'Deviation from the seasonal baseline', Icon: TrendingUp },
  { id: 'error_new', label: 'New error', desc: 'An error group never seen before appears', Icon: Bug },
  { id: 'job_missed', label: 'Missed job', desc: 'Cron/worker did not report in time', Icon: Clock },
  { id: 'composite', label: 'Composite', desc: 'Several monitors firing together (and / or)', Icon: GitMerge },
  { id: 'log_match', label: 'Log query', desc: 'Count of matching log lines', Icon: FileText },
  { id: 'consumer_lag', label: 'Consumer lag', desc: 'Queue backlog and DLQ', Icon: Gauge },
  { id: 'infra', label: 'Infrastructure', desc: 'Hosts, containers, pods, nodes and workloads', Icon: Server },
  { id: 'faas', label: 'Serverless function', desc: 'Lambda / Azure Functions errors, throttles, duration', Icon: SquareFunction },
];
/** Kinds the editor can save to the api (log_match: YAML only for now; consumer_lag: lag editor). */
const SAVABLE = new Set<Kind>(['metric_threshold', 'anomaly', 'composite', 'job_missed', 'error_new', 'infra', 'faas']);

// ---- infra (contracts §20) and faas (§21) rule shapes
type InfraEntity = 'host' | 'container' | 'pod' | 'node' | 'workload';
type InfraMetric = 'cpu' | 'memory' | 'disk' | 'load1' | 'cpu_ratio' | 'memory_ratio' | 'restarts' | 'throttled' | 'not_ready' | 'unavailable';
const INFRA_ENTITIES: { value: InfraEntity; label: string }[] = [
  { value: 'host', label: 'Host' }, { value: 'container', label: 'Container' }, { value: 'pod', label: 'Pod' }, { value: 'node', label: 'Kubernetes node' }, { value: 'workload', label: 'Workload' },
];
const INFRA_METRIC_LABEL: Record<InfraMetric, string> = {
  cpu: 'CPU', memory: 'Memory', disk: 'Disk (fullest filesystem)', load1: 'Load (1 min)', cpu_ratio: 'CPU / limit', memory_ratio: 'Memory / limit',
  restarts: 'Restarts in window', throttled: 'CPU throttled', not_ready: 'Not ready (count)', unavailable: 'Unavailable replicas',
};
const INFRA_METRICS: Record<InfraEntity, InfraMetric[]> = {
  host: ['cpu', 'memory', 'disk', 'load1'], container: ['cpu_ratio', 'memory_ratio', 'restarts', 'throttled'],
  pod: ['cpu_ratio', 'memory_ratio', 'restarts', 'throttled', 'not_ready'], node: ['not_ready'], workload: ['unavailable'],
};
/** 0..1 metrics are typed as percentages */
const INFRA_RATIO = new Set<InfraMetric>(['cpu', 'memory', 'disk', 'cpu_ratio', 'memory_ratio', 'throttled']);
type FaasMetric = 'invocations' | 'errors' | 'error_rate' | 'throttles' | 'duration_p95' | 'duration_p99' | 'cold_starts' | 'concurrency' | 'iterator_age_ms';
const FAAS_METRICS: { value: FaasMetric; label: string }[] = [
  { value: 'error_rate', label: 'Error rate' }, { value: 'errors', label: 'Errors' }, { value: 'throttles', label: 'Throttles' },
  { value: 'duration_p95', label: 'p95 duration' }, { value: 'duration_p99', label: 'p99 duration' }, { value: 'invocations', label: 'Invocations' },
  { value: 'cold_starts', label: 'Cold starts' }, { value: 'concurrency', label: 'Concurrent executions' }, { value: 'iterator_age_ms', label: 'Iterator age (streams)' },
];
const faasUnit = (m: FaasMetric) => (m === 'error_rate' ? '%' : m === 'duration_p95' || m === 'duration_p99' || m === 'iterator_age_ms' ? 'ms' : 'count');
const faasFmt = (m: FaasMetric) => (m === 'error_rate' ? (v: number) => pct(v) : faasUnit(m) === 'ms' ? ms : (v: number) => compact(v));
const isDuration = (m: FaasMetric) => m === 'duration_p95' || m === 'duration_p99';
const isFaasMetric = (v: unknown): v is FaasMetric => FAAS_METRICS.some((x) => x.value === v);
const isInfraEntity = (v: unknown): v is InfraEntity => INFRA_ENTITIES.some((x) => x.value === v);
const str = (v: unknown) => (typeof v === 'string' ? v : '');

type Metric = 'error_rate' | 'p95' | 'rpm';
const METRICS: { value: Metric; label: string }[] = [{ value: 'error_rate', label: 'Error rate' }, { value: 'p95', label: 'p95 latency' }, { value: 'rpm', label: 'Throughput' }];
const unitOf = (m: Metric) => (m === 'error_rate' ? '%' : m === 'p95' ? 'ms' : 'req/min');
const fmtOf = (m: Metric) => (m === 'error_rate' ? (v: number) => pct(v) : m === 'p95' ? ms : (v: number) => compact(v));
const MOCK_BASE: Record<Metric, number> = { error_rate: 0.009, p95: 320, rpm: 1400 };

type Sensitivity = 'low' | 'medium' | 'high';
const Z: Record<Sensitivity, number> = { low: 4, medium: 3, high: 2 };
const sensitivityOf = (z: number): Sensitivity | undefined => (Object.entries(Z) as [Sensitivity, number][]).find(([, v]) => v === z)?.[0];

interface Preview { timestamps: number[]; data: number[]; th: number; episodes: number; firingNow: boolean; now: number }

const toKind = (m?: ApiMonitor): Kind | undefined => {
  if (!m) return undefined;
  const k = m.apiKind ?? m.kind;
  if (k === 'latency' || k === 'error_rate' || k === 'throughput') return 'metric_threshold';
  return KINDS.some((x) => x.id === k) ? (k as Kind) : undefined;
};
const num = (v: unknown, d: number) => (typeof v === 'number' && Number.isFinite(v) ? v : d);

/** Rolling z-score of each point against the previous `w` points (mock anomaly preview). */
function zScores(data: number[], w = 16): number[] {
  return data.map((v, i) => {
    const win = data.slice(Math.max(0, i - w), i);
    if (win.length < 4) return 0;
    const mean = win.reduce((a, x) => a + x, 0) / win.length;
    const sd = Math.sqrt(win.reduce((a, x) => a + (x - mean) ** 2, 0) / win.length) || mean * 0.05 || 1;
    return (v - mean) / sd;
  });
}
const episodesOf = (hit: boolean[]) => hit.filter((h, i) => h && !hit[i - 1]).length;

export function AlertEditor() {
  useData();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const editing = monitors.find((m) => m.id === params.get('edit')) as ApiMonitor | undefined;
  const managed = editing?.source === 'code';
  const readOnly = managed || !can('write');
  const [kind, setKind] = useState<Kind>(toKind(editing) ?? (KINDS.some((k) => k.id === params.get('kind')) ? params.get('kind') as Kind : 'metric_threshold'));
  // monitors live under their service; go back to its Alerts tab
  const svcParam = params.get('service') ?? editing?.targetSpec?.service ?? null;
  const svcName = services.find((s) => s.id === svcParam)?.name ?? svcParam ?? '';
  const back = `/services/${svcParam}?tab=alerts`;
  const goBack = () => (svcParam ? nav(back) : nav(-1));

  const q = editing?.query ?? {};
  const initMetric = (editing?.apiKind === 'latency' ? 'p95' : (q.metric as Metric | undefined)) ?? 'error_rate';
  const [name, setName] = useState(editing?.name ?? '');
  const [service, setService] = useState(svcParam ?? services.find((s) => s.type !== 'web' && s.type !== 'mobile')?.id ?? '');
  const [endpoint, setEndpoint] = useState(editing?.targetSpec?.endpoint ?? params.get('endpoint') ?? '*');
  const [metric, setMetric] = useState<Metric>(initMetric);
  const [threshold, setThreshold] = useState(editing?.rule && kind === 'metric_threshold' ? String(initMetric === 'error_rate' ? +(editing.rule.threshold * 100).toFixed(3) : editing.rule.threshold) : '2');
  const [forMin, setForMin] = useState(String(editing?.rule?.forMin ?? 5));
  const [minReq, setMinReq] = useState(String(editing?.rule?.minRequests ?? 50));
  const [renotify, setRenotify] = useState(String(editing?.rule?.renotifyMin ?? 0));
  const [severity, setSeverity] = useState<string>(editing?.severity ?? 'critical');
  const [channels, setChannels] = useState<string[]>(editing?.channels ?? (isApi ? [] : ['slack:C01PAYALRT#payments-alerts', 'pagerduty:payments']));
  // anomaly
  const [z, setZ] = useState(String(num(q.sensitivity, editing?.rule && kind === 'anomaly' ? editing.rule.threshold : 3)));
  const [baseline, setBaseline] = useState(String(q.baseline ?? '7d'));
  const [direction, setDirection] = useState<'up' | 'down' | 'both'>((q.direction as 'up' | 'down' | 'both' | undefined) ?? 'up');
  // composite
  const [compOp, setCompOp] = useState<'and' | 'or'>(q.op === 'or' ? 'or' : 'and');
  const [picked, setPicked] = useState<string[]>(Array.isArray(q.monitorIds) ? (q.monitorIds as string[]) : []);
  // job_missed
  const [process, setProcess] = useState(editing?.targetSpec?.process ?? '');
  const [everyMin, setEveryMin] = useState(String(num(q.expectedEveryMin, 60)));
  const [graceMin, setGraceMin] = useState(String(num(q.graceMin, 10)));
  // error_new
  const [errScope, setErrScope] = useState<string>(kind === 'error_new' && editing && !editing.targetSpec?.service ? '*' : svcParam ?? '*');
  // infra
  const qf = (q.filter ?? {}) as Record<string, unknown>;
  const [entity, setEntity] = useState<InfraEntity>(isInfraEntity(q.entity) ? q.entity : 'host');
  const [infraMetric, setInfraMetric] = useState<InfraMetric>((INFRA_METRICS[isInfraEntity(q.entity) ? q.entity : 'host'] as string[]).includes(str(q.metric)) ? q.metric as InfraMetric : 'cpu');
  const [agg, setAgg] = useState<'max' | 'avg'>(q.agg === 'avg' ? 'avg' : 'max');
  const [fCluster, setFCluster] = useState(str(qf.cluster));
  const [fNamespace, setFNamespace] = useState(str(qf.namespace));
  const [fHost, setFHost] = useState(str(qf.host));
  const [fWorkload, setFWorkload] = useState(str(qf.workload));
  const [fService, setFService] = useState(str(qf.service) || (kind === 'infra' ? svcParam ?? '' : ''));
  // faas
  const fns = serverlessFunctions('1h');
  const [fnId, setFnId] = useState(str(q.functionId) || params.get('function') || (kind === 'faas' && (qf.provider || qf.region || qf.service) ? '*' : fns[0]?.id ?? '*'));
  const [faasMetric, setFaasMetric] = useState<FaasMetric>(isFaasMetric(q.metric) ? q.metric : isFaasMetric(params.get('metric')) ? params.get('metric') as FaasMetric : 'error_rate');
  const [fProvider, setFProvider] = useState(str(qf.provider));
  const [fRegion, setFRegion] = useState(str(qf.region));
  const [op, setOp] = useState<'>' | '<'>(editing?.rule?.op === '<' ? '<' : '>');
  // "p95 > timeout × 0.8" = relativeToTimeout + threshold 0.8 (contracts §21)
  const [relTimeout, setRelTimeout] = useState(q.relativeToTimeout === true);
  const [condWindow, setCondWindow] = useState<string | undefined>(str((editing?.rule as { window?: unknown } | undefined)?.window) || undefined);
  const [templateId, setTemplateId] = useState<string>();
  const [ruleTh, setRuleTh] = useState(() => {
    const t = editing?.rule?.threshold;
    if (kind === 'infra') return t === undefined ? '90' : String(INFRA_RATIO.has(infraMetric) ? +(t * 100).toFixed(2) : t);
    if (kind === 'faas') return t === undefined ? (faasMetric === 'error_rate' ? '5' : '0') : String(faasMetric === 'error_rate' ? +(t * 100).toFixed(3) : t);
    return '90';
  });

  const [yaml, setYaml] = useState(false);
  const [lagOpen, setLagOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const lagGroup = svcParam ? groupsOfService(svcParam)[0] : undefined;
  const lagCluster = lagGroup?.clusterId ?? clusters[0]?.id;

  const backend = services.filter((s) => s.type !== 'web' && s.type !== 'mobile');
  const eps = endpointsFor(service);
  const ep = eps.find((e) => `${e.method} ${e.route}` === endpoint);
  const svcLabel = services.find((s) => s.id === service)?.name ?? service;
  const below = metric === 'rpm';
  const fn = fns.find((f) => f.id === fnId);
  const ruleValue = (kind === 'infra' && INFRA_RATIO.has(infraMetric)) || (kind === 'faas' && faasMetric === 'error_rate') ? Number(ruleTh) / 100 : Number(ruleTh);
  const relative = kind === 'faas' && relTimeout && isDuration(faasMetric);
  const ruleUnit = kind === 'infra' ? (INFRA_RATIO.has(infraMetric) ? '%' : infraMetric === 'load1' ? 'load' : 'count') : relative ? '× timeout' : faasUnit(faasMetric);
  const ruleLabel = ruleUnit === '%' ? `${ruleTh}%` : ruleUnit === 'ms' ? ms(Number(ruleTh) || 0) : relative ? `${ruleTh} × timeout` : ruleTh;
  const infraFilter = Object.fromEntries(Object.entries({ cluster: fCluster, namespace: fNamespace, host: fHost, workload: fWorkload, service: fService }).map(([k, v]) => [k, v.trim()]).filter(([, v]) => v));
  const faasFilter = Object.fromEntries(Object.entries({ provider: fProvider, region: fRegion.trim(), service: fService.trim() }).filter(([, v]) => v));
  const templates = kind === 'infra' || kind === 'faas' ? monitorTemplates(kind) : [];
  /** Fill the form from a template (GET /monitor-templates); scope filters stay as picked. */
  const applyTemplate = (t: MonitorTemplate) => {
    const tq = t.query ?? {};
    const c = t.condition ?? {};
    const th = typeof c.threshold === 'number' ? c.threshold : 0;
    setTemplateId(t.id); setName(t.name);
    setOp(c.op === '<' ? '<' : '>'); setForMin(String(c.forMin ?? 0)); setCondWindow(typeof c.window === 'string' ? c.window : undefined);
    if (typeof c.minRequests === 'number') setMinReq(String(c.minRequests));
    if (t.severity === 'critical' || t.severity === 'warning' || t.severity === 'info') setSeverity(t.severity);
    if (t.kind === 'infra' && isInfraEntity(tq.entity)) {
      const m = (INFRA_METRICS[tq.entity] as string[]).includes(str(tq.metric)) ? tq.metric as InfraMetric : INFRA_METRICS[tq.entity][0];
      setEntity(tq.entity); setInfraMetric(m); if (tq.agg === 'avg' || tq.agg === 'max') setAgg(tq.agg);
      setRuleTh(String(INFRA_RATIO.has(m) ? +(th * 100).toFixed(2) : th));
    }
    if (t.kind === 'faas' && isFaasMetric(tq.metric)) {
      const rel = tq.relativeToTimeout === true;
      setFaasMetric(tq.metric); setRelTimeout(rel);
      setRuleTh(String(tq.metric === 'error_rate' ? +(th * 100).toFixed(3) : th));
    }
  };
  const thValue = metric === 'error_rate' ? Number(threshold) / 100 : Number(threshold);
  const zValue = Number(z);
  const every = Math.max(1, Number(everyMin) || 0);
  const grace = Math.max(0, Number(graceMin) || 0);
  // processes a missed-job monitor can watch: cron / consumer subscriptions of the service, else its routes
  const processes = eps.filter((e) => e.sub).map((e) => e.sub!.process);
  const candidates = monitors.filter((m) => m.id !== editing?.id && m.kind !== 'composite' && (!svcParam || m.target.startsWith(svcName)));
  const otherMonitors = monitors.filter((m) => m.id !== editing?.id && m.kind !== 'composite' && !candidates.includes(m));

  const rule = { forMin: Number(forMin) || 0, ...(Number(renotify) ? { renotifyMin: Number(renotify) } : {}) };
  const common = { severity, channels, source: editing?.source === 'suggestion' ? 'suggestion' : !editing && templateId && (kind === 'infra' || kind === 'faas') ? 'template' : 'manual' };
  const ruleExtra = { ...(condWindow ? { window: condWindow } : {}) };
  const autoName: Record<Kind, string> = {
    metric_threshold: `${svcLabel} ${ep ? `${ep.method} ${ep.route}` : ''} ${METRICS.find((m) => m.value === metric)!.label.toLowerCase()}`,
    anomaly: `${svcLabel} ${ep ? `${ep.method} ${ep.route}` : ''} ${METRICS.find((m) => m.value === metric)!.label.toLowerCase()} anomaly`,
    composite: `${compOp === 'and' ? 'All' : 'Any'} of ${picked.length} monitors`,
    job_missed: `${process || svcLabel} missed run`,
    error_new: `New errors in ${errScope === '*' ? 'any service' : services.find((s) => s.id === errScope)?.name ?? errScope}`,
    log_match: `${svcLabel} log query`, consumer_lag: `${svcLabel} consumer lag`,
    infra: `${INFRA_ENTITIES.find((e) => e.value === entity)!.label} ${INFRA_METRIC_LABEL[infraMetric].toLowerCase()}${fService || fCluster || fHost ? ` · ${fService || fCluster || fHost}` : ''}`,
    faas: `${fn && fnId !== '*' ? fn.name : 'Functions'} ${FAAS_METRICS.find((m) => m.value === faasMetric)!.label.toLowerCase()}`,
  };
  const finalName = (name.trim() || autoName[kind]).replace(/\s+/g, ' ').trim();
  const target = { service, endpoint: ep ? `${ep.method} ${ep.route}` : undefined };
  const bodies: Partial<Record<Kind, object>> = {
    metric_threshold: {
      name: finalName, kind: metric === 'p95' ? 'latency' : 'metric_threshold', target,
      query: metric === 'p95' ? { percentile: 'p95' } : { metric },
      condition: { op: below ? '<' : '>', threshold: Number.isFinite(thValue) ? thValue : 0, minRequests: Number(minReq) || 0, ...rule }, ...common,
    },
    anomaly: {
      name: finalName, kind: 'anomaly', target, query: { metric, baseline, direction, sensitivity: zValue },
      condition: { op: '>', threshold: Number.isFinite(zValue) ? zValue : 3, minRequests: Number(minReq) || 0, ...rule }, ...common,
    },
    composite: {
      name: finalName, kind: 'composite', target: { service: svcParam ?? undefined }, query: { op: compOp, monitorIds: picked },
      condition: { op: '>=', threshold: compOp === 'and' ? picked.length : 1, ...rule }, ...common,
    },
    job_missed: {
      name: finalName, kind: 'job_missed', target: { service, process: process || undefined },
      query: { expectedEveryMin: every, graceMin: grace },
      condition: { op: '>', threshold: every + grace, forMin: 0, ...(Number(renotify) ? { renotifyMin: Number(renotify) } : {}) }, ...common,
    },
    infra: {
      name: finalName, kind: 'infra', target: { service: infraFilter.service || undefined },
      query: { entity, metric: infraMetric, filter: infraFilter, agg },
      condition: { op, threshold: Number.isFinite(ruleValue) ? ruleValue : 0, ...ruleExtra, ...rule }, ...common,
    },
    faas: {
      name: finalName, kind: 'faas', target: { service: fnId !== '*' ? fn?.service : faasFilter.service || undefined },
      query: { ...(fnId !== '*' ? { functionId: fnId } : { filter: faasFilter }), metric: faasMetric, ...(relative ? { relativeToTimeout: true } : {}) },
      condition: { op, threshold: Number.isFinite(ruleValue) ? ruleValue : 0, ...ruleExtra, ...(faasMetric === 'error_rate' && Number(minReq) ? { minRequests: Number(minReq) } : {}), ...rule }, ...common,
    },
    error_new: {
      name: finalName, kind: 'error_new', target: { service: errScope === '*' ? undefined : errScope }, query: {},
      condition: { op: '>', threshold: 0, forMin: 0, ...(Number(renotify) ? { renotifyMin: Number(renotify) } : {}) }, ...common,
    },
  };
  const body = bodies[kind];

  const validate = (): string | undefined => {
    if (kind === 'metric_threshold' || kind === 'anomaly' || kind === 'job_missed') if (!service) return 'Pick a service.';
    if (kind === 'anomaly' && !(zValue > 0)) return 'Sensitivity must be a positive number of standard deviations.';
    if (kind === 'composite' && picked.length < 2) return 'Pick at least two monitors.';
    if (kind === 'job_missed' && !process.trim()) return 'Name the job or process that should report.';
    if ((kind === 'infra' || kind === 'faas') && !(Number.isFinite(ruleValue) && ruleTh.trim() !== '')) return 'Enter a numeric threshold.';
    if (kind === 'faas' && fnId !== '*' && !fn) return 'Pick a function.';
    return undefined;
  };

  const save = async () => {
    if (readOnly) return;
    if (!isApi || !SAVABLE.has(kind) || !body) return goBack();
    const v = validate();
    if (v) return setError(v);
    setSaving(true); setError(undefined);
    try {
      if (editing) await mutate('PUT', `/monitors/${seg(editing.id)}`, { ...body, status: editing.status === 'muted' ? 'muted' : 'active' }, ['/monitors']);
      else await mutate('POST', '/monitors', body, ['/monitors']);
      goBack();
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
    }
    setSaving(false);
  };

  const togglePick = (id: string) => setPicked((p) => (p.includes(id) ? p.filter((x) => x !== id) : [...p, id]));
  const title = managed ? editing!.name : editing ? `Edit ${editing.name}` : 'New monitor';

  return (
    <>
      <PageHeader title={title} crumbs={[...(svcParam ? [{ label: svcName, to: back }] : []), { label: editing ? (managed ? 'Monitor' : 'Edit monitor') : 'New monitor' }]}
        actions={<>
          {!readOnly && <span className="row secondary" style={{ fontSize: 'var(--text-sm)' }}>Sync to repo as YAML <Toggle label="Sync to repo" checked={yaml} onChange={setYaml} /></span>}
          <Button onClick={goBack}>{readOnly ? 'Back' : 'Cancel'}</Button>
          {!readOnly && <Button variant="primary" disabled={saving} onClick={() => void save()}>{editing ? 'Save monitor' : 'Create monitor'}</Button>}
        </>} />

      {managed && (
        <Callout icon={<Code2 size={16} color="var(--accent)" />}>
          <span className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}><Badge tone="accent" icon={<Lock />}>Managed in repository</Badge>
            <span className="mono secondary" style={{ fontSize: 'var(--text-sm)' }}>{editing?.codeRef ?? '.trooper/monitors/*.yaml'}</span></span>
          <div className="secondary" style={{ marginTop: 'var(--space-1)' }}>Change the YAML file and push to the default branch — the next analysis updates this monitor. It can’t be edited or deleted here.</div>
        </Callout>
      )}
      {!managed && !can('write') && <Callout icon={<Lock size={16} />}>{denied('write')} — monitors can’t be created or changed.</Callout>}
      {editing?.source === 'suggestion' && <Callout icon={<Sparkles size={16} color="var(--accent)" />}>Created from a code suggestion. Editing it here makes it a regular monitor.</Callout>}
      {error && <Callout icon={<Activity size={16} color="var(--status-critical)" />}>{error}</Callout>}

      <fieldset disabled={readOnly} style={{ border: 0, margin: 0, padding: 0, minWidth: 0 }}>
      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 420px) minmax(0, 1fr)', gap: 'var(--space-4)', alignItems: 'start' }}>
        <div className="stack">
          <Card title="1 · Type">
            <div className="stack" style={{ gap: 'var(--space-1)' }}>
              {KINDS.map((k) => (
                <button key={k.id} type="button" onClick={() => setKind(k.id)} className="card card--interactive" aria-pressed={kind === k.id}
                  style={{ flexDirection: 'row', gap: 'var(--space-3)', padding: 'var(--space-3)', alignItems: 'center', textAlign: 'left', borderColor: kind === k.id ? 'var(--accent)' : undefined, boxShadow: kind === k.id ? '0 0 0 1px var(--accent)' : undefined }}>
                  <k.Icon size={18} color={kind === k.id ? 'var(--accent)' : 'var(--text-muted)'} />
                  <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{k.label}</div><div className="muted" style={{ fontSize: 'var(--text-sm)' }}>{k.desc}</div></div>
                </button>
              ))}
            </div>
          </Card>

          {kind === 'consumer_lag' && (
            <Card title="Consumer lag is defined on the consumer group">
              <p className="secondary" style={{ marginTop: 0 }}>Lag comes from the broker (Kafka, RabbitMQ, SQS, Pub/Sub), per consumer group and topic. The lag alert editor picks the group, offers lag, time behind, oldest message, hot partition, growing-lag and rebalance conditions, and replays the last 24 hours.</p>
              {lagCluster
                ? <Button variant="primary" onClick={() => setLagOpen(true)}>Open lag alert editor</Button>
                : <span className="muted">Connect a message broker first (Infrastructure → Message queues).</span>}
            </Card>
          )}
          {lagOpen && lagCluster && <LagAlertEditor scope={{ clusterId: lagCluster, group: lagGroup?.id }} onClose={() => setLagOpen(false)}
            onSaved={() => { setLagOpen(false); if (svcParam) nav(back); else nav('/infrastructure/queues'); }} />}
          {isApi && kind === 'log_match' && (
            <Callout icon={<Sparkles size={16} />}>Log query monitors can’t be created from the editor yet — define them as YAML in the repository (<span className="mono">.trooper/monitors/</span>).</Callout>
          )}

          {(kind === 'metric_threshold' || kind === 'anomaly') && (
            <Card title="2 · Scope">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <Field label="Service"><Select value={service} onChange={(e) => { setService(e.target.value); setEndpoint('*'); }} options={backend.map((s) => ({ value: s.id, label: s.name }))} /></Field>
                <Field label="Endpoint"><Select value={ep ? endpoint : '*'} onChange={(e) => setEndpoint(e.target.value)}
                  options={[{ value: '*', label: 'All endpoints' }, ...eps.map((e) => ({ value: `${e.method} ${e.route}`, label: `${e.method} ${e.route}` }))]} /></Field>
                <Field label="Metric"><Select value={metric} onChange={(e) => { const m = e.target.value as Metric; setMetric(m); setThreshold(m === 'error_rate' ? '2' : m === 'p95' ? '600' : '100'); if (kind === 'anomaly') setDirection(m === 'rpm' ? 'down' : 'up'); }} options={METRICS} /></Field>
                <Field label="Name"><Input value={name} placeholder={finalName} onChange={(e) => setName(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {kind === 'metric_threshold' && (
            <Card title="3 · Condition">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                  <Field label={`${below ? 'Below' : 'Above'} (${unitOf(metric)})`}><Input mono value={threshold} onChange={(e) => setThreshold(e.target.value)} inputMode="decimal" /></Field>
                  <ForField value={forMin} onChange={setForMin} />
                </div>
                <Field label="Minimum requests" hint="Skip evaluation when traffic is too low to be meaningful."><Input mono value={minReq} onChange={(e) => setMinReq(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {kind === 'anomaly' && (
            <Card title="3 · Sensitivity">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <Field label="Sensitivity" hint="How far from the seasonal baseline, in standard deviations (z), counts as anomalous.">
                  <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
                    <Segmented label="Sensitivity" value={sensitivityOf(zValue) ?? ('' as Sensitivity)} onChange={(s) => setZ(String(Z[s]))}
                      options={[{ id: 'low', label: 'Low · 4σ' }, { id: 'medium', label: 'Medium · 3σ' }, { id: 'high', label: 'High · 2σ' }]} />
                    <div style={{ width: 'var(--space-12)' }}><Input mono aria-label="z threshold" style={{ width: '100%' }} value={z} onChange={(e) => setZ(e.target.value)} inputMode="decimal" /></div>
                  </div>
                </Field>
                <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                  <Field label="Baseline window" hint="Same hour of day / week is compared.">
                    <Select value={baseline} onChange={(e) => setBaseline(e.target.value)} options={[{ value: '1d', label: 'Last day' }, { value: '7d', label: 'Last 7 days' }, { value: '14d', label: 'Last 14 days' }, { value: '28d', label: 'Last 28 days' }]} />
                  </Field>
                  <ForField value={forMin} onChange={setForMin} />
                </div>
                <Field label="Direction"><Segmented label="Direction" value={direction} onChange={setDirection}
                  options={[{ id: 'up', label: 'Above baseline' }, { id: 'down', label: 'Below baseline' }, { id: 'both', label: 'Either' }]} /></Field>
                <Field label="Minimum requests"><Input mono value={minReq} onChange={(e) => setMinReq(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {kind === 'composite' && (
            <Card title="2 · Monitors" subtitle="Fires only on the combination — use it to cut noise from correlated alerts.">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <Field label="Fire when"><Segmented label="Operator" value={compOp} onChange={setCompOp}
                  options={[{ id: 'and', label: 'All are firing (AND)' }, { id: 'or', label: 'Any is firing (OR)' }]} /></Field>
                <div className="pick-list" role="group" aria-label="Monitors">
                  {[...candidates, ...otherMonitors].map((m) => (
                    <label key={m.id} className="pick-list__row">
                      <input type="checkbox" checked={picked.includes(m.id)} onChange={() => togglePick(m.id)} />
                      <div style={{ flex: 1, minWidth: 0 }}>
                        <div className="truncate">{m.name}</div>
                        <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{m.target}</div>
                      </div>
                      <StatusBadge status={m.state} label={m.state === 'ok' ? 'OK' : undefined} />
                    </label>
                  ))}
                  {candidates.length + otherMonitors.length === 0 && <div className="pick-list__row muted">No other monitors yet.</div>}
                </div>
                <Field label="Name"><Input value={name} placeholder={finalName} onChange={(e) => setName(e.target.value)} /></Field>
                <ForField value={forMin} onChange={setForMin} label="Held for" />
              </div>
            </Card>
          )}

          {kind === 'job_missed' && (
            <Card title="2 · Job">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <Field label="Service"><Select value={service} onChange={(e) => { setService(e.target.value); setProcess(''); }} options={backend.map((s) => ({ value: s.id, label: s.name }))} /></Field>
                <Field label="Process or route" hint="The cron job, worker process or heartbeat route that must report a success.">
                  <Input mono list="job-processes" value={process} placeholder={processes[0] ?? 'invoice-batch'} onChange={(e) => setProcess(e.target.value)} />
                </Field>
                <datalist id="job-processes">
                  {[...processes, ...eps.filter((e) => !e.sub).map((e) => `${e.method} ${e.route}`)].map((p) => <option key={p} value={p} />)}
                </datalist>
                <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                  <Field label="Expected every">
                    <Select value={everyMin} onChange={(e) => setEveryMin(e.target.value)} options={[
                      { value: '5', label: '5 minutes' }, { value: '15', label: '15 minutes' }, { value: '60', label: 'Hour' },
                      { value: '360', label: '6 hours' }, { value: '1440', label: 'Day' }, { value: '10080', label: 'Week' },
                    ]} />
                  </Field>
                  <Field label="Grace (min)" hint="Late runs inside the grace don’t alert."><Input mono value={graceMin} onChange={(e) => setGraceMin(e.target.value)} inputMode="numeric" /></Field>
                </div>
                <Field label="Name"><Input value={name} placeholder={finalName} onChange={(e) => setName(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {kind === 'error_new' && (
            <Card title="2 · Scope">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <Field label="Service" hint="Errors are grouped by fingerprint (type + top frames); the first event of an unseen group alerts.">
                  <Select value={errScope} onChange={(e) => setErrScope(e.target.value)}
                    options={[{ value: '*', label: 'All services' }, ...services.map((s) => ({ value: s.id, label: s.name }))]} />
                </Field>
                <Field label="Name"><Input value={name} placeholder={finalName} onChange={(e) => setName(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {kind === 'infra' && (
            <Card title="2 · Scope" subtitle="The worst matching entity decides; the incident lists the worst five.">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                {templates.length > 0 && (
                  <Field label="Template" hint={templates.find((t) => t.id === templateId)?.description}>
                    <div className="row" style={{ gap: 'var(--space-1)', flexWrap: 'wrap' }}>
                      {templates.map((t) => <Button key={t.id} size="sm" variant={t.id === templateId ? 'primary' : 'secondary'} title={t.description} onClick={() => applyTemplate(t)}>{t.name}</Button>)}
                    </div>
                  </Field>
                )}
                <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                  <Field label="Entity"><Select value={entity} onChange={(e) => { const v = e.target.value as InfraEntity; setEntity(v); setInfraMetric(INFRA_METRICS[v][0]); }} options={INFRA_ENTITIES} /></Field>
                  <Field label="Metric"><Select value={infraMetric} onChange={(e) => setInfraMetric(e.target.value as InfraMetric)} options={INFRA_METRICS[entity].map((m) => ({ value: m, label: INFRA_METRIC_LABEL[m] }))} /></Field>
                </div>
                <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                  <Field label="Service" hint="Pods / containers of this service.">
                    <Select value={fService} onChange={(e) => setFService(e.target.value)} options={[{ value: '', label: 'Any service' }, ...backend.map((s) => ({ value: s.id, label: s.name }))]} />
                  </Field>
                  {entity === 'host'
                    ? <Field label="Host"><Input mono value={fHost} placeholder="any" onChange={(e) => setFHost(e.target.value)} /></Field>
                    : <Field label="Cluster"><Input mono value={fCluster} placeholder="any" onChange={(e) => setFCluster(e.target.value)} /></Field>}
                </div>
                {entity !== 'host' && entity !== 'node' && (
                  <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                    <Field label="Namespace"><Input mono value={fNamespace} placeholder="any" onChange={(e) => setFNamespace(e.target.value)} /></Field>
                    <Field label="Workload"><Input mono value={fWorkload} placeholder="any" onChange={(e) => setFWorkload(e.target.value)} /></Field>
                  </div>
                )}
                <Field label="Name"><Input value={name} placeholder={finalName} onChange={(e) => setName(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {kind === 'faas' && (
            <Card title="2 · Scope">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                {templates.length > 0 && (
                  <Field label="Template" hint={templates.find((t) => t.id === templateId)?.description}>
                    <div className="row" style={{ gap: 'var(--space-1)', flexWrap: 'wrap' }}>
                      {templates.map((t) => <Button key={t.id} size="sm" variant={t.id === templateId ? 'primary' : 'secondary'} title={t.description} onClick={() => applyTemplate(t)}>{t.name}</Button>)}
                    </div>
                  </Field>
                )}
                <Field label="Function"><Select value={fnId} onChange={(e) => setFnId(e.target.value)}
                  options={[...fns.map((f) => ({ value: f.id, label: `${f.name} · ${f.provider === 'aws' ? 'Lambda' : 'Azure'} · ${f.region}` })), { value: '*', label: 'Every function matching a filter' }]} /></Field>
                {fnId === '*' && (
                  <div className="grid grid-3" style={{ gap: 'var(--space-2)' }}>
                    <Field label="Provider"><Select value={fProvider} onChange={(e) => setFProvider(e.target.value)} options={[{ value: '', label: 'Any' }, { value: 'aws', label: 'AWS' }, { value: 'azure', label: 'Azure' }]} /></Field>
                    <Field label="Region"><Input mono value={fRegion} placeholder="any" onChange={(e) => setFRegion(e.target.value)} /></Field>
                    <Field label="Service"><Input mono value={fService} placeholder="any" onChange={(e) => setFService(e.target.value)} /></Field>
                  </div>
                )}
                <Field label="Metric"><Select value={faasMetric} onChange={(e) => { const m = e.target.value as FaasMetric; setFaasMetric(m); setRelTimeout(false); setRuleTh(m === 'error_rate' ? '5' : isDuration(m) ? String(Math.round((fn?.timeoutS ?? 30) * 800)) : '0'); }} options={FAAS_METRICS} /></Field>
                <Field label="Name"><Input value={name} placeholder={finalName} onChange={(e) => setName(e.target.value)} /></Field>
              </div>
            </Card>
          )}

          {(kind === 'infra' || kind === 'faas') && (
            <Card title="3 · Condition">
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <div className="grid grid-2" style={{ gap: 'var(--space-2)' }}>
                  <Field label={`Value (${ruleUnit})`}>
                    <div className="row" style={{ gap: 'var(--space-1)' }}>
                      <Segmented label="Operator" value={op} onChange={setOp} options={[{ id: '>', label: 'Above' }, { id: '<', label: 'Below' }]} />
                      <Input mono aria-label="Threshold" value={ruleTh} onChange={(e) => setRuleTh(e.target.value)} inputMode="decimal" style={{ flex: 1, minWidth: 0 }} />
                    </div>
                  </Field>
                  <ForField value={forMin} onChange={setForMin} />
                </div>
                {kind === 'infra' && <Field label="Across matching entities"><Segmented label="Aggregation" value={agg} onChange={setAgg} options={[{ id: 'max', label: 'Worst (max)' }, { id: 'avg', label: 'Average' }]} /></Field>}
                {kind === 'faas' && isDuration(faasMetric) && (
                  <Field label="Compare" hint={relative ? 'Each function against its own timeout, e.g. 0.8 = 80 % of it. Functions without a timeout (Azure) are skipped.' : undefined}>
                    <Segmented label="Compare" value={relTimeout ? 'rel' : 'abs'} options={[{ id: 'abs', label: 'Milliseconds' }, { id: 'rel', label: 'Share of timeout' }]}
                      onChange={(v) => { setRelTimeout(v === 'rel'); setRuleTh(v === 'rel' ? '0.8' : String(Math.round((fn?.timeoutS ?? 30) * 800))); }} />
                  </Field>
                )}
                {kind === 'faas' && !relative && fn && fnId !== '*' && fn.timeoutS && isDuration(faasMetric) && (
                  <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>Timeout of {fn.name}: {fn.timeoutS} s · 80% = {ms(fn.timeoutS * 800)}</span>
                )}
                {condWindow && <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>Evaluated over a {condWindow} window (from the template).</span>}
              </div>
            </Card>
          )}

          {kind !== 'consumer_lag' && (
            <Card title={kind === 'metric_threshold' || kind === 'anomaly' || kind === 'infra' || kind === 'faas' ? '4 · Notify' : '3 · Notify'}>
              <div className="stack" style={{ gap: 'var(--space-3)' }}>
                <Field label="Severity"><Select value={severity} onChange={(e) => setSeverity(e.target.value)} options={[{ value: 'critical', label: 'Critical' }, { value: 'warning', label: 'Warning' }, { value: 'info', label: 'Info' }]} /></Field>
                <div className="field"><span className="field__label">Notify</span><NotifyPicker value={channels} onChange={setChannels} disabled={readOnly} /></div>
                <Field label="Remind while firing" hint="Notify the channels again until someone acknowledges."><Select value={renotify} onChange={(e) => setRenotify(e.target.value)}
                  options={[{ value: '0', label: 'Only once' }, { value: '30', label: 'Every 30 minutes' }, { value: '60', label: 'Every hour' }, { value: '240', label: 'Every 4 hours' }]} /></Field>
              </div>
            </Card>
          )}
        </div>

        <div className="stack" style={{ position: 'sticky', top: 'calc(var(--topbar-h) + var(--space-4))' }}>
          {kind === 'metric_threshold' && <ThresholdPreview body={body!} metric={metric} below={below} thValue={thValue} thLabel={metric === 'error_rate' ? `${threshold}%` : `${threshold} ${unitOf(metric)}`} />}
          {kind === 'anomaly' && <AnomalyPreview body={body!} metric={metric} z={zValue} direction={direction} seed={`${service}|${endpoint}|${metric}`} />}
          {kind === 'composite' && <CompositePreview picked={monitors.filter((m) => picked.includes(m.id))} op={compOp} />}
          {kind === 'job_missed' && <JobPreview body={body!} every={every} grace={grace} seed={`${service}|${process}`} />}
          {kind === 'error_new' && <ErrorNewPreview body={body!} seed={errScope} />}
          {(kind === 'infra' || kind === 'faas') && <RulePreview cap={(kind === 'infra' && INFRA_RATIO.has(infraMetric)) || (kind === 'faas' && faasMetric === 'error_rate') ? 1 : undefined} body={body!} seed={JSON.stringify(body)} threshold={ruleValue} below={op === '<'} thLabel={ruleLabel}
            label={kind === 'infra' ? INFRA_METRIC_LABEL[infraMetric] : FAAS_METRICS.find((m) => m.value === faasMetric)!.label}
            format={kind === 'infra' ? (INFRA_RATIO.has(infraMetric) ? (v: number) => pct(v, 0) : (v: number) => compact(v)) : relative ? (v: number) => `${v.toFixed(2)}×` : faasFmt(faasMetric)} />}
          {kind === 'metric_threshold' && !isApi && (
            <Card title={<span className="row"><Sparkles size={14} color="var(--accent)" />Recommendation</span>}>
              <p className="secondary">14-day p99 error rate for this endpoint is <strong>1.9%</strong>. A 2% threshold with a 5-minute window would have caught the 2 incidents on Oct 19 and Oct 22 with no false positives.</p>
            </Card>
          )}
        </div>
      </div>
      </fieldset>
    </>
  );
}

function ForField({ value, onChange, label = 'For' }: { value: string; onChange: (v: string) => void; label?: string }) {
  return (
    <Field label={label}><Select value={value} onChange={(e) => onChange(e.target.value)}
      options={[{ value: '0', label: 'Immediately' }, { value: '5', label: '5 minutes' }, { value: '10', label: '10 minutes' }, { value: '15', label: '15 minutes' }]} /></Field>
  );
}

// ---------------------------------------------------------------- previews

/** Api replay of the rule over the last 24 h (`POST /monitors/preview`); null until it lands or when the api can't preview the kind. */
function useApiPreview(body: object): Preview | null {
  return isApi ? postRemote<Preview | null>(`alert-editor:${JSON.stringify(body)}`, `/monitors/preview${qs({ range: '24h' })}`, body, null) : null;
}
function fit(d: number[], n: number) {
  return d.length >= n ? d.slice(d.length - n) : [...Array<number>(n - d.length).fill(0), ...d];
}

function PreviewCard({ value, hint, children }: { value: string; hint: ReactNode; children: ReactNode }) {
  return <MetricCard className="metric--hero" label="Preview · last 24 hours" value={value} hint={hint}>{children}</MetricCard>;
}
function NoPreview() {
  return <Card title="Preview · last 24 hours"><p className="muted" style={{ margin: 0 }}>The api can’t replay this monitor type yet — it is evaluated from the moment it is saved.</p></Card>;
}
const alerts = (n: number) => `${n} alert${n === 1 ? '' : 's'}`;

function ThresholdPreview({ body, metric, below, thValue, thLabel }: { body: object; metric: Metric; below: boolean; thValue: number; thLabel: string }) {
  const ts = timestamps('24h');
  const p = useApiPreview(body);
  let data: number[];
  let episodes: number;
  if (isApi) {
    data = fit(p?.data ?? [], ts.length);
    episodes = p?.episodes ?? 0;
  } else {
    data = series('editor', '24h', { base: MOCK_BASE.error_rate, noise: 0.35, spikeAt: 0.62, spikeMul: 3.2 });
    if (metric !== 'error_rate') data = series(`editor-${metric}`, '24h', { base: MOCK_BASE[metric], noise: 0.35, spikeAt: 0.62, spikeMul: metric === 'rpm' ? 0.3 : 3.2 });
    episodes = episodesOf(data.map((v) => (below ? v < thValue : v > thValue)));
  }
  const breaches = data.filter((v) => (below ? v < thValue : v > thValue)).length;
  return (
    <PreviewCard value={alerts(episodes)} hint={<>would have fired · {breaches} of {data.length} intervals {below ? 'below' : 'above'} {thLabel}</>}>
      <TimeSeries timestamps={ts} height={280} area format={fmtOf(metric)}
        threshold={Number.isFinite(thValue) ? { value: thValue, label: thLabel } : undefined}
        series={[{ id: 'er', label: METRICS.find((m) => m.value === metric)!.label, slot: 8, data }]} />
    </PreviewCard>
  );
}

function AnomalyPreview({ body, metric, z, direction, seed }: { body: object; metric: Metric; z: number; direction: 'up' | 'down' | 'both'; seed: string }) {
  const ts = timestamps('24h');
  const p = useApiPreview(body);
  if (isApi && !p) return <NoPreview />;
  // api: data = deviation in σ per bucket; mock: rolling z of a plausible metric curve
  const dev = isApi ? fit(p!.data, ts.length)
    : zScores(series(`anomaly-${seed}`, '24h', { base: MOCK_BASE[metric], noise: 0.3, spikeAt: 0.7, spikeMul: metric === 'rpm' ? 0.35 : 3, spikeWidth: 2 }));
  const shown = direction === 'down' ? dev.map((v) => -v) : direction === 'both' ? dev.map(Math.abs) : dev;
  const hits = shown.map((v) => v > z);
  const episodes = isApi ? p!.episodes : episodesOf(hits);
  return (
    <PreviewCard value={alerts(episodes)} hint={<>would have fired · {hits.filter(Boolean).length} of {shown.length} intervals beyond {z}σ {direction === 'down' ? 'below' : direction === 'both' ? 'from' : 'above'} the baseline</>}>
      <TimeSeries timestamps={ts} height={280} area format={(v) => `${v.toFixed(1)}σ`}
        threshold={Number.isFinite(z) ? { value: z, label: `${z}σ` } : undefined}
        series={[{ id: 'z', label: 'Deviation from baseline (σ)', slot: 7, data: shown.map((v) => Math.max(0, v)) }]} />
    </PreviewCard>
  );
}

function CompositePreview({ picked, op }: { picked: Monitor[]; op: 'and' | 'or' }) {
  const firing = picked.filter((m) => m.state === 'firing');
  const now = picked.length > 0 && (op === 'and' ? firing.length === picked.length : firing.length > 0);
  return (
    <Card title="Right now">
      {picked.length === 0 ? <p className="muted" style={{ margin: 0 }}>Pick the monitors to combine.</p> : (
        <div className="stack" style={{ gap: 'var(--space-3)' }}>
          <div className="row" style={{ gap: 'var(--space-2)' }}>
            <StatusBadge status={now ? 'firing' : 'ok'} label={now ? 'Would be firing' : 'Would be OK'} />
            <span className="secondary">{firing.length} of {picked.length} firing · {op === 'and' ? 'needs all' : 'needs any'}</span>
          </div>
          <div className="list">
            {picked.map((m) => (
              <div key={m.id} className="list__row">
                <div className="list__main"><span className="list__title">{m.name}</span><span className="list__meta">{m.condition}</span></div>
                <StatusBadge status={m.state} label={m.state === 'ok' ? 'OK' : undefined} />
              </div>
            ))}
          </div>
        </div>
      )}
    </Card>
  );
}

function JobPreview({ body, every, grace, seed }: { body: object; every: number; grace: number; seed: string }) {
  const ts = timestamps('24h');
  const p = useApiPreview(body);
  if (isApi && !p) return <NoPreview />;
  let runs: number[];
  let episodes: number;
  if (isApi) {
    runs = fit(p!.data, ts.length);
    episodes = p!.episodes;
  } else {
    // a run every `every` minutes; one outage around 60% of the window
    const step = ts[1] - ts[0];
    const gapFrom = ts[Math.floor(ts.length * 0.6)];
    const gapTo = gapFrom + Math.max(3, Math.ceil(((every + grace) * 60_000 * 1.5) / step)) * step;
    let h = 0; for (const c of seed) h = (h * 31 + c.charCodeAt(0)) >>> 0;
    const offset = (h % every) * 60_000;
    runs = ts.map((t) => {
      if (t >= gapFrom && t < gapTo) return 0;
      let n = 0;
      for (let x = Math.ceil((t - offset) / (every * 60_000)) * every * 60_000 + offset; x < t + step; x += every * 60_000) n++;
      return n;
    });
    const missed = (gapTo - gapFrom) > (every + grace) * 60_000;
    episodes = missed ? 1 : 0;
  }
  return (
    <PreviewCard value={alerts(episodes)} hint={<>would have fired · a run is expected every {every >= 60 ? `${every / 60} h` : `${every} min`} (+{grace} min grace)</>}>
      <Columns timestamps={ts} height={220} format={(v) => compact(v)} series={[{ id: 'runs', label: 'Successful runs', data: runs, color: 'var(--series-1)' }]} />
    </PreviewCard>
  );
}

function ErrorNewPreview({ body, seed }: { body: object; seed: string }) {
  const ts = timestamps('24h');
  const p = useApiPreview(body);
  if (isApi && !p) return <NoPreview />;
  const groups = isApi ? fit(p!.data, ts.length)
    : series(`err-new-${seed}`, '24h', { base: seed === '*' ? 0.35 : 0.12, noise: 0.9, spikeAt: 0.8, spikeMul: 8, spikeWidth: 1 }).map((v) => Math.floor(v * 1.6));
  const total = isApi ? p!.episodes : groups.reduce((a, v) => a + v, 0);
  return (
    <PreviewCard value={alerts(total)} hint="would have fired · one per new error group (first occurrence only)">
      <Columns timestamps={ts} height={220} format={(v) => compact(v)} series={[{ id: 'g', label: 'New error groups', data: groups, color: 'var(--series-8)' }]} />
    </PreviewCard>
  );
}

/** infra / faas: api replay when available, else a plausible curve around the threshold (mock). */
function RulePreview({ body, seed, threshold, below, thLabel, label, format, cap }: {
  body: object; seed: string; threshold: number; below: boolean; thLabel: string; label: string; format: (v: number) => string; cap?: number;
}) {
  const ts = timestamps('24h');
  const p = useApiPreview(body);
  if (isApi && !p) return <NoPreview />;
  const th = Number.isFinite(threshold) ? threshold : 0;
  const data = isApi ? fit(p!.data, ts.length)
    : series(`rule-${seed}`, '24h', { base: th > 0 ? th * (below ? 1.4 : 0.62) : 0.3, noise: 0.3, spikeAt: 0.7, spikeMul: below ? 0.4 : th > 0 ? 1.9 : 9, spikeWidth: 2 })
      .map((v) => (th === 0 || Number.isInteger(th) && th >= 1 && th < 50 ? Math.floor(v) : v))
      .map((v) => (cap === undefined ? v : Math.min(cap, v)));
  const hits = data.map((v) => (below ? v < th : v > th));
  const episodes = isApi ? p!.episodes : episodesOf(hits);
  return (
    <PreviewCard value={alerts(episodes)} hint={<>would have fired · {hits.filter(Boolean).length} of {data.length} intervals {below ? 'below' : 'above'} {thLabel}</>}>
      <TimeSeries timestamps={ts} height={280} area format={format} threshold={{ value: th, label: thLabel }}
        series={[{ id: 'v', label, slot: 1, data }]} />
    </PreviewCard>
  );
}
