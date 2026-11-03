/**
 * Infrastructure the services run on: databases/caches and message brokers.
 * Collected by the TaskTrooper agent, a cloud integration (RDS / MSK CloudWatch) or a direct
 * read-only connection. Service-side views (dependencies, consumer processes) link here by name.
 */
import { endpointsFor, timestamps } from './apm';
import { hydrateList, qs, registerHydrator, remote, seg } from './remote';
import type { Dependency, Endpoint, Health, Service, TimeRange } from './types';

// ---------- api series helpers (ADR-013)
interface SeriesSet { timestamps: number[]; series: { id: string; label?: string; data: number[] }[] }
const zeros = (range: TimeRange) => timestamps(range).map(() => 0);
/** Fit api data to the UI's buckets for the range (same count; pads/trims defensively). */
const fit = (data: number[] | undefined, range: TimeRange) => {
  const n = timestamps(range).length;
  const d = (data ?? []).map((v) => (Number.isFinite(v) ? v : 0));
  return d.length >= n ? d.slice(d.length - n) : [...Array(n - d.length).fill(0), ...d];
};
const pick = (raw: SeriesSet, id: string, range: TimeRange) => fit(raw.series.find((x) => x.id === id)?.data, range);
function merge(raw: SeriesSet, range: TimeRange, how: 'sum' | 'max' | 'min'): number[] {
  const all = raw.series.map((x) => fit(x.data, range));
  if (!all.length) return zeros(range);
  return all[0].map((_, i) => {
    const vs = all.map((a) => a[i]);
    if (how === 'sum') return vs.reduce((a, b) => a + b, 0);
    if (how === 'max') return Math.max(...vs);
    const pos = vs.filter((v) => v > 0);
    return pos.length ? Math.min(...pos) : 0;
  });
}

export type Collector = 'agent' | 'cloud' | 'direct';
export const COLLECTOR_LABEL: Record<Collector, string> = { agent: 'TaskTrooper agent', cloud: 'Cloud integration', direct: 'Direct connection' };

// =================================================================== databases
export type DbEngine = 'postgres' | 'mysql' | 'redis' | 'mongodb' | 'clickhouse';
export const ENGINE_LABEL: Record<DbEngine, string> = { postgres: 'PostgreSQL', mysql: 'MySQL', redis: 'Redis', mongodb: 'MongoDB', clickhouse: 'ClickHouse' };
/** Redis is a key-value store: "ops", "commands", memory and hit ratio instead of queries and buffer cache. */
export const isKv = (e: DbEngine) => e === 'redis';

export interface DbNode {
  id: string;
  role: 'primary' | 'replica' | 'shard';
  zone: string;
  cpu: number;
  memory: number;
  connections: number;
  /** replicas only */
  replLagMs?: number;
}

export interface Problem { title: string; detail: string; fix: string }

export interface Database {
  id: string;
  /** the name services use in their dependency (`postgres · orders-db`) */
  name: string;
  engine: DbEngine;
  version: string;
  host: string;
  instanceClass: string;
  collector: Collector;
  source: string;
  status: Health;
  cpu: number; prevCpu: number;
  memory: number;
  disk: number; diskGb: number;
  connections: number; maxConnections: number;
  /** p95 of a query / command as the database sees it */
  p95: number; prevP95: number;
  slowPerMin: number;
  /** buffer cache hit (SQL) or keyspace hit ratio (Redis) */
  hitRatio: number;
  evictionsPerMin?: number;
  nodes: DbNode[];
  /** start of the current problem — charts step there */
  since?: number;
  problem?: Problem;
  /** connected from the UI in this session */
  addedAt?: number;
  /** set when discovered by a cloud integration (data/integrations.ts) */
  integrationId?: string;
}

export const databases: Database[] = [];

/** "postgres · orders-db" → "orders-db" */
export const depTargetName = (d: Pick<Dependency, 'name'>) => d.name.split(' · ')[1] ?? d.name;
export const databaseFor = (d: Pick<Dependency, 'name' | 'kind'>) =>
  d.kind === 'db' || d.kind === 'cache' ? databases.find((x) => x.name === depTargetName(d)) : undefined;

export interface DbUser { service: Service; dep: Dependency }
/** Instrumented services whose SDK reports calls to this database. */
export function dbUsers(db: Database): DbUser[] {
  return remote(`/infra/databases/${seg(db.id)}/users${qs({ range: '1h' })}`, [] as DbUser[]);
}

/** Queries (SQL) or commands (Redis) per second, as the api reports them (0 when unknown). */
export function dbOpsPerSec(db: Database): number {
  return (db as Database & { opsPerSec?: number }).opsPerSec ?? 0;
}

