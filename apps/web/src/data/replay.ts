/**
 * Session replay (ADR-021, .ai/contracts.md §23).
 *
 * Api mode: `GET /replays`, `GET /replays/{sessionId}`, `GET /replays/{sessionId}/segments/{n}`.
 * Mock mode: every mock RUM session with a replay gets a deterministic recording. Web apps get real rrweb 2.x events of a
 * small shop (FullSnapshot, mutations, mouse, scroll, masked input, a failed checkout); mobile apps get wireframe frames,
 * touches and keyboard events. Text and inputs are masked the way the SDK masks them by default.
 */
import { NOW, services } from './mock';
import { sessionsFor } from './frontend';
import { ApiError, api, isApi, qs, remote, remoteStatus, seg } from './remote';
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
  if (!isApi) return filterReplays(mockReplaysFor(appId), f);
  const path = `/replays${qs({ app: appName(appId), range: f.range ?? '24h', q: f.q?.trim(), hasErrors: f.hasErrors ? 'true' : undefined, minDurationS: f.minDurationS, limit: 100 })}`;
  return remote<ReplaySummary[]>(path, [], (raw: { replays?: RawSummary[] | null } | null) => (raw?.replays ?? []).map(mapReplaySummary));
}

/** Summary + segment index + RUM timeline of one replay. */
export function replayDetail(sessionId: string): ReplayDetailState {
  if (!isApi) {
    const m = mockRecording(sessionId);
    return { detail: { summary: m.summary, segments: m.segments.map(segInfo), timeline: m.timeline }, loading: false };
  }
  const path = `/replays/${seg(sessionId)}`;
  const detail = remote<ReplayDetail | undefined>(path, undefined, (raw: RawDetail) => mapDetail(raw));
  if (detail) return { detail, loading: false };
  const err = remoteStatus().errors.find((e) => e.path === path)?.error;
  if (err) return { loading: false, error: err, notFound: /not found|404/i.test(err) };
  return { loading: true };
}

/** One segment's events. Segments are fetched lazily by the player (first ones, then the rest). */
export async function fetchReplaySegment(sessionId: string, segment: number): Promise<ReplaySegmentPayload> {
  if (isApi) {
    try {
      return await api<ReplaySegmentPayload>(`/replays/${seg(sessionId)}/segments/${segment}`);
    } catch (e) {
      if (e instanceof ApiError && e.status === 503) throw new Error('replay storage (S3) is not configured on the server');
      if (e instanceof ApiError && e.status === 404) throw new Error(`segment ${segment} is missing or past the 30-day retention`);
      throw e;
    }
  }
  const s = mockRecording(sessionId).segments.find((x) => x.segment === segment);
  if (!s) throw new Error('segment not found');
  // simulate network latency so the lazy loading path is exercised in mock mode
  await new Promise((r) => setTimeout(r, 60 + (segment % 4) * 40));
  return s;
}

/** Does this RUM session have a recording? (`hasReplay` on `/apps/{app}/sessions` rows.) */
export function sessionHasReplay(appId: string, s: Session): boolean {
  if (isApi) return !!s.hasReplay;
  return mockHasReplay(appId, s);
}

/**
 * Replay moment of an error (`replay` on `/apps/{app}/errors/{fingerprint}`): the session and the ms offset of the error.
 * Mock: a recorded session of the app whose error matches the message (else any recorded session with an error).
 */
export function errorReplay(appId: string, fingerprint: string, message?: string): { sessionId: string; offsetMs: number } | undefined {
  if (isApi) {
    type R = { replay?: { sessionId: string; offsetMs: number } | null; samples?: { replay?: { sessionId: string; offsetMs: number } | null }[] | null };
    return remote<{ sessionId: string; offsetMs: number } | undefined>(`/apps/${seg(appName(appId))}/errors/${seg(fingerprint)}${qs({ range: '24h' })}`, undefined,
      (r: R | null) => r?.replay ?? r?.samples?.find((x) => x.replay)?.replay ?? undefined);
  }
  const recs = mockReplaysFor(appId).filter((r) => r.errors > 0).map((r) => mockRecording(r.sessionId));
  const match = recs.find((m) => m.timeline.some((t) => t.kind === 'error' && message && message.startsWith(t.label.slice(0, 24)))) ?? recs[0];
  const err = match?.timeline.find((t) => t.kind === 'error');
  return match && err ? { sessionId: match.summary.sessionId, offsetMs: err.ts - match.summary.startedAt } : undefined;
}

function filterReplays(rows: ReplaySummary[], f: ReplayFilter): ReplaySummary[] {
  const q = f.q?.trim().toLowerCase();
  return rows.filter((r) => (!f.hasErrors || r.errors > 0)
    && (!f.minDurationS || r.durationMs >= f.minDurationS * 1000)
    && (!q || [r.user.id, r.user.anonymousId, r.firstView, r.sessionId].some((v) => v?.toLowerCase().includes(q))));
}

