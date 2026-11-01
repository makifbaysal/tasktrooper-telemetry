/**
 * Call-graph snapshots for root cause analysis, built from real span aggregates (ADR-013): /endpoints/{e}/calls
 * (fan-out), endpoint + dependency series (now vs. before) and endpoint callers (which downstream endpoint an HTTP
 * dependency lands on). The alert engine runs the same analysis server-side (see .ai/alerting.md § Root cause analysis).
 *
 * Values per node: errors → failure ratio, latency → avg ms per request. "current" = the last ~10 minutes of the hour,
 * "baseline" = the rest of the hour; while series load both equal the summary value (nothing "moved").
 */
import {
  breakdownSeries, deploysFor, depErrorSeries, depLatencySeries, endpointCallers, outgoingSeries, redSeries, services,
  endpointCalls as liveCalls, endpointsFor as liveEndpoints,
} from './apm';
import type { Dependency, Endpoint, RcaEvidence, RcaNode, TimeRange } from './types';

export type RcaMetric = 'errors' | 'latency';

const svcName = (id: string) => services.find((s) => s.id === id)?.name ?? id;

export function endpointNode(serviceId: string, endpointId: string, m: RcaMetric, seen = new Set<string>()): RcaNode | undefined {
  return apiEndpointNode(serviceId, endpointId, m, seen);
}

/** Service-level tree: each endpoint weighted by its share of the service's traffic. */
export function serviceNode(serviceId: string, m: RcaMetric): RcaNode | undefined {
  const eps = liveEndpoints(serviceId);
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
