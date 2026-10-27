import { useState } from 'react';
import { useNavigate } from 'react-router';
import { ArrowDownRight, ArrowUpRight, ChevronRight, Gauge, Play, Plus, Sparkles } from 'lucide-react';
import { Badge, Button, Card, EmptyState, MethodBadge, Section, Table, useDrawer } from '@/components/ui';
import { runs, scenarios, services, type LoadTestRun, type LoadTestScenario } from '@/data';
import { ago, compact, duration, ms, pct } from '@/lib/format';
import { SPEC, levelsOf, liveRun, runCurves, runEndpoints, useLoadTests, type RunEndpoint } from '@/lib/loadtest';
import { scenarioState } from '@/data/loadtest';
import { RunBlockedCallout, RunState, ScenarioDrawer, ScenarioSource, useRunStarter } from './LoadTests';

const MAX_COMPARE = 4;

/**
 * Load tests that hit one API: its scenarios (run them from here), its runs, and an
 * endpoint-by-endpoint comparison of the runs you pick. The Load tests page is the
 * cross-service list of the same data.
 */
export function ServiceLoadTests({ serviceId }: { serviceId: string }) {
  useLoadTests();
  const nav = useNavigate();
  const drawer = useDrawer<LoadTestScenario>();
  const svc = services.find((s) => s.id === serviceId)!;
  const mine = scenarios.filter((s) => SPEC[s.id]?.services.includes(serviceId));
  const live = runs.map(liveRun).filter((r) => mine.some((s) => s.id === r.scenarioId)).sort((a, b) => b.startedAt - a.startedAt);
  const running = live.filter((r) => r.state === 'running');
  const finished = live.filter((r) => r.state !== 'running');
  const [picked, setPicked] = useState<string[]>(() => finished.slice(0, 2).map((r) => r.id));
  const selected = live.filter((r) => picked.includes(r.id)).sort((a, b) => a.startedAt - b.startedAt);
  const scName = (r: LoadTestRun) => scenarios.find((s) => s.id === r.scenarioId)?.name ?? r.scenarioId;
  const starter = useRunStarter();
  const run = starter.run;
  const togglePick = (id: string) => setPicked((p) => (p.includes(id) ? p.filter((x) => x !== id) : p.length >= MAX_COMPARE ? [...p.slice(1), id] : [...p, id]));

  const newActions = <>
    <Button size="sm" icon={<Sparkles size={14} />} onClick={() => nav(`/load-tests/new?from=traffic&service=${serviceId}`)}>Generate from traffic</Button>
    <Button size="sm" variant="primary" icon={<Plus size={14} />} onClick={() => nav(`/load-tests/new?service=${serviceId}`)}>New load test</Button>
  </>;

  if (mine.length === 0) {
    return (
      <Card>
        <EmptyState icon={<Gauge />} title={`No load test targets ${svc.name} yet`} action={<div className="row" style={{ gap: 'var(--space-2)' }}>{newActions}</div>}>
          Generate one from its production traffic — steps, mix and request data come from real requests.
        </EmptyState>
      </Card>
    );
  }

  return (
    <>
      {starter.blocked && <RunBlockedCallout err={starter.blocked.err} scenarioName={scenarios.find((s) => s.id === starter.blocked!.scenarioId)?.name} onClose={starter.clear} />}
      <Section title="Scenarios" subtitle={`${mine.length} hit ${svc.name}`} actions={newActions}>
        <Card flush>
          <Table rows={mine} rowKey={(s) => s.id} onRowClick={drawer.open} columns={[
            { key: 'name', header: 'Scenario', render: (s) => <div><div style={{ fontWeight: 500 }}>{s.name}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{SPEC[s.id].steps.length} steps · {SPEC[s.id].services.join(', ')}</div></div> },
            { key: 'levels', header: 'Levels', render: (s) => {
              const spec = SPEC[s.id];
              return <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{levelsOf(spec).filter((l) => l.target > 0).map((l) => l.target).join(' → ')} {spec.mode === 'rps' ? 'req/s' : 'VUs'}</span>;
            } },
            { key: 'dur', header: 'Duration', numeric: true, sortValue: (s) => s.duration, render: (s) => duration(s.duration) },
            { key: 'last', header: 'Last run', render: (s) => { const r = live.find((x) => x.id === s.lastRun); return r ? <RunState state={r.state} /> : <span className="muted">Never run</span>; } },
            { key: 'src', header: 'Source', render: (s) => <ScenarioSource s={s} /> },
            { key: 'run', header: '', width: 72, render: (s) => scenarioState(s.id) === 'draft'
              ? <Button size="sm" onClick={(e) => { e.stopPropagation(); drawer.open(s); }}>Review</Button>
              : (
              <Button size="sm" icon={<Play size={12} />} disabled={running.some((r) => r.scenarioId === s.id) || starter.busy === s.id}
                onClick={(e) => { e.stopPropagation(); run(s.id); }}>Run</Button>) },
            { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
          ]} />
        </Card>
      </Section>

      <Section title="Runs" subtitle={`Tick up to ${MAX_COMPARE} runs to compare them endpoint by endpoint`}>
        <Card flush>
          <Table rows={live} rowKey={(r) => r.id} onRowClick={(r) => nav(`/load-tests/runs/${r.id}`)} defaultSort={{ key: 'when', dir: 'desc' }} columns={[
            { key: 'pick', header: 'Compare', width: 80, render: (r) => (
              <input type="checkbox" aria-label={`Compare run ${r.id}`} checked={picked.includes(r.id)}
                onClick={(e) => e.stopPropagation()} onChange={() => togglePick(r.id)} />) },
            { key: 'state', header: 'Result', render: (r) => <RunState state={r.state} /> },
            { key: 'sc', header: 'Scenario', render: (r) => <span>{scName(r)} <span className="muted mono" style={{ fontSize: 'var(--text-xs)' }}>{r.id}</span></span> },
            { key: 'rps', header: 'Peak RPS', numeric: true, sortValue: (r) => r.rps, render: (r) => compact(r.rps) },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (r) => r.p95, render: (r) => ms(r.p95) },
            { key: 'err', header: 'Errors', numeric: true, sortValue: (r) => r.errorRate, render: (r) => pct(r.errorRate) },
            { key: 'when', header: 'Started', numeric: true, sortValue: (r) => r.startedAt, render: (r) => <span className="muted">{ago(r.startedAt)}</span> },
            { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
          ]} />
        </Card>
      </Section>

      <Section title="Compare runs" subtitle={selected.length >= 2 ? `${svc.name} endpoints · oldest selected run is the baseline · Δ vs baseline` : undefined}>
        {selected.length < 2
          ? <Card><EmptyState icon={<Gauge />} title="Pick two or more runs">Tick runs in the table above to compare p95, errors and request volume per endpoint.</EmptyState></Card>
          : <Comparison runs={selected} serviceId={serviceId} scName={scName} />}
      </Section>

      {drawer.item && <ScenarioDrawer sc={drawer.item} running={running.some((r) => r.scenarioId === drawer.item!.id)} onClose={drawer.close}
        onRun={() => run(drawer.item!.id)} onEdit={() => nav(`/load-tests/scenarios/${drawer.item!.id}/edit?service=${serviceId}`)}
        onOpenRun={(id) => nav(`/load-tests/runs/${id}`)} />}
    </>
  );
}

/** Change vs baseline; for latency and errors up is bad. Small moves (< 5%) stay neutral. */
function Delta({ now, base }: { now: number; base: number }) {
  if (!base) return null;
  const d = (now - base) / base;
  if (Math.abs(d) < 0.05) return <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>±{Math.round(Math.abs(d) * 100)}%</span>;
  const worse = d > 0;
  const Icon = worse ? ArrowUpRight : ArrowDownRight;
  return (
    <span className={worse ? 'delta delta--bad' : 'delta delta--good'} style={{ fontSize: 'var(--text-xs)', display: 'inline-flex', alignItems: 'center', gap: 2 }}>
      <Icon size={12} aria-hidden />{worse ? '+' : '−'}{Math.round(Math.abs(d) * 100)}%
    </span>
  );
}

function Comparison({ runs: list, serviceId, scName }: { runs: LoadTestRun[]; serviceId: string; scName: (r: LoadTestRun) => string }) {
  const nav = useNavigate();
  const perRun = list.map((r) => ({ run: r, eps: runEndpoints(r, runCurves(r)).filter((e) => e.serviceId === serviceId) }));
  const keys = [...new Map(perRun.flatMap((p) => p.eps).map((e) => [`${e.method} ${e.route}`, e])).values()]
    .sort((a, b) => b.rpm - a.rpm);
  const base = perRun[0];
  const cell = (p: (typeof perRun)[number], k: RunEndpoint) => p.eps.find((e) => e.id === k.id);
  const sumReq = (p: (typeof perRun)[number]) => p.eps.reduce((a, e) => a + e.requests, 0);

  return (
    <Card flush>
      <div className="table-wrap">
        <table className="table">
          <thead>
            <tr>
              <th rowSpan={2} style={{ verticalAlign: 'bottom' }}>Endpoint</th>
              {perRun.map((p, i) => (
                <th key={p.run.id} colSpan={3} style={{ textAlign: 'left', borderLeft: '1px solid var(--divider)', textTransform: 'none', letterSpacing: 0 }}>
                  <a href={`/load-tests/runs/${p.run.id}`} onClick={(e) => { e.preventDefault(); nav(`/load-tests/runs/${p.run.id}`); }} className="row" style={{ gap: 'var(--space-2)', color: 'inherit' }}>
                    <RunState state={p.run.state} />
                    <span style={{ fontWeight: 600 }}>{p.run.id}</span>
                    <span className="muted" style={{ fontWeight: 400 }}>{ago(p.run.startedAt)}</span>
                    {i === 0 && <Badge>baseline</Badge>}
                  </a>
                  <div className="muted truncate" style={{ fontWeight: 400, fontSize: 'var(--text-xs)', maxWidth: 220 }}>{scName(p.run)}</div>
                </th>
              ))}
            </tr>
            <tr>
              {perRun.map((p) => (
                <FragmentHeaders key={p.run.id} />
              ))}
            </tr>
          </thead>
          <tbody>
            <tr style={{ background: 'var(--bg-sunken)' }}>
              <td style={{ fontWeight: 600 }}>Whole run</td>
              {perRun.map((p, i) => (
                <RunCells key={p.run.id}
                  p95={p.run.p95} err={p.run.errorRate} req={sumReq(p)}
                  baseP95={i ? base.run.p95 : undefined} baseErr={i ? base.run.errorRate : undefined} />
              ))}
            </tr>
            {keys.map((k) => (
              <tr key={k.id}>
                <td><div className="row" style={{ gap: 'var(--space-2)', whiteSpace: 'nowrap' }}><MethodBadge method={k.method} /><span className="mono">{k.route}</span></div></td>
                {perRun.map((p, i) => {
                  const c = cell(p, k);
                  const b = cell(base, k);
                  return c
                    ? <RunCells key={p.run.id} p95={c.runP95} err={c.runErr} req={c.requests}
                        baseP95={i && b ? b.runP95 : undefined} baseErr={i && b ? b.runErr : undefined} />
                    : <td key={p.run.id} colSpan={3} className="muted" style={{ borderLeft: '1px solid var(--divider)' }}>not in this run</td>;
                })}
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </Card>
  );
}

function FragmentHeaders() {
  return <>
    <th className="is-num" style={{ borderLeft: '1px solid var(--divider)' }}>p95</th>
    <th className="is-num">Errors</th>
    <th className="is-num">Requests</th>
  </>;
}

function RunCells({ p95, err, req, baseP95, baseErr }: { p95: number; err: number; req: number; baseP95?: number; baseErr?: number }) {
  return <>
    <td className="is-num" style={{ borderLeft: '1px solid var(--divider)' }}>
      <div>{ms(p95)}</div>{baseP95 !== undefined && <Delta now={p95} base={baseP95} />}
    </td>
    <td className="is-num">
      <div>{pct(err)}</div>{baseErr !== undefined && <Delta now={err} base={baseErr} />}
    </td>
    <td className="is-num muted">{compact(req)}</td>
  </>;
}
