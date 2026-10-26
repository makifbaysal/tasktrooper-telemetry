import type { ReactNode } from 'react';
import { useNavigate } from 'react-router';
import {
  AlertOctagon, ArrowRight, Box, Cloud, Code2, Database, FileCode2, GitCommit, Globe, HardDrive, Inbox, Layers, Monitor, ScrollText, Server, Smartphone, Zap,
} from 'lucide-react';
import { Badge, Button, Drawer, DrawerSection, useDrawer } from '@/components/ui';
import { endpointNode, kpis, kpiTree, kpiUnit, monitors, serviceNode, services, type RcaEvidence, type RcaMetric, type RcaNode, type RcaResult } from '@/data';
import { ago, ms, pct } from '@/lib/format';
import { findRootCause } from '@/lib/rca';

// ---------------------------------------------------------------- analysis helpers
export interface Diagnosis {
  metric: RcaMetric;
  tree: RcaNode;
  result: RcaResult;
  /** where the cause sits relative to the page you're on */
  origin: 'own-code' | 'own-dependency' | 'downstream';
}

const MIN_REL = 0.3;
const MIN_ABS: Record<RcaMetric, number> = { errors: 0.001, latency: 20 };

/** Runs the drill-down for a service or endpoint; only returns metrics that clearly moved. */
export function diagnose(serviceId: string, endpointId?: string): Diagnosis[] {
  const name = services.find((s) => s.id === serviceId)?.name ?? serviceId;
  return (['errors', 'latency'] as const).flatMap((metric) => {
    const tree = endpointId ? endpointNode(serviceId, endpointId, metric) : serviceNode(serviceId, metric);
    if (!tree) return [];
    const delta = tree.current - tree.baseline;
    if (delta < MIN_ABS[metric] || delta / (tree.baseline || 1) < MIN_REL) return [];
    const result = findRootCause(tree, metric === 'errors' ? 'ratio' : 'ms');
    if (!result.path.length) return [];
    return [{ metric, tree, result, origin: originOf(result.root, name) }];
  });
}

function originOf(cause: RcaNode, serviceName: string): Diagnosis['origin'] {
  if (cause.service !== serviceName) return 'downstream';
  return cause.kind === 'code' ? 'own-code' : 'own-dependency';
}

export const fmtFor = (unit: 'ms' | 'ratio') => (unit === 'ms' ? ms : (v: number) => pct(v));

/** KPIs at risk whose root cause sits in this service — "who feels it". */
export function impactedKpis(serviceName: string) {
  return kpis.filter((k) => {
    const t = kpiTree(k);
    if (!t) return false;
    const r = findRootCause(t, kpiUnit(k));
    return r.path.some((h) => h.node.service === serviceName && h !== r.path[0]) || r.root.service === serviceName;
  });
}

// ---------------------------------------------------------------- verdict card
const ORIGIN_LABEL: Record<Diagnosis['origin'], { label: string; tone: 'critical' | 'warning' | 'accent' }> = {
  'own-code': { label: 'Your code', tone: 'critical' },
  'own-dependency': { label: 'Your dependency', tone: 'warning' },
  downstream: { label: 'Not your code · downstream', tone: 'accent' },
};

/**
 * "Why is this broken?" — the drill-down's answer at the top of a service / endpoint page.
 * Says whether the cause is the code you're looking at, something it calls directly, or deeper.
 */
export function RootCauseCard({ d, scope }: { d: Diagnosis; scope: string }) {
  const drawer = useDrawer<true>();
  const fmt = fmtFor(d.result.unit);
  const o = ORIGIN_LABEL[d.origin];
  const nav = useNavigate();
  const impacted = impactedKpis(d.result.root.service ?? '');
  const title = d.metric === 'errors'
    ? `Why ${scope}'s error rate is up (${fmt(d.tree.baseline)} → ${fmt(d.tree.current)})`
    : `Why ${scope} is slower (avg ${fmt(d.tree.baseline)} → ${fmt(d.tree.current)})`;
  return (
    <>
      <section className={`card card--interactive rca${d.origin === 'own-dependency' ? ' rca--warning' : ''}`} onClick={() => drawer.open(true)}>
        <div className="rca__head">
          <AlertOctagon size={18} className="rca__icon" color="var(--status-critical)" aria-hidden />
          <div className="rca__body">
            <div className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
              <span className="rca__title">{title}</span>
              <Badge tone={o.tone}>{o.label}</Badge>
            </div>
            <div className="rca__summary">{d.result.summary}</div>
            <PathCrumbs result={d.result} start={d.tree} />
            {impacted.length > 0 && (
              <div className="row" style={{ gap: 8, flexWrap: 'wrap', fontSize: 'var(--text-sm)' }}>
                <span className="muted">Users feel it in</span>
                {impacted.map((k) => (
                  <a key={k.id} href={`/kpis/${k.id}`} onClick={(e) => { e.preventDefault(); e.stopPropagation(); nav(`/kpis/${k.id}`); }}>
                    {k.name} <span className="muted">({(k.current * 100).toFixed(0)}% good)</span>
                  </a>
                ))}
              </div>
            )}
          </div>
          <Button size="sm" onClick={(e) => { e.stopPropagation(); drawer.open(true); }}>Analysis <ArrowRight size={14} /></Button>
        </div>
      </section>
      {drawer.item && <RcaDrawer title={title} tree={d.tree} result={d.result} onClose={drawer.close} />}
    </>
  );
}

