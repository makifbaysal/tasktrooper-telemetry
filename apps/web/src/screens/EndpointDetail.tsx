import { useEffect } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import { AlertTriangle, ArrowRight, Bell, FileCode2, Play, Sparkles } from 'lucide-react';
import { BreakdownBar, Columns, TimeSeries, seriesColor } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CodeBlock, Drawer, DrawerSection, EmptyState, KeyValues, MetricCard, PageHeader, Section, StatusBadge, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  TIME_CATEGORIES, breakdownSeries, callerSeries, endpointBreakdown, endpointCallers, endpointCalls, endpointErrorSeries, endpointTrafficSeries,
  endpointsFor, incidentShape, logs, series, services, suggestions, timestamps, tracesFor,
  type CallerTraffic, type Dependency, type Endpoint, type LogLine, type TimeCategory,
} from '@/data';
import { ago, compact, dateTime, ms, pct } from '@/lib/format';
import { CallerDrawer, DependencyDrawer, ErrorCell, TraceTable, epLabel, epRef, sourceLabel, latencyHint, latencySeries, statusSplit } from './apm';
import { AlertCount, AlertsBody, OpenIncidents } from './Alerts';
import { CALLER_KIND_LABEL, CriticalityBadge, DEP_KIND_LABEL, callerColorFor, categoryOf, traceUrl } from './shared';
import { RootCauseCard, diagnose } from './rca';

const catColor = (k: TimeCategory) => seriesColor(TIME_CATEGORIES.find((c) => c.id === k)!.slot);

interface Trace { id: string; dur: number; status: number; ts: number }

