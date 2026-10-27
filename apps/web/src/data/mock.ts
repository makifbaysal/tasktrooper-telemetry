/**
 * Deterministic mock data. Screens read only through `src/data/index.ts`;
 * when the API exists those functions switch to fetch and screens stay unchanged.
 */
import type {
  Subscription,
  Dependency, Endpoint, EndpointCall, Incident, InventoryItem, LoadTestRun, LoadTestScenario, LogLine, Monitor,
  Caller, CallerKind, CallerTraffic, RumPage, Repo, Service, Session, Suggestion, TimeBreakdown, TimeCategory, TimeRange,
} from './types';
import { hydrateList, isApi, registerHydrator, remote, seg } from './remote';
import * as A from './api/apm';

// ---------- seeded random ----------
function mulberry32(seed: number) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}

export const RANGE_POINTS: Record<TimeRange, { points: number; stepMs: number }> = {
  '15m': { points: 15, stepMs: 60_000 },
  '30m': { points: 30, stepMs: 60_000 },
  '1h': { points: 60, stepMs: 60_000 },
  '6h': { points: 72, stepMs: 5 * 60_000 },
  '24h': { points: 96, stepMs: 15 * 60_000 },
  '7d': { points: 84, stepMs: 2 * 3600_000 },
};

export const NOW = Math.floor(Date.now() / 60_000) * 60_000;

function mockTimestamps(range: TimeRange): number[] {
  const { points, stepMs } = RANGE_POINTS[range];
  return Array.from({ length: points }, (_, i) => NOW - (points - 1 - i) * stepMs);
}

interface SeriesOpts {
  base: number;
  noise?: number;      // relative noise 0..1
  daily?: number;      // relative daily swing amplitude
  spikeAt?: number;    // 0..1 position of an incident spike
  spikeMul?: number;
  spikeWidth?: number; // points
  /** ongoing degradation: from this timestamp on the value is `base`, before it `base × stepFrom` */
  stepAt?: number;
  stepFrom?: number;
  min?: number;
}

export function series(key: string, range: TimeRange, o: SeriesOpts): number[] {
  const rnd = mulberry32(hash(key + range));
  const ts = timestamps(range);
  const n = ts.length;
  const spikeIdx = o.spikeAt !== undefined ? Math.round(o.spikeAt * (n - 1)) : -1;
  const w = o.spikeWidth ?? 4;
  let drift = 0;
  return ts.map((t, i) => {
    const hour = new Date(t).getHours() + new Date(t).getMinutes() / 60;
    const daily = 1 + (o.daily ?? 0.25) * Math.sin(((hour - 9) / 24) * Math.PI * 2);
    drift = drift * 0.8 + (rnd() - 0.5) * (o.noise ?? 0.08);
    let v = o.base * daily * (1 + drift);
    if (spikeIdx >= 0 && Math.abs(i - spikeIdx) <= w) {
      const k = 1 - Math.abs(i - spikeIdx) / (w + 1);
      v *= 1 + ((o.spikeMul ?? 3) - 1) * k;
    }
    if (o.stepAt !== undefined && t < o.stepAt) {
      // short ramp in the last few minutes before the step
      const ramp = Math.max(0, 1 - (o.stepAt - t) / (3 * 60_000));
      v *= (o.stepFrom ?? 1) + (1 - (o.stepFrom ?? 1)) * ramp;
    }
    return Math.max(o.min ?? 0, v);
  });
}

// ---------- entities ----------
export const services: Service[] = [
  { id: 'orders-api', name: 'orders-api', type: 'http', language: 'Node.js', version: 'v2.41.0', rpm: 18420, errorRate: 0.0042, p95: 182, prevRpm: 17100, prevErrorRate: 0.0031, prevP95: 171, status: 'warning', instances: 12 },
  { id: 'payments-api', name: 'payments-api', type: 'http', language: 'Go', version: 'v1.18.3', rpm: 6390, errorRate: 0.0213, p95: 640, prevRpm: 6580, prevErrorRate: 0.0040, prevP95: 410, status: 'critical', instances: 6 },
  { id: 'catalog-api', name: 'catalog-api', type: 'http', language: 'Go', version: 'v3.2.0', rpm: 42800, errorRate: 0.0006, p95: 48, prevRpm: 40100, prevErrorRate: 0.0007, prevP95: 51, status: 'ok', instances: 18 },
  { id: 'auth-api', name: 'auth-api', type: 'http', language: 'Node.js', version: 'v5.0.2', rpm: 9100, errorRate: 0.0019, p95: 96, prevRpm: 9300, prevErrorRate: 0.0020, prevP95: 94, status: 'ok', instances: 4, source: 'ebpf' },
  { id: 'order-events', name: 'order-events-consumer', type: 'consumer', language: 'Node.js', version: 'v2.41.0', rpm: 15200, errorRate: 0.0011, p95: 34, prevRpm: 14800, prevErrorRate: 0.0010, prevP95: 33, status: 'warning', instances: 8, lag: 18400 },
  { id: 'email-worker', name: 'email-worker', type: 'worker', language: 'Python', version: 'v0.9.12', rpm: 820, errorRate: 0.0090, p95: 1240, prevRpm: 790, prevErrorRate: 0.0085, prevP95: 1180, status: 'ok', instances: 2, lag: 120 },
  { id: 'shop-web', name: 'shop-web', type: 'web', language: 'React', version: '2026.10.24', rpm: 3900, errorRate: 0.0034, p95: 2140, prevRpm: 3600, prevErrorRate: 0.0031, prevP95: 2300, status: 'ok', instances: 0 },
  { id: 'shop-mobile', name: 'shop-mobile', type: 'mobile', language: 'React Native', version: '4.12.0', rpm: 2100, errorRate: 0.0021, p95: 1650, prevRpm: 2050, prevErrorRate: 0.0024, prevP95: 1720, status: 'ok', instances: 0 },
  // homepage chain: shop-web "/" → storefront-bff → recommendation-api → reco-db (degraded since a migration)
  { id: 'storefront-bff', name: 'storefront-bff', type: 'http', language: 'Node.js', version: 'v1.9.4', rpm: 7000, errorRate: 0.0015, p95: 940, prevRpm: 6800, prevErrorRate: 0.0014, prevP95: 260, status: 'warning', instances: 6 },
  { id: 'recommendation-api', name: 'recommendation-api', type: 'http', language: 'Python', version: 'v0.14.0', rpm: 11500, errorRate: 0.0024, p95: 1380, prevRpm: 11300, prevErrorRate: 0.0011, prevP95: 120, status: 'critical', instances: 4 },
  // more client apps — any number of web / mobile apps can report through the RUM SDKs
  { id: 'backoffice-web', name: 'backoffice-web', type: 'web', language: 'React', version: '2026.10.20', rpm: 310, errorRate: 0.0015, p95: 1980, prevRpm: 290, prevErrorRate: 0.0016, prevP95: 2010, status: 'ok', instances: 0 },
  { id: 'seller-portal', name: 'seller-portal', type: 'web', language: 'Vue', version: '3.8.1', rpm: 640, errorRate: 0.0042, p95: 2620, prevRpm: 610, prevErrorRate: 0.0038, prevP95: 2540, status: 'ok', instances: 0 },
  { id: 'landing-web', name: 'landing-web', type: 'web', language: 'Next.js', version: '2026.10.22', rpm: 1450, errorRate: 0.0006, p95: 1240, prevRpm: 1180, prevErrorRate: 0.0007, prevP95: 1290, status: 'ok', instances: 0 },
  { id: 'courier-app', name: 'courier-app', type: 'mobile', language: 'Flutter', version: '2.3.0', rpm: 380, errorRate: 0.0031, p95: 1420, prevRpm: 360, prevErrorRate: 0.0029, prevP95: 1380, status: 'ok', instances: 0 },
];

/** Start of the ongoing homepage degradation (reco-db lost an index in migration 0042). */
export const DEGRADED_SINCE = NOW - 14 * 60_000;
const DEGRADED = new Set(['storefront-bff', 'recommendation-api']);
/**
 * Shape options for a service's charts: an ongoing step for the degraded chain,
 * a spike for other critical services, nothing otherwise.
 */
function mockIncidentShape(serviceId: string, stepFrom = 0.3): { stepAt?: number; stepFrom?: number; spikeAt?: number; spikeMul?: number; spikeWidth?: number } {
  if (DEGRADED.has(serviceId)) return { stepAt: DEGRADED_SINCE, stepFrom };
  const s = services.find((x) => x.id === serviceId);
  return s?.status === 'critical' ? { spikeAt: 0.8, spikeMul: 2.6, spikeWidth: 6 } : {};
}

