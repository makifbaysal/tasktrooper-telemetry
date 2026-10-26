/**
 * Per-app RUM data. Any number of web / mobile apps report through the browser,
 * React Native and Flutter SDKs; each one is a "service" of type web | mobile.
 */
import { NOW, mockApm, rumPages, series, services, sessions, timestamps } from './mock';
import { isApi, qs, remote, seg } from './remote';
import {
  emptyPage, list, mapEndpoint, mapRumPages, mapService, mapSessions, mapVital, pick, seriesSet,
  type RawAppApiCall, type RawAppSummary, type RawRumError,
} from './api/apm';
import type { Endpoint, RumPage, Service, Session, TimeRange } from './types';

const { endpointsFor, endpointCallers } = mockApm;
const appPath = (appId: string) => `/apps/${seg(services.find((s) => s.id === appId)?.name ?? appId)}`;
const H1 = qs({ range: '1h' });

export const isClientApp = (s: Pick<Service, 'type'>) => s.type === 'web' || s.type === 'mobile';
export const clientApps = () => services.filter(isClientApp);

export interface AppSummary {
  activeUsers: number;
  prevActiveUsers: number;
  sessions: number;
  pageViewsPerMin: number;
  avgSessionSec: number;
  /** web: JS error rate (page views with an error), mobile: crash rate (sessions) */
  errorRate: number;
  prevErrorRate: number;
  /** web: Core Web Vitals p75, mobile: start-up timings p75 */
  vitals: { id: string; label: string; value: number; good: number; poor: number; unit: 'ms' | 'score' }[];
}

function h(s: string) {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}
const vary = (key: string, base: number, spread = 0.3) => base * (1 - spread / 2 + ((h(key) % 1000) / 1000) * spread);

const PAGES: Record<string, string[]> = {
  'shop-mobile': ['Home', 'ProductDetail', 'Search', 'Cart', 'Checkout', 'OrderTracking'],
  'backoffice-web': ['/orders', '/orders/:id', '/refunds', '/customers/:id', '/reports'],
  'seller-portal': ['/dashboard', '/orders', '/products', '/products/:id/edit', '/payouts'],
  'landing-web': ['/', '/pricing', '/blog/:slug', '/campaign/:slug'],
  'courier-app': ['Deliveries', 'DeliveryDetail', 'ProofOfDelivery', 'Earnings'],
};

/** Pages (web) or screens (mobile) of an app. */
function mockRumPagesFor(appId: string): RumPage[] {
  if (appId === 'shop-web') return rumPages;
  const app = services.find((s) => s.id === appId);
  const names = PAGES[appId] ?? ['/'];
  const scale = (app?.rpm ?? 500) * 12;
  return names.map((path, i) => {
    const k = `${appId}${path}`;
    const views = Math.round(vary(k, scale / (i + 1.4)));
    return {
      id: path.replace(/[^a-z0-9]+/gi, '-').replace(/^-|-$/g, '').toLowerCase() || 'root',
      path, views, users: Math.round(views * vary(`${k}u`, 0.45, 0.4)), avgTime: Math.round(vary(`${k}t`, 55, 0.8)),
      lcp: Math.round(vary(`${k}l`, (app?.p95 ?? 2000) * 0.85, 0.5)), inp: Math.round(vary(`${k}i`, 150, 0.8)),
      cls: Number(vary(`${k}c`, 0.05, 1.4).toFixed(2)), errorRate: vary(`${k}e`, app?.errorRate ?? 0.002, 1), bounce: vary(`${k}b`, 0.25, 0.8),
    };
  });
}

function mockSessionsFor(appId: string): Session[] {
  const pages = mockRumPagesFor(appId);
  const mobile = services.find((s) => s.id === appId)?.type === 'mobile';
  return sessions.map((s, i) => ({
    ...s,
    id: `${s.id}${appId === 'shop-web' ? '' : `-${(h(appId) % 997).toString(36)}`}`,
    device: mobile ? (i % 2 ? 'iPhone 15 · iOS 19' : 'Pixel 9 · Android 16') : s.device,
    entry: pages[i % Math.min(3, pages.length)].path,
    exit: pages[(i * 3) % pages.length].path,
    startedAt: NOW - ((h(`${appId}${i}`) % 3600) * 1000),
  }));
}

