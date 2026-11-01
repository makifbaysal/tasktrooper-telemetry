/**
 * Load test model (ADR-015): scenario specs (as stored by the api, plus edits made here), run start / stop /
 * scenario save through the api, and pure helpers for levels and stages. Run curves and results come from the
 * api (data/loadtest.ts `useRunDetail`).
 */
import { useEffect, useState } from 'react';
import {
  api, dependenciesFor, endpointsFor, hydrate, runs, scenarios, type Endpoint, type LoadTestRun, type LoadTestScenario,
} from '@/data';
import { linkIds, scenarioSpecs, serverId } from '@/data/api/ops';

export interface Stage { duration: number; target: number }

/** A load level as the user edits it: ramp to `target`, then hold it. */
export interface Level { name: string; ramp: number; hold: number; target: number }

/** What each request carries. Values may use template variables (`{{pool.order_ids}}`, `{{int 1-3}}`, …). */
export interface RequestSpec {
  /** one `name: value` per line — path params and query params */
  params: string;
  /** one `Header: value` per line */
  headers: string;
  /** JSON template; empty for GET/DELETE */
  body: string;
  /** data the templates draw from */
  pools: { name: string; detail: string }[];
  /** set when generated from production traffic */
  source?: { samples: number; window: string; masked: string[] };
}

export type LoadMode = 'vus' | 'rps';

export interface ScenarioSpec {
  /** services the scenario's requests land on */
  services: string[];
  stages: Stage[];
  /** "METHOD /route" of each step */
  steps: string[];
  thresholds: string[];
  /** vus = closed model (virtual users); rps = open model (arrival rate) */
  mode?: LoadMode;
  levels?: Level[];
  /** share of requests per step, sums to 1; defaults to the production mix */
  weights?: Record<string, number>;
  requests?: Record<string, RequestSpec>;
  /** seconds a virtual user waits between requests */
  thinkTime?: number;
  /** who moves the run to the next level: a timer, the user, or the AI gate */
  progression?: Progression;
  gate?: Gate;
}

export type Progression = 'auto' | 'manual' | 'ai';
/** AI gate limits: downstream p95 may grow to `latencyX` × production; CPU up to `cpu`. */
export interface Gate { latencyX: number; cpu: number }
export const DEFAULT_GATE: Gate = { latencyX: 3, cpu: 0.85 };

const arr = <T>(v: unknown, d: T[] = []): T[] => (Array.isArray(v) ? (v as T[]) : d);
/** Scenario spec JSON from the api → ScenarioSpec with every required field present. */
function specFromApi(raw: Record<string, unknown> | undefined): ScenarioSpec {
  const r = (raw ?? {}) as Partial<ScenarioSpec>;
  const levels = arr<Level>(r.levels);
  const stages = arr<Stage>(r.stages, levels.length ? levelsToStages(levels) : []);
  return {
    ...r, services: arr<string>(r.services), steps: arr<string>(r.steps), thresholds: arr<string>(r.thresholds),
    stages, levels: levels.length ? levels : undefined,
  };
}

/**
 * Scenario specs by scenario id: the `spec` each scenario was stored with (data/api/ops.ts), normalized;
 * edits made here win until the api has the saved version.
 */
const edited: Record<string, ScenarioSpec> = {};
export const SPEC: Record<string, ScenarioSpec> = new Proxy(edited, {
  get(t, k) {
    if (typeof k !== 'string') return undefined;
    if (t[k]) return t[k];
    if (scenarioSpecs.has(k) || scenarios.some((s) => s.id === k)) return specFromApi(scenarioSpecs.get(k));
    return undefined;
  },
});

export const REGIONS = ['eu-central', 'us-east', 'ap-south'];

// ---------- levels ----------

export const levelsToStages = (levels: Level[]): Stage[] =>
  levels.flatMap((l) => [{ duration: l.ramp, target: l.target }, { duration: l.hold, target: l.target }]).filter((s) => s.duration > 0);

/** Older specs only have stages: a stage that keeps the previous target is the hold of that level. */
export function levelsFromStages(stages: Stage[]): Level[] {
  const out: Level[] = [];
  let prev = 0;
  for (const s of stages) {
    const last = out[out.length - 1];
    if (last && s.target === prev) last.hold += s.duration;
    else out.push({ name: s.target === 0 ? 'Cool-down' : out.length === 0 ? 'Warm-up' : `Level ${out.length + 1}`, ramp: s.duration, hold: 0, target: s.target });
    prev = s.target;
  }
  return out;
}

export const levelsOf = (spec: ScenarioSpec) => spec.levels ?? levelsFromStages(spec.stages);

