import { useEffect, useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import {
  AlertTriangle, Bell, Check, ChevronRight, CircleDashed, Code2, ExternalLink, Eye, EyeOff, FileCode2, FolderGit2, GitBranch, Gauge, Plus,
  RefreshCw, ShieldCheck, Sparkles, Trash2, Unplug, Webhook, X,
} from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CodeBlock, Drawer, DrawerSection, EmptyState, KeyValues, Meter, MetricCard, PageHeader, Section, Segmented, Select,
  StatusBadge, Table, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { isApi, monitors, series, services, timestamps, type InventoryItem, type TimeRange } from '@/data';
import {
  acceptSuggestion, acceptedSuggestions, analyzeRepo, codeIntelAvailable, codeLink, codeRefLink, deleteRepo, inventoryRows, refreshCodeIntel, rejectSuggestion, repoList,
  rotateWebhookSecret, suggestionRows, type InventoryRow, type RepoInfo, type RepoWebhook, type SuggestionRow,
} from '@/data/codeintel';
import { ago, compact } from '@/lib/format';
import { CriticalityBadge } from './shared';
import { ConnectRepoDrawer, WebhookSetup } from './ConnectRepo';

type Filter = 'all' | 'monitored' | 'unmonitored' | 'dead' | 'shadow';

const KIND_LABEL: Record<InventoryItem['kind'], string> = {
  endpoint: 'Endpoint', consumer: 'Consumer', cron: 'Scheduled job', view: 'View', dependency: 'Dependency',
};

/** The AI quota (ADR-018) stopped the LLM step of the last analysis: the rest kept static criticality. */
type LlmInfo = { status?: string; reason?: string; skipped?: number };
const REASON_TEXT: Record<string, string> = {
  user_limit: 'your personal monthly AI limit was reached', org_limit: 'your team’s monthly AI credits are used up', check_failed: 'the AI quota check was unavailable',
};
function AiQuotaNotice({ repos, inventory }: { repos: RepoInfo[]; inventory: InventoryRow[] }) {
  const nav = useNavigate();
  // repo summary (repositories.analysis.llm) and/or per item (inventory.analysis.llm)
  const fromRepos = repos.filter((r) => r.llm?.status === 'quota_exceeded').map((r) => ({ name: r.name, llm: r.llm as LlmInfo }));
  const itemLlm = inventory.map((i) => (i as InventoryRow & { analysis?: { llm?: LlmInfo | null } | null }).analysis?.llm).filter((l): l is LlmInfo => l?.status === 'quota_exceeded');
  if (fromRepos.length === 0 && itemLlm.length === 0) return null;
  const all = [...fromRepos.map((h) => h.llm), ...itemLlm];
  const skipped = fromRepos.length ? fromRepos.reduce((a, h) => a + (h.llm.skipped ?? 0), 0) : itemLlm.length;
  const reason = (['user_limit', 'org_limit', 'check_failed'] as const).find((r) => all.some((l) => l.reason === r)) ?? 'org_limit';
  const hit = fromRepos;
  return (
    <Callout icon={<AlertTriangle size={16} color="var(--status-warning)" />}>
      <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        <Badge tone="warning" icon={<AlertTriangle />}>{reason === 'check_failed' ? 'AI quota check unavailable' : 'AI quota reached'}</Badge>
        <span>
          {hit.length ? <>The last analysis of {hit.map((r) => r.name).join(', ')}</> : 'The last analysis'} ran without AI because {REASON_TEXT[reason]}
          {skipped > 0 && <> — {skipped} code path{skipped === 1 ? '' : 's'} kept rule-based criticality and no failure modes</>}.
        </span>
        <Button size="sm" variant="ghost" onClick={() => nav('/settings/usage')}>View AI usage</Button>
      </div>
    </Callout>
  );
}

const monitorFor = (i: InventoryItem) => monitors.find((m) => m.target.includes(i.key));
const suggestionFor = (i: InventoryItem) => suggestionRows().find((s) => s.target.endsWith(i.key));
const serviceId = (name: string) => services.find((s) => s.name === name || s.id === name)?.id;

function whyCritical(i: InventoryRow): string {
  if (i.summary) return i.summary;
  const s = suggestionFor(i);
  if (s) return s.rationale;
  if (i.criticality === 'critical') return 'Writes money or order state; a failure here is customer-visible and not retried automatically.';
  if (i.criticality === 'important') return 'Part of a user-facing flow, but failures are retried or degrade gracefully.';
  return 'Read-only or internal path; failures have no lasting side effects.';
}

/** Mock mode only: an illustrative handler excerpt (the api does not return source). */
function handlerSnippet(i: InventoryItem): string {
  if (i.kind === 'consumer') {
    return `// ${i.file}:${i.line}
consumer.wrap('${i.source}', { process: '${i.key}' }, async (msg) => {
  const order = await orders.get(msg.orderId);
  await shipments.create(order);         // side effect
}, { retries: 3, dlq: '${(i.source ?? '').replace('.dlq', '')}.dlq' });`;
  }
  if (i.kind === 'cron') return `# ${i.file}:${i.line}\n@schedule("${i.source}")\ndef send_invoices():\n    for account in billing.due_today():\n        mailer.send_invoice(account)      # external call`;
  if (i.kind === 'dependency') return `// ${i.file}:${i.line}\nclient := iyzico.New(cfg.APIKey, iyzico.WithTimeout(5*time.Second))\nres, err := client.CreatePayment(ctx, req)  // synchronous, on the charge path`;
  const [method, route] = i.key.split(' ');
  const m = (method ?? 'GET')[0] + (method ?? 'GET').slice(1).toLowerCase();
  if (i.file.endsWith('.go')) return `// ${i.file}:${i.line}\nr.${m}("${route}", func(w http.ResponseWriter, r *http.Request) {\n    tx, _ := db.BeginTx(r.Context(), nil)\n    defer tx.Rollback()\n    // …\n})`;
  return `// ${i.file}:${i.line}\n@${m}('${route}')\nasync handle(@Body() dto: Dto) {\n  return this.db.transaction(async (tx) => {\n    // …\n  });\n}`;
}

function trafficFor(i: InventoryItem, range: TimeRange) {
  const base = i.kind === 'cron' ? 0.4 : i.criticality === 'critical' ? 220 : i.criticality === 'important' ? 90 : 25;
  return series(`inv-${i.id}`, range, { base, noise: 0.18, daily: 0.35, min: 0 });
}

export function CodeInsights() {
  const nav = useNavigate();
  const { range } = useApp();
  const drawer = useDrawer<InventoryRow>();
  const [params, setParams] = useSearchParams();
  // back from the GitHub App installation: ?github=<integrationId> opens the repo picker
  const [githubId] = useState(() => params.get('github') ?? undefined);
  const [connecting, setConnecting] = useState(!!githubId);
  useEffect(() => {
    if (params.has('github')) setParams((p) => { const n = new URLSearchParams(p); n.delete('github'); return n; }, { replace: true });
  }, []); // eslint-disable-line react-hooks/exhaustive-deps
  const [filter, setFilter] = useState<Filter>('all');
  const [kind, setKind] = useState('');
  const [svc, setSvc] = useState('');
  const [enrichment, setEnrichment] = useState<{ summary: string; failureModes: string[]; provider: string } | null>(null);
  const [monitorDraft, setMonitorDraft] = useState<{ suggestions: { name: string; kind: string; condition: string; rationale: string }[] } | null>(null);
  const [loadTestDraft, setLoadTestDraft] = useState<{ yaml: string; note: string } | null>(null);
  const [aiError, setAiError] = useState('');
  const [aiBusy, setAiBusy] = useState(false);
  const [aiItemId, setAiItemId] = useState('');
  const runLocalAI = async (action: 'inventory-enrichment' | 'monitor-suggestions' | 'loadtest-draft') => {
    const selected = drawer.item;
    if (!selected) return;
    if (aiItemId !== selected.id) { setEnrichment(null); setMonitorDraft(null); setLoadTestDraft(null); setAiItemId(selected.id); }
    setAiBusy(true); setAiError('');
    try {
      const response = await fetch(`/local-ai/${action}`, {
        method: 'POST', headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ key: selected.key, kind: selected.kind, service: selected.service, file: selected.file, criticality: selected.criticality }),
      });
      if (!response.ok) throw new Error(`Local mock server returned HTTP ${response.status}`);
      const result = await response.json();
      if (action === 'inventory-enrichment') setEnrichment(result);
      else if (action === 'monitor-suggestions') setMonitorDraft(result);
      else setLoadTestDraft(result);
    } catch (cause) { setAiError(cause instanceof Error ? cause.message : 'Could not reach the local mock server.'); }
    finally { setAiBusy(false); }
  };

  const repos = repoList();
  const inventory = inventoryRows();
  const analyzing = repos.some((r) => r.status === 'analyzing');

  // poll while an analysis runs
  useEffect(() => {
    if (!analyzing || !isApi) return;
    const t = setInterval(() => { void refreshCodeIntel(); }, 5000);
    return () => clearInterval(t);
  }, [analyzing]);

  const inCode = inventory.filter((i) => !i.shadow);
  const scoped = inCode.filter((i) => i.criticality !== 'low');
  const covered = scoped.filter((i) => i.monitored).length;
  const coverage = scoped.length ? covered / scoped.length : 0;
  const gaps = scoped.filter((i) => !i.monitored)
    .sort((a, b) => (a.criticality === 'critical' ? 0 : 1) - (b.criticality === 'critical' ? 0 : 1));
  const criticalGaps = gaps.filter((i) => i.criticality === 'critical').length;
  const dead = inCode.filter((i) => i.dead ?? !i.observed);
  const shadow = inventory.filter((i) => i.shadow);
  const pending = suggestionRows();
  const justAccepted = [...acceptedSuggestions.values()].map((a) => a.suggestion).filter((s) => !pending.includes(s));

  const kinds = [...new Set(inventory.map((i) => i.kind))];
  const svcs = [...new Set(inventory.map((i) => i.service))].sort();
  const rows = inventory
    .filter((i) => (!kind || i.kind === kind) && (!svc || i.service === svc))
    .filter((i) => {
      switch (filter) {
        case 'monitored': return !i.shadow && i.monitored;
        case 'unmonitored': return !i.shadow && !i.monitored;
        case 'dead': return !i.shadow && (i.dead ?? !i.observed);
        case 'shadow': return !!i.shadow;
        default: return true;
      }
    });
  const showFilter = (f: Filter) => { setFilter(f); document.getElementById('inventory')?.scrollIntoView({ behavior: 'smooth' }); };

  const connectBtn = <Button variant="primary" icon={<Plus size={14} />} onClick={() => setConnecting(true)}>Connect repository</Button>;
  const header = <PageHeader title="Code insights" subtitle="What your code exposes, what runs in production, and what a monitor protects" actions={connectBtn} />;
  const connectDrawer = <ConnectRepoDrawer open={connecting} integrationId={githubId} onClose={() => setConnecting(false)} />;

  if (repos.length === 0) {
    return (
      <>
        {header}
        <Card>
          <EmptyState icon={<FolderGit2 size={20} />} title="No repository connected" action={connectBtn}>
            {isApi && !codeIntelAvailable
              ? 'The code-intelligence api is not available on this server yet. Once it is, connect a repository to see its endpoints, consumers and jobs here.'
              : 'Install the TaskTrooper GitHub App and pick repositories (or use a token / local checkout under Advanced). TaskTrooper reads the routes, consumers and jobs, matches them with production traffic and suggests monitors for the critical paths.'}
          </EmptyState>
        </Card>
        {connectDrawer}
      </>
    );
  }

  const item = drawer.item;
  const sug = item && suggestionFor(item);
  const mon = item && monitorFor(item);
  const link = item && codeLink(item);

  return (
    <>
      {header}
      <AiQuotaNotice repos={repos} inventory={inventory} />

      {/* 1 — the headline: how much of what matters is protected */}
      <div className="grid grid-3">
        <MetricCard className="metric--hero span-2" label="Alert coverage"
          value={scoped.length ? `${Math.round(coverage * 100)}%` : '—'}
          hint={scoped.length
            ? <>{covered} of {scoped.length} critical &amp; important code paths have a monitor · <strong>{criticalGaps} critical path{criticalGaps === 1 ? '' : 's'} without one</strong></>
            : analyzing ? 'Analysis in progress — coverage appears when the inventory is ready' : 'No critical or important code paths found yet'}
          actions={pending.length > 0 && <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>{pending.length} suggested monitor{pending.length === 1 ? '' : 's'} below</span>}>
          <div style={{ paddingTop: 'var(--space-2)' }}><Meter value={coverage} tone={scoped.length && coverage < 0.8 ? 'warning' : undefined} /></div>
        </MetricCard>

        <Card title="Drift" subtitle="Code vs. production traffic, last 30 days">
          <div className="list" style={{ margin: 'calc(-1 * var(--space-2)) 0' }}>
            <DriftRow icon={<EyeOff size={16} className="muted" aria-hidden />} title="No traffic in 30 days" meta="Dead code, a flag that is off, or a missing SDK" count={dead.length} onClick={() => showFilter('dead')} />
            <DriftRow icon={<Eye size={16} className="muted" aria-hidden />} title="Traffic but not in code" meta={shadow[0] ? `${shadow[0].key}${shadow[0].rpm ? ` · ${shadow[0].rpm} rpm` : ''}` : 'Every observed route has a handler'} count={shadow.length} onClick={() => showFilter('shadow')} />
          </div>
        </Card>
      </div>

      {/* 2 — what to do: suggested monitors */}
      <Section title="Suggested monitors" subtitle="Generated from the code and calibrated on production data where traffic exists">
        {pending.length + justAccepted.length === 0
          ? <Card><EmptyState icon={<Sparkles size={20} />} title={analyzing ? 'Analysis in progress' : 'No open suggestions'}>{analyzing ? 'Suggestions appear when the analysis finishes.' : 'Accepted suggestions became monitors; rejected ones stay hidden.'}</EmptyState></Card>
          : <div className="stack" style={{ gap: 'var(--space-3)' }}>{[...pending, ...justAccepted].map((s) => <SuggestionCard key={s.id} s={s} />)}</div>}
      </Section>

      {/* 3 — the unprotected paths */}
      <Section title="Coverage gaps" subtitle="Critical and important code paths with no monitor — click for code, traffic and a suggested alert">
        {gaps.length === 0 ? (
          <Card><EmptyState icon={<ShieldCheck size={20} />} title={scoped.length ? 'Every important path is monitored' : 'Nothing to cover yet'} /></Card>
        ) : (
          <Card flush>
            <div className="list">
              {gaps.map((i) => {
                const s = suggestionFor(i);
                return (
                  <div key={i.id} className="list__row list__row--clickable" onClick={() => drawer.open(i)}>
                    <CriticalityBadge c={i.criticality} />
                    <div className="list__main">
                      <span className="list__title mono">{i.key}{i.source && <span className="muted"> · {i.source}</span>}</span>
                      <span className="list__meta">{KIND_LABEL[i.kind]} · {i.service} · {i.observed ? 'receiving traffic' : 'no traffic in 30d'}</span>
                    </div>
                    {s ? <Badge tone="accent" icon={<Bell />}>Suggestion ready</Badge> : <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>No suggestion yet</span>}
                    <ChevronRight size={14} className="muted" aria-hidden />
                  </div>
                );
              })}
            </div>
          </Card>
        )}
      </Section>

      {/* 4 — reference: full inventory */}
      <div id="inventory">
        <Section title="Inventory" subtitle={`${inCode.length} code paths found in ${repos.length} repositor${repos.length === 1 ? 'y' : 'ies'}`}
          actions={<div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
            <Select aria-label="Kind" value={kind} onChange={(e) => setKind(e.target.value)}
              options={[{ value: '', label: 'All kinds' }, ...kinds.map((k) => ({ value: k, label: KIND_LABEL[k] }))]} />
            <Select aria-label="Service" value={svc} onChange={(e) => setSvc(e.target.value)}
              options={[{ value: '', label: 'All services' }, ...svcs.map((s) => ({ value: s, label: s }))]} />
          </div>}>
          <div style={{ marginBottom: 'var(--space-3)' }}>
            <Segmented label="Filter" value={filter} onChange={setFilter} options={[
              { id: 'all', label: 'All' },
              { id: 'monitored', label: 'Monitored' },
              { id: 'unmonitored', label: 'Not monitored' },
              { id: 'dead', label: `No traffic in 30 days · ${dead.length}` },
              { id: 'shadow', label: `Traffic but not in code · ${shadow.length}` },
            ]} />
          </div>
          <Card flush>
            <Table<InventoryRow> rows={rows} rowKey={(i) => i.id} onRowClick={drawer.open}
              empty={filter === 'shadow' ? 'Every route seen in production has a handler in a connected repository.' : 'No code paths match these filters.'}
              columns={[
                { key: 'key', header: 'Code path', sortValue: (i) => i.key, render: (i) => <span className="mono">{i.key}{i.source && <span className="muted"> · {i.source}</span>}</span> },
                { key: 'kind', header: 'Kind', sortValue: (i) => i.kind, render: (i) => <span className="secondary">{KIND_LABEL[i.kind]}</span> },
                { key: 'svc', header: 'Service', sortValue: (i) => i.service, render: (i) => <span className="secondary">{i.service}</span> },
                { key: 'crit', header: 'Criticality', sortValue: (i) => ['critical', 'important', 'low'].indexOf(i.criticality), render: (i) => <CriticalityBadge c={i.criticality} /> },
                { key: 'obs', header: 'Traffic', render: (i) => (i.shadow
                  ? <Badge tone="warning" icon={<AlertTriangle />}>Not in code</Badge>
                  : i.observed ? <span className="secondary">Observed</span> : <span className="row muted" style={{ gap: 4 }}><EyeOff size={12} aria-hidden />None (30d)</span>) },
                { key: 'mon', header: 'Monitor', render: (i) => (i.monitored ? <Badge tone="good" icon={<Bell />}>Active</Badge> : <span className="muted">—</span>) },
                { key: 'go', header: '', render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
              ]} />
          </Card>
        </Section>
      </div>

      {/* 5 — secondary: where the inventory comes from */}
      <Section title="Repositories">
        <Card flush>
          <div className="list">{repos.map((r) => <RepoRow key={r.id} r={r} />)}</div>
        </Card>
      </Section>

      <Drawer open={!!item} onClose={drawer.close}
        title={item ? <span className="mono">{item.key}</span> : ''}
        subtitle={item && <>{KIND_LABEL[item.kind]} · {item.service}</>}
        actions={item && <>
          <Button size="sm" icon={<Sparkles size={14} />} disabled={aiBusy} onClick={() => void runLocalAI('inventory-enrichment')}>Enrich inventory</Button>
          {item.kind === 'endpoint' && !item.shadow && <Button size="sm" icon={<Gauge size={14} />} onClick={() => nav('/load-tests')}>Load test this</Button>}
        </>}>
        {item && (
          <>
            <DrawerSection title="Summary" aside={<CriticalityBadge c={item.criticality} />}>
              <p className="secondary" style={{ margin: 0 }}>{whyCritical(item)}</p>
            </DrawerSection>

            <DrawerSection title="Local AI drafts" aside="Offline mock · 0 tokens">
              <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
                <Button size="sm" icon={<Bell size={12} />} disabled={aiBusy} onClick={() => void runLocalAI('monitor-suggestions')}>Suggest monitors</Button>
                {item.kind === 'endpoint' && <Button size="sm" icon={<Gauge size={12} />} disabled={aiBusy} onClick={() => void runLocalAI('loadtest-draft')}>Draft load test</Button>}
              </div>
              {aiItemId === item.id && enrichment && <div className="stack" style={{ marginTop: 'var(--space-3)' }}><p>{enrichment.summary}</p><ul>{enrichment.failureModes.map((mode) => <li key={mode}>{mode}</li>)}</ul></div>}
              {aiItemId === item.id && monitorDraft && <div className="stack" style={{ marginTop: 'var(--space-3)' }}>{monitorDraft.suggestions.map((suggestion) => <div key={suggestion.name}><strong>{suggestion.name}</strong><div className="mono secondary">{suggestion.kind} · {suggestion.condition}</div><div className="muted">{suggestion.rationale}</div></div>)}<span className="muted">Drafts only; no monitor was created.</span></div>}
              {aiItemId === item.id && loadTestDraft && <div className="stack" style={{ marginTop: 'var(--space-3)' }}><CodeBlock>{loadTestDraft.yaml}</CodeBlock><span className="muted">{loadTestDraft.note}</span></div>}
              {aiItemId === item.id && aiError && <p role="alert" className="secondary">{aiError}</p>}
            </DrawerSection>

            {item.shadow ? (
              <DrawerSection title="Not in code">
                <p className="secondary" style={{ margin: 0 }}>
                  Production traffic{item.rpm ? ` (${item.rpm} requests / min)` : ''} hits this route, but no connected repository declares it. Connect the repository that serves it, or retire the clients still calling it.
                </p>
              </DrawerSection>
            ) : (
              <DrawerSection title="Protection">
                {mon ? (
                  <div className="row"><StatusBadge status={mon.state} /><span className="list__title">{mon.name}</span><span className="muted">{mon.condition}</span>
                    <span className="spacer" /><Button size="sm" variant="ghost" onClick={() => nav(`/services/${serviceId(item.service) ?? ''}?tab=alerts`)}>Open</Button></div>
                ) : item.monitored ? (
                  <Badge tone="good" icon={<Bell />}>Monitored</Badge>
                ) : sug ? (
                  <SuggestionCard s={sug} compact />
                ) : (
                  <div className="row"><span className="muted">No monitor and no suggestion yet.</span><span className="spacer" /><Button size="sm" icon={<Plus size={14} />} onClick={() => nav('/alerts/new')}>Create monitor</Button></div>
                )}
              </DrawerSection>
            )}

            {(item.failureModes?.length ?? 0) > 0 && (
              <DrawerSection title="Failure modes" aside={<Badge tone="accent" icon={<Sparkles />}>From code analysis</Badge>}>
                <div className="stack" style={{ gap: 'var(--space-3)' }}>
                  {item.failureModes!.map((f, k) => (
                    <div key={k}>
                      <div className="row" style={{ gap: 'var(--space-2)' }}><AlertTriangle size={14} className="muted" aria-hidden /><span className="list__title">{f.title}</span></div>
                      {(f.impact || f.mitigation) && (
                        <div className="secondary" style={{ fontSize: 'var(--text-sm)', paddingLeft: 'var(--space-5)' }}>
                          {f.impact && <>Impact: {f.impact}</>}{f.impact && f.mitigation && ' · '}{f.mitigation && <>Mitigation: {f.mitigation}</>}
                        </div>
                      )}
                    </div>
                  ))}
                </div>
              </DrawerSection>
            )}

            {!isApi && (
              <DrawerSection title="Production traffic" aside={item.observed ? `${compact(trafficFor(item, range).at(-1) ?? 0)} / min now` : undefined}>
                {item.observed
                  ? <TimeSeries timestamps={timestamps(range)} height={140} area format={(v) => compact(v)}
                      series={[{ id: 't', label: 'Requests / min', slot: 1, data: trafficFor(item, range) }]} />
                  : <EmptyState icon={<EyeOff size={20} />} title="No traffic in 30 days">Dead code, a feature flag that is off, or the SDK is not installed on this service.</EmptyState>}
              </DrawerSection>
            )}

            {item.file && (
              <DrawerSection title="Source" aside={link
                ? <a className="row mono" style={{ gap: 4 }} href={link} target="_blank" rel="noreferrer"><FileCode2 size={12} aria-hidden />{item.file}:{item.line}<ExternalLink size={12} aria-hidden /></a>
                : <span className="row mono" style={{ gap: 4 }}><FileCode2 size={12} aria-hidden />{item.file}:{item.line}</span>}>
                {!isApi ? <CodeBlock>{handlerSnippet(item)}</CodeBlock> : <span className="muted">{link ? 'Open the handler on GitHub with the link above.' : 'Local checkout — open the file in your editor.'}</span>}
              </DrawerSection>
            )}

            <DrawerSection title="Details">
              <KeyValues items={[
                ['Service', item.service],
                ['Repository', repos.find((r) => r.id === item.repoId || r.services.includes(item.service))?.name ?? '—'],
                ['Kind', KIND_LABEL[item.kind]],
                ['Traffic', item.shadow ? 'Seen in production, not in code' : (item.dead ?? !item.observed) ? 'No traffic in 30 days' : 'Observed'],
              ]} />
            </DrawerSection>
          </>
        )}
      </Drawer>

      {connectDrawer}
    </>
  );
}

