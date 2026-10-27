/**
 * Code intelligence data (ADR-014, .ai/contracts.md §13, §16): connected repositories, the code inventory and
 * monitor suggestions. The lists are the arrays exported from ./mock (other screens read them too); in api mode
 * they are filled from `GET /repos`, `/inventory`, `/suggestions?state=proposed`. Missing endpoints (404) leave
 * them empty — screens show empty states instead of failing.
 */
import { NOW, inventory, monitors, repos, services, suggestions } from './mock';
import { API_URL, ApiError, api, invalidate, isApi, registerHydrator } from './remote';
import type { Criticality, InventoryItem, Monitor, Repo, Suggestion } from './types';

// ---------------------------------------------------------------- types

export type RepoProvider = 'github' | 'local';

export type RepoStatus = Repo['status'] | 'disconnected';

/** What the api returns for a repository on top of the UI `Repo`. */
export interface RepoInfo extends Omit<Repo, 'status'> {
  /** disconnected = removed from the GitHub App installation */
  status: RepoStatus;
  provider?: RepoProvider;
  installationId?: number;
  /** files changed by the last push, analysis pending (incremental) */
  pendingPaths?: string[];
  /** GitHub App webhook delivers pushes for this repo */
  webhookActive?: boolean;
  /** analysis failure (status = error) */
  error?: string;
  /** browse URL of the default branch, e.g. https://github.com/org/repo */
  url?: string;
  localPath?: string;
  /** LLM step of the last analysis (api: `analysis.llm`, contracts §19) */
  llm?: RepoLlm;
}

/** quota_exceeded: the AI quota stopped the LLM step; the rest of the inventory kept static criticality. */
export interface RepoLlm { status: 'ok' | 'disabled' | 'quota_exceeded'; reason?: string; credits: number; skipped?: number }

export interface FailureMode { title: string; impact?: string; mitigation?: string }

/** Inventory row as the api returns it: + dead / shadow / summary / llm output. */
export interface InventoryRow extends InventoryItem {
  /** in code, no traffic in 30 days */
  dead?: boolean;
  /** traffic seen, no handler in any connected repo (`file` empty) */
  shadow?: boolean;
  summary?: string;
  failureModes?: FailureMode[];
  /** requests/min observed (shadow rows) */
  rpm?: number;
  repoId?: string;
}

export interface SuggestionRow extends Suggestion { repoId?: string }

export interface ConnectRepoInput {
  provider: RepoProvider;
  /** owner/name for GitHub, a display name for local */
  fullName: string;
  defaultBranch?: string;
  token?: string;
  installationId?: number;
  localPath?: string;
}

// ---------------------------------------------------------------- availability

/** false when the api has no code-intelligence endpoints yet */
export let codeIntelAvailable = !isApi;
const notify = () => invalidate('\u0000');

// ---------------------------------------------------------------- mapping

const CRIT = new Set<Criticality>(['critical', 'important', 'low']);
const crit = (c: unknown): Criticality => {
  const s = String(c ?? '').toLowerCase();
  if (s === 'high') return 'important';
  if (s === 'medium') return 'low';
  return CRIT.has(s as Criticality) ? (s as Criticality) : 'low';
};
const toMs = (v: unknown): number => {
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
  if (typeof v === 'string') { const t = Date.parse(v); return Number.isFinite(t) ? t : 0; }
  return 0;
};

