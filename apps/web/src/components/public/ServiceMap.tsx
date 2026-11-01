import { useEffect, useRef, useState, useSyncExternalStore, type ComponentType, type SVGProps } from 'react';
import { Database, Globe, Inbox, Network, Server, ShieldCheck, Smartphone } from 'lucide-react';
import { useMotionAllowed } from '@/components/layout/motion';

/**
 * Landing / sign-in scene: an example system from the user's screen (web + mobile app) down to the data tier, told
 * as one incident in five beats on a clock that starts at mount:
 *   0.0s  the system draws itself tier by tier, traffic starts flowing
 *   1.6s  a deploy (migration 0042) drops onto reco-db, which ripples
 *   2.4s  latency climbs back up the call path: reco-db → recommendation-api → storefront-bff → shop-web
 *         (edges turn amber, node metrics count up)
 *   4.2s  root-cause analysis scans down the same path
 *   5.6s  reco-db is tagged as the root cause and keeps pulsing; the cause sentence settles
 * Reduced motion renders the last frame. Below 720px (and on the sign-in panel) a portrait layout with fewer nodes keeps
 * text at its real size. Visual states live in public.css (`smap-*`).
 */

type Icon = ComponentType<SVGProps<SVGSVGElement> & { size?: number | string }>;
interface Metric { label: string; from: number; to?: number; at?: number; fmt?: 'ms' | 'n' }
interface Node { id: string; label: string; x: number; y: number; icon: Icon; metric: Metric; kind?: string /* for readers of the code; the icon shows it */ }
interface Edge { id: string; from: string; to: string; dur: number }
interface Lane { label: string; y: number }
interface Layout {
  box: string; nodes: Node[]; edges: Edge[]; lanes: Lane[]; seps: number[]; laneLabels: boolean;
  /** Bottom edge of the graph (WIDE only): a leader drops from reco-db to the finding band below. */
  findingY?: number;
  pin: { x: number; y: number }; notes: { x: number; y: number; at: number; text: string; anchor?: 'start' | 'end' }[];
}

/** Incident path, bottom-up = the order latency spreads; the scan walks it top-down. */
const PATH = ['db', 'reco', 'bff', 'web'];
const T = { deploy: 1.6, spread: 2.4, step: 0.5, scan: 4.2, scanDur: 1.4, cause: 5.6, end: 6.6 };
const spreadAt = (id: string) => T.spread + PATH.indexOf(id) * T.step;
const scanAt = (id: string) => T.scan + ((PATH.length - 1 - PATH.indexOf(id)) / (PATH.length - 1)) * T.scanDur;

const N = {
  web: (x: number, y: number): Node => ({ id: 'web', label: 'shop-web', kind: 'web app', x, y, icon: Globe, metric: { label: 'LCP', from: 1600, to: 4800, at: spreadAt('web') } }),
  app: (x: number, y: number): Node => ({ id: 'app', label: 'courier-app', kind: 'mobile app', x, y, icon: Smartphone, metric: { label: 'start', from: 820 } }),
  bff: (x: number, y: number): Node => ({ id: 'bff', label: 'storefront-bff', x, y, icon: Network, metric: { label: 'p95', from: 380, to: 4100, at: spreadAt('bff') } }),
  auth: (x: number, y: number): Node => ({ id: 'auth', label: 'auth-api', x, y, icon: ShieldCheck, metric: { label: 'p95', from: 42 } }),
  orders: (x: number, y: number): Node => ({ id: 'orders', label: 'orders-api', x, y, icon: Server, metric: { label: 'p95', from: 120 } }),
  reco: (x: number, y: number): Node => ({ id: 'reco', label: 'recommendation-api', x, y, icon: Server, metric: { label: 'p95', from: 210, to: 3900, at: spreadAt('reco') } }),
  catalog: (x: number, y: number): Node => ({ id: 'catalog', label: 'catalog-api', x, y, icon: Server, metric: { label: 'p95', from: 88 } }),
  db: (x: number, y: number): Node => ({ id: 'db', label: 'reco-db', x, y, icon: Database, metric: { label: 'query', from: 12, to: 760, at: spreadAt('db') } }),
  queue: (x: number, y: number): Node => ({ id: 'queue', label: 'orders.events', x, y, icon: Inbox, metric: { label: 'lag', from: 140, fmt: 'n' } }),
};

/** The WIDE graph ends at the root-cause lane: the leader from reco-db reaches the SVG's bottom edge, where the finding band starts. */
const FINDING_Y = 610;
const WIDE_H = FINDING_Y;

