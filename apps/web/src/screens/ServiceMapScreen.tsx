import { useEffect, useMemo, useRef, useState, type ComponentType, type PointerEvent as ReactPointerEvent, type SVGProps } from 'react';
import { Link, useNavigate } from 'react-router';
import {
  Activity, AlertOctagon, AlertTriangle, AppWindow, Bot, ChevronLeft, ChevronRight, Cloud, Cog, Database, Globe, Handshake, Inbox, Maximize2,
  Minus, Network, Plus, Server, Smartphone, Waypoints, Webhook, X, Zap,
} from 'lucide-react';
import { useApp } from '@/components/layout/context';
import { useMotionAllowed } from '@/components/layout/motion';
import { Button, ConnectSdk, PageHeader, Segmented, SkeletonPage, StatusBadge, Toggle } from '@/components/ui';
import { serviceMap, services, useData, type Health, type MapEdge, type MapNode, type MapTier } from '@/data';
import { compact, ms, pct } from '@/lib/format';
import '@/styles/servicemap.css';

/**
 * Service map: every client app, service and dependency of the project with the traffic between them, drawn in the
 * landing graph's language (field ground, tiered lanes, moving traffic dots). `/service-map` shows the whole system;
 * the service detail's Map tab passes `serviceId` and draws only what that service is connected to.
 * Problems are status-coloured with an icon and a label (incident tag, problem cards). Hover highlights a node's
 * neighbours, click opens the details panel over the map, scroll zooms, drag pans.
 * Data: GET /service-map (contracts §8). Layout is computed here: lanes by tier, services by call depth, order by
 * neighbour barycentre to cut crossings.
 */

type Icon = ComponentType<SVGProps<SVGSVGElement>>;
const H = 52;
const GAP_X = 56;
const LANE_GAP = 150;
const SUB_GAP = 108;
const PAD_X = 112;
const PAD_Y = 44;
const MAX_PER_ROW = 6;

const ICONS: Record<string, Icon> = {
  web: AppWindow, mobile: Smartphone, http: Server, worker: Cog, consumer: Inbox, service: Server,
  db: Database, cache: Zap, queue: Waypoints, ai: Bot, partner: Handshake, webhook: Webhook, probe: Activity,
};
const iconOf = (n: MapNode): Icon => (n.tier === 'external' ? Cloud : n.tier === 'client' && n.kind === 'web' && !isService(n) ? Globe
  : ICONS[n.kind] ?? Server);

/** Generic callers detected from user agents read better as groups. */
const CALLER_NAMES: Record<string, string> = { web: 'Browsers (direct)', mobile: 'Mobile apps (direct)', probe: 'Probes' };
/** Why a synthetic caller exists: traffic whose sender has no SDK trace context, classified by user-agent. */
const CALLER_HINTS: Record<string, string> = {
  web: 'Browser requests without a TaskTrooper trace header: page navigations straight to the api (sign-in, OAuth returns such as Slack or GitHub), CORS preflights, or a web app without the browser SDK.',
  mobile: 'Mobile requests without a TaskTrooper trace header: an app without the mobile SDK, or calls made outside it.',
  probe: 'Health checks from load balancers and Kubernetes (kube-probe, ELB).',
};
/** Fixed colour slot per tier (Clients 1, Services 2, Data 3, External 4) so a tier keeps its colour when others are empty. */
const laneSlot = (label: string) => Math.max(0, LANES.findIndex((l) => l.label === label)) + 1;
const callerHint = (n: MapNode) => (n.tier === 'client' && !isService(n) ? CALLER_HINTS[n.id] : undefined);
const KIND_LABEL: Record<string, string> = {
  http: 'HTTP service', worker: 'Worker', consumer: 'Consumer', web: 'Web app', mobile: 'Mobile app', service: 'Service',
  db: 'Database', cache: 'Cache', queue: 'Queue', ai: 'AI model', partner: 'Partner', webhook: 'Webhook', probe: 'Probe',
};
const LANES: { tier: MapTier; label: string }[] = [
  { tier: 'client', label: 'Clients' }, { tier: 'service', label: 'Services' }, { tier: 'data', label: 'Data' }, { tier: 'external', label: 'External' },
];