const endpointDefs: Record<string, [string, string, number, number, number, boolean, Endpoint['criticality']][]> = {
  'orders-api': [
    ['POST', '/v1/orders', 2100, 0.0081, 240, true, 'critical'],
    ['GET', '/v1/orders/:id', 7400, 0.0012, 62, true, 'important'],
    ['GET', '/v1/orders', 4300, 0.0009, 148, false, 'important'],
    ['POST', '/v1/orders/:id/cancel', 320, 0.0140, 310, false, 'critical'],
    ['GET', '/v1/cart', 3100, 0.0004, 41, false, 'low'],
    ['PUT', '/v1/cart/items/:itemId', 900, 0.0021, 88, false, 'low'],
    ['GET', '/healthz', 300, 0, 2, false, 'low'],
  ],
  'payments-api': [
    ['POST', '/v1/charges', 2900, 0.0310, 820, true, 'critical'],
    ['POST', '/v1/refunds', 320, 0.0070, 690, false, 'critical'],
    ['GET', '/v1/charges/:id', 2800, 0.0010, 55, false, 'important'],
    ['POST', '/v1/webhooks/stripe', 370, 0.0300, 210, false, 'critical'],
  ],
  'storefront-bff': [
    ['GET', '/v1/home', 2400, 0.0021, 2150, false, 'critical'],
    ['GET', '/v1/product/:slug', 3100, 0.0012, 1900, true, 'critical'],
    ['GET', '/v1/search', 1500, 0.0011, 210, false, 'important'],
  ],
  'auth-api': [
    ['POST', '/v1/auth/refresh', 5200, 0.0012, 70, true, 'critical'],
    ['POST', '/v1/auth/login', 1800, 0.0041, 180, true, 'critical'],
    ['GET', '/v1/auth/me', 1900, 0.0006, 24, false, 'important'],
    ['POST', '/v1/auth/logout', 200, 0.0010, 30, false, 'low'],
  ],
  'recommendation-api': [
    ['GET', '/v1/recommendations', 5500, 0.0048, 2040, false, 'important'],
    ['POST', '/v1/events', 6000, 0.0002, 18, false, 'low'],
  ],
};

// consumers & workers: [process, source, topic, group/schedule, partitions, msg/min, errorRate, p95, lag, retries/min, dlq/min, monitored, criticality]
type SubDef = [string, Subscription['source'], string, string, number, number, number, number, number, number, number, boolean, Endpoint['criticality']];
const subDefs: Record<string, SubDef[]> = {
  'order-events': [
    ['order-created-handler', 'kafka', 'orders.created', 'order-events.fulfilment', 12, 6200, 0.0012, 38, 17900, 40, 2, true, 'critical'],
    ['order-search-indexer', 'kafka', 'orders.created', 'order-events.search-index', 12, 6200, 0.0004, 21, 300, 3, 0, false, 'important'],
    ['order-status-projector', 'kafka', 'orders.status-changed', 'order-events.projections', 6, 2400, 0.0008, 17, 150, 1, 0, false, 'important'],
    ['dlq-replayer', 'kafka', 'orders.created.dlq', 'order-events.dlq-replay', 1, 400, 0.031, 64, 50, 0, 0, false, 'critical'],
  ],
  'email-worker': [
    ['email-sender', 'rabbitmq', 'emails.transactional', 'email-sender', 0, 700, 0.0085, 1100, 90, 6, 0.4, false, 'critical'],
    ['newsletter-sender', 'rabbitmq', 'emails.marketing', 'newsletter-sender', 0, 100, 0.012, 2100, 30, 2, 0.1, false, 'low'],
    ['invoice-batch', 'cron', 'send_invoices', '0 2 * * *', 0, 20, 0.0, 4800, 0, 0, 0, true, 'important'],
  ],
};

function mockEndpointsFor(serviceId: string): Endpoint[] {
  const svc = services.find((s) => s.id === serviceId);
  if (svc && (svc.type === 'web' || svc.type === 'mobile')) return [];
  const subs = subDefs[serviceId];
  if (subs) {
    return subs.map(([process, source, topic, groupOrSchedule, partitions, rpm, er, p95, lag, retries, dlq, monitored, criticality]) => ({
      id: `${process}`, serviceId, method: source === 'cron' ? 'CRON' : source.toUpperCase(), route: topic,
      rpm, errorRate: er, p50: Math.round(p95 * 0.42), p95, p99: Math.round(p95 * 2.1), monitored, criticality,
      sub: {
        process, source, topic, lag, retriesPerMin: retries, dlqPerMin: dlq,
        ...(source === 'cron' ? { schedule: groupOrSchedule } : { group: groupOrSchedule || undefined, partitions: partitions || undefined }),
      },
    }));
  }
  const defs = endpointDefs[serviceId] ?? [
    ['GET', '/v1/items', 12000, 0.0005, 40, true, 'important'],
    ['GET', '/v1/items/:id', 20000, 0.0004, 22, false, 'important'],
    ['GET', '/v1/search', 9000, 0.0011, 120, false, 'important'],
    ['POST', '/v1/items', 300, 0.0020, 95, false, 'low'],
  ];
  return defs.map(([method, route, rpm, er, p95, monitored, criticality]) => ({
    id: `${method.toLowerCase()}-${route.replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, '')}`,
    serviceId, method, route, rpm, errorRate: er, p50: Math.round(p95 * 0.42), p95, p99: Math.round(p95 * 2.1),
    monitored, criticality,
  }));
}

export const rumPages: RumPage[] = [
  { id: 'home', path: '/', views: 48200, users: 21400, avgTime: 42, lcp: 2543, inp: 120, cls: 0.04, errorRate: 0.0012, bounce: 0.38 },
  { id: 'product', path: '/p/:slug', views: 61300, users: 18900, avgTime: 74, lcp: 3173, inp: 180, cls: 0.09, errorRate: 0.0021, bounce: 0.22 },
  { id: 'search', path: '/search', views: 29800, users: 11200, avgTime: 51, lcp: 2050, inp: 240, cls: 0.03, errorRate: 0.0018, bounce: 0.17 },
  { id: 'cart', path: '/cart', views: 12400, users: 8100, avgTime: 63, lcp: 1690, inp: 140, cls: 0.02, errorRate: 0.0030, bounce: 0.11 },
  { id: 'checkout', path: '/checkout', views: 7300, users: 5900, avgTime: 128, lcp: 3120, inp: 310, cls: 0.12, errorRate: 0.0094, bounce: 0.08 },
  { id: 'account', path: '/account', views: 5200, users: 4100, avgTime: 39, lcp: 1540, inp: 96, cls: 0.01, errorRate: 0.0006, bounce: 0.30 },
  { id: 'orders', path: '/account/orders/:id', views: 3900, users: 3200, avgTime: 33, lcp: 1710, inp: 110, cls: 0.02, errorRate: 0.0011, bounce: 0.25 },
];

const names = ['ayse.k', 'mehmet.y', 'anon-8f21', 'zeynep.a', 'anon-c03e', 'can.d', 'elif.s', 'anon-1b9a', 'burak.t', 'deniz.o'];
const countries = ['TR', 'DE', 'NL', 'TR', 'GB', 'TR', 'US', 'TR', 'FR', 'TR'];
export const sessions: Session[] = names.map((user, i) => {
  const rnd = mulberry32(i + 7);
  const pages = 2 + Math.floor(rnd() * 12);
  return {
    id: `s_${(hash(user) % 1e8).toString(36)}`,
    user, identified: !user.startsWith('anon'), country: countries[i],
    device: i % 3 === 0 ? 'iPhone 15' : i % 3 === 1 ? 'Chrome · macOS' : 'Android 15',
    startedAt: NOW - Math.round(rnd() * 3600_000), duration: Math.round(60 + rnd() * 1500), pages,
    errors: rnd() > 0.75 ? 1 + Math.floor(rnd() * 3) : 0,
    entry: rumPages[Math.floor(rnd() * 3)].path, exit: rumPages[Math.floor(rnd() * rumPages.length)].path,
  };
});

