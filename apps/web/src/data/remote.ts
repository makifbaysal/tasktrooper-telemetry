/**
 * Data mode switch and remote cache (ADR-013, .ai/contracts.md §11).
 *
 * Screens call data functions synchronously. In api mode a function returns what is cached for its
 * request; on a miss it starts the request and returns a fallback. When the response lands the data
 * version bumps and every component using useApp()/useData() re-renders with the real value.
 */
import { useSyncExternalStore } from 'react';

export type DataMode = 'mock' | 'api';
export const DATA_MODE: DataMode = import.meta.env.VITE_DATA === 'api' ? 'api' : 'mock';
export const isApi = DATA_MODE === 'api';
export const API_URL: string = (import.meta.env.VITE_API_URL as string | undefined) ?? 'http://localhost:8080';
const EXPLICIT_KEY = (import.meta.env.VITE_API_KEY as string | undefined) || undefined;
/** VITE_API_KEY set explicitly: always send it (automation / dev), never show the login screen (ADR-016). */
export const hasKeyOverride = !!EXPLICIT_KEY;
const API_KEY: string = EXPLICIT_KEY ?? 'tt_dev_api_key';

export class ApiError extends Error {
  constructor(public status: number, message: string, public path: string) { super(message); }
}

// ── auth transport (ADR-016, contracts §15) ──
// 'key': Bearer api key (default until data/auth.ts finds a session-capable api) · 'session': tt_session cookie + X-Trooper-Project
let session = false;
let projectHeader: string | undefined;
let onUnauthorized: (() => void) | undefined;
/** Called by data/auth.ts once the auth mode and the selected project are known. */
export function configureTransport(o: { session: boolean; projectId?: string; onUnauthorized?: () => void }) {
  session = o.session;
  projectHeader = o.projectId;
  onUnauthorized = o.onUnauthorized;
}
/** Headers + credentials for a request to the api (also used by data/auth.ts for /auth and /me). */
export function authInit(init?: RequestInit, opts: { cookies?: boolean } = {}): RequestInit {
  const cookies = session || !!opts.cookies;
  return {
    ...init,
    // cookies only when the api is session-capable: credentialed CORS needs Allow-Credentials on the api
    credentials: cookies ? 'include' : 'same-origin',
    headers: {
      ...(!session || hasKeyOverride ? { Authorization: `Bearer ${API_KEY}` } : {}),
      ...(session && projectHeader && !hasKeyOverride ? { 'X-Trooper-Project': projectHeader } : {}),
      ...(init?.body ? { 'Content-Type': 'application/json' } : {}),
      ...init?.headers,
    },
  };
}

/** Fetch JSON from the api (`path` starts after /api/v1). */
export async function api<T>(path: string, init?: RequestInit): Promise<T> {
  const res = await fetch(`${API_URL}/api/v1${path}`, authInit(init));
  if (res.status === 401 && session && !hasKeyOverride) onUnauthorized?.();
  if (!res.ok) {
    let msg = res.statusText;
    try { const j = await res.json() as { error?: string; message?: string }; msg = j.error ?? j.message ?? msg; } catch { /* not json */ }
    throw new ApiError(res.status, msg, path);
  }
  if (res.status === 204) return undefined as T;
  // an empty 200 body is treated like 204 instead of failing the whole hydrate
  const text = await res.text();
  return (text ? JSON.parse(text) : undefined) as T;
}

// ── change notification ──
let version = 0;
const listeners = new Set<() => void>();
let scheduled = false;
function bump() {
  if (scheduled) return;
  scheduled = true;
  // coalesce many responses into one render
  setTimeout(() => { scheduled = false; version++; listeners.forEach((l) => l()); }, 16);
}
/** Re-render data consumers without dropping anything (local/mock mutations). */
export function notifyData() { bump(); }
export function subscribe(l: () => void) { listeners.add(l); return () => { listeners.delete(l); }; }
export const dataVersion = () => version;

/** Re-render on every data change. useApp() already includes this; use it in components that don't call useApp(). */
export function useData(): number {
  return useSyncExternalStore(subscribe, dataVersion, dataVersion);
}

// ── cache ──
interface Entry { value?: unknown; at: number; inflight?: Promise<void>; error?: string }
const cache = new Map<string, Entry>();
const TTL_MS = 60_000;

