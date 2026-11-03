/**
 * Identity, org/project selection, roles and the admin lists of Settings (ADR-016, ADR-028, .ai/contracts.md §11 + §15).
 *
 * Boot calls `GET /auth/config` + `GET /me` with the `tt_session` cookie; every later request carries the cookie and
 * `X-Trooper-Project`. Outcomes: `ready` (a project is selected), `login` (401 → Login screen), `onboarding`
 * (signed in but no org yet → create org + first project, `POST /orgs`).
 */
import { useSyncExternalStore } from 'react';
import { API_URL, ApiError, authInit, configureTransport, invalidate, hydrate, mutate, qs, remote, seg } from './remote';

export type Role = 'owner' | 'admin' | 'member' | 'viewer';
export interface Project { id: string; name: string; slug?: string }
export interface Org { id: string; name: string; role: Role; projects: Project[] }
export interface User { id: string; email: string; name?: string; avatarUrl?: string }
export interface Me { user: User; orgs: Org[]; currentProjectId?: string }
export interface AuthConfig { oidc: boolean; devLogin: boolean; password: boolean }

interface State { config?: AuthConfig; me?: Me; projectId?: string; expired: boolean }
let state: State = { expired: false };
let version = 0;
const listeners = new Set<() => void>();
function set(patch: Partial<State>) {
  state = { ...state, ...patch };
  version++;
  listeners.forEach((l) => l());
}

const PROJECT_KEY = 'trooper-project';
function readProject(): string | undefined {
  try { return localStorage.getItem(PROJECT_KEY) ?? undefined; } catch { return undefined; }
}
function writeProject(id: string) {
  try { localStorage.setItem(PROJECT_KEY, id); } catch { /* storage unavailable */ }
}

function pickProject(me: Me): string | undefined {
  const all = me.orgs.flatMap((o) => o.projects.map((p) => p.id));
  const stored = readProject();
  return stored && all.includes(stored) ? stored : me.currentProjectId && all.includes(me.currentProjectId) ? me.currentProjectId : all[0];
}

// ---------------------------------------------------------------- boot

interface RawMe {
  user?: Partial<User> & { id?: string | number; avatar?: string; picture?: string } | null;
  orgs?: { id: string | number; name: string; role?: string; projects?: { id: string | number; name: string }[] | null }[] | null;
  currentProjectId?: string | number | null;
}
const ROLES = new Set<Role>(['owner', 'admin', 'member', 'viewer']);
const role = (r?: string): Role => (ROLES.has(r as Role) ? (r as Role) : 'viewer');
function mapMe(r: RawMe): Me {
  const u = r.user ?? {};
  return {
    user: { id: String(u.id ?? ''), email: u.email ?? '', name: u.name || undefined, avatarUrl: u.avatarUrl ?? u.avatar ?? u.picture ?? undefined },
    orgs: (r.orgs ?? []).map((o) => ({ id: String(o.id), name: o.name, role: role(o.role), projects: (o.projects ?? []).map((p) => ({ id: String(p.id), name: p.name })) })),
    currentProjectId: r.currentProjectId != null ? String(r.currentProjectId) : undefined,
  };
}

async function raw<T>(path: string, init?: RequestInit): Promise<{ status: number; body?: T }> {
  const res = await fetch(`${API_URL}/api/v1${path}`, authInit(init));
  let body: T | undefined;
  try { const t = await res.text(); body = t ? (JSON.parse(t) as T) : undefined; } catch { /* not json */ }
  return { status: res.status, body };
}

function applyTransport() {
  configureTransport({
    projectId: state.projectId,
    // a session that expires mid-use sends the app back to the login screen
    onUnauthorized: () => { if (!state.expired && state.me) set({ expired: true }); },
  });
}

export type BootStep = 'ready' | 'login' | 'onboarding';

/**
 * Boot step. `ready` → hydrate and render the app; `login` → Login screen; `onboarding` → create the first org.
 * Throws when the api cannot be reached (network / CORS / 5xx) so the boot screen can offer a retry.
 */