const logTemplates: [LogLine['level'], string, string][] = [
  ['info', 'orders-api', 'order created order_id={id} total=129.90 currency=TRY'],
  ['info', 'catalog-api', 'GET /v1/items/:id 200 12ms'],
  ['warn', 'orders-api', 'inventory reservation slow duration_ms=840 sku={sku}'],
  ['error', 'payments-api', 'charge failed: provider timeout after 5000ms provider=iyzico order_id={id}'],
  ['error', 'payments-api', 'payment declined code=insufficient_funds order_id={id}'],
  ['info', 'order-events-consumer', 'processed message topic=orders.created partition=3 offset={off}'],
  ['warn', 'order-events-consumer', 'retrying message attempt=2 topic=orders.created reason=lock_timeout'],
  ['info', 'auth-api', 'token refreshed user_id={uid}'],
  ['error', 'email-worker', 'SMTP 421 service not available, will retry'],
  ['debug', 'catalog-api', 'cache hit key=item:{sku}'],
  ['error', 'orders-api', 'TypeError: Cannot read properties of undefined (reading \'address\') at buildShipment (shipment.ts:88)'],
];
export const logs: LogLine[] = Array.from({ length: 120 }, (_, i) => {
  const rnd = mulberry32(i * 31 + 3);
  const weights = rnd();
  const t = logTemplates[weights < 0.55 ? Math.floor(rnd() * 3) * 2 % logTemplates.length : Math.floor(rnd() * logTemplates.length)];
  return {
    id: `l${i}`, ts: NOW - i * 4_700 - Math.round(rnd() * 3000), level: t[0], service: t[1],
    message: t[2]
      .replace('{id}', `ord_${Math.floor(rnd() * 1e6)}`)
      .replace('{sku}', `SKU-${Math.floor(rnd() * 9000 + 1000)}`)
      .replace('{off}', String(Math.floor(8e6 + rnd() * 1e5)))
      .replace('{uid}', `u_${Math.floor(rnd() * 1e5)}`),
    traceId: rnd() > 0.3 ? Math.floor(rnd() * 1e16).toString(16).padStart(16, '0') : undefined,
  };
});

export const monitors: Monitor[] = [
  { id: 'm1', name: 'Charges error rate', kind: 'metric_threshold', target: 'payments-api · POST /v1/charges', condition: 'error rate > 2% for 5m', severity: 'critical', state: 'firing', source: 'code', codeRef: 'shop/payments:.trooper/monitors/charges.yaml', lastChange: NOW - 23 * 60_000, channels: ['#payments-alerts', 'PagerDuty'] },
  { id: 'm2', name: 'Charges p95 latency', kind: 'latency', target: 'payments-api · POST /v1/charges', condition: 'p95 > 600 ms for 10m', severity: 'warning', state: 'firing', source: 'suggestion', lastChange: NOW - 41 * 60_000, channels: ['#payments-alerts'] },
  { id: 'm3', name: 'order-created-handler lag', kind: 'consumer_lag', target: 'order-events-consumer · order-created-handler', condition: 'lag > 10K for 5m', severity: 'warning', state: 'pending', source: 'code', lastChange: NOW - 3 * 60_000, channels: ['#orders-oncall'],
    queue: { clusterId: 'kafka-main', group: 'order-events.fulfilment', topic: 'orders.created', metric: 'lag', threshold: 10_000, forMin: 5 } },
  { id: 'm12', name: 'reco-trainer drains every run', kind: 'consumer_lag', target: 'kafka-main · reco-trainer', condition: 'lag not drained in 2h', severity: 'warning', state: 'ok', source: 'manual', lastChange: NOW - 4 * 86400_000, channels: ['#data-platform'],
    queue: { clusterId: 'kafka-main', group: 'reco-trainer', topic: 'reco.events', metric: 'not_drained', threshold: 2, forMin: 0 } },
  { id: 'm4', name: 'Create order errors', kind: 'metric_threshold', target: 'orders-api · POST /v1/orders', condition: 'error rate > 1% for 5m', severity: 'critical', state: 'ok', source: 'manual', lastChange: NOW - 2 * 86400_000, channels: ['#orders-oncall', 'PagerDuty'] },
  { id: 'm5', name: 'Payment provider timeouts', kind: 'log_match', target: 'payments-api logs', condition: 'count("provider timeout") > 20 in 5m', severity: 'critical', state: 'ok', source: 'suggestion', lastChange: NOW - 26 * 3600_000, channels: ['#payments-alerts'] },
  { id: 'm6', name: 'Checkout LCP', kind: 'rum_vital', target: 'shop-web · /checkout', condition: 'p75 LCP > 2.5 s for 15m', severity: 'warning', state: 'ok', source: 'template', lastChange: NOW - 5 * 3600_000, channels: ['#web-team'] },
  { id: 'm7', name: 'Catalog throughput drop', kind: 'throughput_change', target: 'catalog-api · all endpoints', condition: '−50% vs same hour last week', severity: 'warning', state: 'ok', source: 'manual', lastChange: NOW - 6 * 86400_000, channels: ['#platform'] },
  { id: 'm8', name: 'Nightly invoice job', kind: 'job_missed', target: 'email-worker · invoice-batch', condition: 'no success in 26h', severity: 'warning', state: 'muted', source: 'code', codeRef: 'shop/billing:.trooper/monitors/invoice-batch.yaml', lastChange: NOW - 3 * 86400_000, channels: ['#billing'] },
  { id: 'm9', name: 'Homepage loads fast · fast burn', kind: 'kpi_burn', target: 'KPI · Homepage loads fast', condition: 'budget burns > 6× too fast over 1h and 5m', severity: 'critical', state: 'firing', source: 'code', lastChange: DEGRADED_SINCE + 5 * 60_000, channels: ['#web-team', 'PagerDuty'] },
  { id: 'm10', name: 'Checkout succeeds · fast burn', kind: 'kpi_burn', target: 'KPI · Checkout succeeds', condition: 'budget burns > 3× too fast over 1h and 5m', severity: 'critical', state: 'firing', source: 'code', lastChange: NOW - 18 * 60_000, channels: ['#payments-alerts', 'PagerDuty'] },
  { id: 'm11', name: 'Order placement API succeeds · slow burn', kind: 'kpi_burn', target: 'KPI · Order placement API succeeds', condition: 'budget burns > 1.5× too fast over 6h and 30m', severity: 'warning', state: 'pending', source: 'code', lastChange: NOW - 40 * 60_000, channels: ['#orders-oncall'] },
];

export const incidents: Incident[] = [
  { id: 'i1', monitorId: 'm1', title: 'Charges error rate above 2%', severity: 'critical', state: 'firing', startedAt: NOW - 23 * 60_000, value: '3.1%', service: 'payments-api' },
  { id: 'i2', monitorId: 'm2', title: 'Charges p95 latency above 600 ms', severity: 'warning', state: 'acknowledged', startedAt: NOW - 41 * 60_000, value: '820 ms', service: 'payments-api' },
  { id: 'i3', monitorId: 'm3', title: 'order-created-handler lag above 10K', severity: 'warning', state: 'pending', startedAt: NOW - 3 * 60_000, value: '18.4K', service: 'order-events-consumer' },
  { id: 'i4', monitorId: 'm9', kpiId: 'homepage-fast', title: 'Homepage loads fast — error budget burning 6.4×', severity: 'critical', state: 'firing', startedAt: DEGRADED_SINCE + 5 * 60_000, value: '68% good (objective 95%)', service: 'shop-web' },
  { id: 'i5', monitorId: 'm10', kpiId: 'checkout-success', title: 'Checkout succeeds — error budget burning 3.2×', severity: 'critical', state: 'firing', startedAt: NOW - 18 * 60_000, value: '96.8% good (objective 99%)', service: 'shop-web' },
];