function isService(n: MapNode) {
  return services.some((s) => s.id === n.id);
}
const displayName = (n: MapNode) => (n.tier === 'client' && !isService(n) && CALLER_NAMES[n.id]) || n.name;
const kindLabel = (n: MapNode) => (n.tier === 'external' ? 'External API' : n.system ? `${KIND_LABEL[n.kind] ?? n.kind} · ${n.system}` : KIND_LABEL[n.kind] ?? n.kind);
const clip = (s: string, max = 24) => (s.length > max ? `${s.slice(0, max - 1)}…` : s);
/** Box width fits both lines: name (mono 14 px ≈ 8.4 px/char) and metric (mono 12 px ≈ 7.2 px/char), plus icon and padding. */
const nodeW = (label: string, metric: string) => Math.round(Math.min(Math.max(clip(label).length * 8.4, clip(metric, 28).length * 7.2, 120) + 62, 290));
const rank: Record<Health, number> = { ok: 0, warning: 1, critical: 2 };

interface Placed { n: MapNode; x: number; y: number; w: number; label: string; rpm: number }
interface Layout { nodes: Placed[]; edges: MapEdge[]; width: number; height: number; lanes: { label: string; y: number; sep: number }[] }

/** Lanes by tier; service lane split by call depth; wrapped rows; barycentre ordering. */
function layout(nodes: MapNode[], edges: MapEdge[]): Layout {
  const byId = new Map(nodes.map((n) => [n.id, n]));
  const es = edges.filter((e) => byId.has(e.from) && byId.has(e.to));
  // service depth = longest chain of service callers (cycles capped by the iteration count)
  const depth = new Map<string, number>();
  nodes.filter((n) => n.tier === 'service').forEach((n) => depth.set(n.id, 0));
  for (let i = 0; i < 4; i++) {
    for (const e of es) {
      if (byId.get(e.from)?.tier === 'service' && byId.get(e.to)?.tier === 'service' && e.from !== e.to) {
        depth.set(e.to, Math.max(depth.get(e.to)!, Math.min(depth.get(e.from)! + 1, 3)));
      }
    }
  }
  const groups: { label: string; ids: string[] }[] = [];
  for (const l of LANES) {
    const of = nodes.filter((n) => n.tier === l.tier);
    if (!of.length) continue;
    if (l.tier === 'service') {
      const max = Math.max(...of.map((n) => depth.get(n.id)!));
      for (let d = 0; d <= max; d++) {
        const ids = of.filter((n) => depth.get(n.id) === d).map((n) => n.id);
        if (ids.length) groups.push({ label: groups.some((g) => g.label === l.label) ? '' : l.label, ids: ids.sort() });
      }
    } else groups.push({ label: l.label, ids: of.map((n) => n.id).sort() });
  }
  // barycentre sweeps: order each group by the mean relative position of its neighbours in the other groups
  const neigh = new Map<string, string[]>();
  for (const e of es) {
    neigh.set(e.from, [...(neigh.get(e.from) ?? []), e.to]);
    neigh.set(e.to, [...(neigh.get(e.to) ?? []), e.from]);
  }
  const posOf = () => {
    const p = new Map<string, number>();
    groups.forEach((g) => g.ids.forEach((id, i) => p.set(id, (i + 0.5) / g.ids.length)));
    return p;
  };
  for (let sweep = 0; sweep < 6; sweep++) {
    const order = sweep % 2 === 0 ? groups : [...groups].reverse();
    for (const g of order) {
      const p = posOf();
      const key = (id: string) => {
        const ns = (neigh.get(id) ?? []).filter((x) => !g.ids.includes(x));
        return ns.length ? ns.reduce((a, x) => a + (p.get(x) ?? 0.5), 0) / ns.length : p.get(id)!;
      };
      g.ids = [...g.ids].sort((a, b) => key(a) - key(b));
    }
  }
  // place: wrap long groups into sub-rows, centre every row
  const rpmOf = (n: MapNode) => n.rpm || es.filter((e) => e.from === n.id || e.to === n.id).reduce((a, e) => a + e.callsPerMin, 0);
  const rows: { lane?: string; ids: string[]; y: number }[] = [];
  let y = PAD_Y;
  groups.forEach((g, gi) => {
    if (gi > 0) y += g.label ? LANE_GAP : SUB_GAP;
    for (let i = 0; i < g.ids.length; i += MAX_PER_ROW) {
      if (i > 0) y += SUB_GAP;
      rows.push({ lane: i === 0 && g.label ? g.label : undefined, ids: g.ids.slice(i, i + MAX_PER_ROW), y });
    }
  });
  const placed: Placed[] = [];
  const widthOf = (ids: string[]) => ids.reduce((a, id) => { const n = byId.get(id)!; return a + nodeW(displayName(n), metricText(n, rpmOf(n))); }, 0) + GAP_X * (ids.length - 1);
  const inner = Math.max(...rows.map((r) => widthOf(r.ids)), 480);
  for (const r of rows) {
    let x = PAD_X + (inner - widthOf(r.ids)) / 2;
    for (const id of r.ids) {
      const n = byId.get(id)!;
      const label = displayName(n);
      const w = nodeW(label, metricText(n, rpmOf(n)));
      placed.push({ n, x: x + w / 2, y: r.y, w, label, rpm: rpmOf(n) });
      x += w + GAP_X;
    }
  }
  const lanes = rows.filter((r) => r.lane).map((r) => ({ label: r.lane!, y: r.y - H / 2 - 10, sep: r.y - H / 2 - LANE_GAP / 2 + 6 }));
  return { nodes: placed, edges: es, width: PAD_X + inner + 40, height: y + H / 2 + PAD_Y, lanes };
}

