// Per-service log lines. Every service (backend, worker, web, mobile) ships logs through its SDK,
// so the service page can show its own logs (api: GET /logs, /logs/series, /logs/patterns).
import type { LogLine, TimeRange } from './types';
import { logs as baseLogs, services, timestamps } from './apm';
import { api, qs, registerHydrator, remote } from './remote';
import { level, mapLogPage, pick, seriesSet, type RawLogPage, type RawLogPattern } from './api/apm';

/** Every log line in the project, newest first: the newest lines of the last hour, refreshed with the lists. */
export const allLogs: LogLine[] = [];

const LIMIT = 500;
// one request fills both exported line lists in place (`logs` is the short project-wide list in apm.ts)
registerHydrator(async () => {
  const lines = mapLogPage(await api<RawLogPage>(`/logs${qs({ range: '1h', limit: LIMIT })}`));
  allLogs.splice(0, allLogs.length, ...lines);
  baseLogs.splice(0, baseLogs.length, ...lines.slice(0, 120));
});

/** Stable per-service arrays: screens memoize the returned array, so api responses are copied into it in place. */
const perService = new Map<string, LogLine[]>();

/** Log lines emitted by one service (looked up by service id). */
export function logsFor(serviceId: string): LogLine[] {
  const s = services.find((x) => x.id === serviceId);
  const name = s?.name ?? serviceId;
  let out = perService.get(name);
  if (!out) {
    const arr = allLogs.filter((l) => l.service === name);
    out = arr;
    perService.set(name, arr);
  }
  const target = out;
  remote(`/logs${qs({ range: '1h', service: name, limit: LIMIT })}`, undefined, (raw: RawLogPage) => {
    const lines = mapLogPage(raw);
    target.splice(0, target.length, ...lines);
    return lines;
  });
  return out;
}

export interface LogQuery {
  serviceId?: string; levels?: LogLine['level'][]; q?: string; traceId?: string; route?: string; limit?: number; /** Vercel drain source */ source?: string;
  /** Kubernetes pod / namespace (attrs k8s.pod.name / k8s.namespace.name; contracts §22) */
  pod?: string; namespace?: string;
}
const svcName = (id?: string) => (id ? services.find((x) => x.id === id)?.name ?? id : undefined);
const logQs = (range: TimeRange, f: LogQuery) =>
  qs({ range, service: svcName(f.serviceId), level: f.levels?.join(','), q: f.q, traceId: f.traceId, route: f.route, source: f.source, pod: f.pod, namespace: f.namespace });

/** Server-side log search (full-text `q`, levels, trace id). */
export function searchLogs(range: TimeRange, f: LogQuery = {}): LogLine[] {
  return remote(`/logs${logQs(range, f)}&limit=${f.limit ?? 200}`, [], mapLogPage);
}

/** Log volume per level over time (error / warn / info / debug). */
export function logVolumeSeries(range: TimeRange, f: LogQuery = {}): Record<LogLine['level'], number[]> {
  const n = timestamps(range).length;
  const set = seriesSet(`/logs/series${logQs(range, f)}`, range);
  return { error: pick(set, 'error', n), warn: pick(set, 'warn', n), info: pick(set, 'info', n), debug: pick(set, 'debug', n) };
}

export interface LogPattern { id: string; pattern: string; level: LogLine['level']; services: string[]; count: number; last: number }
/** Drain-style log templates. */
export function logPatterns(range: TimeRange, f: LogQuery = {}): LogPattern[] {
  return remote(`/logs/patterns${logQs(range, f)}`, [], (raw: RawLogPattern[] | null) => (raw ?? []).map((p) => ({
    id: p.id, pattern: p.pattern, level: level(p.level),
    services: p.services ?? [], count: p.count, last: p.last,
  })));
}
