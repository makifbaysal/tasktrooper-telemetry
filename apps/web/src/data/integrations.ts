/**
 * Cloud integrations (ADR-012): AWS, Google Cloud, Vercel, Neon. Each connection discovers resources
 * (databases, broker clusters, deploys) and pulls their metrics; discovered rows show up in the existing
 * Databases / Message queues / deploy marker views with `collector: 'cloud'`.
 *
 * api mode (contracts §10–11): `GET /integrations` and `GET /deploys?range=7d` hydrate the lists below;
 * connect / test / sync / patch / delete go through `mutate`. Mock mode keeps everything in memory.
 */
import { NOW, deploysFor, services } from './mock';
import { databases, type Cluster, type Database } from './infra';
import { api, hydrateList, invalidate, isApi, mutate, registerHydrator, seg } from './remote';

export type IntegrationProvider = 'aws' | 'gcp' | 'azure' | 'vercel' | 'neon';
export type IntegrationStatus = 'pending' | 'ok' | 'error' | 'disabled';

export const PROVIDERS: IntegrationProvider[] = ['aws', 'gcp', 'azure', 'vercel', 'neon'];

export const PROVIDER_LABEL: Record<IntegrationProvider, string> = {
  aws: 'Amazon Web Services', gcp: 'Google Cloud', azure: 'Microsoft Azure', vercel: 'Vercel', neon: 'Neon',
};
/** Short form for badges ("AWS · aws-prod"). */
export const PROVIDER_SHORT: Record<IntegrationProvider, string> = { aws: 'AWS', gcp: 'Google Cloud', azure: 'Azure', vercel: 'Vercel', neon: 'Neon' };

/** What each provider brings, in the words the UI uses. */
export const PROVIDER_CAPABILITIES: Record<IntegrationProvider, string[]> = {
  aws: ['RDS & Aurora metrics (CPU, memory, disk, connections, replica lag)', 'ElastiCache Redis', 'MSK broker metrics and consumer lag', 'SQS queue depth and age', 'Lambda invocations, errors, throttles, duration'],
  azure: ['Function Apps: executions, HTTP 5xx, response time, memory', 'Estimated cost from execution units'],
  gcp: ['Cloud SQL metrics', 'Memorystore Redis', 'Pub/Sub subscription backlog and oldest message age'],
  vercel: ['Deploy markers on every service chart', 'Runtime logs (log drain)', 'Traces (trace drain)', 'Speed Insights → Web Vitals', 'Web Analytics → page views'],
  neon: ['Projects, branches and compute endpoints as databases', 'Compute state, size and storage', 'Query stats via direct connection'],
};

export interface DiscoveredResource {
  kind: 'database' | 'cluster' | 'project' | 'queue' | 'subscription' | 'function';
  /** id of the Database / Cluster row it became, when it did */
  refId?: string;
  name: string;
  detail: string;
}

export interface Integration {
  id: string;
  provider: IntegrationProvider;
  name: string;
  status: IntegrationStatus;
  error?: string;
  lastSyncAt?: number;
  /** provider-specific, never contains secrets */
  config: Record<string, unknown>;
  resources: DiscoveredResource[];
  createdAt: number;
}

export interface IntegrationDraft {
  provider: IntegrationProvider;
  name: string;
  config: Record<string, unknown>;
  /** token / access key / service-account JSON — sent once, stored encrypted, never returned */
  secret?: string;
  /** azure (contracts §21): the client secret; the api also accepts it as `secret` */
  credentials?: { clientSecret: string };
}

export interface IntegrationTestResult {
  ok: boolean;
  message: string;
  resources?: DiscoveredResource[];
}

export interface DeployMarker {
  service: string;
  env: string;
  version: string;
  ts: number;
  source: 'spans' | 'vercel' | 'ci' | 'manual' | 'aws' | 'gcp';
  url?: string;
  commitMessage?: string;
  author?: string;
  state?: 'building' | 'ready' | 'error' | 'canceled';
}

export const DEPLOY_SOURCE_LABEL: Record<DeployMarker['source'], string> = {
  spans: 'SDK (service.version)', vercel: 'Vercel', ci: 'CI webhook', manual: 'Manual', aws: 'AWS', gcp: 'Google Cloud',
};

