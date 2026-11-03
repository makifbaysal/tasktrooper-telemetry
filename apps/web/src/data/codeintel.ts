/**
 * Code intelligence data (ADR-014, .ai/contracts.md §13, §16): connected repositories, the code inventory and
 * monitor suggestions. The lists are the arrays exported from ./apm (other screens read them too); they are
 * filled from `GET /repos`, `/inventory`, `/suggestions?state=proposed`. Missing endpoints (404) leave
 * them empty — screens show empty states instead of failing.
 */
import { inventory, repos, suggestions } from './apm';
import { API_URL, ApiError, api, invalidate, mutate, qs, registerHydrator } from './remote';
import type { Criticality, InventoryItem, Monitor, Repo, Suggestion } from './types';

// ---------------------------------------------------------------- types

export type RepoProvider = 'github' | 'gitlab' | 'bitbucket' | 'local';

/** Where to point a per-repository webhook (GitLab, Bitbucket, App-less GitHub); the secret is only shown once. */
export interface RepoWebhook { url: string; secret: string }

export type RepoStatus = Repo['status'] | 'disconnected';

/** What the api returns for a repository on top of the UI `Repo`. */
export interface RepoInfo extends Omit<Repo, 'status'> {
  /** disconnected = removed from the GitHub App installation */
  status: RepoStatus;
  provider?: RepoProvider;
  installationId?: number;
  /** files changed by the last push, analysis pending (incremental) */
  pendingPaths?: string[];
  /** pushes trigger analyses: GitHub App installation, or a verified per-repository webhook delivery */
  webhookActive?: boolean;
  /** self-managed GitLab address */
  baseUrl?: string;
  /** only right after connecting / rotating: webhook URL + secret, shown once */
  webhook?: RepoWebhook;
  /** analysis failure (status = error) */
  error?: string;
  /** browse URL of the repository, e.g. https://github.com/org/repo, https://gitlab.com/group/sub/project */
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

export interface SuggestionRow extends Suggestion { repoId?: string; monitorDraft?: { name?: string; severity?: string } | null }

/** Service and endpoint a suggestion targets: target is `<service>[ logs] · <endpoint>`. */
export function suggestionScope(s: Pick<Suggestion, 'target'>): { service: string; endpoint?: string } {
  const [svc, ...rest] = s.target.split(' · ');
  return { service: svc.replace(/ logs$/, ''), endpoint: rest.join(' · ') || undefined };
}

export interface ConnectRepoInput {
  provider: RepoProvider;
  /** owner/name (GitHub), group/subgroup/project (GitLab), workspace/repo (Bitbucket), a display name for local */
  fullName: string;
  defaultBranch?: string;
  /** GitHub/GitLab access token, Bitbucket access token or username:app_password */
  token?: string;
  /** GitLab only: self-managed address (https) */
  baseUrl?: string;
  installationId?: number;
  localPath?: string;
}

// ---------------------------------------------------------------- availability

/** false when the api has no code-intelligence endpoints yet */
export let codeIntelAvailable = false;
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
  baseUrl?: string | null; webhook?: RepoWebhook | null;
  analysis?: { llm?: { status?: string; reason?: string | null; credits?: number | null; skipped?: number | null } | null } | null;
}
const STATUSES = new Set<RepoStatus>(['ready', 'analyzing', 'error', 'disconnected']);
const PROVIDERS = new Set<RepoProvider>(['github', 'gitlab', 'bitbucket', 'local']);

/** Web host of a repository (GitLab: self-managed baseUrl when set). */
export function repoHost(provider: RepoProvider | undefined, baseUrl?: string): string {
  if (provider === 'gitlab') return (baseUrl || 'https://gitlab.com').replace(/\/+$/, '');
  if (provider === 'bitbucket') return 'https://bitbucket.org';
  return 'https://github.com';
}

const SEGMENT = /^[A-Za-z0-9_][A-Za-z0-9_.-]*$/;
/** Same rules as the api (services/pkg/vcs): error message, or '' when the full name is valid. */
export function fullNameError(provider: RepoProvider, name: string): string {
  const parts = name.trim().replace(/^\/+|\/+$/g, '').split('/');
  const bad = parts.some((p) => !SEGMENT.test(p) || p.endsWith('.git') || p.endsWith('.atom') || p.includes('..'));
  if (provider === 'gitlab') return parts.length < 2 || parts.length > 21 || bad ? 'Use group/project (subgroups allowed: group/sub/project)' : '';
  if (provider === 'bitbucket') return parts.length !== 2 || bad ? 'Use workspace/repo' : '';
  if (provider === 'github') return parts.length !== 2 || bad ? 'Use owner/name' : '';
  return '';
}

