import { useData } from '@/data/remote';
import { Link, useNavigate, useParams } from 'react-router';
import { AlertTriangle, CheckCircle2, ChevronRight, CircleDashed, ExternalLink, Pencil, Play, Square, XCircle } from 'lucide-react';
import { BarList, Sparkline, TimeSeries } from '@/components/charts';
import {
  Button, Callout, Card, Delta, Drawer, DrawerSection, EmptyState, KeyValues, MethodBadge, MetricCard, Meter, PageHeader, Section, StatusBadge, Table, useDrawer,
} from '@/components/ui';
import { isApi, runs, services, type LoadTestRun as Run } from '@/data';
import { compact, duration, int, ms, pct } from '@/lib/format';
import {
  SPEC, hasControl, levelState, limitOf, liveRun, runCurves, runEndpoints, scenarioOf, stageAt, stopRun, useLoadTests, type RunCurves, type RunEndpoint,
} from '@/lib/loadtest';
import { previousRun, runError, scenarioState, useRunDetail, type StepStat, type TargetRed } from '@/data/loadtest';
import { RunBlockedCallout, RunState, useRunStarter } from './LoadTests';
import { LevelControl } from './LevelControl';
import { latencyFromP95, latencyHint } from './apm';

const EMPTY_CURVES: RunCurves = { ts: [], vus: [], rps: [], p95: [], err: [], load: [], upto: 0, step: 5000 };
const last = (a: number[]) => a[a.length - 1] ?? 0;
const peak = (a: number[]) => Math.max(0, ...a);

