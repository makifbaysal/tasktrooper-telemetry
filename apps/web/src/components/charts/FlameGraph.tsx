import { useEffect, useMemo, useRef, useState } from 'react';
import { RotateCcw, Search } from 'lucide-react';
import { useWidth } from './scale';
import '@/styles/flamegraph.css';

/**
 * Flame graph (icicle: root on top, callees below). DOM blocks, one row per stack depth.
 *
 * - Block width = value (total) of the frame; click → zoom (the frame fills the width, its ancestors stay on top as a breadcrumb row),
 *   breadcrumb / Reset to zoom out. Search dims frames that don't match and reports the matched share.
 * - Colors: `--series-1..8` by package/module (stable hash, so a package has the same color everywhere), soft tint behind ink text.
 * - Diff mode (nodes carry `baseValue`, `baseTotal` given): color = change in share of total between base and target on the
 *   diverging pair `--diff-increase` / `--diff-decrease` (never status colors); unchanged frames are neutral.
 */
export interface FlameDatum {
  name: string;
  /** total (self + children) */
  value: number;
  self: number;
  baseValue?: number;
  baseSelf?: number;
  children: FlameDatum[];
}

interface Props {
  root: FlameDatum;
  total: number;
  /** diff mode: total of the base profile */
  baseTotal?: number;
  format: (v: number) => string;
  /** controlled search (e.g. a click in the Top functions table); uncontrolled when omitted */
  search?: string;
  onSearchChange?: (q: string) => void;
  /** max height of the scrolling plot area (px) */
  maxHeight?: number;
  /** text shown above the plot on the right, e.g. "36 profiles" */
  meta?: React.ReactNode;
}

const ROW = 20;
/** frames narrower than this (px) are not drawn; their width still counts in the parent */
const MIN_PX = 0.75;
const SPECIAL = /^(all|\[.*\]|\(.*\))$/;

function h32(s: string) {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return x >>> 0;
}

/**
 * Package / module of a frame name — the color key.
 * Go `github.com/jackc/pgx/v5/pgconn.(*PgConn).Read` → `github.com/jackc/pgx/v5/pgconn`, `runtime.mallocgc` → `runtime`;
 * Node `parse (node_modules/pg-protocol/dist/parser.js)` → `pg-protocol`, `fn (src/routes/a.js)` → `src/routes`;
 * JVM `com.acme.orders.OrderService.create` → `com.acme.orders`.
 */
export function framePackage(name: string): string {
  if (SPECIAL.test(name)) return '';
  const node = name.match(/^.* \((.+)\)$/);
  if (node) {
    const p = node[1];
    const nm = p.match(/node_modules\/((?:@[^/]+\/)?[^/]+)/);
    if (nm) return nm[1];
    if (p.startsWith('node:')) return 'node';
    return p.split('/').slice(0, 2).join('/');
  }
  const paren = name.indexOf('(');
  const head = paren > 0 ? name.slice(0, paren) : name;
  const slash = head.lastIndexOf('/');
  if (slash >= 0) {
    const dot = name.indexOf('.', slash + 1);
    return dot > 0 ? name.slice(0, dot) : name;
  }
  const parts = head.split('.').filter(Boolean);
  if (paren > 0 || parts.length <= 2) return parts[0] ?? name;
  return parts.slice(0, -2).join('.');
}

/** Categorical slot of a package — fixed per package, never generated colors. */
export const packageSlot = (pkg: string) => (h32(pkg) % 8) + 1;
export const frameColor = (name: string) => {
  const pkg = framePackage(name);
  return pkg ? `color-mix(in srgb, var(--series-${packageSlot(pkg)}) 32%, var(--bg-surface))` : 'var(--bg-sunken)';
};

interface Block { node: FlameDatum; depth: number; x: number; w: number; parent: number; ancestor: boolean }