export const LEVEL_PRESETS: Record<string, (peak: number) => Level[]> = {
  'Ramp & hold': (p) => [{ name: 'Warm-up', ramp: 60, hold: 60, target: Math.round(p * 0.25) }, { name: 'Peak', ramp: 120, hold: 300, target: p }, { name: 'Cool-down', ramp: 60, hold: 0, target: 0 }],
  'Step 1× → 2× → 3×': (p) => [1, 2, 3].map((k) => ({ name: `${k}× level`, ramp: 60, hold: 180, target: Math.round((p / 3) * k) })).concat([{ name: 'Cool-down', ramp: 60, hold: 0, target: 0 }]),
  Spike: (p) => [{ name: 'Baseline', ramp: 60, hold: 120, target: Math.round(p * 0.2) }, { name: 'Spike', ramp: 10, hold: 60, target: p }, { name: 'Recovery', ramp: 10, hold: 120, target: Math.round(p * 0.2) }, { name: 'Cool-down', ramp: 30, hold: 0, target: 0 }],
  Soak: (p) => [{ name: 'Warm-up', ramp: 120, hold: 0, target: Math.round(p * 0.6) }, { name: 'Soak', ramp: 0, hold: 3600, target: Math.round(p * 0.6) }, { name: 'Cool-down', ramp: 60, hold: 0, target: 0 }],
};

/** Empty template for a manually added step: route params listed, values left to the user. */
export function blankRequestSpec(e: Endpoint): RequestSpec {
  const params = e.route.split('/').filter((x) => x.startsWith(':')).map((x) => `${x.slice(1)}: `);
  const body = e.method === 'GET' || e.method === 'DELETE' ? '' : '{\n  \n}';
  return { params: params.join('\n'), headers: ['Authorization: Bearer {{session.token}}', ...(body ? ['Content-Type: application/json'] : [])].join('\n'), body, pools: [] };
}

// ---------- store ----------

const listeners = new Set<() => void>();
const emit = () => listeners.forEach((f) => f());

/** Re-renders on store changes. */
export function useLoadTests() {
  const [, bump] = useState(0);
  useEffect(() => {
    const f = () => bump((n) => n + 1);
    listeners.add(f);
    return () => { listeners.delete(f); };
  }, []);
}

export const scenarioOf = (r: LoadTestRun): LoadTestScenario => scenarios.find((s) => s.id === r.scenarioId)
  ?? { id: r.scenarioId, name: 'Deleted scenario', target: '', source: 'manual', steps: 0, peakVus: r.vus, duration: 0, regions: [] };

/** A run as stored (state and progress come from the api; data/loadtest.ts polls the detail while it runs). */
export const liveRun = (r: LoadTestRun): LoadTestRun => r;

/** Parse "p(95) < 500 ms" / "rate < 1%" into a numeric limit. */
export function limitOf(name: string) {
  const m = /<\s*([\d.]+)\s*(ms|%)/.exec(name);
  if (!m) return undefined;
  return m[2] === '%' ? Number(m[1]) / 100 : Number(m[1]);
}

/** Endpoints hit by a scenario's steps, across its services. */
export function scenarioEndpoints(sc: LoadTestScenario): Endpoint[] {
  const spec = SPEC[sc.id];
  if (!spec) return [];
  return spec.services.flatMap((s) => endpointsFor(s)).filter((e) => spec.steps.includes(`${e.method} ${e.route}`));
}

/** Refresh the lists after a write; the local id keeps pointing at the new item. */
/** scenario saves in flight (a run started right after "save & run" waits for its server id) */
const saving = new Map<string, Promise<unknown>>();
const refresh = () => { void hydrate().catch((e: unknown) => console.warn('[data] refresh', e)).finally(emit); };

/**
 * Start a run: POST /loadtests/scenarios/{id}/runs — the run is queued; the returned id is linked to the
 * local one so the run page keeps working.
 */
export function startRun(scenarioId: string): string {
  const sc = scenarios.find((s) => s.id === scenarioId);
  const id = `run-${Date.now().toString(36)}`;
  const run: LoadTestRun = { id, scenarioId, state: 'queued', startedAt: Date.now(), progress: 0, rps: 0, p95: 0, errorRate: 0, vus: sc?.peakVus ?? 0, thresholds: [] };
  runs.unshift(run);
  if (sc) sc.lastRun = id;
  emit();
  Promise.resolve(saving.get(scenarioId))
    .then(() => api<{ id: string }>(`/loadtests/scenarios/${encodeURIComponent(serverId(scenarioId))}/runs`, { method: 'POST' }))
    .then((r) => { linkIds(id, r.id); refresh(); })
    .catch((e: unknown) => { console.warn('[data] start run', e); run.state = 'failed'; emit(); });
  return id;
}
export function stopRun(id: string) {
  const r = runs.find((x) => x.id === id);
  if (!r) return;
  const prev = r.state;
  r.state = 'aborted';
  emit();
  api(`/loadtests/runs/${encodeURIComponent(serverId(id))}/stop`, { method: 'POST' })
    .then(refresh)
    .catch((e: unknown) => { console.warn('[data] stop run', e); r.state = prev; emit(); });
}