interface RawRepo {
  id: string | number; name?: string; fullName?: string; branch?: string; defaultBranch?: string; sha?: string;
  analyzedAt?: number | string | null; services?: string[] | null; language?: string; languages?: string[] | null;
  status?: string; error?: string | null; provider?: string; url?: string; localPath?: string;
  installationId?: number | string | null; pendingPaths?: string[] | null; pending_paths?: string[] | null; webhookActive?: boolean | null;
  analysis?: { llm?: { status?: string; reason?: string | null; credits?: number | null; skipped?: number | null } | null } | null;
}
const STATUSES = new Set<RepoStatus>(['ready', 'analyzing', 'error', 'disconnected']);
const mapRepo = (r: RawRepo): RepoInfo => ({
  id: String(r.id), name: r.name ?? r.fullName ?? String(r.id), branch: r.branch ?? r.defaultBranch ?? 'main', sha: r.sha ?? '',
  analyzedAt: toMs(r.analyzedAt), services: r.services ?? [], language: r.language ?? (r.languages ?? []).join(', '),
  status: STATUSES.has(r.status as RepoStatus) ? (r.status as RepoStatus) : r.status === 'pending' || r.status === 'queued' ? 'analyzing' : 'ready',
  error: r.error ?? undefined, provider: r.provider === 'local' ? 'local' : 'github', localPath: r.localPath,
  url: r.url ?? (r.provider !== 'local' && (r.fullName ?? r.name)?.includes('/') ? `https://github.com/${r.fullName ?? r.name}` : undefined),
  installationId: r.installationId != null ? Number(r.installationId) : undefined,
  pendingPaths: r.pendingPaths ?? r.pending_paths ?? undefined,
  webhookActive: r.webhookActive ?? (r.installationId != null ? true : undefined),
  llm: r.analysis?.llm?.status === 'ok' || r.analysis?.llm?.status === 'disabled' || r.analysis?.llm?.status === 'quota_exceeded'
    ? { status: r.analysis.llm.status, reason: r.analysis.llm.reason ?? undefined, credits: r.analysis.llm.credits ?? 0, skipped: r.analysis.llm.skipped ?? undefined }
    : undefined,
});

interface RawItem extends Omit<InventoryRow, 'criticality' | 'id' | 'line' | 'failureModes'> {
  id: string | number; criticality?: string; line?: number | null;
  failureModes?: (FailureMode | string)[] | null;
  /** LLM interpretation (api: `analysis`, contract: `llm`) */
  llm?: { failure_modes?: (FailureMode | string)[]; failureModes?: (FailureMode | string)[] } | null;
  analysis?: { failure_modes?: (FailureMode | string)[]; failureModes?: (FailureMode | string)[]; summary?: string } | null;
}
const mapFailure = (f: FailureMode | string | Record<string, unknown>): FailureMode => {
  if (typeof f === 'string') return { title: f };
  const o = f as Record<string, unknown>;
  return { title: String(o.title ?? o.mode ?? o.name ?? o.description ?? ''), impact: o.impact as string | undefined, mitigation: (o.mitigation ?? o.fix) as string | undefined };
};
const mapItem = (r: RawItem): InventoryRow => ({
  ...r, id: String(r.id), criticality: crit(r.criticality), line: r.line ?? 0, file: r.file ?? '', service: r.service ?? '',
  observed: !!r.observed, monitored: !!r.monitored,
  summary: r.summary ?? r.analysis?.summary,
  failureModes: (r.failureModes ?? r.analysis?.failure_modes ?? r.analysis?.failureModes ?? r.llm?.failure_modes ?? r.llm?.failureModes ?? []).map(mapFailure).filter((f) => f.title),
});

interface RawSuggestion extends Omit<Suggestion, 'criticality' | 'id'> { id: string | number; criticality?: string }
const mapSuggestion = (r: RawSuggestion): SuggestionRow => ({ ...r, id: String(r.id), criticality: crit(r.criticality), calibrated: !!r.calibrated, confidence: r.confidence ?? 0 });

const unavailable = (e: unknown) => e instanceof ApiError && (e.status === 404 || e.status === 405 || e.status === 501);

async function loadAll() {
  try {
    const [rp, inv, sg] = await Promise.all([
      api<RawRepo[] | null>('/repos'),
      api<RawItem[] | null>('/inventory'),
      api<RawSuggestion[] | null>('/suggestions?state=proposed'),
    ]);
    repos.splice(0, repos.length, ...((rp ?? []).map(mapRepo) as unknown as Repo[]));
    inventory.splice(0, inventory.length, ...(inv ?? []).map(mapItem));
    suggestions.splice(0, suggestions.length, ...(sg ?? []).map(mapSuggestion));
    codeIntelAvailable = true;
  } catch (e) {
    // never block startup: code intelligence is optional
    if (unavailable(e)) codeIntelAvailable = false;
    else console.warn('[data] code intelligence', e);
  }
}

if (isApi) registerHydrator(loadAll);

/** Refresh repos / inventory / suggestions now (api) and re-render. */
export async function refreshCodeIntel() {
  if (isApi) await loadAll();
  notify();
}

// ---------------------------------------------------------------- GitHub App (ADR-017, contracts §18)

export interface GithubInstallation { id: string; account: string; installationId: number }
export interface GithubRepoOption { fullName: string; defaultBranch: string; private: boolean; language?: string; added: boolean }

