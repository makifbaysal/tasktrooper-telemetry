import {
  RANGE_POINTS, clusterOf, consumerGroups, isApi, isQueueKind, lagSeries, monitors, mutate, qs, readLag, services, timestamps, topicOf,
  type BrokerKind, type ConsumerGroup, type GroupRead, type Monitor, type QueueRule, type Severity, type TimeRange,
} from '@/data';
import { postRemote } from '@/data/api/ops';
import { compact } from '@/lib/format';
import { emit } from './infra';

/** QueueRule metrics plus `oldest_age` (seconds the oldest waiting message has been in a queue — SQS / Pub/Sub / RabbitMQ). */
export type LagMetric = QueueRule['metric'] | 'oldest_age';
/** A QueueRule whose metric may be `oldest_age` (types.ts QueueRule doesn't list it yet). */
export type LagRule = Omit<QueueRule, 'metric'> & { metric: LagMetric };
const asRule = (r: LagRule) => r as QueueRule;

export const LAG_METRICS: { id: LagMetric; label: string; unit: string; desc: string; needsThreshold: boolean; kafkaOnly?: boolean; queueOnly?: boolean }[] = [
  { id: 'lag', label: 'Lag above', unit: 'msgs', needsThreshold: true, desc: 'Messages waiting. Simple, but the same number means different things at 10 msg/s and 1,000 msg/s.' },
  { id: 'time_behind', label: 'Time behind above', unit: 's', needsThreshold: true, desc: 'Lag ÷ produce rate: how old the oldest unread message roughly is. Holds steady when traffic grows — usually the best one.' },
  { id: 'oldest_age', label: 'Oldest message older than', unit: 's', needsThreshold: true, queueOnly: true, desc: 'How long the oldest waiting message has been in the queue (SQS ApproximateAgeOfOldestMessage, Pub/Sub oldest_unacked_message_age; broker_lag_1m.oldest_age_s). Catches a stuck consumer even when the queue is short.' },
  { id: 'partition_lag', label: 'Any partition above', unit: 'msgs', needsThreshold: true, kafkaOnly: true, desc: 'Catches a single hot or stuck partition that the total hides.' },
  { id: 'growing', label: 'Lag keeps growing', unit: '', needsThreshold: false, desc: 'Lag rose during the whole window: consumers are slower than producers, whatever the size.' },
  { id: 'not_drained', label: 'Not drained within', unit: 'h', needsThreshold: true, desc: 'For batch consumers whose lag climbs and resets: fires when it has not come back near zero in time.' },
  { id: 'rebalances', label: 'Rebalances above', unit: '/ h', needsThreshold: true, kafkaOnly: true, desc: 'Members leaving and joining. Every rebalance pauses the group\'s partitions.' },
];
export const metricMeta = (m: LagMetric) => LAG_METRICS.find((x) => x.id === m) ?? LAG_METRICS[0];
/** Metrics that make sense on a broker: partitions and rebalances are Kafka-only; message age is reported for SQS / Pub/Sub. */
export const lagMetricsFor = (kind: BrokerKind | undefined) =>
  LAG_METRICS.filter((m) => !(m.kafkaOnly && kind && isQueueKind(kind)) && !(m.queueOnly && kind !== 'sqs' && kind !== 'pubsub'));

export const groupOf = (r: Pick<QueueRule, 'clusterId' | 'group'>) => consumerGroups.find((g) => g.clusterId === r.clusterId && g.id === r.group);
const readsOf = (g: ConsumerGroup, topic?: string) => g.reads.filter((x) => !topic || x.topic === topic);

const stepMin = (range: TimeRange) => RANGE_POINTS[range].stepMs / 60_000;

/** The value the rule compares, over time. */
export function ruleSeries(r: LagRule, range: TimeRange): number[] {
  if (isApi) return evaluate(r, range).data;
  const g = groupOf(r);
  const n = timestamps(range).length;
  if (!g) return Array(n).fill(0);
  const reads = readsOf(g, r.topic);
  const sum = (f: (x: GroupRead) => number[]) => reads.map(f).reduce((a, s) => a.map((v, i) => v + s[i]), Array(n).fill(0));
  const lag = sum((x) => lagSeries(g, x, range));
  switch (r.metric) {
    case 'lag': return lag;
    case 'time_behind': {
      const rate = reads.reduce((a, x) => a + (topicOf(g.clusterId, x.topic)?.inPerSec ?? 0), 0) || 1;
      return lag.map((v) => v / rate);
    }
    case 'partition_lag': {
      const total = reads.reduce((a, x) => a + readLag(x), 0) || 1;
      const max = Math.max(...reads.flatMap((x) => x.partitionLag));
      return lag.map((v) => (v * max) / total);
    }
    case 'growing': {
      // trend of the smoothed lag (msgs/min), so single noisy points don't count as growth
      const w = 5;
      const smooth = lag.map((_, i) => { const xs = lag.slice(Math.max(0, i - w + 1), i + 1); return xs.reduce((x, y) => x + y, 0) / xs.length; });
      const k = 4;
      return smooth.map((v, i) => (i < k + w ? 0 : (v - smooth[i - k]) / (k * stepMin(range))));
    }
    case 'not_drained': {
      // hours since lag was last below 5% of its peak
      const peak = Math.max(...lag) || 1;
      let since = 0;
      return lag.map((v) => { since = v < peak * 0.05 ? 0 : since + stepMin(range) / 60; return since; });
    }
    case 'oldest_age': {
      // the age follows the backlog: scale the reported oldest age by the lag curve
      const total = reads.reduce((a, x) => a + readLag(x), 0) || 1;
      const age = Math.max(0, ...reads.map((x) => x.oldestAgeS ?? readLag(x) / (x.consumePerSec || 1)));
      return lag.map((v) => (age * v) / total);
    }
    case 'rebalances': return lag.map((_, i) => Math.max(0, g.rebalancesPerHour * (0.7 + 0.6 * (((i * 7919) % 13) / 13))));
  }
}

