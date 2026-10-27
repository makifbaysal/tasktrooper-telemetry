import { useData } from '@/data/remote';
import { useEffect, useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { AlertTriangle, Check, ChevronRight, Globe2, Pencil, Play, Plus, ShieldCheck, Sparkles, X } from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CodeBlock, Drawer, DrawerSection, KeyValues, Meter, PageHeader, Section, StatusBadge, Table, useDrawer,
  type Tone,
} from '@/components/ui';
import { NOW, runs, scenarios, series, type LoadTestRun, type LoadTestScenario } from '@/data';
import { ago, compact, duration, ms, pct } from '@/lib/format';
import { SPEC, gateVerdict, levelState, levelsOf, liveRun, runCurves, useLoadTests } from '@/lib/loadtest';
import { RunBlocked, approveScenario, runBlockers, scenarioState, startRunChecked } from '@/data/loadtest';
import { Link } from 'react-router';

export const RUN_STATE: Record<LoadTestRun['state'], { tone: Tone; label: string }> = {
  queued: { tone: 'neutral', label: 'Queued' },
  running: { tone: 'accent', label: 'Running' },
  passed: { tone: 'good', label: 'Passed' },
  failed: { tone: 'critical', label: 'Failed' },
  aborted: { tone: 'warning', label: 'Aborted' },
};

export function RunState({ state }: { state: LoadTestRun['state'] }) {
  if (state === 'passed') return <StatusBadge status="ok" label="Passed" />;
  if (state === 'failed') return <StatusBadge status="critical" label="Failed" />;
  if (state === 'aborted') return <StatusBadge status="warning" label="Aborted" />;
  return <Badge tone={RUN_STATE[state].tone}>{state === 'running' && <span className="dot" style={{ background: 'var(--accent)' }} />}{RUN_STATE[state].label}</Badge>;
}

const slug = (name: string) => name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');

