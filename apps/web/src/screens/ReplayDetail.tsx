/**
 * Session replay player (`/replays/:sessionId?t=<ms>`, ADR-021, contracts §23).
 * Web: rrweb DOM replay; mobile: wireframe SVG. Segments load lazily: the first ones, then the rest in order.
 */
import { useCallback, useEffect, useMemo, useRef, useState, type ReactNode } from 'react';
import { Link, useParams, useSearchParams } from 'react-router';
import {
  AlertTriangle, ArrowRight, Eye, FastForward, Gauge, Globe, Loader2, Lock, MousePointerClick, Pause, Play, RotateCcw, UserRound,
} from 'lucide-react';
import { Badge, Button, Card, CodeBlock, Drawer, DrawerSection, EmptyState, KeyValues, PageHeader, Segmented, StatusBadge, Toggle } from '@/components/ui';
import { ReplayPlayer, WireframePlayer } from '@/components/replay';
import { useApp } from '@/components/layout/context';
import {
  fetchReplaySegment, replayAppId, replayDetail, replayFormat, services,
  type ReplayDetail, type ReplaySegmentPayload, type RrwebEvent, type TimelineItem, type WireEvent,
} from '@/data';
import { ago, dateTime, duration, ms as fmtMs } from '@/lib/format';
import { categoryOf, traceUrl } from './shared';
import '@/styles/replay.css';

/** mm:ss (h:mm:ss over an hour) for a ms offset. */
export function clockOffset(v: number) {
  const s = Math.max(0, Math.floor(v / 1000));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const ss = String(s % 60).padStart(2, '0');
  return h ? `${h}:${String(m).padStart(2, '0')}:${ss}` : `${m}:${ss}`;
}

type MarkerKind = 'view' | 'action' | 'error' | 'failed';
const MARKER: Record<MarkerKind, { label: string; color: string }> = {
  view: { label: 'Page view', color: 'var(--series-1)' },
  action: { label: 'Click / tap', color: 'var(--series-3)' },
  error: { label: 'Error', color: 'var(--series-8)' },
  failed: { label: 'Failed request', color: 'var(--series-2)' },
};
const isFailed = (i: TimelineItem) => i.kind === 'resource' && (i.status === 0 || (i.status ?? 0) >= 400);
const markerOf = (i: TimelineItem): MarkerKind | null =>
  i.kind === 'view' ? 'view' : i.kind === 'action' ? 'action' : i.kind === 'error' ? 'error' : isFailed(i) ? 'failed' : null;
const KIND_ICON: Record<TimelineItem['kind'], typeof Eye> = { view: Eye, action: MousePointerClick, error: AlertTriangle, resource: Globe, vital: Gauge };

const SPEEDS = ['1', '2', '4', '8'] as const;
type Speed = (typeof SPEEDS)[number];
type ListFilter = 'all' | 'errors' | 'network' | 'actions';

// ---------------------------------------------------------------- screen
export function ReplayDetailScreen() {
  const { sessionId = '' } = useParams();
  const [params] = useSearchParams();
  useApp(); // re-render when api data lands
  const st = replayDetail(sessionId);
  const t = Number(params.get('t'));
  if (st.loading) {
    return (
      <>
        <PageHeader title="Session replay" crumbs={[{ label: 'Services', to: '/services' }, { label: sessionId }]} />
        <Card><div className="row muted" style={{ gap: 'var(--space-2)' }}><Loader2 size={14} className="spin" aria-hidden />Loading replay…</div></Card>
      </>
    );
  }
  if (!st.detail) {
    return <EmptyState icon={<Play />} title={st.notFound ? 'Replay not found' : 'Could not load the replay'}>
      {st.notFound ? 'This session was not recorded, or its replay is older than the 30-day retention.' : st.error}
    </EmptyState>;
  }
  return <ReplayView key={sessionId} detail={st.detail} initialT={Number.isFinite(t) && t > 0 ? t : 0} />;
}

// ---------------------------------------------------------------- lazy segments
interface Loaded { events: (RrwebEvent | WireEvent)[]; loadedUntil: number; done: boolean; error?: string }

