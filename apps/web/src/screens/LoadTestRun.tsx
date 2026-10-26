import { useData } from '@/data/remote';
import { useNavigate, useParams } from 'react-router';
import { CheckCircle2, ChevronRight, CircleDashed, ExternalLink, Pencil, Play, Square, XCircle } from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import {
  Button, Card, Drawer, DrawerSection, EmptyState, KeyValues, MethodBadge, MetricCard, Meter, PageHeader, Section, Table, useDrawer,
} from '@/components/ui';
import { runs, services, type LoadTestRun as Run } from '@/data';
import { compact, duration, int, ms, pct } from '@/lib/format';
import {
  SPEC, dbOf, hasControl, levelState, limitOf, liveRun, runCurves, runEndpoints, scenarioOf, stageAt, startRun, stopRun, useLoadTests, type RunEndpoint,
} from '@/lib/loadtest';
import { RunState } from './LoadTests';
import { LevelControl } from './LevelControl';
import { latencyFromP95, latencyFromP95Series, latencyHint } from './apm';

export function LoadTestRun() {
  useData();
  useLoadTests();
  const nav = useNavigate();
  const { runId = '' } = useParams();
  const d = useDrawer<RunEndpoint>();
  const stored = runs.find((r) => r.id === runId);
  if (!stored) return <EmptyState icon={<Square />} title="Run not found" />;
  const run = liveRun(stored);
  const sc = scenarioOf(run);
  const spec = SPEC[sc.id];
  const running = run.state === 'running';

  const c = runCurves(run);
  const last = (a: number[]) => a[a.length - 1] ?? 0;
  const peak = (a: number[]) => Math.max(0, ...a);
  const p95Th = run.thresholds.find((t) => t.name.includes('duration'));
  const errTh = run.thresholds.find((t) => t.name.includes('failed'));
  const p95Limit = p95Th ? limitOf(p95Th.name) : undefined;
  const errLimit = errTh ? limitOf(errTh.name) : undefined;
  // while running, thresholds are judged on what has been seen so far
  const seenP95 = running ? peak(c.p95) : run.p95;
  const seenErr = running ? peak(c.err) : run.errorRate;
  const verdict = run.thresholds.map((t) => {
    const lim = limitOf(t.name);
    const v = t.name.includes('duration') ? seenP95 : seenErr;
    return { ...t, value: t.name.includes('duration') ? ms(v) : pct(v), passed: running ? lim === undefined || v < lim : t.passed };
  });
  const failedCount = verdict.filter((t) => !t.passed).length;
  const stage = stageAt(sc, run.progress);
  const eps = runEndpoints(run, c);
  // level changes marked on the load charts
  const marks = hasControl(run)
    ? levelState(run).decisions.filter((x) => x.kind !== 'wait').map((x) => ({ ts: x.at, label: x.kind === 'hold' ? `${x.actor === 'ai' ? 'AI' : 'you'}: hold` : x.kind === 'back' ? 'step back' : x.kind === 'advance' ? (x.actor === 'ai' ? 'AI ↑' : x.actor === 'you' ? 'you ↑' : '↑') : x.kind }))
    : undefined;

  return (
    <>
      <PageHeader title={sc.name} crumbs={[{ label: 'Load tests', to: '/load-tests' }, { label: `Run ${run.id}` }]}
        subtitle={<span className="row"><RunState state={run.state} /><span className="mono muted">{sc.target}</span><span className="muted">· {sc.regions.join(', ')} · {duration(sc.duration)} · peak {sc.peakVus} VUs</span></span>}
        actions={running
          ? <Button variant="danger" icon={<Square size={14} />} onClick={() => stopRun(run.id)}>Stop run</Button>
          : <>
              <Button icon={<Pencil size={14} />} onClick={() => nav(`/load-tests/scenarios/${sc.id}/edit`)}>Edit scenario</Button>
              <Button variant="primary" icon={<Play size={14} />} onClick={() => nav(`/load-tests/runs/${startRun(sc.id)}`)}>Run again</Button>
            </>} />

      {/* 0 · who moves the load between levels */}
      {hasControl(run) && <LevelControl run={run} />}

      {/* 1 · verdict */}
      <Card>
        <div className="stack" style={{ gap: 'var(--space-4)' }}>
          {running && !hasControl(run) && (
            <div>
              <div className="row" style={{ justifyContent: 'space-between', marginBottom: 'var(--space-2)' }}>
                <span className="secondary">Stage {stage.index + 1} of {stage.count} · {stage.target === 0 ? 'ramping down' : `ramping to ${stage.target} VUs`}</span>
                <span className="num">{Math.round(run.progress * 100)}% · {duration(sc.duration * (1 - run.progress))} left</span>
              </div>
              <Meter value={run.progress} />
            </div>
          )}
          {run.state === 'aborted' && <div className="secondary">Stopped at {Math.round(run.progress * 100)}% — results cover the part that ran.</div>}
          <div className="row" style={{ gap: 'var(--space-6)', flexWrap: 'wrap', alignItems: 'flex-start' }}>
            <div style={{ minWidth: 180 }}>
              <div className="eyebrow">Thresholds</div>
              <div className="row" style={{ gap: 'var(--space-2)', fontSize: 'var(--text-xl)', fontWeight: 600 }}>
                {running
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
        </div>
      </Card>

      {/* 2 · outcome metrics, then load */}
      <Section title="Load generator" subtitle={`Measured at the load generator, all regions · ${running ? 'live' : `over ${duration(sc.duration * run.progress)}`}`}>
        <div className="grid grid-2">
          <MetricCard label="Response time · p95" value={ms(running ? last(c.p95) : run.p95)}
            hint={`${latencyHint(latencyFromP95(running ? last(c.p95) : run.p95), 'p95')}${p95Limit !== undefined ? ` · threshold ${ms(p95Limit)}` : ''}`}>
            <TimeSeries timestamps={c.ts} height={200} format={ms}
              threshold={p95Limit !== undefined ? { value: p95Limit, label: ms(p95Limit) } : undefined}
              series={latencyFromP95Series(c.p95)} />
          </MetricCard>
          <MetricCard label="Failed requests" value={pct(running ? last(c.err) : run.errorRate)}
            hint={errLimit !== undefined ? `Threshold ${pct(errLimit)}` : undefined}>
            <TimeSeries timestamps={c.ts} height={200} format={(v) => pct(v)}
              threshold={errLimit !== undefined ? { value: errLimit, label: pct(errLimit) } : undefined}
              series={[{ id: 'err', label: 'Failed', slot: 8, data: c.err }]} />
          </MetricCard>
          <MetricCard label="Virtual users" value={int(running ? last(c.vus) : peak(c.vus))} unit={running ? 'now' : 'peak'}
            hint={(spec.levels ?? []).length ? spec.levels!.map((l) => l.target).join(' → ') + ' VUs' : spec.stages.map((s) => `${s.target} VUs / ${duration(s.duration)}`).join(' → ')}>
            <TimeSeries timestamps={c.ts} height={140} events={marks} series={[{ id: 'vu', label: 'VUs', slot: 2, data: c.vus }]} />
          </MetricCard>
          <MetricCard label="Requests / sec" value={compact(running ? last(c.rps) : peak(c.rps))} unit={running ? 'now' : 'peak'}
            hint={`${compact(c.rps.reduce((a, v) => a + v, 0) * (c.step / 1000))} requests sent`}>
            <TimeSeries timestamps={c.ts} height={140} format={(v) => compact(v)} series={[{ id: 'rps', label: 'RPS', slot: 3, data: c.rps }]} />
          </MetricCard>
        </div>
      </Section>

      <Section title="Target services" subtitle="From Trooper SDK, test traffic only">
        <div className="grid grid-2">
          {spec.services.flatMap((sid) => {
            const svc = services.find((s) => s.id === sid);
            const db = dbOf(sid);
            // fewer instances → hotter under the same load
            const heat = Math.min(0.75, 0.25 + 2.4 / (svc?.instances ?? 4));
            const cpu = c.load.map((l, i) => Math.min(0.97, 0.12 + heat * l + ((i * 13 + sid.length) % 7) / 200));
            const dbP95 = db ? c.load.map((l, i) => db.p95 * (1 + 1.4 * l ** 2) * (0.95 + ((i * 11) % 5) / 50)) : [];
            return [
              <MetricCard key={`${sid}-cpu`} label={`${svc?.name ?? sid} CPU`} value={pct(running ? last(cpu) : peak(cpu), 0)} unit={running ? 'now' : 'peak'}
                hint={`Average across ${svc?.instances ?? 1} instances`}>
                <TimeSeries timestamps={c.ts} height={140} format={(v) => pct(v, 0)} yMax={1} series={[{ id: 'cpu', label: 'CPU', slot: 5, data: cpu }]} />
              </MetricCard>,
              db && (
                <MetricCard key={`${sid}-db`} label={`${db.name} query p95`} value={ms(running ? last(dbP95) : peak(dbP95))} unit={running ? 'now' : 'peak'}
                  hint={`called by ${svc?.name ?? sid} · production p95 ${ms(db.p95)}`}>
                  <TimeSeries timestamps={c.ts} height={140} format={ms} series={[{ id: 'db', label: 'DB p95', slot: 6, data: dbP95 }]} />
                </MetricCard>
              ),
            ];
          })}
        </div>
      </Section>

      <Section title="Endpoints in this run" subtitle="Each scenario step · click for its latency and errors over the run">
        <Card flush>
          <Table rows={eps} rowKey={(e) => `${e.serviceId}-${e.id}`} onRowClick={d.open} defaultSort={{ key: 'p95', dir: 'desc' }} empty="This scenario's steps don't match any endpoint." columns={[
            { key: 'ep', header: 'Endpoint', render: (e) => <div><div className="row" style={{ gap: 'var(--space-2)' }}><MethodBadge method={e.method} /><span className="mono">{e.route}</span></div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{e.serviceId}</div></div> },
            { key: 'req', header: 'Requests', numeric: true, sortValue: (e) => e.requests, render: (e) => compact(e.requests) },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (e) => e.runP95, render: (e) => ms(e.runP95) },
            { key: 'prod', header: 'Prod p95', numeric: true, sortValue: (e) => e.p95, render: (e) => <span className="muted">{ms(e.p95)}</span> },
            { key: 'trend', header: 'p95 over run', render: (e) => <Sparkline data={e.p95Series} width={100} height={24} /> },
            { key: 'err', header: 'Errors', numeric: true, sortValue: (e) => e.runErr, render: (e) => pct(e.runErr) },
            { key: 'go', header: '', width: 32, render: () => <ChevronRight size={16} className="muted" aria-hidden /> },
          ]} />
        </Card>
      </Section>

      {d.item && <EndpointRunDrawer ep={d.item} ts={c.ts} run={run} onClose={d.close}
        onOpen={() => nav(`/services/${d.item!.serviceId}/endpoints/${d.item!.id}`)} p95Limit={p95Limit} errLimit={errLimit} />}
    </>
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
      <DrawerSection title="Response time">
        <TimeSeries timestamps={ts} height={160} format={ms}
          threshold={p95Limit !== undefined ? { value: p95Limit, label: ms(p95Limit) } : undefined}
          series={latencyFromP95Series(ep.p95Series)} />
      </DrawerSection>
      <DrawerSection title="Failed requests">
        <TimeSeries timestamps={ts} height={160} format={(v) => pct(v)}
          threshold={errLimit !== undefined ? { value: errLimit, label: pct(errLimit) } : undefined}
          series={[{ id: 'err', label: 'Failed', slot: 8, data: ep.errSeries }]} />
      </DrawerSection>
    </Drawer>
  );
}
