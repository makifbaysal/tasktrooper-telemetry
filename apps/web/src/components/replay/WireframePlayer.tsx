import { useEffect, useId, useMemo, useRef, useState } from 'react';
import type { WireEvent, WireNode } from '@/data';
import type { PlaybackProps } from './ReplayPlayer';
import '@/styles/replay.css';

type Screen = Extract<WireEvent, { type: 'screen' }>;
type Frame = Extract<WireEvent, { type: 'frame' }>;
type Touch = Extract<WireEvent, { type: 'touch' }>;
type Keyboard = Extract<WireEvent, { type: 'keyboard' }>;

/** Index of the last item with ts ≤ t (−1 when none). */
function lastAt<T extends { ts: number }>(xs: T[], t: number): number {
  let lo = 0;
  let hi = xs.length - 1;
  let ans = -1;
  while (lo <= hi) {
    const mid = (lo + hi) >> 1;
    if (xs[mid].ts <= t) { ans = mid; lo = mid + 1; } else hi = mid - 1;
  }
  return ans;
}

/** Gap (ms) without any event that "skip inactivity" jumps over. */
const IDLE_MS = 2500;

/**
 * Mobile session replay (wireframe format, contracts §23): SVG drawing of the recorded view hierarchy. No pixels are ever
 * recorded — nodes are boxes by kind with their colors, masked text is drawn as bars (labels) or dots (inputs), images as
 * placeholder boxes. Touches show as rings with a short trail; the soft keyboard as a neutral panel.
 */
export function WireframePlayer({ events, startedAt, durationMs, limitMs, playing, speed, skipInactive, seek, onTime, onEnded, maxHeight = 560 }: PlaybackProps & {
  events: WireEvent[];
  startedAt: number;
  durationMs: number;
  /** last loaded ms (lazy segments); playback waits here */
  limitMs?: number;
}) {
  const uid = useId().replace(/:/g, '');
  const [t, setT] = useState(seek.t);
  const tRef = useRef(seek.t);
  const live = useRef({ onTime, onEnded, speed, skipInactive, limitMs, durationMs });
  live.current = { onTime, onEnded, speed, skipInactive, limitMs, durationMs };

  const idx = useMemo(() => {
    const rel = <T extends WireEvent>(e: T): T => ({ ...e, ts: e.ts - startedAt });
    const by = <K extends WireEvent['type']>(k: K) => events.filter((e) => e.type === k).map(rel) as Extract<WireEvent, { type: K }>[];
    return {
      screens: by('screen') as Screen[], frames: by('frame') as Frame[], touches: by('touch') as Touch[], keyboard: by('keyboard') as Keyboard[],
      all: events.map((e) => ({ ts: e.ts - startedAt })).sort((a, b) => a.ts - b.ts),
    };
  }, [events, startedAt]);
  const idxRef = useRef(idx);
  idxRef.current = idx;

  useEffect(() => { tRef.current = seek.t; setT(seek.t); onTime(seek.t); }, [seek.n]); // eslint-disable-line react-hooks/exhaustive-deps

  useEffect(() => {
    if (!playing) return;
    let raf = 0;
    let prev = performance.now();
    const tick = (now: number) => {
      const L = live.current;
      let next = tRef.current + (now - prev) * L.speed;
      prev = now;
      if (L.skipInactive) {
        const all = idxRef.current.all;
        const i = lastAt(all, next) + 1;
        if (i < all.length && all[i].ts - next > IDLE_MS) next = all[i].ts - 400;
      }
      const end = Math.min(L.durationMs, L.limitMs ?? Number.POSITIVE_INFINITY);
      if (next >= end) {
        tRef.current = end; setT(end); L.onTime(end); L.onEnded();
        return;
      }
      tRef.current = next;
      setT(next);
      L.onTime(Math.round(next));
      raf = requestAnimationFrame(tick);
    };
    raf = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf);
  }, [playing]);

  const screen = idx.screens[Math.max(0, lastAt(idx.screens, t))];
  const frame = idx.frames[Math.max(0, lastAt(idx.frames, t))];
  const kb = idx.keyboard[lastAt(idx.keyboard, t)];
  const W = screen?.width ?? 390;
  const H = screen?.height ?? 844;
  const recent = idx.touches.filter((x) => x.ts <= t && t - x.ts < 700);
  const lastTouch = idx.touches[lastAt(idx.touches, t)];
  const pressed = lastTouch && lastTouch.phase !== 'up' && t - lastTouch.ts < 1500;
  const clips = new Map<string, [number, number, number, number]>();
  for (const n of frame?.nodes ?? []) if (n.clip) clips.set(n.clip.join('-'), n.clip);
  const h = Math.min(maxHeight, H);
  const w = (h * W) / H;

  return (
    <div className="wire">
      <div className="wire__label mono">{screen?.name ?? '—'}</div>
      <div className="wire__device" style={{ width: w, height: h }}>
        <svg viewBox={`0 0 ${W} ${H}`} width={w} height={h} role="img" aria-label={`Wireframe of ${screen?.name ?? 'screen'}`}>
          <defs>
            {[...clips].map(([k, c]) => <clipPath key={k} id={`${uid}-c-${k}`}><rect x={c[0]} y={c[1]} width={c[2]} height={c[3]} /></clipPath>)}
          </defs>
          <rect className="wire__empty" x={0} y={0} width={W} height={H} />
          {(frame?.nodes ?? []).map((n) => <WireNodeShape key={n.id} n={n} clipId={n.clip ? `${uid}-c-${n.clip.join('-')}` : undefined} />)}
          {kb?.visible && <Keyboard W={W} H={H} />}
          {recent.filter((x) => x.phase === 'move').length > 1 && (
            <polyline className="wire__trail" points={recent.filter((x) => x.phase !== 'up').map((x) => `${x.x},${x.y}`).join(' ')} />
          )}
          {recent.map((x, i) => (
            <circle key={`${x.ts}-${i}`} className="wire__touch" cx={x.x} cy={x.y} r={18 + (t - x.ts) / 30} style={{ opacity: Math.max(0, 1 - (t - x.ts) / 700) }} />
          ))}
          {pressed && <circle className="wire__touch wire__touch--down" cx={lastTouch.x} cy={lastTouch.y} r={16} />}
        </svg>
      </div>
    </div>
  );
}

