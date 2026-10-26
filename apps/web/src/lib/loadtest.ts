/**
 * Load test model for the mock UI: scenario specs, a tiny in-memory store (start / stop / create)
 * and the per-run curves. Every curve is derived from the scenario's stages, so VUs, RPS,
 * latency and errors move together and the summary numbers match the charts.
 */
import { useEffect, useState } from 'react';
import {
  api, dependenciesFor, endpointsFor, hydrate, isApi, runs, scenarios, series, services, type Endpoint, type LoadTestRun, type LoadTestScenario,
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

const MOCK_SPEC: Record<string, ScenarioSpec> = {
  lt1: {
    services: ['orders-api', 'payments-api'],
    progression: 'ai',
    levels: [
      { name: 'Warm-up', ramp: 30, hold: 30, target: 50 },
      { name: 'Level 2', ramp: 30, hold: 60, target: 100 },
      { name: 'Level 3', ramp: 30, hold: 60, target: 150 },
      { name: 'Peak', ramp: 30, hold: 120, target: 200 },
      { name: 'Cool-down', ramp: 60, hold: 0, target: 0 },
    ],
    stages: [{ duration: 30, target: 50 }, { duration: 30, target: 50 }, { duration: 30, target: 100 }, { duration: 60, target: 100 }, { duration: 30, target: 150 }, { duration: 60, target: 150 }, { duration: 30, target: 200 }, { duration: 120, target: 200 }, { duration: 60, target: 0 }],
    steps: ['GET /v1/cart', 'POST /v1/orders', 'GET /v1/orders/:id', 'POST /v1/charges', 'GET /v1/charges/:id'],
    thresholds: ['http_req_failed rate < 1%', 'http_req_duration p(95) < 500 ms'],
  },
  lt2: {
    services: ['catalog-api'],
    stages: [{ duration: 120, target: 200 }, { duration: 660, target: 600 }, { duration: 120, target: 0 }],
    steps: ['GET /v1/items', 'GET /v1/items/:id', 'GET /v1/search'],
    thresholds: ['http_req_failed rate < 1%', 'http_req_duration p(95) < 200 ms'],
  },
  lt3: {
    services: ['auth-api'],
    stages: [{ duration: 60, target: 80 }, { duration: 3540, target: 80 }],
    steps: ['POST /v1/auth/login', 'POST /v1/auth/refresh'],
    thresholds: ['http_req_failed rate < 1%'],
  },
};

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
 * Scenario specs by scenario id. Mock: the table above (+ what the editor saves). Api mode: the
 * `spec` each scenario was stored with (data/api/ops.ts), normalized; edits made here win.
 */
export const SPEC: Record<string, ScenarioSpec> = new Proxy(MOCK_SPEC, {
  get(t, k) {
    if (typeof k !== 'string') return undefined;
    if (t[k]) return t[k];
    if (isApi && (scenarioSpecs.has(k) || scenarios.some((s) => s.id === k))) return specFromApi(scenarioSpecs.get(k));
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

// ---------- request data from production traffic (AI, mocked) ----------

const singular = (seg: string) => seg.replace(/ies$/, 'y').replace(/s$/, '');

/**
 * What the AI derives from sampled production requests for one endpoint: parameter
 * distributions, header mix, a body template and the data pools it needs. Secrets and
 * PII never leave production — they are replaced by staging pools.
 */
export function aiRequestSpec(e: Endpoint): RequestSpec {
  const segs = e.route.split('/').filter(Boolean);
  const resource = singular(segs.filter((x) => !x.startsWith(':') && x !== 'v1').slice(-1)[0] ?? 'item');
  const pools: RequestSpec['pools'] = [];
  const params: string[] = [];
  for (const sgm of segs.filter((x) => x.startsWith(':'))) {
    const owner = singular(segs[segs.indexOf(sgm) - 1] ?? resource);
    params.push(`${sgm.slice(1)}: {{pool.${owner}_ids}}`);
    pools.push({ name: `${owner}_ids`, detail: `${compactN(2000 + (e.rpm % 7) * 500)} ${owner} ids seen in production responses, last 7 days` });
  }
  const route = `${e.method} ${e.route}`;
  let body = '';
  const masked = ['Authorization', 'Cookie'];
  if (route === 'GET /v1/search') {
    params.push('q: {{pool.search_terms}}', 'page: {{weighted 1:92 2:6 3:2}}');
    pools.push({ name: 'search_terms', detail: '2,000 most frequent queries, weighted by frequency' });
  } else if (route === 'GET /v1/items' || route === 'GET /v1/orders') {
    params.push('limit: {{weighted 20:71 50:29}}', ...(route === 'GET /v1/items' ? ['category: {{pool.categories}}'] : ['status: {{weighted open:64 shipped:28 cancelled:8}}']));
    if (route === 'GET /v1/items') pools.push({ name: 'categories', detail: '48 categories, weighted by browse volume' });
  } else if (route === 'POST /v1/orders') {
    body = JSON.stringify({ cart_id: '{{session.cart_id}}', shipping_address_id: '{{pool.address_ids}}', payment_method: '{{weighted card:78 wallet:15 bnpl:7}}', coupon: '{{optional 12% pool.coupons}}' }, null, 2);
    pools.push({ name: 'address_ids', detail: '1,200 staging addresses (production addresses are PII, not copied)' }, { name: 'coupons', detail: '35 active staging coupon codes' });
    masked.push('shipping_address', 'email');
  } else if (route === 'POST /v1/charges') {
    body = JSON.stringify({ order_id: '{{prev "POST /v1/orders" .id}}', amount: '{{prev "POST /v1/orders" .total}}', currency: '{{weighted TRY:91 EUR:6 USD:3}}', provider_token: '{{pool.test_card_tokens}}' }, null, 2);
    pools.push({ name: 'test_card_tokens', detail: 'iyzico sandbox tokens — real card data is never sampled' });
    masked.push('card_number', 'cvc', 'provider_token');
  } else if (/cart\/items/.test(e.route) && e.method !== 'GET') {
    body = JSON.stringify({ sku: '{{pool.skus}}', qty: '{{weighted 1:81 2:14 3:5}}' }, null, 2);
    pools.push({ name: 'skus', detail: '6,400 SKUs, weighted by add-to-cart frequency' });
  } else if (route === 'POST /v1/auth/login') {
    body = JSON.stringify({ email: '{{pool.test_users.email}}', password: '{{pool.test_users.password}}' }, null, 2);
    pools.push({ name: 'test_users', detail: '500 seeded staging accounts — production credentials are never sampled' });
    masked.push('password', 'email');
  } else if (route === 'POST /v1/auth/refresh') {
    body = JSON.stringify({ refresh_token: '{{prev "POST /v1/auth/login" .refresh_token}}' }, null, 2);
    masked.push('refresh_token');
  } else if (e.method !== 'GET' && e.method !== 'DELETE') {
    body = JSON.stringify({ name: '{{faker.words 2}}', note: '{{optional 30% faker.sentence}}' }, null, 2);
  }
  const auth = /\/auth\/(login|refresh)/.test(e.route) ? [] : ['Authorization: Bearer {{session.token}}'];
  const headers = [
    ...auth,
    ...(body ? ['Content-Type: application/json'] : []),
    'Accept-Language: {{weighted tr-TR:83 en-US:11 de-DE:6}}',
    'X-Client: {{weighted web:54 ios:28 android:18}}',
  ];
  if (auth.length) pools.push({ name: 'session.token', detail: 'one staging login per virtual user, reused for its requests' });
  return { params: params.join('\n'), headers: headers.join('\n'), body, pools, source: { samples: Math.round(e.rpm * 60 * 24 * 7 * 0.01), window: 'last 7 days · 1% sample', masked } };
}

/** Empty template for a manually added step: route params listed, values left to the user. */
export function blankRequestSpec(e: Endpoint): RequestSpec {
  const params = e.route.split('/').filter((x) => x.startsWith(':')).map((x) => `${x.slice(1)}: `);
  const body = e.method === 'GET' || e.method === 'DELETE' ? '' : '{\n  \n}';
  return { params: params.join('\n'), headers: ['Authorization: Bearer {{session.token}}', ...(body ? ['Content-Type: application/json'] : [])].join('\n'), body, pools: [] };
}

function compactN(n: number) { return n >= 1000 ? `${(n / 1000).toFixed(n % 1000 ? 1 : 0)}K` : String(n); }

// ---------- store ----------

const listeners = new Set<() => void>();
const emit = () => listeners.forEach((f) => f());

/** Re-renders on store changes, and every 2 s while a run is in progress so progress moves. */
export function useLoadTests() {
  const [, bump] = useState(0);
  useEffect(() => {
    const f = () => bump((n) => n + 1);
    listeners.add(f);
    const t = setInterval(() => { if (runs.some((r) => liveRun(r).state === 'running')) f(); }, 2000);
    return () => { listeners.delete(f); clearInterval(t); };
  }, []);
}

export const scenarioOf = (r: LoadTestRun): LoadTestScenario => scenarios.find((s) => s.id === r.scenarioId)
  ?? { id: r.scenarioId, name: 'Deleted scenario', target: '', source: 'manual', steps: 0, peakVus: r.vus, duration: 0, regions: [] };

/**
 * Running runs are driven by the level engine below: it catches up to the clock, moves between
 * levels (timer / user / AI gate) and finishes the run after the last level.
 */
export function liveRun(r: LoadTestRun): LoadTestRun {
  if (r.state !== 'running') return r;
  advance(r, Date.now());
  if (r.state !== 'running') return r;
  const sc = scenarioOf(r);
  const elapsed = (Date.now() - r.startedAt) / 1000;
  return { ...r, progress: Math.min(0.99, elapsed / (sc.duration || 1)) };
}

/** Parse "p(95) < 500 ms" / "rate < 1%" into a numeric limit. */
export function limitOf(name: string) {
  const m = /<\s*([\d.]+)\s*(ms|%)/.exec(name);
  if (!m) return undefined;
  return m[2] === '%' ? Number(m[1]) / 100 : Number(m[1]);
}

function judge(thresholds: string[], p95: number, errorRate: number) {
  return thresholds.map((name) => {
    const lim = limitOf(name);
    const v = name.includes('duration') ? p95 : errorRate;
    return { name, passed: lim === undefined || v < lim };
  });
}

/** Endpoints hit by a scenario's steps, across its services. */
export function scenarioEndpoints(sc: LoadTestScenario): Endpoint[] {
  const spec = SPEC[sc.id];
  if (!spec) return [];
  return spec.services.flatMap((s) => endpointsFor(s)).filter((e) => spec.steps.includes(`${e.method} ${e.route}`));
}

/** Api mode: refresh the lists after a write; the local id keeps pointing at the new item. */
/** scenario saves in flight (a run started right after "save & run" waits for its server id) */
const saving = new Map<string, Promise<unknown>>();
const refresh = () => { void hydrate().catch((e: unknown) => console.warn('[data] refresh', e)).finally(emit); };

/**
 * Start a run. Api mode: POST /loadtests/scenarios/{id}/runs — the run is queued (the runner is not
 * implemented yet); the returned id is linked to the local one so the run page keeps working.
 */
export function startRun(scenarioId: string): string {
  if (isApi) {
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
  const sc = scenarios.find((s) => s.id === scenarioId)!;
  const spec = SPEC[sc.id];
  const prev = runs.find((r) => r.scenarioId === sc.id && r.state !== 'running');
  const eps = scenarioEndpoints(sc);
  const jitter = 0.92 + Math.random() * 0.16;
  const p95 = Math.round((prev?.p95 ?? Math.max(50, ...eps.map((e) => e.p95)) * 1.6) * jitter);
  const errorRate = (prev?.errorRate ?? 0.002) * jitter;
  const rps = Math.round(prev?.rps ?? sc.peakVus * 2);
  const id = `r${Math.max(0, ...runs.map((x) => Number(x.id.slice(1)) || 0)) + 1}`;
  runs.unshift({ id, scenarioId: sc.id, state: 'running', startedAt: Date.now(), progress: 0, rps, p95, errorRate, vus: sc.peakVus, thresholds: judge(spec.thresholds, p95, errorRate) });
  sc.lastRun = id;
  emit();
  return id;
}

export function stopRun(id: string) {
  const r = runs.find((x) => x.id === id);
  if (!r) return;
  if (isApi) {
    const prev = r.state;
    r.state = 'aborted';
    emit();
    api(`/loadtests/runs/${encodeURIComponent(serverId(id))}/stop`, { method: 'POST' })
      .then(refresh)
      .catch((e: unknown) => { console.warn('[data] stop run', e); r.state = prev; emit(); });
    return;
  }
  const now = liveRun(r);
  const c = controlOf(r);
  const t = Date.now();
  c.points.push({ t, v: targetAt(r, c, t) });
  c.decisions.push({ at: t, actor: 'you', kind: 'stop', text: 'Stopped the run' });
  Object.assign(r, { state: 'aborted', progress: now.progress });
  c.endedAt = t;
  emit();
}

// ---------- level engine: timer, manual or AI-gated transitions ----------

export interface Decision {
  at: number;
  actor: 'ai' | 'you' | 'timer';
  kind: 'advance' | 'hold' | 'back' | 'wait' | 'done' | 'stop' | 'resume';
  text: string;
  reasons?: string[];
}

interface Control {
  level: number;
  /** waiting = the level's hold is over and nobody moved on (manual, AI held, or user pressed Hold) */
  phase: 'ramp' | 'hold' | 'waiting';
  phaseAt: number;
  /** load at the start of the current ramp */
  from: number;
  /** load actually applied, as (time, target) corners — the curves interpolate between them */
  points: { t: number; v: number }[];
  decisions: Decision[];
  /** set when the user pressed Hold — the engine won't move on by itself */
  userHold?: boolean;
  /** load frozen by a Hold pressed mid-ramp */
  frozen?: number;
  endedAt?: number;
}

const controls: Record<string, Control> = {};

export const levelsOfRun = (r: LoadTestRun) => levelsOf(SPEC[r.scenarioId]);
export const progressionOf = (r: LoadTestRun): Progression => SPEC[r.scenarioId]?.progression ?? 'auto';
const peakOf = (levels: Level[]) => Math.max(1, ...levels.map((l) => l.target));

function controlOf(r: LoadTestRun): Control {
  return (controls[r.id] ??= { level: 0, phase: 'ramp', phaseAt: r.startedAt, from: 0, points: [{ t: r.startedAt, v: 0 }], decisions: [] });
}
export const hasControl = (r: LoadTestRun) => !!controls[r.id];

function targetAt(r: LoadTestRun, c: Control, t: number) {
  const lv = levelsOfRun(r)[c.level];
  if (c.frozen !== undefined) return c.frozen;
  if (c.phase !== 'ramp') return lv.target;
  const f = lv.ramp ? Math.min(1, (t - c.phaseAt) / (lv.ramp * 1000)) : 1;
  return c.from + (lv.target - c.from) * f;
}

function goTo(r: LoadTestRun, c: Control, level: number, at: number) {
  const levels = levelsOfRun(r);
  const cur = targetAt(r, c, at);
  c.points.push({ t: at, v: cur });
  c.level = Math.max(0, Math.min(levels.length - 1, level));
  c.phase = 'ramp';
  c.phaseAt = at;
  c.from = cur;
  c.userHold = false;
  c.frozen = undefined;
}

function finish(r: LoadTestRun, c: Control, at: number) {
  c.points.push({ t: at, v: levelsOfRun(r)[c.level].target });
  c.endedAt = at;
  const ok = r.thresholds.every((t) => t.passed);
  c.decisions.push({ at, actor: 'timer', kind: 'done', text: ok ? 'Run finished — all thresholds passed' : 'Run finished — thresholds failed' });
  Object.assign(r, { state: ok ? 'passed' : 'failed', progress: 1 });
}

/** Catch the run up to `now`: end ramps and holds that are due and decide what happens next. */
function advance(r: LoadTestRun, now: number) {
  const c = controlOf(r);
  const levels = levelsOfRun(r);
  for (let guard = 0; guard < 50 && r.state === 'running'; guard++) {
    const lv = levels[c.level];
    if (c.phase === 'ramp') {
      const end = c.phaseAt + lv.ramp * 1000;
      if (now < end) return;
      c.points.push({ t: end, v: lv.target });
      c.phase = 'hold';
      c.phaseAt = end;
      continue;
    }
    if (c.phase === 'waiting') return;
    const end = c.phaseAt + lv.hold * 1000;
    if (now < end) return;
    if (c.level === levels.length - 1) { finish(r, c, end); return; }
    decide(r, c, end, levels);
  }
}

function decide(r: LoadTestRun, c: Control, at: number, levels: Level[]) {
  const next = levels[c.level + 1];
  const lv = levels[c.level];
  const mode = progressionOf(r);
  // ramping down is always safe
  if (mode === 'auto' || next.target <= lv.target) {
    c.decisions.push({ at, actor: 'timer', kind: 'advance', text: `${lv.name} done → ${next.name} (${next.target})` });
    goTo(r, c, c.level + 1, at);
    return;
  }
  if (mode === 'manual') {
    c.decisions.push({ at, actor: 'timer', kind: 'wait', text: `${lv.name} held for ${Math.round(lv.hold)}s — waiting for you to move to ${next.name}` });
    c.phase = 'waiting'; c.phaseAt = at;
    return;
  }
  const g = gateVerdict(r, c.level);
  if (g.verdict === 'back' && c.level > 0) {
    const prev = levels[c.level - 1];
    c.decisions.push({ at, actor: 'ai', kind: 'back', text: `Stepped back to ${prev.name} (${prev.target}) — ${lv.name} is already hurting a dependency`, reasons: g.reasons });
    goTo(r, c, c.level - 1, at);
    return;
  }
  if (g.verdict === 'hold' || g.verdict === 'back') {
    c.decisions.push({ at, actor: 'ai', kind: 'hold', text: `Holding at ${lv.name} (${lv.target}) — ${next.name} is not safe`, reasons: g.reasons });
    c.phase = 'waiting'; c.phaseAt = at;
    return;
  }
  c.decisions.push({ at, actor: 'ai', kind: 'advance', text: `${lv.name} healthy → raising to ${next.name} (${next.target})`, reasons: g.reasons });
  goTo(r, c, c.level + 1, at);
}

export type ControlAction = 'next' | 'hold' | 'resume' | 'back';

/** User moves: go to the next level now, hold here, let the engine continue, or step one level down. */
export function controlRun(id: string, action: ControlAction) {
  const r = runs.find((x) => x.id === id);
  if (!r || r.state !== 'running') return;
  advance(r, Date.now());
  const c = controlOf(r);
  const levels = levelsOfRun(r);
  const at = Date.now();
  const lv = levels[c.level];
  if (action === 'next' && c.level < levels.length - 1) {
    const g = progressionOf(r) === 'ai' ? gateVerdict(r, c.level) : undefined;
    const against = g && g.verdict !== 'go';
    c.decisions.push({ at, actor: 'you', kind: 'advance', text: `Moved to ${levels[c.level + 1].name} (${levels[c.level + 1].target})${against ? ' against the AI recommendation' : ''}` });
    goTo(r, c, c.level + 1, at);
  } else if (action === 'back' && c.level > 0) {
    c.decisions.push({ at, actor: 'you', kind: 'back', text: `Stepped back to ${levels[c.level - 1].name} (${levels[c.level - 1].target})` });
    goTo(r, c, c.level - 1, at);
  } else if (action === 'hold') {
    // freeze where we are, even mid-ramp
    const v = targetAt(r, c, at);
    c.points.push({ t: at, v });
    c.frozen = v;
    c.decisions.push({ at, actor: 'you', kind: 'hold', text: `Holding at ${Math.round(v)} — progression paused` });
    c.phase = 'waiting'; c.phaseAt = at; c.userHold = true;
  } else if (action === 'resume') {
    c.decisions.push({ at, actor: 'you', kind: 'resume', text: `Resumed ${lv.name}` });
    c.userHold = false;
    if (c.frozen !== undefined && c.frozen !== lv.target) {
      // finish the interrupted ramp
      c.from = c.frozen; c.frozen = undefined; c.phase = 'ramp'; c.phaseAt = at;
    } else {
      // hold already served: let the timer / AI decide right away
      c.frozen = undefined; c.phase = 'hold'; c.phaseAt = at - lv.hold * 1000;
      advance(r, at);
    }
  }
  emit();
}

export interface LevelState {
  mode: Progression;
  levels: Level[];
  index: number;
  phase: Control['phase'];
  /** load right now */
  now: number;
  /** seconds left in the current ramp/hold; undefined while waiting */
  left?: number;
  userHold: boolean;
  decisions: Decision[];
}

export function levelState(r: LoadTestRun): LevelState {
  const c = controlOf(r);
  const levels = levelsOfRun(r);
  const t = c.endedAt ?? Date.now();
  const lv = levels[c.level];
  const end = c.phase === 'ramp' ? c.phaseAt + lv.ramp * 1000 : c.phase === 'hold' ? c.phaseAt + lv.hold * 1000 : undefined;
  return {
    mode: progressionOf(r), levels, index: c.level, phase: c.phase, now: targetAt(r, c, t),
    left: end === undefined ? undefined : Math.max(0, (end - t) / 1000), userHold: !!c.userHold, decisions: c.decisions,
  };
}

// ---------- AI gate: is the API and everything it calls fine at this level, and at the next? ----------

export type CheckStatus = 'ok' | 'warn' | 'bad';
export interface Check {
  id: string;
  label: string;
  /** service or dependency the check is about */
  scope: string;
  now: number;
  next?: number;
  limit: number;
  warnAt: number;
  fmt: (v: number) => string;
  statusNow: CheckStatus;
  statusNext?: CheckStatus;
}

const statusOf = (v: number, warn: number, limit: number): CheckStatus => (v >= limit ? 'bad' : v >= warn ? 'warn' : 'ok');
const fmtMs = (v: number) => (v >= 1000 ? `${(v / 1000).toFixed(2)} s` : `${Math.round(v)} ms`);
const fmtPct = (v: number) => `${(v * 100).toFixed(v < 0.01 ? 2 : 1)}%`;

/** Health checks at load fraction `l` (and projected at `next`), for the API under test and its dependencies. */
export function gateChecks(r: LoadTestRun, l: number, next?: number): Check[] {
  const spec = SPEC[r.scenarioId];
  const gate = spec.gate ?? DEFAULT_GATE;
  const p95Lim = limitOf(spec.thresholds.find((t) => t.includes('duration')) ?? '');
  const errLim = limitOf(spec.thresholds.find((t) => t.includes('failed')) ?? '') ?? 0.01;
  const out: Check[] = [];
  const add = (c: Omit<Check, 'statusNow' | 'statusNext'>) => out.push({
    ...c, statusNow: statusOf(c.now, c.warnAt, c.limit), statusNext: c.next === undefined ? undefined : statusOf(c.next, c.warnAt, c.limit),
  });
  const runP95 = (x: number) => r.p95 * (0.45 + 0.55 * x ** 1.5);
  const runErr = (x: number) => r.errorRate * (0.2 + 0.8 * x ** 2);
  if (p95Lim) add({ id: 'p95', label: 'Response time p95', scope: 'load generator', now: runP95(l), next: next === undefined ? undefined : runP95(next), limit: p95Lim, warnAt: p95Lim * 0.8, fmt: fmtMs });
  add({ id: 'err', label: 'Failed requests', scope: 'load generator', now: runErr(l), next: next === undefined ? undefined : runErr(next), limit: errLim, warnAt: errLim * 0.6, fmt: fmtPct });
  for (const sid of spec.services) {
    const svc = services.find((s) => s.id === sid);
    const heat = Math.min(0.75, 0.25 + 2.4 / (svc?.instances ?? 4));
    const cpu = (x: number) => 0.12 + heat * x;
    add({ id: `cpu-${sid}`, label: 'CPU', scope: sid, now: cpu(l), next: next === undefined ? undefined : cpu(next), limit: gate.cpu, warnAt: gate.cpu - 0.15, fmt: (v) => `${Math.round(v * 100)}%` });
    for (const d of dependenciesFor(sid)) {
      // dependencies already unhealthy in production degrade much faster under load
      const sens = d.errorRate > 0.01 ? 3.5 : d.kind === 'http' ? 1.2 : 0.8;
      const lat = (x: number) => d.p95 * (1 + sens * x ** 2);
      const er = (x: number) => d.errorRate * (1 + sens * x ** 2);
      add({ id: `lat-${sid}-${d.id}`, label: 'p95', scope: d.name, now: lat(l), next: next === undefined ? undefined : lat(next), limit: d.p95 * gate.latencyX, warnAt: d.p95 * gate.latencyX * 0.66, fmt: fmtMs });
      if (d.errorRate > 0) add({ id: `err-${sid}-${d.id}`, label: 'errors', scope: d.name, now: er(l), next: next === undefined ? undefined : er(next), limit: 0.1, warnAt: 0.066, fmt: fmtPct });
    }
  }
  const rank = { bad: 0, warn: 1, ok: 2 };
  return out.sort((a, b) => rank[a.statusNext ?? a.statusNow] - rank[b.statusNext ?? b.statusNow] || rank[a.statusNow] - rank[b.statusNow]);
}

export interface GateVerdict {
  verdict: 'go' | 'hold' | 'back';
  checks: Check[];
  /** one line per reason, worst first */
  reasons: string[];
  /** what the user could do about it */
  advice: string[];
}

export function gateVerdict(r: LoadTestRun, level: number): GateVerdict {
  const levels = levelsOfRun(r);
  const peak = peakOf(levels);
  const cur = levels[level];
  const nxt = levels[level + 1];
  const checks = gateChecks(r, cur.target / peak, nxt && nxt.target > cur.target ? nxt.target / peak : undefined);
  const badNow = checks.filter((c) => c.statusNow === 'bad');
  const badNext = checks.filter((c) => c.statusNext === 'bad');
  const warnNow = checks.filter((c) => c.statusNow === 'warn');
  const line = (c: Check, v: number) => `${c.scope} ${c.label} ${c.fmt(v)} (limit ${c.fmt(c.limit)})`;
  const reasons = badNow.length
    ? badNow.map((c) => `Now: ${line(c, c.now)}`)
    : badNext.length
      ? [...badNext.map((c) => `At ${nxt.name}: ${line(c, c.next!)} — projected`), ...warnNow.map((c) => `Now: ${line(c, c.now)} — close to the limit`)]
      : warnNow.length ? warnNow.map((c) => `Watch: ${line(c, c.now)}`) : [`All ${checks.length} checks green at ${cur.name}`];
  // blame the deepest system: a third-party API beats one of our own services that merely waits on it
  const ours = (scope: string) => services.some((s) => scope.endsWith(` ${s.name}`) || scope === s.id);
  const failing = badNow.length ? badNow : badNext;
  const worst = failing.find((c) => !ours(c.scope) && c.label !== 'CPU') ?? failing[0];
  const victims = failing.filter((c) => c !== worst && ours(c.scope) && c.scope !== worst?.scope).map((c) => c.scope.replace(/^HTTP · /, ''));
  const advice: string[] = [];
  if (worst) {
    if (/HTTP/.test(worst.scope) && !ours(worst.scope)) advice.push(`${worst.scope.replace(/^HTTP · /, '')} (third party) is the bottleneck${victims.length ? ` — ${[...new Set(victims)].join(', ')} slow${victims.length === 1 ? 's' : ''} down because ${victims.length === 1 ? 'it waits' : 'they wait'} on it` : ''}. Add a timeout + circuit breaker, or test against the provider's sandbox with higher rate limits, before trying ${nxt?.name ?? 'higher'}.`);
    else if (worst.label === 'CPU') advice.push(`${worst.scope} runs out of CPU — scale it out (more instances) or profile the hot endpoint, then retry.`);
    else if (/postgres|db/i.test(worst.scope)) advice.push(`${worst.scope} slows down — check the slow queries in Time spent and the connection pool size.`);
    else advice.push(`Fix ${worst.scope} first, then rerun from ${cur.name}.`);
    advice.push(badNow.length ? `Stay at ${levels[Math.max(0, level - 1)].name} until it recovers.` : `${cur.name} (${cur.target}) is the highest safe level right now — treat it as today's capacity.`);
  }
  return { verdict: badNow.length ? 'back' : badNext.length ? 'hold' : 'go', checks, reasons, advice };
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
  if (isApi) {
    // the api derives target / steps / peak / duration / regions from the spec
    const body = { name: d.name, source: d.source, spec: { ...spec, target: d.target, regions: d.regions } };
    const req = existing && id
      ? api(`/loadtests/scenarios/${encodeURIComponent(serverId(sid))}`, { method: 'PUT', body: JSON.stringify(body) })
      : api<{ id: string }>('/loadtests/scenarios', { method: 'POST', body: JSON.stringify(body) }).then((r) => linkIds(sid, r.id));
    const done = req.then(refresh).catch((e: unknown) => console.warn('[data] save scenario', e)).finally(() => saving.delete(sid));
    saving.set(sid, done);
  }
  return sid;
}

// ---------- curves ----------

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

/** Scale `a` so its max equals `peak`. */
const toPeak = (a: number[], peak: number) => { const m = Math.max(...a) || 1; return a.map((v) => (v / m) * peak); };

export interface RunCurves {
  ts: number[]; vus: number[]; rps: number[]; p95: number[]; err: number[];
  /** fraction of peak load at each point, used for target-service curves */
  load: number[];
  upto: number; step: number;
}

export function runCurves(run: LoadTestRun): RunCurves {
  if (hasControl(run)) return controlledCurves(run);
  const sc = scenarioOf(run);
  const stages = SPEC[sc.id].stages;
  const step = (sc.duration * 1000) / POINTS;
  const upto = Math.max(2, Math.round(POINTS * run.progress));
  const idx = Array.from({ length: upto }, (_, i) => i);
  const load = idx.map((i) => vusAt(stages, i / (POINTS - 1)) / (sc.peakVus || 1));
  const n = (key: string, noise: number) => series(`lt-${key}-${run.id}`, '1h', { base: 1, noise, daily: 0 }).slice(0, upto);
  const rpsN = n('rps', 0.04);
  const p95N = n('p95', 0.08);
  const errN = n('err', 0.35);
  return {
    ts: idx.map((i) => run.startedAt + i * step),
    vus: load.map((l) => Math.round(l * run.vus)),
    rps: toPeak(load.map((l, i) => l * rpsN[i]), run.rps * Math.min(1, Math.max(...load) || 1)),
    // latency and errors climb with load; the overall run number is reached at peak load
    p95: load.map((l, i) => run.p95 * (0.45 + 0.55 * l ** 1.5) * p95N[i]),
    err: load.map((l, i) => run.errorRate * (0.2 + 0.8 * l ** 2) * errN[i]),
    load,
    upto, step,
  };
}

/** Curves for a run driven by the level engine: load follows what was actually applied, not the plan. */
function controlledCurves(run: LoadTestRun): RunCurves {
  const c = controls[run.id];
  const levels = levelsOfRun(run);
  const peak = peakOf(levels);
  const end = c.endedAt ?? Date.now();
  const pts = [...c.points, { t: end, v: targetAt(run, c, end) }].sort((a, b) => a.t - b.t);
  const upto = Math.max(2, Math.min(POINTS, Math.ceil((end - run.startedAt) / 8000)));
  const step = Math.max(1, end - run.startedAt) / (upto - 1);
  const at = (t: number) => {
    const k = pts.findIndex((p) => p.t >= t);
    if (k <= 0) return pts[0].v;
    const a = pts[k - 1], b = pts[k];
    return b.t === a.t ? b.v : a.v + ((b.v - a.v) * (t - a.t)) / (b.t - a.t);
  };
  const idx = Array.from({ length: upto }, (_, i) => i);
  const ts = idx.map((i) => run.startedAt + i * step);
  const load = ts.map((t) => at(t) / peak);
  const n = (key: string, noise: number) => series(`lt-${key}-${run.id}`, '1h', { base: 1, noise, daily: 0 }).slice(0, upto);
  const rpsN = n('rps', 0.04), p95N = n('p95', 0.08), errN = n('err', 0.35);
  const mode = SPEC[run.scenarioId].mode ?? 'vus';
  return {
    ts,
    vus: load.map((l) => Math.round(l * run.vus)),
    rps: load.map((l, i) => l * run.rps * rpsN[i] * (mode === 'rps' ? 1 : 1)),
    p95: load.map((l, i) => run.p95 * (0.45 + 0.55 * l ** 1.5) * p95N[i]),
    err: load.map((l, i) => run.errorRate * (0.2 + 0.8 * l ** 2) * errN[i]),
    load, upto, step,
  };
}

export interface RunEndpoint extends Endpoint { requests: number; runP95: number; runErr: number; p95Series: number[]; errSeries: number[] }

/**
 * Endpoint results for a run. Request counts split the run's traffic by each endpoint's
 * production share; latency and errors are scaled so their request-weighted means match the run.
 */
export function runEndpoints(run: LoadTestRun, c: RunCurves): RunEndpoint[] {
  const eps = scenarioEndpoints(scenarioOf(run));
  const totalReq = c.rps.reduce((a, v) => a + v, 0) * (c.step / 1000);
  const rpmSum = eps.reduce((a, e) => a + e.rpm, 0) || 1;
  const w = SPEC[run.scenarioId]?.weights;
  const share = (e: Endpoint) => w?.[`${e.method} ${e.route}`] ?? e.rpm / rpmSum;
  const kP95 = (run.p95 * 0.8) / (eps.reduce((a, e) => a + e.p95 * share(e), 0) || 1);
  const errW = eps.reduce((a, e) => a + Math.max(e.errorRate, 0.0002) * share(e), 0) || 1;
  const kErr = run.errorRate / errW;
  return eps.map((e) => {
    // each run hits endpoints a little differently (deploys, cache state) — deterministic per run × endpoint
    const h = [...`${run.id}${e.id}`].reduce((a, ch) => (a * 31 + ch.charCodeAt(0)) >>> 0, 7);
    const runP95 = e.p95 * kP95 * (0.85 + (h % 31) / 100);
    const runErr = Math.max(e.errorRate, 0.0002) * kErr * (0.8 + ((h >> 5) % 41) / 100);
    return {
      ...e, runP95, runErr,
      requests: Math.round(totalReq * share(e)),
      p95Series: c.p95.map((v, i) => v * (runP95 / run.p95) * (0.94 + ((i * 7 + e.id.length) % 9) / 70)),
      errSeries: c.err.map((v) => v * (runErr / (run.errorRate || 1))),
    };
  }).sort((a, b) => b.runP95 - a.runP95);
}

/** First database dependency of a service, for the "target services" panel. */
export const dbOf = (serviceId: string) => dependenciesFor(serviceId).find((d) => d.kind === 'db');