// ---------- top queries
export interface DbQuery {
  id: string;
  text: string;
  serviceId: string;
  /** endpoint key "METHOD /route" or consumer process name */
  endpoint: string;
  /** calls per request of that endpoint */
  perCall: number;
  avgMs: number;
  p95: number;
  rows: number;
  /** plan note when the query is the problem */
  plan?: string;
}
export interface DbQueryRow extends DbQuery { callsPerMin: number; ep?: Endpoint; share: number }
export function dbQueries(db: Database): DbQueryRow[] {
  return remote(`/infra/databases/${seg(db.id)}/queries${qs({ range: '1h' })}`, [] as DbQueryRow[], (raw: DbQueryRow[]) => raw.map((q) => ({
    ...q, plan: q.plan ?? undefined,
    ep: endpointsFor(q.serviceId).find((e) => `${e.method} ${e.route}` === q.endpoint || e.id === q.endpoint),
  })));
}

// ---------- series
export type DbMetric = 'cpu' | 'memory' | 'connections' | 'ops' | 'p95' | 'hit' | 'repl';
export function dbSeries(db: Database, metric: DbMetric, range: TimeRange, nodeId?: string): number[] {
  // one series per node; the database-level value sums counts and takes the worst node otherwise
  const combine = metric === 'connections' || metric === 'ops' ? 'sum' : metric === 'hit' ? 'min' : 'max';
  return remote(`/infra/databases/${seg(db.id)}/series${qs({ range, metric, node: nodeId })}`, zeros(range),
    (raw: SeriesSet) => (nodeId ? pick(raw, nodeId, range) : merge(raw, range, combine)));
}

// =================================================================== message brokers
export type BrokerKind = 'kafka' | 'rabbitmq' | 'sqs' | 'pubsub';
export const BROKER_LABEL: Record<BrokerKind, string> = { kafka: 'Kafka', rabbitmq: 'RabbitMQ', sqs: 'Amazon SQS', pubsub: 'Google Pub/Sub' };
/** Queue-model brokers: one "partition", lag = messages waiting, no partitions/rebalances. */
export const isQueueKind = (k: BrokerKind) => k !== 'kafka';
/** Kafka: topics, consumer groups, partitions. RabbitMQ/SQS: queues, consumers. Pub/Sub: subscriptions. */
export const brokerTerms = (k: BrokerKind) => k === 'kafka'
  ? { topic: 'Topic', topics: 'Topics', group: 'Consumer group', groups: 'Consumer groups', lag: 'Lag', unit: 'msgs behind' }
  : k === 'pubsub'
    ? { topic: 'Subscription', topics: 'Subscriptions', group: 'Subscriber', groups: 'Subscribers', lag: 'Undelivered', unit: 'msgs undelivered' }
    : k === 'sqs'
      ? { topic: 'Queue', topics: 'Queues', group: 'Consumer', groups: 'Consumers', lag: 'Visible', unit: 'msgs visible' }
      : { topic: 'Queue', topics: 'Queues', group: 'Consumer', groups: 'Consumers', lag: 'Ready', unit: 'msgs waiting' };

export interface BrokerNode {
  id: string;
  host: string;
  cpu: number;
  memory: number;
  disk: number;
  netInMBs: number;
  /** Kafka: partition leaders on this broker */
  leaders?: number;
  underReplicated?: number;
  status: Health;
  note?: string;
}

export interface Cluster {
  id: string;
  name: string;
  kind: BrokerKind;
  version: string;
  /** bootstrap servers (Kafka) or management API (RabbitMQ) */
  endpoint: string;
  vhost?: string;
  source: string;
  collector: Collector;
  nodes: BrokerNode[];
  addedAt?: number;
  /** set when discovered by a cloud integration (data/integrations.ts) */
  integrationId?: string;
}

export interface Topic {
  clusterId: string;
  name: string;
  /** Kafka partitions; RabbitMQ queues have one */
  partitions: number;
  inPerSec: number;
  avgBytes: number;
  retention: string;
  /** service ids, or a description for producers outside TaskTrooper */
  producers: string[];
  /** RabbitMQ: exchange + routing key it is bound to */
  binding?: string;
}

export interface GroupRead {
  topic: string;
  /** messages behind per partition (RabbitMQ: one entry = ready messages) */
  partitionLag: number[];
  /** SQS / Pub/Sub / RabbitMQ: age of the oldest waiting message, seconds */
  oldestAgeS?: number;
  consumePerSec: number;
  /** RabbitMQ: delivered, not yet acked */
  unacked?: number;
}

export type GroupTrend = 'growing' | 'steady' | 'shrinking' | 'batch';
export interface ConsumerGroup {
  clusterId: string;
  /** Kafka group.id; RabbitMQ: consumer tag prefix */
  id: string;
  /** instrumented consumer — links to the service / process page */
  serviceId?: string;
  process?: string;
  /** who reads when no TaskTrooper SDK is in it */
  external?: string;
  members: number;
  state: 'Stable' | 'Rebalancing' | 'Empty';
  rebalancesPerHour: number;
  trend: GroupTrend;
  reads: GroupRead[];
  /** domain-specific explanation, shown before the generic insights */
  note?: string;
}

