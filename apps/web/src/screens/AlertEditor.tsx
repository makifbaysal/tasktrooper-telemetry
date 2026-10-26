import { useData } from '@/data/remote';
import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { Activity, Clock, FileText, Gauge, Sparkles, TrendingUp } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Button, Callout, Card, Field, Input, MetricCard, PageHeader, Select, Toggle } from '@/components/ui';
import { clusters, endpointsFor, groupsOfService, isApi, monitors, mutate, qs, seg, series, services, timestamps } from '@/data';
import { postRemote, type ApiMonitor } from '@/data/api/ops';
import { LagAlertEditor } from './LagAlerts';
import { compact, ms, pct } from '@/lib/format';

const KINDS = [
  { id: 'metric_threshold', label: 'Metric threshold', desc: 'Error rate, throughput, latency above/below a value', Icon: Activity },
  { id: 'anomaly', label: 'Anomaly', desc: 'Deviation from seasonal baseline', Icon: TrendingUp },
  { id: 'log_match', label: 'Log query', desc: 'Count of matching log lines', Icon: FileText },
  { id: 'consumer_lag', label: 'Consumer lag', desc: 'Queue backlog and DLQ', Icon: Gauge },
  { id: 'job_missed', label: 'Missed job', desc: 'Cron/heartbeat did not report', Icon: Clock },
] as const;

type Metric = 'error_rate' | 'p95' | 'rpm';
const METRICS: { value: Metric; label: string }[] = [{ value: 'error_rate', label: 'Error rate' }, { value: 'p95', label: 'p95 latency' }, { value: 'rpm', label: 'Throughput' }];
const unitOf = (m: Metric) => (m === 'error_rate' ? '%' : m === 'p95' ? 'ms' : 'req/min');
const fmtOf = (m: Metric) => (m === 'error_rate' ? (v: number) => pct(v) : m === 'p95' ? ms : (v: number) => compact(v));

interface Preview { timestamps: number[]; data: number[]; th: number; episodes: number; firingNow: boolean; now: number }