export async function initAuth(): Promise<BootStep> {
  const c = await raw<Partial<AuthConfig>>('/auth/config');
  if (c.status !== 200 || !c.body) throw new ApiError(c.status, `GET /auth/config returned ${c.status}`, '/auth/config');
  const config: AuthConfig = { oidc: !!c.body.oidc, devLogin: !!c.body.devLogin, password: !!c.body.password };

  const r = await raw<RawMe>('/me');
  if (r.status === 401) {
    set({ config, me: undefined, projectId: undefined, expired: false });
    return 'login';
  }
  if (r.status !== 200 || !r.body) throw new ApiError(r.status, `GET /me returned ${r.status}`, '/me');
  const me = mapMe(r.body);
  set({ config, me, projectId: pickProject(me), expired: false });
  applyTransport();
  // contracts §11: an org-less user gets `orgs: []` + `currentProjectId: null` → create the first org
  return me.orgs.length === 0 || !state.projectId ? 'onboarding' : 'ready';
}

/** Where the "Continue with SSO" button goes; the api redirects back here after the callback. */
export function loginUrl(redirect = window.location.href): string {
  return `${API_URL}/api/v1/auth/login${qs({ redirect })}`;
}

/** Dev login (TT_AUTH_DEV=true, no OIDC issuer): sets the session cookie. */
export async function devLogin(email: string): Promise<void> {
  const r = await raw<{ error?: string; message?: string }>('/auth/dev-login', { method: 'POST', body: JSON.stringify({ email }) });
  if (r.status >= 300) throw new ApiError(r.status, r.body?.error ?? r.body?.message ?? `Sign-in failed (${r.status})`, '/auth/dev-login');
}

/**
 * Email + password login (TT_AUTH_PASSWORD=true, ADR-035): sets the session cookie. Errors carry a message for the
 * sign-in form: 401 → invalid credentials, 429 → too many attempts (with the api's Retry-After).
 */
export async function passwordLogin(email: string, password: string): Promise<void> {
  const res = await fetch(`${API_URL}/api/v1/auth/password-login`, authInit({ method: 'POST', body: JSON.stringify({ email, password }) }));
  if (res.ok) return;
  let body: { error?: string } | undefined;
  try { body = (await res.json()) as { error?: string }; } catch { /* not json */ }
  const retry = Number(res.headers.get('Retry-After'));
  const why = res.status === 401 ? 'Invalid e-mail or password.'
    : res.status === 429 ? `Too many failed attempts. Try again in ${retry > 0 ? Math.ceil(retry / 60) : 15} minute${retry > 0 && retry <= 60 ? '' : 's'}.`
    : res.status === 404 ? 'Password sign-in is not enabled on this api.'
    : body?.error ?? `Sign-in failed (${res.status})`;
  throw new ApiError(res.status, why, '/auth/password-login');
}

export async function logout(): Promise<void> {
  try { await raw('/auth/logout', { method: 'POST' }); } catch { /* cookie may already be gone */ }
  window.location.reload();
}

/** Switch project: persist, re-scope requests, drop every cache and refetch the lists. */
export async function switchProject(id: string): Promise<void> {
  if (id === state.projectId) return;
  writeProject(id);
  set({ projectId: id });
  applyTransport();
  invalidate();
  await hydrate().catch((e: unknown) => console.warn('[auth] refresh after project switch failed', e));
}

// ---------------------------------------------------------------- onboarding (contracts §11)

export interface CreatedOrg {
  org: { id: string; name: string; role: Role };
  project: Project;
  ingestKey: { id: string; prefix: string; key: string };
  /** full DSN including the secret ingest key — shown once */
  dsn: string;
}
interface RawCreatedOrg {
  org?: { id: string | number; name: string; role?: string } | null;
  project?: { id: string | number; name: string; slug?: string | null } | null;
  ingestKey?: { id: string | number; prefix?: string | null; key?: string | null } | null;
  dsn?: string | null;
}

/**
 * Create an org + its first project (`POST /orgs`); the caller becomes owner. The response carries the ingest key and
 * DSN exactly once. Selects the new project and remembers the DSN *without* the key for setup snippets.
 */
