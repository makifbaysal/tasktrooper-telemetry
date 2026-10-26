/**
 * Infrastructure the services run on: databases/caches and message brokers.
 * Collected by the TaskTrooper agent, a cloud integration (RDS / MSK CloudWatch) or a direct
 * read-only connection. Service-side views (dependencies, consumer processes) link here by name.
 */
import { DEGRADED_SINCE, RANGE_POINTS, dependenciesFor, endpointsFor, series, services, timestamps } from './mock';
import { hydrateList, isApi, qs, registerHydrator, remote, seg } from './remote';
import type { Dependency, Endpoint, Health, Service, TimeRange } from './types';

function h(s: string): number {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}

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
export type DbEngine = 'postgres' | 'mysql' | 'redis' | 'mongodb';
export const ENGINE_LABEL: Record<DbEngine, string> = { postgres: 'PostgreSQL', mysql: 'MySQL', redis: 'Redis', mongodb: 'MongoDB' };
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

const N = (id: string, role: DbNode['role'], zone: string, cpu: number, memory: number, connections: number, replLagMs?: number): DbNode =>
  ({ id, role, zone, cpu, memory, connections, replLagMs });

export const databases: Database[] = [
  {
    id: 'orders-db', name: 'orders-db', engine: 'postgres', version: '16.4', host: 'orders-db.cluster-c8x.eu-central-1.rds.amazonaws.com',
    instanceClass: 'db.r6g.2xlarge', collector: 'cloud', source: 'AWS RDS · eu-central-1', status: 'warning',
    cpu: 0.48, prevCpu: 0.44, memory: 0.62, disk: 0.58, diskGb: 500, connections: 342, maxConnections: 400,
    p95: 12, prevP95: 11, slowPerMin: 4, hitRatio: 0.992,
    nodes: [N('orders-db-1', 'primary', 'eu-central-1a', 0.52, 0.64, 296), N('orders-db-2', 'replica', 'eu-central-1b', 0.41, 0.6, 26, 40), N('orders-db-3', 'replica', 'eu-central-1c', 0.44, 0.61, 20, 55)],
    problem: {
      title: 'Connections at 86% of max_connections',
      detail: 'orders-api opens a pool of 25 per instance (12 instances → 300) and order-events-consumer 5 per instance (8 → 40). The next orders-api scale-out step (14 instances) hits the limit and new connections start failing.',
      fix: 'Put PgBouncer (transaction pooling) in front of orders-db, or lower the orders-api pool to 15 — it uses ~11 connections per instance at peak.',
    },
  },
  {
    id: 'payments-db', name: 'payments-db', engine: 'postgres', version: '15.7', host: 'payments-db.cluster-c8x.eu-central-1.rds.amazonaws.com',
    instanceClass: 'db.r6g.xlarge', collector: 'cloud', source: 'AWS RDS · eu-central-1', status: 'ok',
    cpu: 0.31, prevCpu: 0.29, memory: 0.48, disk: 0.41, diskGb: 250, connections: 88, maxConnections: 300,
    p95: 15, prevP95: 14, slowPerMin: 1, hitRatio: 0.995,
    nodes: [N('payments-db-1', 'primary', 'eu-central-1a', 0.33, 0.5, 78), N('payments-db-2', 'replica', 'eu-central-1b', 0.22, 0.46, 10, 30)],
  },
  {
    id: 'reco-db', name: 'reco-db', engine: 'postgres', version: '16.2', host: 'reco-db-0.reco-db.data.svc.cluster.local',
    instanceClass: '8 vCPU · 32 GiB (k8s StatefulSet)', collector: 'agent', source: 'TaskTrooper agent · pg_stat_statements', status: 'critical',
    cpu: 0.94, prevCpu: 0.32, memory: 0.71, disk: 0.44, diskGb: 200, connections: 182, maxConnections: 200,
    p95: 1100, prevP95: 9, slowPerMin: 410, hitRatio: 0.87,
    nodes: [N('reco-db-0', 'primary', 'eu-central-1a', 0.94, 0.71, 168), N('reco-db-1', 'replica', 'eu-central-1b', 0.38, 0.66, 14, 2100)],
    since: DEGRADED_SINCE,
    problem: {
      title: 'Sequential scans on reco_items after migration 0042',
      detail: 'Migration 0042 dropped idx_reco_items_user_score. The top query now scans 4.2M rows per call: CPU went from 32% to 94%, query p95 from 9 ms to 1.1 s, and slow queries hold connections (182 of 200). recommendation-api and storefront-bff slow down with it.',
      fix: 'CREATE INDEX CONCURRENTLY idx_reco_items_user_score ON reco_items (user_id, score DESC); — or roll back migration 0042.',
    },
  },
  {
    id: 'catalog-db', name: 'catalog-db', engine: 'postgres', version: '16.4', host: 'catalog-db.cluster-c8x.eu-central-1.rds.amazonaws.com',
    instanceClass: 'db.r6g.2xlarge', collector: 'cloud', source: 'AWS RDS · eu-central-1', status: 'ok',
    cpu: 0.36, prevCpu: 0.34, memory: 0.55, disk: 0.37, diskGb: 400, connections: 120, maxConnections: 500,
    p95: 6, prevP95: 6, slowPerMin: 0, hitRatio: 0.998,
    nodes: [N('catalog-db-1', 'primary', 'eu-central-1a', 0.29, 0.56, 40), N('catalog-db-2', 'replica', 'eu-central-1b', 0.42, 0.54, 41, 20), N('catalog-db-3', 'replica', 'eu-central-1c', 0.4, 0.55, 39, 25)],
  },
  {
    id: 'auth-db', name: 'auth-db', engine: 'postgres', version: '15.7', host: 'auth-db.cluster-c8x.eu-central-1.rds.amazonaws.com',
    instanceClass: 'db.r6g.large', collector: 'cloud', source: 'AWS RDS · eu-central-1', status: 'ok',
    cpu: 0.22, prevCpu: 0.23, memory: 0.4, disk: 0.18, diskGb: 100, connections: 48, maxConnections: 200,
    p95: 4, prevP95: 4, slowPerMin: 0, hitRatio: 0.997,
    nodes: [N('auth-db-1', 'primary', 'eu-central-1a', 0.24, 0.41, 40), N('auth-db-2', 'replica', 'eu-central-1b', 0.12, 0.38, 8, 15)],
  },
  {
    id: 'notifications-db', name: 'notifications-db', engine: 'mysql', version: '8.0.36', host: 'notifications-db.internal:3306',
    instanceClass: '4 vCPU · 16 GiB (VM)', collector: 'direct', source: 'Direct · read-only user trooper_monitor', status: 'ok',
    cpu: 0.18, prevCpu: 0.17, memory: 0.52, disk: 0.66, diskGb: 120, connections: 40, maxConnections: 300,
    p95: 14, prevP95: 13, slowPerMin: 2, hitRatio: 0.981,
    nodes: [N('notifications-db', 'primary', 'eu-central-1a', 0.18, 0.52, 40)],
  },
  {
    id: 'cache-main', name: 'cache-main', engine: 'redis', version: '7.2', host: 'cache-main.x1y2.clustercfg.euc1.cache.amazonaws.com',
    instanceClass: 'cache.r6g.large × 3 shards', collector: 'cloud', source: 'AWS ElastiCache · eu-central-1', status: 'warning',
    cpu: 0.34, prevCpu: 0.31, memory: 0.88, disk: 0, diskGb: 0, connections: 610, maxConnections: 65000,
    p95: 1.4, prevP95: 1.3, slowPerMin: 0, hitRatio: 0.91, evictionsPerMin: 120,
    nodes: [N('cache-main-0001', 'shard', 'eu-central-1a', 0.36, 0.89, 205), N('cache-main-0002', 'shard', 'eu-central-1b', 0.33, 0.87, 203), N('cache-main-0003', 'shard', 'eu-central-1c', 0.32, 0.88, 202)],
    problem: {
      title: 'Memory at 88% of maxmemory — evicting 120 keys/min',
      detail: 'allkeys-lru is evicting cart:* and order:* keys; the hit ratio fell from 96% to 91% this week, so orders-api GET /v1/cart and GET /v1/orders/:id fall through to orders-db more often (part of its connection pressure).',
      fix: 'session:* keys use 38% of memory and have no TTL — set one (auth-api refresh tokens live 14 days). Or move to cache.r6g.xlarge.',
    },
  },
  {
    id: 'cache-payments', name: 'cache-payments', engine: 'redis', version: '7.2', host: 'cache-payments.x1y2.euc1.cache.amazonaws.com',
    instanceClass: 'cache.r6g.large', collector: 'cloud', source: 'AWS ElastiCache · eu-central-1', status: 'ok',
    cpu: 0.12, prevCpu: 0.12, memory: 0.34, disk: 0, diskGb: 0, connections: 96, maxConnections: 65000,
    p95: 1.1, prevP95: 1.1, slowPerMin: 0, hitRatio: 0.97, evictionsPerMin: 0,
    nodes: [N('cache-payments-001', 'primary', 'eu-central-1a', 0.12, 0.34, 90), N('cache-payments-002', 'replica', 'eu-central-1b', 0.05, 0.34, 6, 1)],
  },
  {
    id: 'cache-bff', name: 'cache-bff', engine: 'redis', version: '7.2', host: 'cache-bff.x1y2.euc1.cache.amazonaws.com',
    instanceClass: 'cache.r6g.large', collector: 'cloud', source: 'AWS ElastiCache · eu-central-1', status: 'ok',
    cpu: 0.21, prevCpu: 0.2, memory: 0.52, disk: 0, diskGb: 0, connections: 72, maxConnections: 65000,
    p95: 1.3, prevP95: 1.2, slowPerMin: 0, hitRatio: 0.95, evictionsPerMin: 0,
    nodes: [N('cache-bff-001', 'primary', 'eu-central-1a', 0.21, 0.52, 72)],
  },
  {
    id: 'reco-cache', name: 'reco-cache', engine: 'redis', version: '7.0', host: 'reco-cache-0.reco-cache.data.svc.cluster.local',
    instanceClass: '2 vCPU · 8 GiB (k8s)', collector: 'agent', source: 'TaskTrooper agent · INFO', status: 'ok',
    cpu: 0.16, prevCpu: 0.15, memory: 0.46, disk: 0, diskGb: 0, connections: 40, maxConnections: 10000,
    p95: 1.4, prevP95: 1.4, slowPerMin: 0, hitRatio: 0.82, evictionsPerMin: 0,
    nodes: [N('reco-cache-0', 'primary', 'eu-central-1a', 0.16, 0.46, 40)],
  },
];

