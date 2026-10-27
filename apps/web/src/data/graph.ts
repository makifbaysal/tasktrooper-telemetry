/**
 * Call-graph snapshots for root cause analysis, built from the same mock topology the
 * screens use (endpoint fan-out → dependency → downstream service endpoint).
 * Server-side these come from span aggregates (see .ai/alerting.md § Kök neden analizi).
 *
 * Values per node: errors → failure ratio, latency → avg ms per request.
 * Baselines are derived bottom-up: a node's own part stays the same unless OWN_DELTA says
 * it changed (e.g. a bad deploy), so a node only "moved" if something below it moved.
 */
import {
  DEGRADED_SINCE, NOW, breakdownSeries, deploysFor, depErrorSeries, depLatencySeries, endpointCallers, mockApm, outgoingSeries, redSeries, services,
  endpointCalls as liveCalls, endpointsFor as liveEndpoints,
} from './mock';
import { isApi } from './remote';
import type { Dependency, Endpoint, RcaEvidence, RcaNode, TimeRange } from './types';

const { endpointBreakdown, endpointCalls, endpointsFor } = mockApm;

export type RcaMetric = 'errors' | 'latency';

/** Dependencies that are another instrumented service → which endpoint they hit. */
const DEP_TARGET: Record<string, { serviceId: string; endpointId: string }> = {
  'orders-api/payments-api': { serviceId: 'payments-api', endpointId: 'post-v1-refunds' },
  'storefront-bff/reco-api': { serviceId: 'recommendation-api', endpointId: 'get-v1-recommendations' },
  'storefront-bff/catalog-api': { serviceId: 'catalog-api', endpointId: 'get-v1-items' },
};

/** Leaf dependencies whose normal behaviour differs from now. */
const DEP_BASELINE: Record<string, Partial<Record<RcaMetric, number>>> = {
  'payments-api/iyzico': { errors: 0.003, latency: 180 },
  'recommendation-api/reco-db': { errors: 0.0002, latency: 3 },
};

/** Endpoints whose OWN code changed (not explained by any dependency). */
const OWN_DELTA: Record<string, Partial<Record<RcaMetric, number>>> = {
  'orders-api/post-v1-orders': { errors: 0.0062 },
};

const H = 3600_000;
const EVIDENCE: Record<string, RcaEvidence[]> = {
  'recommendation-api/reco-db': [
    { kind: 'migration', title: 'Migration 0042_rebuild_user_affinity ran', detail: 'Dropped idx_user_affinity_user_score and recreated the table; the index was not restored.', at: DEGRADED_SINCE - 60_000, link: '/code' },
    { kind: 'deploy', title: 'recommendation-api v0.14.0 deployed', detail: 'Ships migration 0042', at: DEGRADED_SINCE - 2 * 60_000, link: '/services/recommendation-api?tab=deploys' },
    { kind: 'query', title: 'SELECT item_id, score FROM user_affinity WHERE user_id = $1 ORDER BY score DESC LIMIT 50', detail: '3 ms → 190 ms avg · rows examined 50 → 1.2M (sequential scan) · 4 calls per request' },
    { kind: 'resource', title: 'reco-db CPU 22% → 91% · connections 18 → 96 of 100', detail: 'Connection pool close to saturation; new queries queue for a free connection' },
  ],
  'payments-api/iyzico': [
    { kind: 'external', title: 'iyzico API timing out', detail: 'p95 1.4 s, 2.8% of calls hit the 5 s client timeout (normally 0.3%). No change on our side in this window.' },
    { kind: 'log', title: 'charge failed: provider timeout after 5000ms provider=iyzico', detail: '≈ 80 / min in payments-api logs', link: '/services/payments-api?tab=logs' },
  ],
  'orders-api/post-v1-orders': [
    { kind: 'deploy', title: 'orders-api v2.41.0 deployed — feat: split shipment builder', at: NOW - 2 * H, link: '/services/orders-api?tab=deploys' },
    { kind: 'log', title: "TypeError: Cannot read properties of undefined (reading 'address') at buildShipment (shipment.ts:88)", detail: 'New since v2.41.0 · ≈ 13 / min', link: '/services/orders-api?tab=logs' },
  ],
};

const svcName = (id: string) => services.find((s) => s.id === id)?.name ?? id;

function epValue(ep: Endpoint, m: RcaMetric) {
  return m === 'errors' ? ep.errorRate : endpointBreakdown(ep).avgMs;
}
function depValue(d: Dependency, m: RcaMetric) {
  return m === 'errors' ? d.errorRate : d.avgMs;
}

