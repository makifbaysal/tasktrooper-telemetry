import { useCallback, useEffect, useMemo, useRef, useState, type KeyboardEvent, type ReactNode } from 'react';
import { Link, useNavigate, useParams, useSearchParams } from 'react-router';
import { AlertOctagon, ArrowRight, Boxes, ChevronDown, ChevronRight, FileText, ListTree, Loader2, Server, X } from 'lucide-react';
import { Badge, Button, Callout, Card, CodeBlock, EmptyState, PageHeader, StatTile, StatusBadge } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  TIME_CATEGORIES, endpointsFor, k8sPods, services, traceDetail, useData,
  type Endpoint, type Service, type TraceDetail as Trace, type TraceSpan,
} from '@/data';
import { ago, clock, dateTime, int, ms, pct } from '@/lib/format';
import { LINE_SLOTS } from './shared';
import { clusterUrl, hostUrl, podUrl } from './k8sShared';
import { LlmSpanSection } from './AiMonitoring';

/** Rows rendered at most; larger traces show a notice (collapse subtrees to reach the rest). */
const MAX_ROWS = 2000;
/** Above this many spans, subtrees deeper than level 2 start collapsed. */
const AUTO_COLLAPSE = 600;
/** Categorical order for services within one trace (yellow last, see design-system.md § Data viz). */
const SLOTS = [...LINE_SLOTS, 4];
const OTHER = 'var(--text-muted)';

const KIND_LABEL: Record<TraceSpan['kind'], string> = { server: 'Server', client: 'Client', producer: 'Producer', consumer: 'Consumer', internal: 'Internal' };
const catLabel = (c: TraceSpan['category']) => TIME_CATEGORIES.find((x) => x.id === c)?.label ?? c;

// ---------------------------------------------------------------- tree model
interface TNode { span: TraceSpan; depth: number; parent?: TNode; children: TNode[]; self: number }
interface Model { roots: TNode[]; byId: Map<string, TNode>; order: TNode[]; maxDepth: number; colors: Map<string, string>; legend: { label: string; color: string }[] }

function buildModel(t: Trace): Model {
  const byId = new Map<string, TNode>();
  for (const s of t.spans) byId.set(s.spanId, { span: s, depth: 0, children: [], self: s.dur });
  const roots: TNode[] = [];
  for (const n of byId.values()) {
    const p = n.span.parentSpanId ? byId.get(n.span.parentSpanId) : undefined;
    // spans whose parent is missing (partial trace) are shown as roots
    if (p && p !== n) { n.parent = p; p.children.push(n); } else roots.push(n);
  }
  const byStart = (a: TNode, b: TNode) => a.span.start - b.span.start || b.span.dur - a.span.dur;
  roots.sort(byStart);
  const order: TNode[] = [];
  let maxDepth = 0;
  // iterative DFS: deep traces must not overflow the stack
  const stack = [...roots].reverse().map((n) => ({ n, d: 0 }));
  while (stack.length) {
    const { n, d } = stack.pop()!;
    n.depth = d; maxDepth = Math.max(maxDepth, d);
    n.children.sort(byStart);
    n.self = Math.max(0, n.span.dur - n.children.reduce((a, c) => a + c.span.dur, 0));
    order.push(n);
    for (let i = n.children.length - 1; i >= 0; i--) stack.push({ n: n.children[i], d: d + 1 });
  }
  // color per service in order of first appearance; 9th+ service → "Other"
  const colors = new Map<string, string>();
  for (const n of order) if (!colors.has(n.span.service)) colors.set(n.span.service, colors.size < SLOTS.length ? `var(--series-${SLOTS[colors.size]})` : OTHER);
  const legend = [...colors].filter(([, c]) => c !== OTHER).map(([label, color]) => ({ label, color }));
  if ([...colors.values()].includes(OTHER)) legend.push({ label: 'Other', color: OTHER });
  return { roots, byId, order, maxDepth, colors, legend };
}

const findService = (name: string): Service | undefined => services.find((s) => s.name === name || s.id === name);
const methodOf = (s: TraceSpan) => s.attrs['http.request.method'] ?? s.attrs['http.method'] ?? '';
function endpointOf(s: TraceSpan): Endpoint | undefined {
  const svc = findService(s.service);
  if (!svc || (s.kind !== 'server' && s.kind !== 'consumer')) return undefined;
  const route = s.route ?? s.attrs['http.route'];
  if (!route) return undefined;
  const eps = endpointsFor(svc.id);
  return eps.find((e) => e.sub ? e.sub.process === route : e.route === route && (!methodOf(s) || e.method === methodOf(s)));
}
const logsUrl = (svc: Service, traceId: string) => `/services/${encodeURIComponent(svc.id)}?${new URLSearchParams({ tab: 'logs', traceId })}`;

