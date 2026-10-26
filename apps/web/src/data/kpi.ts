/**
 * KPIs: objectives on what users experience ("homepage loads fast", "checkout succeeds").
 * Alerts are defined on the KPI (error-budget burn), and when a KPI degrades the root cause
 * analysis walks its call tree down to the deepest node that explains the change.
 */
import { DEGRADED_SINCE, NOW, RANGE_POINTS, series, timestamps } from './mock';
import { endpointNode } from './graph';
import { hydrateList, isApi, mutate, qs, registerHydrator, remote, seg } from './remote';
import type { Kpi, KpiAlertRule, KpiSli, RcaNode, Severity, TimeRange } from './types';
import './api/ops'; // registers the monitors / incidents / load-test hydrators (api mode)

const M = 60_000;
const PAGERDUTY = 'PagerDuty';

export const kpis: Kpi[] = [
  {
    id: 'homepage-fast', name: 'Homepage loads fast', owner: 'web-team',
    description: 'Visitors see the homepage quickly. The first thing every shopper sees.',
    target: { kind: 'page', serviceId: 'shop-web', ref: '/' },
    sli: 'latency', goodWhen: 'Page view with LCP ≤ 2.5 s and no JS error', threshold: { metric: 'LCP', value: 2500 },
    objective: 0.95, window: '28d', current: 0.68, windowGood: 0.962, budgetRemaining: 0.41, burnRate1h: 6.4, burnRate6h: 1.9,
    status: 'critical', source: 'code', degradedSince: DEGRADED_SINCE,
    metric: { label: 'LCP p75', baseline: 1780, current: 2543, unit: 'ms' },
    alerts: [
      { id: 'homepage-fast-fast', kind: 'burn_rate', condition: 'Budget burns > 6× too fast over 1h and 5m', severity: 'critical', channels: ['#web-team', PAGERDUTY], state: 'firing' },
      { id: 'homepage-fast-slow', kind: 'burn_rate', condition: 'Budget burns > 3× too fast over 6h and 30m', severity: 'warning', channels: ['#web-team'], state: 'pending' },
    ],
  },
  {
    id: 'product-fast', name: 'Product page loads fast', owner: 'web-team',
    description: 'Product detail pages render quickly; most purchases start here.',
    target: { kind: 'page', serviceId: 'shop-web', ref: '/p/:slug' },
    sli: 'latency', goodWhen: 'Page view with LCP ≤ 2.5 s and no JS error', threshold: { metric: 'LCP', value: 2500 },
    objective: 0.9, window: '28d', current: 0.71, windowGood: 0.93, budgetRemaining: 0.62, burnRate1h: 2.9, burnRate6h: 0.9,
    status: 'warning', source: 'template', degradedSince: DEGRADED_SINCE,
    metric: { label: 'LCP p75', baseline: 2410, current: 3173, unit: 'ms' },
    alerts: [
      { id: 'product-fast-fast', kind: 'burn_rate', condition: 'Budget burns > 6× too fast over 1h and 5m', severity: 'critical', channels: ['#web-team'], state: 'ok' },
      { id: 'product-fast-slow', kind: 'burn_rate', condition: 'Budget burns > 2× too fast over 6h and 30m', severity: 'warning', channels: ['#web-team'], state: 'pending' },
    ],
  },
  {
    id: 'checkout-success', name: 'Checkout succeeds', owner: 'payments-team',
    description: 'A shopper who submits checkout ends up with a paid order.',
    target: { kind: 'journey', serviceId: 'shop-web', ref: 'Checkout: /checkout → POST /v1/orders → POST /v1/charges' },
    sli: 'success_rate', goodWhen: 'Checkout submit ends with order created and payment captured',
    objective: 0.99, window: '28d', current: 0.968, windowGood: 0.991, budgetRemaining: 0.34, burnRate1h: 3.2, burnRate6h: 1.4,
    status: 'warning', source: 'code', degradedSince: NOW - 23 * M,
    metric: { label: 'Failed checkouts', baseline: 0.009, current: 0.032, unit: 'ratio' },
    alerts: [
      { id: 'checkout-success-fast', kind: 'burn_rate', condition: 'Budget burns > 3× too fast over 1h and 5m', severity: 'critical', channels: ['#payments-alerts', PAGERDUTY], state: 'firing' },
      { id: 'checkout-success-below', kind: 'sli_below', condition: 'Success below 97% for 15m', severity: 'warning', channels: ['#payments-alerts'], state: 'firing' },
    ],
  },
  {
    id: 'order-api-success', name: 'Order placement API succeeds', owner: 'orders-team',
    description: 'POST /v1/orders answers successfully; partners and apps rely on it.',
    target: { kind: 'endpoint', serviceId: 'orders-api', ref: 'POST /v1/orders', endpointId: 'post-v1-orders' },
    sli: 'success_rate', goodWhen: 'Response is not 5xx',
    objective: 0.995, window: '28d', current: 0.9919, windowGood: 0.9952, budgetRemaining: 0.55, burnRate1h: 1.6, burnRate6h: 1.5,
    status: 'warning', source: 'code', degradedSince: NOW - 118 * M,
    metric: { label: 'Failure rate', baseline: 0.0019, current: 0.0081, unit: 'ratio' },
    alerts: [
      { id: 'order-api-success-slow', kind: 'burn_rate', condition: 'Budget burns > 1.5× too fast over 6h and 30m', severity: 'warning', channels: ['#orders-oncall'], state: 'pending' },
    ],
  },
  {
    id: 'homepage-available', name: 'Homepage is up', owner: 'web-team',
    description: 'The homepage renders at all — document served and no fatal script error.',
    target: { kind: 'page', serviceId: 'shop-web', ref: '/' },
    sli: 'availability', goodWhen: 'Document 2xx and app mounted (no fatal JS error)',
    objective: 0.999, window: '28d', current: 0.9993, windowGood: 0.9995, budgetRemaining: 0.78, burnRate1h: 0.7, burnRate6h: 0.4,
    status: 'ok', source: 'code',
    alerts: [
      { id: 'homepage-available-fast', kind: 'burn_rate', condition: 'Budget burns > 14× too fast over 1h and 5m', severity: 'critical', channels: ['#web-team', PAGERDUTY], state: 'ok' },
    ],
  },
  {
    id: 'search-fast', name: 'Search answers fast', owner: 'catalog-team',
    description: 'Search results come back quickly for apps and the storefront.',
    target: { kind: 'endpoint', serviceId: 'catalog-api', ref: 'GET /v1/search', endpointId: 'get-v1-search' },
    sli: 'latency', goodWhen: 'Response ≤ 300 ms and not 5xx', threshold: { metric: 'Duration', value: 300 },
    objective: 0.99, window: '28d', current: 0.995, windowGood: 0.994, budgetRemaining: 0.71, burnRate1h: 0.5, burnRate6h: 0.6,
    status: 'ok', source: 'template',
    alerts: [{ id: 'search-fast-fast', kind: 'burn_rate', condition: 'Budget burns > 6× too fast over 1h and 5m', severity: 'warning', channels: ['#catalog'], state: 'ok' }],
  },
  {
    id: 'courier-start', name: 'Courier app opens fast', owner: 'mobile-team',
    description: 'Couriers can start working right away after opening the app.',
    target: { kind: 'app_start', serviceId: 'courier-app', ref: 'Cold start' },
    sli: 'latency', goodWhen: 'Cold start ≤ 2 s and no crash', threshold: { metric: 'Cold start', value: 2000 },
    objective: 0.95, window: '7d', current: 0.962, windowGood: 0.958, budgetRemaining: 0.84, burnRate1h: 0.8, burnRate6h: 0.8,
    status: 'ok', source: 'manual',
    alerts: [{ id: 'courier-start-slow', kind: 'burn_rate', condition: 'Budget burns > 2× too fast over 6h and 30m', severity: 'warning', channels: ['#mobile'], state: 'ok' }],
  },
  {
    id: 'seller-orders-fast', name: 'Seller order list loads', owner: 'seller-team',
    description: 'Sellers see their orders quickly in the seller portal.',
    target: { kind: 'page', serviceId: 'seller-portal', ref: '/orders' },
    sli: 'latency', goodWhen: 'Page view with LCP ≤ 3 s and no JS error', threshold: { metric: 'LCP', value: 3000 },
    objective: 0.9, window: '28d', current: 0.93, windowGood: 0.925, budgetRemaining: 0.75, burnRate1h: 0.7, burnRate6h: 0.8,
    status: 'ok', source: 'manual',
    alerts: [{ id: 'seller-orders-fast-slow', kind: 'burn_rate', condition: 'Budget burns > 2× too fast over 6h and 30m', severity: 'warning', channels: ['#seller-team'], state: 'ok' }],
  },
];

