import { useState } from 'react';
import { Bell, Plus, Sparkles } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Badge, Button, Card, Drawer, DrawerSection, EmptyState, Field, Input, Select, StatusBadge, Table } from '@/components/ui';
import { BROKER_LABEL, brokerTerms, clusterOf, clusters, consumerGroups, services, timestamps, type Monitor, type Severity } from '@/data';
import { useInfra } from '@/lib/infra';
import {
  addLagMonitor, evaluate, formatRuleValue, lagMetricsFor, lagMonitorsOf, metricMeta, recommend, type LagMetric, type LagRule,
} from '@/lib/lagAlerts';

export interface LagAlertScope { clusterId: string; group?: string; topic?: string }

/** Lag alerts of a cluster or one consumer group, with "New lag alert". */
export function LagAlertList({ clusterId, group, compact: small }: { clusterId?: string; group?: string; compact?: boolean }) {
  useInfra();
  const [editing, setEditing] = useState<LagAlertScope | null>(null);
  const rows = (clusterId ? lagMonitorsOf(clusterId, group) : clusters.flatMap((c) => lagMonitorsOf(c.id)));
  const firstCluster = clusterId ?? clusters[0]?.id;
  if (!firstCluster) return <Card><EmptyState icon={<Bell />} title="No brokers connected">Connect a Kafka or RabbitMQ broker (or SQS / Pub/Sub through a cloud integration) to alert on consumer lag.</EmptyState></Card>;
  const scope: LagAlertScope = { clusterId: firstCluster, group };
  const add = <Button size="sm" variant={small ? 'secondary' : 'primary'} icon={<Plus size={14} />} onClick={() => setEditing(scope)}>New lag alert</Button>;
  return (
    <>
      {!small && <div className="row" style={{ justifyContent: 'flex-end', marginBottom: 'var(--space-3)' }}>{add}</div>}
      {rows.length === 0
        ? <Card><EmptyState icon={<Bell />} title="No lag alerts yet" action={small ? add : undefined}>Get notified when a consumer falls behind — before messages expire or users notice.</EmptyState></Card>
        : <div className="card" style={{ overflow: 'hidden' }}>
            <Table rows={rows} rowKey={(m) => m.id} columns={[
              { key: 'state', header: 'State', width: 110, render: (m) => <StatusBadge status={m.state} label={m.state === 'ok' ? 'OK' : undefined} /> },
              { key: 'name', header: 'Alert', render: (m) => (
                <div><div style={{ fontWeight: 500 }}>{m.name}</div><div className="muted mono" style={{ fontSize: 'var(--text-xs)' }}>{m.queue!.group}</div></div>) },
              { key: 'cond', header: 'Condition', render: (m) => <span className="mono" style={{ fontSize: 'var(--text-sm)' }}>{m.condition}</span> },
              { key: 'now', header: 'Now', numeric: true, render: (m) => formatRuleValue(m.queue!.metric, evaluate(m.queue!, '1h').now) },
              { key: 'sev', header: 'Severity', render: (m) => <Badge tone={m.severity === 'critical' ? 'critical' : m.severity === 'warning' ? 'warning' : 'accent'}>{m.severity}</Badge> },
              { key: 'ch', header: 'Notifies', render: (m) => <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{m.channels.join(', ')}</span> },
            ]} />
          </div>}
      {small && rows.length > 0 && <div style={{ marginTop: 'var(--space-2)' }}>{add}</div>}
      {editing && <LagAlertEditor scope={editing} onClose={() => setEditing(null)} onSaved={() => setEditing(null)} />}
    </>
  );
}

/** Button that opens the editor pre-scoped (consumer Lag tab, topic rows…). */
export function LagAlertButton({ scope, label = 'Alert on lag' }: { scope: LagAlertScope; label?: string }) {
  const [open, setOpen] = useState(false);
  return <>
    <Button size="sm" variant="ghost" icon={<Bell size={14} />} onClick={() => setOpen(true)}>{label}</Button>
    {open && <LagAlertEditor scope={scope} onClose={() => setOpen(false)} onSaved={() => setOpen(false)} />}
  </>;
}