export function LoadTestRun() {
  useData();
  useLoadTests();
  const nav = useNavigate();
  const { runId = '' } = useParams();
  const d = useDrawer<RunEndpoint>();
  const starter = useRunStarter();
  const stored = runs.find((r) => r.id === runId);
  const detail = useRunDetail(stored ? liveRun(stored) : undefined);
  if (!stored) return <EmptyState icon={<Square />} title="Run not found" />;
  const run = liveRun(stored);
  const sc = scenarioOf(run);
  const spec = SPEC[sc.id] ?? { services: [], stages: [], steps: [], thresholds: [] };
  const running = run.state === 'running';
  const queued = run.state === 'queued';
  const error = runError(run.id, detail);

  // api: the series endpoint; mock: the simulated curves
  const c = isApi ? detail.curves ?? EMPTY_CURVES : runCurves(run);
  const hasData = c.ts.length > 1;
  const live = detail.live;
  const progress = live?.progress ?? run.progress;
  const p95Th = run.thresholds.find((t) => t.name.includes('duration'));
  const errTh = run.thresholds.find((t) => t.name.includes('failed'));
  const p95Limit = p95Th ? limitOf(p95Th.name) : undefined;
  const errLimit = errTh ? limitOf(errTh.name) : undefined;
  const nowP95 = live?.p95 ?? last(c.p95);
  const nowErr = live?.errorRate ?? last(c.err);
  // while running, thresholds are judged on what has been seen so far
  const seenP95 = running ? Math.max(peak(c.p95), live?.p95 ?? 0) : run.p95;
  const seenErr = running ? Math.max(peak(c.err), live?.errorRate ?? 0) : run.errorRate;
  const verdict = run.thresholds.map((t) => {
    const lim = limitOf(t.name);
    const isLat = t.name.includes('duration');
    const v = isLat ? seenP95 : seenErr;
    return { ...t, limit: lim === undefined ? '—' : isLat ? ms(lim) : pct(lim), value: isLat ? ms(v) : pct(v), passed: running ? lim === undefined || v < lim : t.passed };
  });
  const failedCount = verdict.filter((t) => !t.passed).length;
  const stage = stageAt(sc, progress);
  const eps = isApi ? [] : runEndpoints(run, c);
  const prev = !running && !queued && !error ? previousRun(run) : undefined;
  const totalReq = detail.steps.reduce((a, s) => a + s.requests, 0) || c.rps.reduce((a, v) => a + v, 0) * (c.step / 1000);
  // level changes marked on the load charts
  const marks = hasControl(run)
    ? levelState(run).decisions.filter((x) => x.kind !== 'wait').map((x) => ({ ts: x.at, label: x.kind === 'hold' ? `${x.actor === 'ai' ? 'AI' : 'you'}: hold` : x.kind === 'back' ? 'step back' : x.kind === 'advance' ? (x.actor === 'ai' ? 'AI ↑' : x.actor === 'you' ? 'you ↑' : '↑') : x.kind }))
    : undefined;
  const draft = scenarioState(sc.id) === 'draft';

  return (
    <>
      <PageHeader title={sc.name} crumbs={[{ label: 'Load tests', to: '/load-tests' }, { label: `Run ${run.id}` }]}
        subtitle={<span className="row"><RunState state={run.state} /><span className="mono muted">{sc.target}</span><span className="muted">· {sc.regions.join(', ')} · {duration(sc.duration)} · peak {sc.peakVus} VUs</span></span>}
        actions={running || queued
          ? <Button variant="danger" icon={<Square size={14} />} onClick={() => stopRun(run.id)}>{queued ? 'Cancel run' : 'Stop run'}</Button>
          : <>
              <Button icon={<Pencil size={14} />} onClick={() => nav(`/load-tests/scenarios/${sc.id}/edit`)}>Edit scenario</Button>
              <Button variant="primary" icon={<Play size={14} />} disabled={draft || starter.busy === sc.id} onClick={() => starter.run(sc.id)}>Run again</Button>
            </>} />

      {starter.blocked && <RunBlockedCallout err={starter.blocked.err} onClose={starter.clear} />}

      {/* 0 · the run never produced load */}
      {error && (
        <Callout icon={<AlertTriangle size={16} aria-hidden />}>
          <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
            <strong>The run did not complete.</strong><span className="secondary">{error}</span>
            <span className="spacer" />
            {/domain|verif/i.test(error) && <Link to="/load-tests/domains">Verify domain →</Link>}
          </div>
        </Callout>
      )}

      {/* 0 · who moves the load between levels */}
      {hasControl(run) && <LevelControl run={run} />}

      {/* 1 · verdict */}
      {!error && (
        <Card>
          <div className="stack" style={{ gap: 'var(--space-4)' }}>
            {queued && <div className="row secondary" style={{ gap: 'var(--space-2)' }}><CircleDashed size={16} aria-hidden />Queued — waiting for a runner to pick it up. This page updates every 5 seconds.</div>}
            {running && !hasControl(run) && (
              <div>
                <div className="row" style={{ justifyContent: 'space-between', marginBottom: 'var(--space-2)' }}>
                  <span className="secondary">{stage.count ? `Stage ${stage.index + 1} of ${stage.count} · ${stage.target === 0 ? 'ramping down' : `ramping to ${stage.target} VUs`}` : 'Running'}</span>
                  <span className="num">{Math.round(progress * 100)}% · {duration(sc.duration * (1 - progress))} left</span>
                </div>
                <Meter value={progress} />
              </div>
            )}
            {run.state === 'aborted' && <div className="secondary">Stopped at {Math.round(progress * 100)}% — results cover the part that ran.</div>}
            {verdict.length > 0 && (
              <div className="row" style={{ gap: 'var(--space-6)', flexWrap: 'wrap', alignItems: 'flex-start' }}>
                <div style={{ minWidth: 180 }}>
                  <div className="eyebrow">Thresholds</div>
                  <div className="row" style={{ gap: 'var(--space-2)', fontSize: 'var(--text-xl)', fontWeight: 600 }}>
                    {running || queued
                      ? <><CircleDashed size={20} color="var(--accent)" />{failedCount ? `${failedCount} failing so far` : 'Passing so far'}</>
                      : failedCount
                        ? <><XCircle size={20} color="var(--status-critical)" />{failedCount} of {verdict.length} failed</>
                        : <><CheckCircle2 size={20} color="var(--status-good)" />All {verdict.length} passed</>}
                  </div>
                </div>
                {verdict.map((t) => (
                  <div key={t.name} style={{ minWidth: 200 }}>
                    <div className="row" style={{ gap: 'var(--space-2)' }}>
                      {t.passed ? <CheckCircle2 size={14} color="var(--status-good)" /> : <XCircle size={14} color="var(--status-critical)" />}
                      <span className={t.passed ? 'delta delta--good' : 'delta delta--bad'}>{t.passed ? 'Pass' : 'Fail'}</span>
                      <span className="num">{t.value}</span>
                    </div>
                    <div className="mono muted" style={{ fontSize: 'var(--text-xs)', marginTop: 2 }}>{t.name}</div>
                  </div>
                ))}
              </div>
            )}
          </div>
        </Card>
      )}

      {/* 2 · load generator: one chart per measure (single y-axis each) */}
      {!error && (
        <Section title="Load generator" subtitle={`Measured at the load generator, all regions · ${running ? 'live, every 5 s' : queued ? 'waiting for the first data points' : `over ${duration(sc.duration * progress)}`}`}>
          {!hasData && isApi ? (
            <Card><EmptyState icon={<CircleDashed size={20} />} title={running || queued ? 'Waiting for the first data points' : 'No load-generator series for this run'}>
              {running || queued ? 'Series appear within a few seconds of the runner starting.' : 'The api returned no series (older run, or the series endpoint is not available yet).'}
            </EmptyState></Card>
          ) : (
            <div className="grid grid-2">
              <MetricCard label="Response time · p95" value={ms(running ? nowP95 : run.p95)}
                hint={`${latencyHint(latencyFromP95(running ? nowP95 : run.p95), 'p95')}${p95Limit !== undefined ? ` · threshold ${ms(p95Limit)}` : ''}`}>
                <TimeSeries timestamps={c.ts} height={180} format={ms}
                  threshold={p95Limit !== undefined ? { value: p95Limit, label: ms(p95Limit) } : undefined}
                  series={[{ id: 'p95', label: 'p95', slot: 1, data: c.p95 }]} />
              </MetricCard>
              <MetricCard label="Error rate" value={pct(running ? nowErr : run.errorRate)}
                hint={errLimit !== undefined ? `Threshold ${pct(errLimit)}` : undefined}>
                <TimeSeries timestamps={c.ts} height={180} format={(v) => pct(v)}
                  threshold={errLimit !== undefined ? { value: errLimit, label: pct(errLimit) } : undefined}
                  series={[{ id: 'err', label: 'Failed', slot: 8, data: c.err }]} />
              </MetricCard>
              <MetricCard label="Virtual users" value={int(running ? live?.vus ?? last(c.vus) : peak(c.vus))} unit={running ? 'now' : 'peak'}
                hint={(spec.levels ?? []).length ? spec.levels!.map((l) => l.target).join(' → ') + ' VUs' : spec.stages.map((s) => `${s.target} VUs / ${duration(s.duration)}`).join(' → ')}>
                <TimeSeries timestamps={c.ts} height={140} events={marks} series={[{ id: 'vu', label: 'VUs', slot: 2, data: c.vus }]} />
              </MetricCard>
              <MetricCard label="Requests / sec" value={compact(running ? live?.rps ?? last(c.rps) : peak(c.rps))} unit={running ? 'now' : 'peak'}
                hint={`${compact(totalReq)} requests sent`}>
                <TimeSeries timestamps={c.ts} height={140} format={(v) => compact(v)} series={[{ id: 'rps', label: 'RPS', slot: 3, data: c.rps }]} />
              </MetricCard>
            </div>
          )}
        </Section>
      )}

      {/* 3 · the services under test, test traffic vs everything they served */}
      {!error && spec.services.length > 0 && (
        <Section title="Target services" subtitle="From Trooper SDK · load-test traffic (caller loadtest:<run>) vs all traffic in the same window">
          {detail.targets.length === 0
            ? <Card><EmptyState icon={<CircleDashed size={20} />} title="No target-service data yet">{isApi ? 'Appears once the services report spans for this window (or when the api serves /target).' : 'Waiting for data.'}</EmptyState></Card>
            : <div className="stack" style={{ gap: 'var(--space-4)' }}>{detail.targets.map((t) => <TargetCard key={t.service} t={t} running={running} />)}</div>}
        </Section>
      )}

      {/* 4 · final summary */}
      {!running && !queued && !error && (
        <Section title="Summary" subtitle={prev ? `Compared with the previous run of this scenario (${prev.id}, ${new Date(prev.startedAt).toLocaleDateString()})` : 'First run of this scenario'}>
          <div className="grid grid-2">
            <Card title="Thresholds" flush>
              <Table rows={verdict} rowKey={(t) => t.name} empty="No thresholds defined." columns={[
                { key: 'n', header: 'Threshold', render: (t) => <span className="mono" style={{ fontSize: 'var(--text-sm)' }}>{t.name}</span> },
                { key: 'l', header: 'Limit', numeric: true, render: (t) => t.limit },
                { key: 'v', header: 'Measured', numeric: true, render: (t) => t.value },
                { key: 'r', header: 'Result', render: (t) => <StatusBadge status={t.passed ? 'ok' : 'critical'} label={t.passed ? 'Pass' : 'Fail'} /> },
              ]} />
            </Card>
            <Card title="Status codes" subtitle={`${compact(Object.values(detail.statusCodes).reduce((a, v) => a + v, 0))} responses`}>
              {Object.keys(detail.statusCodes).length === 0
                ? <span className="muted">No status-code breakdown for this run.</span>
                : <BarList items={Object.entries(detail.statusCodes).sort(([a], [b]) => a.localeCompare(b)).map(([code, n]) => ({ id: code, label: <span className="mono">{code}</span>, value: n, display: compact(n) }))} />}
            </Card>
          </div>
          {prev && <Comparison run={run} prev={prev} />}
        </Section>
      )}

      {/* 5 · per step */}
      {!error && (
        <Section title="Per-step results" subtitle={eps.length ? 'Each scenario step · click for its latency and errors over the run' : 'Each scenario step'}>
          <Card flush>
            <Table<StepStat> rows={detail.steps} rowKey={(s) => s.key} defaultSort={{ key: 'p95', dir: 'desc' }}
              onRowClick={eps.length ? (s) => { const e = eps.find((x) => `${x.method} ${x.route}` === s.key); if (e) d.open(e); } : undefined}
              empty={running || queued ? 'Per-step numbers appear when the run reports them.' : 'No per-step stats for this run.'}
              columns={[
                { key: 'ep', header: 'Step', render: (s) => { const [m, ...r] = s.key.split(' '); return <div className="row" style={{ gap: 'var(--space-2)' }}>{r.length ? <><MethodBadge method={m} /><span className="mono">{r.join(' ')}</span></> : <span className="mono">{s.key}</span>}</div>; } },
                { key: 'req', header: 'Requests', numeric: true, sortValue: (s) => s.requests, render: (s) => compact(s.requests) },
                { key: 'err', header: 'Errors', numeric: true, sortValue: (s) => s.errors / (s.requests || 1), render: (s) => <>{pct(s.errors / (s.requests || 1))} <span className="muted">({compact(s.errors)})</span></> },
                { key: 'p50', header: 'p50', numeric: true, sortValue: (s) => s.p50 ?? 0, render: (s) => (s.p50 !== undefined ? ms(s.p50) : '—') },
                { key: 'p95', header: 'p95', numeric: true, sortValue: (s) => s.p95, render: (s) => ms(s.p95) },
                { key: 'p99', header: 'p99', numeric: true, sortValue: (s) => s.p99 ?? 0, render: (s) => (s.p99 !== undefined ? ms(s.p99) : '—') },
                ...(eps.length ? [
                  { key: 'trend', header: 'p95 over run', render: (s: StepStat) => { const e = eps.find((x) => `${x.method} ${x.route}` === s.key); return e ? <Sparkline data={e.p95Series} width={100} height={24} /> : null; } },
                  { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
                ] : []),
              ]} />
          </Card>
        </Section>
      )}

      {d.item && <EndpointRunDrawer ep={d.item} ts={c.ts} run={run} onClose={d.close}
        onOpen={() => nav(`/services/${d.item!.serviceId}/endpoints/${d.item!.id}`)} p95Limit={p95Limit} errLimit={errLimit} />}
    </>
  );
}

/** RED of one service under test: test traffic vs all traffic, one measure per chart. */
function TargetCard({ t, running }: { t: TargetRed; running: boolean }) {
  const svc = services.find((s) => s.id === t.service || s.name === t.service);
  const v = (a: number[]) => (running ? last(a) : peak(a));
  const share = v(t.total.rps) ? v(t.test.rps) / v(t.total.rps) : 0;
  const two = (id: string, total: number[], test: number[]) => [
    { id: `${id}-total`, label: 'All traffic', slot: 1, data: total },
    { id: `${id}-test`, label: 'Load test', slot: 2, data: test },
  ];
  return (
    <Card title={svc?.name ?? t.service} subtitle={`load test is ${pct(share, 0)} of its traffic ${running ? 'now' : 'at peak'}`}>
      <div className="grid grid-3">
        <MetricCard label="Requests / sec" value={compact(v(t.total.rps))} unit={running ? 'now' : 'peak'} hint={`${compact(v(t.test.rps))} from the load test`}>
          <TimeSeries timestamps={t.ts} height={140} format={(x) => compact(x)} series={two('rps', t.total.rps, t.test.rps)} />
        </MetricCard>
        <MetricCard label="Error rate" value={pct(v(t.total.errorRate))} unit={running ? 'now' : 'peak'} hint={`${pct(v(t.test.errorRate))} on load-test requests`}>
          <TimeSeries timestamps={t.ts} height={140} format={(x) => pct(x)} series={two('err', t.total.errorRate, t.test.errorRate)} />
        </MetricCard>
        <MetricCard label="p95" value={ms(v(t.total.p95))} unit={running ? 'now' : 'peak'} hint={`${ms(v(t.test.p95))} on load-test requests`}>
          <TimeSeries timestamps={t.ts} height={140} format={ms} series={two('p95', t.total.p95, t.test.p95)} />
        </MetricCard>
      </div>
    </Card>
  );
}

/** This run vs the previous run of the same scenario. */
function Comparison({ run, prev }: { run: Run; prev: Run }) {
  const passed = (r: Run) => r.thresholds.filter((t) => t.passed).length;
  const rows: { k: string; label: string; cur: number; prev: number; fmt: (v: number) => string; upIsGood: boolean }[] = [
    { k: 'p95', label: 'p95', cur: run.p95, prev: prev.p95, fmt: ms, upIsGood: false },
    { k: 'err', label: 'Error rate', cur: run.errorRate, prev: prev.errorRate, fmt: (v) => pct(v), upIsGood: false },
    { k: 'rps', label: 'Peak requests / sec', cur: run.rps, prev: prev.rps, fmt: (v) => compact(v), upIsGood: true },
    { k: 'vus', label: 'Peak virtual users', cur: run.vus, prev: prev.vus, fmt: int, upIsGood: true },
    { k: 'th', label: 'Thresholds passed', cur: passed(run), prev: passed(prev), fmt: (v) => String(v), upIsGood: true },
  ];
  return (
    <Card title="Compared with the previous run" flush actions={<Link to={`/load-tests/runs/${prev.id}`} className="row" style={{ gap: 4 }}>Open {prev.id}<ExternalLink size={12} aria-hidden /></Link>}>
      <Table rows={rows} rowKey={(r) => r.k} columns={[
        { key: 'm', header: 'Measure', render: (r) => r.label },
        { key: 'p', header: `Previous (${prev.id})`, numeric: true, render: (r) => <span className="muted">{r.fmt(r.prev)}</span> },
        { key: 'c', header: `This run (${run.id})`, numeric: true, render: (r) => r.fmt(r.cur) },
        { key: 'd', header: 'Change', numeric: true, render: (r) => <Delta curr={r.cur} prev={r.prev} upIsGood={r.upIsGood} period="" /> },
      ]} />
    </Card>
  );
}

function EndpointRunDrawer({ ep, ts, run, onClose, onOpen, p95Limit, errLimit }: {
  ep: RunEndpoint; ts: number[]; run: Run; onClose: () => void; onOpen: () => void; p95Limit?: number; errLimit?: number;
}) {
  return (
    <Drawer open onClose={onClose}
      title={<span className="row" style={{ gap: 'var(--space-2)' }}><MethodBadge method={ep.method} /><span className="mono">{ep.route}</span></span>}
      subtitle={`${ep.serviceId} · run ${run.id}`}
      actions={<Button size="sm" icon={<ExternalLink size={12} />} onClick={onOpen}>Open endpoint</Button>}>
      <DrawerSection title="Summary">
        <KeyValues items={[
          ['Requests', int(ep.requests)],
          ['p95 in run', ms(ep.runP95)],
          ['p95 in production', ms(ep.p95)],
          ['Failed in run', pct(ep.runErr)],
        ]} />
      </DrawerSection>
      <DrawerSection title="Response time · p95">
        <TimeSeries timestamps={ts} height={160} format={ms}
          threshold={p95Limit !== undefined ? { value: p95Limit, label: ms(p95Limit) } : undefined}
          series={[{ id: 'p95', label: 'p95', slot: 1, data: ep.p95Series }]} />
      </DrawerSection>
      <DrawerSection title="Failed requests">
        <TimeSeries timestamps={ts} height={160} format={(v) => pct(v)}
          threshold={errLimit !== undefined ? { value: errLimit, label: pct(errLimit) } : undefined}
          series={[{ id: 'err', label: 'Failed', slot: 8, data: ep.errSeries }]} />
      </DrawerSection>
    </Drawer>
  );
}