// ---------------------------------------------------------------- call trees behind KPIs
/** Page node on top of the backend subtree. Page metric: LCP p75 in ms. */
function pageTree(kpi: Kpi, apiService: string, apiEndpoint: string): RcaNode | undefined {
  const api = endpointNode(apiService, apiEndpoint, 'latency');
  if (!api || !kpi.metric) return undefined;
  const assets: RcaNode = { id: 'cdn:shop-web', kind: 'external', label: 'CDN · static assets', service: 'shop-web', baseline: 380, current: 395 };
  // the page waits for its API call and its assets; the rest is client-side rendering
  // client-side rendering is whatever the baseline LCP doesn't spend waiting; assumed unchanged
  const render = Math.max(0, kpi.metric.baseline - api.baseline - assets.baseline);
  return {
    id: `page:${kpi.id}`, kind: 'page', label: `${kpi.target.ref} on ${kpi.target.serviceId}`, service: kpi.target.serviceId,
    link: `/services/${kpi.target.serviceId}`,
    baseline: kpi.metric.baseline, current: render + api.current + assets.current,
    children: [{ node: api, callsPerReq: 1 }, { node: assets, callsPerReq: 1 }],
  };
}

function checkoutTree(kpi: Kpi): RcaNode | undefined {
  const orders = endpointNode('orders-api', 'post-v1-orders', 'errors');
  const charges = endpointNode('payments-api', 'post-v1-charges', 'errors');
  if (!orders || !charges || !kpi.metric) return undefined;
  const page: RcaNode = { id: 'page:checkout-js', kind: 'page', label: '/checkout page (JS errors)', service: 'shop-web', link: '/services/shop-web', baseline: 0.0092, current: 0.0094 };
  return {
    id: `journey:${kpi.id}`, kind: 'page', label: 'Checkout journey', service: 'shop-web', link: '/services/shop-web',
    baseline: kpi.metric.baseline, current: kpi.metric.current,
    children: [{ node: page, callsPerReq: 1 }, { node: orders, callsPerReq: 1 }, { node: charges, callsPerReq: 1 }],
  };
}