/** Masked text as rounded bars, one per word, proportional to its length. */
function TextBars({ x, y, w, h, text, color, align = 'start' }: { x: number; y: number; w: number; h: number; text: string; color: string; align?: 'start' | 'middle' }) {
  const words = text.split(/\s+/).filter(Boolean);
  const chars = words.reduce((a, s) => a + s.length, 0) + Math.max(0, words.length - 1);
  if (!chars) return null;
  const cw = Math.min(h * 0.55, w / chars);
  const bh = Math.max(3, Math.min(h * 0.6, 12));
  const used = chars * cw;
  let cx = align === 'middle' ? x + (w - used) / 2 : x;
  const cy = y + (h - bh) / 2;
  return (
    <g fill={color} opacity={0.55}>
      {words.map((wd, i) => {
        const bw = wd.length * cw;
        const r = <rect key={i} x={cx} y={cy} width={Math.max(2, bw - 1)} height={bh} rx={bh / 2} />;
        cx += bw + cw;
        return r;
      })}
    </g>
  );
}

function Dots({ x, y, w, h, count, color }: { x: number; y: number; w: number; h: number; count: number; color: string }) {
  const step = 9;
  const n = Math.min(count, Math.floor((w - 24) / step));
  return <g fill={color}>{Array.from({ length: n }, (_, i) => <circle key={i} cx={x + 14 + i * step} cy={y + h / 2} r={2.6} />)}</g>;
}