export function endpointNode(serviceId: string, endpointId: string, m: RcaMetric, seen = new Set<string>()): RcaNode | undefined {
  if (isApi) return apiEndpointNode(serviceId, endpointId, m, seen);
  const ep = endpointsFor(serviceId).find((e) => e.id === endpointId);
  if (!ep) return undefined;
  const key = `${serviceId}/${endpointId}`;
  seen.add(key);

  const children = endpointCalls(ep).map((c) => {
    const target = DEP_TARGET[`${serviceId}/${c.dep.id}`];
    const sub = target && !seen.has(`${target.serviceId}/${target.endpointId}`)
      ? endpointNode(target.serviceId, target.endpointId, m, seen)
      : undefined;
    const node: RcaNode = sub ?? depNode(serviceId, c.dep, m);
    // downstream HTTP calls of one request run concurrently; DB/cache calls run one after another
    return { node, callsPerReq: c.callsPerReq, parallel: c.dep.kind === 'http' && m === 'latency' };
  });

  const current = epValue(ep, m);
  const below = (pick: 'current' | 'baseline') => sumChildren(children, pick, m);
  const ownNow = Math.max(0, current - below('current'));
  const ownBefore = Math.max(0, ownNow - (OWN_DELTA[key]?.[m] ?? 0));
  return {
    id: `ep:${key}`, kind: 'endpoint', label: `${ep.method} ${ep.route}`, service: svcName(serviceId),
    link: `/services/${serviceId}/endpoints/${endpointId}`,
    current, baseline: ownBefore + below('baseline'),
    children, ownEvidence: EVIDENCE[key],
  };
}

function depNode(serviceId: string, d: Dependency, m: RcaMetric): RcaNode {
  const key = `${serviceId}/${d.id}`;
  const current = depValue(d, m);
  const isExternal = d.kind === 'http';
  return {
    id: `dep:${key}`, kind: isExternal ? 'external' : d.kind, label: d.name, service: svcName(serviceId),
    link: `/services/${serviceId}?tab=traffic`,
    current, baseline: DEP_BASELINE[key]?.[m] ?? current,
    evidence: EVIDENCE[key],
  };
}

/** Errors add up across calls; latency adds up for serial calls and takes the slowest parallel one. */
function sumChildren(children: { node: RcaNode; callsPerReq: number; parallel?: boolean }[], pick: 'current' | 'baseline', m: RcaMetric) {
  const serial = children.filter((c) => !c.parallel).reduce((a, c) => a + c.callsPerReq * c.node[pick], 0);
  const par = children.filter((c) => c.parallel).map((c) => c.callsPerReq * c.node[pick]);
  return serial + (m === 'latency' ? Math.max(0, ...par) : par.reduce((a, v) => a + v, 0));
}

/** Service-level tree: each endpoint weighted by its share of the service's traffic. */
export function serviceNode(serviceId: string, m: RcaMetric): RcaNode | undefined {
  const eps = isApi ? liveEndpoints(serviceId) : endpointsFor(serviceId);
  if (!eps.length) return undefined;
  const total = eps.reduce((a, e) => a + e.rpm, 0);
  const children = eps
    .map((e) => ({ node: endpointNode(serviceId, e.id, m)!, callsPerReq: e.rpm / (total || 1) }))
    .filter((c) => c.node);
  const sum = (pick: 'current' | 'baseline') => children.reduce((a, c) => a + c.callsPerReq * c.node[pick], 0);
  return {
    id: `svc:${serviceId}`, kind: 'endpoint', label: svcName(serviceId), service: svcName(serviceId), link: `/services/${serviceId}`,
    current: sum('current'), baseline: sum('baseline'), children,
  };
}

// =====================================================================================================
// Api mode (ADR-013): the same tree from real span aggregates. There is no RCA endpoint, so the tree is built
// from /endpoints/{e}/calls (fan-out), endpoint + dependency series (now vs. before) and endpoint callers
// (which downstream endpoint an HTTP dependency lands on). "current" = the last ~10 minutes of the hour,
// "baseline" = the rest of the hour; while series load both equal the summary value (nothing "moved").
// =====================================================================================================
const RCA_RANGE: TimeRange = '1h';