/** Vercel drain types (contracts §10). */
export const VERCEL_DRAINS = [
  { id: 'logs', label: 'Logs', detail: 'Runtime, build and edge logs → the mapped service\'s Logs tab' },
  { id: 'traces', label: 'Traces', detail: 'OpenTelemetry spans from Vercel Functions → traces and RED metrics' },
  { id: 'speed_insights', label: 'Speed Insights', detail: 'LCP, INP, CLS, FCP, TTFB → the app\'s Web Vitals' },
  { id: 'web_analytics', label: 'Web Analytics', detail: 'Page views → Pages tab (visitor ids are hashed)' },
] as const;

const MIN = 60_000;
const DAY = 24 * 60 * MIN;

export const integrations: Integration[] = [
  {
    id: 'int-aws', provider: 'aws', name: 'aws-prod', status: 'ok', lastSyncAt: NOW - 2 * MIN, createdAt: NOW - 40 * DAY,
    config: { roleArn: 'arn:aws:iam::123456789012:role/TaskTrooperReadOnly', externalId: 'tt-shopco-7f3a91', regions: ['eu-central-1', 'us-east-1'], services: ['rds', 'elasticache', 'msk', 'sqs', 'lambda'] },
    resources: [
      { kind: 'database', refId: 'orders-db', name: 'orders-db', detail: 'RDS PostgreSQL · db.r6g.2xlarge' },
      { kind: 'database', refId: 'payments-db', name: 'payments-db', detail: 'RDS PostgreSQL · db.r6g.xlarge' },
      { kind: 'database', refId: 'catalog-db', name: 'catalog-db', detail: 'RDS PostgreSQL · db.r6g.2xlarge' },
      { kind: 'database', refId: 'auth-db', name: 'auth-db', detail: 'RDS PostgreSQL · db.r6g.large' },
      { kind: 'database', refId: 'cache-main', name: 'cache-main', detail: 'ElastiCache Redis · 3 shards' },
      { kind: 'database', refId: 'cache-payments', name: 'cache-payments', detail: 'ElastiCache Redis · 1 replica' },
      { kind: 'database', refId: 'cache-bff', name: 'cache-bff', detail: 'ElastiCache Redis · 1 node' },
      { kind: 'cluster', refId: 'kafka-main', name: 'kafka-main', detail: 'MSK · 3 brokers' },
      { kind: 'cluster', refId: 'aws-sqs-eu-central-1', name: 'sqs-eu-central-1', detail: 'SQS · 3 queues' },
      { kind: 'function', refId: 'aws-eu-central-1-checkout-webhook', name: 'checkout-webhook', detail: 'Lambda · nodejs22.x · eu-central-1' },
      { kind: 'function', refId: 'aws-eu-central-1-order-processor', name: 'order-processor', detail: 'Lambda · python3.12 · eu-central-1' },
      { kind: 'function', refId: 'aws-eu-central-1-image-resize', name: 'image-resize', detail: 'Lambda · nodejs22.x · eu-central-1' },
      { kind: 'function', refId: 'aws-eu-central-1-clickstream-enricher', name: 'clickstream-enricher', detail: 'Lambda · java21 · eu-central-1' },
      { kind: 'function', refId: 'aws-us-east-1-nightly-report', name: 'nightly-report', detail: 'Lambda · python3.12 · us-east-1' },
    ],
  },
  {
    id: 'int-azure', provider: 'azure', name: 'azure-shop', status: 'ok', lastSyncAt: NOW - 4 * MIN, createdAt: NOW - 6 * DAY,
    config: { tenantId: '72f988bf-86f1-41af-91ab-2d7cd011db47', clientId: '0b5d1e7c-3c1a-4f7e-9a51-6c2f0e9d8a11', subscriptionIds: ['3f2a9c4e-0d6b-4b7a-8e15-9c1d2f3a4b5c'], resourceGroups: ['rg-shop-prod'] },
    resources: [
      { kind: 'function', refId: 'azure-shop-notifications', name: 'shop-notifications', detail: 'Function App · 3 functions · westeurope' },
      { kind: 'function', refId: 'azure-invoice-pdf', name: 'invoice-pdf', detail: 'Function App · 2 functions · westeurope' },
    ],
  },
  {
    id: 'int-gcp', provider: 'gcp', name: 'gcp-shop-prod', status: 'ok', lastSyncAt: NOW - 3 * MIN, createdAt: NOW - 9 * DAY,
    config: { projectIds: ['shop-prod'], services: ['pubsub'], clientEmail: 'tasktrooper@shop-prod.iam.gserviceaccount.com' },
    resources: [
      { kind: 'cluster', refId: 'gcp-pubsub-shop-prod', name: 'shop-prod', detail: 'Pub/Sub · 2 subscriptions' },
    ],
  },
  {
    id: 'int-vercel', provider: 'vercel', name: 'shop-team', status: 'ok', lastSyncAt: NOW - MIN, createdAt: NOW - 12 * DAY,
    config: { teamId: 'team_shop', projectMap: { 'shop-web': 'shop-web', 'landing': 'landing-web' }, drains: ['logs', 'speed_insights', 'web_analytics'] },
    resources: [
      { kind: 'project', name: 'shop-web', detail: 'Next.js · production + 14 previews' },
      { kind: 'project', name: 'landing', detail: 'Next.js · production → landing-web' },
    ],
  },
  {
    id: 'int-neon', provider: 'neon', name: 'neon-analytics', status: 'error', error: 'API key revoked (401) — reconnect to resume syncing',
    lastSyncAt: NOW - 3 * 60 * MIN, createdAt: NOW - 5 * DAY,
    config: { orgId: 'org-shopco', projectIds: ['bold-fog-123'] },
    resources: [{ kind: 'database', refId: 'neon-analytics-main', name: 'analytics/main', detail: 'Neon Postgres 17 · 0.25–2 CU · aws-eu-central-1' }],
  },
];