function mockAppSummary(appId: string): AppSummary {
  const app = services.find((s) => s.id === appId)!;
  const mobile = app.type === 'mobile';
  const users = Math.round(app.rpm * 0.42);
  const lcp = mockRumPagesFor(appId).reduce((a, p) => a + p.lcp * p.views, 0) / mockRumPagesFor(appId).reduce((a, p) => a + p.views, 0);
  return {
    activeUsers: users, prevActiveUsers: Math.round(users * app.prevRpm / app.rpm),
    sessions: Math.round(users * 1.3), pageViewsPerMin: app.rpm, avgSessionSec: Math.round(vary(`${appId}s`, mobile ? 420 : 310)),
    errorRate: app.errorRate, prevErrorRate: app.prevErrorRate,
    vitals: mobile
      ? [
          { id: 'cold', label: 'Cold start', value: vary(`${appId}cold`, 1500), good: 2000, poor: 4000, unit: 'ms' },
          { id: 'warm', label: 'Warm start', value: vary(`${appId}warm`, 520), good: 1000, poor: 2000, unit: 'ms' },
          { id: 'render', label: 'Slow frames', value: vary(`${appId}fr`, 0.04, 0.6), good: 0.05, poor: 0.1, unit: 'score' },
          { id: 'net', label: 'API p75', value: vary(`${appId}net`, 380), good: 500, poor: 1000, unit: 'ms' },
        ]
      : [
          { id: 'lcp', label: 'LCP', value: lcp, good: 2500, poor: 4000, unit: 'ms' },
          { id: 'inp', label: 'INP', value: vary(`${appId}inp`, 170), good: 200, poor: 500, unit: 'ms' },
          { id: 'cls', label: 'CLS', value: vary(`${appId}cls`, 0.06, 0.8), good: 0.1, poor: 0.25, unit: 'score' },
          { id: 'ttfb', label: 'TTFB', value: vary(`${appId}ttfb`, 420), good: 800, poor: 1800, unit: 'ms' },
        ],
  };
}

export interface AppApiCall { endpoint: Endpoint; service: Service; rpm: number; errorRate: number; p95: number }

/** Backend endpoints this app calls (from the app's traceparent on each request). */
function mockAppApiCalls(appId: string): AppApiCall[] {
  return services
    .filter((s) => !isClientApp(s))
    .flatMap((s) => endpointsFor(s.id).map((e) => ({ e, s, c: endpointCallers(e).find((x) => x.caller.id === appId) })))
    .filter((x) => x.c)
    .map(({ e, s, c }) => ({ endpoint: e, service: s, rpm: c!.rpm, errorRate: c!.errorRate, p95: c!.p95 }))
    .sort((a, b) => b.rpm - a.rpm);
}

// =====================================================================================================
// Data mode switch (ADR-013). Mock implementations above stay as-is.
// =====================================================================================================

/** Pages (web) or screens (mobile) of an app. Never empty (a zero "/" row while loading / without data). */
export function rumPagesFor(appId: string): RumPage[] {
  if (!isApi) return mockRumPagesFor(appId);
  return remote(`${appPath(appId)}/pages${H1}`, [emptyPage()], mapRumPages);
}

export function sessionsFor(appId: string): Session[] {
  if (!isApi) return mockSessionsFor(appId);
  return list(`${appPath(appId)}/sessions${qs({ range: '1h', limit: 50 })}`, mapSessions);
}

const VITAL_DEFAULTS: Record<'web' | 'mobile', AppSummary['vitals']> = {
  web: [
    { id: 'lcp', label: 'LCP', value: 0, good: 2500, poor: 4000, unit: 'ms' },
    { id: 'inp', label: 'INP', value: 0, good: 200, poor: 500, unit: 'ms' },
    { id: 'cls', label: 'CLS', value: 0, good: 0.1, poor: 0.25, unit: 'score' },
    { id: 'ttfb', label: 'TTFB', value: 0, good: 800, poor: 1800, unit: 'ms' },
  ],
  mobile: [
    { id: 'cold', label: 'Cold start', value: 0, good: 2000, poor: 4000, unit: 'ms' },
    { id: 'warm', label: 'Warm start', value: 0, good: 1000, poor: 2000, unit: 'ms' },
    { id: 'render', label: 'Slow frames', value: 0, good: 0.05, poor: 0.1, unit: 'score' },
    { id: 'net', label: 'API p75', value: 0, good: 500, poor: 1000, unit: 'ms' },
  ],
};

