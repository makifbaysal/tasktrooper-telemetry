import { useNavigate } from 'react-router';
import { ArrowRight, Sparkles } from 'lucide-react';
import { Badge, Button, Card, DrawerSection } from '@/components/ui';
import { databases, dbQueries, type Dependency, type Endpoint, type RumPage, type TimeBreakdown } from '@/data';
import { ms } from '@/lib/format';

const assistantUrl = (question: string, ref?: string) => `/assistant?${new URLSearchParams({ q: question, ...(ref ? { ref } : {}) })}`;

/** Evidence-first diagnosis. The Assistant can investigate further; local clues never claim an unverified cause. */
export function EndpointPerformanceDiagnosis({ service, endpoint, breakdown, calls, codeFile }: {
  service: string; endpoint: Endpoint; breakdown: TimeBreakdown;
  calls: { dep: Dependency; callsPerReq: number; msPerReq: number }[]; codeFile?: string;
}) {
  const nav = useNavigate();
  const label = endpoint.sub?.process ?? `${endpoint.method} ${endpoint.route}`;
  const top = [...calls].sort((a, b) => b.msPerReq - a.msPerReq)[0];
  const query = databases.flatMap(dbQueries).filter((q) => q.serviceId === endpoint.serviceId &&
    (q.endpoint === label || q.endpoint === endpoint.id)).sort((a, b) => b.avgMs - a.avgMs)[0];
  const dbDominates = breakdown.parts.db > 0 && breakdown.parts.db >= breakdown.avgMs * 0.4;
  const question = `Investigate why ${service} ${label} is slow (p95 ${ms(endpoint.p95)}). ` +
    `Check endpoint RCA, traces, database queries and available code inventory. ` +
    `${top ? `Largest observed outgoing call: ${top.dep.name}, ${ms(top.msPerReq)} per request. ` : ''}` +
    `${query ? `Database query: ${query.text}; p95 ${ms(query.p95)}${query.plan ? `; plan note: ${query.plan}` : ''}. ` : ''}` +
    `${codeFile ? `Code inventory points to ${codeFile}. ` : ''}` +
    `Identify evidence, distinguish hypotheses from confirmed causes, and suggest specific checks and fixes. ` +
    `Do not claim a missing index without a query plan or comparable evidence.`;
  return <Card title="Performance investigation" subtitle="Telemetry clues to check before changing code"
    actions={<Button size="sm" icon={<Sparkles size={14} />} onClick={() => nav(assistantUrl(question, `endpoint:${label}`))}>Investigate with Assistant</Button>}>
    <div className="list">
      <div className="list__row"><div className="list__main"><strong>Latency</strong><span className="list__meta">p95 {ms(endpoint.p95)} · average {ms(breakdown.avgMs)}</span></div></div>
      {top && <div className="list__row"><div className="list__main"><strong>{top.dep.name}</strong><span className="list__meta">Largest observed outgoing call · {ms(top.msPerReq)} per request</span></div><Badge>{top.dep.kind}</Badge></div>}
      {dbDominates && <div className="list__row"><div className="list__main"><strong>Database time is prominent</strong><span className="list__meta">{ms(breakdown.parts.db)} of {ms(breakdown.avgMs)} average request time. Inspect query plans and indexes.</span></div><Badge tone="warning">Clue</Badge></div>}
      {query && <div className="list__row"><div className="list__main"><strong>Slow database query</strong><span className="list__meta">{query.text} · p95 {ms(query.p95)}{query.plan ? ` · ${query.plan}` : ''}</span></div><Badge tone={query.plan ? 'warning' : undefined}>{query.plan ? 'Plan note' : 'Check plan'}</Badge></div>}
      {codeFile && <div className="list__row"><div className="list__main"><strong>Code location</strong><span className="list__meta">{codeFile}</span></div></div>}
      {!top && !dbDominates && !query && <div className="list__row"><span className="muted">No specific bottleneck is confirmed by the available breakdown. Inspect a slow trace and compare it with a healthy request.</span></div>}
    </div>
  </Card>;
}

export function PagePerformanceDiagnosis({ app, page }: { app: string; page: RumPage }) {
  const nav = useNavigate();
  const slowLcp = page.lcp >= 2500;
  const slowInp = page.inp >= 200;
  const question = `Investigate slow loading for ${app} page ${page.path}. ` +
    `Observed p75 LCP ${ms(page.lcp)}, INP ${ms(page.inp)}, CLS ${page.cls}. ` +
    `Check RUM vitals, relevant API calls, traces and available code. ` +
    `Consider image size/format/lazy loading only as a hypothesis until resource timing or page assets confirm it. ` +
    `Separate confirmed evidence from suggested checks and fixes.`;
  return <DrawerSection title="Performance investigation" aside="evidence and next checks">
    <div className="list">
      {slowLcp && <div className="list__row"><div className="list__main"><strong>Largest Contentful Paint is slow</strong><span className="list__meta">p75 {ms(page.lcp)}. Check the LCP element, image transfer size, format, and loading priority.</span></div><Badge tone="warning">Clue</Badge></div>}
      {slowInp && <div className="list__row"><div className="list__main"><strong>Interaction is slow</strong><span className="list__meta">p75 INP {ms(page.inp)}. Inspect long tasks and JavaScript execution.</span></div><Badge tone="warning">Clue</Badge></div>}
      {!slowLcp && !slowInp && <div className="list__row"><span className="muted">No slow LCP or INP is visible for this page in the current range.</span></div>}
    </div>
    <p className="muted">Image optimisation is a check, not a confirmed cause: resource timing and asset size are not available in this view.</p>
    <Button size="sm" icon={<Sparkles size={14} />} onClick={() => nav(assistantUrl(question, `service:${app}`))}>Investigate with Assistant <ArrowRight size={14} /></Button>
  </DrawerSection>;
}