const WIDE: Layout = {
  box: `0 0 880 ${WIDE_H}`,
  laneLabels: true,
  lanes: [{ label: 'Clients', y: 18 }, { label: 'Services', y: 150 }, { label: 'Data', y: 430 }],
  seps: [128, 408],
  findingY: FINDING_Y,
  nodes: [N.web(250, 72), N.app(680, 72), N.bff(250, 200), N.auth(470, 200), N.orders(730, 200), N.reco(330, 330), N.catalog(600, 330), N.db(330, 486), N.queue(790, 486)],
  edges: [
    { id: 'web-bff', from: 'web', to: 'bff', dur: 1.6 },
    { id: 'app-orders', from: 'app', to: 'orders', dur: 1.8 },
    { id: 'app-auth', from: 'app', to: 'auth', dur: 2.3 },
    { id: 'bff-auth', from: 'bff', to: 'auth', dur: 2 },
    { id: 'bff-reco', from: 'bff', to: 'reco', dur: 1.9 },
    { id: 'bff-catalog', from: 'bff', to: 'catalog', dur: 2.5 },
    { id: 'orders-catalog', from: 'orders', to: 'catalog', dur: 1.7 },
    { id: 'reco-db', from: 'reco', to: 'db', dur: 1.5 },
    { id: 'orders-queue', from: 'orders', to: 'queue', dur: 2.1 },
  ],
  pin: { x: 520, y: 566 },
  notes: [
    { x: 236, y: 152, at: spreadAt('bff') + 0.4, text: 'homepage p95 4.1 s', anchor: 'end' },
    { x: 344, y: 418, at: T.cause, text: '63× slower since 0042' },
  ],
};

const NARROW: Layout = {
  box: '0 -8 360 600',
  laneLabels: false,
  lanes: [],
  seps: [124, 404],
  nodes: [N.web(92, 56), N.app(268, 56), N.bff(100, 190), N.orders(270, 190), N.reco(180, 320), N.db(180, 470)],
  edges: [
    { id: 'web-bff', from: 'web', to: 'bff', dur: 1.6 },
    { id: 'app-orders', from: 'app', to: 'orders', dur: 1.8 },
    { id: 'bff-orders', from: 'bff', to: 'orders', dur: 2.2 },
    { id: 'bff-reco', from: 'bff', to: 'reco', dur: 1.9 },
    { id: 'reco-db', from: 'reco', to: 'db', dur: 1.5 },
  ],
  pin: { x: 180, y: 560 },
  notes: [
    { x: 196, y: 404, at: T.cause, text: '63× slower' },
  ],
};

const H = 52;
const nodeW = (n: Node) => Math.round(Math.max(n.label.length * 8.4, (n.metric.label.length + 8) * 7.2, 92) + 54);
const fmt = (v: number, f: Metric['fmt']) => (f === 'n' ? `${Math.round(v)}` : v >= 1000 ? `${(v / 1000).toFixed(1)} s` : `${Math.round(v)} ms`);
const ease = (k: number) => 1 - Math.pow(1 - Math.min(1, Math.max(0, k)), 3);
const metricAt = (m: Metric, t: number) => (m.to == null || m.at == null ? m.from : m.from + (m.to - m.from) * ease((t - m.at) / 0.9));

/** Vertical bezier from the bottom of `a` to the top of `b` (or a horizontal one for same-row nodes). */
function curve(a: Node, b: Node) {
  if (Math.abs(a.y - b.y) < 10) {
    const dir = Math.sign(b.x - a.x);
    const x1 = a.x + (dir * nodeW(a)) / 2, x2 = b.x - (dir * nodeW(b)) / 2;
    return `M${x1} ${a.y} L${x2} ${b.y}`;
  }
  const y1 = a.y + H / 2, y2 = b.y - H / 2, my = (y1 + y2) / 2;
  return `M${a.x} ${y1} C${a.x} ${my} ${b.x} ${my} ${b.x} ${y2}`;
}

/** The same curve walked the other way (bottom of `b` up to `a`): the slowdown climbs towards the user. */
function curveUp(a: Node, b: Node) {
  const y1 = a.y + H / 2, y2 = b.y - H / 2, my = (y1 + y2) / 2;
  return `M${b.x} ${y2} C${b.x} ${my} ${a.x} ${my} ${a.x} ${y1}`;
}

/** Scene clock in seconds since mount; stops ticking at the last beat. Static (reduced motion) = the last frame. */
function useSceneClock(animate: boolean) {
  const [t, setT] = useState(animate ? 0 : Infinity);
  const raf = useRef(0);
  useEffect(() => {
    if (!animate) { setT(Infinity); return; }
    const start = performance.now();
    const tick = () => {
      const s = (performance.now() - start) / 1000;
      setT(s);
      if (s < T.end) raf.current = requestAnimationFrame(tick);
    };
    raf.current = requestAnimationFrame(tick);
    return () => cancelAnimationFrame(raf.current);
  }, [animate]);
  return t;
}