/** Fetch the first segments, then the rest in order (events must stay ordered for rrweb). */
function useSegments(detail: ReplayDetail): Loaded {
  const [st, setSt] = useState<Loaded>({ events: [], loadedUntil: 0, done: false });
  const { summary, segments } = detail;
  useEffect(() => {
    let cancelled = false;
    const order = [...segments].sort((a, b) => a.segment - b.segment);
    const acc: ReplaySegmentPayload[] = [];
    const publish = (done: boolean) => {
      if (cancelled) return;
      const events = acc.flatMap((p) => p.events);
      const last = acc[acc.length - 1];
      setSt({ events, loadedUntil: done ? summary.durationMs : last ? last.end_ts - summary.startedAt : 0, done });
    };
    (async () => {
      try {
        const FIRST = 2;
        const BATCH = 4;
        acc.push(...await Promise.all(order.slice(0, FIRST).map((s) => fetchReplaySegment(summary.sessionId, s.segment))));
        publish(order.length <= FIRST);
        for (let i = FIRST; i < order.length && !cancelled; i += BATCH) {
          acc.push(...await Promise.all(order.slice(i, i + BATCH).map((s) => fetchReplaySegment(summary.sessionId, s.segment))));
          publish(i + BATCH >= order.length);
        }
      } catch (e) {
        if (!cancelled) setSt((s) => ({ ...s, error: e instanceof Error ? e.message : String(e) }));
      }
    })();
    return () => { cancelled = true; };
  }, [segments, summary.sessionId, summary.startedAt, summary.durationMs]);
  return st;
}