/** Self-managed GitLab address check (https, no credentials, query or fragment): error message or ''. */
export function baseUrlError(raw: string): string {
  if (!raw.trim()) return '';
  let u: URL;
  try { u = new URL(raw.trim()); } catch { return 'Not a URL'; }
  if (u.protocol !== 'https:') return 'Use https';
  if (u.username || u.password) return 'Remove the credentials from the URL';
  if (u.search || u.hash) return 'Remove the query or fragment';
  return '';
}

const mapRepo = (r: RawRepo): RepoInfo => {
  const provider: RepoProvider = PROVIDERS.has(r.provider as RepoProvider) ? (r.provider as RepoProvider) : 'github';
  const name = r.name ?? r.fullName ?? String(r.id);
  return {
  id: String(r.id), name, branch: r.branch ?? r.defaultBranch ?? 'main', sha: r.sha ?? '',
  analyzedAt: toMs(r.analyzedAt), services: r.services ?? [], language: r.language ?? (r.languages ?? []).join(', '),
  status: STATUSES.has(r.status as RepoStatus) ? (r.status as RepoStatus) : r.status === 'pending' || r.status === 'queued' ? 'analyzing' : 'ready',
  error: r.error ?? undefined, provider, localPath: r.localPath, baseUrl: r.baseUrl ?? undefined,
  url: r.url ?? (provider !== 'local' && name.includes('/') ? `${repoHost(provider, r.baseUrl ?? undefined)}/${name}` : undefined),
  installationId: r.installationId != null && r.installationId !== '' ? Number(r.installationId) : undefined,
  pendingPaths: r.pendingPaths ?? r.pending_paths ?? undefined,
  webhookActive: r.webhookActive ?? (r.installationId != null ? true : undefined),
  webhook: r.webhook ?? undefined,
  llm: r.analysis?.llm?.status === 'ok' || r.analysis?.llm?.status === 'disabled' || r.analysis?.llm?.status === 'quota_exceeded'
    ? { status: r.analysis.llm.status, reason: r.analysis.llm.reason ?? undefined, credits: r.analysis.llm.credits ?? 0, skipped: r.analysis.llm.skipped ?? undefined }
    : undefined,
  };
};

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

registerHydrator(loadAll);

/** Refresh repos / inventory / suggestions now and re-render. */
export async function refreshCodeIntel() {
  await loadAll();
  notify();
}

// ---------------------------------------------------------------- GitHub App (ADR-017, contracts §18)

export interface GithubInstallation { id: string; account: string; installationId: number }
export interface GithubRepoOption { fullName: string; defaultBranch: string; private: boolean; language?: string; added: boolean }

/** Full-page redirect target that starts the GitHub App installation. */
export const githubInstallUrl = () => `${API_URL}/api/v1/integrations/github/install`;

/**
 * Starts the GitHub App installation from anywhere (Settings): asks the api for the signed install URL and leaves the
 * page. `redirect` is where the api sends the browser back, with `?github=<integrationId>` (Code insights opens the
 * repo picker on it).
 */
export async function connectGithub(redirect = `${window.location.origin}/code`): Promise<void> {
  try {
    const { url } = await api<{ url: string }>(`/integrations/github/install-url${qs({ redirect })}`, { method: 'POST' });
    window.location.href = url;
  } catch (e) {
    if (e instanceof ApiError && e.status === 501) {
      throw new Error('GitHub isn’t set up on this server yet: the operator has to register a GitHub App and set TT_GITHUB_APP_ID, TT_GITHUB_APP_SLUG and TT_GITHUB_APP_PRIVATE_KEY on the api.');
    }
    throw e;
  }
}