/** Threshold drawn on the preview; `growing` fires above 0 msgs/min. */
export const ruleThreshold = (r: LagRule) => (r.metric === 'growing' ? 0 : r.threshold);

export function formatRuleValue(m: LagMetric, v: number): string {
  if (m === 'time_behind' || m === 'oldest_age') return v < 90 ? `${Math.round(v)} s` : v < 5400 ? `${Math.round(v / 60)} min` : `${(v / 3600).toFixed(1)} h`;
  if (m === 'not_drained') return `${v.toFixed(1)} h`;
  if (m === 'growing') return `${v >= 0 ? '+' : ''}${compact(v)}/min`;
  if (m === 'rebalances') return `${v.toFixed(1)}/h`;
  return compact(v);
}

export interface Evaluation { data: number[]; th: number; episodes: number; firingNow: boolean; now: number }

/** MonitorInput for a lag rule (POST /monitors, /monitors/preview — services/alerting/README.md QueueRule). */
function lagMonitorBody(r: LagRule, extra: { name?: string; severity?: Severity; channels?: string[]; service?: string; process?: string } = {}) {
  const rule = { clusterId: r.clusterId, group: r.group, topic: r.topic || undefined, metric: r.metric, threshold: r.threshold, forMin: r.forMin };
  return {
    name: extra.name ?? 'preview', kind: 'consumer_lag', severity: extra.severity ?? 'warning', channels: extra.channels ?? [], source: 'manual',
    target: { service: extra.service, process: extra.process },
    condition: { op: '>', threshold: ruleThreshold(r), forMin: r.forMin },
    queue: rule, query: rule,
  };
}

/** Breach episodes in the series: values above the threshold for at least `forMin`. Api mode: POST /monitors/preview. */
export function evaluate(r: LagRule, range: TimeRange): Evaluation {
  if (isApi) {
    const n = timestamps(range).length;
    const fallback: Evaluation = { data: Array(n).fill(0), th: ruleThreshold(r), episodes: 0, firingNow: false, now: 0 };
    const body = lagMonitorBody(r);
    const out = postRemote<Evaluation>(`lag-preview:${range}:${JSON.stringify(body.queue)}`, `/monitors/preview${qs({ range })}`, body, fallback);
    const d = out.data ?? [];
    return { ...out, data: d.length >= n ? d.slice(d.length - n) : [...Array(n - d.length).fill(0), ...d] };
  }
  const data = ruleSeries(r, range);
  const th = ruleThreshold(r);
  const need = Math.max(1, Math.ceil(r.forMin / stepMin(range)));
  let run = 0, episodes = 0, firingNow = false;
  data.forEach((v, i) => {
    run = v > th ? run + 1 : 0;
    if (run === need) episodes++;
    if (i === data.length - 1) firingNow = run >= need;
  });
  return { data, th, episodes, firingNow, now: data[data.length - 1] };
}

export function describeRule(r: LagRule): string {
  const m = metricMeta(r.metric);
  const scope = r.topic ? ` on ${r.topic}` : '';
  if (r.metric === 'oldest_age') return `oldest message > ${formatRuleValue('oldest_age', r.threshold)}${r.forMin ? ` for ${r.forMin}m` : ''}${scope}`;
  if (r.metric === 'growing') return `lag growing for ${r.forMin}m${scope}`;
  if (r.metric === 'not_drained') return `lag not drained in ${r.threshold}h${scope}`;
  const v = r.metric === 'lag' || r.metric === 'partition_lag' ? compact(r.threshold) : `${r.threshold}${m.unit === 's' ? 's' : ''}`;
  const what = r.metric === 'lag' ? 'lag' : r.metric === 'partition_lag' ? 'max partition lag' : r.metric === 'time_behind' ? 'time behind' : 'rebalances';
  return `${what} > ${v}${r.metric === 'rebalances' ? '/h' : ''}${r.forMin ? ` for ${r.forMin}m` : ''}${scope}`;
}