export function AlertEditor() {
  useData();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const editing = isApi ? (monitors.find((m) => m.id === params.get('edit')) as ApiMonitor | undefined) : undefined;
  const [kind, setKind] = useState<string>(params.get('kind') ?? 'metric_threshold');
  // monitors live under their service; go back to its Alerts tab
  const svcParam = params.get('service') ?? editing?.targetSpec?.service ?? null;
  const svcName = services.find((s) => s.id === svcParam)?.name ?? svcParam ?? '';
  const back = `/services/${svcParam}?tab=alerts`;
  const goBack = () => (svcParam ? nav(back) : nav(-1));

  const initMetric = (editing?.apiKind === 'latency' ? 'p95' : (editing?.query?.metric as Metric | undefined)) ?? 'error_rate';
  const [name, setName] = useState(editing?.name ?? '');
  const [service, setService] = useState(svcParam ?? services.find((s) => s.type !== 'web' && s.type !== 'mobile')?.id ?? '');
  const [endpoint, setEndpoint] = useState(editing?.targetSpec?.endpoint ?? params.get('endpoint') ?? '*');
  const [metric, setMetric] = useState<Metric>(initMetric);
  const [threshold, setThreshold] = useState(editing?.rule ? String(initMetric === 'error_rate' ? +(editing.rule.threshold * 100).toFixed(3) : editing.rule.threshold) : '2');
  const [forMin, setForMin] = useState(String(editing?.rule?.forMin ?? 5));
  const [minReq, setMinReq] = useState(String(editing?.rule?.minRequests ?? 50));
  const [renotify, setRenotify] = useState(String(editing?.rule?.renotifyMin ?? 0));
  const [severity, setSeverity] = useState<string>(editing?.severity ?? 'critical');
  const [channels, setChannels] = useState(editing?.channels.join(', ') ?? '#payments-alerts, PagerDuty: payments');
  const [yaml, setYaml] = useState(false);
  const [lagOpen, setLagOpen] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const lagGroup = svcParam ? groupsOfService(svcParam)[0] : undefined;
  const lagCluster = lagGroup?.clusterId ?? clusters[0]?.id;

  const eps = endpointsFor(service);
  const ep = eps.find((e) => `${e.method} ${e.route}` === endpoint);
  const below = metric === 'rpm';
  const thValue = metric === 'error_rate' ? Number(threshold) / 100 : Number(threshold);
  const fmt = fmtOf(metric);
  const thLabel = metric === 'error_rate' ? `${threshold}%` : `${threshold} ${unitOf(metric)}`;
  const body = {
    name: name.trim() || `${services.find((s) => s.id === service)?.name ?? service} ${ep ? `${ep.method} ${ep.route}` : ''} ${METRICS.find((m) => m.value === metric)!.label.toLowerCase()}`.replace(/\s+/g, ' ').trim(),
    kind: metric === 'p95' ? 'latency' : 'metric_threshold',
    target: { service, endpoint: ep ? `${ep.method} ${ep.route}` : undefined },
    query: metric === 'p95' ? { percentile: 'p95' } : { metric },
    condition: { op: below ? '<' : '>', threshold: Number.isFinite(thValue) ? thValue : 0, forMin: Number(forMin) || 0, minRequests: Number(minReq) || 0, ...(Number(renotify) ? { renotifyMin: Number(renotify) } : {}) },
    severity, channels: channels.split(',').map((s) => s.trim()).filter(Boolean), source: 'manual',
  };

  // preview: api replays the rule over the last 24 h; mock draws a plausible error-rate curve
  const ts = timestamps('24h');
  let data: number[];
  let episodes: number;
  let breaches: number;
  if (isApi) {
    const p = postRemote<Preview | null>(`alert-editor:${JSON.stringify(body)}`, `/monitors/preview${qs({ range: '24h' })}`, body, null);
    const d = p?.data ?? [];
    data = d.length >= ts.length ? d.slice(d.length - ts.length) : [...Array(ts.length - d.length).fill(0), ...d];
    episodes = p?.episodes ?? 0;
    breaches = data.filter((v) => (below ? v < thValue : v > thValue)).length;
  } else {
    data = series('editor', '24h', { base: 0.009, noise: 0.35, spikeAt: 0.62, spikeMul: 3.2 });
    breaches = data.filter((v) => v > thValue).length;
    // count separate breach episodes, not intervals
    episodes = data.filter((v, i) => v > thValue && !(data[i - 1] > thValue)).length;
  }

  const save = async () => {
    if (!isApi || kind !== 'metric_threshold') return goBack();
    if (!service) return setError('Pick a service.');
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

  return (
    <>
      <PageHeader title={editing ? `Edit ${editing.name}` : 'New monitor'} crumbs={[...(svcParam ? [{ label: svcName, to: back }] : []), { label: editing ? 'Edit monitor' : 'New monitor' }]}
        actions={<>
          <span className="row secondary" style={{ fontSize: 'var(--text-sm)' }}>Sync to repo as YAML <Toggle label="Sync to repo" checked={yaml} onChange={setYaml} /></span>
          <Button onClick={goBack}>Cancel</Button>
          <Button variant="primary" disabled={saving} onClick={() => void save()}>{editing ? 'Save monitor' : 'Create monitor'}</Button>
        </>} />

      {error && <Callout icon={<Activity size={16} color="var(--status-critical)" />}>{error}</Callout>}

      <div style={{ display: 'grid', gridTemplateColumns: 'minmax(0, 420px) minmax(0, 1fr)', gap: 'var(--space-4)', alignItems: 'start' }}>
        <div className="stack">
          <Card title="1 · Type">
            <div className="stack" style={{ gap: 6 }}>
              {KINDS.map((k) => (
                <button key={k.id} onClick={() => setKind(k.id)} className="card card--interactive"
                  style={{ flexDirection: 'row', gap: 12, padding: 12, alignItems: 'center', textAlign: 'left', borderColor: kind === k.id ? 'var(--accent)' : undefined, boxShadow: kind === k.id ? '0 0 0 1px var(--accent)' : undefined }}>
                  <k.Icon size={18} color={kind === k.id ? 'var(--accent)' : 'var(--text-muted)'} />
                  <div><div style={{ fontWeight: 500 }}>{k.label}</div><div className="muted" style={{ fontSize: 'var(--text-sm)' }}>{k.desc}</div></div>
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
          {isApi && kind !== 'metric_threshold' && kind !== 'consumer_lag' && (
            <Callout icon={<Sparkles size={16} />}>This monitor type can't be created from the editor yet — use a metric threshold, or define it as YAML in the repository.</Callout>
          )}
          <Card title="2 · Scope">
            <div className="stack" style={{ gap: 12 }}>
              <Field label="Service"><Select value={service} onChange={(e) => { setService(e.target.value); setEndpoint('*'); }}
                options={services.filter((s) => s.type !== 'web' && s.type !== 'mobile').map((s) => ({ value: s.id, label: s.name }))} /></Field>
              <Field label="Endpoint"><Select value={ep ? endpoint : '*'} onChange={(e) => setEndpoint(e.target.value)}
                options={[{ value: '*', label: 'All endpoints' }, ...eps.map((e) => ({ value: `${e.method} ${e.route}`, label: `${e.method} ${e.route}` }))]} /></Field>
              <Field label="Metric"><Select value={metric} onChange={(e) => { const m = e.target.value as Metric; setMetric(m); setThreshold(m === 'error_rate' ? '2' : m === 'p95' ? '600' : '100'); }} options={METRICS} /></Field>
              <Field label="Name"><Input value={name} placeholder={body.name} onChange={(e) => setName(e.target.value)} /></Field>
            </div>
          </Card>
          <Card title="3 · Condition">
            <div className="stack" style={{ gap: 12 }}>
              <div className="grid grid-2" style={{ gap: 8 }}>
                <Field label={`${below ? 'Below' : 'Above'} (${unitOf(metric)})`}><Input mono value={threshold} onChange={(e) => setThreshold(e.target.value)} inputMode="decimal" /></Field>
                <Field label="For"><Select value={forMin} onChange={(e) => setForMin(e.target.value)} options={[{ value: '5', label: '5 minutes' }, { value: '10', label: '10 minutes' }, { value: '15', label: '15 minutes' }]} /></Field>
              </div>
              <Field label="Minimum requests" hint="Skip evaluation when traffic is too low to be meaningful."><Input mono value={minReq} onChange={(e) => setMinReq(e.target.value)} /></Field>
              <Field label="Severity"><Select value={severity} onChange={(e) => setSeverity(e.target.value)} options={[{ value: 'critical', label: 'Critical' }, { value: 'warning', label: 'Warning' }, { value: 'info', label: 'Info' }]} /></Field>
              <Field label="Notify"><Input value={channels} onChange={(e) => setChannels(e.target.value)} /></Field>
              <Field label="Remind while firing" hint="Notify the channels again until someone acknowledges."><Select value={renotify} onChange={(e) => setRenotify(e.target.value)}
                options={[{ value: '0', label: 'Only once' }, { value: '30', label: 'Every 30 minutes' }, { value: '60', label: 'Every hour' }, { value: '240', label: 'Every 4 hours' }]} /></Field>
            </div>
          </Card>
        </div>

        <div className="stack" style={{ position: 'sticky', top: 'calc(var(--topbar-h) + 16px)' }}>
          <MetricCard className="metric--hero" label="Preview · last 24 hours"
            value={`${episodes} alert${episodes === 1 ? '' : 's'}`}
            hint={<>would have fired · {breaches} of {data.length} intervals {below ? 'below' : 'above'} {thLabel}</>}>
            <TimeSeries timestamps={ts} height={280} area format={isApi ? fmt : (v) => pct(v)}
              threshold={Number.isFinite(thValue) ? { value: thValue, label: thLabel } : undefined}
              series={[{ id: 'er', label: METRICS.find((m) => m.value === metric)!.label, slot: 8, data }]} />
          </MetricCard>
          {!isApi && (
            <Card title={<span className="row"><Sparkles size={14} color="var(--accent)" />Recommendation</span>}>
              <p className="secondary">14-day p99 error rate for this endpoint is <strong>1.9%</strong>. A 2% threshold with a 5-minute window would have caught the 2 incidents on Oct 19 and Oct 22 with no false positives.</p>
            </Card>
          )}
        </div>
      </div>
    </>
  );
}