/** Weighted average of the last `k` buckets vs. the ones before. */
function window(values: number[], weights: number[]): { current: number; baseline: number } | undefined {
  const n = Math.min(values.length, weights.length);
  if (!n) return undefined;
  const k = Math.max(5, Math.round(n / 6));
  const avg = (from: number, to: number) => {
    let s = 0, w = 0;
    for (let i = Math.max(0, from); i < to; i++) { s += values[i] * weights[i]; w += weights[i]; }
    return w > 0 ? s / w : undefined;
  };
  const current = avg(n - k, n);
  if (current === undefined) return undefined;
  return { current, baseline: avg(0, n - k) ?? current };
}

function epWindow(ep: Endpoint, m: RcaMetric) {
  const red = redSeries(RCA_RANGE, ep.serviceId, ep.id);
  if (m === 'errors') return window(red.errorRate, red.rpm) ?? { current: ep.errorRate, baseline: ep.errorRate };
  const b = breakdownSeries(`${ep.serviceId}-${ep.id}`, { avgMs: 0, parts: { app: 0, db: 0, cache: 0, http: 0, queue: 0, ai: 0 } }, RCA_RANGE, false);
  const avg = red.rpm.map((_, i) => b.app[i] + b.db[i] + b.cache[i] + b.http[i] + b.queue[i]);
  const fallback = ep.p50 || ep.p95 / 2;
  return window(avg, red.rpm.map((r, i) => (avg[i] > 0 ? r : 0))) ?? { current: fallback, baseline: fallback };
}

function depWindow(d: Dependency, m: RcaMetric) {
  const calls = outgoingSeries(d, RCA_RANGE);
  const v = m === 'errors' ? depErrorSeries(d, RCA_RANGE) : depLatencySeries(d, RCA_RANGE, 'avg');
  const now = m === 'errors' ? d.errorRate : d.avgMs;
  return window(v, calls) ?? { current: now, baseline: now };
}

/** The downstream endpoint an HTTP dependency hits: the target service's endpoint this service calls most. */
function downstream(serviceId: string, d: Dependency): { serviceId: string; endpointId: string } | undefined {
  if (d.kind !== 'http') return undefined;
  const target = services.find((s) => s.id !== serviceId && s.type !== 'web' && s.type !== 'mobile' && (d.name.endsWith(` · ${s.name}`) || d.id === s.id));
  if (!target) return undefined;
  const me = svcName(serviceId);
  const best = liveEndpoints(target.id)
    .map((e) => ({ e, rpm: endpointCallers(e).find((c) => c.caller.id === me || c.caller.name === me)?.rpm ?? 0 }))
    .sort((a, b) => b.rpm - a.rpm)[0];
  return best && best.rpm > 0 ? { serviceId: target.id, endpointId: best.e.id } : undefined;
}

function deployEvidence(serviceId: string): RcaEvidence[] | undefined {
  const ev = deploysFor(serviceId, '24h').map((d): RcaEvidence => ({
    kind: 'deploy', title: `${d.service} ${d.version} deployed`, detail: d.env, at: d.ts, link: `/services/${serviceId}?tab=deploys`,
  }));
  return ev.length ? ev : undefined;
}

function apiEndpointNode(serviceId: string, endpointId: string, m: RcaMetric, seen: Set<string>): RcaNode | undefined {
  const ep = liveEndpoints(serviceId).find((e) => e.id === endpointId);
  if (!ep) return undefined;
  const key = `${serviceId}/${endpointId}`;
  seen.add(key);
  const children = liveCalls(ep).map((c) => {
    const target = downstream(serviceId, c.dep);
    const sub = target && !seen.has(`${target.serviceId}/${target.endpointId}`) ? apiEndpointNode(target.serviceId, target.endpointId, m, seen) : undefined;
    const node: RcaNode = sub ?? apiDepNode(serviceId, c.dep, m);
    return { node, callsPerReq: c.callsPerReq, parallel: c.dep.kind === 'http' && m === 'latency' };
  });
  const w = epWindow(ep, m);
  return {
    id: `ep:${key}`, kind: 'endpoint', label: ep.sub ? ep.sub.process : `${ep.method} ${ep.route}`, service: svcName(serviceId),
    link: `/services/${serviceId}/endpoints/${endpointId}`,
    current: w.current, baseline: w.baseline, children, ownEvidence: deployEvidence(serviceId),
  };
}

function apiDepNode(serviceId: string, d: Dependency, m: RcaMetric): RcaNode {
  const w = depWindow(d, m);
  return {
    id: `dep:${serviceId}/${d.id}`, kind: d.kind === 'http' ? 'external' : d.kind, label: d.name, service: svcName(serviceId),
    link: `/services/${serviceId}?tab=traffic`, current: w.current, baseline: w.baseline,
  };
}