export function FlameGraph({ root, total, baseTotal, format, search, onSearchChange, maxHeight = 520, meta }: Props) {
  const [ref, width] = useWidth<HTMLDivElement>();
  const [zoom, setZoom] = useState<FlameDatum[]>([root]);
  const [ownQuery, setOwnQuery] = useState('');
  const [hover, setHover] = useState<{ b: Block; x: number; y: number } | null>(null);
  const plot = useRef<HTMLDivElement>(null);
  const query = (search ?? ownQuery).trim().toLowerCase();
  const setQuery = (q: string) => (onSearchChange ? onSearchChange(q) : setOwnQuery(q));
  const diff = baseTotal !== undefined;

  // new data → reset zoom
  useEffect(() => { setZoom([root]); setHover(null); }, [root]);

  const focus = zoom[zoom.length - 1];
  const { blocks, rows } = useMemo(() => {
    const out: Block[] = [];
    // ancestors of the focused frame as full-width rows
    zoom.forEach((n, i) => out.push({ node: n, depth: i, x: 0, w: 1, parent: i - 1, ancestor: i < zoom.length - 1 }));
    let maxDepth = zoom.length - 1;
    const fv = focus.value || 1;
    const minW = width ? MIN_PX / width : 0.001;
    const stack: { n: FlameDatum; depth: number; x: number; parent: number }[] = [];
    const pushKids = (n: FlameDatum, depth: number, x: number, parent: number) => {
      let cx = x;
      const kids: typeof stack = [];
      for (const c of n.children) {
        const w = c.value / fv;
        if (w >= minW) kids.push({ n: c, depth: depth + 1, x: cx, parent });
        cx += w;
      }
      for (let i = kids.length - 1; i >= 0; i--) stack.push(kids[i]);
    };
    pushKids(focus, zoom.length - 1, 0, zoom.length - 1);
    while (stack.length) {
      const { n, depth, x, parent } = stack.pop()!;
      const idx = out.length;
      out.push({ node: n, depth, x, w: n.value / fv, parent, ancestor: false });
      maxDepth = Math.max(maxDepth, depth);
      pushKids(n, depth, x, idx);
    }
    return { blocks: out, rows: maxDepth + 1 };
  }, [zoom, focus, width]);

  // search: matched frames, and the share of the total they cover (outermost matches only)
  const { matched, matchedValue } = useMemo(() => {
    const m = new Set<FlameDatum>();
    let v = 0;
    if (!query) return { matched: m, matchedValue: 0 };
    const walk = (n: FlameDatum, inside: boolean) => {
      const hit = n.name.toLowerCase().includes(query);
      if (hit) m.add(n);
      if (hit && !inside) v += n.value;
      n.children.forEach((c) => walk(c, inside || hit));
    };
    walk(root, false);
    return { matched: m, matchedValue: v };
  }, [root, query]);

  const maxDelta = useMemo(() => {
    if (!diff) return 0;
    let mx = 0;
    const walk = (n: FlameDatum) => {
      if (n !== root) mx = Math.max(mx, Math.abs(delta(n)));
      n.children.forEach(walk);
    };
    walk(root);
    return mx;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [root, diff, total, baseTotal]);

  function delta(n: FlameDatum) {
    const cur = total ? n.value / total : 0;
    const base = baseTotal ? (n.baseValue ?? 0) / baseTotal : 0;
    return cur - base;
  }
  function fill(n: FlameDatum) {
    if (!diff) return frameColor(n.name);
    const d = delta(n);
    if (Math.abs(d) < 0.0005 || !maxDelta) return 'var(--bg-sunken)';
    const strength = Math.round(14 + 52 * Math.min(1, Math.abs(d) / maxDelta));
    return `color-mix(in srgb, var(${d > 0 ? '--diff-increase' : '--diff-decrease'}) ${strength}%, var(--bg-surface))`;
  }

  function onClick(b: Block) {
    if (b.ancestor || b.depth === zoom.length - 1) {
      setZoom(zoom.slice(0, b.depth + 1));
      return;
    }
    const path: FlameDatum[] = [];
    let i: number = blocks.indexOf(b);
    while (i >= zoom.length) { path.unshift(blocks[i].node); i = blocks[i].parent; }
    setZoom([...zoom, ...path]);
  }
  function onMove(e: React.MouseEvent, b: Block) {
    const r = plot.current?.getBoundingClientRect();
    if (!r) return;
    setHover({ b, x: e.clientX - r.left + (plot.current?.scrollLeft ?? 0), y: e.clientY - r.top + (plot.current?.scrollTop ?? 0) });
  }

  const pct = (v: number, of: number) => (of ? `${((v / of) * 100).toFixed(v / of >= 0.1 ? 1 : 2)}%` : '—');
  const crumbs = zoom.length > 5 ? [zoom[0], null, ...zoom.slice(-3)] : zoom;

  return (
    <div className="flame">
      <div className="flame__bar">
        <div className="flame__crumbs" aria-label="Zoom path">
          {crumbs.map((n, i) => n === null
            ? <span key="gap" className="flame__crumb-sep">› …</span>
            : (
              <span key={i} className="flame__crumb-wrap">
                {i > 0 && <span className="flame__crumb-sep">›</span>}
                <button className="flame__crumb" aria-current={n === focus} onClick={() => setZoom(zoom.slice(0, zoom.indexOf(n) + 1))} title={n.name}>
                  {n === root ? 'all' : n.name}
                </button>
              </span>
            ))}
          {zoom.length > 1 && (
            <button className="btn btn--ghost btn--sm flame__reset" onClick={() => setZoom([root])}><RotateCcw size={12} aria-hidden />Reset</button>
          )}
        </div>
        <div className="flame__tools">
          {query && <span className="flame__match">{matched.size ? `${pct(matchedValue, root.value)} of total matches` : 'No match'}</span>}
          {meta && <span className="flame__meta">{meta}</span>}
          <label className="input-group flame__search">
            <Search size={14} aria-hidden />
            <input className="input" placeholder="Search function or package" value={search ?? ownQuery} onChange={(e) => setQuery(e.target.value)} aria-label="Search frames" />
          </label>
        </div>
      </div>
      <div ref={ref} className="flame__frame">
        <div ref={plot} className="flame__plot" style={{ maxHeight }} onMouseLeave={() => setHover(null)}>
          <div className="flame__canvas" style={{ height: rows * ROW }} role="img" aria-label={`Flame graph, ${blocks.length} frames`}>
            {width > 0 && blocks.map((b, i) => {
              const px = b.w * width;
              const dim = (query && !matched.has(b.node)) || b.ancestor;
              return (
                <div key={i} className={`flame__block${dim ? ' is-dim' : ''}${query && matched.has(b.node) ? ' is-match' : ''}`}
                  style={{ left: `${b.x * 100}%`, width: `${b.w * 100}%`, top: b.depth * ROW, background: fill(b.node) }}
                  onClick={() => onClick(b)} onMouseMove={(e) => onMove(e, b)}>
                  {px > 28 && <span className="flame__label">{b.node.name === 'all' ? 'all' : b.node.name}</span>}
                </div>
              );
            })}
          </div>
          {hover && (
            <div className="chart__tooltip flame__tip" style={{
              left: Math.min(hover.x + 14, Math.max(0, width - 340)), top: hover.y + 18,
            }}>
              <div className="flame__tip-name">{hover.b.node.name}</div>
              {framePackage(hover.b.node.name) && <div className="chart__tooltip-time">{framePackage(hover.b.node.name)}</div>}
              {diff ? (
                <>
                  <div className="chart__tooltip-row"><span>Share before</span><span>{pct(hover.b.node.baseValue ?? 0, baseTotal ?? 0)}</span></div>
                  <div className="chart__tooltip-row"><span>Share after</span><span>{pct(hover.b.node.value, total)}</span></div>
                  <div className="chart__tooltip-row"><span>Change</span><span>{signedPp(delta(hover.b.node))}</span></div>
                  <div className="chart__tooltip-row"><span>Total after</span><span>{format(hover.b.node.value)}</span></div>
                </>
              ) : (
                <>
                  <div className="chart__tooltip-row"><span>Total</span><span>{format(hover.b.node.value)} · {pct(hover.b.node.value, total)}</span></div>
                  <div className="chart__tooltip-row"><span>Self</span><span>{format(hover.b.node.self)} · {pct(hover.b.node.self, total)}</span></div>
                  {focus !== root && !hover.b.ancestor && <div className="chart__tooltip-row"><span>Of {shortName(focus.name)}</span><span>{pct(hover.b.node.value, focus.value)}</span></div>}
                </>
              )}
            </div>
          )}
        </div>
      </div>
    </div>
  );
}

/** "+1.24 pp" — change of share in percentage points. */
export function signedPp(d: number) {
  const v = d * 100;
  return `${v > 0 ? '+' : v < 0 ? '−' : ''}${Math.abs(v).toFixed(Math.abs(v) >= 10 ? 1 : 2)} pp`;
}
const shortName = (s: string) => (s.length > 32 ? `…${s.slice(-30)}` : s);

/** Legend for diff mode: the diverging pair with labels. */
export function FlameDiffLegend() {
  return (
    <div className="legend">
      <span className="legend__item"><span className="legend__swatch" style={{ background: 'var(--diff-increase)' }} />Larger share than base</span>
      <span className="legend__item"><span className="legend__swatch" style={{ background: 'var(--diff-decrease)' }} />Smaller share than base</span>
      <span className="legend__item"><span className="legend__swatch" style={{ background: 'var(--bg-sunken)', boxShadow: 'inset 0 0 0 1px var(--border)' }} />Unchanged</span>
    </div>
  );
}