export function EndpointDetail() {
  const { serviceId = '', endpointId = '' } = useParams();
  const nav = useNavigate();
  const { range } = useApp();
  const [params] = useSearchParams();
  const depDrawer = useDrawer<Dependency>();
  const callerDrawer = useDrawer<CallerTraffic>();
  const errDrawer = useDrawer<LogLine>();
  useEffect(() => {
    if (params.get('tab') === 'time') document.getElementById('time')?.scrollIntoView({ behavior: 'smooth' });
  }, [params]);
  const svc = services.find((s) => s.id === serviceId);
  const ep = endpointsFor(serviceId).find((e) => e.id === endpointId);
  if (!svc || !ep) return <EmptyState icon={<Bell />} title="Not found" />;

  const ts = timestamps(range);
  const hot = ep.errorRate > 0.01;
  const shape = incidentShape(serviceId, 0.25);
  const spike = shape.stepAt ? shape : hot ? { spikeAt: 0.8, spikeMul: 2.8, spikeWidth: 6 } : {};
  const key = `${serviceId}-${endpointId}`;
  const suggestion = suggestions.find((s) => s.target.endsWith(` · ${epRef(ep)}`) && s.target.startsWith(svc.name));
  const traffic = endpointTrafficSeries(ep, range);
  const err = endpointErrorSeries(ep, range);
  const callers = endpointCallers(ep);
  const callerColor = callerColorFor(callers.map((c) => c.caller.id));
  const calls = endpointCalls(ep);
  const breakdown = endpointBreakdown(ep);
  const perCat = breakdownSeries(key, breakdown, range, hot);
  const lat = { p50: ep.p50, avg: breakdown.avgMs, p95: ep.p95, p99: ep.p99 };
  const sub = ep.sub;
  const ref = epRef(ep);
  const unit = sub ? 'msg/min' : 'rpm';
  const unitOf = sub ? 'message' : 'request';
  const sharedWith = sub ? endpointsFor(serviceId).filter((x) => x.id !== ep.id && x.sub?.topic === sub.topic) : [];
  const timeRows = [
    { id: 'app', label: 'Own code', sub: 'App code', dep: null as Dependency | null, ms: breakdown.parts.app, color: catColor('app') },
    ...calls.map((c) => ({ id: c.dep.id, label: c.dep.name, sub: `${DEP_KIND_LABEL[c.dep.kind]} · ${c.callsPerReq}× ${ms(c.dep.avgMs)}`, dep: c.dep as Dependency | null, ms: c.msPerReq, color: catColor(c.dep.kind) })),
  ].sort((a, b) => b.ms - a.ms);
  const maxRow = Math.max(...timeRows.map((r) => r.ms));
  const errors = logs.filter((l) => l.level === 'error' && l.service === svc.name).slice(0, 4);
  const traces: Trace[] = tracesFor(serviceId, ep, range, { limit: 6 });

  return (
    <>
      <PageHeader title={sub ? epLabel(ep) : <span><span className="mono muted">{ep.method}</span> {ep.route}</span>}
        crumbs={[{ label: categoryOf(svc.type).label, to: `/services?type=${categoryOf(svc.type).id}` }, { label: svc.name, to: `/services/${svc.id}` }, { label: epLabel(ep) }]}
        subtitle={<span className="row"><CriticalityBadge c={ep.criticality} />{ep.monitored ? <Badge tone="good" icon={<Bell />}>Monitored</Badge> : <Badge>No monitor</Badge>}{sub && <span className="row" style={{ gap: 6 }}><span className="mono muted">{sourceLabel(ep)}</span><span className="mono">{sub.topic}</span>{sub.group && <span className="muted">· group {sub.group}</span>}{sub.schedule && <span className="muted">· {sub.schedule}</span>}</span>}<span className="row muted" style={{ gap: 4 }}><FileCode2 size={14} />{sub ? `src/consumers/${sub.process}.ts:18` : 'src/orders/orders.controller.ts:42'}</span></span>}
        actions={<>
          <Button icon={<Play size={14} />} onClick={() => nav('/load-tests')}>Load test</Button>
        </>} />

      <OpenIncidents serviceId={svc.id} endpointRef={ref} />
      {diagnose(svc.id, ep.id).map((d) => <RootCauseCard key={d.metric} d={d} scope={epLabel(ep)} />)}

      {suggestion && !ep.monitored && (
        <Callout icon={<Sparkles size={16} />}>
          <strong>Suggested from code:</strong> {suggestion.condition}. <span className="secondary">{suggestion.rationale}</span>{' '}
          <a href="?atab=suggested#alerts" onClick={(e) => { e.preventDefault(); nav('?atab=suggested', { replace: true }); document.getElementById('alerts')?.scrollIntoView({ behavior: 'smooth' }); }}>Review →</a>
        </Callout>
      )}

      {/* Golden signals — each once. Status columns replace a separate status-code chart. */}
      <div className="grid grid-3">
        <MetricCard className="span-2 metric--hero" label={sub ? 'Messages processed' : 'Requests'} value={compact(ep.rpm)} unit={unit} curr={ep.rpm} prev={ep.rpm * 0.94}
          hint={<>{compact(ep.rpm * ep.errorRate)} failed / min{sub && sub.source !== 'cron' && <> · lag <strong className="secondary">{compact(sub.lag)}</strong> msgs · {compact(sub.retriesPerMin)} retries / {compact(sub.dlqPerMin)} DLQ per min</>}</>}>
          <Columns timestamps={ts} height={300} format={(v) => compact(v)} series={statusSplit(traffic, err, !!sub)} />
        </MetricCard>
        <div className="stack">
          <MetricCard label="Error rate" value={pct(ep.errorRate)} curr={ep.errorRate} prev={ep.errorRate * (hot ? 0.3 : 1.05)} upIsGood={false}
            badge={hot && ep.monitored ? <StatusBadge status="firing" label="Alert firing" /> : undefined}>
            <TimeSeries timestamps={ts} height={104} format={(v) => pct(v)} area
              threshold={ep.monitored ? { value: 0.02, label: '2%' } : undefined}
              series={[{ id: 'err', label: 'Error rate', slot: 8, data: err }]} />
          </MetricCard>
          <MetricCard label={sub ? 'Processing time · p95' : 'Latency · p95'} value={ms(ep.p95)} curr={ep.p95} prev={ep.p95 * (hot ? 0.7 : 1.02)} upIsGood={false}
            hint={latencyHint(lat, 'p95')}>
            <TimeSeries timestamps={ts} height={140} format={ms} series={latencySeries(key, range, lat, spike)} />
          </MetricCard>
        </div>
      </div>

      <div id="time" style={{ scrollMarginTop: 'calc(var(--topbar-h) + 16px)' }}>
        <Section title="Where time goes" subtitle={`avg ${ms(breakdown.avgMs)} per ${unitOf} · ${calls.reduce((a, c) => a + c.callsPerReq, 0)} outgoing calls each`}>
          <div className="grid grid-3">
            <Card className="span-2" title="Over time" subtitle={`Avg time per ${unitOf}, by kind of work`}>
              <Columns timestamps={ts} height={340} format={ms}
                series={TIME_CATEGORIES.map((c) => ({ id: c.id, label: c.label, color: seriesColor(c.slot), data: perCat[c.id] }))} />
            </Card>
            <Card title={`Per ${unitOf}, now`} subtitle="Click a call for response time & errors" flush>
              <div className="list">
                {timeRows.map((r) => (
                  <div key={r.id} className={`list__row${r.dep ? ' list__row--clickable' : ''}`} onClick={r.dep ? () => depDrawer.open(r.dep!) : undefined}
                    style={{ flexDirection: 'column', alignItems: 'stretch', gap: 6 }}>
                    <div className="row">
                      <div className="list__main">
                        <span className={r.dep ? 'mono truncate' : 'truncate'}>{r.label}</span>
                        <span className="list__meta" style={{ fontSize: 'var(--text-xs)' }}>{r.sub}</span>
                      </div>
                      {r.dep && r.dep.errorRate >= 0.005 && <ErrorCell rate={r.dep.errorRate} />}
                      <span className="num" style={{ minWidth: 56, textAlign: 'right' }}>{ms(r.ms)}</span>
                    </div>
                    <BreakdownBar max={maxRow} format={ms} segments={[{ id: r.id, label: r.label, value: r.ms, color: r.color }]} />
                  </div>
                ))}
              </div>
            </Card>
          </div>
        </Section>
      </div>

      <Section title={sub ? 'Reads from' : 'Where requests come from'}
        subtitle={sub ? (sharedWith.length ? `also read by ${sharedWith.map((x) => epLabel(x)).join(', ')} (own consumer group each)` : 'only this process reads it') : `${callers.length} caller${callers.length === 1 ? '' : 's'}`}>
        <div className="grid grid-3">
          <Card className="span-2" title={sub ? 'Messages consumed' : 'Requests by caller'} subtitle={unit}>
            <Columns timestamps={ts} height={200} format={(v) => compact(v)}
              series={callers.map((c) => ({ id: c.caller.id, label: c.caller.name, color: callerColor(c.caller.id), data: callerSeries(key, c, range) }))} />
          </Card>
          <Card flush title={sub ? 'Source' : 'Callers'} subtitle="Click for trend">
            <div className="list">
              {callers.map((c) => (
                <div key={c.caller.id} className="list__row list__row--clickable" onClick={() => callerDrawer.open(c)}>
                  <span className="legend__swatch" style={{ background: callerColor(c.caller.id) }} />
                  <div className="list__main">
                    <span className="list__title">{c.caller.name}</span>
                    <span className="list__meta">{c.caller.via ?? CALLER_KIND_LABEL[c.caller.kind]} · {compact(c.rpm)} {unit}</span>
                  </div>
                  <span className="num">{pct(c.share, 1)}</span>
                </div>
              ))}
            </div>
          </Card>
        </div>
      </Section>

      <div id="alerts" />
      <Section title={<span className="row" style={{ gap: 8 }}>{sub ? 'Alerts on this process' : 'Alerts on this endpoint'} <AlertCount serviceId={svc.id} endpointRef={ref} /></span>}
        subtitle={`monitors and KPI burn alerts defined on this ${sub ? 'process' : 'endpoint'}`}>
        <AlertsBody key={ep.id} serviceId={svc.id} endpointRef={ref} />
      </Section>

      <Section title="Errors & traces">
        <div className="grid grid-2">
          <Card title="Top errors" subtitle="Click for stack trace" flush>
            <div className="list">
              {errors.map((l) => (
                <div key={l.id} className="list__row list__row--clickable" onClick={() => errDrawer.open(l)}>
                  <AlertTriangle size={14} color="var(--status-critical)" aria-hidden />
                  <div className="list__main"><span className="list__title mono">{l.message}</span><span className="list__meta">{ago(l.ts)} · trace {l.traceId?.slice(0, 8) ?? '—'}</span></div>
                </div>
              ))}
              {errors.length === 0 && <div className="list__row muted">No errors in range.</div>}
            </div>
          </Card>
          <Card title="Sample traces" subtitle="Click for the span waterfall" flush>
            <TraceTable rows={traces} />
          </Card>
        </div>
      </Section>

      <DependencyDrawer dep={depDrawer.item} serviceRpm={svc.rpm} onClose={depDrawer.close} />
      <CallerDrawer caller={callerDrawer.item} targetKey={key} color={callerDrawer.item ? callerColor(callerDrawer.item.caller.id) : ''} onClose={callerDrawer.close} />
      {errDrawer.item && <ErrorDrawer log={errDrawer.item} ep={ep} onClose={errDrawer.close} onTrace={() => { const id = errDrawer.item?.traceId ?? traces[0]?.id; errDrawer.close(); if (id) nav(traceUrl(id)); }} />}
    </>
  );
}

