/**
 * Load-test data beyond the scenario / run lists (ADR-015, .ai/contracts.md §14, §16):
 * scenario state (draft → ready), verified target domains, plan limits, run start with pre-checks,
 * and the live run detail (`GET /loadtests/runs/{id}`, `/series`, `/target?service=`) polled every 5 s.
 * Degrades to empty values when an endpoint is missing (404).
 */
import { useEffect, useState } from 'react';
import { runs, scenarios } from './apm';
import { ApiError, api, invalidate, registerHydrator } from './remote';
import { linkIds, localId, mapRun, serverId } from './api/ops';
import type { LoadTestRun } from './types';
import { SPEC, type RunCurves } from '@/lib/loadtest';

const notify = () => invalidate('\u0000');
const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
const missing = (e: unknown) => e instanceof ApiError && (e.status === 404 || e.status === 405 || e.status === 501);
const toMs = (v: unknown): number | undefined => {
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
  if (typeof v === 'string') { const t = Date.parse(v); return Number.isFinite(t) ? t : undefined; }
  return undefined;
};

// ---------------------------------------------------------------- scenario state

export type ScenarioState = 'draft' | 'ready';
const states = new Map<string, ScenarioState>();
/** Generated scenarios start as `draft` and cannot run until approved. */
export const scenarioState = (id: string): ScenarioState => states.get(id) ?? 'ready';

/** Review & approve: PATCH state=ready. */
export async function approveScenario(id: string) {
  await api(`/loadtests/scenarios/${encodeURIComponent(serverId(id))}`, { method: 'PATCH', body: JSON.stringify({ state: 'ready' }) });
  states.set(id, 'ready');
  notify();
}

// ---------------------------------------------------------------- domains

export interface LoadTestDomain {
  id: string;
  host: string;
  token: string;
  verifiedAt?: number;
  method?: 'dns' | 'http';
  createdAt: number;
  /** last failed verification attempt */
  lastError?: string;
  /** a check was requested and has not succeeded yet (api: verifyRequestedAt) */
  verifying?: boolean;
  /** TXT value / URL to publish (api provides them; derived otherwise) */
  dnsRecord?: string;
  httpUrl?: string;
}

export const domains: LoadTestDomain[] = [];
/** false when the api has no /loadtests/domains yet */
export let domainsAvailable = false;

interface RawDomain {
  id: string | number; host: string; token: string; verified?: boolean; verifiedAt?: number | string | null; verified_at?: string | null; method?: string | null;
  createdAt?: number | string; lastError?: string | null; error?: string | null; verifyRequestedAt?: number | null; dnsRecord?: string; httpUrl?: string;
}
const mapDomain = (d: RawDomain): LoadTestDomain => ({
  id: String(d.id), host: d.host, token: d.token, verifiedAt: toMs(d.verifiedAt ?? d.verified_at ?? undefined) ?? (d.verified ? Date.now() : undefined),
  verifying: !d.verified && !!d.verifyRequestedAt, dnsRecord: d.dnsRecord, httpUrl: d.httpUrl,
  method: d.method === 'http' ? 'http' : d.method === 'dns' ? 'dns' : undefined, createdAt: toMs(d.createdAt) ?? 0,
  lastError: d.lastError ?? d.error ?? undefined,
});