export const clusters: Cluster[] = [];
export const topics: Topic[] = [];
export const consumerGroups: ConsumerGroup[] = [];

export const readLag = (r: GroupRead) => r.partitionLag.reduce((a, b) => a + b, 0);
export const groupLag = (g: ConsumerGroup) => g.reads.reduce((a, r) => a + readLag(r), 0);
export const topicOf = (clusterId: string, name: string) => topics.find((t) => t.clusterId === clusterId && t.name === name);
export const clusterOf = (id: string) => clusters.find((c) => c.id === id);
export const groupsReading = (t: Topic) => consumerGroups.filter((g) => g.clusterId === t.clusterId && g.reads.some((r) => r.topic === t.name));
export const topicLag = (t: Topic) => groupsReading(t).reduce((a, g) => a + readLag(g.reads.find((r) => r.topic === t.name)!), 0);
/** Consumer groups of one service (the "what does this consumer read" view). */
export const groupsOfService = (serviceId: string) => consumerGroups.filter((g) => g.serviceId === serviceId);

export function readHealth(g: ConsumerGroup, r: GroupRead): Health {
  const lag = readLag(r);
  if (g.trend === 'batch') return 'ok';
  if (g.trend === 'growing' && lag > 10_000) return 'critical';
  // queues: how long the oldest message has waited says more than the count
  if (r.oldestAgeS !== undefined && r.oldestAgeS > 3600) return 'critical';
  if (r.oldestAgeS !== undefined && r.oldestAgeS > 300) return 'warning';
  if (g.trend === 'growing' || g.state === 'Rebalancing') return 'warning';
  return 'ok';
}
const RANK: Record<Health, number> = { ok: 0, warning: 1, critical: 2 };
export const worst = (hs: Health[]): Health => hs.reduce<Health>((a, b) => (RANK[b] > RANK[a] ? b : a), 'ok');
export const groupHealth = (g: ConsumerGroup) => worst(g.reads.map((r) => readHealth(g, r)));
export const topicHealth = (t: Topic) => worst(groupsReading(t).map((g) => readHealth(g, g.reads.find((r) => r.topic === t.name)!)));
export const clusterHealth = (c: Cluster) => worst([...c.nodes.map((n) => n.status), ...consumerGroups.filter((g) => g.clusterId === c.id).map(groupHealth)]);

/** Seconds to drain the lag at the current rates; Infinity when it is not draining. */
export function catchUp(g: ConsumerGroup, r: GroupRead): number {
  const t = topicOf(g.clusterId, r.topic);
  const net = r.consumePerSec - (t?.inPerSec ?? 0);
  const lag = readLag(r);
  if (lag === 0) return 0;
  if (g.trend === 'growing' || net <= 0) return Infinity;
  return lag / net;
}

