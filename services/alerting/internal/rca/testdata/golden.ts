import { findRootCause } from './rca.ts';
const n = (id: string, kind: string, label: string, baseline: number, current: number, service?: string, children?: any[]) => ({ id, kind, label, baseline, current, service, children });
const trees: [string, any, 'ms' | 'ratio'][] = [
  ['db deep', n('home', 'page', 'Homepage', 1200, 2600, 'shop-web', [
    { callsPerReq: 1, node: n('api', 'endpoint', 'GET /v1/home', 300, 1700, 'orders-api', [
      { callsPerReq: 1, node: n('reco', 'http', 'HTTP · recommendation-api', 100, 1450, 'recommendation-api', [
        { callsPerReq: 1, node: n('reco-ep', 'endpoint', 'GET /reco', 90, 1440, 'recommendation-api', [
          { callsPerReq: 2, node: n('db', 'db', 'postgres · reco-db', 10, 680, 'recommendation-api') },
          { callsPerReq: 1, node: n('cache', 'cache', 'redis · reco-cache', 2, 3, 'recommendation-api') },
        ]) },
      ]) },
      { callsPerReq: 3, node: n('odb', 'db', 'postgres · orders-db', 20, 22, 'orders-api') },
    ]) },
  ]), 'ms'],
  ['own code', n('ep', 'endpoint', 'POST /checkout', 200, 500, 'orders-api', [
    { callsPerReq: 1, node: n('db', 'db', 'postgres · orders-db', 50, 60, 'orders-api') },
  ]), 'ms'],
  ['parallel', n('ep', 'endpoint', 'GET /feed', 100, 400, 'feed-api', [
    { callsPerReq: 1, parallel: true, node: n('a', 'external', 'HTTP · ads', 50, 380, 'feed-api') },
    { callsPerReq: 1, parallel: true, node: n('b', 'external', 'HTTP · news', 60, 70, 'feed-api') },
    { callsPerReq: 1, node: n('c', 'cache', 'redis · feed', 5, 6, 'feed-api') },
  ]), 'ms'],
  ['errors with alternatives', n('ep', 'endpoint', 'POST /pay', 0.01, 0.2, 'payments-api', [
    { callsPerReq: 1, node: n('iyz', 'external', 'HTTP · iyzico', 0.0, 0.12, 'payments-api') },
    { callsPerReq: 1, node: n('db', 'db', 'postgres · pay-db', 0.0, 0.05, 'payments-api') },
    { callsPerReq: 1, node: n('q', 'queue', 'kafka · pay.events', 0.0, 0.005, 'payments-api') },
  ]), 'ratio'],
  ['no change', n('ep', 'endpoint', 'GET /x', 100, 90, 'x', [{ callsPerReq: 1, node: n('d', 'db', 'pg · x', 1, 2, 'x') }]), 'ms'],
];
const out = trees.map(([name, t, u]) => {
  const r = findRootCause(t, u);
  return { name, summary: r.summary, confidence: r.confidence, root: r.root.id, path: r.path.map((h) => ({ id: h.node.id, explains: h.explains, contribution: h.contribution, alts: h.alternatives.map((a) => a.node.id) })) };
});
console.log(JSON.stringify({ trees: trees.map(([name, t, u]) => ({ name, unit: u, tree: t })), out }));