export const suggestions: Suggestion[] = [
  { id: 'sg1', title: 'Cancel order error rate', kind: 'metric_threshold', target: 'orders-api · POST /v1/orders/:id/cancel', condition: 'error rate > 2% for 5m (min 20 req)', criticality: 'critical', confidence: 0.92, rationale: 'Handler refunds the payment and restocks inventory inside one DB transaction; failures leave orders in an inconsistent state. Observed error rate 1.4% (14-day p99: 1.9%).', codeRef: 'src/orders/cancel.controller.ts:42', calibrated: true },
  { id: 'sg2', title: 'Stripe webhook failures', kind: 'metric_threshold', target: 'payments-api · POST /v1/webhooks/stripe', condition: 'error rate > 1% for 5m', criticality: 'critical', confidence: 0.88, rationale: 'Webhook marks charges as captured. Non-2xx responses make Stripe retry for up to 3 days, delaying order confirmation.', codeRef: 'internal/webhooks/stripe.go:71', calibrated: true },
  { id: 'sg3', title: 'Refund latency', kind: 'latency', target: 'payments-api · POST /v1/refunds', condition: 'p95 > 1.2 s for 10m', criticality: 'critical', confidence: 0.74, rationale: 'Calls external provider synchronously with a 5s timeout. Threshold = 14-day p95 × 1.5.', codeRef: 'internal/refunds/handler.go:28', calibrated: true },
  { id: 'sg4', title: 'Order events DLQ backlog', kind: 'consumer_lag', target: 'order-events-consumer · dlq-replayer', condition: 'DLQ messages on orders.created.dlq > 0', criticality: 'important', confidence: 0.81, rationale: 'Consumer publishes to orders.created.dlq after 3 attempts; each DLQ message is an order that never got a shipment.', codeRef: 'src/consumers/order-created.ts:19', calibrated: false },
  { id: 'sg5', title: '"inventory reservation slow" log spike', kind: 'log_match', target: 'orders-api logs', condition: 'count > 50 in 5m', criticality: 'important', confidence: 0.63, rationale: 'logger.warn in reserveStock() fires when reservation exceeds 500 ms; spikes preceded 2 past incidents.', codeRef: 'src/inventory/reserve.ts:57', calibrated: false },
];

export const scenarios: LoadTestScenario[] = [
  { id: 'lt1', name: 'Checkout flow', target: 'https://staging.shop.com', source: 'generated', steps: 4, peakVus: 200, duration: 480, regions: ['eu-central', 'us-east'], lastRun: 'r1' },
  { id: 'lt2', name: 'Catalog browse 3× prod', target: 'https://staging.shop.com', source: 'generated', steps: 3, peakVus: 600, duration: 900, regions: ['eu-central'], lastRun: 'r3' },
  { id: 'lt3', name: 'Auth token refresh soak', target: 'https://auth.staging.shop.com', source: 'manual', steps: 2, peakVus: 80, duration: 3600, regions: ['eu-central'], lastRun: 'r4' },
];

export const runs: LoadTestRun[] = [
  { id: 'r1', scenarioId: 'lt1', state: 'running', startedAt: NOW - 4 * 60_000, progress: 0.52, rps: 412, p95: 388, errorRate: 0.004, vus: 200, thresholds: [{ name: 'http_req_failed rate < 1%', passed: true }, { name: 'http_req_duration p(95) < 500 ms', passed: true }] },
  { id: 'r2', scenarioId: 'lt1', state: 'failed', startedAt: NOW - 2 * 86400_000, progress: 1, rps: 380, p95: 742, errorRate: 0.031, vus: 200, thresholds: [{ name: 'http_req_failed rate < 1%', passed: false }, { name: 'http_req_duration p(95) < 500 ms', passed: false }] },
  { id: 'r3', scenarioId: 'lt2', state: 'passed', startedAt: NOW - 5 * 86400_000, progress: 1, rps: 2240, p95: 61, errorRate: 0.0002, vus: 600, thresholds: [{ name: 'http_req_failed rate < 1%', passed: true }, { name: 'http_req_duration p(95) < 200 ms', passed: true }] },
  { id: 'r4', scenarioId: 'lt3', state: 'passed', startedAt: NOW - 9 * 86400_000, progress: 1, rps: 160, p95: 102, errorRate: 0.0011, vus: 80, thresholds: [{ name: 'http_req_failed rate < 1%', passed: true }] },
  { id: 'r5', scenarioId: 'lt1', state: 'passed', startedAt: NOW - 8 * 86400_000, progress: 1, rps: 395, p95: 402, errorRate: 0.0042, vus: 200, thresholds: [{ name: 'http_req_failed rate < 1%', passed: true }, { name: 'http_req_duration p(95) < 500 ms', passed: true }] },
  { id: 'r6', scenarioId: 'lt1', state: 'passed', startedAt: NOW - 15 * 86400_000, progress: 1, rps: 401, p95: 371, errorRate: 0.0031, vus: 200, thresholds: [{ name: 'http_req_failed rate < 1%', passed: true }, { name: 'http_req_duration p(95) < 500 ms', passed: true }] },
  { id: 'r7', scenarioId: 'lt2', state: 'passed', startedAt: NOW - 12 * 86400_000, progress: 1, rps: 2190, p95: 66, errorRate: 0.0003, vus: 600, thresholds: [{ name: 'http_req_failed rate < 1%', passed: true }, { name: 'http_req_duration p(95) < 200 ms', passed: true }] },
];

export const repos: Repo[] = [
  { id: 'rp1', name: 'shopco/orders-service', branch: 'main', sha: 'a91f3c2', analyzedAt: NOW - 2 * 3600_000, services: ['orders-api', 'order-events-consumer'], language: 'TypeScript', status: 'ready' },
  { id: 'rp2', name: 'shopco/payments', branch: 'main', sha: '4be0d17', analyzedAt: NOW - 26 * 60_000, services: ['payments-api'], language: 'Go', status: 'ready' },
  { id: 'rp3', name: 'shopco/storefront', branch: 'main', sha: 'c7d2e90', analyzedAt: NOW - 3 * 60_000, services: ['shop-web'], language: 'TypeScript', status: 'analyzing' },
];

export const inventory: InventoryItem[] = [
  ...mockEndpointsFor('orders-api').map((e) => ({ id: `inv-${e.id}`, kind: 'endpoint' as const, key: `${e.method} ${e.route}`, service: 'orders-api', file: `src/orders/${e.route.split('/')[2] ?? 'health'}.controller.ts`, line: 20 + e.route.length, criticality: e.criticality, observed: e.route !== '/v1/orders/:id/cancel' || true, monitored: e.monitored })),
  ...mockEndpointsFor('payments-api').map((e) => ({ id: `inv-p-${e.id}`, kind: 'endpoint' as const, key: `${e.method} ${e.route}`, service: 'payments-api', file: `internal/${e.route.split('/')[2]}/handler.go`, line: 28, criticality: e.criticality, observed: true, monitored: e.monitored })),
  { id: 'inv-c1', kind: 'consumer', key: 'order-created-handler', source: 'orders.created', service: 'order-events-consumer', file: 'src/consumers/order-created.ts', line: 12, criticality: 'critical', observed: true, monitored: true },
  { id: 'inv-c2', kind: 'consumer', key: 'dlq-replayer', source: 'orders.created.dlq', service: 'order-events-consumer', file: 'src/consumers/order-created.ts', line: 19, criticality: 'important', observed: false, monitored: false },
  { id: 'inv-j1', kind: 'cron', key: 'invoice-batch', source: '0 2 * * *', service: 'email-worker', file: 'worker/tasks/invoices.py', line: 8, criticality: 'important', observed: true, monitored: true },
  { id: 'inv-d1', kind: 'dependency', key: 'iyzico API', service: 'payments-api', file: 'internal/provider/iyzico.go', line: 33, criticality: 'critical', observed: true, monitored: false },
  { id: 'inv-e9', kind: 'endpoint', key: 'POST /v1/orders/bulk', service: 'orders-api', file: 'src/orders/bulk.controller.ts', line: 15, criticality: 'low', observed: false, monitored: false },
];

// ---------- shared error-rate series (same numbers on every screen) ----------
function mockServiceErrorSeries(serviceId: string, range: TimeRange): number[] {
  const s = services.find((x) => x.id === serviceId);
  if (!s) return [];
  const spike = mockIncidentShape(s.id, s.prevErrorRate / s.errorRate);
  return series(`err-${s.id}`, range, { base: s.errorRate, noise: 0.3, ...spike });
}

function mockEndpointErrorSeries(ep: Endpoint, range: TimeRange): number[] {
  const hot = ep.errorRate > 0.01;
  const spike = DEGRADED.has(ep.serviceId) ? mockIncidentShape(ep.serviceId, 0.45) : hot ? { spikeAt: 0.8, spikeMul: 2.8, spikeWidth: 6 } : {};
  return series(`err-${ep.serviceId}-${ep.id}`, range, { base: ep.errorRate, noise: 0.3, ...spike });
}

// ---------- shared traffic series ----------
function mockServiceTrafficSeries(serviceId: string, range: TimeRange): number[] {
  const s = services.find((x) => x.id === serviceId);
  return s ? series(`rpm-${s.id}`, range, { base: s.rpm }) : [];
}