export interface Insight { tone: 'critical' | 'warning' | 'info'; text: string }
/** What is wrong with this read and what to do about it — shown on the group drawer and the consumer's Lag tab. */
export function readInsights(g: ConsumerGroup, r: GroupRead): Insight[] {
  const out: Insight[] = [];
  const t = topicOf(g.clusterId, r.topic);
  const lag = readLag(r);
  const kafka = clusterOf(g.clusterId)?.kind === 'kafka';
  if (g.trend === 'growing' && t) {
    out.push({ tone: readHealth(g, r) === 'critical' ? 'critical' : 'warning',
      text: `Falling behind: consuming ${r.consumePerSec.toFixed(1)}/s while producers write ${t.inPerSec.toFixed(1)}/s — about ${Math.round((t.inPerSec - r.consumePerSec) * 3600).toLocaleString('en-US')} more messages behind every hour.` });
  }
  if (kafka && !g.note && r.partitionLag.length > 1 && lag > 500) {
    const max = Math.max(...r.partitionLag);
    const p = r.partitionLag.indexOf(max);
    if (max / lag > 0.4) out.push({ tone: 'warning', text: `Partition ${p} holds ${Math.round((max / lag) * 100)}% of the lag — a hot key or a stuck member, not a lack of members.` });
  }
  if (kafka && g.trend === 'growing' && t && g.members > 0 && g.members < t.partitions) {
    out.push({ tone: 'info', text: `${g.members} members for ${t.partitions} partitions: up to ${t.partitions} members add throughput; beyond that they sit idle.` });
  }
  const kind = clusterOf(g.clusterId)?.kind;
  if (r.oldestAgeS !== undefined && r.oldestAgeS >= 60 && kind) {
    const age = fmtAge(r.oldestAgeS);
    const hint = kind === 'sqs' ? 'messages that keep becoming visible again point at a visibility timeout shorter than processing, or a poison message without a DLQ redrive'
      : kind === 'pubsub' ? 'unacked messages are redelivered after the ack deadline — check the subscriber acks after processing and the deadline covers it'
      : 'a consumer that stopped acking (prefetch full) holds the head of the queue';
    out.push({ tone: r.oldestAgeS > 300 ? 'warning' : 'info', text: `Oldest message has waited ${age} — ${hint}.` });
  }
  if (kafka && g.state === 'Rebalancing') out.push({ tone: 'warning', text: `Rebalancing (${g.rebalancesPerHour}× in the last hour) — partitions are paused during each rebalance.` });
  if (g.external) out.push({ tone: 'info', text: `${g.external}: lag comes from the broker only — no processing time or errors without the TaskTrooper SDK.` });
  const s = catchUp(g, r);
  if (lag > 0 && Number.isFinite(s) && g.trend !== 'batch') out.push({ tone: 'info', text: `Draining: at the current rate the lag is gone in ${s < 90 ? `${Math.round(s)} s` : s < 5400 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`}.` });
  return out;
}

const fmtAge = (s: number) => (s < 90 ? `${Math.round(s)} s` : s < 5400 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`);

/** Lag over time for one (group, topic), optionally one partition. */
export function lagSeries(g: ConsumerGroup, r: GroupRead, range: TimeRange, partition?: number): number[] {
  const byPartition = partition !== undefined;
  return remote(`/infra/clusters/${seg(g.clusterId)}/groups/${seg(g.id)}/lag${qs({ range, topic: r.topic, by: byPartition ? 'partition' : undefined })}`, zeros(range),
    (raw: SeriesSet) => pick(raw, byPartition ? `${r.topic}#${partition}` : r.topic, range));
}

export function topicInSeries(t: Topic, range: TimeRange): number[] {
  return remote(`/infra/clusters/${seg(t.clusterId)}/topics/${seg(t.name)}/series${qs({ range })}`, zeros(range), (raw: SeriesSet) => pick(raw, 'in', range));
}
export function brokerNodeSeries(c: Cluster, n: BrokerNode, metric: 'cpu' | 'memory' | 'disk' | 'net', range: TimeRange): number[] {
  return remote(`/infra/clusters/${seg(c.id)}/nodes/series${qs({ range, metric })}`, zeros(range), (raw: SeriesSet) => pick(raw, n.id, range));
}

/** messaging.system (contracts §5) → broker kind */
const SYSTEM_KIND: Record<string, BrokerKind> = { kafka: 'kafka', rabbitmq: 'rabbitmq', aws_sqs: 'sqs', sqs: 'sqs', gcp_pubsub: 'pubsub', pubsub: 'pubsub' };

/** A dependency `kafka · orders.created` / `rabbitmq · email.events` / `aws_sqs · notifications-sms` → the topic it publishes to. */
export const topicForDep = (d: Pick<Dependency, 'name' | 'kind'>) => {
  if (d.kind !== 'queue') return undefined;
  const [system, name] = d.name.split(' · ');
  const kind = SYSTEM_KIND[system.toLowerCase()];
  for (const c of clusters.filter((x) => x.kind === kind)) {
    const t = topicOf(c.id, name);
    if (t) return t;
  }
  return undefined;
};

// =================================================================== hydration (ADR-013)
type Nullable<T> = { [K in keyof T]?: T[K] | null };
const undef = <T>(v: T | null | undefined): T | undefined => (v === null ? undefined : v);

registerHydrator(() => hydrateList(databases, `/infra/databases${qs({ range: '1h' })}`, (raw: (Database & Nullable<Pick<Database, 'evictionsPerMin' | 'problem'>>)[]) => raw.map((d) => ({
  ...d, evictionsPerMin: undef(d.evictionsPerMin), problem: undef(d.problem),
  nodes: (d.nodes ?? []).map((n) => ({ ...n, replLagMs: undef(n.replLagMs as number | null) })),
}))));
registerHydrator(() => hydrateList(clusters, '/infra/clusters', (raw: Cluster[]) => raw.map((c) => ({
  ...c, nodes: (c.nodes ?? []).map((n) => ({ ...n, leaders: undef(n.leaders as number | null), underReplicated: undef(n.underReplicated as number | null) })),
}))));
registerHydrator(() => hydrateList(topics, '/infra/topics', (raw: Topic[]) => raw.map((t) => ({ ...t, producers: t.producers ?? [] }))));
registerHydrator(() => hydrateList(consumerGroups, '/infra/groups', (raw: ConsumerGroup[]) => raw.map((g) => ({
  ...g, reads: (g.reads ?? []).map((r) => ({ ...r, unacked: undef(r.unacked as number | null), oldestAgeS: undef(r.oldestAgeS as number | null) })),
}))));