/** "postgres · orders-db" → "orders-db" */
export const depTargetName = (d: Pick<Dependency, 'name'>) => d.name.split(' · ')[1] ?? d.name;
export const databaseFor = (d: Pick<Dependency, 'name' | 'kind'>) =>
  d.kind === 'db' || d.kind === 'cache' ? databases.find((x) => x.name === depTargetName(d)) : undefined;

export interface DbUser { service: Service; dep: Dependency }
/** Instrumented services whose SDK reports calls to this database. */
export function dbUsers(db: Database): DbUser[] {
  if (isApi) return remote(`/infra/databases/${seg(db.id)}/users${qs({ range: '1h' })}`, [] as DbUser[]);
  return services
    .filter((s) => s.type !== 'web' && s.type !== 'mobile')
    .flatMap((s) => dependenciesFor(s.id).filter((d) => (d.kind === 'db' || d.kind === 'cache') && depTargetName(d) === db.name).map((dep) => ({ service: s, dep })))
    .sort((a, b) => b.dep.callsPerMin - a.dep.callsPerMin);
}

/** Queries (SQL) or commands (Redis) per second, from what the services send plus ~8% background (jobs, replication, admin). */
export function dbOpsPerSec(db: Database): number {
  if (isApi) return (db as Database & { opsPerSec?: number }).opsPerSec ?? 0;
  const sent = dbUsers(db).reduce((a, u) => a + u.dep.callsPerMin, 0) / 60;
  return sent ? sent * 1.08 : 40 + (h(db.id) % 200);
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
const Q = (text: string, serviceId: string, endpoint: string, perCall: number, avgMs: number, p95: number, rows: number, plan?: string) =>
  ({ text, serviceId, endpoint, perCall, avgMs, p95, rows, plan });

const QUERY_DEFS: Record<string, Omit<DbQuery, 'id'>[]> = {
  'orders-db': [
    Q('INSERT INTO orders (customer_id, merchant_id, total, currency, status) VALUES ($1, $2, $3, $4, $5) RETURNING id', 'orders-api', 'POST /v1/orders', 1, 4.1, 14, 1),
    Q('SELECT * FROM orders WHERE id = $1', 'orders-api', 'GET /v1/orders/:id', 1, 0.8, 2.4, 1),
    Q('SELECT * FROM orders WHERE customer_id = $1 ORDER BY created_at DESC LIMIT $2', 'orders-api', 'GET /v1/orders', 2, 3.6, 11, 20),
    Q('INSERT INTO order_items (order_id, sku, qty, price) SELECT $1, unnest($2::text[]), unnest($3::int[]), unnest($4::numeric[])', 'orders-api', 'POST /v1/orders', 3, 2.2, 7, 4),
    Q('SELECT ci.*, p.price FROM cart_items ci JOIN prices p ON p.sku = ci.sku WHERE ci.cart_id = $1', 'orders-api', 'GET /v1/cart', 1, 1.9, 6, 6),
    Q('UPDATE orders SET status = $1, updated_at = now() WHERE id = $2', 'order-events', 'order-status-projector', 1, 1.4, 5, 1),
    Q('SELECT id, status FROM orders WHERE id = ANY($1) FOR UPDATE', 'order-events', 'order-created-handler', 1, 2.8, 12, 1),
  ],
  'payments-db': [
    Q('INSERT INTO charges (order_id, amount, currency, provider, status) VALUES ($1, $2, $3, $4, $5) RETURNING id', 'payments-api', 'POST /v1/charges', 1, 3.2, 11, 1),
    Q('UPDATE charges SET status = $1, provider_ref = $2 WHERE id = $3', 'payments-api', 'POST /v1/charges', 2, 2.4, 9, 1),
    Q('SELECT * FROM charges WHERE id = $1', 'payments-api', 'GET /v1/charges/:id', 1, 0.7, 2, 1),
    Q('UPDATE charges SET status = $1 WHERE provider_ref = $2', 'payments-api', 'POST /v1/webhooks/stripe', 2, 3.9, 15, 1),
    Q('INSERT INTO refunds (charge_id, amount, reason) VALUES ($1, $2, $3)', 'payments-api', 'POST /v1/refunds', 3, 2.9, 10, 1),
  ],
  'reco-db': [
    Q('SELECT item_id, score FROM reco_items WHERE user_id = $1 ORDER BY score DESC LIMIT $2', 'recommendation-api', 'GET /v1/recommendations', 2, 480, 1100, 4_200_000,
      'Seq Scan on reco_items (rows=4.2M) → Sort (top-N heapsort). Before migration 0042: Index Scan using idx_reco_items_user_score (rows=20), 1.8 ms.'),
    Q('SELECT * FROM items WHERE id = ANY($1)', 'recommendation-api', 'GET /v1/recommendations', 1, 3.1, 9, 20),
    Q('SELECT segment FROM user_segments WHERE user_id = $1', 'recommendation-api', 'GET /v1/recommendations', 1, 1.2, 4, 1),
  ],
  'cache-main': [
    Q('GET cart:{id}', 'orders-api', 'GET /v1/cart', 2, 0.3, 1.1, 1),
    Q('GET order:{id}', 'orders-api', 'GET /v1/orders/:id', 1, 0.3, 1.0, 1),
    Q('SETEX order:{id} 300 …', 'orders-api', 'POST /v1/orders', 2, 0.4, 1.3, 1),
    Q('MGET item:{id} …', 'catalog-api', 'GET /v1/items/:id', 1, 0.5, 1.6, 12),
    Q('SET session:{id} … (no TTL)', 'auth-api', 'POST /v1/auth/refresh', 1, 0.4, 1.4, 1),
    Q('GET session:{id}', 'auth-api', 'GET /v1/auth/me', 1, 0.3, 1.0, 1),
  ],
};

const resourceOf = (route: string) => route.split('/').filter((p) => p && !p.startsWith(':') && !/^v\d+$/.test(p)).pop() ?? 'items';

/** Fallback for databases without hand-written queries: derived from the busiest endpoints that call it. */
function genericQueries(db: Database): Omit<DbQuery, 'id'>[] {
  return dbUsers(db).flatMap((u) => endpointsFor(u.service.id).sort((a, b) => b.rpm - a.rpm).slice(0, 2).map((e) => {
    const r = resourceOf(e.sub?.topic ?? e.route).replace(/[^a-z_]/gi, '_');
    const key = e.sub ? e.id : `${e.method} ${e.route}`;
    const text = isKv(db.engine)
      ? (e.method === 'GET' ? `GET ${r}:{id}` : `SETEX ${r}:{id} 600 …`)
      : e.method === 'GET' ? `SELECT * FROM ${r} WHERE id = $1`
      : e.sub ? `INSERT INTO ${r}_log (message_id, payload, processed_at) VALUES ($1, $2, now())`
      : `INSERT INTO ${r} (…) VALUES (…) RETURNING id`;
    const base = isKv(db.engine) ? 0.35 : db.p95 / 3.5;
    return Q(text, u.service.id, key, 1, base, base * 3.2, e.method === 'GET' ? 1 : 1);
  }));
}

export interface DbQueryRow extends DbQuery { callsPerMin: number; ep?: Endpoint; share: number }
export function dbQueries(db: Database): DbQueryRow[] {
  if (isApi) {
    return remote(`/infra/databases/${seg(db.id)}/queries${qs({ range: '1h' })}`, [] as DbQueryRow[], (raw: DbQueryRow[]) => raw.map((q) => ({
      ...q, plan: q.plan ?? undefined,
      ep: endpointsFor(q.serviceId).find((e) => `${e.method} ${e.route}` === q.endpoint || e.id === q.endpoint),
    })));
  }
  const defs = QUERY_DEFS[db.id] ?? genericQueries(db);
  const rows = defs.map((q, i) => {
    const ep = endpointsFor(q.serviceId).find((e) => `${e.method} ${e.route}` === q.endpoint || e.id === q.endpoint);
    return { ...q, id: `${db.id}-q${i}`, ep, callsPerMin: (ep?.rpm ?? 100) * q.perCall, share: 0 };
  });
  const total = rows.reduce((a, r) => a + r.callsPerMin * r.avgMs, 0) || 1;
  return rows.map((r) => ({ ...r, share: (r.callsPerMin * r.avgMs) / total })).sort((a, b) => b.share - a.share);
}

// ---------- series
export type DbMetric = 'cpu' | 'memory' | 'connections' | 'ops' | 'p95' | 'hit' | 'repl';
export function dbSeries(db: Database, metric: DbMetric, range: TimeRange, nodeId?: string): number[] {
  if (isApi) {
    // one series per node; the database-level value sums counts and takes the worst node otherwise
    const combine = metric === 'connections' || metric === 'ops' ? 'sum' : metric === 'hit' ? 'min' : 'max';
    return remote(`/infra/databases/${seg(db.id)}/series${qs({ range, metric, node: nodeId })}`, zeros(range),
      (raw: SeriesSet) => (nodeId ? pick(raw, nodeId, range) : merge(raw, range, combine)));
  }
  const node = nodeId ? db.nodes.find((n) => n.id === nodeId) : undefined;
  const cur: Record<DbMetric, number> = {
    cpu: node?.cpu ?? db.cpu, memory: node?.memory ?? db.memory, connections: node?.connections ?? db.connections,
    ops: dbOpsPerSec(db), p95: db.p95, hit: db.hitRatio, repl: node?.replLagMs ?? 0,
  };
  const prev: Partial<Record<DbMetric, number>> = { cpu: db.prevCpu, p95: db.prevP95, connections: db.id === 'reco-db' ? 60 : undefined, hit: db.id === 'reco-db' ? 0.995 : undefined, repl: db.id === 'reco-db' ? 30 : undefined };
  // a replica follows the primary's step only for replication lag
  const stepped = db.since && (!node || node.role === 'primary' || metric === 'repl') && prev[metric] !== undefined;
  const v = series(`db-${db.id}-${nodeId ?? ''}-${metric}`, range, {
    base: cur[metric], noise: metric === 'hit' ? 0.004 : 0.08, daily: metric === 'hit' ? 0.002 : metric === 'p95' ? 0.08 : 0.2,
    ...(stepped ? { stepAt: db.since, stepFrom: prev[metric]! / cur[metric] } : {}),
  });
  if (metric === 'cpu' || metric === 'memory' || metric === 'hit') return v.map((x) => Math.min(0.999, x));
  if (metric === 'connections') return v.map((x) => Math.min(db.maxConnections, Math.round(x)));
  return v;
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

export const clusters: Cluster[] = [
  {
    id: 'kafka-main', name: 'kafka-main', kind: 'kafka', version: 'Kafka 3.7 (KRaft)', collector: 'cloud', source: 'Amazon MSK · eu-central-1',
    endpoint: 'b-1.kafkamain.c3.kafka.eu-central-1.amazonaws.com:9096, b-2…, b-3…',
    nodes: [
      { id: 'broker-1', host: 'b-1.kafkamain.c3.kafka.eu-central-1.amazonaws.com', cpu: 0.41, memory: 0.58, disk: 0.62, netInMBs: 18.4, leaders: 12, underReplicated: 0, status: 'ok' },
      { id: 'broker-2', host: 'b-2.kafkamain.c3.kafka.eu-central-1.amazonaws.com', cpu: 0.47, memory: 0.6, disk: 0.81, netInMBs: 21.2, leaders: 13, underReplicated: 0, status: 'warning',
        note: 'Disk 81% — it leads the busiest orders.created partitions (7-day retention). Full in ~9 days at the current rate: rebalance leaders or cut retention to 3 days.' },
      { id: 'broker-3', host: 'b-3.kafkamain.c3.kafka.eu-central-1.amazonaws.com', cpu: 0.38, memory: 0.57, disk: 0.59, netInMBs: 16.9, leaders: 11, underReplicated: 0, status: 'ok' },
    ],
  },
  {
    id: 'rabbit-main', name: 'rabbit-main', kind: 'rabbitmq', version: 'RabbitMQ 3.13', collector: 'agent', source: 'TaskTrooper agent · management API',
    endpoint: 'https://rabbit-main.internal:15672', vhost: '/shop',
    nodes: [
      { id: 'rabbit@rabbit-0', host: 'rabbit-0.rabbit.data.svc.cluster.local', cpu: 0.12, memory: 0.31, disk: 0.22, netInMBs: 1.2, status: 'ok' },
      { id: 'rabbit@rabbit-1', host: 'rabbit-1.rabbit.data.svc.cluster.local', cpu: 0.1, memory: 0.29, disk: 0.21, netInMBs: 0.9, status: 'ok' },
      { id: 'rabbit@rabbit-2', host: 'rabbit-2.rabbit.data.svc.cluster.local', cpu: 0.11, memory: 0.3, disk: 0.22, netInMBs: 1.0, status: 'ok' },
    ],
  },
  // managed queues: no brokers to watch — lag is what CloudWatch / Cloud Monitoring report per queue / subscription
  {
    id: 'aws-sqs-eu-central-1', name: 'sqs-eu-central-1', kind: 'sqs', version: 'Amazon SQS', collector: 'cloud', source: 'Amazon SQS · eu-central-1',
    endpoint: 'https://sqs.eu-central-1.amazonaws.com/123456789012', nodes: [], integrationId: 'int-aws',
  },
  {
    id: 'gcp-pubsub-shop-prod', name: 'shop-prod', kind: 'pubsub', version: 'Google Pub/Sub', collector: 'cloud', source: 'Google Pub/Sub · shop-prod',
    endpoint: 'projects/shop-prod', nodes: [], integrationId: 'int-gcp',
  },
];

export const topics: Topic[] = [
  { clusterId: 'kafka-main', name: 'orders.created', partitions: 12, inPerSec: 106, avgBytes: 1800, retention: '7d', producers: ['orders-api'] },
  { clusterId: 'kafka-main', name: 'orders.status-changed', partitions: 6, inPerSec: 39, avgBytes: 420, retention: '7d', producers: ['orders-api', 'payments-api', 'order-events'] },
  { clusterId: 'kafka-main', name: 'orders.created.dlq', partitions: 1, inPerSec: 0.04, avgBytes: 2100, retention: '30d', producers: ['order-events'] },
  { clusterId: 'kafka-main', name: 'payments.events', partitions: 6, inPerSec: 61, avgBytes: 950, retention: '14d', producers: ['payments-api'] },
  { clusterId: 'kafka-main', name: 'reco.events', partitions: 6, inPerSec: 100, avgBytes: 260, retention: '2d', producers: ['recommendation-api'] },
  { clusterId: 'kafka-main', name: 'catalog.events', partitions: 6, inPerSec: 2.1, avgBytes: 3400, retention: '7d', producers: ['catalog-api'] },
  { clusterId: 'kafka-main', name: 'auth.events', partitions: 3, inPerSec: 15, avgBytes: 380, retention: '30d', producers: ['auth-api'] },
  { clusterId: 'rabbit-main', name: 'emails.transactional', partitions: 1, inPerSec: 11.7, avgBytes: 2600, retention: 'x-message-ttl 24h', producers: ['orders-api', 'payments-api'], binding: 'emails (topic) · email.transactional.#' },
  { clusterId: 'rabbit-main', name: 'emails.marketing', partitions: 1, inPerSec: 1.7, avgBytes: 5400, retention: 'x-message-ttl 72h', producers: ['backoffice-web campaigns'], binding: 'emails (topic) · email.marketing.#' },
  { clusterId: 'rabbit-main', name: 'email.events', partitions: 1, inPerSec: 3, avgBytes: 300, retention: 'none', producers: ['email-worker'], binding: 'email.events (fanout)' },
  { clusterId: 'aws-sqs-eu-central-1', name: 'notifications-sms', partitions: 1, inPerSec: 4.2, avgBytes: 640, retention: '4d', producers: ['orders-api'], binding: 'standard queue · DLQ notifications-sms-dlq after 5 receives' },
  { clusterId: 'aws-sqs-eu-central-1', name: 'notifications-push', partitions: 1, inPerSec: 7.4, avgBytes: 900, retention: '4d', producers: ['orders-api', 'payments-api'], binding: 'standard queue · DLQ notifications-push-dlq after 5 receives' },
  { clusterId: 'aws-sqs-eu-central-1', name: 'notifications-sms-dlq', partitions: 1, inPerSec: 0.01, avgBytes: 700, retention: '14d', producers: ['SQS redrive'] },
  { clusterId: 'gcp-pubsub-shop-prod', name: 'catalog-updates.search-sync', partitions: 1, inPerSec: 2.1, avgBytes: 3400, retention: '7d', producers: ['catalog-api'], binding: 'topic catalog-updates · pull · ack deadline 60 s' },
  { clusterId: 'gcp-pubsub-shop-prod', name: 'reco-feedback.bq-export', partitions: 1, inPerSec: 38, avgBytes: 220, retention: '7d', producers: ['recommendation-api'], binding: 'topic reco-feedback · BigQuery subscription' },
];

/** Spreads `total` over `n` partitions; `hot` gets `hotShare` of it. Deterministic. */
function spread(key: string, total: number, n: number, hot?: number, hotShare = 0): number[] {
  if (n <= 1) return [total];
  const w = Array.from({ length: n }, (_, i) => 0.6 + ((h(`${key}-${i}`) % 1000) / 1000) * 0.8);
  const restTotal = total * (hot !== undefined ? 1 - hotShare : 1);
  const sum = w.reduce((a, b) => a + b, 0) - (hot !== undefined ? w[hot] : 0);
  return w.map((x, i) => Math.round(i === hot ? total * hotShare : (x / sum) * restTotal));
}

/** Per-group specifics on top of what the consumer's SDK reports. */
const GROUP_EXTRA: Record<string, Partial<ConsumerGroup> & { hot?: number; hotShare?: number; consumeMul?: number }> = {
  'order-events.fulfilment': {
    trend: 'growing', hot: 7, hotShare: 0.68, consumeMul: 0.975,
    note: 'Partition 7 holds most of the lag: orders are keyed by merchant_id and merchant m-1042 is 31% of today\'s orders, so one member does a third of the work. More members won\'t help one partition — key by order_id (ordering is only needed per order), or give m-1042 its own topic.',
  },
};

function ownGroups(): ConsumerGroup[] {
  return services.filter((s) => s.type === 'consumer' || s.type === 'worker').flatMap((s) =>
    endpointsFor(s.id).filter((e) => e.sub && e.sub.source !== 'cron' && e.sub.source !== 'sqs').map((e): ConsumerGroup => {
      const sub = e.sub!;
      const id = sub.group ?? sub.process;
      const x = GROUP_EXTRA[id] ?? {};
      const kind: BrokerKind = sub.source === 'kafka' ? 'kafka' : 'rabbitmq';
      return {
        clusterId: kind === 'kafka' ? 'kafka-main' : 'rabbit-main', id, serviceId: s.id, process: sub.process,
        members: kind === 'kafka' ? Math.min(s.instances, sub.partitions ?? s.instances) : s.instances * 4,
        state: 'Stable', rebalancesPerHour: 0, trend: x.trend ?? 'steady', note: x.note,
        reads: [{
          topic: sub.topic,
          partitionLag: spread(id, sub.lag, sub.partitions ?? 1, x.hot, x.hotShare),
          consumePerSec: (e.rpm / 60) * (x.consumeMul ?? 1),
          unacked: kind === 'rabbitmq' ? s.instances * 10 : undefined,
        }],
      };
    }));
}

const externalGroups: ConsumerGroup[] = [
  {
    clusterId: 'kafka-main', id: 'analytics.s3-sink', external: 'Kafka Connect · S3 sink', members: 3, state: 'Stable', rebalancesPerHour: 0, trend: 'steady',
    reads: [{ topic: 'orders.created', partitionLag: spread('s3', 1240, 12), consumePerSec: 106 }, { topic: 'payments.events', partitionLag: spread('s3p', 380, 6), consumePerSec: 61 }],
  },
  {
    clusterId: 'kafka-main', id: 'ledger-sync', external: 'ledger-service (no TaskTrooper SDK)', members: 2, state: 'Rebalancing', rebalancesPerHour: 14, trend: 'growing',
    reads: [{ topic: 'payments.events', partitionLag: spread('ledger', 42000, 6), consumePerSec: 44 }],
    note: 'The group rebalanced 14 times in the last hour: members leave because processing a batch takes longer than max.poll.interval.ms (300 s), and every rebalance pauses all partitions. Lower max.poll.records or raise max.poll.interval.ms — and add the SDK to ledger-service to see why a batch is slow.',
  },
  {
    clusterId: 'kafka-main', id: 'reco-trainer', external: 'reco-trainer (Spark, hourly batch)', members: 0, state: 'Empty', rebalancesPerHour: 1, trend: 'batch',
    reads: [{ topic: 'reco.events', partitionLag: spread('reco-trainer', 180000, 6), consumePerSec: 0 }],
    note: 'Batch consumer: it starts every hour, reads everything since its last commit and leaves. Lag climbing to ~360K and dropping to zero each hour is normal; alert only if it stops dropping.',
  },
  {
    clusterId: 'kafka-main', id: 'audit-log.sink', external: 'audit-log (Kafka Connect · OpenSearch sink)', members: 1, state: 'Stable', rebalancesPerHour: 0, trend: 'steady',
    reads: [{ topic: 'auth.events', partitionLag: spread('audit', 40, 3), consumePerSec: 15 }],
  },
  {
    clusterId: 'rabbit-main', id: 'crm-sync', external: 'crm-sync (not instrumented)', members: 2, state: 'Stable', rebalancesPerHour: 0, trend: 'steady',
    reads: [{ topic: 'email.events', partitionLag: [15], consumePerSec: 3, unacked: 4 }],
  },
];

/** SQS consumers and Pub/Sub subscribers: one "partition", lag = visible / undelivered messages, plus the oldest message's age. */
const cloudQueueGroups: ConsumerGroup[] = [
  {
    clusterId: 'aws-sqs-eu-central-1', id: 'sms-sender', serviceId: 'email-worker', process: 'sms-sender', members: 4, state: 'Stable', rebalancesPerHour: 0, trend: 'steady',
    reads: [{ topic: 'notifications-sms', partitionLag: [38], oldestAgeS: 14, consumePerSec: 4.3 }],
  },
  {
    clusterId: 'aws-sqs-eu-central-1', id: 'push-sender', serviceId: 'email-worker', process: 'push-sender', members: 2, state: 'Stable', rebalancesPerHour: 0, trend: 'growing',
    reads: [{ topic: 'notifications-push', partitionLag: [2650], oldestAgeS: 610, consumePerSec: 6.6 }],
    note: 'push-sender polls with 2 workers and a 30 s visibility timeout; the APNs call got slower after the provider\'s outage, so messages come back visible and are received again. Raise the visibility timeout to 120 s and add workers — the oldest push is 10 minutes old.',
  },
  {
    clusterId: 'gcp-pubsub-shop-prod', id: 'catalog-search-sync', serviceId: 'catalog-api', process: 'catalog-search-sync', members: 2, state: 'Stable', rebalancesPerHour: 0, trend: 'steady',
    reads: [{ topic: 'catalog-updates.search-sync', partitionLag: [12], oldestAgeS: 6, consumePerSec: 2.1 }],
  },
  {
    clusterId: 'gcp-pubsub-shop-prod', id: 'bq-export', external: 'BigQuery subscription (managed)', members: 1, state: 'Stable', rebalancesPerHour: 0, trend: 'steady',
    reads: [{ topic: 'reco-feedback.bq-export', partitionLag: [420], oldestAgeS: 11, consumePerSec: 38 }],
  },
];

export const consumerGroups: ConsumerGroup[] = [...ownGroups(), ...externalGroups, ...cloudQueueGroups];

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

/** Lag over time for one (group, topic). Growing groups ramp up, batch groups saw-tooth hourly. */
export function lagSeries(g: ConsumerGroup, r: GroupRead, range: TimeRange, partition?: number): number[] {
  if (isApi) {
    const byPartition = partition !== undefined;
    return remote(`/infra/clusters/${seg(g.clusterId)}/groups/${seg(g.id)}/lag${qs({ range, topic: r.topic, by: byPartition ? 'partition' : undefined })}`, zeros(range),
      (raw: SeriesSet) => pick(raw, byPartition ? `${r.topic}#${partition}` : r.topic, range));
  }
  const base = partition === undefined ? readLag(r) : r.partitionLag[partition];
  const v = series(`lag-${g.clusterId}-${g.id}-${r.topic}-${partition ?? 'all'}`, range, { base, noise: 0.12, daily: 0.15 });
  const n = v.length;
  if (g.trend === 'growing') return v.map((x, i) => x * (0.3 + 0.7 * (i / (n - 1)) ** 1.4));
  if (g.trend === 'shrinking') return v.map((x, i) => x * (2 - i / (n - 1)));
  if (g.trend === 'batch') {
    // points per hour: the batch job drains everything once an hour
    const per = Math.round(3_600_000 / RANGE_POINTS[range].stepMs);
    return v.map((x, i) => (per > 1 ? x * 2 * (((i + 3) % per) / per) : x));
  }
  return v;
}

export function topicInSeries(t: Topic, range: TimeRange): number[] {
  if (isApi) return remote(`/infra/clusters/${seg(t.clusterId)}/topics/${seg(t.name)}/series${qs({ range })}`, zeros(range), (raw: SeriesSet) => pick(raw, 'in', range));
  return series(`topic-in-${t.clusterId}-${t.name}`, range, { base: t.inPerSec, noise: 0.06, daily: 0.3 });
}
export function brokerNodeSeries(c: Cluster, n: BrokerNode, metric: 'cpu' | 'memory' | 'disk' | 'net', range: TimeRange): number[] {
  if (isApi) return remote(`/infra/clusters/${seg(c.id)}/nodes/series${qs({ range, metric })}`, zeros(range), (raw: SeriesSet) => pick(raw, n.id, range));
  const base = metric === 'cpu' ? n.cpu : metric === 'memory' ? n.memory : metric === 'disk' ? n.disk : n.netInMBs;
  return series(`broker-${c.id}-${n.id}-${metric}`, range, { base, noise: metric === 'disk' || metric === 'memory' ? 0.01 : 0.08, daily: metric === 'disk' || metric === 'memory' ? 0.02 : 0.25 }).map((x) => (metric === 'net' ? x : Math.min(0.999, x)));
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

// =================================================================== api mode (ADR-013)
type Nullable<T> = { [K in keyof T]?: T[K] | null };
const undef = <T>(v: T | null | undefined): T | undefined => (v === null ? undefined : v);

if (isApi) {
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
}