function mockEndpointTrafficSeries(ep: Endpoint, range: TimeRange): number[] {
  return series(`rpm-${ep.serviceId}-${ep.id}`, range, { base: ep.rpm });
}

// ---------- outgoing dependencies & time breakdown ----------

export const TIME_CATEGORIES: { id: TimeCategory; label: string; slot: number }[] = [
  { id: 'app', label: 'App code', slot: 1 },
  { id: 'db', label: 'Database', slot: 2 },
  { id: 'cache', label: 'Cache', slot: 3 },
  { id: 'http', label: 'External HTTP', slot: 7 },
  { id: 'queue', label: 'Queue publish', slot: 5 },
];

type DepDef = Omit<Dependency, 'serviceId' | 'callsPerMin'>;
const depDefs: Record<string, DepDef[]> = {
  'orders-api': [
    { id: 'orders-db', name: 'postgres · orders-db', kind: 'db', avgMs: 3.2, p95: 12, errorRate: 0.0002 },
    { id: 'cache-main', name: 'redis · cache-main', kind: 'cache', avgMs: 0.6, p95: 1.4, errorRate: 0 },
    { id: 'payments-api', name: 'HTTP · payments-api', kind: 'http', avgMs: 180, p95: 640, errorRate: 0.0213 },
    { id: 'inventory-api', name: 'HTTP · inventory-api', kind: 'http', avgMs: 45, p95: 120, errorRate: 0.0011 },
    { id: 'orders-topic', name: 'kafka · orders.created', kind: 'queue', avgMs: 3, p95: 8, errorRate: 0.0001 },
  ],
  'payments-api': [
    { id: 'payments-db', name: 'postgres · payments-db', kind: 'db', avgMs: 4.1, p95: 15, errorRate: 0.0003 },
    { id: 'cache-pay', name: 'redis · cache-payments', kind: 'cache', avgMs: 0.5, p95: 1.1, errorRate: 0 },
    { id: 'iyzico', name: 'HTTP · iyzico API', kind: 'http', avgMs: 310, p95: 1400, errorRate: 0.028 },
    { id: 'stripe', name: 'HTTP · Stripe API', kind: 'http', avgMs: 140, p95: 420, errorRate: 0.004 },
    { id: 'payments-topic', name: 'kafka · payments.events', kind: 'queue', avgMs: 2.6, p95: 7, errorRate: 0.0001 },
  ],
  'storefront-bff': [
    { id: 'catalog-api', name: 'HTTP · catalog-api', kind: 'http', avgMs: 42, p95: 110, errorRate: 0.0006 },
    { id: 'reco-api', name: 'HTTP · recommendation-api', kind: 'http', avgMs: 900, p95: 2040, errorRate: 0.0048 },
    { id: 'cache-bff', name: 'redis · cache-bff', kind: 'cache', avgMs: 0.6, p95: 1.3, errorRate: 0 },
  ],
  'recommendation-api': [
    { id: 'reco-db', name: 'postgres · reco-db', kind: 'db', avgMs: 190, p95: 1100, errorRate: 0.002 },
    { id: 'reco-cache', name: 'redis · reco-cache', kind: 'cache', avgMs: 0.6, p95: 1.4, errorRate: 0 },
    { id: 'reco-topic', name: 'kafka · reco.events', kind: 'queue', avgMs: 2.5, p95: 7, errorRate: 0.0001 },
  ],
  // consumers keep the generic ids (their fan-out is generic) but point at the real systems
  'order-events': [
    { id: 'order-events-db', name: 'postgres · orders-db', kind: 'db', avgMs: 2.9, p95: 11, errorRate: 0.0002 },
    { id: 'order-events-cache', name: 'redis · cache-main', kind: 'cache', avgMs: 0.6, p95: 1.4, errorRate: 0 },
    { id: 'order-events-topic', name: 'kafka · orders.status-changed', kind: 'queue', avgMs: 2.7, p95: 8, errorRate: 0.0001 },
  ],
  'email-worker': [
    { id: 'email-worker-db', name: 'mysql · notifications-db', kind: 'db', avgMs: 3.8, p95: 14, errorRate: 0.0003 },
    { id: 'email-worker-cache', name: 'redis · cache-main', kind: 'cache', avgMs: 0.6, p95: 1.3, errorRate: 0 },
    { id: 'email-worker-topic', name: 'rabbitmq · email.events', kind: 'queue', avgMs: 3.1, p95: 9, errorRate: 0.0001 },
  ],
};
const genericDeps = (id: string): DepDef[] => [
  { id: `${id}-db`, name: `postgres · ${id.split('-')[0]}-db`, kind: 'db', avgMs: 2.4, p95: 9, errorRate: 0.0002 },
  { id: `${id}-cache`, name: 'redis · cache-main', kind: 'cache', avgMs: 0.5, p95: 1.2, errorRate: 0 },
  { id: `${id}-topic`, name: `kafka · ${id.split('-')[0]}.events`, kind: 'queue', avgMs: 2.8, p95: 8, errorRate: 0.0001 },
];

// calls per request, keyed by "METHOD route"
const fanOut: Record<string, Record<string, Record<string, number>>> = {
  'orders-api': {
    'POST /v1/orders': { 'orders-db': 6, 'cache-main': 2, 'inventory-api': 1, 'orders-topic': 1 },
    'GET /v1/orders/:id': { 'orders-db': 1, 'cache-main': 1 },
    'GET /v1/orders': { 'orders-db': 2, 'cache-main': 1 },
    'POST /v1/orders/:id/cancel': { 'orders-db': 4, 'payments-api': 1, 'orders-topic': 1 },
    'GET /v1/cart': { 'cache-main': 2, 'orders-db': 1 },
    'PUT /v1/cart/items/:itemId': { 'orders-db': 2, 'cache-main': 2, 'inventory-api': 1 },
    'GET /healthz': {},
  },
  'payments-api': {
    'POST /v1/charges': { 'payments-db': 3, 'cache-pay': 1, iyzico: 1, 'payments-topic': 1 },
    'POST /v1/refunds': { 'payments-db': 3, iyzico: 1, 'payments-topic': 1 },
    'GET /v1/charges/:id': { 'payments-db': 1, 'cache-pay': 1 },
    'POST /v1/webhooks/stripe': { 'payments-db': 2, stripe: 1, 'payments-topic': 1 },
  },
  'storefront-bff': {
    'GET /v1/home': { 'catalog-api': 1, 'reco-api': 1, 'cache-bff': 3 },
    'GET /v1/product/:slug': { 'catalog-api': 2, 'reco-api': 1, 'cache-bff': 2 },
    'GET /v1/search': { 'catalog-api': 1, 'cache-bff': 1 },
  },
  'recommendation-api': {
    'GET /v1/recommendations': { 'reco-db': 4, 'reco-cache': 2 },
    'POST /v1/events': { 'reco-cache': 1, 'reco-topic': 1 },
  },
};

function rawDeps(serviceId: string): DepDef[] {
  return depDefs[serviceId] ?? genericDeps(serviceId);
}

function fanOutFor(ep: Endpoint): Record<string, number> {
  const known = fanOut[ep.serviceId]?.[`${ep.method} ${ep.route}`];
  if (known) return known;
  const [db, cache, topic] = genericDeps(ep.serviceId).map((d) => d.id);
  const h = hash(ep.id);
  const out: Record<string, number> = { [db]: 1 + (h % 3), [cache]: 1 + (h % 2) };
  if (ep.method !== 'GET') out[topic] = 1;
  return out;
}

function mockDependenciesFor(serviceId: string): Dependency[] {
  const eps = mockEndpointsFor(serviceId);
  return rawDeps(serviceId).map((d) => ({
    ...d,
    serviceId,
    callsPerMin: eps.reduce((a, e) => a + e.rpm * (fanOutFor(e)[d.id] ?? 0), 0),
  }));
}

function mockEndpointCalls(ep: Endpoint): EndpointCall[] {
  const deps = mockDependenciesFor(ep.serviceId);
  return Object.entries(fanOutFor(ep))
    .map(([id, n]) => {
      const dep = deps.find((d) => d.id === id)!;
      return { dep, callsPerReq: n, msPerReq: n * dep.avgMs };
    })
    .sort((a, b) => b.msPerReq - a.msPerReq);
}