/** Vercel deploys of shop-web / landing-web over the last days (mock). */
export const deployMarkers: DeployMarker[] = [
  { service: 'shop-web', env: 'production', version: 'dpl_8f2c', ts: NOW - 45 * MIN, source: 'vercel', commitMessage: 'checkout: lazy-load payment widget', author: 'deniz', state: 'ready', url: 'https://shop-web-8f2c.vercel.app' },
  { service: 'shop-web', env: 'production', version: 'dpl_77a1', ts: NOW - 7 * 60 * MIN, source: 'vercel', commitMessage: 'pdp: new image CDN', author: 'mert', state: 'ready', url: 'https://shop-web-77a1.vercel.app' },
  { service: 'shop-web', env: 'production', version: 'dpl_5e10', ts: NOW - 30 * 60 * MIN, source: 'vercel', commitMessage: 'search: debounce suggestions', author: 'ayse', state: 'error', url: 'https://shop-web-5e10.vercel.app' },
  { service: 'landing-web', env: 'production', version: 'dpl_c3d4', ts: NOW - 3 * 60 * MIN, source: 'vercel', commitMessage: 'campaign: black friday hero', author: 'selin', state: 'ready', url: 'https://landing-c3d4.vercel.app' },
];

// ------------------------------------------------------------------ lookups
export const integrationOf = (id?: string) => (id ? integrations.find((i) => i.id === id) : undefined);

/** The integration that discovered a Database / Cluster row: its `integrationId`, else a resource pointing at it. */
export function integrationFor(row: Pick<Database | Cluster, 'id' | 'integrationId'>): Integration | undefined {
  return integrationOf(row.integrationId) ?? integrations.find((i) => i.resources.some((r) => r.refId === row.id));
}

function bumpVersion(v: string, back: number) {
  const m = v.match(/^(v?\d+\.\d+\.)(\d+)$/);
  return m ? `${m[1]}${Math.max(0, Number(m[2]) - back)}` : `${v}-${back}`;
}
const MOCK_COMMITS: [string, string, string, number][] = [
  ['a91f3c2', 'feat: split shipment builder', 'deniz.k', 2],
  ['7c0d9e1', 'fix: cart rounding', 'mert.a', 26],
  ['13fe2aa', 'chore: bump pg driver', 'deniz.k', 74],
];