const nice = (v: number) => {
  if (v <= 0) return 0;
  const p = 10 ** Math.floor(Math.log10(v));
  return Math.ceil(v / p) * p;
};

/** A threshold that would have stayed quiet on a normal day, plus why. */
export function recommend(g: ConsumerGroup, topic?: string): { rule: Omit<LagRule, 'clusterId' | 'group' | 'topic'>; why: string } {
  const kind = clusterOf(g.clusterId)?.kind;
  if (kind && isQueueKind(kind)) {
    const reads = g.reads.filter((x) => !topic || x.topic === topic);
    const age = Math.max(0, ...reads.map((x) => x.oldestAgeS ?? 0));
    const th = Math.max(300, nice(age * 3));
    return {
      rule: { metric: 'oldest_age', threshold: th, forMin: 10 },
      why: `On a ${kind === 'pubsub' ? 'subscription' : 'queue'} the age of the oldest waiting message is the signal users feel: it is ~${formatRuleValue('oldest_age', age)} now. ${formatRuleValue('oldest_age', th)} for 10 minutes fires on a stuck or too-slow consumer without paging on short bursts.`,
    };
  }
  if (g.trend === 'batch') return { rule: { metric: 'not_drained', threshold: 2, forMin: 0 }, why: `${g.id} is a batch consumer: its lag climbs and resets every hour, so a fixed lag threshold would fire every run. Alert when it hasn't drained in 2 hours instead.` };
  if (g.trend === 'growing') return { rule: { metric: 'growing', threshold: 0, forMin: 15 }, why: `${g.id} is falling behind right now — consumers are slower than producers. "Lag keeps growing for 15 min" fires on that whatever the size, and does not fire on a short backlog after a deploy.` };
  if (g.rebalancesPerHour > 3) return { rule: { metric: 'rebalances', threshold: 5, forMin: 15 }, why: `${g.id} rebalanced ${g.rebalancesPerHour}× in the last hour; a healthy group does it only on deploys. Pair it with a "lag keeps growing" alert.` };
  const r: LagRule = { clusterId: g.clusterId, group: g.id, topic, metric: 'time_behind', threshold: 0, forMin: 5 };
  const week = ruleSeries(r, '7d');
  const base = week.slice(0, Math.floor(week.length * 0.8));
  const p99 = [...base].sort((a, b) => a - b)[Math.floor(base.length * 0.99)] ?? 0;
  const th = Math.max(60, nice(p99 * 2));
  return {
    rule: { metric: 'time_behind', threshold: th, forMin: 10 },
    why: `On a normal day ${g.id} reads at most ~${formatRuleValue('time_behind', p99)} behind (7-day p99). ${formatRuleValue('time_behind', th)} for 10 minutes leaves room for deploy pauses and still fires well before messages expire.`,
  };
}

export const lagMonitorsOf = (clusterId: string, group?: string) => monitors.filter((m) => m.queue && m.queue.clusterId === clusterId && (!group || m.queue.group === group));

export interface LagAlertDraft { rule: LagRule; name: string; severity: Severity; channels: string[] }

/**
 * Monitor targets read "service · process" for instrumented consumers so they also show on the service's Alerts tab.
 * Api mode: shown right away, POSTed to /monitors in the background (the list refreshes when it lands).
 */
export function addLagMonitor(d: LagAlertDraft): Monitor {
  const g = groupOf(d.rule);
  const svc = services.find((s) => s.id === g?.serviceId);
  const target = svc ? `${svc.name} · ${g?.process ?? d.rule.group}` : `${clusterOf(d.rule.clusterId)?.name ?? d.rule.clusterId} · ${d.rule.group}`;
  const { firingNow } = isApi ? { firingNow: false } : evaluate(d.rule, '1h');
  const m: Monitor = {
    id: `m${monitors.length + 1}-${Date.now() % 10000}`, name: d.name, kind: 'consumer_lag', target, condition: describeRule(d.rule),
    severity: d.severity, state: firingNow ? 'pending' : 'ok', source: 'manual', lastChange: Date.now(), channels: d.channels, queue: asRule(d.rule),
  };
  monitors.push(m);
  emit();
  if (isApi) {
    const body = lagMonitorBody(d.rule, { name: d.name, severity: d.severity, channels: d.channels, service: svc?.id, process: svc ? g?.process ?? d.rule.group : undefined });
    mutate('POST', '/monitors', body, ['/monitors']).catch((e: unknown) => {
      console.warn('[data] POST /monitors', e);
      const i = monitors.indexOf(m);
      if (i >= 0) monitors.splice(i, 1);
      emit();
    });
  }
  return m;
}