function edgePath(a: Placed, b: Placed) {
  if (Math.abs(a.y - b.y) < 4) {
    const dir = Math.sign(b.x - a.x) || 1;
    const x1 = a.x + (dir * a.w) / 2, x2 = b.x - (dir * b.w) / 2;
    return `M${x1} ${a.y} C${x1 + dir * 30} ${a.y - 34} ${x2 - dir * 30} ${b.y - 34} ${x2} ${b.y}`;
  }
  const down = b.y > a.y;
  const y1 = a.y + (down ? H / 2 : -H / 2), y2 = b.y + (down ? -H / 2 : H / 2), my = (y1 + y2) / 2;
  return `M${a.x} ${y1} C${a.x} ${my} ${b.x} ${my} ${b.x} ${y2}`;
}

function metricText(n: MapNode, rpm: number) {
  return rpm > 0 ? `${compact(rpm)}/min${n.p95 > 0 ? ` · p95 ${ms(n.p95)}` : ''}` : kindLabel(n);
}
const metricLine = (p: Placed) => metricText(p.n, p.rpm);

/** Everything `id` reaches downstream plus everything that reaches it upstream. */
function neighbourhood(id: string, edges: MapEdge[]): Set<string> {
  const keep = new Set([id]);
  const walk = (down: boolean) => {
    const queue = [id];
    const seen = new Set([id]);
    while (queue.length) {
      const cur = queue.shift()!;
      for (const e of edges) {
        const next = down ? (e.from === cur ? e.to : undefined) : (e.to === cur ? e.from : undefined);
        if (next && !seen.has(next)) { seen.add(next); keep.add(next); queue.push(next); }
      }
    }
  };
  walk(true);
  walk(false);
  return keep;
}

type Box = { x: number; y: number; w: number; h: number };
const MIN_W = 320;
const PANEL_PX = 368;
const FIT_MIN_W = 1100;
const FIT_MIN_H = 520;

