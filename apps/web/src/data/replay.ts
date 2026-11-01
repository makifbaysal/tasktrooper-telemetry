/**
 * Session replay (ADR-021, .ai/contracts.md §23).
 *
 * `GET /replays`, `GET /replays/{sessionId}`, `GET /replays/{sessionId}/segments/{n}`.
 */
import { services } from './apm';
import { ApiError, api, qs, remote, remoteStatus, seg } from './remote';
import type { Session, TimeRange } from './types';

// ---------------------------------------------------------------- types (contracts §23)
export type ReplayPlatform = 'web' | 'android' | 'ios';
export type ReplayFormat = 'rrweb' | 'wireframe';

export interface ReplaySummary {
  sessionId: string;
  /** app (service) name */
  app: string;
  platform: ReplayPlatform;
  /** rrweb (web) | wireframe (mobile); derived from platform when the api omits it */
  format: ReplayFormat;
  /** unix ms */
  startedAt: number;
  durationMs: number;
  segments: number;
  user: { id?: string; anonymousId: string };
  country?: string;
  device?: string;
  browser?: string;
  os?: string;
  appVersion?: string;
  firstView: string;
  views: number;
  errors: number;
  clicks: number;
  rageClicks: number;
}

export interface ReplaySegmentInfo { segment: number; startTs: number; endTs: number; events: number; bytes: number; hasFullSnapshot: boolean }

export type TimelineKind = 'view' | 'action' | 'error' | 'resource' | 'vital';
export interface TimelineItem {
  /** unix ms */
  ts: number;
  kind: TimelineKind;
  label: string;
  detail?: string;
  viewName?: string;
  traceId?: string;
  fingerprint?: string;
  /** resource: HTTP status */
  status?: number;
  durationMs?: number;
}

export interface ReplayDetail { summary: ReplaySummary; segments: ReplaySegmentInfo[]; timeline: TimelineItem[] }
export interface ReplayDetailState { detail?: ReplayDetail; loading: boolean; error?: string; notFound?: boolean }

/** rrweb 2.x event as recorded by `@rrweb/record` (opaque to us except type/timestamp). */
export interface RrwebEvent { type: number; data: unknown; timestamp: number }

export type WireNodeKind = 'view' | 'text' | 'image' | 'button' | 'input' | 'switch' | 'webview' | 'list' | 'icon';
export interface WireNode {
  id: number;
  x: number; y: number; w: number; h: number;
  kind: WireNodeKind;
  bg?: string; fg?: string; radius?: number; border?: string;
  text?: string; masked?: boolean; alpha?: number;
  clip?: [number, number, number, number];
  /** Toggle state, `switch` nodes only. */
  checked?: boolean;
}
export type WireEvent =
  | { type: 'screen'; ts: number; width: number; height: number; density: number; name: string }
  | { type: 'frame'; ts: number; nodes: WireNode[] }
  | { type: 'touch'; ts: number; x: number; y: number; phase: 'down' | 'move' | 'up' }
  | { type: 'keyboard'; ts: number; visible: boolean };

/** One segment as stored (`POST /v1/replay` body, served back by `GET …/segments/{n}`). */
export interface ReplaySegmentPayload {
  session_id: string;
  segment: number;
  start_ts: number;
  end_ts: number;
  format: ReplayFormat;
  has_full_snapshot: boolean;
  view_name?: string;
  events: (RrwebEvent | WireEvent)[];
}

export const replayFormat = (p: ReplayPlatform): ReplayFormat => (p === 'web' ? 'rrweb' : 'wireframe');
export const replayUrl = (sessionId: string, offsetMs?: number) =>
  `/replays/${encodeURIComponent(sessionId)}${offsetMs !== undefined ? `?t=${Math.max(0, Math.round(offsetMs))}` : ''}`;

// ---------------------------------------------------------------- api mapping
interface RawSummary {
  sessionId: string; app: string; platform: string; format?: string | null; startedAt: number; durationMs: number; segments: number;
  user?: { id?: string | null; anonymousId?: string | null } | null; country?: string | null; device?: string | null; browser?: string | null;
  os?: string | null; appVersion?: string | null; firstView?: string | null; views?: number; errors?: number; clicks?: number; rageClicks?: number;
}
interface RawDetail {
  summary: RawSummary;
  segments?: { segment: number; startTs: number; endTs: number; events?: number; bytes?: number; hasFullSnapshot?: boolean }[] | null;
  timeline?: (Omit<TimelineItem, 'kind'> & { kind: string })[] | null;
}
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
const opt = (v: string | null | undefined) => v || undefined;
const PLATFORMS: ReplayPlatform[] = ['web', 'android', 'ios'];
const KINDS: TimelineKind[] = ['view', 'action', 'error', 'resource', 'vital'];