export async function createOrg(name: string, projectName: string): Promise<CreatedOrg> {
  const r = await raw<RawCreatedOrg & { error?: string; message?: string }>('/orgs', {
    method: 'POST', body: JSON.stringify({ name, project: { name: projectName } }),
  });
  if (r.status >= 300 || !r.body?.org || !r.body.project) {
    const why = r.status === 409 ? 'You have reached the maximum number of organizations for your account.'
      : r.status === 403 ? 'Organizations can only be created by a signed-in user (not with an api key).'
      : r.body?.error ?? r.body?.message ?? `Could not create the organization (${r.status})`;
    throw new ApiError(r.status, why, '/orgs');
  }
  const b = r.body;
  const out: CreatedOrg = {
    org: { id: String(b.org!.id), name: b.org!.name, role: role(b.org!.role ?? 'owner') },
    project: { id: String(b.project!.id), name: b.project!.name, slug: b.project!.slug ?? undefined },
    ingestKey: { id: String(b.ingestKey?.id ?? ''), prefix: b.ingestKey?.prefix ?? '', key: b.ingestKey?.key ?? '' },
    dsn: b.dsn ?? '',
  };
  if (out.dsn) rememberDsn(out.project.id, out.ingestKey.key ? out.dsn.replace(out.ingestKey.key, KEY_PLACEHOLDER) : out.dsn);
  const org: Org = { ...out.org, projects: [out.project] };
  const me: Me = state.me ? { ...state.me, orgs: [...state.me.orgs.filter((o) => o.id !== org.id), org] } : { user: { id: '', email: '' }, orgs: [org] };
  writeProject(out.project.id);
  set({ me, projectId: out.project.id });
  applyTransport();
  return out;
}

// ---------------------------------------------------------------- DSN for setup snippets

export const KEY_PLACEHOLDER = '<ingest-key>';
const DSN_KEY = (projectId: string) => `trooper-dsn:${projectId}`;
/** Public ingest base URL (`VITE_INGEST_URL`, set by the demo deploy to the public ingest host). */
export const INGEST_URL: string = (import.meta.env.VITE_INGEST_URL as string | undefined) ?? 'http://localhost:4318';
function rememberDsn(projectId: string, dsnWithoutKey: string) {
  try { localStorage.setItem(DSN_KEY(projectId), dsnWithoutKey); } catch { /* storage unavailable */ }
}
/**
 * DSN of the current project with the secret replaced by `<ingest-key>` (keys are shown only once, ADR-016):
 * the one returned at onboarding in this browser, else `VITE_INGEST_URL` (default http://localhost:4318) + project id.
 */
export function projectDsn(projectId = state.projectId): string {
  const id = projectId ?? '<project-id>';
  try { const s = localStorage.getItem(DSN_KEY(id)); if (s) return s; } catch { /* storage unavailable */ }
  try {
    const u = new URL(INGEST_URL);
    return `${u.protocol}//${KEY_PLACEHOLDER}@${u.host}${u.pathname.replace(/\/$/, '')}/${id}`;
  } catch {
    return `https://${KEY_PLACEHOLDER}@ingest.example.com/${id}`;
  }
}

// ---------------------------------------------------------------- reading the state

function subscribe(l: () => void) { listeners.add(l); return () => { listeners.delete(l); }; }
const snapshot = () => version;
/** Non-React subscription to login / project switch (e.g. RUM `identify` in lib/telemetry.ts). */
export const onAuthChange = subscribe;

export interface AuthView {
  config?: AuthConfig;
  me?: Me;
  org?: Org;
  project?: Project;
  role?: Role;
  /** session expired while the app was open */
  expired: boolean;
}
export function authState(): AuthView {
  const org = state.me?.orgs.find((o) => o.projects.some((p) => p.id === state.projectId)) ?? state.me?.orgs[0];
  const project = org?.projects.find((p) => p.id === state.projectId) ?? org?.projects[0];
  return { config: state.config, me: state.me, org, project, role: org?.role, expired: state.expired };
}
/** Re-render on login / project switch. */
export function useAuth(): AuthView {
  useSyncExternalStore(subscribe, snapshot, snapshot);
  return authState();
}

/**
 * Role check (ADR-016): viewer = read only; member writes monitors/incidents/tests; admin+ manages members,
 * keys and integrations. The api enforces the same rules.
 */
export type Action = 'write' | 'manage';
export function can(action: Action): boolean {
  const r = authState().role;
  if (!r) return true;
  if (action === 'write') return r !== 'viewer';
  return r === 'owner' || r === 'admin';
}
/** Tooltip for disabled controls. */
export const denied = (action: Action) => (action === 'manage' ? 'Requires the admin or owner role' : 'Viewers have read-only access');

// ---------------------------------------------------------------- members

export interface Member { userId: string; email: string; name?: string; role: Role; joinedAt?: number; lastSeenAt?: number; pending?: boolean }
interface RawMember { userId?: string | number; id?: string | number; email: string; name?: string | null; role?: string; joinedAt?: number | string | null; createdAt?: number | string | null; lastSeenAt?: number | string | null; pending?: boolean }
const toMs = (v: number | string | null | undefined) => (v == null || v === '' ? undefined : typeof v === 'number' ? v : Date.parse(v) || undefined);
const mapMember = (m: RawMember): Member => ({
  userId: String(m.userId ?? m.id ?? m.email), email: m.email, name: m.name || undefined, role: role(m.role),
  joinedAt: toMs(m.joinedAt ?? m.createdAt), lastSeenAt: toMs(m.lastSeenAt), pending: m.pending || undefined,
});