/** Wheel zoom around the pointer, drag to pan, buttons; `vb` null = fit the whole map. */
function useViewport(view: Layout | null) {
  const svgRef = useRef<SVGSVGElement>(null);
  const [vb, setVb] = useState<Box | null>(null);
  // fit never zooms in past ~1:1 on small maps (a single service would otherwise fill the canvas)
  const full: Box | null = view ? (() => {
    const w = Math.max(view.width, FIT_MIN_W), h = Math.max(view.height, FIT_MIN_H);
    return { x: (view.width - w) / 2, y: (view.height - h) / 2, w, h };
  })() : null;
  const cur = vb ?? full;
  const curRef = useRef(cur);
  curRef.current = cur;
  const drag = useRef<{ px: number; py: number; box: Box; k: number; moved: boolean } | null>(null);
  // refit only when the layout itself changes (nodes or size), not on every 60 s data refresh: keeps the user's zoom/pan
  const shape = view ? `${Math.round(view.width)}x${Math.round(view.height)}:${view.nodes.map((p) => p.n.id).join(',')}` : '';
  useEffect(() => { setVb(null); }, [shape]);

  const toSvg = (cx: number, cy: number) => {
    const ctm = svgRef.current?.getScreenCTM();
    return ctm ? new DOMPoint(cx, cy).matrixTransform(ctm.inverse()) : null;
  };
  const zoomAt = (factor: number, cx?: number, cy?: number) => {
    const box = curRef.current;
    if (!box || !view) return;
    const r = svgRef.current?.getBoundingClientRect();
    const p = cx != null && cy != null ? toSvg(cx, cy) : r ? toSvg(r.left + r.width / 2, r.top + r.height / 2) : null;
    if (!p) return;
    const w = Math.min(Math.max(box.w * factor, MIN_W), view.width * 2.5);
    const k = w / box.w;
    setVb({ x: p.x - (p.x - box.x) * k, y: p.y - (p.y - box.y) * k, w, h: box.h * k });
  };
  useEffect(() => {
    const svg = svgRef.current;
    if (!svg) return;
    const onWheel = (e: WheelEvent) => {
      e.preventDefault();
      zoomAt(Math.exp(e.deltaY * (e.ctrlKey ? 0.01 : 0.0015)), e.clientX, e.clientY);
    };
    svg.addEventListener('wheel', onWheel, { passive: false });
    return () => svg.removeEventListener('wheel', onWheel);
  });
  const handlers = {
    onPointerDown: (e: ReactPointerEvent<SVGSVGElement>) => {
      const box = curRef.current;
      const ctm = svgRef.current?.getScreenCTM();
      if (!box || !ctm || e.button !== 0) return;
      drag.current = { px: e.clientX, py: e.clientY, box, k: 1 / ctm.a, moved: false };
    },
    onPointerMove: (e: ReactPointerEvent<SVGSVGElement>) => {
      const d = drag.current;
      if (!d) return;
      const dx = e.clientX - d.px, dy = e.clientY - d.py;
      if (!d.moved && Math.hypot(dx, dy) < 4) return;
      if (!d.moved) { d.moved = true; svgRef.current?.setPointerCapture(e.pointerId); }
      setVb({ ...d.box, x: d.box.x - dx * d.k, y: d.box.y - dy * d.k });
    },
    onPointerUp: () => { setTimeout(() => { drag.current = null; }, 0); },
  };
  const dragged = () => !!drag.current?.moved;
  const centerOn = (x: number, y: number) => {
    const r = svgRef.current?.getBoundingClientRect();
    if (!r || !view) return;
    const w = Math.min(curRef.current?.w ?? view.width, 1100);
    const h = w * (r.height / r.width);
    // the details panel covers the left side: centre the node in the visible part
    const panelPx = Math.min(r.width * 0.45, PANEL_PX);
    setVb({ x: x - w / 2 - (panelPx / 2) * (w / r.width), y: y - h / 2, w, h });
  };
  /** Layout point → px inside the canvas (for HTML tooltips). */
  const toCanvas = (x: number, y: number) => {
    const svg = svgRef.current;
    const ctm = svg?.getScreenCTM();
    const host = svg?.parentElement?.getBoundingClientRect();
    if (!ctm || !host) return null;
    const p = new DOMPoint(x, y).matrixTransform(ctm);
    return { left: p.x - host.left, top: p.y - host.top };
  };
  return { svgRef, box: cur, zoomed: vb != null, zoomAt, fit: () => setVb(null), handlers, dragged, centerOn, toCanvas };
}