// ---------------------------------------------------------------- Error drawer
function ErrorDrawer({ log, ep, onClose, onTrace }: { log: LogLine; ep: Endpoint; onClose: () => void; onTrace: () => void }) {
  const { range } = useApp();
  const ts = timestamps(range);
  const same = logs.filter((l) => l.message === log.message).length;
  const occurrences = series(`errocc-${log.message}`, range, { base: Math.max(1, ep.rpm * ep.errorRate * 0.3), noise: 0.4, ...(ep.errorRate > 0.01 ? { spikeAt: 0.8, spikeMul: 3, spikeWidth: 6 } : {}) });
  const fn = ep.route.split('/').filter(Boolean).pop()?.replace(/[^a-z]/gi, '') || 'handler';
  return (
    <Drawer open onClose={onClose} title={<span className="mono">{log.message}</span>}
      subtitle={<><StatusBadge status="critical" label="Error" /><span className="muted">{log.service} · {ep.method} {ep.route}</span></>}
      actions={<Button size="sm" onClick={onTrace}>View trace <ArrowRight size={14} /></Button>}>
      <KeyValues items={[
        ['Last seen', dateTime(log.ts)], ['Trace', <span key="t" className="mono">{log.traceId ?? '—'}</span>],
        ['Same message in range', same], ['Est. per minute now', compact(occurrences[occurrences.length - 1])],
      ]} />
      <DrawerSection title="Occurrences">
        <TimeSeries timestamps={ts} height={150} area format={(v) => compact(v)} series={[{ id: 'n', label: 'Errors / min', slot: 8, data: occurrences }]} />
      </DrawerSection>
      <DrawerSection title="Stack trace">
        <CodeBlock>{`Error: ${log.message}
    at ${fn}Handler (src/${log.service.split('-')[0]}/${fn}.controller.ts:42:17)
    at processTicksAndRejections (node:internal/process/task_queues:95:5)
    at async Router.handle (node_modules/@trooper/node/dist/http.js:118:9)`}</CodeBlock>
      </DrawerSection>
    </Drawer>
  );
}