export function membersOf(orgId: string | undefined): Member[] {
  if (!orgId) return [];
  return remote(`/orgs/${seg(orgId)}/members`, [], (r: RawMember[] | null) => (r ?? []).map(mapMember));
}
export async function inviteMember(orgId: string, email: string, r: Role): Promise<void> {
  await mutate('POST', `/orgs/${seg(orgId)}/members`, { email, role: r }, [`/orgs/${seg(orgId)}/members`, '/audit-log']);
}
export async function setMemberRole(orgId: string, userId: string, r: Role): Promise<void> {
  await mutate('PATCH', `/orgs/${seg(orgId)}/members/${seg(userId)}`, { role: r }, [`/orgs/${seg(orgId)}/members`, '/audit-log']);
}
export async function removeMember(orgId: string, userId: string): Promise<void> {
  await mutate('DELETE', `/orgs/${seg(orgId)}/members/${seg(userId)}`, undefined, [`/orgs/${seg(orgId)}/members`, '/audit-log']);
}

// ---------------------------------------------------------------- api keys

export type KeyScope = 'ingest' | 'api';
export interface ApiKeyRow {
  id: string; name: string; scope: KeyScope; prefix: string; origins: string[];
  createdAt?: number; createdBy?: string; lastUsedAt?: number; revokedAt?: number;
}
interface RawKey {
  id: string | number; name: string; kind?: string; scope?: string; prefix?: string; allowedOrigins?: string[] | null; origins?: string[] | null;
  createdAt?: number | string | null; createdBy?: string | null; lastUsedAt?: number | string | null; revokedAt?: number | string | null;
}
const mapKey = (k: RawKey): ApiKeyRow => ({
  id: String(k.id), name: k.name, scope: (k.kind ?? k.scope) === 'ingest' ? 'ingest' : 'api', prefix: k.prefix ?? '',
  origins: k.allowedOrigins ?? k.origins ?? [], createdAt: toMs(k.createdAt), createdBy: k.createdBy ?? undefined,
  lastUsedAt: toMs(k.lastUsedAt), revokedAt: toMs(k.revokedAt),
});


export function apiKeys(): ApiKeyRow[] {
  return remote('/api-keys', [], (r: RawKey[] | null) => (r ?? []).map(mapKey));
}
/** Creates a key; the response carries the full secret exactly once. */
export async function createApiKey(name: string, scope: KeyScope, origins: string[] = []): Promise<{ row: ApiKeyRow; secret: string }> {
  const out = await mutate<RawKey & { key?: string; secret?: string; token?: string }>('POST', '/api-keys', { name, kind: scope, scope, allowedOrigins: origins }, ['/api-keys', '/audit-log']);
  return { row: mapKey(out), secret: out.key ?? out.secret ?? out.token ?? '' };
}
export async function revokeApiKey(id: string): Promise<void> {
  await mutate('DELETE', `/api-keys/${seg(id)}`, undefined, ['/api-keys', '/audit-log']);
}

// ---------------------------------------------------------------- audit log

export interface AuditEntry { id: string; at: number; actor: string; action: string; target: string; meta?: Record<string, unknown> }
interface RawAudit {
  id: string | number; at?: number | string; ts?: number | string; user?: string | null; userEmail?: string | null; actor?: string | null;
  apiKey?: string | null; apiKeyName?: string | null; action: string; target?: string | null; meta?: Record<string, unknown> | null;
}
const mapAudit = (a: RawAudit): AuditEntry => ({
  id: String(a.id), at: toMs(a.at ?? a.ts) ?? 0, action: a.action, target: a.target ?? '', meta: a.meta ?? undefined,
  actor: a.actor ?? a.userEmail ?? a.user ?? (a.apiKeyName || a.apiKey ? `key · ${a.apiKeyName ?? a.apiKey}` : 'system'),
});


export function auditLog(limit = 200): AuditEntry[] {
  return remote(`/audit-log${qs({ limit })}`, [], (r: RawAudit[] | { entries?: RawAudit[] } | null) =>
    (Array.isArray(r) ? r : r?.entries ?? []).map(mapAudit));
}