/** "orders-api → payments-api → iyzico API" with the culprit emphasised. */
export function PathCrumbs({ result, start }: { result: RcaResult; start?: RcaNode }) {
  const nodes = [...(start ? [start] : []), ...result.path.map((h) => h.node)];
  return (
    <div className="rca__path">
      {nodes.map((n, i) => (
        <span key={n.id} className="row" style={{ gap: 8 }}>
          {i > 0 && <span className="rca__path-sep" aria-hidden>→</span>}
          <span className={i === nodes.length - 1 ? 'rca__path-root' : undefined}>
            {n.kind === 'endpoint' && n.service && n.label !== n.service ? <><span className="muted">{n.service}</span> {n.label}</> : n.label}
          </span>
        </span>
      ))}
      <span className="muted">· explains {Math.round(result.confidence * 100)}%</span>
    </div>
  );
}

// ---------------------------------------------------------------- full chain
const KIND_ICON: Record<RcaNode['kind'], typeof Server> = {
  page: Globe, app_start: Smartphone, endpoint: Server, db: Database, cache: HardDrive, http: Cloud, queue: Inbox, external: Cloud, code: Code2,
};
const KIND_LABEL: Record<RcaNode['kind'], string> = {
  page: 'Page', app_start: 'App start', endpoint: 'Endpoint', db: 'Database', cache: 'Cache', http: 'HTTP', queue: 'Queue', external: 'External', code: 'Own code',
};

export function RcaChain({ tree, result }: { tree: RcaNode; result: RcaResult }) {
  const nav = useNavigate();
  const fmt = fmtFor(result.unit);
  const steps = [{ node: tree, explains: 1, alternatives: [] as { node: RcaNode; explains: number }[] }, ...result.path];
  return (
    <div className="chain">
      {steps.map((s, i) => {
        const last = i === steps.length - 1;
        const Icon = tree.kind === 'page' && i === 0 && tree.service && services.find((x) => x.id === tree.service)?.type === 'mobile' ? Smartphone : KIND_ICON[s.node.kind];
        const x = s.node.baseline > 0 ? s.node.current / s.node.baseline : 0;
        return (
          <div key={s.node.id} className={`chain__step${last && i > 0 ? ' chain__step--cause' : ''}`}>
            <div className="chain__rail">
              <span className="chain__dot"><Icon size={14} aria-hidden /></span>
              {!last && <span className="chain__line" />}
            </div>
            <div className="chain__content">
              <div className="chain__label">
                <span className={s.node.kind === 'db' || s.node.kind === 'cache' || s.node.kind === 'queue' ? 'mono' : undefined}>{s.node.label}</span>
                {s.node.service && s.node.label !== s.node.service && !s.node.label.startsWith(s.node.service) && <span className="muted" style={{ fontWeight: 'normal' }}>{s.node.service}</span>}
                <Badge>{KIND_LABEL[s.node.kind]}</Badge>
                {last && i > 0 && <Badge tone="critical" icon={<AlertOctagon aria-hidden />}>Root cause</Badge>}
              </div>
              <div className="chain__meta">
                {s.node.kind === 'code'
                  ? <span>+{fmt(s.node.current)} {result.unit === 'ratio' ? 'failures not explained by any call' : 'not explained by any call'}</span>
                  : <span className="num">{fmt(s.node.baseline)} → <strong>{fmt(s.node.current)}</strong>{x >= 1.5 && <span className="muted"> ({x >= 10 ? Math.round(x) : x.toFixed(1)}×)</span>}</span>}
                {s.node.link && (
                  <a href={s.node.link} onClick={(e) => { e.preventDefault(); nav(s.node.link!); }} className="row" style={{ gap: 2 }}>
                    Open <ArrowRight size={12} />
                  </a>
                )}
              </div>
              {i > 0 && (
                <div className="chain__share">
                  <span className="chain__share-bar"><span style={{ width: `${Math.round(s.explains * 100)}%` }} /></span>
                  explains {Math.round(s.explains * 100)}% of the change above
                  {s.alternatives.length > 0 && <span>· also: {s.alternatives.map((a) => `${a.node.label} ${Math.round(a.explains * 100)}%`).join(', ')}</span>}
                </div>
              )}
              {last && i > 0 && s.node.evidence && <EvidenceList items={s.node.evidence} />}
            </div>
          </div>
        );
      })}
    </div>
  );
}