/** Call tree for a degraded KPI, or undefined when it's healthy. */
export function kpiTree(kpi: Kpi): RcaNode | undefined {
  // the hand-built trees below walk mock endpoints; api KPIs get their root cause from the alert engine (incident.rootCause)
  if (isApi || kpi.status === 'ok') return undefined;
  switch (kpi.id) {
    case 'homepage-fast': return pageTree(kpi, 'storefront-bff', 'get-v1-home');
    case 'product-fast': return pageTree(kpi, 'storefront-bff', 'get-v1-product-slug');
    case 'checkout-success': return checkoutTree(kpi);
    case 'order-api-success': return kpi.target.endpointId ? endpointNode(kpi.target.serviceId, kpi.target.endpointId, 'errors') : undefined;
    default: return undefined;
  }
}

export const kpiUnit = (k: Kpi): 'ms' | 'ratio' => (k.metric?.unit ?? (k.sli === 'latency' ? 'ms' : 'ratio'));

// ---------------------------------------------------------------- series
// ---------------------------------------------------------------- api mode (ADR-013)
interface SeriesSet { timestamps: number[]; series: { id: string; data: number[] }[] }
type RawKpi = Omit<Kpi, 'threshold' | 'metric' | 'alerts'> & {
  threshold?: Kpi['threshold'] | null; metric?: Kpi['metric'] | null; noData?: boolean;
  alerts?: (Omit<KpiAlertRule, 'kind' | 'state'> & { kind: string; state: string })[] | null;
};
const mapKpi = (k: RawKpi): Kpi => ({
  ...k, threshold: k.threshold ?? undefined, metric: k.metric ?? undefined, degradedSince: k.degradedSince || undefined,
  window: k.window === '7d' ? '7d' : '28d', source: k.source === 'code' || k.source === 'template' ? k.source : 'manual',
  alerts: (k.alerts ?? []).map((a) => ({
    ...a, kind: a.kind === 'burn_rate' ? 'burn_rate' : 'sli_below', channels: a.channels ?? [],
    state: a.state === 'firing' || a.state === 'pending' ? a.state : 'ok',
  })),
});
if (isApi) registerHydrator(() => hydrateList(kpis, '/kpis', (raw: RawKpi[]) => raw.map(mapKpi)));

const kpiSet = (k: Kpi, range: TimeRange) => remote<SeriesSet | null>(`/kpis/${seg(k.id)}/series${qs({ range })}`, null);
function kpiData(k: Kpi, range: TimeRange, id: 'good' | 'total' | 'burnRate', empty: number): number[] {
  const n = timestamps(range).length;
  const d = kpiSet(k, range)?.series.find((x) => x.id === id)?.data ?? [];
  return d.length >= n ? d.slice(d.length - n) : [...Array(n - d.length).fill(empty), ...d];
}