export function ServiceGraph({ narrow = false, className }: { narrow?: boolean; className?: string }) {
  const animate = useMotionAllowed();
  const t = useSceneClock(animate);
  const layout = narrow ? NARROW : WIDE;
  const [hover, setHover] = useState<string>();
  const { nodes, edges, pin } = layout;
  const byId = Object.fromEntries(nodes.map((n) => [n.id, n]));
  const onPath = (e: Edge) => PATH.includes(e.from) && PATH.includes(e.to);
  const degraded = (e: Edge) => onPath(e) && t >= spreadAt(e.to);
  const touches = (e: Edge) => !hover || e.from === hover || e.to === hover;
  const near = (id: string) => !hover || id === hover || edges.some((e) => touches(e) && (e.from === id || e.to === id));
  const tier = (n: Node) => (n.y < 130 ? 0 : n.y < 410 ? 1 : 2);
  const db = byId.db;
  const dbW = nodeW(db);
  const pathNodes = [...PATH].reverse().map((id) => byId[id]);
  const scanPath = pathNodes.slice(1).map((b, i) => {
    const a = pathNodes[i];
    return `${i === 0 ? '' : `L${a.x} ${a.y + H / 2} `}${curve(a, b).replace(/^M/, i === 0 ? 'M' : 'L')}`;
  }).join(' ');

  return (
    <svg className={`smap${className ? ` ${className}` : ''}`} viewBox={layout.box} data-static={animate ? undefined : ''} role="img"
      aria-label="Example system: shop-web (web app) and courier-app (mobile app) call backend services. After migration 0042, reco-db queries got 63 times slower; the slowdown spreads up through recommendation-api and storefront-bff to the shop-web homepage, and root-cause analysis points at reco-db.">
      <defs>
        {edges.map((e) => <path key={e.id} id={`smap-${narrow ? 'n' : 'w'}-${e.id}`} d={curve(byId[e.from], byId[e.to])} />)}
        <radialGradient id={`smap-halo-${narrow ? 'n' : 'w'}`}>
          <stop offset="0" stopColor="var(--field-signal)" stopOpacity="0.32" />
          <stop offset="1" stopColor="var(--field-signal)" stopOpacity="0" />
        </radialGradient>
      </defs>

      <g className="smap__lanes" aria-hidden>
        {layout.seps.map((y) => <line key={y} x1={0} x2={880} y1={y} y2={y} className="smap__sep" />)}
        {layout.laneLabels && layout.lanes.map((l) => <text key={l.label} x={0} y={l.y} className="smap__lane">{l.label}</text>)}
      </g>

      {t >= T.cause && <circle cx={db.x} cy={db.y} r={150} fill={`url(#smap-halo-${narrow ? 'n' : 'w'})`} className="smap__halo" />}

      <g className="smap__edges">
        {edges.map((e, i) => (
          <path key={e.id} d={curve(byId[e.from], byId[e.to])} pathLength={1}
            className={`smap__edge${degraded(e) ? ' is-hot' : ''}${touches(e) ? '' : ' is-dim'}`}
            style={{ ['--d' as string]: `${0.25 + tier(byId[e.from]) * 0.25 + i * 0.03}s` }} />
        ))}
        {edges.filter(degraded).map((e) => (
          // drawn bottom-up: the slowdown climbs from the database towards the user
          <path key={`hot-${e.id}`} d={curveUp(byId[e.from], byId[e.to])} pathLength={1} className="smap__hot" />
        ))}
        {t >= T.deploy && (
          <path className="smap__dep" d={narrow
            ? `M${db.x} ${db.y + H / 2} L${pin.x} ${pin.y - 17}`
            : `M${db.x + dbW / 2} ${db.y} C${pin.x - 40} ${db.y} ${pin.x - 60} ${pin.y} ${pin.x - 74} ${pin.y}`} />
        )}
        {t >= T.scan && t < T.scan + T.scanDur + 0.6 && <path d={scanPath} pathLength={1} className="smap__scan" />}
        {layout.findingY && t >= T.cause && (
          <g className="smap__lead">
            <path d={`M${db.x} ${db.y + H / 2} L${db.x} ${layout.findingY}`} pathLength={1} />
            <circle cx={db.x} cy={layout.findingY} r={4} />
          </g>
        )}
      </g>

      {animate && t >= 0.9 && (
        <g className="smap__traffic" aria-hidden>
          {edges.map((e, i) => {
            const hot = degraded(e);
            return (
              <circle key={`${e.id}-${hot ? 'h' : 'c'}`} r={hot ? 3 : 2.4} className={`smap__dot${hot ? ' smap__dot--hot' : ''}${touches(e) ? '' : ' is-dim'}`}>
                <animateMotion dur={`${hot ? e.dur * 2.4 : e.dur}s`} begin={`${-i * 0.41}s`} repeatCount="indefinite"><mpath href={`#smap-${narrow ? 'n' : 'w'}-${e.id}`} /></animateMotion>
              </circle>
            );
          })}
        </g>
      )}

      <g className="smap__nodes">
        {nodes.map((n) => {
          const w = nodeW(n);
          const hot = PATH.includes(n.id) && t >= spreadAt(n.id);
          const scanned = PATH.includes(n.id) && t >= scanAt(n.id);
          const cause = n.id === 'db' && t >= T.cause;
          const NodeIcon = n.icon;
          return (
            <g key={n.id} transform={`translate(${n.x - w / 2} ${n.y - H / 2})`}
              className={`smap__node${hot ? ' is-hot' : ''}${scanned ? ' is-scanned' : ''}${cause ? ' is-cause' : ''}${near(n.id) ? '' : ' is-dim'}`}
              style={{ ['--d' as string]: `${tier(n) * 0.25}s` }}
              onPointerEnter={() => setHover(n.id)} onPointerLeave={() => setHover(undefined)}>
              <g className="smap__pop">
                {cause && <rect className="smap__ripple" width={w} height={H} rx={12} />}
                {n.id === 'db' && t >= T.deploy && t < T.spread + 0.6 && <rect className="smap__ripple smap__ripple--once" width={w} height={H} rx={12} />}
                <rect className="smap__box" width={w} height={H} rx={12} />
                <NodeIcon x={14} y={(H - 18) / 2} width={18} height={18} className="smap__icon" aria-hidden />
                <text x={42} y={22} className="smap__name">{n.label}</text>
                <text x={42} y={39} className="smap__metric">
                  {n.metric.label} {fmt(metricAt(n.metric, t), n.metric.fmt)}
                </text>
              </g>
              {cause && (
                <g transform={`translate(${w / 2} -14)`}>
                  <g className="smap__tag">
                    <rect x={-46} y={-12} width={92} height={22} rx={11} />
                    <text y={4} textAnchor="middle">root cause</text>
                  </g>
                </g>
              )}
            </g>
          );
        })}
        {t >= T.deploy && (
          <g className="smap__pin" transform={`translate(${pin.x} ${pin.y})`}>
            <g className="smap__pin-drop">
              <rect x={-74} y={-17} width={148} height={34} rx={17} />
              <text y={5} textAnchor="middle">migration 0042</text>
            </g>
          </g>
        )}
      </g>

      <g className="smap__notes">
        {layout.notes.filter((n) => t >= n.at).map((n) => (
          <text key={n.text} x={n.x} y={n.y} textAnchor={n.anchor ?? 'start'} className="smap__note">{n.text}</text>
        ))}
      </g>
    </svg>
  );
}