/** Deploys of one service, newest first: Vercel / CI markers plus versions seen on its spans. */
export function deploysOf(service: string): DeployMarker[] {
  const svc = services.find((s) => s.id === service || s.name === service);
  const names = new Set([service, svc?.id, svc?.name].filter(Boolean) as string[]);
  const own = deployMarkers.filter((d) => names.has(d.service));
  if (isApi && svc) {
    // span-derived deploys of the service (api `/services/{s}/deploys`) merged with Vercel / CI markers
    const spans = deploysFor(svc.id, '7d').filter((d) => !own.some((o) => o.version === d.version))
      .map((d): DeployMarker => ({ ...d, source: 'spans', state: 'ready' }));
    return [...own, ...spans].sort((a, b) => b.ts - a.ts);
  }
  if (isApi || !svc || own.length || svc.type === 'web' || svc.type === 'mobile') return own.sort((a, b) => b.ts - a.ts);
  // mock: backend services report their version on every span; a new version = a deploy
  return MOCK_COMMITS.map(([sha, msg, author, hoursAgo], i): DeployMarker => ({
    service: svc.name, env: 'production', version: i === 0 ? svc.version : bumpVersion(svc.version, i), ts: NOW - hoursAgo * 60 * MIN,
    source: 'spans', commitMessage: `${msg} (${sha})`, author, state: 'ready',
  }));
}

/** Chart `events` for the deploys inside the visible range. */
export function deployEvents(service: string, from: number, to = Number.POSITIVE_INFINITY) {
  return deploysOf(service).filter((d) => d.ts >= from && d.ts <= to && d.state !== 'error' && d.state !== 'canceled')
    .map((d) => ({ ts: d.ts, label: d.version.replace(/^dpl_/, ''), detail: [d.commitMessage, d.author && `by ${d.author}`, DEPLOY_SOURCE_LABEL[d.source]].filter(Boolean).join(' · ') }));
}

// ------------------------------------------------------------------ Neon
export interface NeonCompute {
  project: string;
  branch: string;
  endpoint: string;
  state: 'active' | 'idle' | 'init';
  cuMin: number;
  cuMax: number;
  cuNow: number;
  storageGb: number;
  storageLimitGb: number;
  /** active compute time this billing period, hours */
  activeHours: number;
}
const NEON_MOCK: Record<string, NeonCompute> = {
  'neon-analytics-main': { project: 'analytics (bold-fog-123)', branch: 'main', endpoint: 'ep-bold-fog-123456', state: 'idle', cuMin: 0.25, cuMax: 2, cuNow: 0, storageGb: 6.4, storageLimitGb: 50, activeHours: 41 },
};
/** Neon branch / compute state of a database row; api mode reads `meta` written by discovery. */
export function neonInfo(db: Database): NeonCompute | undefined {
  const meta = (db as Database & { meta?: Partial<NeonCompute> & { computeState?: NeonCompute['state'] } }).meta;
  if (meta && (meta.branch || meta.state || meta.computeState)) {
    return {
      project: meta.project ?? '—', branch: meta.branch ?? 'main', endpoint: meta.endpoint ?? db.host.split('.')[0], state: meta.state ?? meta.computeState ?? 'idle',
      cuMin: meta.cuMin ?? 0.25, cuMax: meta.cuMax ?? 1, cuNow: meta.cuNow ?? 0, storageGb: meta.storageGb ?? 0, storageLimitGb: meta.storageLimitGb ?? 0, activeHours: meta.activeHours ?? 0,
    };
  }
  return NEON_MOCK[db.id] ?? (integrationFor(db)?.provider === 'neon' ? { project: '—', branch: 'main', endpoint: db.host.split('.')[0], state: 'idle', cuMin: 0.25, cuMax: 1, cuNow: 0, storageGb: 0, storageLimitGb: 0, activeHours: 0 } : undefined);
}

// ------------------------------------------------------------------ mock rows discovered by the integrations
/**
 * Mock only: the Neon compute the neon integration above discovered (SQS / Pub/Sub clusters live in the
 * infra mock). Idempotent; skipped when the infra mock already has a Neon row.
 */
function seedDiscovered() {
  if (isApi) return;
  if (!databases.some((d) => d.id === 'neon-analytics-main') && !databases.some((d) => integrationOf(d.integrationId)?.provider === 'neon')) {
    databases.push({
      id: 'neon-analytics-main', name: 'analytics', engine: 'postgres', version: '17.2', host: 'ep-bold-fog-123456.eu-central-1.aws.neon.tech',
      instanceClass: '0.25–2 CU (autoscaling)', collector: 'cloud', source: 'Neon · aws-eu-central-1', status: 'ok', integrationId: 'int-neon',
      cpu: 0.04, prevCpu: 0.06, memory: 0.18, disk: 0.13, diskGb: 50, connections: 3, maxConnections: 112,
      p95: 22, prevP95: 24, slowPerMin: 0, hitRatio: 0.97,
      nodes: [{ id: 'ep-bold-fog-123456', role: 'primary', zone: 'aws-eu-central-1', cpu: 0.04, memory: 0.18, connections: 3 }],
    });
  }
}
seedDiscovered();

