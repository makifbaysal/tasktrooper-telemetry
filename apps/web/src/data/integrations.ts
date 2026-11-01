/**
 * Cloud integrations (ADR-012): AWS, Google Cloud, Vercel, Neon. Each connection discovers resources
 * (databases, broker clusters, deploys) and pulls their metrics; discovered rows show up in the existing
 * Databases / Message queues / deploy marker views with `collector: 'cloud'`.
 *
 * Contracts §10–11: `GET /integrations` and `GET /deploys?range=7d` hydrate the lists below;
 * connect / test / sync / patch / delete go through `mutate`.
 */
import { deploysFor, services } from './apm';
import type { Cluster, Database } from './infra';
import { api, hydrateList, mutate, qs, registerHydrator, seg } from './remote';

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

export const integrations: Integration[] = [];

/** Vercel / CI / cloud deploy markers of the last 7 days. */
export const deployMarkers: DeployMarker[] = [];

// ------------------------------------------------------------------ lookups
export const integrationOf = (id?: string) => (id ? integrations.find((i) => i.id === id) : undefined);

/** The integration that discovered a Database / Cluster row: its `integrationId`, else a resource pointing at it. */
export function integrationFor(row: Pick<Database | Cluster, 'id' | 'integrationId'>): Integration | undefined {
  return integrationOf(row.integrationId) ?? integrations.find((i) => i.resources.some((r) => r.refId === row.id));
}

/** Deploys of one service, newest first: Vercel / CI markers plus versions seen on its spans. */
export function deploysOf(service: string): DeployMarker[] {
  const svc = services.find((s) => s.id === service || s.name === service);
  const names = new Set([service, svc?.id, svc?.name].filter(Boolean) as string[]);
  const own = deployMarkers.filter((d) => names.has(d.service));
  if (!svc) return own.sort((a, b) => b.ts - a.ts);
  // span-derived deploys of the service (api `/services/{s}/deploys`) merged with Vercel / CI markers
  const spans = deploysFor(svc.id, '7d').filter((d) => !own.some((o) => o.version === d.version))
    .map((d): DeployMarker => ({ ...d, source: 'spans', state: 'ready' }));
  return [...own, ...spans].sort((a, b) => b.ts - a.ts);
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
/** Neon branch / compute state of a database row, from the `meta` written by discovery. */
export function neonInfo(db: Database): NeonCompute | undefined {
  const meta = (db as Database & { meta?: Partial<NeonCompute> & { computeState?: NeonCompute['state'] } }).meta;
  if (meta && (meta.branch || meta.state || meta.computeState)) {
    return {
      project: meta.project ?? '—', branch: meta.branch ?? 'main', endpoint: meta.endpoint ?? db.host.split('.')[0], state: meta.state ?? meta.computeState ?? 'idle',
      cuMin: meta.cuMin ?? 0.25, cuMax: meta.cuMax ?? 1, cuNow: meta.cuNow ?? 0, storageGb: meta.storageGb ?? 0, storageLimitGb: meta.storageLimitGb ?? 0, activeHours: meta.activeHours ?? 0,
    };
  }
  return undefined;
}

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
  await Promise.all([
    hydrateList(integrations, '/integrations', (raw: RawIntegration[] | { items: RawIntegration[] }) => (Array.isArray(raw) ? raw : raw.items ?? []).map(mapIntegration)),
    hydrateList(deployMarkers, `/deploys${qs({ range: '7d' })}`, (raw: RawDeploy[]) => (Array.isArray(raw) ? raw : []).map(mapDeploy)),
  ]);
});

// ------------------------------------------------------------------ mutations
/** `POST /integrations/test` — checks the credentials and lists what it would discover. */
export async function testIntegration(d: IntegrationDraft): Promise<IntegrationTestResult> {
  return api<IntegrationTestResult>('/integrations/test', { method: 'POST', body: JSON.stringify(d) });
}

/** `POST /integrations` — store it; discovery starts in the background (status `pending` → `ok`/`error`). */
export async function connectIntegration(d: IntegrationDraft): Promise<Integration> {
  return mapIntegration(await mutate<RawIntegration>('POST', '/integrations', d, ['/integrations', '/databases', '/clusters', '/deploys']));
}

/** `POST /integrations/{id}/sync` — discover and pull metrics now. */
export async function syncIntegration(id: string): Promise<void> {
  await mutate('POST', `/integrations/${seg(id)}/sync`, undefined, ['/integrations']);
}

/** `PATCH /integrations/{id}` — rename, change config, disable / enable, or reconnect with a new secret. */
export async function updateIntegration(id: string, patch: Partial<Pick<Integration, 'name' | 'config' | 'status'>> & { secret?: string }): Promise<void> {
  await mutate('PATCH', `/integrations/${seg(id)}`, patch, ['/integrations', '/databases', '/clusters']);
}

/** `DELETE /integrations/{id}` — stops syncing; discovered rows stay (marked missing) so history is kept. */
export async function deleteIntegration(id: string): Promise<void> {
  await mutate('DELETE', `/integrations/${seg(id)}`, undefined, ['/integrations', '/databases', '/clusters']);
}