function mockEndpointBreakdown(ep: Endpoint): TimeBreakdown {
  const parts: Record<TimeCategory, number> = { app: 0, db: 0, cache: 0, http: 0, queue: 0 };
  for (const c of mockEndpointCalls(ep)) parts[c.dep.kind] += c.msPerReq;
  const depMs = parts.db + parts.cache + parts.http + parts.queue;
  // avg sits a bit above p50 (latency is right-skewed); own code takes at least 15%
  const avgMs = Math.max(ep.p50 * 1.2, depMs / 0.85);
  parts.app = avgMs - depMs;
  return { avgMs, parts };
}

/** rpm-weighted average of endpoint breakdowns. */
function mockServiceBreakdown(serviceId: string): TimeBreakdown {
  const eps = mockEndpointsFor(serviceId);
  const total = eps.reduce((a, e) => a + e.rpm, 0) || 1;
  const parts: Record<TimeCategory, number> = { app: 0, db: 0, cache: 0, http: 0, queue: 0 };
  for (const e of eps) {
    const b = mockEndpointBreakdown(e);
    for (const k of Object.keys(parts) as TimeCategory[]) parts[k] += (b.parts[k] * e.rpm) / total;
  }
  return { avgMs: Object.values(parts).reduce((a, v) => a + v, 0), parts };
}

/** Time-per-request series per category. External HTTP spikes when the service is critical. */
function mockBreakdownSeries(key: string, b: TimeBreakdown, range: TimeRange, critical: boolean): Record<TimeCategory, number[]> {
  const out = {} as Record<TimeCategory, number[]>;
  for (const c of TIME_CATEGORIES) {
    const spike = critical && c.id === 'http' ? { spikeAt: 0.8, spikeMul: 2.4, spikeWidth: 6 } : {};
    out[c.id] = series(`tb-${key}-${c.id}`, range, { base: b.parts[c.id], noise: 0.08, daily: 0.1, ...spike });
  }
  return out;
}

function mockOutgoingSeries(dep: Dependency, range: TimeRange): number[] {
  return series(`out-${dep.serviceId}-${dep.id}`, range, { base: dep.callsPerMin });
}

// ---------- incoming traffic sources (callers) ----------
/** Registry order is the stable color order inside a page. */
export const CALLERS: Caller[] = [
  { id: 'shop-web', name: 'shop-web', kind: 'web', via: 'Browser SDK · traceparent' },
  { id: 'shop-mobile', name: 'shop-mobile', kind: 'mobile', via: 'React Native SDK · traceparent' },
  { id: 'orders-api', name: 'orders-api', kind: 'service', via: 'Trace context' },
  { id: 'payments-api', name: 'payments-api', kind: 'service', via: 'Trace context' },
  { id: 'backoffice-web', name: 'backoffice-web', kind: 'web', via: 'Browser SDK · traceparent' },
  { id: 'partner-acme', name: 'Acme ERP (partner)', kind: 'partner', via: 'API key · acme-erp' },
  { id: 'stripe', name: 'Stripe', kind: 'webhook', via: 'User-Agent · Stripe/1.0' },
  { id: 'k8s-probe', name: 'kubelet probes', kind: 'probe', via: 'User-Agent · kube-probe/1.31' },
  { id: 'unknown', name: 'Uninstrumented', kind: 'unknown', via: 'No trace context, API key or known user-agent' },
  { id: 'storefront-bff', name: 'storefront-bff', kind: 'service', via: 'Trace context' },
  { id: 'seller-portal', name: 'seller-portal', kind: 'web', via: 'Browser SDK · traceparent' },
  { id: 'landing-web', name: 'landing-web', kind: 'web', via: 'Browser SDK · traceparent' },
  { id: 'courier-app', name: 'courier-app', kind: 'mobile', via: 'Flutter SDK · traceparent' },
];

const callerMixDefault: Record<string, Record<string, number>> = {
  'orders-api': { 'shop-web': 0.46, 'shop-mobile': 0.36, 'partner-acme': 0.07, 'courier-app': 0.04, 'seller-portal': 0.03, unknown: 0.04 },
  'catalog-api': { 'shop-web': 0.42, 'shop-mobile': 0.32, 'storefront-bff': 0.16, 'seller-portal': 0.05, unknown: 0.05 },
  'storefront-bff': { 'shop-web': 0.62, 'shop-mobile': 0.33, 'landing-web': 0.05 },
  'recommendation-api': { 'storefront-bff': 1 },
  'payments-api': { 'shop-web': 0.55, 'shop-mobile': 0.45 },
  'order-events': { 'orders-api': 1 },
  'email-worker': { 'orders-api': 0.6, 'payments-api': 0.4 },
};
const callerMixOverride: Record<string, Record<string, Record<string, number>>> = {
  'orders-api': {
    'GET /healthz': { 'k8s-probe': 1 },
    'POST /v1/orders/:id/cancel': { 'shop-web': 0.45, 'shop-mobile': 0.35, 'backoffice-web': 0.2 },
  },
  'payments-api': {
    // orders-api cancel → payments-api refund (1 call per cancel; matches orders-api outgoing)
    'POST /v1/refunds': { 'orders-api': 1 },
    'POST /v1/webhooks/stripe': { stripe: 1 },
  },
  'recommendation-api': {
    // storefront-bff calls recommendations once per home / product request (matches its outgoing)
    'GET /v1/recommendations': { 'storefront-bff': 1 },
    'POST /v1/events': { 'shop-web': 0.55, 'shop-mobile': 0.45 },
  },
};
const callerFactor: Partial<Record<CallerKind, { err: number; p95: number }>> = {
  mobile: { err: 1.1, p95: 1.15 },
  partner: { err: 1.8, p95: 1.3 },
  unknown: { err: 1.4, p95: 1.1 },
  probe: { err: 0, p95: 0.5 },
};

function callerMix(ep: Endpoint): Record<string, number> {
  const override = callerMixOverride[ep.serviceId]?.[`${ep.method} ${ep.route}`];
  if (override) return override;
  const base = callerMixDefault[ep.serviceId] ?? { 'shop-web': 0.55, 'shop-mobile': 0.4, unknown: 0.05 };
  // per-endpoint variation (±25%), then renormalize
  const h = hash(ep.id);
  const varied = Object.fromEntries(Object.entries(base).map(([k, v], i) => [k, v * (0.75 + (((h >> (i * 3)) & 7) / 7) * 0.5)]));
  const sum = Object.values(varied).reduce((a, v) => a + v, 0);
  return Object.fromEntries(Object.entries(varied).map(([k, v]) => [k, v / sum]));
}

/** Who writes to each topic/queue — shown as "produced by" on the consumer side. */
const PRODUCERS: Record<string, string> = {
  'orders.created': 'orders-api',
  'orders.status-changed': 'orders-api, payments-api',
  'orders.created.dlq': 'order-created-handler (failed messages)',
  'emails.transactional': 'orders-api, payments-api',
  'emails.marketing': 'backoffice-web campaigns',
};

/** A consumer's only "caller" is the data source it reads. */
function sourceCaller(ep: Endpoint): CallerTraffic {
  const s = ep.sub!;
  const kind = s.source === 'cron' ? 'schedule' : s.source === 'kafka' ? 'topic' : 'queue';
  const via = s.source === 'cron' ? `cron · ${s.schedule}` : `${s.source === 'kafka' ? 'Kafka' : s.source === 'sqs' ? 'SQS' : 'RabbitMQ'} · produced by ${PRODUCERS[s.topic] ?? 'unknown producer'}`;
  return { caller: { id: `src-${s.topic}`, name: s.topic, kind, via }, share: 1, rpm: ep.rpm, errorRate: ep.errorRate, p95: ep.p95 };
}

function mockEndpointCallers(ep: Endpoint): CallerTraffic[] {
  if (ep.sub) return [sourceCaller(ep)];
  return Object.entries(callerMix(ep))
    .map(([id, share]) => {
      const caller = CALLERS.find((c) => c.id === id)!;
      const f = callerFactor[caller.kind] ?? { err: 1, p95: 1 };
      return { caller, share, rpm: ep.rpm * share, errorRate: ep.errorRate * f.err, p95: ep.p95 * f.p95 };
    })
    .sort((a, b) => b.rpm - a.rpm);
}

function mockServiceCallers(serviceId: string): (CallerTraffic & { topEndpoint: Endpoint })[] {
  return aggregateCallers(mockEndpointsFor(serviceId), mockEndpointCallers);
}

