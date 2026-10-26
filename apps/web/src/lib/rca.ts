import type { RcaHop, RcaNode, RcaResult } from '@/data/types';

/**
 * Root cause drill-down over a KPI's call tree. Same algorithm the alert engine runs
 * server-side (see .ai/alerting.md § Kök neden analizi):
 *
 * 1. At each node, the change to explain is `current - baseline`.
 * 2. Each child contributes `callsPerReq × (child.current - child.baseline)`.
 *    Parallel siblings only add their slowest member (critical path), not the sum.
 * 3. Whatever the children don't explain is the node's own code.
 * 4. Follow the candidate that explains the most; stop when own code wins,
 *    when the node is a leaf (DB, cache, external API) or nothing moved.
 *
 * Shares are normalised over positive contributions, so retries/fan-out that
 * over-explain the parent still produce shares in 0..1.
 */
const MIN_SHARE = 0.15;

export function findRootCause(root: RcaNode, unit: 'ms' | 'ratio' = 'ms'): RcaResult {
  const path: RcaHop[] = [];
  let node = root;
  let confidence = 1;

  for (let depth = 0; depth < 12; depth++) {
    const children = node.children ?? [];
    if (children.length === 0 || node.current - node.baseline <= 0) break;

    const candidates = contributions(node);
    const positive = candidates.filter((c) => c.contribution > 0);
    const total = positive.reduce((a, c) => a + c.contribution, 0);
    if (total <= 0) break;
    const ranked = positive
      .map((c) => ({ ...c, explains: c.contribution / total }))
      .sort((a, b) => b.explains - a.explains);
    const best = ranked[0];
    const alternatives = ranked.slice(1).filter((c) => c.explains >= MIN_SHARE).map((c) => ({ node: c.node, explains: c.explains }));

    path.push({ node: best.node, explains: best.explains, contribution: best.contribution, callsPerReq: best.callsPerReq, alternatives });
    confidence *= best.explains;
    if (best.node.kind === 'code') break;
    node = best.node;
  }

  const last = path[path.length - 1]?.node ?? root;
  return { unit, path, confidence, root: last, summary: summarize(root, last, unit) };
}

function contributions(node: RcaNode) {
  const children = node.children ?? [];
  const serial = children.filter((c) => !c.parallel);
  const parallel = children.filter((c) => c.parallel);
  const delta = (c: { node: RcaNode; callsPerReq: number }) => c.callsPerReq * (c.node.current - c.node.baseline);

  const out = serial.map((c) => ({ node: c.node, callsPerReq: c.callsPerReq, contribution: delta(c) }));
  if (parallel.length) {
    // only the slowest parallel branch is on the critical path
    const before = Math.max(...parallel.map((c) => c.callsPerReq * c.node.baseline));
    const after = Math.max(...parallel.map((c) => c.callsPerReq * c.node.current));
    const critical = parallel.reduce((a, c) => (c.callsPerReq * c.node.current > a.callsPerReq * a.node.current ? c : a));
    out.push({ node: critical.node, callsPerReq: critical.callsPerReq, contribution: after - before });
  }
  const explained = out.reduce((a, c) => a + c.contribution, 0);
  const own = node.current - node.baseline - explained;
  if (own > 0) {
    out.push({
      node: { id: `${node.id}:code`, kind: 'code', label: `${node.service ?? node.label} own code`, service: node.service, link: node.link, baseline: 0, current: own, evidence: node.ownEvidence },
      callsPerReq: 1,
      contribution: own,
    });
  }
  return out;
}

const short = (n: RcaNode) => n.label.replace(/^.* · /, '');
function noun(n: RcaNode) {
  switch (n.kind) {
    case 'db': return `database ${short(n)}`;
    case 'cache': return `cache ${short(n)}`;
    case 'queue': return `queue ${short(n)}`;
    case 'external': return `external dependency ${short(n)}`;
    case 'endpoint': return `endpoint ${n.label}`;
    default: return short(n);
  }
}

function summarize(root: RcaNode, cause: RcaNode, unit: 'ms' | 'ratio') {
  if (cause === root) return `${root.label} changed, but no single dependency explains it.`;
  const svc = cause.service ?? 'The service';
  if (cause.kind === 'code') {
    return unit === 'ratio'
      ? `${svc}'s own code started failing — none of the services or databases it calls explain it.`
      : `${svc}'s own code got slower — none of the services or databases it calls explain it.`;
  }
  const owner = cause.service ? `${cause.service}'s ` : '';
  if (unit === 'ratio') {
    return `Failures come from ${owner}${noun(cause)} (${(cause.baseline * 100).toFixed(1)}% → ${(cause.current * 100).toFixed(1)}% of calls failing).`;
  }
  const x = cause.baseline > 0 ? cause.current / cause.baseline : 0;
  const factor = x >= 2 ? `${x >= 10 ? Math.round(x) : x.toFixed(1)}× slower` : 'slower';
  return `Caused by ${owner}${noun(cause)} — ${factor} than usual.`;
}