function WireNodeShape({ n, clipId }: { n: WireNode; clipId?: string }) {
  const ink = n.fg ?? 'var(--text-muted)';
  const common = { opacity: n.alpha ?? 1, clipPath: clipId ? `url(#${clipId})` : undefined };
  const box = (cls?: string) => (
    <rect className={cls} x={n.x} y={n.y} width={n.w} height={n.h} rx={n.radius ?? 0}
      fill={n.bg ?? 'none'} stroke={n.border ?? 'none'} strokeWidth={n.border ? 1 : 0} />
  );
  switch (n.kind) {
    case 'text':
      return <g {...common}>{n.masked || !n.text ? <TextBars x={n.x} y={n.y} w={n.w} h={n.h} text={n.text ?? '••••'} color={ink} /> : <text x={n.x} y={n.y + n.h * 0.75} fontSize={n.h * 0.8} fill={ink}>{n.text}</text>}</g>;
    case 'button':
      return <g {...common}>{box()}{n.text && <TextBars x={n.x + 8} y={n.y} w={n.w - 16} h={n.h} text={n.text} color={ink} align="middle" />}</g>;
    case 'input':
      return <g {...common}>{box()}{n.text ? (n.masked ? <Dots x={n.x} y={n.y} w={n.w} h={n.h} count={n.text.replace(/\s/g, '').length} color={ink} /> : <text x={n.x + 12} y={n.y + n.h * 0.65} fontSize={n.h * 0.4} fill={ink}>{n.text}</text>) : null}</g>;
    case 'image':
      return (
        <g {...common}>
          <rect x={n.x} y={n.y} width={n.w} height={n.h} rx={n.radius ?? 0} fill={n.bg ?? 'var(--bg-sunken)'} />
          {n.w > 40 && n.h > 30 && (
            <polyline className="wire__glyph" points={`${n.x + n.w * 0.3},${n.y + n.h * 0.68} ${n.x + n.w * 0.45},${n.y + n.h * 0.45} ${n.x + n.w * 0.56},${n.y + n.h * 0.58} ${n.x + n.w * 0.63},${n.y + n.h * 0.5} ${n.x + n.w * 0.72},${n.y + n.h * 0.68}`} />
          )}
        </g>
      );
    case 'switch':
      return <g {...common}><rect x={n.x} y={n.y} width={n.w} height={n.h} rx={n.h / 2} fill={n.bg ?? 'var(--bg-sunken)'} stroke={n.border ?? 'none'} /><circle cx={n.x + n.h / 2} cy={n.y + n.h / 2} r={n.h / 2 - 3} fill={n.fg ?? 'var(--bg-surface)'} className="wire__knob" /></g>;
    case 'icon':
      return <g {...common}><rect x={n.x + 2} y={n.y + 2} width={n.w - 4} height={n.h - 4} rx={(n.w - 4) / 3} fill="none" stroke={ink} strokeWidth={2} opacity={0.7} /></g>;
    case 'webview':
      return <g {...common}>{box()}<line x1={n.x} y1={n.y} x2={n.x + n.w} y2={n.y + n.h} className="wire__glyph" /><line x1={n.x + n.w} y1={n.y} x2={n.x} y2={n.y + n.h} className="wire__glyph" /></g>;
    default:
      return <g {...common}>{box()}</g>;
  }
}

function Keyboard({ W, H }: { W: number; H: number }) {
  const kh = Math.round(H * 0.34);
  const y0 = H - kh;
  const rows = [10, 9, 7, 3];
  return (
    <g>
      <rect className="wire__kb" x={0} y={y0} width={W} height={kh} />
      {rows.map((count, r) => {
        const keyW = r === 3 ? [W * 0.2, W * 0.5, W * 0.2] : Array.from({ length: count }, () => (W - 12) / 10 - 6);
        const total = keyW.reduce((a, x) => a + x + 6, -6);
        let x = (W - total) / 2;
        return keyW.map((kw, i) => {
          const el = <rect key={`${r}-${i}`} className="wire__key" x={x} y={y0 + 12 + r * (kh - 24) / 4} width={kw} height={(kh - 24) / 4 - 10} rx={5} />;
          x += kw + 6;
          return el;
        });
      })}
    </g>
  );
}