const EVIDENCE_ICON: Record<RcaEvidence['kind'], typeof Server> = {
  deploy: GitCommit, migration: Layers, query: Database, resource: Zap, log: ScrollText, external: Cloud,
};

export function EvidenceList({ items }: { items: RcaEvidence[] }) {
  const nav = useNavigate();
  return (
    <div className="chain__evidence">
      {items.map((e) => {
        const Icon = EVIDENCE_ICON[e.kind] ?? Box;
        return (
          <div key={e.title} className="evidence">
            <Icon size={14} className="muted" style={{ marginTop: 3, flexShrink: 0 }} aria-hidden />
            <div className="evidence__main">
              <span className={`evidence__title${e.kind === 'query' || e.kind === 'log' ? ' mono' : ''}`}>{e.title}</span>
              {(e.detail || e.at) && <span className="muted">{e.at ? `${ago(e.at)} · ` : ''}{e.detail}</span>}
            </div>
            {e.link && (
              <a href={e.link} onClick={(ev) => { ev.preventDefault(); nav(e.link!); }} style={{ marginLeft: 'auto', flexShrink: 0 }}>View</a>
            )}
          </div>
        );
      })}
    </div>
  );
}

export function RcaDrawer({ title, tree, result, onClose, actions }: { title: ReactNode; tree: RcaNode; result: RcaResult; onClose: () => void; actions?: ReactNode }) {
  const impacted = impactedKpis(result.root.service ?? '');
  const nav = useNavigate();
  return (
    <Drawer open onClose={onClose} width={720} title={title} subtitle={<span>{result.summary}</span>} actions={actions}>
      <DrawerSection title="How we got there" aside={`confidence ${Math.round(result.confidence * 100)}%`}>
        <RcaChain tree={tree} result={result} />
      </DrawerSection>
      {impacted.length > 0 && (
        <DrawerSection title="KPIs hit by the same cause">
          <div className="stack" style={{ gap: 6 }}>
            {impacted.map((k) => (
              <a key={k.id} href={`/kpis/${k.id}`} onClick={(e) => { e.preventDefault(); nav(`/kpis/${k.id}`); }} className="row">
                <Monitor size={14} aria-hidden />{k.name}<span className="muted">· {(k.current * 100).toFixed(1)}% good, objective {(k.objective * 100).toFixed(1)}%</span>
              </a>
            ))}
          </div>
        </DrawerSection>
      )}
      <DrawerSection title="How this is computed">
        <div className="muted" style={{ fontSize: 'var(--text-sm)' }}>
          <FileCode2 size={12} aria-hidden /> From traces we know which calls each request makes. At every level we compare each call's
          contribution now vs. before the change and follow the one that explains most of it, until we reach a database, cache,
          external API or the service's own code.
        </div>
      </DrawerSection>
    </Drawer>
  );
}

/** Root cause behind an incident: the KPI's tree, or the service's errors/latency tree. */
export function incidentCause(i: { kpiId?: string; monitorId: string; service: string }): RcaResult | undefined {
  if (i.kpiId) {
    const k = kpis.find((x) => x.id === i.kpiId);
    const t = k && kpiTree(k);
    return k && t ? findRootCause(t, kpiUnit(k)) : undefined;
  }
  const svc = services.find((s) => s.name === i.service || s.id === i.service);
  if (!svc) return undefined;
  const metric: RcaMetric = monitors.find((m) => m.id === i.monitorId)?.kind === 'latency' ? 'latency' : 'errors';
  const t = serviceNode(svc.id, metric);
  if (!t || t.current - t.baseline <= 0) return undefined;
  const r = findRootCause(t, metric === 'errors' ? 'ratio' : 'ms');
  return r.path.length ? r : undefined;
}
