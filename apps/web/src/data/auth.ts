/**
 * Identity, org/project selection, roles and the admin lists of Settings (ADR-016, .ai/contracts.md §15).
 *
 * Api mode: boot calls `GET /auth/config` + `GET /me` with cookies. Modes:
 * - `session`: the api is session-capable and /me answered → cookie + `X-Trooper-Project` on every request.
 * - `key`: `/auth/config` is missing (older api, 404 / CORS) or `VITE_API_KEY` is set → Bearer key, no login screen.
 * - `mock`: VITE_DATA=mock — a fixed user with two orgs (owner in one, viewer in the other) to preview roles.
 */
import { useSyncExternalStore } from 'react';
import { API_URL, ApiError, authInit, configureTransport, hasKeyOverride, invalidate, hydrate, isApi, mutate, notifyData, qs, remote, seg } from './remote';
import { NOW } from './mock';

export type Role = 'owner' | 'admin' | 'member' | 'viewer';
export interface Project { id: string; name: string }
export interface Org { id: string; name: string; role: Role; projects: Project[] }
export interface User { id: string; email: string; name?: string; avatarUrl?: string }
export interface Me { user: User; orgs: Org[]; currentProjectId?: string }
export interface AuthConfig { oidc: boolean; devLogin: boolean }
export type AuthMode = 'mock' | 'key' | 'session';

interface State { mode: AuthMode; config?: AuthConfig; me?: Me; projectId?: string; expired: boolean }
let state: State = { mode: isApi ? 'key' : 'mock', expired: false };
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

// ---------------------------------------------------------------- mock identity

const MOCK_ME: Me = {
  user: { id: 'u-ada', email: 'ada@shop.com', name: 'Ada Yılmaz' },
  orgs: [
    { id: 'org-shopco', name: 'Shopco', role: 'owner', projects: [{ id: 'shop-platform', name: 'shop-platform' }, { id: 'shop-staging', name: 'shop-staging' }] },
    { id: 'org-acme', name: 'Acme Labs', role: 'viewer', projects: [{ id: 'acme-web', name: 'acme-web' }] },
  ],
  currentProjectId: 'shop-platform',
};

function pickProject(me: Me): string | undefined {
  const all = me.orgs.flatMap((o) => o.projects.map((p) => p.id));
  const stored = readProject();
  return stored && all.includes(stored) ? stored : me.currentProjectId && all.includes(me.currentProjectId) ? me.currentProjectId : all[0];
}

if (!isApi) state = { ...state, me: MOCK_ME, projectId: pickProject(MOCK_ME) };

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
  const res = await fetch(`${API_URL}/api/v1${path}`, authInit(init, { cookies: true }));
  let body: T | undefined;
  try { const t = await res.text(); body = t ? (JSON.parse(t) as T) : undefined; } catch { /* not json */ }
  return { status: res.status, body };
}

function applyTransport() {
  configureTransport({
    session: state.mode === 'session', projectId: state.projectId,
    // a session that expires mid-use sends the app back to the login screen
    onUnauthorized: () => { if (!state.expired) set({ expired: true }); },
  });
}

/**
 * Api mode boot step. `ready` → hydrate and render the app; `login` → show the Login screen.
 * Never throws: an api without the auth endpoints degrades to api-key mode.
 */