/** Service-level callers = endpoint callers summed; the top endpoint is the one the caller hits most. */
function aggregateCallers(eps: Endpoint[], callersOf: (e: Endpoint) => CallerTraffic[]): (CallerTraffic & { topEndpoint: Endpoint })[] {
  const total = eps.reduce((a, e) => a + e.rpm, 0) || 1;
  const acc = new Map<string, { caller: Caller; rpm: number; errW: number; p95W: number; top: { ep: Endpoint; rpm: number } }>();
  for (const e of eps) {
    for (const c of callersOf(e)) {
      const cur = acc.get(c.caller.id) ?? { caller: c.caller, rpm: 0, errW: 0, p95W: 0, top: { ep: e, rpm: 0 } };
      cur.rpm += c.rpm;
      cur.errW += c.errorRate * c.rpm;
      cur.p95W += c.p95 * c.rpm;
      if (c.rpm > cur.top.rpm) cur.top = { ep: e, rpm: c.rpm };
      acc.set(c.caller.id, cur);
    }
  }
  return [...acc.values()]
    .map((v) => ({ caller: v.caller, rpm: v.rpm, share: v.rpm / total, errorRate: v.errW / (v.rpm || 1), p95: v.p95W / (v.rpm || 1), topEndpoint: v.top.ep }))
    .sort((a, b) => b.rpm - a.rpm);
}

function mockCallerSeries(targetKey: string, c: CallerTraffic, range: TimeRange): number[] {
  return series(`in-${targetKey}-${c.caller.id}`, range, { base: c.rpm, daily: c.caller.kind === 'probe' ? 0 : 0.25 });
}

// ---------- outgoing dependency health over time ----------
/** Unhealthy dependencies (error rate > 1%) share the service incident spike at 80% of the range. */
const DEGRADED_DEPS: Record<string, number> = { 'reco-db': 0.016, 'reco-api': 0.11 };
const depSpike = (d: Dependency) => (d.id in DEGRADED_DEPS ? { stepAt: DEGRADED_SINCE, stepFrom: DEGRADED_DEPS[d.id] } : d.errorRate > 0.01 ? { spikeAt: 0.8, spikeMul: 2.6, spikeWidth: 6 } : {});

function mockDepErrorSeries(d: Dependency, range: TimeRange): number[] {
  return series(`dep-err-${d.serviceId}-${d.id}`, range, { base: d.errorRate, noise: 0.3, ...depSpike(d) });
}

function mockDepLatencySeries(d: Dependency, range: TimeRange, stat: 'avg' | 'p95'): number[] {
  return series(`dep-${stat}-${d.serviceId}-${d.id}`, range, { base: stat === 'avg' ? d.avgMs : d.p95, noise: 0.1, ...depSpike(d) });
}

// =====================================================================================================
// Data mode switch (ADR-013): every function above has an api twin below. Mock implementations stay as-is.
// =====================================================================================================

/** Chart buckets for a range. Api mode: the bucket timestamps of the last api series for that range. */
export function timestamps(range: TimeRange): number[] {
  if (isApi) {
    const t = A.apiTimestamps(range);
    if (t && t.length === RANGE_POINTS[range].points) return t;
  }
  return mockTimestamps(range);
}
const zeros = (range: TimeRange) => new Array<number>(RANGE_POINTS[range].points).fill(0);
const H1 = A.rangeQs('1h');

/** Mock-only chart shaping (incident spikes); real data has its own shape. */
export function incidentShape(serviceId: string, stepFrom = 0.3): ReturnType<typeof mockIncidentShape> {
  return isApi ? {} : mockIncidentShape(serviceId, stepFrom);
}

/** "svc" or "svc-endpointId" (the key screens use for series) → ids. Service ids may contain dashes. */
function parseKey(key: string): { serviceId: string; endpointId?: string } {
  if (services.some((s) => s.id === key)) return { serviceId: key };
  const s = services.filter((x) => key.startsWith(`${x.id}-`)).sort((a, b) => b.id.length - a.id.length)[0];
  return s ? { serviceId: s.id, endpointId: key.slice(s.id.length + 1) } : { serviceId: key };
}
const scopePath = (k: { serviceId: string; endpointId?: string }) => (k.endpointId ? A.epPath(k.serviceId, k.endpointId) : A.svcPath(k.serviceId));

// ---------- endpoints ----------
export function endpointsFor(serviceId: string): Endpoint[] {
  if (!isApi) return mockEndpointsFor(serviceId);
  const svc = services.find((s) => s.id === serviceId);
  if (svc && (svc.type === 'web' || svc.type === 'mobile')) return [];
  return A.list(`${A.svcPath(serviceId)}/endpoints${H1}`, A.mapEndpoints);
}

// ---------- RED series ----------
const isClient = (id: string) => { const t = services.find((s) => s.id === id)?.type; return t === 'web' || t === 'mobile'; };
export function serviceTrafficSeries(serviceId: string, range: TimeRange): number[] {
  if (!isApi) return mockServiceTrafficSeries(serviceId, range);
  // web / mobile apps: /services/{app}/series only covers spans (zeros) → RUM views per minute
  if (isClient(serviceId)) return A.pick(A.seriesSet(`/apps/${seg(serviceId)}/series${A.rangeQs(range)}`, range), 'views', zeros(range).length);
  return A.pick(A.seriesSet(`${A.svcPath(serviceId)}/series${A.rangeQs(range)}`, range), 'rpm', zeros(range).length);
}
export function serviceErrorSeries(serviceId: string, range: TimeRange): number[] {
  if (!isApi) return mockServiceErrorSeries(serviceId, range);
  if (isClient(serviceId)) return A.pick(A.seriesSet(`/apps/${seg(serviceId)}/series${A.rangeQs(range)}`, range), 'errorRate', zeros(range).length);
  return A.pick(A.seriesSet(`${A.svcPath(serviceId)}/series${A.rangeQs(range)}`, range), 'errorRate', zeros(range).length);
}
export function endpointTrafficSeries(ep: Endpoint, range: TimeRange): number[] {
  if (!isApi) return mockEndpointTrafficSeries(ep, range);
  return A.pick(A.seriesSet(`${A.epPath(ep.serviceId, ep.id)}/series${A.rangeQs(range)}`, range), 'rpm', zeros(range).length);
}
export function endpointErrorSeries(ep: Endpoint, range: TimeRange): number[] {
  if (!isApi) return mockEndpointErrorSeries(ep, range);
  return A.pick(A.seriesSet(`${A.epPath(ep.serviceId, ep.id)}/series${A.rangeQs(range)}`, range), 'errorRate', zeros(range).length);
}

export type RedSeriesId = 'rpm' | 'errorRate' | 'p50' | 'p95' | 'p99' | 's2xx' | 's3xx' | 's4xx' | 's5xx';
/**
 * Full RED series (traffic, error rate, latency percentiles, status classes) of the project (no args), a service
 * or one endpoint. Consumers' endpoint series also carry `retried` / `dlq` in api mode (returned under s4xx / s5xx = 0).
 * Screens currently synthesize latency/status charts locally; this is the real-data source for them.
 */
export function redSeries(range: TimeRange, serviceId?: string, endpointId?: string): Record<RedSeriesId, number[]> {
  const ids: RedSeriesId[] = ['rpm', 'errorRate', 'p50', 'p95', 'p99', 's2xx', 's3xx', 's4xx', 's5xx'];
  if (isApi) {
    const path = serviceId ? `${scopePath({ serviceId, endpointId })}/series` : '/overview/series';
    const set = A.seriesSet(`${path}${A.rangeQs(range)}`, range);
    return Object.fromEntries(ids.map((id) => [id, A.pick(set, id, zeros(range).length)])) as Record<RedSeriesId, number[]>;
  }
  const backend = services.filter((s) => s.type === 'http' || s.type === 'consumer' || s.type === 'worker');
  const ep = serviceId && endpointId ? mockEndpointsFor(serviceId).find((e) => e.id === endpointId) : undefined;
  const key = ep ? `${serviceId}-${endpointId}` : serviceId ?? 'all';
  const rpm = ep ? mockEndpointTrafficSeries(ep, range) : serviceId ? mockServiceTrafficSeries(serviceId, range)
    : series('t-rpm', range, { base: backend.reduce((a, s) => a + s.rpm, 0) });
  const err = ep ? mockEndpointErrorSeries(ep, range) : serviceId ? mockServiceErrorSeries(serviceId, range)
    : series('err-all', range, { base: 0.004, noise: 0.25, spikeAt: 0.82, spikeMul: 3.4, spikeWidth: 5 });
  const p95 = ep?.p95 ?? services.find((s) => s.id === serviceId)?.p95 ?? 214;
  const lat = (stat: string, mul: number) => series(`${stat}-${key}`, range, { base: p95 * mul });
  const s5 = rpm.map((v, i) => v * (err[i] ?? 0));
  const s4 = rpm.map((v) => v * 0.012);
  return {
    rpm, errorRate: err, p50: lat('p50', 0.42), p95: lat('p95', 1), p99: lat('p99', 2.1),
    s2xx: rpm.map((v, i) => Math.max(0, v - s5[i] - s4[i])), s3xx: rpm.map(() => 0), s4xx: s4, s5xx: s5,
  };
}