export interface KpiDraft {
  name: string; description: string; owner: string;
  target: Kpi['target']; sli: KpiSli;
  good: { metric?: string; thresholdMs?: number; noError?: boolean; description?: string };
  objective: number; window: '7d' | '28d';
  alerts: { kind: 'burn_rate' | 'sli_below'; longWindow: string; shortWindow?: string; threshold: number; severity: Severity; channels: string[] }[];
}
/** Create a KPI (api: POST /kpis; mock: appended to `kpis`). Resolves to its id. */
export async function createKpi(d: KpiDraft): Promise<string> {
  if (isApi) {
    const k = await mutate<{ id: string }>('POST', '/kpis', { ...d, source: 'manual' }, ['/kpis']);
    return k.id;
  }
  const id = `${d.name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '') || 'kpi'}-${Date.now().toString(36).slice(-4)}`;
  kpis.push({
    id, name: d.name, description: d.description, owner: d.owner, target: d.target, sli: d.sli,
    goodWhen: d.good.description ?? '', threshold: d.good.thresholdMs ? { metric: d.good.metric ?? 'Duration', value: d.good.thresholdMs } : undefined,
    objective: d.objective, window: d.window, current: d.objective + (1 - d.objective) / 2, windowGood: d.objective + (1 - d.objective) / 2,
    budgetRemaining: 1, burnRate1h: 0.5, burnRate6h: 0.5, status: 'ok', source: 'manual',
    alerts: d.alerts.map((a, i) => ({
      id: `${id}-${i}`, kind: a.kind, severity: a.severity, channels: a.channels, state: 'ok',
      condition: a.kind === 'burn_rate' ? `Budget burns > ${a.threshold}× too fast over ${a.longWindow}${a.shortWindow ? ` and ${a.shortWindow}` : ''}` : `Good share below ${a.threshold} over ${a.longWindow}`,
    })),
  });
  return id;
}

/** Share of good events over time. Steps down at `degradedSince`. */
export function kpiSeries(k: Kpi, range: TimeRange): number[] {
  if (isApi) return kpiData(k, range, 'good', 1);
  const before = Math.min(0.999, Math.max(k.windowGood, k.objective + (1 - k.objective) * 0.3));
  const bad = series(`kpi-bad-${k.id}`, range, {
    base: 1 - k.current, noise: 0.12, daily: 0.05,
    ...(k.degradedSince ? { stepAt: k.degradedSince, stepFrom: (1 - before) / (1 - k.current) } : {}),
  });
  return bad.map((b) => Math.max(0, Math.min(1, 1 - b)));
}

/** The KPI's headline metric (e.g. LCP p75) over time. */
export function kpiMetricSeries(k: Kpi, range: TimeRange): number[] {
  if (!k.metric) return [];
  // the api has no headline-metric series yet: flat at the current value
  if (isApi) return timestamps(range).map(() => k.metric!.current);
  return series(`kpi-m-${k.id}`, range, {
    base: k.metric.current, noise: 0.08, daily: 0.05,
    ...(k.degradedSince ? { stepAt: k.degradedSince, stepFrom: k.metric.baseline / k.metric.current } : {}),
  });
}

/** Error budget left over the range; drops faster while degraded. */
export function kpiBudgetSeries(k: Kpi, range: TimeRange): number[] {
  if (isApi) {
    // walk back from the current budget: each bucket's bad events use (bad / allowed) of the window's events
    const good = kpiData(k, range, 'good', 1), total = kpiData(k, range, 'total', 0);
    const bucketsPerWindow = ((k.window === '7d' ? 7 : 28) * 86400_000) / RANGE_POINTS[range].stepMs;
    const windowEvents = (total.reduce((a, b) => a + b, 0) / (total.length || 1)) * bucketsPerWindow;
    const allowed = 1 - k.objective;
    const out = Array<number>(good.length);
    let acc = k.budgetRemaining;
    for (let i = good.length - 1; i >= 0; i--) {
      out[i] = Math.min(1, acc);
      if (windowEvents > 0 && allowed > 0) acc += ((1 - good[i]) * total[i]) / (allowed * windowEvents);
    }
    return out;
  }
  const ts = timestamps(range);
  const span = ts[ts.length - 1] - ts[0] || 1;
  const windowMs = (k.window === '7d' ? 7 : 28) * 24 * 3600_000;
  return ts.map((t) => {
    const ago = ts[ts.length - 1] - t;
    const fast = k.degradedSince ? Math.max(0, Math.min(ago, NOW - k.degradedSince)) : 0;
    const slow = ago - fast;
    const used = (fast * k.burnRate1h + slow * Math.min(1, k.burnRate6h)) / windowMs;
    return Math.min(1, k.budgetRemaining + used * (span > 0 ? 1 : 0));
  });
}

/** KPIs that target the service, or whose call tree passes through it. */
export function kpisTouching(serviceId: string): Kpi[] {
  const name = serviceId;
  const walk = (n: RcaNode | undefined): boolean => !!n && (n.service === name || !!n.children?.some((c) => walk(c.node)));
  return kpis.filter((k) => k.target.serviceId === serviceId || walk(kpiTree(k)));
}