/** Host part of a target URL (lower case, no port). */
export function hostOf(target: string): string {
  try { return new URL(/^[a-z]+:\/\//i.test(target) ? target : `https://${target}`).hostname.toLowerCase(); } catch { return target.toLowerCase(); }
}
/** localhost / *.localhost / 127.0.0.1 need no verification (dev). */
export const isLocalHost = (h: string) => h === 'localhost' || h.endsWith('.localhost') || h === '127.0.0.1' || h === '::1' || h === '[::1]';

export type TargetCheck = { ok: true; host: string; local?: boolean } | { ok: false; host: string; reason: string; domain?: LoadTestDomain };
/** Is the scenario's target host verified for this project? */
export function checkTarget(target: string): TargetCheck {
  const host = hostOf(target);
  if (!host) return { ok: false, host, reason: 'Enter the base URL of the system under test.' };
  if (isLocalHost(host)) return { ok: true, host, local: true };
  // api without the domains endpoint: the server decides when the run starts
  if (!domainsAvailable) return { ok: true, host };
  const d = domains.find((x) => x.host === host);
  if (d?.verifiedAt) return { ok: true, host };
  return { ok: false, host, domain: d, reason: d ? `${host} is not verified yet — add the TXT record or the well-known file, then verify.` : `${host} is not a verified domain. Add and verify it before running load against it.` };
}

export async function addDomain(hostInput: string): Promise<LoadTestDomain> {
  const host = hostOf(hostInput.trim());
  if (!host || !/^[a-z0-9.-]+$/.test(host) || !host.includes('.')) throw new Error('Enter a host name like staging.shop.com');
  if (domains.some((d) => d.host === host)) throw new Error(`${host} is already added`);
  const raw = await api<RawDomain>('/loadtests/domains', { method: 'POST', body: JSON.stringify({ host }) });
  const d = mapDomain(raw);
  domains.push(d);
  notify();
  return d;
}

/** Ask the server to check DNS TXT / the well-known file. Resolves with the updated domain (verified or with `lastError`). */
export async function verifyDomain(id: string): Promise<LoadTestDomain> {
  const d = domains.find((x) => x.id === id);
  if (!d) throw new Error('Domain not found');
  try {
    // the check is asynchronous (loadtest-ctl): request it, then watch the domain for ~20 s
    const raw = await api<RawDomain | null>(`/loadtests/domains/${encodeURIComponent(id)}/verify`, { method: 'POST' });
    if (raw) Object.assign(d, mapDomain(raw));
    d.verifying = !d.verifiedAt;
    d.lastError = undefined;
    notify();
    for (let k = 0; k < 10 && !d.verifiedAt; k++) {
      await sleep(2000);
      const list = await api<RawDomain[] | null>('/loadtests/domains');
      const cur = list?.find((x) => String(x.id) === id);
      if (cur) Object.assign(d, mapDomain(cur));
      notify();
    }
    if (!d.verifiedAt) d.lastError = 'Neither the TXT record nor the well-known file was found yet. DNS changes can take a few minutes — the check keeps running; verify again later.';
  } catch (e) {
    d.lastError = e instanceof Error ? e.message : String(e);
  }
  d.verifying = false;
  notify();
  return d;
}

export async function deleteDomain(id: string) {
  await api(`/loadtests/domains/${encodeURIComponent(id)}`, { method: 'DELETE' });
  const k = domains.findIndex((x) => x.id === id);
  if (k >= 0) domains.splice(k, 1);
  notify();
}

// ---------------------------------------------------------------- starting a run

export type RunBlockKind = 'draft' | 'domain' | 'limit' | 'api';
export class RunBlocked extends Error {
  constructor(public kind: RunBlockKind, message: string) { super(message); }
}

function classify(msg: string): RunBlockKind {
  const m = msg.toLowerCase();
  if (m.includes('domain') || m.includes('verif')) return 'domain';
  if (m.includes('limit') || m.includes('quota') || m.includes('exceed') || m.includes('max')) return 'limit';
  if (m.includes('draft') || m.includes('approve')) return 'draft';
  return 'api';
}

/** Problems that stop a scenario from running, checked in the UI before asking the server (plan limits: the server). */
export function runBlockers(scenarioId: string): RunBlocked[] {
  const sc = scenarios.find((s) => s.id === scenarioId);
  if (!sc) return [new RunBlocked('api', 'Scenario not found')];
  const out: RunBlocked[] = [];
  if (scenarioState(sc.id) === 'draft') out.push(new RunBlocked('draft', 'Generated from code — review & approve it before running.'));
  const t = checkTarget(sc.target);
  if (!t.ok) out.push(new RunBlocked('domain', t.reason));
  return out;
}

const isLocalScenarioId = (id: string) => /^lt\d+-[a-z0-9]{1,4}$/.test(id);

/**
 * Start a run after the UI checks; rejects with RunBlocked (draft / domain / limit / api error message).
 * `afterSave`: the scenario was just saved — wait for the save to reach the api first.
 */
export async function startRunChecked(scenarioId: string, afterSave = false): Promise<string> {
  const blocked = runBlockers(scenarioId);
  if (blocked.length) throw blocked[0];
  // a scenario created a moment ago gets its server id when the POST returns
  for (let k = 0; k < 50 && isLocalScenarioId(scenarioId) && serverId(scenarioId) === scenarioId; k++) await sleep(200);
  if (afterSave) await sleep(500);
  try {
    const raw = await api<Parameters<typeof mapRun>[0]>(`/loadtests/scenarios/${encodeURIComponent(serverId(scenarioId))}/runs`, { method: 'POST' });
    const run = mapRun(raw);
    run.scenarioId = scenarioId;
    if (!runs.some((r) => r.id === run.id)) runs.unshift(run);
    const sc = scenarios.find((s) => s.id === scenarioId);
    if (sc) sc.lastRun = run.id;
    notify();
    return run.id;
  } catch (e) {
    const msg = e instanceof Error ? e.message : String(e);
    throw new RunBlocked(classify(msg), msg);
  }
}
export { linkIds };

// ---------------------------------------------------------------- run detail

export interface StepStat { key: string; requests: number; errors: number; p50?: number; p95: number; p99?: number }
export interface RedSplit { rps: number[]; errorRate: number[]; p95: number[] }
export interface TargetRed { service: string; ts: number[]; total: RedSplit; test: RedSplit }
export interface RunLive { ts: number; vus: number; rps: number; p95: number; errorRate: number; progress: number }

export interface RunDetail {
  /** why the run failed to start / was stopped by the controller (e.g. domain not verified) */
  error?: string;
  live?: RunLive;
  finishedAt?: number;
  steps: StepStat[];
  /** "200" → count */
  statusCodes: Record<string, number>;
  /** load-generator series; undefined = not available (yet) */
  curves?: RunCurves;
  targets: TargetRed[];
  /** the series endpoint answered */
  loaded: boolean;
}

interface RawRunDetail {
  id: string; state: string; error?: string | null; live?: RunLive | null; finishedAt?: number | string | null;
  summary?: { steps?: StepStat[] | null; statusCodes?: Record<string, number> | null } | null;
  steps?: StepStat[] | null; statusCodes?: Record<string, number> | null;
  [k: string]: unknown;
}
interface RawSeries { ts?: number[] | null; vus?: number[] | null; rps?: number[] | null; p95?: number[] | null; errorRate?: number[] | null }
type RawRedPart = Partial<Record<'rps' | 'rpm' | 'errorRate' | 'p95', number[] | null>>;
interface RawTarget extends RawRedPart {
  /** api shape: timerange Set — series ids rpm, rpmLoadtest, errorRate, errorRateLoadtest, p95, p95Loadtest */
  timestamps?: number[] | null; series?: { id: string; data: number[] | null }[] | null;
  ts?: number[] | null; total?: RawRedPart | null; test?: RawRedPart | null;
  testRps?: number[] | null; testRpm?: number[] | null; testErrorRate?: number[] | null; testP95?: number[] | null;
}

const n = (a: number[] | null | undefined, len: number) => (a && a.length ? a.map((v) => v ?? 0) : Array<number>(len).fill(0));
function redPart(p: RawRedPart | null | undefined, len: number): RedSplit {
  const rps = p?.rps ?? (p?.rpm ? p.rpm.map((v) => v / 60) : undefined);
  return { rps: n(rps, len), errorRate: n(p?.errorRate, len), p95: n(p?.p95, len) };
}
function mapTarget(service: string, r: RawTarget): TargetRed | undefined {
  if (r.timestamps && r.series) {
    const ts = r.timestamps.map((t) => toMs(t) ?? 0);
    if (!ts.length) return undefined;
    const get = (id: string) => n(r.series!.find((x) => x.id === id)?.data, ts.length);
    return {
      service, ts,
      total: { rps: get('rpm').map((v) => v / 60), errorRate: get('errorRate'), p95: get('p95') },
      test: { rps: get('rpmLoadtest').map((v) => v / 60), errorRate: get('errorRateLoadtest'), p95: get('p95Loadtest') },
    };
  }
  const ts = (r.ts ?? []).map((t) => toMs(t) ?? 0);
  if (!ts.length) return undefined;
  const total = r.total ? redPart(r.total, ts.length) : redPart(r, ts.length);
  const test = r.test ? redPart(r.test, ts.length)
    : redPart({ rps: r.testRps ?? undefined, rpm: r.testRpm ?? undefined, errorRate: r.testErrorRate ?? undefined, p95: r.testP95 ?? undefined }, ts.length);
  return { service, ts, total, test };
}
function curvesFromSeries(s: RawSeries, peakVus: number): RunCurves | undefined {
  const ts = (s.ts ?? []).map((t) => toMs(t) ?? 0);
  if (!ts.length) return undefined;
  const vus = n(s.vus, ts.length);
  const peak = Math.max(1, peakVus, ...vus);
  return {
    ts, vus, rps: n(s.rps, ts.length), p95: n(s.p95, ts.length), err: n(s.errorRate, ts.length),
    load: vus.map((v) => v / peak), upto: ts.length, step: ts.length > 1 ? ts[1] - ts[0] : 5000,
  };
}

/** Run detail; polls every 5 s while the run is queued or running. */
export function useRunDetail(run: LoadTestRun | undefined): RunDetail {
  const [detail, setDetail] = useState<RunDetail>({ steps: [], statusCodes: {}, targets: [], loaded: false });
  const id = run?.id;
  const active = run?.state === 'running' || run?.state === 'queued';
  useEffect(() => {
    if (!id) return;
    let alive = true;
    const sid = encodeURIComponent(serverId(id));
    const get = <T,>(path: string) => api<T>(path).catch((e: unknown) => { if (!missing(e)) console.warn('[data]', path, e); return null; });
    const tick = async () => {
      const raw = await get<RawRunDetail>(`/loadtests/runs/${sid}`);
      const stored = runs.find((r) => r.id === id);
      if (raw && stored) {
        const m = mapRun(raw as unknown as Parameters<typeof mapRun>[0]);
        Object.assign(stored, { state: raw.state === 'error' ? 'failed' : m.state, progress: raw.live?.progress ?? m.progress, rps: m.rps, p95: m.p95, errorRate: m.errorRate, vus: m.vus, thresholds: m.thresholds });
      }
      const sc = scenarios.find((s) => s.id === (stored?.scenarioId ?? ''));
      const svcs = SPEC[sc?.id ?? '']?.services ?? [];
      const [ser, ...tg] = await Promise.all([
        get<RawSeries>(`/loadtests/runs/${sid}/series?step=5s`),
        ...svcs.map((s) => get<RawTarget>(`/loadtests/runs/${sid}/target?service=${encodeURIComponent(s)}`)),
      ]);
      if (!alive) return;
      setDetail({
        error: raw?.error ?? undefined,
        live: raw?.live ?? undefined,
        finishedAt: toMs(raw?.finishedAt ?? undefined),
        steps: raw?.summary?.steps ?? raw?.steps ?? [],
        statusCodes: raw?.summary?.statusCodes ?? raw?.statusCodes ?? {},
        curves: ser ? curvesFromSeries(ser, sc?.peakVus ?? 0) : undefined,
        targets: tg.map((t, k) => (t ? mapTarget(svcs[k], t) : undefined)).filter((t): t is TargetRed => !!t),
        loaded: true,
      });
      notify();
    };
    void tick();
    const t = active ? setInterval(() => { void tick(); }, 5000) : undefined;
    return () => { alive = false; if (t) clearInterval(t); };
  }, [id, active]);
  return detail;
}

/** Previous finished run of the same scenario, for comparison. */
export function previousRun(run: LoadTestRun): LoadTestRun | undefined {
  return runs
    .filter((r) => r.scenarioId === run.scenarioId && r.id !== run.id && r.startedAt < run.startedAt && r.state !== 'running' && r.state !== 'queued')
    .sort((a, b) => b.startedAt - a.startedAt)[0];
}

/** Error message of a run (api detail). */
export const runError = (d?: RunDetail) => d?.error;

// ---------------------------------------------------------------- hydration

async function loadDomains() {
  try {
    const raw = await api<RawDomain[] | null>('/loadtests/domains');
    domains.splice(0, domains.length, ...(raw ?? []).map(mapDomain));
    domainsAvailable = true;
  } catch (e) {
    domainsAvailable = false;
    if (!missing(e)) console.warn('[data] /loadtests/domains', e);
  }
}

async function loadStates() {
  try {
    const raw = await api<{ id: string; state?: string | null }[] | null>('/loadtests/scenarios');
    for (const r of raw ?? []) states.set(localId(String(r.id)), r.state === 'draft' ? 'draft' : 'ready');
  } catch { /* the scenario list hydrator reports errors */ }
}

registerHydrator(loadDomains);
registerHydrator(loadStates);