// ---------------------------------------------------------------- screen
export function TraceDetail() {
  useData();
  const { traceId = '' } = useParams();
  const st = traceDetail(traceId);
  if (!st.trace) {
    if (st.loading) return <EmptyState icon={<Loader2 />} title="Loading trace…" />;
    return (
      <EmptyState icon={<ListTree />} title={st.notFound || !st.error ? 'Trace not found' : 'Could not load the trace'}
        action={<Link to="/services">Back to services</Link>}>
        {st.notFound || !st.error
          ? <>No spans for <span className="mono">{traceId}</span>. It may be outside the retention window, or it was sampled out.</>
          : st.error}
      </EmptyState>
    );
  }
  return <TraceView key={st.trace.id} trace={st.trace} />;
}

function TraceView({ trace }: { trace: Trace }) {
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const model = useMemo(() => buildModel(trace), [trace]);
  const [collapsed, setCollapsed] = useState<Set<string>>(() => new Set(trace.spans.length > AUTO_COLLAPSE
    ? model.order.filter((n) => n.depth >= 2 && n.children.length).map((n) => n.span.spanId) : []));
  const selectedId = params.get('span') ?? '';
  const selected = selectedId ? model.byId.get(selectedId) : undefined;
  const select = useCallback((id: string | null) => setParams((p) => {
    const n = new URLSearchParams(p);
    if (id) n.set('span', id); else n.delete('span');
    return n;
  }, { replace: true }), [setParams]);
  const [focusId, setFocusId] = useState<string>(() => selectedId || model.order[0]?.span.spanId || '');
  const treeRef = useRef<HTMLDivElement>(null);

  // a selected span inside a collapsed subtree (deep link) is revealed
  useEffect(() => {
    if (!selected) return;
    const hidden: string[] = [];
    for (let p = selected.parent; p; p = p.parent) if (collapsed.has(p.span.spanId)) hidden.push(p.span.spanId);
    if (hidden.length) setCollapsed((c) => { const n = new Set(c); hidden.forEach((h) => n.delete(h)); return n; });
  }, [selected]); // eslint-disable-line react-hooks/exhaustive-deps

  const visible = useMemo(() => {
    const out: TNode[] = [];
    let skipDepth = Infinity;
    for (const n of model.order) {
      if (n.depth > skipDepth) continue;
      skipDepth = collapsed.has(n.span.spanId) ? n.depth : Infinity;
      out.push(n);
    }
    return out;
  }, [model, collapsed]);
  const rows = visible.length > MAX_ROWS ? visible.slice(0, MAX_ROWS) : visible;

  const root = model.roots[0]?.span;
  const failed = trace.spans.filter((s) => s.failed);
  const status = root?.status ?? 0;
  // the backend service to scope logs to: the failing one, else the first server span of a known service
  const mainSvc = [...failed, ...trace.spans.filter((s) => s.kind === 'server')].map((s) => findService(s.service)).find(Boolean);

  const toggle = (id: string) => setCollapsed((c) => { const n = new Set(c); if (n.has(id)) n.delete(id); else n.add(id); return n; });
  const setAll = (open: boolean) => setCollapsed(open ? new Set() : new Set(model.order.filter((n) => n.depth >= 1 && n.children.length).map((n) => n.span.spanId)));

  const focusRow = (id: string) => {
    setFocusId(id);
    requestAnimationFrame(() => treeRef.current?.querySelector<HTMLElement>(`[data-span="${CSS.escape(id)}"]`)?.focus());
  };
  const onKey = (e: KeyboardEvent<HTMLDivElement>) => {
    const i = rows.findIndex((r) => r.span.spanId === focusId);
    const cur = rows[i];
    if (!cur) return;
    const id = cur.span.spanId;
    const open = cur.children.length > 0 && !collapsed.has(id);
    let handled = true;
    switch (e.key) {
      case 'ArrowDown': if (rows[i + 1]) focusRow(rows[i + 1].span.spanId); break;
      case 'ArrowUp': if (rows[i - 1]) focusRow(rows[i - 1].span.spanId); break;
      case 'Home': focusRow(rows[0].span.spanId); break;
      case 'End': focusRow(rows[rows.length - 1].span.spanId); break;
      case 'ArrowRight':
        if (cur.children.length && !open) toggle(id);
        else if (open) focusRow(cur.children[0].span.spanId);
        break;
      case 'ArrowLeft':
        if (open) toggle(id);
        else if (cur.parent) focusRow(cur.parent.span.spanId);
        break;
      case 'Enter': case ' ': select(id); break;
      case 'Escape': if (selected) select(null); else handled = false; break;
      default: handled = false;
    }
    if (handled) e.preventDefault();
  };

  const ticks = [0, 0.25, 0.5, 0.75, 1];
  const rootSvc = root && findService(root.service);

  return (
    <>
      <PageHeader
        crumbs={[{ label: 'Services', to: '/services' }, ...(mainSvc ? [{ label: mainSvc.name, to: `/services/${mainSvc.id}` }] : []), { label: `Trace ${trace.id.slice(0, 12)}` }]}
        title={root ? <span className="row trace__title"><span className="legend__swatch" style={{ background: model.colors.get(root.service) }} aria-hidden />{root.service} <span className="mono">{root.name}</span></span> : 'Trace'}
        subtitle={<span className="row trace__meta">
          {trace.errors > 0
            ? <StatusBadge status="critical" label={`${trace.errors} error span${trace.errors === 1 ? '' : 's'}`} />
            : <StatusBadge status="ok" label="No errors" />}
          {status > 0 && (status >= 500 ? <Badge tone="critical">HTTP {status}</Badge> : <Badge>HTTP {status}</Badge>)}
          <span className="mono muted">{trace.id}</span>
        </span>}
        actions={mainSvc && <Button icon={<FileText size={14} />} onClick={() => nav(logsUrl(mainSvc, trace.id))}>Logs for this trace</Button>} />

      <div className="grid grid-4">
        <StatTile label="Total duration" value={ms(trace.dur)} />
        <StatTile label="Spans" value={int(trace.spans.length)} unit={`· depth ${model.maxDepth + 1}`} />
        <StatTile label="Services" value={int(model.colors.size)} unit={rootSvc ? `· root ${rootSvc.name}` : undefined} />
        <StatTile label="Started" value={clock(trace.ts, true)} unit={`· ${dateTime(trace.ts)} · ${ago(trace.ts)}`} />
      </div>

      <div className={`trace${selected ? ' trace--panel' : ''}`}>
        <Card flush title="Waterfall"
          subtitle="Arrow keys move, → / ← expand and collapse, Enter opens the span"
          actions={<>
            <Button size="sm" variant="ghost" onClick={() => setAll(true)}>Expand all</Button>
            <Button size="sm" variant="ghost" onClick={() => setAll(false)}>Collapse all</Button>
          </>}>
          <div className="wf__legend">
            <div className="legend">
              {model.legend.map((l) => <span key={l.label} className="legend__item"><span className="legend__swatch" style={{ background: l.color }} />{l.label}</span>)}
            </div>
          </div>
          {visible.length > MAX_ROWS && (
            <div className="wf__notice">
              <Callout icon={<AlertOctagon size={16} />}>
                Showing the first {int(MAX_ROWS)} of {int(visible.length)} rows ({int(trace.spans.length)} spans). Collapse subtrees to reach the rest.
              </Callout>
            </div>
          )}
          <div className="wf__axis" aria-hidden>
            <div className="wf__name">Span</div>
            <div className="wf__ticks">
              {ticks.map((t) => <span key={t} className="wf__tick" style={{ left: `${t * 100}%` }}>{ms(trace.dur * t)}</span>)}
            </div>
          </div>
          <div ref={treeRef} className="wf" role="tree" aria-label="Spans" aria-multiselectable={false} onKeyDown={onKey}>
            {rows.map((n) => {
              const s = n.span;
              const id = s.spanId;
              const open = n.children.length > 0 && !collapsed.has(id);
              const left = trace.dur > 0 ? (s.start / trace.dur) * 100 : 0;
              const width = trace.dur > 0 ? (s.dur / trace.dur) * 100 : 100;
              return (
                <div key={id} data-span={id} role="treeitem" aria-level={n.depth + 1} aria-selected={selectedId === id}
                  aria-expanded={n.children.length ? open : undefined} tabIndex={focusId === id ? 0 : -1}
                  className={`wf__row${selectedId === id ? ' wf__row--selected' : ''}`}
                  onClick={() => { setFocusId(id); select(id); }} onFocus={() => setFocusId(id)}>
                  <div className="wf__name" style={{ paddingLeft: `calc(var(--space-2) + var(--space-4) * ${n.depth})` }}>
                    {n.children.length
                      ? <button type="button" className="wf__toggle" tabIndex={-1} aria-label={open ? 'Collapse' : 'Expand'}
                          onClick={(e) => { e.stopPropagation(); toggle(id); }}>{open ? <ChevronDown size={14} /> : <ChevronRight size={14} />}</button>
                      : <span className="wf__toggle" aria-hidden />}
                    <span className="legend__swatch" style={{ background: model.colors.get(s.service) }} aria-hidden />
                    <span className="wf__svc">{s.service}</span>
                    <span className="mono truncate" title={s.name}>{s.name}</span>
                    {!open && n.children.length > 0 && <span className="muted wf__count">+{countDescendants(n)}</span>}
                    {s.failed && <Badge tone="critical" icon={<AlertOctagon aria-hidden />}>Error</Badge>}
                  </div>
                  <div className="wf__track">
                    <div className={`wf__bar${s.failed ? ' wf__bar--error' : ''}`}
                      style={{ left: `${left}%`, width: `max(var(--space-1) / 2, ${width}%)`, background: model.colors.get(s.service) }} />
                    <span className="wf__dur num" style={left + width > 82 ? { right: `${100 - left + 0.5}%` } : { left: `calc(${left + width}% + var(--space-1))` }}>{ms(s.dur)}</span>
                  </div>
                </div>
              );
            })}
          </div>
        </Card>

        {selected && <SpanPanel node={selected} trace={trace} color={model.colors.get(selected.span.service) ?? OTHER} onClose={() => { select(null); focusRow(selected.span.spanId); }}
          onSelect={(id) => { select(id); focusRow(id); }} />}
      </div>
    </>
  );
}