// ---------- dependencies, calls, time breakdown ----------
export function dependenciesFor(serviceId: string): Dependency[] {
  if (!isApi) return mockDependenciesFor(serviceId);
  return A.list(`${A.svcPath(serviceId)}/dependencies${H1}`, A.mapDependencies);
}
export function endpointCalls(ep: Endpoint): EndpointCall[] {
  if (!isApi) return mockEndpointCalls(ep);
  return A.list(`${A.epPath(ep.serviceId, ep.id)}/calls${H1}`, A.mapCalls);
}
const loadingBreakdown = (avg: number): TimeBreakdown => ({ avgMs: avg, parts: { app: avg, db: 0, cache: 0, http: 0, queue: 0 } });
export function endpointBreakdown(ep: Endpoint): TimeBreakdown {
  if (!isApi) return mockEndpointBreakdown(ep);
  return remote(`${A.epPath(ep.serviceId, ep.id)}/breakdown${H1}`, loadingBreakdown(ep.p50), A.mapBreakdown);
}
export function serviceBreakdown(serviceId: string): TimeBreakdown {
  if (!isApi) return mockServiceBreakdown(serviceId);
  const s = services.find((x) => x.id === serviceId);
  return remote(`${A.svcPath(serviceId)}/breakdown${H1}`, loadingBreakdown((s?.p95 ?? 0) * 0.5), A.mapBreakdown);
}
/** `key` = service id or `${serviceId}-${endpointId}`. Api mode ignores `b` / `critical` (mock shaping). */
export function breakdownSeries(key: string, b: TimeBreakdown, range: TimeRange, critical: boolean): Record<TimeCategory, number[]> {
  if (!isApi) return mockBreakdownSeries(key, b, range, critical);
  const set = A.seriesSet(`${scopePath(parseKey(key))}/breakdown/series${A.rangeQs(range)}`, range);
  return Object.fromEntries(A.TIME_PARTS.map((k) => [k, A.pick(set, k, zeros(range).length)])) as Record<TimeCategory, number[]>;
}

const depSeries = (d: Dependency, range: TimeRange) =>
  A.seriesSet(`${A.svcPath(d.serviceId)}/dependencies/${seg(d.id)}/series${A.rangeQs(range)}`, range);
export function outgoingSeries(dep: Dependency, range: TimeRange): number[] {
  return isApi ? A.pick(depSeries(dep, range), 'calls', zeros(range).length) : mockOutgoingSeries(dep, range);
}
export function depErrorSeries(d: Dependency, range: TimeRange): number[] {
  return isApi ? A.pick(depSeries(d, range), 'errorRate', zeros(range).length) : mockDepErrorSeries(d, range);
}
export function depLatencySeries(d: Dependency, range: TimeRange, stat: 'avg' | 'p95'): number[] {
  return isApi ? A.pick(depSeries(d, range), stat, zeros(range).length) : mockDepLatencySeries(d, range, stat);
}

// ---------- callers ----------
const strip = (r: A.CallerRow): CallerTraffic => ({ caller: r.caller, rpm: r.rpm, share: r.share, errorRate: r.errorRate, p95: r.p95 });

export function endpointCallers(ep: Endpoint): CallerTraffic[] {
  if (!isApi) return mockEndpointCallers(ep);
  return A.list(`${A.epPath(ep.serviceId, ep.id)}/callers${H1}`, A.mapCallers(CALLERS)).map(strip);
}

export function serviceCallers(serviceId: string): (CallerTraffic & { topEndpoint: Endpoint })[] {
  if (!isApi) return mockServiceCallers(serviceId);
  const eps = endpointsFor(serviceId);
  const rows = A.list(`${A.svcPath(serviceId)}/callers${H1}`, A.mapCallers(CALLERS));
  // the api has no service-level sources for consumers (only per process) → sum the processes' sources
  if (rows.length === 0 && eps.some((e) => e.sub)) return aggregateCallers(eps, endpointCallers);
  const placeholder: Endpoint = { id: '', serviceId, method: '', route: '', rpm: 0, errorRate: 0, p50: 0, p95: 0, p99: 0, monitored: false, criticality: 'low' };
  return rows.map((r) => ({ ...strip(r), topEndpoint: eps.find((e) => e.id === r.topEndpointId) ?? r.topEndpointRaw ?? placeholder }));
}

/** `targetKey` = service id or `${serviceId}-${endpointId}`. */
export function callerSeries(targetKey: string, c: CallerTraffic, range: TimeRange): number[] {
  if (!isApi) return mockCallerSeries(targetKey, c, range);
  const k = parseKey(targetKey);
  if (c.caller.kind === 'topic' || c.caller.kind === 'queue' || c.caller.kind === 'schedule') {
    // a consumer's "caller" is its topic: the api has no caller series for it → messages of the processes reading it
    const eps = endpointsFor(k.serviceId).filter((e) => (!k.endpointId || e.id === k.endpointId) && e.sub?.topic === c.caller.name);
    return eps.map((e) => endpointTrafficSeries(e, range)).reduce((a, v) => a.map((x, i) => x + (v[i] ?? 0)), zeros(range));
  }
  const set = A.seriesSet(`${scopePath(k)}/callers/series${A.rangeQs(range)}`, range);
  return A.pick(set, c.caller.id, zeros(range).length);
}

// ---------- deploys & traces (no screen uses these yet: DeploysTab and the endpoint trace list are screen-local mock) ----------
export interface ServiceDeploy { service: string; env: string; version: string; ts: number }
export function deploysFor(serviceId: string, range: TimeRange): ServiceDeploy[] {
  if (isApi) return A.list(`${A.svcPath(serviceId)}/deploys${A.rangeQs(range)}`, (raw: A.RawDeploy[] | null) => (raw ?? []).map((d) => ({ ...d })));
  const s = services.find((x) => x.id === serviceId);
  return s ? [{ service: s.name, env: 'production', version: s.version, ts: NOW - 2 * 3600_000 }] : [];
}
export type TraceSummary = A.RawTraceSummary;
export function tracesFor(serviceId: string, ep: Endpoint | undefined, range: TimeRange, opts: { errors?: boolean; sort?: 'recent' | 'slowest'; limit?: number } = {}): TraceSummary[] {
  const s = services.find((x) => x.id === serviceId);
  if (isApi) {
    return A.list(`/traces${A.rangeQs(range, {
      service: s?.name ?? serviceId, endpoint: ep ? (ep.sub ? ep.sub.process : `${ep.method} ${ep.route}`) : undefined,
      errors: opts.errors ? 'true' : undefined, sort: opts.sort, limit: opts.limit ?? 20,
    })}`, (raw: TraceSummary[] | null) => raw ?? []);
  }
  const p95 = ep?.p95 ?? s?.p95 ?? 100;
  const hot = (ep?.errorRate ?? s?.errorRate ?? 0) > 0.01;
  return Array.from({ length: Math.min(opts.limit ?? 6, 6) }, (_, i) => ({
    id: (0x9f3a12 + i * 7919).toString(16) + 'c0ffee' + i, ts: NOW - i * 47_000, dur: p95 * (0.6 + ((i * 37) % 10) / 6),
    status: hot && i % 2 === 0 ? 502 : 200, service: s?.name ?? serviceId, method: ep?.method ?? 'GET', route: ep?.route ?? '/', isError: hot && i % 2 === 0,
  }));
}

/** The mock implementations, for modules that derive mock data from mock data (graph.ts). */
export const mockApm = {
  endpointsFor: mockEndpointsFor, endpointCalls: mockEndpointCalls, endpointBreakdown: mockEndpointBreakdown, endpointCallers: mockEndpointCallers,
};

// ---------- list hydration ----------
if (isApi) {
  // real services replace the mock registry; derived mock lists above were built before this point
  services.length = 0;
  registerHydrator(() => hydrateList(services, `/services${H1}`, A.mapServices));
}