/** GitHub App installations of this project (integrations with provider=github). */
export async function githubInstallations(): Promise<GithubInstallation[]> {
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

/** Repos the installation can read: `GET /integrations/github/{id}/repos?q=`. */
export async function githubRepos(integrationId: string, q = ''): Promise<GithubRepoOption[]> {
  const added = (name: string) => repos.some((r) => r.name.toLowerCase() === name.toLowerCase());
  const raw = await api<Partial<GithubRepoOption>[] | null>(`/integrations/github/${encodeURIComponent(integrationId)}/repos${q.trim() ? `?q=${encodeURIComponent(q.trim())}` : ''}`);
  return (raw ?? []).map((r) => ({
    fullName: r.fullName ?? '', defaultBranch: r.defaultBranch ?? 'main', private: !!r.private, language: r.language, added: !!r.added || added(r.fullName ?? ''),
  }));
}

// ---------------------------------------------------------------- rows with optional api fields

/** Repos, inventory rows and suggestions with their optional api fields. */
export const repoList = () => repos as unknown as RepoInfo[];
export const inventoryRows = () => inventory as InventoryRow[];
export const suggestionRows = () => suggestions as SuggestionRow[];

// ---------------------------------------------------------------- links

/** Link to the handler on the repo host (GitHub blob URL) when known. */
export function codeLink(item: Pick<InventoryItem, 'file' | 'line' | 'service'>): string | undefined {
  if (!item.file) return undefined;
  const r = repoList().find((x) => x.services.includes(item.service));
  if (!r || r.provider === 'local' || !r.url) return undefined;
  const ref = r.sha || r.branch;
  if (r.provider === 'gitlab') return `${r.url}/-/blob/${ref}/${item.file}#L${item.line}`;
  if (r.provider === 'bitbucket') return `${r.url}/src/${ref}/${item.file}#lines-${item.line}`;
  return `${r.url}/blob/${ref}/${item.file}#L${item.line}`;
}

/** Link to the handler of a suggestion's `codeRef` ("path/file.ts:42"). */
export function codeRefLink(s: Pick<Suggestion, 'codeRef' | 'target'>): string | undefined {
  const [file, line] = s.codeRef.split(':');
  const svc = s.target.split(' · ')[0].replace(/ logs$/, '');
  return codeLink({ file, line: Number(line) || 1, service: svc });
}

// ---------------------------------------------------------------- mutations

/** Connect a repository; analysis starts right away. */
export async function connectRepo(input: ConnectRepoInput): Promise<RepoInfo> {
  const raw = await api<RawRepo>('/repos', { method: 'POST', body: JSON.stringify(input) });
  await refreshCodeIntel();
  return raw ? mapRepo(raw) : repoList()[0];
}


/** Issue a new webhook secret for a GitLab / Bitbucket / App-less GitHub repository (`POST /repos/{id}/webhook-secret`). */
export async function rotateWebhookSecret(id: string): Promise<RepoWebhook> {
  const raw = await api<RawRepo>(`/repos/${encodeURIComponent(id)}/webhook-secret`, { method: 'POST' });
  await refreshCodeIntel();
  if (!raw?.webhook) throw new Error('The api did not return a webhook secret');
  return raw.webhook;
}

export async function deleteRepo(id: string) {
  await api(`/repos/${encodeURIComponent(id)}`, { method: 'DELETE' });
  await refreshCodeIntel();
}

export async function analyzeRepo(id: string) {
  await api(`/repos/${encodeURIComponent(id)}/analyze`, { method: 'POST' });
  await refreshCodeIntel();
}

/** Suggestions accepted in this session (they leave the proposed list), with the monitor each created. */
export const acceptedSuggestions = new Map<string, { suggestion: SuggestionRow; monitor: Pick<Monitor, 'id' | 'name' | 'target'> }>();

/** Overrides of the suggestion's monitor draft on accept (POST /suggestions/{id}/accept body). */
export interface SuggestionAcceptInput { name?: string; severity?: string }

/** Accept → a monitor is created (source=suggestion). Refreshes the monitor list so the service's Alerts tab shows it. */
export async function acceptSuggestion(id: string, o: SuggestionAcceptInput = {}): Promise<Pick<Monitor, 'id' | 'name' | 'target'>> {
  const s = suggestions.find((x) => x.id === id);
  const out = await mutate<{ id?: string | number; monitorId?: string | number; monitor?: { id: string | number; name?: string; target?: string } } | null>(
    'POST', `/suggestions/${encodeURIComponent(id)}/accept`, o, ['/monitors']);
  const monId = String(out?.monitorId ?? out?.monitor?.id ?? out?.id ?? '');
  const mon = { id: monId, name: out?.monitor?.name ?? o.name ?? s?.title ?? 'Monitor', target: out?.monitor?.target ?? s?.target ?? '' };
  if (s) acceptedSuggestions.set(id, { suggestion: s, monitor: mon });
  await refreshCodeIntel();
  return mon;
}

export async function rejectSuggestion(id: string) {
  await api(`/suggestions/${encodeURIComponent(id)}/reject`, { method: 'POST' });
  await refreshCodeIntel();
}