function DriftRow({ icon, title, meta, count, onClick }: { icon: ReactNode; title: string; meta: string; count: number; onClick: () => void }) {
  return (
    <button className="list__row list__row--clickable" onClick={onClick}
      style={{ all: 'unset', display: 'flex', alignItems: 'center', gap: 'var(--space-3)', padding: 'var(--space-2) 0', cursor: 'pointer', width: '100%' }}>
      {icon}
      <span className="list__main"><span className="list__title">{title}</span><span className="list__meta mono">{meta}</span></span>
      <span className="num" style={{ fontSize: 'var(--text-lg)', fontWeight: 'var(--weight-semibold)' }}>{count}</span>
      <ChevronRight size={14} className="muted" aria-hidden />
    </button>
  );
}


/** Code-sourced monitor suggestion (design-system § Öneri kartı): accept creates the monitor, reject hides it. */
export function SuggestionCard({ s, compact: small }: { s: SuggestionRow; compact?: boolean }) {
  const nav = useNavigate();
  const [busy, setBusy] = useState<'accept' | 'reject' | null>(null);
  const [error, setError] = useState('');
  const [done, setDone] = useState(acceptedSuggestions.get(s.id)?.monitor);
  const link = codeRefLink(s);
  const svcId = serviceId(s.target.split(' · ')[0].replace(/ logs$/, ''));
  const act = async (kind: 'accept' | 'reject') => {
    setBusy(kind); setError('');
    try {
      if (kind === 'accept') setDone(await acceptSuggestion(s.id));
      else await rejectSuggestion(s.id);
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(null);
  };
  const body = (
    <div className="stack" style={{ gap: 'var(--space-2)' }}>
      <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        {!small && <span className="list__title">{s.title}</span>}
        <CriticalityBadge c={s.criticality} />
        <Badge tone="accent" icon={<Code2 />}>From code</Badge>
        {s.calibrated
          ? <Badge tone="good" icon={<Check />}>Calibrated on traffic</Badge>
          : <Badge icon={<CircleDashed />}>Not calibrated</Badge>}
        <span className="spacer" />
        <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>{Math.round(s.confidence * 100)}% confidence</span>
      </div>
      {small && <span className="list__title">{s.title}</span>}
      <div className="secondary mono" style={{ fontSize: 'var(--text-sm)' }}>{s.target}</div>
      <div className="mono">{s.condition}</div>
      <p className="secondary" style={{ margin: 0 }}>{s.rationale}</p>
      <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        {link
          ? <a className="row mono" style={{ gap: 4, fontSize: 'var(--text-sm)' }} href={link} target="_blank" rel="noreferrer"><FileCode2 size={12} aria-hidden />{s.codeRef}<ExternalLink size={12} aria-hidden /></a>
          : <span className="row mono muted" style={{ gap: 4, fontSize: 'var(--text-sm)' }}><FileCode2 size={12} aria-hidden />{s.codeRef}</span>}
        <span className="spacer" />
        {done ? (
          <>
            <StatusBadge status="ok" label="Monitor created" />
            <Button size="sm" variant="ghost" icon={<ExternalLink size={12} />} onClick={() => nav(svcId ? `/services/${svcId}?tab=alerts` : '/')}>Open {done.name}</Button>
          </>
        ) : (
          <>
            <Button size="sm" variant="ghost" icon={<X size={14} />} disabled={!!busy} onClick={() => void act('reject')}>{busy === 'reject' ? 'Rejecting…' : 'Reject'}</Button>
            <Button size="sm" variant="ghost" onClick={() => nav('/alerts/new')}>Customize…</Button>
            <Button size="sm" variant="primary" icon={<Check size={14} />} disabled={!!busy} onClick={() => void act('accept')}>{busy === 'accept' ? 'Creating…' : 'Accept'}</Button>
          </>
        )}
      </div>
      {error && <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="critical" label="Failed" /><span className="secondary">{error}</span></div>}
    </div>
  );
  return small ? body : <Card>{body}</Card>;
}

function RepoRow({ r }: { r: RepoInfo }) {
  const [confirm, setConfirm] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const [hook, setHook] = useState<RepoWebhook | null>(null);
  const run = async (f: () => Promise<void>) => {
    setBusy(true); setError('');
    try { await f(); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  // GitLab, Bitbucket and App-less GitHub repos are notified by a per-repository webhook (contracts §18a)
  const repoHook = r.provider !== 'local' && !r.installationId && r.status !== 'disconnected';
  return (
    <div className="list__row" style={{ alignItems: 'flex-start' }}>
      <FolderGit2 size={16} className="muted" aria-hidden style={{ marginTop: 2 }} />
      <div className="list__main">
        <span className="list__title row" style={{ gap: 'var(--space-2)' }}>
          {r.url ? <a href={r.url} target="_blank" rel="noreferrer">{r.name}</a> : r.name}
          {r.provider === 'local' && <Badge>Local</Badge>}
          {r.provider === 'gitlab' && <Badge>{r.baseUrl ? 'GitLab self-managed' : 'GitLab'}</Badge>}
          {r.provider === 'bitbucket' && <Badge>Bitbucket</Badge>}
          {repoHook && !r.webhookActive && !hook && (
            <span className="row muted" style={{ gap: 4, fontSize: 'var(--text-xs)', fontWeight: 'var(--weight-regular)' }} title="No push has been received yet — set up the repository webhook">
              <Webhook size={12} aria-hidden />No webhook delivery yet
            </span>
          )}
          {r.webhookActive && r.status !== 'disconnected' && <span className="row muted" style={{ gap: 4, fontSize: 'var(--text-xs)', fontWeight: 'var(--weight-regular)' }} title="Pushes to the default branch trigger an incremental analysis"><Webhook size={12} aria-hidden />Webhook active</span>}
        </span>
        <span className="list__meta row" style={{ gap: 6, flexWrap: 'wrap' }}>
          <GitBranch size={12} aria-hidden />{r.branch}{r.sha && <> · last analyzed <span className="mono">{r.sha.slice(0, 7)}</span></>}
          {r.language && r.language !== '—' && <> · {r.language}</>}
          {r.services.length > 0 && <> · {r.services.join(', ')}</>}
          {r.localPath && <> · <span className="mono">{r.localPath}</span></>}
        </span>
        {r.status === 'analyzing' && (r.pendingPaths?.length ?? 0) > 0 && (
          <span className="secondary" style={{ fontSize: 'var(--text-sm)' }} title={r.pendingPaths!.join('\n')}>
            Analyzing changes from push · {r.pendingPaths!.length} file{r.pendingPaths!.length === 1 ? '' : 's'} (<span className="mono">{r.pendingPaths![0]}</span>{r.pendingPaths!.length > 1 ? ', …' : ''})
          </span>
        )}
        {(r.status === 'error' || r.status === 'disconnected') && r.error && <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{r.error}</span>}
        {error && <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{error}</span>}
        {hook && r.provider && (
          <div className="stack" style={{ gap: 'var(--space-2)', marginTop: 'var(--space-2)' }}>
            <WebhookSetup provider={r.provider} hook={hook} />
            <div><Button size="sm" variant="ghost" onClick={() => setHook(null)}>Hide</Button></div>
          </div>
        )}
      </div>
      {repoHook && !hook && (
        <Button size="sm" variant="ghost" icon={<Webhook size={14} />} disabled={busy}
          title="Issue a new webhook secret (the old one stops working)"
          onClick={() => void run(async () => setHook(await rotateWebhookSecret(r.id)))}>{r.webhookActive ? 'New secret' : 'Webhook setup'}</Button>
      )}
      {r.status === 'analyzing'
        ? <Badge tone="accent" icon={<CircleDashed />}>{r.pendingPaths?.length ? 'Analyzing push…' : 'Analyzing…'}</Badge>
        : r.status === 'disconnected'
          ? <Badge tone="warning" icon={<Unplug />}>Disconnected</Badge>
        : r.status === 'error'
          ? <StatusBadge status="critical" label="Analysis failed" />
          : <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{r.analyzedAt ? `Analyzed ${ago(r.analyzedAt)}` : 'Ready'}</span>}
      <Button size="sm" variant="ghost" icon={<RefreshCw size={14} />} disabled={busy || r.status === 'analyzing' || r.status === 'disconnected'} onClick={() => void run(() => analyzeRepo(r.id))}>Analyze now</Button>
      {confirm
        ? <>
            <Button size="sm" variant="danger" disabled={busy} onClick={() => void run(() => deleteRepo(r.id))}>Remove</Button>
            <Button size="sm" variant="ghost" onClick={() => setConfirm(false)}>Cancel</Button>
          </>
        : <Button size="sm" variant="ghost" iconOnly aria-label={`Disconnect ${r.name}`} icon={<Trash2 size={14} />} onClick={() => setConfirm(true)} />}
    </div>
  );
}