const SEVERITIES: { value: Severity; label: string }[] = [{ value: 'critical', label: 'Critical' }, { value: 'warning', label: 'Warning' }, { value: 'info', label: 'Info' }];

/**
 * Define a lag alert on a consumer group: pick the group and topic, a condition, a threshold.
 * The preview replays the last 24 h and says how often it would have fired.
 */
export function LagAlertEditor({ scope, onClose, onSaved }: { scope: LagAlertScope; onClose: () => void; onSaved: (m: Monitor) => void }) {
  const [clusterId, setClusterId] = useState(scope.clusterId);
  const groupsHere = consumerGroups.filter((g) => g.clusterId === clusterId);
  const [group, setGroup] = useState(scope.group ?? groupsHere[0]?.id ?? '');
  const g = groupsHere.find((x) => x.id === group);
  const [topic, setTopic] = useState(scope.topic ?? '');
  const rec = g ? recommend(g, topic || undefined) : undefined;
  const [metric, setMetric] = useState<LagMetric>(rec?.rule.metric ?? 'time_behind');
  const [threshold, setThreshold] = useState(String(rec?.rule.threshold ?? 60));
  const [forMin, setForMin] = useState(String(rec?.rule.forMin ?? 10));
  const [severity, setSeverity] = useState<Severity>('warning');
  const svc = services.find((s) => s.id === g?.serviceId);
  const [channels, setChannels] = useState(svc ? `#${svc.name.split('-')[0]}-oncall` : '#platform');
  const [name, setName] = useState('');
  const c = clusterOf(clusterId);

  const rule: LagRule = { clusterId, group, topic: topic || undefined, metric, threshold: Number(threshold) || 0, forMin: Number(forMin) || 0 };
  const meta = metricMeta(metric);
  const ev = g ? evaluate(rule, '24h') : undefined;
  const autoName = `${group} · ${meta.label.toLowerCase()}${meta.needsThreshold ? ` ${threshold}${meta.unit === 'msgs' ? '' : ` ${meta.unit}`}` : ''}`;
  const pickGroup = (id: string) => {
    setGroup(id); setTopic('');
    const ng = groupsHere.find((x) => x.id === id);
    if (ng) { const r = recommend(ng); setMetric(r.rule.metric); setThreshold(String(r.rule.threshold)); setForMin(String(r.rule.forMin)); }
  };
  const applyRec = () => { if (rec) { setMetric(rec.rule.metric); setThreshold(String(rec.rule.threshold)); setForMin(String(rec.rule.forMin)); } };
  const metrics = lagMetricsFor(c?.kind);
  const terms = brokerTerms(c?.kind ?? 'kafka');
  const save = () => onSaved(addLagMonitor({ rule, name: name.trim() || autoName, severity, channels: channels.split(',').map((s) => s.trim()).filter(Boolean) }));

  return (
    <Drawer open onClose={onClose} width={720} title="New lag alert"
      subtitle={c ? <span className="row" style={{ gap: 'var(--space-2)' }}><Badge tone="accent">{BROKER_LABEL[c.kind]} · {c.name}</Badge>{svc && <span className="muted">also listed under {svc.name} → Alerts</span>}</span> : undefined}
      actions={<Button variant="primary" disabled={!g || (meta.needsThreshold && !(Number(threshold) > 0))} onClick={save}>Create alert</Button>}>
      <div className="stack">
        <div className="grid grid-3">
          <Field label="Cluster">
            <Select value={clusterId} onChange={(e) => { setClusterId(e.target.value); const first = consumerGroups.find((x) => x.clusterId === e.target.value); if (first) pickGroup(first.id); else setGroup(''); }}
              options={clusters.map((x) => ({ value: x.id, label: x.name }))} />
          </Field>
          <Field label={terms.group}>
            <Select value={group} onChange={(e) => pickGroup(e.target.value)} options={groupsHere.map((x) => ({ value: x.id, label: x.id }))} />
          </Field>
          <Field label={terms.topic}>
            <Select value={topic} onChange={(e) => setTopic(e.target.value)}
              options={[{ value: '', label: g && g.reads.length > 1 ? `All ${g.reads.length} it reads` : (g?.reads[0]?.topic ?? '—') }, ...(g && g.reads.length > 1 ? g.reads.map((r) => ({ value: r.topic, label: r.topic })) : [])]} />
          </Field>
        </div>

        <Field label="Condition">
          <div className="stack" style={{ gap: 6 }}>
            {metrics.map((m) => (
              <label key={m.id} className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-start', padding: 'var(--space-2) var(--space-3)', border: '1px solid', borderColor: metric === m.id ? 'var(--accent)' : 'var(--border)', borderRadius: 'var(--radius-md)', cursor: 'pointer', background: metric === m.id ? 'var(--accent-soft)' : undefined }}>
                <input type="radio" name="lag-metric" checked={metric === m.id} onChange={() => setMetric(m.id)} style={{ marginTop: 3 }} />
                <span><span style={{ fontWeight: 500 }}>{m.label}</span><span className="muted" style={{ display: 'block', fontSize: 'var(--text-sm)' }}>{m.desc}</span></span>
              </label>
            ))}
          </div>
        </Field>

        <div className="grid grid-3">
          {meta.needsThreshold
            ? <Field label={`Threshold${meta.unit ? ` (${meta.unit})` : ''}`}><Input mono inputMode="decimal" value={threshold} onChange={(e) => setThreshold(e.target.value)} /></Field>
            : <Field label="Threshold"><Input mono value="> 0 msgs/min" disabled /></Field>}
          {metric !== 'not_drained'
            ? <Field label="For (minutes)" hint="Must hold this long before it fires."><Input mono inputMode="numeric" value={forMin} onChange={(e) => setForMin(e.target.value)} /></Field>
            : <div />}
          <Field label="Severity"><Select value={severity} onChange={(e) => setSeverity(e.target.value as Severity)} options={SEVERITIES} /></Field>
        </div>

        {rec && (
          <Card title={<span className="row" style={{ gap: 6 }}><Sparkles size={14} color="var(--accent)" />Recommended for {g!.id}</span>}
            actions={<Button size="sm" onClick={applyRec}>Use it</Button>}>
            <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>
              <strong>{metricMeta(rec.rule.metric).label} {metricMeta(rec.rule.metric).needsThreshold ? formatRuleValue(rec.rule.metric, rec.rule.threshold) : ''}{rec.rule.forMin ? ` for ${rec.rule.forMin} min` : ''}</strong> — {rec.why}
            </p>
          </Card>
        )}

        {ev && (
          <DrawerSection title="Preview · last 24 hours"
            aside={<span className="row" style={{ gap: 'var(--space-2)' }}>
              {ev.firingNow ? <StatusBadge status="firing" label="Would fire now" /> : <StatusBadge status="ok" label="Quiet now" />}
              <span className="secondary">would have fired <strong>{ev.episodes}×</strong></span>
            </span>}>
            <TimeSeries timestamps={timestamps('24h')} height={180} area format={(v) => formatRuleValue(metric, v)}
              threshold={{ value: ev.th, label: metric === 'growing' ? 'growing' : formatRuleValue(metric, ev.th) }}
              series={[{ id: 'v', label: meta.label.replace(/ (above|within)$/, ''), slot: metric === 'growing' ? 7 : 1, data: ev.data }]} />
            <div className="muted" style={{ fontSize: 'var(--text-sm)', marginTop: 'var(--space-2)' }}>Now: {formatRuleValue(metric, ev.now)}</div>
          </DrawerSection>
        )}

        <div className="grid grid-2">
          <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder={autoName} /></Field>
          <Field label="Notify" hint="Comma-separated: Slack channels, PagerDuty services."><Input value={channels} onChange={(e) => setChannels(e.target.value)} /></Field>
        </div>
      </div>
    </Drawer>
  );
}