// ------------------------------------------------------------------ api mapping
type RawIntegration = Omit<Integration, 'id' | 'resources' | 'lastSyncAt' | 'createdAt'> & {
  id: string | number; resources?: DiscoveredResource[] | number; lastSyncAt?: number | string | null; createdAt?: number | string;
};
const toMs = (v: number | string | null | undefined) => (v === null || v === undefined ? undefined : typeof v === 'number' ? v : Date.parse(v));
function mapIntegration(r: RawIntegration): Integration {
  return {
    ...r, id: String(r.id), config: r.config ?? {},
    resources: Array.isArray(r.resources) ? r.resources : [],
    lastSyncAt: toMs(r.lastSyncAt), createdAt: toMs(r.createdAt) ?? Date.now(),
  };
}
type RawDeploy = Partial<DeployMarker> & { service: string; env: string; version: string; ts: number | string };
const mapDeploy = (d: RawDeploy): DeployMarker => ({ ...d, ts: toMs(d.ts) ?? 0, source: d.source ?? 'spans' });

registerHydrator(async () => {
  if (!isApi) return;
  await Promise.all([
    hydrateList(integrations, '/integrations', (raw: RawIntegration[] | { items: RawIntegration[] }) => (Array.isArray(raw) ? raw : raw.items ?? []).map(mapIntegration)),
    hydrateList(deployMarkers, '/deploys?range=7d', (raw: RawDeploy[]) => (Array.isArray(raw) ? raw : []).map(mapDeploy)),
  ]);
});

// ------------------------------------------------------------------ mutations
const wait = (ms: number) => new Promise((r) => setTimeout(r, ms));
const changed = () => invalidate('/integrations');

/** What a successful connection of this draft would discover (mock). */
function mockResources(d: IntegrationDraft): DiscoveredResource[] {
  const c = d.config as { services?: string[]; regions?: string[]; projectIds?: string[]; projectMap?: Record<string, string> };
  const region = c.regions?.[0] ?? 'eu-central-1';
  switch (d.provider) {
    case 'aws': return (c.services ?? []).map((s) => ({
      rds: { kind: 'database' as const, name: `${d.name}-postgres`, detail: `RDS PostgreSQL · ${region}` },
      elasticache: { kind: 'database' as const, name: `${d.name}-redis`, detail: `ElastiCache Redis · ${region}` },
      msk: { kind: 'cluster' as const, name: `${d.name}-msk`, detail: `MSK · 3 brokers · ${region}` },
      sqs: { kind: 'queue' as const, name: `${d.name}-jobs`, detail: `SQS · ${region}` },
      lambda: { kind: 'function' as const, name: `${d.name}-api-handler`, detail: `Lambda · nodejs22.x · ${region}` },
    }[s] ?? { kind: 'database' as const, name: s, detail: region }));
    case 'gcp': return (c.projectIds ?? ['my-project']).flatMap((p) => (c.services ?? []).map((s) => ({
      kind: s === 'pubsub' ? 'subscription' as const : 'database' as const,
      name: `${p}/${s === 'cloudsql' ? 'main-sql' : s === 'memorystore' ? 'cache' : 'events-sub'}`,
      detail: s === 'cloudsql' ? 'Cloud SQL PostgreSQL 16' : s === 'memorystore' ? 'Memorystore Redis 7.2' : 'Pub/Sub subscription',
    })));
    case 'vercel': {
      const map = c.projectMap && Object.keys(c.projectMap).length ? c.projectMap : { 'shop-web': '', landing: '', docs: '' };
      return Object.keys(map).map((p) => ({ kind: 'project', name: p, detail: map[p] ? `Vercel project → ${map[p]}` : 'Next.js · production' }));
    }
    case 'azure': return ((d.config as { resourceGroups?: string[] }).resourceGroups?.length ? (d.config as { resourceGroups: string[] }).resourceGroups : ['rg-prod']).map((g) => ({
      kind: 'function' as const, name: `${g.replace(/^rg-/, '')}-functions`, detail: `Function App · ${g} · westeurope`,
    }));
    case 'neon': return (c.projectIds?.length ? c.projectIds : ['bold-fog-123', 'quiet-river-456']).map((p) => ({ kind: 'database', name: `${p}/main`, detail: 'Neon Postgres 17 · 0.25–2 CU' }));
  }
}