export function appSummary(appId: string): AppSummary {
  if (!isApi) return mockAppSummary(appId);
  const kind = services.find((s) => s.id === appId)?.type === 'mobile' ? 'mobile' : 'web';
  const fallback: AppSummary = { activeUsers: 0, prevActiveUsers: 0, sessions: 0, pageViewsPerMin: 0, avgSessionSec: 0, errorRate: 0, prevErrorRate: 0, vitals: VITAL_DEFAULTS[kind] };
  return remote(`${appPath(appId)}/summary${H1}`, fallback, (r: RawAppSummary) => {
    const vitals = (r.vitals ?? []).map(mapVital);
    return {
      activeUsers: r.activeUsers ?? 0, prevActiveUsers: r.prevActiveUsers ?? 0, sessions: r.sessions ?? 0, pageViewsPerMin: r.pageViewsPerMin ?? 0,
      avgSessionSec: Math.round(r.avgSessionSec ?? 0), errorRate: r.errorRate ?? 0, prevErrorRate: r.prevErrorRate ?? 0,
      // screens read vitals[0] (headline load metric) — keep the defaults when the api has none
      vitals: vitals.length ? vitals : VITAL_DEFAULTS[kind],
    };
  });
}

export function appApiCalls(appId: string): AppApiCall[] {
  if (!isApi) return mockAppApiCalls(appId);
  return list(`${appPath(appId)}/api-calls${H1}`, (raw: RawAppApiCall[] | null) => (raw ?? []).map((c) => ({
    endpoint: mapEndpoint(c.endpoint), service: services.find((s) => s.id === c.service.id) ?? mapService(c.service),
    rpm: c.rpm ?? 0, errorRate: c.errorRate ?? 0, p95: c.p95 ?? 0,
  })).sort((a, b) => b.rpm - a.rpm));
}

/** Views / users / sessions / error rate over time for an app (optionally one page). Screens synthesize these today. */
export function appSeries(appId: string, range: TimeRange, view?: string): Record<'views' | 'users' | 'sessions' | 'errorRate', number[]> {
  const n = timestamps(range).length;
  if (isApi) {
    const set = seriesSet(`${appPath(appId)}/series${qs({ range, view })}`, range);
    return { views: pick(set, 'views', n), users: pick(set, 'users', n), sessions: pick(set, 'sessions', n), errorRate: pick(set, 'errorRate', n) };
  }
  const s = mockAppSummary(appId);
  return {
    views: series(`rum-views-${appId}${view ?? ''}`, range, { base: s.pageViewsPerMin, daily: 0.4 }),
    users: series(`rum-users-${appId}`, range, { base: s.activeUsers, daily: 0.4 }),
    sessions: series(`rum-sess-${appId}`, range, { base: s.sessions, daily: 0.4 }),
    errorRate: series(`rum-err-${appId}`, range, { base: s.errorRate, noise: 0.3 }),
  };
}

/** p75 of one vital (lcp, inp, cls, ttfb, fcp, cold_start, warm_start) over time. */
export function vitalSeries(appId: string, vital: string, range: TimeRange, view?: string): number[] {
  if (isApi) return pick(seriesSet(`${appPath(appId)}/vitals/${seg(vital)}/series${qs({ range, view })}`, range), 'p75', timestamps(range).length);
  const v = mockAppSummary(appId).vitals.find((x) => x.id === vital);
  return series(`rum-${vital}-${appId}`, range, { base: v?.value ?? 0, noise: 0.1 });
}

export interface AppError { fingerprint: string; type: string; message: string; view: string; count: number; sessions: number; crash: boolean; firstSeen: number; lastSeen: number; appVersion: string }
/** JS errors / crashes grouped by fingerprint. Mock: empty (the app screen builds its own sample errors). */
export function appErrors(appId: string): AppError[] {
  if (!isApi) return [];
  return list(`${appPath(appId)}/errors${H1}`, (raw: RawRumError[] | null) => raw ?? []);
}