const MOCK_INSTALLATION: GithubInstallation = { id: 'int-github', account: 'shopco', installationId: 51234567 };
let mockInstalled = true;

/** Full-page redirect target that starts the GitHub App installation. */
export const githubInstallUrl = () => `${API_URL}/api/v1/integrations/github/install`;

/** GitHub App installations of this project (integrations with provider=github). */
export async function githubInstallations(): Promise<GithubInstallation[]> {
  if (!isApi) return mockInstalled ? [MOCK_INSTALLATION] : [];
  try {
    const raw = await api<{ id: string | number; provider: string; name?: string; config?: { installationId?: number | string; account?: string } | null }[] | null>('/integrations');
    return (raw ?? []).filter((i) => i.provider === 'github').map((i) => ({
      id: String(i.id), account: i.config?.account ?? i.name ?? 'GitHub', installationId: Number(i.config?.installationId ?? 0),
    }));
  } catch (e) {
    if (!unavailable(e)) console.warn('[data] /integrations', e);
    return [];
  }
}

/** Mock only: pretend the GitHub App was just installed (the real flow is a redirect). */
export function mockInstallGithub(): GithubInstallation { mockInstalled = true; return MOCK_INSTALLATION; }

const MOCK_GH_REPOS: Omit<GithubRepoOption, 'added'>[] = [
  { fullName: 'shopco/orders-service', defaultBranch: 'main', private: true, language: 'TypeScript' },
  { fullName: 'shopco/payments', defaultBranch: 'main', private: true, language: 'Go' },
  { fullName: 'shopco/storefront', defaultBranch: 'main', private: true, language: 'TypeScript' },
  { fullName: 'shopco/catalog-api', defaultBranch: 'main', private: true, language: 'Kotlin' },
  { fullName: 'shopco/auth', defaultBranch: 'main', private: true, language: 'Go' },
  { fullName: 'shopco/email-worker', defaultBranch: 'main', private: true, language: 'Python' },
  { fullName: 'shopco/mobile-app', defaultBranch: 'develop', private: true, language: 'Swift' },
  { fullName: 'shopco/infra', defaultBranch: 'main', private: true, language: 'HCL' },
  { fullName: 'shopco/design-tokens', defaultBranch: 'main', private: false, language: 'CSS' },
];

/** Repos the installation can read: `GET /integrations/github/{id}/repos?q=`. */
export async function githubRepos(integrationId: string, q = ''): Promise<GithubRepoOption[]> {
  const added = (name: string) => repos.some((r) => r.name.toLowerCase() === name.toLowerCase());
  if (!isApi) {
    await new Promise((r) => setTimeout(r, 250));
    const needle = q.trim().toLowerCase();
    return MOCK_GH_REPOS.filter((r) => !needle || r.fullName.toLowerCase().includes(needle)).map((r) => ({ ...r, added: added(r.fullName) }));
  }
  const raw = await api<Partial<GithubRepoOption>[] | null>(`/integrations/github/${encodeURIComponent(integrationId)}/repos${q.trim() ? `?q=${encodeURIComponent(q.trim())}` : ''}`);
  return (raw ?? []).map((r) => ({
    fullName: r.fullName ?? '', defaultBranch: r.defaultBranch ?? 'main', private: !!r.private, language: r.language, added: !!r.added || added(r.fullName ?? ''),
  }));
}

// ---------------------------------------------------------------- mock enrichment

/** Repos, inventory rows and suggestions with their optional api fields. */
export const repoList = () => repos as unknown as RepoInfo[];
export const inventoryRows = () => inventory as InventoryRow[];
export const suggestionRows = () => suggestions as SuggestionRow[];

const MOCK_SUMMARY: Record<string, { summary: string; failureModes: FailureMode[] }> = {
  critical: {
    summary: 'Writes order or payment state inside one database transaction and calls an external provider on the same path.',
    failureModes: [
      { title: 'Provider timeout leaves the charge in an unknown state', impact: 'Customer charged without an order', mitigation: 'Idempotency key + reconcile job' },
      { title: 'Transaction rollback after a successful external call', impact: 'Inconsistent stock / refund', mitigation: 'Outbox pattern' },
    ],
  },
  important: {
    summary: 'Part of a user-facing flow; failures are retried by the caller or degrade gracefully.',
    failureModes: [{ title: 'Slow downstream response under load', impact: 'Page waits on the request', mitigation: 'Timeout + cached fallback' }],
  },
  low: {
    summary: 'Read-only or internal path without lasting side effects.',
    failureModes: [],
  },
};