export function mapReplaySummary(r: RawSummary): ReplaySummary {
  const platform: ReplayPlatform = PLATFORMS.includes(r.platform as ReplayPlatform) ? (r.platform as ReplayPlatform) : 'web';
  return {
    sessionId: r.sessionId, app: r.app, platform, format: r.format === 'rrweb' || r.format === 'wireframe' ? r.format : replayFormat(platform),
    startedAt: num(r.startedAt), durationMs: num(r.durationMs), segments: num(r.segments),
    user: { id: opt(r.user?.id), anonymousId: r.user?.anonymousId ?? '' },
    country: opt(r.country), device: opt(r.device), browser: opt(r.browser), os: opt(r.os), appVersion: opt(r.appVersion),
    firstView: r.firstView ?? '', views: num(r.views), errors: num(r.errors), clicks: num(r.clicks), rageClicks: num(r.rageClicks),
  };
}
function mapDetail(r: RawDetail): ReplayDetail {
  return {
    summary: mapReplaySummary(r.summary),
    segments: (r.segments ?? []).map((s) => ({ segment: num(s.segment), startTs: num(s.startTs), endTs: num(s.endTs), events: num(s.events), bytes: num(s.bytes), hasFullSnapshot: !!s.hasFullSnapshot }))
      .sort((a, b) => a.segment - b.segment),
    timeline: (r.timeline ?? []).filter((t) => KINDS.includes(t.kind as TimelineKind)).map((t) => ({ ...t, kind: t.kind as TimelineKind, ts: num(t.ts) }))
      .sort((a, b) => a.ts - b.ts),
  };
}

const appName = (appId: string) => services.find((s) => s.id === appId)?.name ?? appId;
/** Service id for a replay's app name (falls back to the name). */
export const replayAppId = (app: string) => services.find((s) => s.name === app || s.id === app)?.id ?? app;

// ---------------------------------------------------------------- public api
export interface ReplayFilter { q?: string; hasErrors?: boolean; minDurationS?: number; range?: TimeRange }

/** Recorded sessions of one client app, newest first. */
export function replaysFor(appId: string, f: ReplayFilter = {}): ReplaySummary[] {
  const path = `/replays${qs({ app: appName(appId), range: f.range ?? '24h', q: f.q?.trim(), hasErrors: f.hasErrors ? 'true' : undefined, minDurationS: f.minDurationS, limit: 100 })}`;
  return remote<ReplaySummary[]>(path, [], (raw: { replays?: RawSummary[] | null } | null) => (raw?.replays ?? []).map(mapReplaySummary));
}

/** Summary + segment index + RUM timeline of one replay. */
export function replayDetail(sessionId: string): ReplayDetailState {
  const path = `/replays/${seg(sessionId)}`;
  const detail = remote<ReplayDetail | undefined>(path, undefined, (raw: RawDetail) => mapDetail(raw));
  if (detail) return { detail, loading: false };
  const err = remoteStatus().errors.find((e) => e.path === path)?.error;
  if (err) return { loading: false, error: err, notFound: /not found|404/i.test(err) };
  return { loading: true };
}

/** One segment's events. Segments are fetched lazily by the player (first ones, then the rest). */
export async function fetchReplaySegment(sessionId: string, segment: number): Promise<ReplaySegmentPayload> {
  try {
    return await api<ReplaySegmentPayload>(`/replays/${seg(sessionId)}/segments/${segment}`);
  } catch (e) {
    if (e instanceof ApiError && e.status === 503) throw new Error('replay storage (S3) is not configured on the server');
    if (e instanceof ApiError && e.status === 404) throw new Error(`segment ${segment} is missing or past the 30-day retention`);
    throw e;
  }
}

/** Does this RUM session have a recording? (`hasReplay` on `/apps/{app}/sessions` rows.) */
export function sessionHasReplay(_appId: string, s: Session): boolean {
  return !!s.hasReplay;
}

/**
 * Replay moment of an error (`replay` on `/apps/{app}/errors/{fingerprint}`): the session and the ms offset of the error.
 */
export function errorReplay(appId: string, fingerprint: string, _message?: string): { sessionId: string; offsetMs: number } | undefined {
  type R = { replay?: { sessionId: string; offsetMs: number } | null; samples?: { replay?: { sessionId: string; offsetMs: number } | null }[] | null };
  return remote<{ sessionId: string; offsetMs: number } | undefined>(`/apps/${seg(appName(appId))}/errors/${seg(fingerprint)}${qs({ range: '24h' })}`, undefined,
    (r: R | null) => r?.replay ?? r?.samples?.find((x) => x.replay)?.replay ?? undefined);
}