export function ServiceMapScreen({ serviceId }: { serviceId?: string } = {}) {
  useData();
  const { range } = useApp();
  const nav = useNavigate();
  const animate = useMotionAllowed();
  const map = serviceMap(range);
  const [deps, setDeps] = useState(true);
  const [scope, setScope] = useState<'all' | 'problems'>('all');
  const [hover, setHover] = useState<string>();
  const [hoverEdge, setHoverEdge] = useState<string>();
  const [selected, setSelected] = useState<string>();
  const strip = useRef<HTMLDivElement>(null);

  const view = useMemo(() => {
    if (!map) return null;
    let nodes = map.nodes.filter((n) => deps || n.tier === 'client' || n.tier === 'service');
    let ids = new Set(nodes.map((n) => n.id));
    let edges = map.edges.filter((e) => ids.has(e.from) && ids.has(e.to));
    if (serviceId) {
      const keep = neighbourhood(serviceId, edges);
      nodes = nodes.filter((n) => keep.has(n.id));
      edges = edges.filter((e) => keep.has(e.from) && keep.has(e.to));
    }
    if (scope === 'problems') {
      const bad = new Set(nodes.filter((n) => n.status !== 'ok' || n.incident).map((n) => n.id));
      edges.filter((e) => e.status !== 'ok').forEach((e) => { bad.add(e.from); bad.add(e.to); });
      if (serviceId) bad.add(serviceId);
      const keep = new Set(bad);
      edges.forEach((e) => { if (bad.has(e.from) || bad.has(e.to)) { keep.add(e.from); keep.add(e.to); } });
      nodes = nodes.filter((n) => keep.has(n.id));
      ids = new Set(nodes.map((n) => n.id));
      edges = edges.filter((e) => ids.has(e.from) && ids.has(e.to) && (bad.has(e.from) || bad.has(e.to)));
    }
    return layout(nodes, edges);
  }, [map, deps, scope, serviceId]);
  const vp = useViewport(view);

  const controls = (
    <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
      <span className="row" style={{ gap: 'var(--space-2)' }}><Toggle checked={deps} onChange={setDeps} label="Dependencies" /><span className="secondary" aria-hidden>Dependencies</span></span>
      <Segmented label="Show" value={scope} onChange={setScope} options={[{ id: 'all', label: 'All' }, { id: 'problems', label: 'Problems' }]} />
      {serviceId && <Link className="btn btn--ghost btn--sm" to="/service-map">Whole system</Link>}
    </div>
  );
  const header = serviceId
    ? <div className="svcmap__bar"><p className="secondary">What {serviceId} calls and what calls it, over the last {range}.</p>{controls}</div>
    : <PageHeader title="Service map" subtitle={`How your apps, services and dependencies call each other over the last ${range}.`} actions={controls} />;
  if (!map || !view || !vp.box) return <>{header}<SkeletonPage label="Loading service map" /></>;
  if (!view.nodes.length) {
    return (
      <>
        {header}
        <ConnectSdk icon={<Network />} title={serviceId ? 'No traffic for this service in this range' : 'No services reporting yet'} setup={!serviceId}>
          The map draws itself from traces once a service sends its first requests.
        </ConnectSdk>
      </>
    );
  }

  const byId = new Map(view.nodes.map((p) => [p.n.id, p]));
  const focus = hover ?? selected;
  const linked = (e: MapEdge) => !focus || e.from === focus || e.to === focus;
  const near = (id: string) => !focus || id === focus || view.edges.some((e) => linked(e) && (e.from === id || e.to === id));
  const problems = view.nodes.map((p) => p.n).filter((n) => n.incident || n.status !== 'ok')
    .sort((a, b) => Number(!!b.incident) - Number(!!a.incident) || rank[b.status] - rank[a.status]);
  const sel = selected ? map.nodes.find((n) => n.id === selected) : undefined;
  const hovered = hover ? byId.get(hover) : undefined;
  const he = hoverEdge ? view.edges.find((e) => e.id === hoverEdge) : undefined;
  const open = (id: string) => {
    setSelected(id);
    const p = byId.get(id);
    if (p) vp.centerOn(p.x, p.y);
  };
  const tipAt = (x: number, y: number) => {
    const p = vp.toCanvas(x, y);
    return p ? { left: p.left, top: p.top } : { display: 'none' };
  };
  const scrollStrip = (dir: number) => strip.current?.scrollBy({ left: dir * strip.current.clientWidth * 0.8, behavior: 'smooth' });

  return (
    <>
      {header}
      {problems.length > 0 && (
        <section className="svcmap__problems" aria-labelledby="svcmap-problems">
          <div className="svcmap__problems-head">
            <h2 id="svcmap-problems">Needs attention <span className="muted">{problems.length}</span></h2>
            <div className="row" style={{ gap: 'var(--space-1)' }}>
              <Button size="sm" variant="ghost" iconOnly icon={<ChevronLeft size={16} />} aria-label="Scroll left" onClick={() => scrollStrip(-1)} />
              <Button size="sm" variant="ghost" iconOnly icon={<ChevronRight size={16} />} aria-label="Scroll right" onClick={() => scrollStrip(1)} />
            </div>
          </div>
          <div className="svcmap__cards" ref={strip}>
            {problems.map((n) => {
              const st = n.status === 'ok' ? 'warning' : n.status;
              const CardIcon = iconOf(n);
              return (
                <button key={n.id} type="button" className={`svcmap__card is-${st}`} aria-pressed={selected === n.id} onClick={() => open(n.id)}>
                  <span className="svcmap__card-top">
                    <CardIcon width={16} height={16} aria-hidden />
                    <span className="svcmap__card-name">{displayName(n)}</span>
                    <StatusBadge status={st} />
                  </span>
                  <span className="svcmap__card-why">{n.incident?.title ?? (n.errorRate >= 0.01 ? `${pct(n.errorRate)} of calls fail` : `${kindLabel(n)} degraded`)}</span>
                  <span className="svcmap__card-num">{compact(n.rpm)}/min · {pct(n.errorRate)} errors{n.p95 > 0 ? ` · p95 ${ms(n.p95)}` : ''}</span>
                </button>
              );
            })}
          </div>
        </section>
      )}
      <div className={`svcmap${serviceId ? ' svcmap--embedded' : ''}`}>
        <div className="svcmap__canvas">
          <svg ref={vp.svgRef} className={`smap svcmap__svg${vp.zoomed ? ' is-zoomed' : ''}`}
            viewBox={`${vp.box.x} ${vp.box.y} ${vp.box.w} ${vp.box.h}`}
            data-static={animate ? undefined : ''} role="img" aria-label={`Service map: ${view.nodes.length} nodes, ${view.edges.length} connections`}
            {...vp.handlers}
            onClick={() => { if (!vp.dragged()) setSelected(undefined); }}>
            <defs>{view.edges.map((e) => <path key={e.id} id={`svcmap-${e.id}`} d={edgePath(byId.get(e.from)!, byId.get(e.to)!)} />)}</defs>
            <g aria-hidden>
              {view.lanes.map((l, i) => {
                // tier band: from this lane's separator to the next one; the first and last bands run off the canvas
                const top = i === 0 ? -view.height * 3 : l.sep;
                const bottom = i === view.lanes.length - 1 ? view.height * 4 : view.lanes[i + 1].sep;
                return <rect key={`band-${l.label}`} x={-view.width * 3} width={view.width * 7} y={top} height={bottom - top} className={`svcmap__band svcmap__band--${laneSlot(l.label)}`} />;
              })}
              {view.lanes.map((l, i) => (
                <g key={l.label}>
                  {i > 0 && <line x1={-view.width * 3} x2={view.width * 4} y1={l.sep} y2={l.sep} className="smap__sep" />}
                  <text x={16} y={l.y + H / 2 + 14} className={`smap__lane svcmap__lane--${laneSlot(l.label)}`}>{l.label}</text>
                </g>
              ))}
            </g>
            <g>
              {view.edges.map((e, i) => {
                const w = Math.min(1 + Math.log10(1 + e.callsPerMin) * 0.9, 3.5);
                return (
                  <path key={e.id} d={edgePath(byId.get(e.from)!, byId.get(e.to)!)} pathLength={1} strokeWidth={w}
                    className={`smap__edge svcmap__edge is-${e.status}${linked(e) ? '' : ' is-dim'}${hoverEdge === e.id ? ' is-focus' : ''}`}
                    style={{ ['--d' as string]: `${Math.min(i * 0.01, 0.4)}s` }} />
                );
              })}
              {view.edges.map((e) => (
                <path key={`hit-${e.id}`} d={edgePath(byId.get(e.from)!, byId.get(e.to)!)} className="svcmap__hit"
                  onPointerEnter={() => setHoverEdge(e.id)} onPointerLeave={() => setHoverEdge(undefined)} />
              ))}
            </g>
            {animate && view.edges.length <= 160 && (
              <g aria-hidden>
                {view.edges.map((e, i) => (
                  <circle key={e.id} r={e.status === 'ok' ? 2.2 : 3} className={`svcmap__dot is-${e.status}${linked(e) ? '' : ' is-dim'}`}>
                    <animateMotion dur={`${Math.max(1.2, 4 - Math.log10(1 + e.callsPerMin))}s`} begin={`${-i * 0.37}s`} repeatCount="indefinite">
                      <mpath href={`#svcmap-${e.id}`} />
                    </animateMotion>
                  </circle>
                ))}
              </g>
            )}
            <g>
              {view.nodes.map((p, i) => {
                const NodeIcon = iconOf(p.n);
                const StatusIcon = p.n.status === 'critical' ? AlertOctagon : AlertTriangle;
                const go = isService(p.n);
                return (
                  <g key={p.n.id} transform={`translate(${p.x - p.w / 2} ${p.y - H / 2})`}
                    className={`smap__node svcmap__node is-${p.n.status}${near(p.n.id) ? '' : ' is-dim'}${selected === p.n.id || serviceId === p.n.id ? ' is-selected' : ''}`}
                    style={{ ['--d' as string]: `${Math.min(i * 0.015, 0.45)}s` }}
                    role="button" tabIndex={0} aria-label={`${p.label}, ${kindLabel(p.n)}, ${p.n.status}${p.n.incident ? `, incident: ${p.n.incident.title}` : ''}`}
                    onPointerEnter={() => setHover(p.n.id)} onPointerLeave={() => setHover(undefined)}
                    onClick={(ev) => { ev.stopPropagation(); if (!vp.dragged()) setSelected(p.n.id); }}
                    onDoubleClick={() => go && p.n.id !== serviceId && nav(`/services/${encodeURIComponent(p.n.id)}`)}
                    onKeyDown={(ev) => { if (ev.key === 'Enter' || ev.key === ' ') { ev.preventDefault(); setSelected(p.n.id); } }}>
                    <g className="smap__pop">
                      {p.n.status === 'critical' && <rect className="smap__ripple svcmap__ripple" width={p.w} height={H} rx={12} />}
                      <rect className="smap__box" width={p.w} height={H} rx={12} />
                      <NodeIcon x={14} y={(H - 18) / 2} width={18} height={18} className="smap__icon" aria-hidden />
                      <text x={42} y={22} className="smap__name">{clip(p.label)}</text>
                      <text x={42} y={39} className="smap__metric">{clip(metricLine(p), 28)}</text>
                      {p.n.status !== 'ok' && <StatusIcon x={p.w - 22} y={8} width={14} height={14} className="svcmap__status" aria-hidden />}
                    </g>
                    {p.n.incident && (
                      <g transform={`translate(${p.w / 2} -12)`}>
                        <g className={`smap__tag svcmap__tag is-${p.n.incident.severity === 'critical' ? 'critical' : 'warning'}`}>
                          <rect x={-44} y={-11} width={88} height={20} rx={10} />
                          <text y={3.5} textAnchor="middle">{p.n.incident.severity === 'critical' ? 'Incident' : 'Warning'}</text>
                        </g>
                      </g>
                    )}
                  </g>
                );
              })}
            </g>
          </svg>
          <div className="svcmap__zoom" role="group" aria-label="Zoom">
            <Button size="sm" iconOnly icon={<Plus size={14} />} aria-label="Zoom in" onClick={() => vp.zoomAt(0.75)} />
            <Button size="sm" iconOnly icon={<Minus size={14} />} aria-label="Zoom out" onClick={() => vp.zoomAt(1 / 0.75)} />
            <Button size="sm" iconOnly icon={<Maximize2 size={14} />} aria-label="Fit the map" onClick={vp.fit} />
          </div>
          {hovered && hovered.n.id !== selected && (
            <div className="svcmap__tip" role="status" style={tipAt(hovered.x, hovered.y + H / 2)}>
              <strong>{hovered.label}</strong>
              <span className="svcmap__tip-kind">{kindLabel(hovered.n)}</span>
              {callerHint(hovered.n) && <span className="svcmap__tip-why">{callerHint(hovered.n)}</span>}
              <span>{compact(hovered.rpm)}/min · {pct(hovered.n.errorRate)} errors{hovered.n.p95 > 0 ? ` · p95 ${ms(hovered.n.p95)}` : ''}</span>
              {hovered.n.incident && <span className="svcmap__tip-inc">{hovered.n.incident.title}</span>}
              <span className="svcmap__tip-hint">Click for details</span>
            </div>
          )}
          {he && !hovered && (() => {
            const a = byId.get(he.from)!, b = byId.get(he.to)!;
            return (
              <div className="svcmap__tip" role="status" style={tipAt((a.x + b.x) / 2, (a.y + b.y) / 2)}>
                <strong>{a.label} → {b.label}</strong>
                <span>{compact(he.callsPerMin)} calls/min · {pct(he.errorRate)} errors · p95 {ms(he.p95)}</span>
              </div>
            );
          })()}
          {sel && <NodePanel key={sel.id} n={sel} map={map} current={serviceId} onClose={() => setSelected(undefined)} />}
        </div>
      </div>
    </>
  );
}