export async function initAuth(): Promise<'ready' | 'login'> {
  if (!isApi) return 'ready';
  let config: AuthConfig | undefined;
  try {
    const r = await raw<Partial<AuthConfig>>('/auth/config');
    if (r.status === 200 && r.body) config = { oidc: !!r.body.oidc, devLogin: !!r.body.devLogin };
  } catch { /* network / CORS without credentials support → older api */ }

  if (!config) {
    set({ mode: 'key', config: undefined, me: undefined, projectId: undefined, expired: false });
    applyTransport();
    return 'ready';
  }

  try {
    const r = await raw<RawMe>('/me');
    if (r.status === 200 && r.body) {
      const me = mapMe(r.body);
      set({ mode: hasKeyOverride ? 'key' : 'session', config, me, projectId: pickProject(me), expired: false });
      applyTransport();
      return 'ready';
    }
    if (r.status === 401 && !hasKeyOverride) {
      set({ mode: 'session', config, me: undefined, expired: false });
      return 'login';
    }
  } catch { /* fall through to key mode */ }
  set({ mode: 'key', config, me: undefined, projectId: undefined, expired: false });
  applyTransport();
  return 'ready';
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

export async function logout(): Promise<void> {
  if (state.mode === 'session') {
    try { await raw('/auth/logout', { method: 'POST' }); } catch { /* cookie may already be gone */ }
  }
  window.location.reload();
}

/** Switch project: persist, re-scope requests, drop every cache and refetch the lists. */
export async function switchProject(id: string): Promise<void> {
  if (id === state.projectId) return;
  writeProject(id);
  set({ projectId: id });
  if (!isApi) { notifyData(); return; }
  applyTransport();
  invalidate();
  await hydrate().catch((e: unknown) => console.warn('[auth] refresh after project switch failed', e));
}

// ---------------------------------------------------------------- reading the state

function subscribe(l: () => void) { listeners.add(l); return () => { listeners.delete(l); }; }
const snapshot = () => version;

export interface AuthView {
  mode: AuthMode;
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
  return { mode: state.mode, config: state.config, me: state.me, org, project, role: org?.role, expired: state.expired };
}
/** Re-render on login / project switch. */
export function useAuth(): AuthView {
  useSyncExternalStore(subscribe, snapshot, snapshot);
  return authState();
}

/**
 * Role check (ADR-016): viewer = read only; member writes monitors/incidents/tests; admin+ manages members,
 * keys and integrations. Without a known user (api-key mode) the key decides — the api enforces it.
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

const DAY = 86400_000;
const mockMembers: Member[] = [
  { userId: 'u-ada', email: 'ada@shop.com', name: 'Ada Yılmaz', role: 'owner', joinedAt: NOW - 140 * DAY, lastSeenAt: NOW - 60_000 },
  { userId: 'u-mert', email: 'mert@shop.com', name: 'Mert Kaya', role: 'admin', joinedAt: NOW - 120 * DAY, lastSeenAt: NOW - 2 * 3600_000 },
  { userId: 'u-deniz', email: 'deniz@shop.com', name: 'Deniz Arslan', role: 'member', joinedAt: NOW - 64 * DAY, lastSeenAt: NOW - DAY },
  { userId: 'u-selin', email: 'selin@shop.com', name: 'Selin Demir', role: 'viewer', joinedAt: NOW - 9 * DAY, lastSeenAt: NOW - 5 * DAY },
  { userId: 'u-inv', email: 'oncall@contractor.io', role: 'member', joinedAt: NOW - 2 * DAY, pending: true },
];

export function membersOf(orgId: string | undefined): Member[] {
  if (!isApi) return mockMembers;
  if (!orgId) return [];
  return remote(`/orgs/${seg(orgId)}/members`, [], (r: RawMember[] | null) => (r ?? []).map(mapMember));
}
export async function inviteMember(orgId: string, email: string, r: Role): Promise<void> {
  if (!isApi) { mockMembers.push({ userId: `u-${Date.now()}`, email, role: r, joinedAt: Date.now(), pending: true }); audit('member.invite', email); return; }
  await mutate('POST', `/orgs/${seg(orgId)}/members`, { email, role: r }, [`/orgs/${seg(orgId)}/members`, '/audit-log']);
}
export async function setMemberRole(orgId: string, userId: string, r: Role): Promise<void> {
  if (!isApi) { const m = mockMembers.find((x) => x.userId === userId); if (m) { m.role = r; audit('member.role', `${m.email} → ${r}`); } return; }
  await mutate('PATCH', `/orgs/${seg(orgId)}/members/${seg(userId)}`, { role: r }, [`/orgs/${seg(orgId)}/members`, '/audit-log']);
}
export async function removeMember(orgId: string, userId: string): Promise<void> {
  if (!isApi) { const i = mockMembers.findIndex((x) => x.userId === userId); if (i >= 0) { audit('member.remove', mockMembers[i].email); mockMembers.splice(i, 1); } return; }
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

const mockKeys: ApiKeyRow[] = [
  { id: 'k1', name: 'Browser ingest', scope: 'ingest', prefix: 'pk_live_3f9a', origins: ['shop.com', '*.shop.com'], createdAt: NOW - 140 * DAY, createdBy: 'mert@shop.com', lastUsedAt: NOW - 2_000 },
  { id: 'k2', name: 'Backend ingest', scope: 'ingest', prefix: 'sk_ing_91c0', origins: [], createdAt: NOW - 140 * DAY, createdBy: 'mert@shop.com', lastUsedAt: NOW - 1_000 },
  { id: 'k3', name: 'CI / Terraform', scope: 'api', prefix: 'sk_api_77ab', origins: [], createdAt: NOW - 41 * DAY, createdBy: 'deniz@shop.com', lastUsedAt: NOW - 3 * DAY },
  { id: 'k4', name: 'Old staging key', scope: 'api', prefix: 'sk_api_0c12', origins: [], createdAt: NOW - 200 * DAY, createdBy: 'ada@shop.com', lastUsedAt: NOW - 90 * DAY, revokedAt: NOW - 60 * DAY },
];

export function apiKeys(): ApiKeyRow[] {
  if (!isApi) return mockKeys;
  return remote('/api-keys', [], (r: RawKey[] | null) => (r ?? []).map(mapKey));
}
/** Creates a key; the response carries the full secret exactly once. */
export async function createApiKey(name: string, scope: KeyScope, origins: string[] = []): Promise<{ row: ApiKeyRow; secret: string }> {
  if (!isApi) {
    const rnd = Array.from(crypto.getRandomValues(new Uint8Array(20)), (b) => b.toString(16).padStart(2, '0')).join('');
    const secret = `${scope === 'ingest' ? 'sk_ing' : 'sk_api'}_${rnd}`;
    const row: ApiKeyRow = { id: `k${Date.now()}`, name, scope, prefix: secret.slice(0, 11), origins, createdAt: Date.now(), createdBy: MOCK_ME.user.email };
    mockKeys.unshift(row);
    audit('api_key.create', name);
    return { row, secret };
  }
  const out = await mutate<RawKey & { key?: string; secret?: string; token?: string }>('POST', '/api-keys', { name, kind: scope, scope, allowedOrigins: origins }, ['/api-keys', '/audit-log']);
  return { row: mapKey(out), secret: out.key ?? out.secret ?? out.token ?? '' };
}
export async function revokeApiKey(id: string): Promise<void> {
  if (!isApi) { const k = mockKeys.find((x) => x.id === id); if (k) { k.revokedAt = Date.now(); audit('api_key.revoke', k.name); } return; }
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

const mockAudit: AuditEntry[] = [
  { id: 'a1', at: NOW - 12 * 60_000, actor: 'mert@shop.com', action: 'monitor.update', target: 'Charges error rate' },
  { id: 'a2', at: NOW - 3 * 3600_000, actor: 'key · CI / Terraform', action: 'monitor.create', target: 'Checkout LCP' },
  { id: 'a3', at: NOW - 26 * 3600_000, actor: 'deniz@shop.com', action: 'incident.ack', target: 'payments-api error rate' },
  { id: 'a4', at: NOW - 2 * DAY, actor: 'ada@shop.com', action: 'member.invite', target: 'oncall@contractor.io' },
  { id: 'a5', at: NOW - 9 * DAY, actor: 'ada@shop.com', action: 'member.invite', target: 'selin@shop.com' },
  { id: 'a6', at: NOW - 12 * DAY, actor: 'mert@shop.com', action: 'integration.create', target: 'Vercel · shop-team' },
  { id: 'a7', at: NOW - 60 * DAY, actor: 'ada@shop.com', action: 'api_key.revoke', target: 'Old staging key' },
];
function audit(action: string, target: string) {
  mockAudit.unshift({ id: `a${Date.now()}`, at: Date.now(), actor: MOCK_ME.user.email, action, target });
  notifyData();
}

export function auditLog(limit = 200): AuditEntry[] {
  if (!isApi) return mockAudit.slice(0, limit);
  return remote(`/audit-log${qs({ limit })}`, [], (r: RawAudit[] | { entries?: RawAudit[] } | null) =>
    (Array.isArray(r) ? r : r?.entries ?? []).map(mapAudit));
}
