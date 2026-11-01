/**
 * Per-app RUM data. Any number of web / mobile apps report through the browser,
 * React Native and Flutter SDKs; each one is a "service" of type web | mobile.
 */
import { services, timestamps } from './apm';
import { qs, remote, seg } from './remote';
import {
  emptyPage, list, mapEndpoint, mapRumPages, mapService, mapSessions, mapVital, pick, seriesSet,
  type RawAppApiCall, type RawAppSummary, type RawRumError,
} from './api/apm';
import type { Endpoint, RumPage, Service, Session, TimeRange } from './types';

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

export interface AppApiCall { endpoint: Endpoint; service: Service; rpm: number; errorRate: number; p95: number }

/** Backend endpoints this app calls (from the app's traceparent on each request). */
/** Pages (web) or screens (mobile) of an app. Never empty (a zero "/" row while loading / without data). */
export function rumPagesFor(appId: string): RumPage[] {
  return remote(`${appPath(appId)}/pages${H1}`, [emptyPage()], mapRumPages);
}

export function sessionsFor(appId: string): Session[] {
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
  return list(`${appPath(appId)}/api-calls${H1}`, (raw: RawAppApiCall[] | null) => (raw ?? []).map((c) => ({
    endpoint: mapEndpoint(c.endpoint), service: services.find((s) => s.id === c.service.id) ?? mapService(c.service),
    rpm: c.rpm ?? 0, errorRate: c.errorRate ?? 0, p95: c.p95 ?? 0,
  })).sort((a, b) => b.rpm - a.rpm));
}

/** Views / users / sessions / error rate over time for an app (optionally one page). */
export function appSeries(appId: string, range: TimeRange, view?: string): Record<'views' | 'users' | 'sessions' | 'errorRate', number[]> {
  const n = timestamps(range).length;
  const set = seriesSet(`${appPath(appId)}/series${qs({ range, view })}`, range);
  return { views: pick(set, 'views', n), users: pick(set, 'users', n), sessions: pick(set, 'sessions', n), errorRate: pick(set, 'errorRate', n) };
}

/** p75 of one vital (lcp, inp, cls, ttfb, fcp, cold_start, warm_start) over time. */
export function vitalSeries(appId: string, vital: string, range: TimeRange, view?: string): number[] {
  return pick(seriesSet(`${appPath(appId)}/vitals/${seg(vital)}/series${qs({ range, view })}`, range), 'p75', timestamps(range).length);
}

export interface AppError { fingerprint: string; type: string; message: string; view: string; count: number; sessions: number; crash: boolean; firstSeen: number; lastSeen: number; appVersion: string }
/** JS errors / crashes grouped by fingerprint. */
export function appErrors(appId: string): AppError[] {
  return list(`${appPath(appId)}/errors${H1}`, (raw: RawRumError[] | null) => raw ?? []);
}

export interface AppErrorDetail { stack: string; symbolicated: boolean }
/** Latest stack of one error group (`/apps/{app}/errors/{fingerprint}`), symbolicated when the release has symbols. */
export function appErrorDetail(appId: string, fingerprint: string): AppErrorDetail | undefined {
  return remote<AppErrorDetail | undefined>(`${appPath(appId)}/errors/${seg(fingerprint)}${qs({ range: '24h' })}`, undefined,
    (r: { stack?: string | null; symbolicated?: boolean | null } | null) => (r ? { stack: r.stack ?? '', symbolicated: !!r.symbolicated } : undefined));
}