function NodePanel({ n, map, current, onClose }: { n: MapNode; map: { nodes: MapNode[]; edges: MapEdge[] }; current?: string; onClose: () => void }) {
  const name = (id: string) => {
    const x = map.nodes.find((m) => m.id === id);
    return x ? displayName(x) : id;
  };
  const inbound = map.edges.filter((e) => e.to === n.id).sort((a, b) => b.callsPerMin - a.callsPerMin);
  const outbound = map.edges.filter((e) => e.from === n.id).sort((a, b) => b.callsPerMin - a.callsPerMin);
  const svc = isService(n) && n.id !== current;
  const row = (e: MapEdge, id: string) => (
    <li key={e.id}>
      <span className="svcmap__panel-peer">{e.status !== 'ok' && <StatusBadge status={e.status} label={e.status === 'critical' ? 'Critical' : 'Warning'} />}{name(id)}</span>
      <span className="svcmap__panel-num">{compact(e.callsPerMin)}/min · {pct(e.errorRate)} · {ms(e.p95)}</span>
    </li>
  );
  return (
    <aside className="svcmap__panel" aria-label={`${displayName(n)} details`} onClick={(e) => e.stopPropagation()}>
      <header className="svcmap__panel-head">
        <div>
          <h2>{displayName(n)}</h2>
          <p>{kindLabel(n)}</p>
          {callerHint(n) && <p>{callerHint(n)}</p>}
        </div>
        <Button size="sm" variant="ghost" iconOnly icon={<X size={14} />} onClick={onClose} aria-label="Close details" />
      </header>
      <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status={n.status} /></div>
      {n.incident && (
        <Link className="svcmap__panel-inc" to={`/incidents?id=${encodeURIComponent(n.incident.id)}`}>
          <StatusBadge status={n.incident.state === 'acknowledged' ? 'acknowledged' : 'firing'} />
          <span>{n.incident.title}</span>
          {n.incident.open > 1 && <span className="muted">+{n.incident.open - 1} more open</span>}
        </Link>
      )}
      <dl className="svcmap__panel-stats">
        <div><dt>Throughput</dt><dd>{compact(n.rpm || inbound.reduce((a, e) => a + e.callsPerMin, 0))}/min</dd></div>
        <div><dt>Errors</dt><dd>{pct(n.errorRate)}</dd></div>
        <div><dt>p95</dt><dd>{ms(n.p95)}</dd></div>
      </dl>
      {inbound.length > 0 && (<><h3>Called by</h3><ul className="svcmap__panel-list">{inbound.map((e) => row(e, e.from))}</ul></>)}
      {outbound.length > 0 && (<><h3>Calls</h3><ul className="svcmap__panel-list">{outbound.map((e) => row(e, e.to))}</ul></>)}
      {svc && <Link className="btn btn--primary btn--sm svcmap__panel-open" to={`/services/${encodeURIComponent(n.id)}`}>Open service</Link>}
    </aside>
  );
}