function countDescendants(n: TNode): number {
  let c = 0;
  const stack = [...n.children];
  while (stack.length) { const x = stack.pop()!; c++; stack.push(...x.children); }
  return c;
}

// ---------------------------------------------------------------- span panel
function AttrList({ attrs, empty }: { attrs: Record<string, string>; empty: string }) {
  const keys = Object.keys(attrs).sort();
  if (!keys.length) return <p className="muted trace__empty">{empty}</p>;
  return (
    <dl className="attrs">
      {keys.map((k) => <div key={k} className="attrs__row"><dt className="mono">{k}</dt><dd className="mono">{attrs[k]}</dd></div>)}
    </dl>
  );
}

function PanelSection({ title, aside, children }: { title: ReactNode; aside?: ReactNode; children: ReactNode }) {
  return (
    <section className="drawer__section">
      <div className="drawer__section-head"><h3>{title}</h3>{aside && <span className="muted">{aside}</span>}</div>
      {children}
    </section>
  );
}

function SpanPanel({ node, trace, color, onClose, onSelect }: { node: TNode; trace: Trace; color: string; onClose: () => void; onSelect: (id: string) => void }) {
  const { range } = useApp();
  const s = node.span;
  const svc = findService(s.service);
  const ep = endpointOf(s);
  const r = s.resource;
  const podName = r['k8s.pod.name'];
  const ns = r['k8s.namespace.name'];
  const cluster = r['k8s.cluster.name'];
  const pods = k8sPods({ cluster: cluster || undefined, namespace: ns || undefined }, range);
  const pod = podName ? pods.find((p) => p.name === podName && (!ns || p.namespace === ns)) : undefined;
  const podLink = pod ? podUrl(pod) : cluster ? `${clusterUrl(cluster)}${ns ? `?ns=${encodeURIComponent(ns)}` : ''}` : '/infrastructure/kubernetes';
  const node_ = r['k8s.node.name'] || r['host.name'];
  const exceptions = s.events.filter((e) => e.name === 'exception');
  const others = s.events.filter((e) => e.name !== 'exception');

  return (
    <aside className="card trace__panel" aria-label="Span details">
      <header className="drawer__head">
        <div className="drawer__titles">
          <h2 className="drawer__title mono">{s.name}</h2>
          <div className="drawer__subtitle">
            <span className="row"><span className="legend__swatch" style={{ background: color }} aria-hidden />{s.service}</span>
            <Badge>{KIND_LABEL[s.kind]}</Badge>
            <Badge>{catLabel(s.category)}</Badge>
            {s.failed ? <StatusBadge status="critical" label={s.errorType || 'Error'} /> : s.status > 0 && <Badge>{s.status}</Badge>}
          </div>
        </div>
        <div className="drawer__actions">
          <button className="btn btn--ghost btn--icon" aria-label="Close span details" onClick={onClose}><X size={16} /></button>
        </div>
      </header>
      <div className="drawer__body">
        <div className="row trace__links">
          {svc && <Link to={`/services/${encodeURIComponent(svc.id)}`} className="row"><Server size={14} aria-hidden />Service {svc.name}</Link>}
          {svc && ep && <Link to={`/services/${encodeURIComponent(svc.id)}/endpoints/${encodeURIComponent(ep.id)}`} className="row"><ArrowRight size={14} aria-hidden />Endpoint {ep.sub ? ep.sub.process : `${ep.method} ${ep.route}`}</Link>}
          {svc && <Link to={logsUrl(svc, trace.id)} className="row"><FileText size={14} aria-hidden />Logs for this trace</Link>}
        </div>

        <dl className="kv">
          {([
            ['Duration', ms(s.dur)],
            ['Share of trace', trace.dur > 0 ? pct(s.dur / trace.dur, 1) : '—'],
            ['Starts at', `+${ms(s.start)}`],
            ['Self time', ms(node.self)],
            ...(s.status > 0 ? [['Status code', String(s.status)]] : []),
            ...(s.dep ? [['Calls', <span key="d" className="mono">{s.dep}</span>]] : []),
            ['Span id', <span key="i" className="mono">{s.spanId}</span>],
            ['Parent', node.parent
              ? <a key="p" href="#" className="mono" onClick={(e) => { e.preventDefault(); onSelect(node.parent!.span.spanId); }}>{node.parent.span.name}</a>
              : <span key="p" className="muted">root</span>],
          ] as [string, ReactNode][]).map(([k, v]) => <div key={k} className="kv__row"><dt>{k}</dt><dd>{v}</dd></div>)}
        </dl>

        {s.llm && <LlmSpanSection key={s.spanId} traceId={trace.id} spanId={s.spanId} llm={s.llm} />}

        {(exceptions.length > 0 || others.length > 0) && (
          <PanelSection title={exceptions.length ? 'Exceptions & events' : 'Events'} aside={`${s.events.length}`}>
            {exceptions.map((e, i) => (
              <div key={`x${i}`} className="stack trace__exception">
                <span className="row">
                  <StatusBadge status="critical" label={e.attrs['exception.type'] || 'Exception'} />
                  <span className="muted">+{ms(e.ts)}</span>
                </span>
                {e.attrs['exception.message'] && <span className="mono">{e.attrs['exception.message']}</span>}
                {e.attrs['exception.stacktrace'] && <CodeBlock>{e.attrs['exception.stacktrace']}</CodeBlock>}
              </div>
            ))}
            {others.map((e, i) => (
              <div key={`e${i}`} className="stack trace__event">
                <span className="row"><Badge>{e.name}</Badge><span className="muted">+{ms(e.ts)}</span></span>
                {Object.keys(e.attrs).length > 0 && <AttrList attrs={e.attrs} empty="" />}
              </div>
            ))}
          </PanelSection>
        )}

        <PanelSection title="Attributes" aside={`${Object.keys(s.attrs).length}`}>
          <AttrList attrs={s.attrs} empty="No span attributes." />
        </PanelSection>

        <PanelSection title="Resource" aside={podName ? 'Kubernetes' : undefined}>
          {podName && (
            <div className="row trace__links">
              <Link to={podLink} className="row"><Boxes size={14} aria-hidden />{pod ? `Pod ${podName}` : `Pods in ${ns || cluster || 'Kubernetes'}`}</Link>
              {node_ && <Link to={hostUrl(node_)} className="row"><Server size={14} aria-hidden />Node {node_}</Link>}
            </div>
          )}
          <AttrList attrs={r} empty="No resource attributes on this span." />
        </PanelSection>
      </div>
    </aside>
  );
}