if (!isApi) {
  const rows = inventory as InventoryRow[];
  for (const r of rows) {
    const m = MOCK_SUMMARY[r.criticality];
    r.summary ??= m.summary;
    r.failureModes ??= m.failureModes;
    r.dead ??= !r.observed;
  }
  rows.push(
    { id: 'inv-s1', kind: 'endpoint', key: 'GET /v0/orders/legacy', service: 'orders-api', file: '', line: 0, criticality: 'important', observed: true, monitored: false, shadow: true, rpm: 14,
      summary: 'Requests arrive for a route no connected repository declares — an old client, a proxy rewrite, or a service whose repo is not connected.' },
    { id: 'inv-s2', kind: 'endpoint', key: 'POST /internal/cache/flush', service: 'catalog-api', file: '', line: 0, criticality: 'low', observed: true, monitored: false, shadow: true, rpm: 2 },
  );
  const rp = repoList();
  rp.forEach((r) => { r.provider = 'github'; r.url = `https://github.com/${r.name}`; r.installationId = MOCK_INSTALLATION.installationId; r.webhookActive = true; });
  const pay = rp.find((r) => r.id === 'rp2');
  const ord = rp.find((r) => r.id === 'rp1');
  if (ord) ord.llm = { status: 'quota_exceeded', reason: 'user_limit', credits: 212.4, skipped: 18 };
  if (pay) { pay.status = 'analyzing'; pay.pendingPaths = ['internal/refunds/handler.go', 'internal/webhooks/stripe.go']; }
  rp.push(
    { id: 'rp4', name: 'shopco/legacy-admin', branch: 'master', sha: '1f0e9d2', analyzedAt: NOW - 6 * 86400_000, services: [], language: 'PHP', status: 'disconnected', provider: 'github',
      url: 'https://github.com/shopco/legacy-admin', installationId: MOCK_INSTALLATION.installationId, error: 'Removed from the GitHub App installation — re-add it on GitHub to resume analysis.' },
    { id: 'rp5', name: 'shopco/email-worker', branch: 'main', sha: '77ab310', analyzedAt: NOW - 3 * 3600_000, services: ['email-worker'], language: 'Python', status: 'error', provider: 'github',
      url: 'https://github.com/shopco/email-worker', installationId: MOCK_INSTALLATION.installationId, webhookActive: true, error: 'Analysis failed: worker/tasks/__init__.py could not be parsed (SyntaxError at line 41).' },
  );
  // first analysis of the storefront finishes while the page is open
  const sf = rp.find((r) => r.id === 'rp3');
  if (sf) setTimeout(() => { sf.status = 'ready'; sf.analyzedAt = Date.now(); notify(); }, 15000);
  // the push is analyzed incrementally
  setTimeout(() => { if (pay) { pay.status = 'ready'; pay.pendingPaths = undefined; pay.analyzedAt = Date.now(); pay.sha = '9c41aa0'; notify(); } }, 8000);
}

// ---------------------------------------------------------------- links

/** Link to the handler on the repo host (GitHub blob URL) when known. */
export function codeLink(item: Pick<InventoryItem, 'file' | 'line' | 'service'>): string | undefined {
  if (!item.file) return undefined;
  const r = repoList().find((x) => x.services.includes(item.service));
  if (!r || r.provider === 'local' || !r.url) return undefined;
  return `${r.url}/blob/${r.sha || r.branch}/${item.file}#L${item.line}`;
}

/** Link to the handler of a suggestion's `codeRef` ("path/file.ts:42"). */
export function codeRefLink(s: Pick<Suggestion, 'codeRef' | 'target'>): string | undefined {
  const [file, line] = s.codeRef.split(':');
  const svc = s.target.split(' · ')[0].replace(/ logs$/, '');
  return codeLink({ file, line: Number(line) || 1, service: svc });
}

// ---------------------------------------------------------------- mutations

const mockTimers = new Set<ReturnType<typeof setTimeout>>();
function finishAnalysisSoon(r: RepoInfo, ms = 4000) {
  const t = setTimeout(() => {
    mockTimers.delete(t);
    r.status = 'ready';
    r.error = undefined;
    r.analyzedAt = Date.now();
    r.sha ||= Math.random().toString(16).slice(2, 9);
    notify();
  }, ms);
  mockTimers.add(t);
}