// =====================================================================================================
// Mock
// =====================================================================================================
function h32(s: string) {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}
function rng(seed: number) {
  return () => {
    seed |= 0; seed = (seed + 0x6d2b79f5) | 0;
    let t = Math.imul(seed ^ (seed >>> 15), 1 | seed);
    t = (t + Math.imul(t ^ (t >>> 7), 61 | t)) ^ t;
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}
const hex = (seed: string, len: number) => {
  let out = '';
  let x = h32(seed);
  while (out.length < len) { x = Math.imul(x ^ (x >>> 13), 0x5bd1e995) >>> 0; out += x.toString(16).padStart(8, '0'); }
  return out.slice(0, len);
};
/** Default SDK text masking: every non-space character becomes `*` (rrweb) or `•` (wireframe). */
const maskStar = (s: string) => s.replace(/\S/g, '*');
const maskDot = (s: string) => s.replace(/\S/g, '•');

const WEB_ERROR_POOL = [
  { label: "TypeError: Cannot read properties of null (reading 'price')", stack: `TypeError: Cannot read properties of null (reading 'price')
    at Summary (src/components/Summary.tsx:41:28)
    at renderWithHooks (react-dom.production.js:1:18345)
    at updateFunctionComponent (react-dom.production.js:1:21233)` },
  { label: 'Unhandled rejection: request timeout after 15000ms', stack: `Error: request timeout after 15000ms
    at ApiClient.send (src/lib/api-client.ts:120:13)
    at async load (src/routes/loader.ts:58:5)` },
];
const MOBILE_ERROR_POOL = [
  { label: 'Unhandled promise rejection: Network request failed', stack: `TypeError: Network request failed
    at XMLHttpRequest.xhr.onerror (whatwg-fetch.js:612:18)
    at api.request (src/lib/api.ts:44:9)` },
  { label: 'Fatal: NullPointerException in ListAdapter.bind', stack: `java.lang.NullPointerException: Attempt to invoke virtual method on a null object reference
    at app.ui.ListAdapter.bind(ListAdapter.kt:88)
    at androidx.recyclerview.widget.RecyclerView$Adapter.onBindViewHolder(RecyclerView.java:7254)` },
];

interface MockRecording { summary: ReplaySummary; segments: ReplaySegmentPayload[]; timeline: TimelineItem[] }
const recordings = new Map<string, MockRecording>();

function mockHasReplay(appId: string, s: Session) {
  // the replay SDK samples sessions (default 10 %, 100 % with an error); the mock records most of them
  return s.errors > 0 || h32(`${appId}${s.id}`) % 4 !== 0;
}

function clientAppIds() { return services.filter((s) => s.type === 'web' || s.type === 'mobile').map((s) => s.id); }

function mockReplaysFor(appId: string): ReplaySummary[] {
  return sessionsFor(appId).filter((s) => mockHasReplay(appId, s)).map((s) => mockRecording(s.id, appId, s).summary)
    .sort((a, b) => b.startedAt - a.startedAt);
}

/** Recording for any session id: a mock RUM session when the id is known, otherwise a synthetic shop-web session. */
function mockRecording(sessionId: string, appIdHint?: string, sessionHint?: Session): MockRecording {
  const cached = recordings.get(sessionId);
  if (cached) return cached;
  let appId = appIdHint;
  let session = sessionHint;
  if (!session) {
    for (const id of clientAppIds()) {
      const s = sessionsFor(id).find((x) => x.id === sessionId);
      if (s) { appId = id; session = s; break; }
    }
  }
  const app = services.find((s) => s.id === (appId ?? 'shop-web')) ?? services.find((s) => s.id === 'shop-web')!;
  const r = rng(h32(sessionId));
  const s: Session = session ?? {
    id: sessionId, user: `anon-${hex(sessionId, 4)}`, identified: false, country: 'TR', device: 'Chrome · macOS',
    startedAt: NOW - Math.round(r() * 3_000_000), duration: 180, pages: 4, errors: 1, entry: '/', exit: '/checkout',
  };
  const mobile = app.type === 'mobile';
  const seed = h32(`${app.id}:${sessionId}`);
  const targetMs = Math.min(Math.max(s.duration, 75) * 1000, 12 * 60_000);
  const rec = mobile ? buildMobile(sessionId, app.id, seed, s, targetMs) : buildWeb(sessionId, app.id, seed, s, targetMs);
  recordings.set(sessionId, rec);
  return rec;
}

const segInfo = (p: ReplaySegmentPayload): ReplaySegmentInfo => ({
  segment: p.segment, startTs: p.start_ts, endTs: p.end_ts, events: p.events.length,
  // ≈ gzip ratio of rrweb JSON
  bytes: Math.round(JSON.stringify(p.events).length * 0.22), hasFullSnapshot: p.has_full_snapshot,
});

/** Split events into ≤ 5 s segments (SDK flush rule); idle stretches produce no segment. */
function toSegments(sessionId: string, format: ReplayFormat, events: (RrwebEvent | WireEvent)[], viewAt: (ts: number) => string): ReplaySegmentPayload[] {
  const tsOf = (e: RrwebEvent | WireEvent) => ('timestamp' in e ? e.timestamp : e.ts);
  const out: ReplaySegmentPayload[] = [];
  let cur: (RrwebEvent | WireEvent)[] = [];
  let start = 0;
  const flush = () => {
    if (!cur.length) return;
    const full = format === 'rrweb' ? cur.some((e) => (e as RrwebEvent).type === 2) : cur.some((e) => (e as WireEvent).type === 'screen');
    out.push({ session_id: sessionId, segment: out.length, start_ts: tsOf(cur[0]), end_ts: tsOf(cur[cur.length - 1]), format, has_full_snapshot: full, view_name: viewAt(tsOf(cur[0])), events: cur });
    cur = [];
  };
  for (const e of events) {
    const t = tsOf(e);
    if (cur.length && t - start >= 5000) flush();
    if (!cur.length) start = t;
    cur.push(e);
  }
  flush();
  return out;
}

function summaryOf(sessionId: string, appId: string, s: Session, platform: ReplayPlatform, timeline: TimelineItem[], segments: ReplaySegmentPayload[], start: number, end: number): ReplaySummary {
  const app = services.find((x) => x.id === appId)!;
  const views = timeline.filter((t) => t.kind === 'view');
  const isWeb = platform === 'web';
  // the RUM device string already carries the OS (and on desktop the browser); only add what is missing
  const [browser, os] = isWeb
    ? (s.device.includes('Chrome') ? [undefined, undefined] : s.device.includes('iPhone') ? ['Safari 26', 'iOS 26'] : ['Chrome 141', undefined])
    : [undefined, s.device.includes('·') ? undefined : platform === 'ios' ? 'iOS 26' : 'Android 16'];
  return {
    sessionId, app: app.name, platform, format: replayFormat(platform), startedAt: start, durationMs: end - start, segments: segments.length,
    user: s.identified ? { id: s.user, anonymousId: `anon-${hex(s.user, 4)}` } : { anonymousId: s.user },
    country: s.country, device: s.device, browser, os, appVersion: app.version,
    firstView: views[0]?.label ?? s.entry, views: views.length,
    errors: timeline.filter((t) => t.kind === 'error').length,
    clicks: timeline.filter((t) => t.kind === 'action' && !t.label.startsWith('Rage')).length + timeline.filter((t) => t.label.startsWith('Rage')).length * 4,
    rageClicks: timeline.filter((t) => t.label.startsWith('Rage')).length,
  };
}

// ---------------------------------------------------------------- web: rrweb shop
interface RrNode {
  type: number; id: number; tagName?: string; attributes?: Record<string, string>; childNodes?: RrNode[];
  textContent?: string; isStyle?: boolean; name?: string; publicId?: string; systemId?: string;
}

const SHOP_CSS = `*{box-sizing:border-box;margin:0;padding:0}
html,body{background:#f5f6fa}
body{font:14px/20px -apple-system,'Segoe UI',Roboto,Helvetica,Arial,sans-serif;color:#1c2333;min-height:980px}
.hdr{height:64px;background:#fff;border-bottom:1px solid #e3e6ee;display:flex;align-items:center;gap:28px;padding:0 32px}
.logo{font-weight:800;font-size:20px;color:#3b2ee6;letter-spacing:-.02em}
.nav{display:flex;gap:20px;color:#596176}
.search{flex:1;max-width:360px;height:36px;border:1px solid #d8dce6;border-radius:8px;padding:0 12px;background:#f8f9fc}
.cartbtn{margin-left:auto;height:36px;padding:0 14px;border:1px solid #d8dce6;border-radius:8px;background:#fff;display:flex;align-items:center;gap:8px}
.badge{min-width:20px;height:20px;border-radius:10px;background:#e3e6ee;color:#596176;font-size:12px;line-height:20px;text-align:center;padding:0 6px}
.badge.on{background:#3b2ee6;color:#fff}
.wrap{display:flex;gap:24px;padding:24px 32px}
.main{width:900px}
.h1{font-size:22px;line-height:32px;font-weight:700;height:32px;margin-bottom:16px}
.grid{display:grid;grid-template-columns:repeat(3,280px);gap:20px}
.card{background:#fff;border:1px solid #e3e6ee;border-radius:12px;height:320px;padding:12px}
.img{height:170px;border-radius:8px}
.title{margin-top:12px;height:20px;font-weight:600}
.price{margin-top:6px;height:20px;color:#3b2ee6;font-weight:700}
.add{margin-top:14px;height:36px;width:100%;border:0;border-radius:8px;background:#3b2ee6;color:#fff;font-weight:600;font-size:14px}
.aside{width:300px;background:#fff;border:1px solid #e3e6ee;border-radius:12px;padding:16px;align-self:flex-start}
.h2{font-size:16px;font-weight:700;height:24px;margin-bottom:12px}
.line{display:flex;justify-content:space-between;align-items:center;height:40px;border-bottom:1px solid #eef0f5}
.empty{height:24px;color:#8a91a3}
.total{display:flex;justify-content:space-between;height:32px;margin-top:8px;align-items:center;font-weight:700}
.checkout{margin-top:12px;height:40px;width:100%;border:0;border-radius:8px;background:#11182b;color:#fff;font-weight:600;font-size:14px}
.checkout.off{background:#c9cdd8}
.form{background:#fff;border:1px solid #e3e6ee;border-radius:12px;padding:24px;width:560px}
.f{height:62px;margin-bottom:14px}
.f label{display:block;height:16px;font-size:12px;line-height:16px;color:#596176;margin-bottom:6px}
.f input{height:40px;width:100%;border:1px solid #d8dce6;border-radius:8px;padding:0 12px;font-size:14px}
.pay{height:44px;width:100%;border:0;border-radius:8px;background:#3b2ee6;color:#fff;font-weight:600;font-size:15px}
.pay.busy{opacity:.6}
.err{background:#fdecec;color:#b42318;border:1px solid #f5c2c0;border-radius:8px;padding:12px;margin-bottom:14px}
.ok{background:#fff;border:1px solid #e3e6ee;border-radius:12px;padding:40px;width:560px;text-align:center}
.ok b{display:block;font-size:20px;line-height:28px;margin-bottom:8px;color:#0f7b47}`;

const PRODUCTS: [string, string, string][] = [
  ['Trail running shoe', '₺2.490', '#dbe4ff'], ['Merino crew sweater', '₺1.750', '#ffe3d6'], ['Canvas weekender bag', '₺1.290', '#d7f3e9'],
  ['Ceramic pour-over set', '₺890', '#fff1c2'], ['Wireless earbuds', '₺3.150', '#f3dcf5'], ['Linen throw blanket', '₺1.120', '#e2e6ee'],
];
const FIELDS: [string, string, number][] = [
  ['Full name', 'text', 14], ['Email', 'email', 21], ['Card number', 'text', 19], ['Expiry', 'text', 5], ['CVC', 'password', 3], ['Address', 'text', 27],
];
const VIEW_W = 1280;
const VIEW_H = 720;

function buildWeb(sessionId: string, appId: string, seed: number, s: Session, targetMs: number): MockRecording {
  const r = rng(seed);
  const start = s.startedAt;
  const withError = s.errors > 0;
  const err = WEB_ERROR_POOL[seed % WEB_ERROR_POOL.length];
  const events: RrwebEvent[] = [];
  const timeline: TimelineItem[] = [];
  let t = start;
  let view = '/';
  const ev = (type: number, data: unknown, at = t) => events.push({ type, data, timestamp: Math.round(at) });
  const inc = (data: Record<string, unknown>, at = t) => ev(3, data, at);
  const tl = (item: Omit<TimelineItem, 'ts' | 'viewName'> & { at?: number }) => {
    const { at, ...rest } = item;
    timeline.push({ ts: Math.round(at ?? t), viewName: view, ...rest });
  };
  const trace = (k: string) => hex(`${sessionId}${k}`, 32);

  // -------- snapshot
  let nid = 0;
  const N = () => ++nid;
  const E = (tagName: string, attributes: Record<string, string> = {}, childNodes: RrNode[] = []): RrNode => ({ type: 2, tagName, attributes, childNodes, id: N() });
  const T = (text: string, masked = true): RrNode => ({ type: 3, textContent: masked ? maskStar(text) : text, id: N() });

  const doc: RrNode = { type: 0, childNodes: [], id: N() };
  const doctype: RrNode = { type: 1, name: 'html', publicId: '', systemId: '', id: N() };
  const styleText: RrNode = { type: 3, textContent: SHOP_CSS, isStyle: true, id: N() };
  const head = E('head', {}, [E('meta', { charset: 'utf-8' }), E('title', {}, [T('Trendyshop · Home')]), E('style', {}, [styleText])]);
  const badgeText = T('0');
  const badge = E('span', { class: 'badge' }, [badgeText]);
  const hdr = E('header', { class: 'hdr' }, [
    E('div', { class: 'logo' }, [T('Trendyshop')]),
    E('nav', { class: 'nav' }, ['Women', 'Men', 'Home', 'Sale'].map((n) => E('a', { href: '#' }, [T(n)]))),
    E('input', { class: 'search', type: 'search', value: '' }),
    E('button', { class: 'cartbtn' }, [T('Cart'), badge]),
  ]);
  const h1Text = T('Popular this week');
  const h1 = E('div', { class: 'h1' }, [h1Text]);
  const addButtons: RrNode[] = [];
  const grid = E('div', { class: 'grid' }, PRODUCTS.map(([name, price, color]) => {
    const btn = E('button', { class: 'add' }, [T('Add to cart')]);
    addButtons.push(btn);
    return E('div', { class: 'card' }, [E('div', { class: 'img', style: `background:${color}` }), E('div', { class: 'title' }, [T(name)]), E('div', { class: 'price' }, [T(price)]), btn]);
  }));
  const main = E('main', { class: 'main' }, [h1, grid]);
  const lines = E('div', { class: 'lines' });
  const empty = E('div', { class: 'empty' }, [T('Your cart is empty')]);
  const totalText = T('₺0');
  const checkoutBtn = E('button', { class: 'checkout off' }, [T('Checkout')]);
  const aside = E('aside', { class: 'aside' }, [E('div', { class: 'h2' }, [T('Your cart')]), lines, empty, E('div', { class: 'total' }, [T('Total'), E('span', {}, [totalText])]), checkoutBtn]);
  const wrap = E('div', { class: 'wrap' }, [main, aside]);
  const body = E('body', {}, [hdr, wrap]);
  const html = E('html', { lang: 'en' }, [head, body]);
  doc.childNodes = [doctype, html];

  /** Parent-first flat list of adds for a subtree (the replayer builds added nodes without children). */
  const adds = (node: RrNode, parentId: number, nextId: number | null = null): unknown[] => {
    const kids = node.childNodes ?? [];
    const self = { parentId, nextId, node: { ...node, childNodes: [] } };
    return [self, ...kids.flatMap((k) => adds(k, node.id))];
  };
  const mutate = (m: { adds?: unknown[]; removes?: { parentId: number; id: number }[]; texts?: { id: number; value: string }[]; attributes?: { id: number; attributes: Record<string, string | null> }[] }, at = t) =>
    inc({ source: 0, adds: m.adds ?? [], removes: m.removes ?? [], texts: m.texts ?? [], attributes: m.attributes ?? [] }, at);

  // -------- input helpers
  let mouse = { x: 640, y: 360 };
  let scrollY = 0;
  const moveTo = (x: number, y: number, dur: number) => {
    const from = mouse;
    const steps = Math.max(2, Math.round(dur / 50));
    let batch: { x: number; y: number; id: number; timeOffset: number }[] = [];
    let batchAt = t;
    const wob = (r() - 0.5) * 80;
    for (let i = 1; i <= steps; i++) {
      const p = i / steps;
      const e = p < 0.5 ? 2 * p * p : 1 - Math.pow(-2 * p + 2, 2) / 2;
      const at = t + p * dur;
      const px = Math.round(from.x + (x - from.x) * e + Math.sin(p * Math.PI) * wob);
      const py = Math.round(from.y + (y - from.y) * e + Math.sin(p * Math.PI) * wob * 0.4);
      batch.push({ x: px, y: py, id: body.id, timeOffset: Math.round(at) });
      if (at - batchAt >= 500 || i === steps) {
        const stamp = Math.round(at);
        inc({ source: 1, positions: batch.map((b) => ({ ...b, timeOffset: b.timeOffset - stamp })) }, stamp);
        batch = [];
        batchAt = at;
      }
    }
    t += dur;
    mouse = { x, y };
  };
  const wander = (dur: number) => {
    const end = t + dur;
    while (t < end - 300) {
      const step = Math.min(end - t, 500 + r() * 900);
      moveTo(Math.round(80 + r() * (VIEW_W - 160)), Math.round(90 + r() * (VIEW_H - 140)), step);
      t += r() * 300;
    }
    t = Math.max(t, end);
  };
  const click = (id: number) => {
    inc({ source: 2, type: 1, id, x: mouse.x, y: mouse.y });
    inc({ source: 2, type: 0, id, x: mouse.x, y: mouse.y }, t + 70);
    inc({ source: 2, type: 2, id, x: mouse.x, y: mouse.y }, t + 75);
    t += 120;
  };
  const scrollTo = (y: number, dur: number) => {
    const from = scrollY;
    const steps = Math.max(2, Math.round(dur / 100));
    for (let i = 1; i <= steps; i++) inc({ source: 3, id: doc.id, x: 0, y: Math.round(from + ((y - from) * i) / steps) }, t + (i * dur) / steps);
    t += dur;
    scrollY = y;
  };
  const idle = (ms: number) => {
    // a twitch of the mouse right after the user goes idle, then nothing
    moveTo(mouse.x + 6, mouse.y + 4, 200);
    t += Math.max(0, ms - 200);
  };

  // -------- positions (viewport coordinates; layout is fixed in SHOP_CSS)
  const addPos = (i: number) => ({ x: 32 + (i % 3) * 300 + 140, y: 136 + Math.floor(i / 3) * 340 + 272 - scrollY });
  const checkoutPos = (lineCount: number) => ({ x: 956 + 150, y: 88 + 16 + 36 + (lineCount ? lineCount * 40 : 24) + 8 + 32 + 12 + 20 - scrollY });
  const fieldPos = (i: number) => ({ x: 32 + 24 + 200, y: 136 + 24 + i * 76 + 22 + 20 });

  // gaps of inactivity stretch the session to its RUM duration
  const ACTIVE = withError ? 84_000 : 64_000;
  const spare = Math.max(0, targetMs - ACTIVE);
  const gaps = [spare * 0.15, spare * 0.25, spare * 0.35, spare * 0.25];

  // -------- scene 0: home
  ev(4, { href: `https://shop.example.com/`, width: VIEW_W, height: VIEW_H });
  ev(2, { node: doc, initialOffset: { top: 0, left: 0 } });
  tl({ kind: 'view', label: '/', detail: 'Page load · navigation' });
  tl({ kind: 'resource', label: 'GET /api/products?list=popular', status: 200, durationMs: 184, traceId: trace('products'), detail: 'fetch · 200', at: t + 320 });
  tl({ kind: 'vital', label: 'LCP 1.9 s', detail: 'Largest Contentful Paint · good', at: t + 1900 });
  t += 400;
  wander(3800);
  scrollTo(300, 900);
  wander(2200);
  scrollTo(0, 700);
  idle(gaps[0]);

  // -------- scene 1–2: add two products
  const cart: number[] = [];
  const addToCart = (i: number) => {
    const p = addPos(i);
    moveTo(p.x, p.y, 900 + r() * 500);
    tl({ kind: 'action', label: 'Click · "Add to cart"', detail: 'button.add' });
    click(addButtons[i].id);
    tl({ kind: 'resource', label: 'POST /api/cart/items', status: 200, durationMs: 120 + Math.round(r() * 80), traceId: trace(`cart${i}`), detail: 'fetch · 200', at: t + 20 });
    t += 200;
    const [name, price] = PRODUCTS[i];
    const line = E('div', { class: 'line' }, [E('span', {}, [T(name)]), E('span', {}, [T(price)])]);
    cart.push(i);
    mutate({
      adds: adds(line, lines.id),
      removes: cart.length === 1 ? [{ parentId: aside.id, id: empty.id }] : [],
      texts: [{ id: badgeText.id, value: maskStar(String(cart.length)) }, { id: totalText.id, value: maskStar(cart.length === 1 ? price : '₺3.380') }],
      attributes: [{ id: badge.id, attributes: { class: 'badge on' } }, ...(cart.length === 1 ? [{ id: checkoutBtn.id, attributes: { class: 'checkout' } }] : [])],
    });
  };
  addToCart(1 + (seed % 2));
  wander(2400);
  addToCart(3 + (seed % 3));
  idle(gaps[1]);

  // -------- scene 3: checkout form
  const cp = checkoutPos(cart.length);
  moveTo(cp.x, cp.y, 1100);
  tl({ kind: 'action', label: 'Click · "Checkout"', detail: 'button.checkout' });
  click(checkoutBtn.id);
  view = '/checkout';
  tl({ kind: 'view', label: '/checkout', detail: 'Route change' });
  const inputs: RrNode[] = [];
  const payText = T('Pay now');
  const pay = E('button', { class: 'pay' }, [payText]);
  const form = E('form', { class: 'form' }, [...FIELDS.map(([label, type]) => {
    const input = E('input', { type, value: '' });
    inputs.push(input);
    return E('div', { class: 'f' }, [E('label', {}, [T(label)]), input]);
  }), pay]);
  mutate({ adds: adds(form, main.id), removes: [{ parentId: main.id, id: grid.id }], texts: [{ id: h1Text.id, value: maskStar('Checkout') }] }, t + 30);
  tl({ kind: 'resource', label: 'GET /api/shipping-options', status: 200, durationMs: 96, traceId: trace('ship'), detail: 'fetch · 200', at: t + 60 });
  t += 600;

  // -------- scene 4: fill the form (inputs are always masked)
  FIELDS.forEach(([, , len], i) => {
    const p = fieldPos(i);
    moveTo(p.x, p.y, 700 + r() * 400);
    click(inputs[i].id);
    inc({ source: 2, type: 5, id: inputs[i].id, x: p.x, y: p.y });
    for (let k = 1; k <= len; k++) {
      t += 110 + r() * 120;
      inc({ source: 5, id: inputs[i].id, text: '*'.repeat(k), isChecked: false, userTriggered: true });
    }
    t += 400 + r() * 500;
  });

  // -------- scene 5: pay
  let errorShown = false;
  const payPos = () => ({ x: 312, y: 136 + 24 + FIELDS.length * 76 + 22 - scrollY + (errorShown ? 62 : 0) });
  const payClick = (ok: boolean, key: string) => {
    const p = payPos();
    moveTo(p.x, p.y, 900);
    tl({ kind: 'action', label: 'Click · "Pay now"', detail: 'button.pay' });
    click(pay.id);
    mutate({ texts: [{ id: payText.id, value: maskStar('Processing…') }], attributes: [{ id: pay.id, attributes: { class: 'pay busy' } }] });
    const dur = ok ? 820 : 1900;
    tl({ kind: 'resource', label: 'POST /api/checkout', status: ok ? 201 : 502, durationMs: dur, traceId: trace(key), detail: ok ? 'fetch · 201 Created' : 'fetch · 502 Bad Gateway', at: t + 30 });
    t += dur + 60;
  };
  if (withError) {
    payClick(false, 'pay1');
    tl({ kind: 'error', label: err.label, detail: err.stack, fingerprint: `fp_${hex(err.label, 12)}`, at: t + 30 });
    const banner = E('div', { class: 'err' }, [T('Payment could not be completed. Please try again.')]);
    errorShown = true;
    mutate({ adds: adds(banner, form.id, pay.id), texts: [{ id: payText.id, value: maskStar('Pay now') }], attributes: [{ id: pay.id, attributes: { class: 'pay' } }] }, t + 40);
    t += 900;
    // rage: four quick clicks on the button while nothing happens
    const p = payPos();
    moveTo(p.x, p.y, 400);
    tl({ kind: 'action', label: 'Rage click · "Pay now" ×4', detail: 'button.pay · 4 clicks in 1.1 s' });
    for (let k = 0; k < 4; k++) { click(pay.id); t += 160; }
    idle(gaps[2]);
    mutate({ removes: [{ parentId: form.id, id: banner.id }] });
    errorShown = false;
  } else {
    idle(gaps[2]);
  }
  payClick(true, 'pay2');
  view = '/order/confirmed';
  tl({ kind: 'view', label: '/order/confirmed', detail: 'Route change' });
  const ok = E('div', { class: 'ok' }, [E('b', {}, [T('Thank you!')]), T(`Order #4${String(seed % 10000).padStart(4, '0')} is confirmed. A receipt is on its way.`)]);
  mutate({ adds: adds(ok, main.id), removes: [{ parentId: main.id, id: form.id }], texts: [{ id: h1Text.id, value: maskStar('Order confirmed') }, { id: badgeText.id, value: maskStar('0') }] });
  tl({ kind: 'resource', label: 'GET /api/orders/latest', status: 200, durationMs: 142, traceId: trace('order'), detail: 'fetch · 200', at: t + 80 });
  wander(3000);
  idle(gaps[3]);
  wander(1500);

  timeline.sort((a, b) => a.ts - b.ts);
  const end = events[events.length - 1].timestamp;
  const viewAt = (ts: number) => [...timeline].reverse().find((x) => x.kind === 'view' && x.ts <= ts)?.label ?? '/';
  const segments = toSegments(sessionId, 'rrweb', events, viewAt);
  return { summary: summaryOf(sessionId, appId, s, 'web', timeline, segments, start, end), segments, timeline };
}

// ---------------------------------------------------------------- mobile: wireframe
const MOBILE_SCREENS: Record<string, [string, string, string, string]> = {
  'shop-mobile': ['Home', 'ProductDetail', 'Cart', 'Checkout'],
  'courier-app': ['Deliveries', 'DeliveryDetail', 'ProofOfDelivery', 'ProofOfDelivery'],
};
const MW = 390;
const MH = 844;
const C = {
  surface: '#ffffff', page: '#f5f6fa', line: '#e6e8ef', ink: '#1c2333', sub: '#7a8194', brand: '#3b2ee6', field: '#f1f3f8',
  imgs: ['#dbe4ff', '#ffe3d6', '#d7f3e9', '#fff1c2', '#f3dcf5', '#e2e6ee'], errBg: '#fdecec', errBorder: '#f5c2c0', errInk: '#b42318',
};

function buildMobile(sessionId: string, appId: string, seed: number, s: Session, targetMs: number): MockRecording {
  const r = rng(seed);
  const start = s.startedAt;
  const withError = s.errors > 0;
  const err = MOBILE_ERROR_POOL[seed % MOBILE_ERROR_POOL.length];
  const platform: ReplayPlatform = s.device.includes('iPhone') || s.device.includes('iOS') ? 'ios' : 'android';
  const [home, detail, cartName, checkout] = MOBILE_SCREENS[appId] ?? MOBILE_SCREENS['shop-mobile'];
  const events: WireEvent[] = [];
  const timeline: TimelineItem[] = [];
  let t = start;
  let view = home;
  let id = 0;
  const trace = (k: string) => hex(`${sessionId}${k}`, 32);
  const tl = (item: Omit<TimelineItem, 'ts' | 'viewName'> & { at?: number }) => {
    const { at, ...rest } = item;
    timeline.push({ ts: Math.round(at ?? t), viewName: view, ...rest });
  };
  const n = (kind: WireNodeKind, x: number, y: number, w: number, h: number, o: Partial<WireNode> = {}): WireNode => ({ id: ++id, kind, x, y, w, h, ...o });
  const txt = (x: number, y: number, w: number, h: number, text: string, o: Partial<WireNode> = {}) => n('text', x, y, w, h, { text: maskDot(text), masked: true, fg: C.ink, ...o });

  const bg = (): WireNode => n('view', 0, 0, MW, MH, { bg: C.page });
  /** Header (and tab bar) drawn last so scrolled content slides under them. */
  const chrome = (title: string, back: boolean, tabs: boolean): WireNode[] => [
    n('view', 0, 0, MW, 99, { bg: C.surface, border: C.line }),
    ...(back ? [n('icon', 16, 63, 24, 24, { fg: C.ink })] : []),
    txt(back ? 120 : 16, 64, 150, 22, title, { fg: C.ink }),
    n('icon', 350, 63, 24, 24, { fg: C.ink }),
    ...(tabs ? [
      n('view', 0, 778, MW, 66, { bg: C.surface, border: C.line }),
      ...[49, 146, 244, 341].flatMap((cx, i) => [n('icon', cx - 12, 788, 24, 24, { fg: i === 0 ? C.brand : C.sub }), txt(cx - 20, 816, 40, 12, ['Home', 'Search', 'Cart', 'Me'][i], { fg: i === 0 ? C.brand : C.sub })]),
    ] : []),
  ];
  const clipBody: [number, number, number, number] = [0, 99, MW, 679];
  const homeFrame = (offset: number): WireNode[] => {
    const body: WireNode[] = [
      n('input', 16, 111 - offset, 358, 40, { bg: C.field, radius: 10, text: maskDot('Search products'), masked: true, fg: C.sub, clip: clipBody }),
      n('image', 16, 163 - offset, 358, 140, { bg: C.imgs[0], radius: 14, clip: clipBody }),
      txt(16, 319 - offset, 160, 20, 'Popular this week', { clip: clipBody }),
    ];
    for (let i = 0; i < 8; i++) {
      const y = 351 + i * 104 - offset;
      if (y > 778 || y + 96 < 99) continue;
      body.push(
        n('view', 16, y, 358, 96, { bg: C.surface, radius: 12, border: C.line, clip: clipBody }),
        n('image', 28, y + 12, 72, 72, { bg: C.imgs[i % C.imgs.length], radius: 8, clip: clipBody }),
        txt(112, y + 16, 170, 18, 'Product name here', { clip: clipBody }),
        txt(112, y + 40, 110, 14, 'Short subtitle', { fg: C.sub, clip: clipBody }),
        txt(112, y + 62, 64, 18, '₺1.290', { fg: C.brand, clip: clipBody }),
        n('button', 300, y + 56, 62, 30, { bg: C.brand, radius: 8, fg: C.surface, text: maskDot('Add'), masked: true, clip: clipBody }),
      );
    }
    return [bg(), ...body, ...chrome(appId === 'courier-app' ? 'Deliveries' : 'Trendyshop', false, true)];
  };
  const detailFrame = (added: boolean): WireNode[] => [
    bg(), ...chrome('Product', true, false),
    n('image', 0, 99, MW, 300, { bg: C.imgs[1] }),
    txt(16, 415, 260, 26, 'Merino crew sweater', { fg: C.ink }),
    txt(16, 449, 80, 22, '₺1.750', { fg: C.brand }),
    txt(16, 487, 358, 16, 'Soft merino wool knit with a relaxed fit and ribbed hems.', { fg: C.sub }),
    txt(16, 509, 300, 16, 'Machine washable on wool cycle. Made in Portugal.', { fg: C.sub }),
    ...['S', 'M', 'L', 'XL'].map((sz, i) => n('button', 16 + i * 64, 545, 52, 36, { bg: i === 1 ? C.ink : C.surface, border: C.line, radius: 8, fg: i === 1 ? C.surface : C.ink, text: maskDot(sz), masked: true })),
    n('view', 0, 744, MW, 100, { bg: C.surface, border: C.line }),
    n('button', 16, 760, 358, 52, { bg: added ? C.ink : C.brand, radius: 12, fg: C.surface, text: maskDot(added ? 'Added to cart' : 'Add to cart'), masked: true }),
  ];
  const cartFrame = (): WireNode[] => [
    bg(), ...chrome('Cart', true, false),
    ...[0, 1].flatMap((i) => [
      n('view', 16, 115 + i * 104, 358, 96, { bg: C.surface, radius: 12, border: C.line }),
      n('image', 28, 127 + i * 104, 72, 72, { bg: C.imgs[i + 1], radius: 8 }),
      txt(112, 131 + i * 104, 170, 18, 'Product name here'),
      txt(112, 177 + i * 104, 64, 18, '₺1.750', { fg: C.brand }),
      n('switch', 316, 170 + i * 104, 46, 28, { checked: i === 0 }),
    ]),
    n('view', 0, 744, MW, 100, { bg: C.surface, border: C.line }),
    txt(16, 712, 60, 20, 'Total'), txt(290, 712, 84, 20, '₺3.040'),
    n('button', 16, 760, 358, 52, { bg: C.ink, radius: 12, fg: C.surface, text: maskDot('Checkout'), masked: true }),
  ];
  const checkoutFrame = (typed: number[], keyboard: boolean, error: boolean, busy: boolean): WireNode[] => [
    bg(), ...chrome('Checkout', true, false),
    ...['Full name', 'Phone', 'Address', 'Card number'].flatMap((label, i) => [
      txt(16, 115 + i * 76, 100, 14, label, { fg: C.sub }),
      n('input', 16, 135 + i * 76, 358, 44, { bg: C.surface, border: C.line, radius: 10, fg: C.ink, masked: true, text: '•'.repeat(typed[i] ?? 0) }),
    ]),
    ...(error ? [n('view', 16, 430, 358, 56, { bg: C.errBg, border: C.errBorder, radius: 12 }), txt(32, 449, 300, 18, 'Something went wrong. Try again.', { fg: C.errInk })] : []),
    ...(keyboard ? [] : [
      n('view', 0, 744, MW, 100, { bg: C.surface, border: C.line }),
      n('button', 16, 760, 358, 52, { bg: C.brand, radius: 12, fg: C.surface, text: maskDot(busy ? 'Placing order…' : 'Place order'), masked: true, alpha: busy ? 0.6 : 1 }),
    ]),
  ];
  const doneFrame = (): WireNode[] => [
    bg(), ...chrome('Order', false, true),
    n('icon', 171, 260, 48, 48, { fg: C.brand }),
    txt(95, 330, 200, 26, 'Order confirmed'),
    txt(60, 368, 270, 16, 'We will notify you when it ships.', { fg: C.sub }),
    n('button', 16, 420, 358, 48, { radius: 12, fg: C.ink, text: maskDot('Track order'), masked: true }),
  ];

  const frame = (nodes: WireNode[], at = t) => events.push({ type: 'frame', ts: Math.round(at), nodes });
  const screen = (name: string, at = t) => events.push({ type: 'screen', ts: Math.round(at), width: MW, height: MH, density: 3, name });
  const tap = (x: number, y: number) => {
    events.push({ type: 'touch', ts: Math.round(t), x, y, phase: 'down' });
    events.push({ type: 'touch', ts: Math.round(t + 90), x, y, phase: 'up' });
    t += 160;
  };
  const scroll = (from: number, to: number, render: (offset: number) => WireNode[]) => {
    const dur = 700;
    const dy = to - from;
    const y0 = 600;
    events.push({ type: 'touch', ts: Math.round(t), x: 200, y: y0, phase: 'down' });
    for (let k = 1; k <= 7; k++) events.push({ type: 'touch', ts: Math.round(t + k * 100), x: 200 + Math.round(r() * 6), y: Math.round(y0 - (dy * k) / 7), phase: 'move' });
    events.push({ type: 'touch', ts: Math.round(t + dur + 20), x: 200, y: Math.round(y0 - dy), phase: 'up' });
    // ≤ 2 frames / s while the list moves, plus the settled frame
    frame(render(Math.round(from + dy * 0.45)), t + 350);
    frame(render(to), t + dur + 200);
    t += dur + 600;
  };
  const ACTIVE = withError ? 70_000 : 56_000;
  const spare = Math.max(0, targetMs - ACTIVE);
  const gaps = [spare * 0.2, spare * 0.3, spare * 0.3, spare * 0.2];

  // home
  screen(home);
  frame(homeFrame(0));
  tl({ kind: 'view', label: home, detail: 'App launch · cold start' });
  tl({ kind: 'vital', label: 'Cold start 1.4 s', detail: 'App launched from scratch until first screen', at: t + 1400 });
  tl({ kind: 'resource', label: 'GET /v1/items?list=popular', status: 200, durationMs: 212, traceId: trace('items'), detail: 'fetch · 200', at: t + 600 });
  t += 2500;
  scroll(0, 260, homeFrame);
  t += 1800;
  scroll(260, 520, homeFrame);
  t += 1500 + gaps[0];

  // product detail
  tap(150, 351 + 4 * 104 - 520 + 40);
  view = detail;
  tl({ kind: 'action', label: 'Tap · product row', detail: 'ProductRow' });
  screen(detail, t + 40);
  frame(detailFrame(false), t + 60);
  tl({ kind: 'view', label: detail, detail: 'Screen' });
  tl({ kind: 'resource', label: 'GET /v1/items/:id', status: 200, durationMs: 138, traceId: trace('item'), detail: 'fetch · 200', at: t + 80 });
  t += 3000;
  tap(16 + 64 + 26, 563);
  t += 1600;
  tap(195, 786);
  tl({ kind: 'action', label: 'Tap · "Add to cart"', detail: 'Button' });
  frame(detailFrame(true), t + 100);
  tl({ kind: 'resource', label: 'POST /v1/cart/items', status: 200, durationMs: 164, traceId: trace('cart'), detail: 'fetch · 200', at: t + 40 });
  t += 2200 + gaps[1];

  // cart
  tap(362, 75);
  view = cartName;
  tl({ kind: 'action', label: 'Tap · cart icon', detail: 'IconButton' });
  screen(cartName, t + 30);
  frame(cartFrame(), t + 50);
  tl({ kind: 'view', label: cartName, detail: 'Screen' });
  t += 3500;
  tap(195, 786);
  tl({ kind: 'action', label: 'Tap · "Checkout"', detail: 'Button' });

  // checkout: keyboard + masked input
  view = checkout;
  screen(checkout, t + 30);
  const typed = [0, 0, 0, 0];
  frame(checkoutFrame(typed, false, false, false), t + 50);
  tl({ kind: 'view', label: checkout, detail: 'Screen' });
  t += 1500;
  const lens = [14, 11, 26, 16];
  lens.forEach((len, i) => {
    tap(195, 157 + i * 76);
    if (i === 0) { events.push({ type: 'keyboard', ts: Math.round(t), visible: true }); frame(checkoutFrame(typed, true, false, false), t + 20); }
    for (let k = 1; k <= len; k += 4) {
      t += 500;
      typed[i] = Math.min(len, k + 3);
      frame(checkoutFrame(typed, true, false, false));
    }
    t += 600;
  });
  events.push({ type: 'keyboard', ts: Math.round(t), visible: false });
  frame(checkoutFrame(typed, false, false, false), t + 20);
  t += 1200 + gaps[2];
  const place = (ok: boolean, key: string) => {
    tap(195, 786);
    tl({ kind: 'action', label: 'Tap · "Place order"', detail: 'Button' });
    frame(checkoutFrame(typed, false, false, true), t + 40);
    const dur = ok ? 760 : 15000;
    tl({ kind: 'resource', label: 'POST /v1/orders', status: ok ? 201 : 0, durationMs: dur, traceId: trace(key), detail: ok ? 'fetch · 201 Created' : 'fetch · network error', at: t + 30 });
    t += ok ? dur + 60 : 4200;
  };
  if (withError) {
    place(false, 'o1');
    tl({ kind: 'error', label: err.label, detail: err.stack, fingerprint: `fp_${hex(err.label, 12)}`, at: t });
    frame(checkoutFrame(typed, false, true, false), t + 50);
    t += 2600;
    tap(195, 786);
    tap(195, 786);
    tap(195, 786);
    tl({ kind: 'action', label: 'Rage tap · "Place order" ×3', detail: 'Button · 3 taps in 0.5 s' });
    t += 1500 + gaps[3];
    frame(checkoutFrame(typed, false, false, false));
    t += 800;
  }
  place(true, 'o2');
  view = 'OrderConfirmed';
  screen('OrderConfirmed', t + 20);
  frame(doneFrame(), t + 40);
  tl({ kind: 'view', label: 'OrderConfirmed', detail: 'Screen' });
  t += 3000;
  tap(195, 444);
  t += 400;
  frame(doneFrame(), t);

  events.sort((a, b) => a.ts - b.ts);
  timeline.sort((a, b) => a.ts - b.ts);
  const end = events[events.length - 1].ts;
  const viewAt = (ts: number) => [...timeline].reverse().find((x) => x.kind === 'view' && x.ts <= ts)?.label ?? home;
  const segments = toSegments(sessionId, 'wireframe', events, viewAt);
  return { summary: summaryOf(sessionId, appId, s, platform, timeline, segments, start, end), segments, timeline };
}