function load(path: string, map?: (raw: unknown) => unknown): Promise<void> {
  const e = cache.get(path) ?? { at: 0 };
  if (e.inflight) return e.inflight;
  e.inflight = api<unknown>(path)
    .then((raw) => { e.value = map ? map(raw) : raw; e.error = undefined; })
    .catch((err: unknown) => { e.error = err instanceof Error ? err.message : String(err); console.warn('[data]', path, e.error); })
    .finally(() => { e.at = Date.now(); e.inflight = undefined; bump(); });
  cache.set(path, e);
  bump();
  return e.inflight;
}

/**
 * Cached value for `path`, or `fallback` while it loads / after an error. Stale entries (> 60 s) are
 * served and refreshed in the background. `map` converts the api shape to the UI type once per response.
 */
export function remote<T>(path: string, fallback: T, map?: (raw: never) => T): T {
  const e = cache.get(path);
  if (!e || (!e.inflight && Date.now() - e.at > TTL_MS)) void load(path, map as ((raw: unknown) => unknown) | undefined);
  return e && 'value' in e && e.value !== undefined ? (e.value as T) : fallback;
}

/** Load `path` now and wait for it (hydrators, mutations). */
export async function prefetch(path: string, map?: (raw: never) => unknown): Promise<unknown> {
  await load(path, map as ((raw: unknown) => unknown) | undefined);
  const e = cache.get(path);
  if (e?.error) throw new Error(e.error);
  return e?.value;
}

/** Drop cached entries whose path starts with one of the prefixes (all when none given). */
export function invalidate(...prefixes: string[]) {
  for (const k of [...cache.keys()]) if (prefixes.length === 0 || prefixes.some((p) => k.startsWith(p))) cache.delete(k);
  bump();
}

/** Requests in flight and failed paths — for a thin loading bar / error banner. */
export function remoteStatus(): { pending: number; errors: { path: string; error: string }[] } {
  let pending = 0;
  const errors: { path: string; error: string }[] = [];
  for (const [path, e] of cache) {
    if (e.inflight) pending++;
    if (e.error) errors.push({ path, error: e.error });
  }
  return { pending: pending + hydrating, errors };
}

// ── list hydration ──
type Hydrator = () => Promise<void>;
const hydrators: Hydrator[] = [];
let hydrating = 0;

/** Register a function that refreshes exported lists from the api. Runs at startup, every 60 s and after mutations. */
export function registerHydrator(fn: Hydrator) { hydrators.push(fn); }

/** Replace the contents of an exported array in place (keeps the reference screens imported). */
export async function hydrateList<T>(arr: T[], path: string, map?: (raw: never) => T[]): Promise<void> {
  const raw = await api<unknown>(path);
  const next = map ? (map as (r: unknown) => T[])(raw) : (raw as T[]);
  arr.splice(0, arr.length, ...next);
}

/** Run every hydrator. Rejects with the first failure but still applies the others. */
export async function hydrate(): Promise<void> {
  hydrating++;
  try {
    const results = await Promise.allSettled(hydrators.map((h) => h()));
    const failed = results.find((r): r is PromiseRejectedResult => r.status === 'rejected');
    if (failed) throw failed.reason;
  } finally {
    hydrating--;
    bump();
  }
}

/** POST/PUT/PATCH/DELETE then refresh lists and drop the given cache prefixes. */
export async function mutate<T>(method: 'POST' | 'PUT' | 'PATCH' | 'DELETE', path: string, body?: unknown, invalidates: string[] = []): Promise<T> {
  const out = await api<T>(path, { method, body: body === undefined ? undefined : JSON.stringify(body) });
  if (invalidates.length) invalidate(...invalidates);
  await hydrate().catch((e: unknown) => console.warn('[data] refresh after mutation failed', e));
  return out;
}

let timer: ReturnType<typeof setInterval> | undefined;
/** Refresh lists and drop series every 60 s. */
export function startPolling() {
  if (!isApi || timer) return;
  timer = setInterval(() => { void hydrate().catch(() => undefined); for (const e of cache.values()) e.at = 0; bump(); }, TTL_MS);
}

// ── helpers ──
/** Query string from defined params. */
export function qs(params: Record<string, string | number | undefined | null>): string {
  const p = new URLSearchParams();
  for (const [k, v] of Object.entries(params)) if (v !== undefined && v !== null && v !== '') p.set(k, String(v));
  const s = p.toString();
  return s ? `?${s}` : '';
}

/** Path segment encoder. */
export const seg = (s: string) => encodeURIComponent(s);
