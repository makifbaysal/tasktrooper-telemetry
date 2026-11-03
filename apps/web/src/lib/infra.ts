import { useEffect, useState } from 'react';
import {
  api, clusters, databases, mutate, type BrokerKind, type Cluster, type Collector, type Database, type DbEngine,
} from '@/data';

/**
 * Registry of connected databases and brokers. "Connect" adds a placeholder entry right away (no metrics
 * until the first scrape), POSTs it to /infra/... in the background and the lists refresh from the api
 * when it lands; a rejected one is removed again.
 */
const listeners = new Set<() => void>();
export const emit = () => listeners.forEach((f) => f());

export function useInfra() {
  const [, bump] = useState(0);
  useEffect(() => {
    const f = () => bump((n) => n + 1);
    listeners.add(f);
    return () => { listeners.delete(f); };
  }, []);
}

const slug = (s: string) => s.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'db';
const uniqueId = (base: string, taken: string[]) => {
  let id = base, i = 2;
  while (taken.includes(id)) id = `${base}-${i++}`;
  return id;
};

export interface DbDraft {
  engine: DbEngine;
  name: string;
  collector: Collector;
  /** agent: k8s namespace / host; cloud: resource id; direct: host:port */
  target: string;
  provider: string;
}

const PORT: Record<DbEngine, number> = { postgres: 5432, mysql: 3306, redis: 6379, mongodb: 27017, clickhouse: 9000 };
export const defaultPort = (e: DbEngine) => PORT[e];

/** "host:port" / "host" → parts (api DatabaseInput). */
function hostPort(target: string, fallbackPort: number): { host: string; port: number } {
  const m = /^(.*?)(?::(\d+))?$/.exec(target.trim());
  return { host: m?.[1] || target.trim(), port: m?.[2] ? Number(m[2]) : fallbackPort };
}

/** POST in the background; drop the optimistic row when the api says no. */
function persist<T>(arr: T[], row: T, path: string, body: unknown) {
  mutate('POST', path, body, [path]).catch((e: unknown) => {
    console.warn('[data] POST', path, e);
    const i = arr.indexOf(row);
    if (i >= 0) arr.splice(i, 1);
    emit();
  });
}

export function addDatabase(d: DbDraft): string {
  const id = uniqueId(slug(d.name), databases.map((x) => x.id));
  const kv = d.engine === 'redis';
  const db: Database = {
    // placeholder until the first scrape: no metrics yet
    id, name: d.name.trim() || id, engine: d.engine, version: '', host: d.target || `${id}.internal:${PORT[d.engine]}`,
    instanceClass: '', collector: d.collector,
    source: d.collector === 'cloud' ? d.provider : d.collector === 'agent' ? 'TaskTrooper agent' : 'Direct · read-only user',
    status: 'ok', cpu: 0, prevCpu: 0, memory: 0, disk: 0, diskGb: 0,
    connections: 0, maxConnections: 0, p95: 0, prevP95: 0, slowPerMin: 0,
    hitRatio: 0, evictionsPerMin: kv ? 0 : undefined,
    nodes: [],
    addedAt: Date.now(),
  };
  databases.push(db);
  emit();
  const { host, port } = hostPort(d.target || `${id}.internal`, PORT[d.engine]);
  persist(databases, db, '/infra/databases', {
    id, name: db.name, engine: d.engine, host, port, collector: d.collector,
    cloudResourceId: d.collector === 'cloud' ? d.target || undefined : undefined,
  });
  return id;
}

export interface BrokerDraft {
  kind: BrokerKind;
  name: string;
  endpoint: string;
  provider: string;
  vhost: string;
}

export function addCluster(d: BrokerDraft): string {
  const id = uniqueId(slug(d.name), clusters.map((x) => x.id));
  const managed = d.kind === 'sqs' || d.kind === 'pubsub';
  const c: Cluster = {
    // placeholder until the first scrape: version and nodes come from the api
    id, name: d.name.trim() || id, kind: d.kind, version: '',
    endpoint: d.endpoint, vhost: d.kind === 'rabbitmq' ? d.vhost || '/' : undefined, source: d.provider, collector: managed ? 'cloud' : 'direct',
    nodes: [],
    addedAt: Date.now(),
  };
  clusters.push(c);
  emit();
  persist(clusters, c, '/infra/clusters', { id, name: c.name, kind: d.kind, endpoint: d.endpoint, vhost: c.vhost, collector: c.collector });
  return id;
}

/** Where to connect for a test; credentials are sent once and never stored. */
export interface ConnTarget { host?: string; port?: number; endpoint?: string; user?: string; password?: string; database?: string; vhost?: string; tls?: boolean }
interface ConnTestResult { ok: boolean; message: string; latencyMs: number; version?: string }

/**
 * Connection test: POST
 * /infra/{databases|clusters}/test-connection with `target` (`host:port` for databases, the bootstrap
 * servers / management URL for brokers).
 */
export async function testConnection(what: 'db' | BrokerKind, engine?: DbEngine, target?: ConnTarget | string): Promise<string> {
  const t: ConnTarget = typeof target === 'string'
    ? (what === 'db' ? hostPort(target, PORT[engine ?? 'postgres']) : { endpoint: target })
    : target ?? {};
  if (what === 'sqs' || what === 'pubsub') return 'Managed queues are discovered by the cloud integration — no direct connection to test.';
  if (what === 'db' ? !t.host : !t.endpoint) return 'Fill in the host first — the test connects to it from the TaskTrooper backend.';
  try {
    const path = what === 'db' ? '/infra/databases/test-connection' : '/infra/clusters/test-connection';
    const r = await api<ConnTestResult>(path, { method: 'POST', body: JSON.stringify({ kind: what === 'db' ? engine ?? 'postgres' : what, ...t }) });
    return `${r.ok ? 'Connected' : 'Failed'} · ${r.message}${r.version ? ` · ${r.version}` : ''} (${Math.round(r.latencyMs)} ms)`;
  } catch (e) {
    return `Failed · ${e instanceof Error ? e.message : String(e)}`;
  }
}