// ---------------------------------------------------------------- view
function ReplayView({ detail, initialT }: { detail: ReplayDetail; initialT: number }) {
  const { summary, timeline, segments } = detail;
  const format = summary.format ?? replayFormat(summary.platform);
  const appId = replayAppId(summary.app);
  const app = services.find((s) => s.id === appId);
  const loaded = useSegments(detail);
  const dur = Math.max(1, summary.durationMs);

  const [playing, setPlaying] = useState(true);
  const [waiting, setWaiting] = useState(false);
  const [speed, setSpeed] = useState<Speed>('1');
  const [skip, setSkip] = useState(true);
  const [t, setT] = useState(Math.min(initialT, dur));
  const tRef = useRef(t);
  const [seek, setSeek] = useState({ t: Math.min(initialT, dur), n: 0 });
  const [errItem, setErrItem] = useState<TimelineItem | null>(null);

  const ready = loaded.events.length >= 2 && (loaded.done || loaded.loadedUntil >= seek.t);
  const onTime = useCallback((v: number) => { tRef.current = v; setT((p) => (Math.abs(p - v) >= 40 ? v : p)); }, []);
  const seekTo = useCallback((v: number) => {
    const x = Math.max(0, Math.min(dur, v));
    tRef.current = x;
    setT(x);
    setSeek((s) => ({ t: x, n: s.n + 1 }));
  }, [dur]);
  const onEnded = useCallback(() => {
    if (loaded.done) { setPlaying(false); setT(dur); }
    else setWaiting(true);
  }, [loaded.done, dur]);
  // more segments arrived while waiting at the end of what was loaded → continue from there
  useEffect(() => {
    if (waiting && (loaded.done || loaded.loadedUntil > tRef.current + 200)) { setWaiting(false); seekTo(tRef.current); }
  }, [waiting, loaded.loadedUntil, loaded.done, seekTo]);

  const toggle = useCallback(() => {
    if (!playing && tRef.current >= dur - 250) seekTo(0);
    setPlaying((p) => !p);
  }, [playing, dur, seekTo]);
  useEffect(() => {
    const onKey = (e: KeyboardEvent) => {
      const el = e.target as HTMLElement | null;
      if (e.code !== 'Space' || el?.closest('input, textarea, select, button, [contenteditable]')) return;
      e.preventDefault();
      toggle();
    };
    window.addEventListener('keydown', onKey);
    return () => window.removeEventListener('keydown', onKey);
  }, [toggle]);

  const idle = useMemo(() => {
    // stretches without any recorded segment (≥ 3 s) — what "skip inactivity" jumps over
    const out: { from: number; to: number }[] = [];
    const segs = [...segments].sort((a, b) => a.startTs - b.startTs);
    for (let i = 1; i < segs.length; i++) {
      const gap = segs[i].startTs - segs[i - 1].endTs;
      if (gap >= 3000) out.push({ from: segs[i - 1].endTs - summary.startedAt, to: segs[i].startTs - summary.startedAt });
    }
    return out;
  }, [segments, summary.startedAt]);

  const playback = { playing: playing && !waiting && ready, speed: Number(speed), skipInactive: skip, seek, onTime, onEnded };
  const userLabel = summary.user.id ?? summary.user.anonymousId ?? 'anonymous';
  const cat = app ? categoryOf(app.type) : undefined;

  return (
    <>
      <PageHeader
        title={<span className="row" style={{ gap: 'var(--space-2)' }}><UserRound size={18} className="muted" aria-hidden />{userLabel}{summary.user.id ? <Badge tone="accent">Identified</Badge> : <Badge>Anonymous</Badge>}</span>}
        crumbs={[
          ...(cat ? [{ label: cat.label, to: `/services?type=${cat.id}` }] : []),
          { label: summary.app, to: `/services/${appId}` },
          { label: 'Replays', to: `/services/${appId}?tab=replays` },
          { label: summary.sessionId },
        ]}
        subtitle={<span className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <span>{[summary.device, summary.browser, summary.os].filter(Boolean).join(' · ')}{summary.country ? ` · ${summary.country}` : ''}</span>
          <span className="muted">{dateTime(summary.startedAt)} · {ago(summary.startedAt)}</span>
          <span>{duration(summary.durationMs / 1000)}</span>
          <span>{summary.views} {format === 'rrweb' ? 'pages' : 'screens'}</span>
          {summary.errors ? <StatusBadge status="warning" label={`${summary.errors} error${summary.errors > 1 ? 's' : ''}`} /> : <span className="muted">no errors</span>}
          {summary.rageClicks > 0 && <StatusBadge status="warning" label={`${summary.rageClicks} rage ${format === 'rrweb' ? 'click' : 'tap'}${summary.rageClicks > 1 ? 's' : ''}`} />}
          {summary.appVersion && <span className="mono muted">{summary.appVersion}</span>}
        </span>}
        actions={<span className="replay__mask"><Lock size={14} aria-hidden />Text and inputs are masked</span>} />

      <div className="replay">
        <Card className="replay__player">
          <div style={{ position: 'relative' }}>
            {ready
              ? format === 'rrweb'
                ? <ReplayPlayer events={loaded.events as RrwebEvent[]} startedAt={summary.startedAt} {...playback} />
                : <WireframePlayer events={loaded.events as WireEvent[]} startedAt={summary.startedAt} durationMs={dur} limitMs={loaded.done ? undefined : loaded.loadedUntil} {...playback} />
              : <div className="replay-stage" style={{ height: 360 }} />}
            {(!ready || waiting) && !loaded.error && <div className="replay__overlay"><span><Loader2 size={14} className="spin" aria-hidden />{ready ? 'Buffering…' : 'Loading recording…'}</span></div>}
            {loaded.error && <div className="replay__overlay"><span><AlertTriangle size={14} aria-hidden />Could not load segments: {loaded.error}</span></div>}
          </div>
          <Controls t={t} dur={dur} playing={playing} onToggle={toggle} speed={speed} onSpeed={setSpeed} skip={skip} onSkip={setSkip}
            loadedUntil={loaded.loadedUntil} idle={idle} timeline={timeline} startedAt={summary.startedAt} onSeek={seekTo} mobile={format === 'wireframe'} />
        </Card>
        <EventList timeline={timeline} startedAt={summary.startedAt} t={t} onSeek={(v) => { seekTo(v); setPlaying(true); }} onError={setErrItem} />
      </div>

      {errItem && (
        <Drawer open onClose={() => setErrItem(null)} title={<span className="mono">{errItem.label}</span>}
          subtitle={<><StatusBadge status="critical" label="Error" /><span className="muted">at {clockOffset(errItem.ts - summary.startedAt)} · {errItem.viewName}</span></>}
          actions={<Button size="sm" onClick={() => { seekTo(errItem.ts - summary.startedAt - 2000); setPlaying(true); setErrItem(null); }}><Play size={14} />Play from here</Button>}>
          <KeyValues items={[
            ['When', `${dateTime(errItem.ts)} · ${clockOffset(errItem.ts - summary.startedAt)} into the session`],
            [format === 'rrweb' ? 'Page' : 'Screen', <span key="v" className="mono">{errItem.viewName ?? '—'}</span>],
            ...(errItem.fingerprint ? [['Fingerprint', <span key="f" className="mono">{errItem.fingerprint}</span>] as [ReactNode, ReactNode]] : []),
          ]} />
          <DrawerSection title="Stack trace">
            <CodeBlock>{errItem.detail ?? errItem.label}</CodeBlock>
          </DrawerSection>
          <Link to={`/services/${appId}?tab=errors`} className="row" style={{ gap: 'var(--space-1)' }}>All errors of {summary.app} <ArrowRight size={14} /></Link>
        </Drawer>
      )}
    </>
  );
}