const NARROW_Q = '(max-width: 720px)';
const subscribeNarrow = (cb: () => void) => {
  const m = window.matchMedia(NARROW_Q);
  m.addEventListener('change', cb);
  return () => m.removeEventListener('change', cb);
};
function useNarrowHero() {
  return useSyncExternalStore(subscribeNarrow, () => window.matchMedia(NARROW_Q).matches, () => false);
}

function Finding() {
  const animate = useMotionAllowed();
  return (
    <div className="finding" data-static={animate ? undefined : ''}>
      <p className="finding__head">
        <span className="finding__label">Root cause</span>
        <span className="finding__conf">High confidence · found 7 min after the deploy</span>
      </p>
      <ol className="finding__steps">
        <li className="finding__step finding__step--1"><span>Users feel it</span>shop-web homepage LCP 1.6 s → 4.8 s</li>
        <li className="finding__step finding__step--2"><span>Along the path</span><code>storefront-bff → recommendation-api → reco-db</code></li>
        <li className="finding__step finding__step--3"><span>Because</span>reco-db queries 63× slower since migration 0042</li>
        <li className="finding__step finding__step--4 finding__step--fix"><span>Suggested fix</span>0042 dropped the index on <code>recommendations(user_id)</code>. Recreate it, or roll back 0042.</li>
      </ol>
    </div>
  );
}

/** Map + root-cause finding as one scene: the finding band is the graph's last lane, fed by the leader from reco-db. */
export function HeroScene() {
  const narrow = useNarrowHero();
  return (
    <div className="hero__scene">
      <div className="hero__map">
        <ServiceGraph key={narrow ? 'n' : 'w'} narrow={narrow} />
        <Finding />
      </div>
    </div>
  );
}