/** Connect a repository; analysis starts right away. */
export async function connectRepo(input: ConnectRepoInput): Promise<RepoInfo> {
  if (isApi) {
    const raw = await api<RawRepo>('/repos', { method: 'POST', body: JSON.stringify(input) });
    await refreshCodeIntel();
    return raw ? mapRepo(raw) : repoList()[0];
  }
  const name = input.fullName.trim();
  if (input.provider === 'github' && !/^[\w.-]+\/[\w.-]+$/.test(name)) throw new Error('Use the owner/name form, e.g. shopco/orders-service');
  if (repos.some((r) => r.name === name)) throw new Error(`${name} is already connected`);
  if (input.provider === 'github' && !input.token && !input.installationId) throw new Error('Add a GitHub token or the GitHub App installation id');
  const lang = MOCK_GH_REPOS.find((x) => x.fullName === name)?.language;
  const svc = services.find((s) => name.toLowerCase().includes(s.id.split('-')[0]));
  const r: RepoInfo = {
    id: `rp${Date.now().toString(36)}`, name, branch: input.defaultBranch || 'main', sha: '', analyzedAt: 0,
    services: svc ? [svc.name] : [], language: lang ?? '—', status: 'analyzing', provider: input.provider,
    url: input.provider === 'github' ? `https://github.com/${name}` : undefined, localPath: input.localPath,
    installationId: input.installationId, webhookActive: !!input.installationId,
  };
  repoList().push(r);
  notify();
  finishAnalysisSoon(r);
  return r;
}

export async function deleteRepo(id: string) {
  if (isApi) { await api(`/repos/${encodeURIComponent(id)}`, { method: 'DELETE' }); await refreshCodeIntel(); return; }
  const k = repos.findIndex((r) => r.id === id);
  if (k >= 0) repos.splice(k, 1);
  notify();
}

export async function analyzeRepo(id: string) {
  if (isApi) { await api(`/repos/${encodeURIComponent(id)}/analyze`, { method: 'POST' }); await refreshCodeIntel(); return; }
  const r = repoList().find((x) => x.id === id);
  if (!r) return;
  r.status = 'analyzing';
  notify();
  finishAnalysisSoon(r, 3000);
}

/** Suggestions accepted in this session (they leave the proposed list), with the monitor each created. */
export const acceptedSuggestions = new Map<string, { suggestion: SuggestionRow; monitor: Pick<Monitor, 'id' | 'name' | 'target'> }>();

/** Accept → a monitor is created (source=suggestion). Returns the new monitor. */
export async function acceptSuggestion(id: string): Promise<Pick<Monitor, 'id' | 'name' | 'target'>> {
  const s = suggestions.find((x) => x.id === id);
  if (isApi) {
    const out = await api<{ id?: string | number; monitorId?: string | number; monitor?: { id: string | number; name?: string } } | null>(
      `/suggestions/${encodeURIComponent(id)}/accept`, { method: 'POST' });
    const monId = String(out?.monitorId ?? out?.monitor?.id ?? out?.id ?? '');
    const mon = { id: monId, name: out?.monitor?.name ?? s?.title ?? 'Monitor', target: s?.target ?? '' };
    if (s) acceptedSuggestions.set(id, { suggestion: s, monitor: mon });
    await refreshCodeIntel();
    invalidate('/monitors');
    return mon;
  }
  if (!s) throw new Error('Suggestion not found');
  const m: Monitor = {
    id: `m${Date.now().toString(36)}`, name: s.title, kind: s.kind, target: s.target, condition: s.condition,
    severity: s.criticality === 'critical' ? 'critical' : 'warning', state: 'ok', source: 'code', lastChange: Date.now(), channels: [],
  };
  monitors.push(m);
  acceptedSuggestions.set(id, { suggestion: s, monitor: m });
  suggestions.splice(suggestions.indexOf(s), 1);
  const key = s.target.split(' · ')[1];
  const inv = inventory.find((i) => key && i.key === key);
  if (inv) inv.monitored = true;
  notify();
  return m;
}

export async function rejectSuggestion(id: string) {
  if (isApi) { await api(`/suggestions/${encodeURIComponent(id)}/reject`, { method: 'POST' }); await refreshCodeIntel(); return; }
  const k = suggestions.findIndex((x) => x.id === id);
  if (k >= 0) suggestions.splice(k, 1);
  notify();
}