export interface ScenarioDraft {
  name: string; target: string; services: string[]; regions: string[]; source: LoadTestScenario['source'];
  mode: LoadMode; levels: Level[]; steps: string[]; weights: Record<string, number>; requests: Record<string, RequestSpec>;
  thinkTime: number; p95Limit?: number; errLimit: number;
  progression: Progression; gate: Gate;
}

/** Create a scenario, or update it in place when `id` is given. */
export function saveScenario(d: ScenarioDraft, id?: string): string {
  const stages = levelsToStages(d.levels);
  const total = stages.reduce((a, x) => a + x.duration, 0);
  const peak = Math.max(0, ...d.levels.map((l) => l.target));
  // open model: VUs needed ≈ arrival rate × (latency + think time); the UI shows the rate
  const peakVus = d.mode === 'rps' ? Math.max(1, Math.ceil(peak * (0.2 + d.thinkTime))) : peak;
  const meta = { name: d.name, target: d.target, source: d.source, steps: d.steps.length, peakVus, duration: total, regions: d.regions };
  const sid = id ?? `lt${scenarios.length + 1}-${Date.now().toString(36).slice(-4)}`;
  const existing = scenarios.find((x) => x.id === sid);
  if (existing) Object.assign(existing, meta);
  else scenarios.push({ id: sid, ...meta });
  const spec: ScenarioSpec = {
    services: d.services, stages, levels: d.levels, steps: d.steps, mode: d.mode, weights: d.weights, requests: d.requests, thinkTime: d.thinkTime,
    progression: d.progression, gate: d.gate,
    thresholds: [`http_req_failed rate < ${+(d.errLimit * 100).toFixed(2)}%`, ...(d.p95Limit ? [`http_req_duration p(95) < ${d.p95Limit} ms`] : [])],
  };
  SPEC[sid] = spec;
  emit();
  // the api derives target / steps / peak / duration / regions from the spec
  const body = { name: d.name, source: d.source, spec: { ...spec, target: d.target, regions: d.regions } };
  const req = existing && id
    ? api(`/loadtests/scenarios/${encodeURIComponent(serverId(sid))}`, { method: 'PUT', body: JSON.stringify(body) })
    : api<{ id: string }>('/loadtests/scenarios', { method: 'POST', body: JSON.stringify(body) }).then((r) => linkIds(sid, r.id));
  const done = req.then(refresh).catch((e: unknown) => console.warn('[data] save scenario', e)).finally(() => saving.delete(sid));
  saving.set(sid, done);
  return sid;
}

// ---------- stages ----------

export const POINTS = 60;

export function stageAt(sc: LoadTestScenario, progress: number) {
  const st = SPEC[sc.id]?.stages ?? [];
  const total = st.reduce((a, s) => a + s.duration, 0) || 1;
  let acc = 0;
  for (let k = 0; k < st.length; k++) {
    acc += st[k].duration;
    if (progress * total <= acc) return { index: k, count: st.length, target: st[k].target };
  }
  return { index: st.length - 1, count: st.length, target: st[st.length - 1]?.target ?? 0 };
}

/** VUs at `frac` (0..1) of the scenario: linear ramps between stage targets, starting from 0. */
export function vusAt(stages: Stage[], frac: number) {
  const total = stages.reduce((a, s) => a + s.duration, 0) || 1;
  let t = frac * total;
  let from = 0;
  for (const s of stages) {
    if (t <= s.duration) return from + (s.target - from) * (t / (s.duration || 1));
    t -= s.duration;
    from = s.target;
  }
  return from;
}

/** Load-generator series of a run (data/loadtest.ts `useRunDetail`). */
export interface RunCurves {
  ts: number[]; vus: number[]; rps: number[]; p95: number[]; err: number[];
  /** fraction of peak load at each point */
  load: number[];
  upto: number; step: number;
}

/** First database dependency of a service, for the "target services" panel. */
export const dbOf = (serviceId: string) => dependenciesFor(serviceId).find((d) => d.kind === 'db');
