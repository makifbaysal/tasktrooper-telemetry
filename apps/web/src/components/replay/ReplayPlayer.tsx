import { useEffect, useRef, useState } from 'react';
import { Replayer } from '@rrweb/replay';
import '@rrweb/replay/dist/style.css';
import '@/styles/replay.css';
import { useWidth } from '@/components/charts/scale';

/** Playback props shared by ReplayPlayer (rrweb) and WireframePlayer. Times are ms from the session start. */
export interface PlaybackProps {
  playing: boolean;
  speed: number;
  skipInactive: boolean;
  /** seek request — applied whenever `n` changes (and once on mount) */
  seek: { t: number; n: number };
  onTime: (t: number) => void;
  /** reached the last loaded event */
  onEnded: () => void;
  /** max height of the stage in px */
  maxHeight?: number;
}

type RrEvent = { type: number; data: unknown; timestamp: number };
type ReplayerEvents = ConstructorParameters<typeof Replayer>[0];

/** rrweb mutates events while applying them (filters removes, queues adds) — give it its own copy. */
const clone = <T,>(e: T): T => structuredClone(e);
const cssVar = (name: string, fallback: string) => {
  try { return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback; } catch { return fallback; }
};

/**
 * Web session replay: `@rrweb/replay` Replayer in a scaled stage. Events may grow while playing (lazy segment loading);
 * new events are passed to `addEvent`, which the Replayer applies when their time comes.
 */
export function ReplayPlayer({ events, startedAt, playing, speed, skipInactive, seek, onTime, onEnded, maxHeight = 560 }: PlaybackProps & {
  events: RrEvent[];
  startedAt: number;
}) {
  const [boxRef, boxW] = useWidth<HTMLDivElement>();
  const mount = useRef<HTMLDivElement>(null);
  const rep = useRef<Replayer | null>(null);
  const fed = useRef(0);
  const base = useRef(0);
  const seekN = useRef(seek.n);
  const live = useRef({ onTime, onEnded, seek });
  live.current = { onTime, onEnded, seek };
  const meta = events.find((e) => e.type === 4)?.data as { width?: number; height?: number } | undefined;
  const [size, setSize] = useState({ w: meta?.width ?? 1280, h: meta?.height ?? 720 });
  const ready = events.length >= 2;

  // create once enough events are there; destroy on unmount
  useEffect(() => {
    if (!ready || !mount.current) return;
    const r = new Replayer(events.map(clone) as unknown as ReplayerEvents, {
      root: mount.current, speed, skipInactive, showWarning: false, triggerFocus: false, UNSAFE_replayCanvas: false, pauseAnimation: true,
      mouseTail: { duration: 600, lineCap: 'round', lineWidth: 2, strokeStyle: cssVar('--series-1', 'currentColor') },
    });
    rep.current = r;
    fed.current = events.length;
    base.current = events[0].timestamp - startedAt;
    seekN.current = live.current.seek.n;
    r.on('resize', (d) => { const { width, height } = d as { width: number; height: number }; if (width && height) setSize({ w: width, h: height }); });
    r.on('finish', () => live.current.onEnded());
    r.pause(Math.max(0, live.current.seek.t - base.current));
    return () => {
      try { r.destroy(); } catch { /* already gone */ }
      rep.current = null;
      fed.current = 0;
    };
    // created once per mount; later events go through addEvent below
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [ready]);

  // lazily loaded segments
  useEffect(() => {
    const r = rep.current;
    if (!r) return;
    for (let i = fed.current; i < events.length; i++) r.addEvent(clone(events[i]) as unknown as ReplayerEvents[number]);
    fed.current = events.length;
  }, [events]);

  useEffect(() => {
    const r = rep.current;
    if (!r) return;
    if (playing) r.play(r.getCurrentTime());
    else r.pause();
  }, [playing, ready]);

  useEffect(() => {
    const r = rep.current;
    if (!r || seek.n === seekN.current) return;
    seekN.current = seek.n;
    const off = Math.max(0, seek.t - base.current);
    if (playing) r.play(off); else r.pause(off);
    // only a new seek request moves the playhead
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [seek.n]);

  useEffect(() => { rep.current?.setConfig({ speed, skipInactive }); }, [speed, skipInactive, ready]);

  // report the playhead every frame
  useEffect(() => {
    let raf = 0;
    let last = -1;
    const tick = () => {
      const r = rep.current;
      if (r) {
        const t = Math.round(r.getCurrentTime() + base.current);
        if (t !== last) { last = t; live.current.onTime(t); }
      }
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, []);

  const scale = boxW ? Math.min(boxW / size.w, maxHeight / size.h) : 0;
  return (
    <div ref={boxRef} className="replay-stage" style={{ height: scale ? size.h * scale : maxHeight * 0.6 }}>
      <div ref={mount} className="replay-stage__mount"
        style={{ width: size.w, height: size.h, transform: `scale(${scale || 0.0001})`, left: scale ? (boxW - size.w * scale) / 2 : 0 }} />
    </div>
  );
}