/** `POST /integrations/test` — checks the credentials and lists what it would discover. */
export async function testIntegration(d: IntegrationDraft): Promise<IntegrationTestResult> {
  if (isApi) return api<IntegrationTestResult>('/integrations/test', { method: 'POST', body: JSON.stringify(d) });
  await wait(900);
  if (!d.secret && d.provider !== 'aws') return { ok: false, message: d.provider === 'gcp' ? 'No service account key — paste the JSON key' : d.provider === 'azure' ? 'No client secret — paste the service principal secret' : 'No token — paste an access token' };
  if (d.provider === 'aws' && !d.secret && !String(d.config.roleArn ?? '').startsWith('arn:aws:iam::')) return { ok: false, message: 'Role ARN looks wrong — expected arn:aws:iam::<account>:role/<name>' };
  const resources = mockResources(d);
  const what = { aws: 'AssumeRole OK · CloudWatch and Describe* readable', gcp: 'Service account OK · monitoring.viewer granted', azure: 'Service principal OK · Reader + Monitoring Reader', vercel: 'Token OK · team readable', neon: 'API key OK · projects readable' }[d.provider];
  return { ok: true, message: `${what} · ${resources.length} resource${resources.length === 1 ? '' : 's'} found`, resources };
}

/** `POST /integrations` — store it; discovery starts in the background (status `pending` → `ok`/`error`). */
export async function connectIntegration(d: IntegrationDraft): Promise<Integration> {
  if (isApi) return mapIntegration(await mutate<RawIntegration>('POST', '/integrations', d, ['/integrations', '/databases', '/clusters', '/deploys']));
  await wait(400);
  let id = `int-${d.provider}-${d.name.trim().toLowerCase().replace(/[^a-z0-9]+/g, '-') || 'new'}`;
  while (integrations.some((i) => i.id === id)) id += '-2';
  const it: Integration = { id, provider: d.provider, name: d.name.trim() || d.provider, status: 'pending', config: d.config, resources: [], createdAt: Date.now() };
  integrations.push(it);
  changed();
  setTimeout(() => { it.status = 'ok'; it.lastSyncAt = Date.now(); it.resources = mockResources(d); changed(); }, 2500);
  return it;
}

/** `POST /integrations/{id}/sync` — discover and pull metrics now. */
export async function syncIntegration(id: string): Promise<void> {
  if (isApi) { await mutate('POST', `/integrations/${seg(id)}/sync`, undefined, ['/integrations']); return; }
  const it = integrationOf(id);
  if (!it || it.status === 'disabled') return;
  it.status = 'pending';
  changed();
  await wait(1200);
  if (it.error) it.status = 'error';
  else { it.status = 'ok'; it.lastSyncAt = Date.now(); }
  changed();
}

/** `PATCH /integrations/{id}` — rename, change config, disable / enable, or reconnect with a new secret. */
export async function updateIntegration(id: string, patch: Partial<Pick<Integration, 'name' | 'config' | 'status'>> & { secret?: string }): Promise<void> {
  if (isApi) { await mutate('PATCH', `/integrations/${seg(id)}`, patch, ['/integrations', '/databases', '/clusters']); return; }
  const it = integrationOf(id);
  if (!it) return;
  if (patch.name) it.name = patch.name;
  if (patch.config) it.config = { ...it.config, ...patch.config };
  if (patch.status === 'disabled') it.status = 'disabled';
  if (patch.secret || (patch.status && patch.status !== 'disabled')) {
    // a new secret or re-enable: sync again
    it.error = undefined;
    it.status = 'pending';
    changed();
    await wait(1200);
    it.status = 'ok';
    it.lastSyncAt = Date.now();
  }
  changed();
}

/** `DELETE /integrations/{id}` — stops syncing; discovered rows stay (marked missing) so history is kept. */
export async function deleteIntegration(id: string): Promise<void> {
  if (isApi) { await mutate('DELETE', `/integrations/${seg(id)}`, undefined, ['/integrations', '/databases', '/clusters']); return; }
  const i = integrations.findIndex((x) => x.id === id);
  if (i >= 0) integrations.splice(i, 1);
  changed();
}