function yamlFor(sc: Pick<LoadTestScenario, 'name' | 'target' | 'regions'>, spec: (typeof SPEC)[string]) {
  return [
    `name: ${sc.name || 'untitled'}`,
    `target: ${sc.target}`,
    `regions: [${sc.regions.join(', ')}]`,
    `executor: ${spec.mode === 'rps' ? 'ramping-arrival-rate' : 'ramping-vus'}`,
    'levels:',
    ...levelsOf(spec).map((l) => `  - { name: ${l.name}, ramp: ${duration(l.ramp)}, hold: ${duration(l.hold)}, target: ${l.target} }`),
    'steps:',
    ...(spec.steps.length ? spec.steps.map((x) => `  - request: ${x}${spec.weights?.[x] !== undefined ? `   # ${pct(spec.weights[x], 1)}` : ''}`) : ['  []']),
    'thresholds:',
    ...spec.thresholds.map((x) => `  - "${x}"`),
  ].join('\n');
}

/** Older runs per scenario, only for the history table/sparkline. Newly created scenarios have none. */
function pastRuns(sc: LoadTestScenario) {
  const real = runs.filter((r) => r.scenarioId === sc.id).map(liveRun).filter((r) => r.state !== 'running');
  if (!['lt1', 'lt2', 'lt3'].includes(sc.id)) return real.map((r) => ({ ...r, real: true }));
  const base = real[real.length - 1]?.p95 ?? 200;
  const extra = series(`lt-hist-${sc.id}`, '7d', { base, noise: 0.25 }).slice(-5).map((p95, k) => ({
    id: `${sc.id}-h${k}`, state: (p95 > base * 1.25 ? 'failed' : 'passed') as LoadTestRun['state'],
    startedAt: NOW - (14 + k * 3) * 86400_000, p95, errorRate: p95 > base * 1.25 ? 0.014 : 0.002, real: false,
  }));
  return [...real.map((r) => ({ ...r, real: true })), ...extra].sort((a, b) => b.startedAt - a.startedAt);
}

// ---------- starting runs ----------

/** Start a run with the pre-checks (draft, domain, plan limits); keeps the last refusal for <RunBlockedCallout>. */
export function useRunStarter() {
  const nav = useNavigate();
  const [blocked, setBlocked] = useState<{ scenarioId: string; err: RunBlocked } | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const run = async (scenarioId: string, afterSave = false) => {
    setBusy(scenarioId); setBlocked(null);
    try { nav(`/load-tests/runs/${await startRunChecked(scenarioId, afterSave)}`); }
    catch (e) { setBlocked({ scenarioId, err: e instanceof RunBlocked ? e : new RunBlocked('api', e instanceof Error ? e.message : String(e)) }); }
    setBusy(null);
  };
  return { run: (id: string, afterSave?: boolean) => void run(id, afterSave), blocked, busy, clear: () => setBlocked(null) };
}

/** Why a run did not start, with the way out (verify the domain, approve the draft, lower the load). */
export function RunBlockedCallout({ err, scenarioName, onClose }: { err: RunBlocked; scenarioName?: string; onClose?: () => void }) {
  const title = { draft: 'Draft scenario', domain: 'Target domain not verified', limit: 'Over your plan limits', api: 'Run could not start' }[err.kind];
  return (
    <Callout icon={<AlertTriangle size={16} aria-hidden />}>
      <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        <strong>{title}{scenarioName ? ` · ${scenarioName}` : ''}</strong>
        <span className="secondary">{err.message}</span>
        <span className="spacer" />
        {err.kind === 'domain' && <Link to="/load-tests/domains">Verify domain →</Link>}
        {onClose && <Button size="sm" variant="ghost" iconOnly aria-label="Dismiss" icon={<X size={14} />} onClick={onClose} />}
      </div>
    </Callout>
  );
}

export function ScenarioSource({ s }: { s: LoadTestScenario }) {
  if (scenarioState(s.id) === 'draft') return <Badge tone="warning" icon={<Sparkles />}>Draft · needs review</Badge>;
  return s.source === 'generated' ? <Badge tone="accent" icon={<Sparkles />}>Generated</Badge> : <Badge>Manual</Badge>;
}

// ---------- screen ----------

export function LoadTests() {
  useData();
  useLoadTests();
  const nav = useNavigate();
  const d = useDrawer<LoadTestScenario>();
  const [params, setParams] = useSearchParams();
  // ?scenario=id opens its drawer (used after saving in the editor)
  useEffect(() => {
    const sc = scenarios.find((s) => s.id === params.get('scenario'));
    if (sc) { d.open(sc); setParams((p) => { const n = new URLSearchParams(p); n.delete('scenario'); return n; }, { replace: true }); }
  }, [params]); // eslint-disable-line react-hooks/exhaustive-deps
  const live = runs.map(liveRun);
  const active = live.filter((r) => r.state === 'running');
  const finished = live.filter((r) => r.state !== 'running');
  const starter = useRunStarter();
  const run = starter.run;
  const drafts = scenarios.filter((s) => scenarioState(s.id) === 'draft');

  return (
    <>
      <PageHeader title="Load tests" subtitle="Generate load from multiple regions and watch your services respond in real time"
        actions={<>
          <Button icon={<ShieldCheck size={14} />} onClick={() => nav('/load-tests/domains')}>Domains</Button>
          <Button icon={<Sparkles size={14} />} onClick={() => nav('/load-tests/new?from=traffic')}>Generate from traffic</Button>
          <Button variant="primary" icon={<Plus size={14} />} onClick={() => nav('/load-tests/new')}>New scenario</Button>
        </>} />

      {active.length > 0 && (
        <Section title="Running now">
          {active.map((r) => <RunningCard key={r.id} run={r} onOpen={() => nav(`/load-tests/runs/${r.id}`)} />)}
        </Section>
      )}

      {starter.blocked && <RunBlockedCallout err={starter.blocked.err} scenarioName={scenarios.find((s) => s.id === starter.blocked!.scenarioId)?.name} onClose={starter.clear} />}

      <Section title="Scenarios" subtitle={drafts.length
        ? `${drafts.length} generated from code analysis wait${drafts.length === 1 ? 's' : ''} for review — drafts can't run until approved`
        : 'Click a scenario for its spec and run history'}>
        <Card flush>
          <Table rows={scenarios} rowKey={(s) => s.id} onRowClick={d.open} columns={[
            { key: 'name', header: 'Scenario', render: (s) => <div><div style={{ fontWeight: 500 }}>{s.name}</div><div className="muted mono" style={{ fontSize: 'var(--text-xs)' }}>{s.target}</div></div> },
            { key: 'src', header: 'Source', render: (s) => <ScenarioSource s={s} /> },
            { key: 'vus', header: 'Peak VUs', numeric: true, sortValue: (s) => s.peakVus, render: (s) => s.peakVus },
            { key: 'dur', header: 'Duration', numeric: true, sortValue: (s) => s.duration, render: (s) => duration(s.duration) },
            { key: 'trend', header: 'p95 over runs', render: (s) => { const h = pastRuns(s); return h.length > 1 ? <Sparkline data={h.map((r) => r.p95).reverse()} width={100} height={24} /> : <span className="muted">—</span>; } },
            { key: 'last', header: 'Last run', render: (s) => { const r = live.find((x) => x.id === s.lastRun); return r ? <RunState state={r.state} /> : <span className="muted">Never run</span>; } },
            { key: 'run', header: '', width: 72, render: (s) => scenarioState(s.id) === 'draft'
              ? <Button size="sm" onClick={(e) => { e.stopPropagation(); d.open(s); }}>Review</Button>
              : (
              <Button size="sm" icon={<Play size={12} />} disabled={active.some((r) => r.scenarioId === s.id) || starter.busy === s.id}
                onClick={(e) => { e.stopPropagation(); run(s.id); }}>Run</Button>) },
            { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
          ]} />
        </Card>
      </Section>

      <Section title="Recent runs">
        <Card flush>
          <Table rows={finished} rowKey={(r) => r.id} onRowClick={(r) => nav(`/load-tests/runs/${r.id}`)} defaultSort={{ key: 'when', dir: 'desc' }} empty="No finished runs yet." columns={[
            { key: 'state', header: 'Result', render: (r) => <RunState state={r.state} /> },
            { key: 'sc', header: 'Scenario', render: (r) => scenarios.find((s) => s.id === r.scenarioId)?.name },
            { key: 'rps', header: 'Peak RPS', numeric: true, sortValue: (r) => r.rps, render: (r) => compact(r.rps) },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (r) => r.p95, render: (r) => ms(r.p95) },
            { key: 'err', header: 'Errors', numeric: true, sortValue: (r) => r.errorRate, render: (r) => pct(r.errorRate) },
            { key: 'th', header: 'Thresholds', render: (r) => `${r.thresholds.filter((t) => t.passed).length}/${r.thresholds.length} passed` },
            { key: 'when', header: 'Started', numeric: true, sortValue: (r) => r.startedAt, render: (r) => <span className="muted">{ago(r.startedAt)}</span> },
            { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
          ]} />
        </Card>
      </Section>

      {d.item && <ScenarioDrawer sc={d.item} running={active.some((r) => r.scenarioId === d.item!.id)} onClose={d.close}
        onRun={() => run(d.item!.id)} onEdit={() => nav(`/load-tests/scenarios/${d.item!.id}/edit`)} onOpenRun={(id) => nav(`/load-tests/runs/${id}`)} />}
    </>
  );
}

function RunningCard({ run, onOpen }: { run: LoadTestRun; onOpen: () => void }) {
  const sc = scenarios.find((s) => s.id === run.scenarioId)!;
  const ls = levelState(run);
  const lv = ls.levels[ls.index];
  const next = ls.levels[ls.index + 1];
  const unit = SPEC[sc.id].mode === 'rps' ? 'req/s' : 'VUs';
  const g = ls.mode !== 'auto' && next && next.target > lv.target ? gateVerdict(run, ls.index) : undefined;
  const status = ls.userHold ? 'held by you'
    : ls.phase === 'waiting' ? (ls.mode === 'ai' ? `AI holding — ${next?.name ?? 'next level'} not safe` : `waiting for you to start ${next?.name}`)
    : ls.phase === 'ramp' ? `ramping to ${lv.target} ${unit}` : `holding ${lv.target} ${unit}`;
  const c = runCurves(run);
  const last = (a: number[]) => a[a.length - 1] ?? 0;
  const facts: [string, string][] = [
    ['Virtual users', String(last(c.vus))],
    ['Requests / sec', compact(last(c.rps))],
    ['p95', ms(last(c.p95))],
    ['Failed', pct(last(c.err))],
  ];
  return (
    <Card onClick={onOpen}>
      <div className="stack" style={{ gap: 'var(--space-4)' }}>
        <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <RunState state={run.state} />
          <h3 style={{ margin: 0 }}>{sc.name}</h3>
          <span className="mono muted" style={{ fontSize: 'var(--text-sm)' }}>{sc.target}</span>
          <span className="row muted" style={{ gap: 4, fontSize: 'var(--text-sm)' }}><Globe2 size={14} />{sc.regions.join(', ')}</span>
          <span className="spacer" />
          <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>started {ago(run.startedAt)} · {duration(Math.max(0, sc.duration * (1 - run.progress)))} left</span>
          <ChevronRight size={16} className="muted" aria-hidden />
        </div>
        <div className="grid" style={{ gridTemplateColumns: 'minmax(0, 1fr) minmax(0, 1.4fr)', gap: 'var(--space-6)', alignItems: 'center' }}>
          <div className="stack" style={{ gap: 'var(--space-4)' }}>
            <div>
              <div className="row" style={{ justifyContent: 'space-between', marginBottom: 'var(--space-2)' }}>
                <span className="secondary row" style={{ gap: 'var(--space-2)' }}>
                  {lv.name} ({ls.index + 1} of {ls.levels.length}) · {status}
                  {ls.mode === 'ai' && <Badge tone={ls.phase === 'waiting' ? 'warning' : 'accent'} icon={<Sparkles />}>{ls.phase === 'waiting' ? 'needs a look' : 'AI-guided'}</Badge>}
                  {ls.mode === 'manual' && ls.phase === 'waiting' && !ls.userHold && <Badge tone="warning">your call{g && g.verdict !== 'go' ? ' · AI says hold' : ''}</Badge>}
                </span>
                <span className="num">{Math.round(run.progress * 100)}%</span>
              </div>
              <Meter value={run.progress} />
            </div>
            <div className="grid grid-4" style={{ gap: 'var(--space-3)' }}>
              {facts.map(([k, v]) => (
                <div key={k}>
                  <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{k}</div>
                  <div className="num" style={{ fontSize: 'var(--text-xl)', fontWeight: 600 }}>{v}</div>
                </div>
              ))}
            </div>
          </div>
          <TimeSeries timestamps={c.ts} height={130} area format={(v) => compact(v)} series={[{ id: 'rps', label: 'Requests / sec', data: c.rps }]} />
        </div>
      </div>
    </Card>
  );
}

export function ScenarioDrawer({ sc, running, onClose, onRun, onEdit, onOpenRun }: { sc: LoadTestScenario; running: boolean; onClose: () => void; onRun: () => void; onEdit: () => void; onOpenRun: (id: string) => void }) {
  const spec = SPEC[sc.id];
  const hist = pastRuns(sc);
  const draft = scenarioState(sc.id) === 'draft';
  const [approving, setApproving] = useState(false);
  const [approveErr, setApproveErr] = useState('');
  const approve = async () => {
    setApproving(true); setApproveErr('');
    try { await approveScenario(sc.id); } catch (e) { setApproveErr(e instanceof Error ? e.message : String(e)); }
    setApproving(false);
  };
  const blockers = draft ? [] : runBlockers(sc.id);
  return (
    <Drawer open onClose={onClose} title={sc.name}
      subtitle={<span className="mono muted">{sc.target}</span>}
      actions={<>
        <Button size="sm" icon={<Pencil size={12} />} onClick={onEdit}>Edit</Button>
        {draft
          ? <Button size="sm" variant="primary" icon={<Check size={12} />} disabled={approving} onClick={() => void approve()}>{approving ? 'Approving…' : 'Approve'}</Button>
          : <Button size="sm" variant="primary" icon={<Play size={12} />} disabled={running || blockers.length > 0} onClick={onRun}>{running ? 'Running…' : 'Run'}</Button>}
      </>}>
      {draft && (
        <DrawerSection title="Review & approve" aside={<ScenarioSource s={sc} />}>
          <p className="secondary" style={{ margin: 0 }}>
            Code analysis generated this scenario from the handlers and their request schemas. Check the target, the load levels and the traffic mix below — a draft never runs until someone approves it.
          </p>
          {approveErr && <div className="row" style={{ gap: 'var(--space-2)', marginTop: 'var(--space-2)' }}><StatusBadge status="critical" label="Error" /><span className="secondary">{approveErr}</span></div>}
        </DrawerSection>
      )}
      {blockers.length > 0 && (
        <DrawerSection title="Can't run yet">
          <div className="stack" style={{ gap: 'var(--space-2)' }}>{blockers.map((b, k) => <RunBlockedCallout key={k} err={b} />)}</div>
        </DrawerSection>
      )}
      <DrawerSection title="Spec">
        <KeyValues items={[
          ['Services', spec.services.join(', ')],
          ['Peak VUs', sc.peakVus],
          ['Duration', duration(sc.duration)],
          ['Regions', sc.regions.join(', ')],
          ['Source', sc.source === 'generated' ? (draft ? 'Generated from code · draft' : 'Generated') : 'Manual'],
        ]} />
      </DrawerSection>
      <DrawerSection title="Load levels" aside={spec.mode === 'rps' ? 'requests / sec' : 'virtual users'}>
        <div className="stack" style={{ gap: 'var(--space-1)' }}>
          {levelsOf(spec).map((l, k) => (
            <div key={k} className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="muted num" style={{ width: 20 }}>{k + 1}</span>
              <span style={{ minWidth: 110 }}>{l.name}</span>
              <span className="secondary">{l.target === 0 ? 'ramp down' : `${l.target} ${spec.mode === 'rps' ? 'req/s' : 'VUs'}`}</span>
              <span className="spacer" />
              <span className="num secondary">{l.ramp ? `ramp ${duration(l.ramp)}` : ''}{l.ramp && l.hold ? ' · ' : ''}{l.hold ? `hold ${duration(l.hold)}` : ''}</span>
            </div>
          ))}
        </div>
      </DrawerSection>
      <DrawerSection title="Traffic mix & thresholds">
        <div className="stack" style={{ gap: 'var(--space-1)' }}>
          {spec.steps.map((s) => (
            <div key={s} className="row" style={{ gap: 'var(--space-3)' }}>
              <span className="mono" style={{ fontSize: 'var(--text-sm)', flex: 1 }}>{s}</span>
              {spec.weights?.[s] !== undefined && <span className="num secondary">{pct(spec.weights[s], 1)}</span>}
              {spec.requests?.[s]?.source && <Badge tone="accent" icon={<Sparkles />}>From production</Badge>}
            </div>
          ))}
        </div>
        <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap', marginTop: 'var(--space-3)' }}>
          {spec.thresholds.map((t) => <Badge key={t}>{t}</Badge>)}
        </div>
      </DrawerSection>
      <DrawerSection title="Run history">
        {hist.length === 0 ? <span className="muted">Not run yet.</span> : (
          <Table rows={hist} rowKey={(r) => r.id} onRowClick={(r) => r.real && onOpenRun(r.id)} columns={[
            { key: 'state', header: 'Result', render: (r) => <RunState state={r.state} /> },
            { key: 'p95', header: 'p95', numeric: true, render: (r) => ms(r.p95) },
            { key: 'err', header: 'Errors', numeric: true, render: (r) => pct(r.errorRate) },
            { key: 'when', header: 'Started', numeric: true, render: (r) => <span className="muted">{ago(r.startedAt)}</span> },
          ]} />
        )}
      </DrawerSection>
      <DrawerSection title="Scenario file" aside={`loadtests/${slug(sc.name)}.yaml`}>
        <CodeBlock>{yamlFor(sc, spec)}</CodeBlock>
      </DrawerSection>
    </Drawer>
  );
}