// ---------------------------------------------------------------- controls + scrubber
function Controls({ t, dur, playing, onToggle, speed, onSpeed, skip, onSkip, loadedUntil, idle, timeline, startedAt, onSeek, mobile }: {
  t: number; dur: number; playing: boolean; onToggle: () => void; speed: Speed; onSpeed: (s: Speed) => void; skip: boolean; onSkip: (v: boolean) => void;
  loadedUntil: number; idle: { from: number; to: number }[]; timeline: TimelineItem[]; startedAt: number; onSeek: (t: number) => void; mobile: boolean;
}) {
  const track = useRef<HTMLDivElement>(null);
  const drag = useRef(false);
  const at = (clientX: number) => {
    const r = track.current?.getBoundingClientRect();
    return r ? ((clientX - r.left) / r.width) * dur : 0;
  };
  const pctOf = (v: number) => `${Math.max(0, Math.min(100, (v / dur) * 100))}%`;
  const markers = timeline.map((i) => ({ i, k: markerOf(i) })).filter((m): m is { i: TimelineItem; k: MarkerKind } => !!m.k);
  const ended = !playing && t >= dur - 250;
  return (
    <div className="rc">
      <div ref={track} className="scrub" role="slider" aria-label="Playback position" aria-valuemin={0} aria-valuemax={Math.round(dur / 1000)} aria-valuenow={Math.round(t / 1000)} tabIndex={0}
        onKeyDown={(e) => { if (e.key === 'ArrowRight') onSeek(t + 5000); if (e.key === 'ArrowLeft') onSeek(t - 5000); }}
        onPointerDown={(e) => { drag.current = true; (e.currentTarget as HTMLElement).setPointerCapture(e.pointerId); onSeek(at(e.clientX)); }}
        onPointerMove={(e) => { if (drag.current) onSeek(at(e.clientX)); }}
        onPointerUp={() => { drag.current = false; }}>
        <div className="scrub__track">
          <div className="scrub__loaded" style={{ width: pctOf(loadedUntil) }} />
          {idle.map((g, i) => <div key={i} className="scrub__idle" style={{ left: pctOf(g.from), width: `calc(${pctOf(g.to)} - ${pctOf(g.from)})` }} title="Inactive" />)}
          <div className="scrub__played" style={{ width: pctOf(t) }} />
        </div>
        {markers.map(({ i, k }, n) => (
          <button key={n} className={`scrub__marker${k === 'action' ? ' scrub__marker--low' : ''}`} style={{ left: pctOf(i.ts - startedAt), background: MARKER[k].color }}
            title={`${clockOffset(i.ts - startedAt)} · ${MARKER[k].label}: ${i.label}`} aria-label={`${MARKER[k].label} at ${clockOffset(i.ts - startedAt)}: ${i.label}`}
            onPointerDown={(e) => e.stopPropagation()}
            onClick={(e) => { e.stopPropagation(); onSeek(i.ts - startedAt - (k === 'error' ? 2000 : 500)); }} />
        ))}
        <div className="scrub__head" style={{ left: pctOf(t) }} />
      </div>
      <div className="rc__row">
        <Button variant="primary" size="sm" iconOnly aria-label={ended ? 'Replay' : playing ? 'Pause' : 'Play'} onClick={onToggle}
          icon={ended ? <RotateCcw size={14} /> : playing ? <Pause size={14} /> : <Play size={14} />} />
        <span className="rc__time">{clockOffset(t)} / {clockOffset(dur)}</span>
        <Segmented label="Speed" value={speed} onChange={onSpeed} options={SPEEDS.map((s) => ({ id: s, label: `${s}×` }))} />
        <span className="row" style={{ gap: 'var(--space-1)' }}><FastForward size={14} className="muted" aria-hidden /><Toggle checked={skip} onChange={onSkip} label="Skip inactivity" /></span>
        <span className="spacer" />
        <div className="legend" style={{ margin: 0 }}>
          {(Object.keys(MARKER) as MarkerKind[]).map((k) => (
            <span key={k} className="legend__item"><span className="legend__swatch" style={{ background: MARKER[k].color }} />{k === 'action' && mobile ? 'Tap' : MARKER[k].label}</span>
          ))}
        </div>
      </div>
    </div>
  );
}

// ---------------------------------------------------------------- event list
function EventList({ timeline, startedAt, t, onSeek, onError }: {
  timeline: TimelineItem[]; startedAt: number; t: number; onSeek: (t: number) => void; onError: (i: TimelineItem) => void;
}) {
  const [filter, setFilter] = useState<ListFilter>('all');
  const list = useRef<HTMLDivElement>(null);
  const rows = timeline.filter((i) => filter === 'all' || (filter === 'errors' ? i.kind === 'error' || isFailed(i) : filter === 'network' ? i.kind === 'resource' : i.kind === 'action'));
  let active = -1;
  for (let k = 0; k < rows.length; k++) if (rows[k].ts - startedAt <= t + 50) active = k;

  // keep the active row in view inside the list (not the page)
  useEffect(() => {
    const box = list.current;
    const el = box?.querySelector<HTMLElement>('.rev__item.is-active');
    if (!box || !el) return;
    const top = el.offsetTop - box.offsetTop;
    if (top < box.scrollTop || top + el.offsetHeight > box.scrollTop + box.clientHeight) box.scrollTo({ top: Math.max(0, top - box.clientHeight / 3), behavior: 'smooth' });
  }, [active, filter]);

  const counts = { errors: timeline.filter((i) => i.kind === 'error' || isFailed(i)).length };
  return (
    <Card title="Events" subtitle={`${timeline.length} from RUM · click to jump`} flush className="rev">
      <div className="rev__filters">
        <Segmented label="Filter events" value={filter} onChange={setFilter} options={[
          { id: 'all', label: 'All' },
          { id: 'errors', label: <>Errors {counts.errors > 0 && <Badge tone="warning">{counts.errors}</Badge>}</> },
          { id: 'network', label: 'Network' },
          { id: 'actions', label: 'Actions' },
        ]} />
      </div>
      <div ref={list} className="rev__list" role="list">
        {rows.length === 0 && <div className="rev__item muted" style={{ cursor: 'default' }}><span /><span /><span>No events.</span></div>}
        {rows.map((i, k) => {
          const Icon = KIND_ICON[i.kind];
          const m = markerOf(i);
          const off = i.ts - startedAt;
          return (
            <div key={`${i.ts}-${k}`} role="listitem" className={`rev__item${k === active ? ' is-active' : ''}${off > t + 50 ? ' is-future' : ''}`}
              onClick={() => (i.kind === 'error' ? onError(i) : onSeek(off - 500))}>
              <span className="rev__ts">{clockOffset(off)}</span>
              <span className="rev__icon" style={{ color: m ? MARKER[m].color : 'var(--text-muted)' }}><Icon size={14} aria-hidden /></span>
              <span className="rev__body">
                <span className={`rev__label${i.kind === 'resource' || i.kind === 'view' ? ' mono' : ''}`}>{i.label}</span>
                <span className="rev__meta">
                  {i.kind === 'resource' && (isFailed(i)
                    ? <StatusBadge status="critical" label={i.status ? String(i.status) : 'network error'} />
                    : <span>{i.status}</span>)}
                  {i.durationMs !== undefined && <span>{fmtMs(i.durationMs)}</span>}
                  {i.kind === 'error' && <span>{i.viewName} · stack trace</span>}
                  {i.kind !== 'error' && i.kind !== 'resource' && i.detail && <span>{i.detail}</span>}
                  {i.traceId && <Link to={traceUrl(i.traceId)} onClick={(e) => e.stopPropagation()} className="row" style={{ gap: 2 }}>Trace <ArrowRight size={12} /></Link>}
                  {i.kind === 'error' && <button className="btn btn--ghost btn--sm" style={{ height: 'auto', padding: 0 }} onClick={(e) => { e.stopPropagation(); onSeek(off - 2000); }}>Jump</button>}
                </span>
              </span>
            </div>
          );
        })}
      </div>
    </Card>
  );
}
