import { useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { AlertTriangle, ArrowRight, MonitorSmartphone, PlayCircle, Target, UserRound } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import {
  Badge, Button, Card, CodeBlock, ConnectSdk, Drawer, DrawerSection, EmptyState, KeyValues, MetricCard, Section, SkeletonText, StatusBadge, Table, Tabs, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  errorReplay, replayUrl, replaysFor, sessionHasReplay, appApiCalls, appErrorDetail, appErrors, appSeries, vitalSeries as vitalSeriesOf, deployEvents, deploysOf, integrations, appSummary, rumPagesFor, sessionsFor, timestamps,
  type AppApiCall, type AppSummary, type RumPage, type Service, type Session, type TimeRange,
} from '@/data';
import { ago, clock, compact, duration, int, ms, pct } from '@/lib/format';
import { ErrorCell } from './apm';
import { LogExplorer } from './Logs';
import { DeployList, IntegrationSourceBadge } from './Integrations';
import { AlertCount, AlertsBody } from './Alerts';
import { KpiCount, ServiceKpis } from './Kpis';
import { ReplaysTab } from './Replays';
import { PagePerformanceDiagnosis } from './PerformanceDiagnosis';

type Tab = 'pages' | 'sessions' | 'replays' | 'errors' | 'api' | 'deploys' | 'kpis' | 'alerts' | 'logs';
const TAB_IDS: Tab[] = ['pages', 'sessions', 'replays', 'errors', 'api', 'deploys', 'kpis', 'alerts', 'logs'];
type Vital = AppSummary['vitals'][number];

// ---------------------------------------------------------------- vitals
const VITAL_DESC: Record<string, string> = {
  lcp: 'Loading · Largest Contentful Paint',
  inp: 'Interactivity · Interaction to Next Paint',
  cls: 'Visual stability · Cumulative Layout Shift',
  ttfb: 'Server response · Time to First Byte',
  cold: 'App launched from scratch until first screen',
  warm: 'App resumed from background until interactive',
  render: 'Share of frames slower than 16 ms',
  net: 'Backend calls made by the app, p75',
};
/** Vitals that also exist per page / screen (the pages endpoint has no TTFB). */
const PAGE_VITALS = ['lcp', 'inp', 'cls'];

function vitalStatus(v: Vital, value = v.value): 'ok' | 'warning' | 'critical' {
  return value <= v.good ? 'ok' : value <= v.poor ? 'warning' : 'critical';
}
const vitalFmt = (v: Vital) => (v.unit === 'ms' ? ms : v.id === 'cls' ? (x: number) => x.toFixed(2) : (x: number) => pct(x, 1));
const STATUS_LABEL = { ok: 'Good', warning: 'Needs improvement', critical: 'Poor' } as const;

function pageVital(p: RumPage, id: string): number {
  return id === 'inp' ? p.inp : id === 'cls' ? p.cls : p.lcp;
}

/** Plain number when good; status badge otherwise (never color-only). */
function VitalCell({ v, value }: { v: Vital; value: number }) {
  const s = vitalStatus(v, value);
  const label = vitalFmt(v)(value);
  return s === 'ok' ? <span className="num">{label}</span> : <StatusBadge status={s} label={label} />;
}

interface JsError {
  id: string;
  message: string;
  location: string;
  page: string;
  events: number;
  users: number;
  firstSeen: number;
  lastSeen: number;
}

// ---------------------------------------------------------------- screen body
/** Body of a web / mobile app's service page (below the page header). */
export function FrontendApp({ app }: { app: Service }) {
  const { range } = useApp();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const initial = params.get('tab');
  const [tab, setTab] = useState<Tab>(TAB_IDS.includes(initial as Tab) ? (initial as Tab) : 'pages');
  const vitalDrawer = useDrawer<Vital>();
  const pageDrawer = useDrawer<RumPage>();
  const sessionDrawer = useDrawer<Session>();
  const errorDrawer = useDrawer<JsError>();
  const ts = timestamps(range);
  const mobile = app.type === 'mobile';
  const pages = rumPagesFor(app.id);
  const sessions = sessionsFor(app.id);
  const summary = appSummary(app.id);
  // grouped RUM errors (`/apps/{id}/errors`)
  const errors: JsError[] = appErrors(app.id).map((e) => ({
    id: e.fingerprint, message: e.type && !e.message.startsWith(e.type) ? `${e.type}: ${e.message}` : e.message,
    location: e.crash ? `crash · ${e.appVersion}` : e.appVersion || '—', page: e.view, events: e.count, users: e.sessions,
    firstSeen: e.firstSeen, lastSeen: e.lastSeen,
  }));
  const appS = appSeries(app.id, range);
  const calls = appApiCalls(app.id);
  const totalViews = pages.reduce((a, p) => a + p.views, 0);
  const pageNoun = mobile ? 'Screen' : 'Page';
  const errLabel = mobile ? 'Crash rate' : 'JS error rate';
  const pageVitals = summary.vitals.filter((v) => PAGE_VITALS.includes(v.id));
  const deploys = deploysOf(app.id);
  const vercel = integrations.find((i) => i.provider === 'vercel' && Object.entries((i.config.projectMap ?? {}) as Record<string, string>).some(([p, svc]) => (svc || p) === app.name || (svc || p) === app.id));

  const vitalSeries = (v: Vital) => vitalSeriesOf(app.id, v.id, range);
  // no RUM data at all yet: a single setup state instead of empty charts
  const noData = totalViews === 0 && sessions.length === 0 && summary.activeUsers === 0 && summary.sessions === 0 && errors.length === 0;

  if (noData) {
    return (
      <Card>
        <ConnectSdk icon={<MonitorSmartphone />} title={`No RUM data from ${app.name} yet`} group="frontend">
          Page views, sessions, Web Vitals, JS errors and the backend calls of this app appear once its {mobile ? 'mobile' : 'browser'} SDK
          reports with the service name <span className="mono">{app.name}</span>.
        </ConnectSdk>
      </Card>
    );
  }

  return (
    <>
      {/* 1 · Primary metrics */}
      <div className="grid grid-3">
        <MetricCard className="metric--hero span-2" label="Active users now" value={int(summary.activeUsers)} curr={summary.activeUsers} prev={summary.prevActiveUsers}
          hint={<>{compact(totalViews)} {mobile ? 'screen' : 'page'} views · avg. session {duration(summary.avgSessionSec)} · {int(summary.sessions)} sessions</>}>
          <TimeSeries timestamps={ts} format={(v) => compact(v)} height={220} events={deployEvents(app.id, ts[0])} series={[
            { id: 'users', label: 'Users', slot: 1, data: appS.users },
            { id: 'sessions', label: 'Sessions', slot: 2, data: appS.sessions },
          ]} />
        </MetricCard>
        <MetricCard label={errLabel} value={pct(summary.errorRate)} curr={summary.errorRate} prev={summary.prevErrorRate} upIsGood={false}
          hint={mobile ? 'Crashed sessions / sessions' : 'Page views with an error / page views'}
          onClick={() => setTab('errors')}>
          <TimeSeries timestamps={ts} format={(v) => pct(v)} height={220} area events={deployEvents(app.id, ts[0])} series={[
            { id: 'err', label: errLabel, slot: 8, data: appS.errorRate },
          ]} />
        </MetricCard>
      </div>

      {/* 2 · Vitals */}
      <Section title={mobile ? 'App performance' : 'Core Web Vitals'} subtitle={`p75 across all ${mobile ? 'screens' : 'pages'} · click for details`}>
        <div className="grid grid-4">
          {summary.vitals.map((v) => {
            const s = vitalStatus(v);
            const fmt = vitalFmt(v);
            return (
              <MetricCard key={v.id} label={v.label} value={fmt(v.value)}
                badge={<StatusBadge status={s} label={STATUS_LABEL[s]} />}
                hint={VITAL_DESC[v.id]} onClick={() => vitalDrawer.open(v)}>
                <TimeSeries timestamps={ts} height={110} format={fmt}
                  threshold={{ value: v.good, label: 'good' }}
                  series={[{ id: v.id, label: `${v.label} p75`, slot: 1, data: vitalSeries(v) }]} />
              </MetricCard>
            );
          })}
        </div>
      </Section>

      {/* 3 · Main lists */}
      <Section>
        <Tabs value={tab} onChange={setTab} tabs={[
          { id: 'pages', label: <>{pageNoun}s <Badge>{pages.length}</Badge></> },
          { id: 'sessions', label: <>Sessions <Badge>{sessions.length}</Badge></> },
          { id: 'replays', label: <>Replays <Badge>{replaysFor(app.id, { range }).length}</Badge></> },
          { id: 'errors', label: <>{mobile ? 'Crashes & errors' : 'Errors'} <Badge>{errors.length}</Badge></> },
          { id: 'api', label: <>API calls <Badge>{calls.length}</Badge></> },
          { id: 'deploys', label: <>Deploys <Badge>{deploys.length}</Badge></> },
          { id: 'kpis', label: <>KPIs <KpiCount serviceId={app.id} /></> },
          { id: 'alerts', label: <>Alerts <AlertCount serviceId={app.id} /></> },
          { id: 'logs', label: 'Logs' },
        ]} />

        {tab === 'pages' && (
          <Card flush>
            <Table<RumPage> rows={pages} rowKey={(p) => p.id} defaultSort={{ key: 'views', dir: 'desc' }} onRowClick={pageDrawer.open}
              empty={<EmptyState icon={<MonitorSmartphone />} title={`No ${pageNoun.toLowerCase()} views in this range`} />} columns={[
              { key: 'path', header: pageNoun, sortValue: (p) => p.path, render: (p) => <span className="mono">{p.path}</span> },
              { key: 'views', header: 'Views', numeric: true, sortValue: (p) => p.views, render: (p) => int(p.views) },
              { key: 'users', header: 'Users', numeric: true, sortValue: (p) => p.users, render: (p) => int(p.users) },
              { key: 'time', header: 'Avg. time', numeric: true, sortValue: (p) => p.avgTime, render: (p) => duration(p.avgTime) },
              ...(mobile
                ? [{ key: 'load', header: 'Load p75', numeric: true, sortValue: (p: RumPage) => p.lcp, render: (p: RumPage) => <span className="num">{ms(p.lcp)}</span> }]
                : pageVitals.map((v) => ({ key: v.id, header: v.label, numeric: true, sortValue: (p: RumPage) => pageVital(p, v.id), render: (p: RumPage) => <VitalCell v={v} value={pageVital(p, v.id)} /> }))),
              { key: 'bounce', header: mobile ? 'Exit' : 'Bounce', numeric: true, sortValue: (p) => p.bounce, render: (p) => pct(p.bounce, 0) },
              { key: 'err', header: 'Errors', numeric: true, sortValue: (p) => p.errorRate, render: (p) => pct(p.errorRate) },
            ]} />
          </Card>
        )}
        {tab === 'sessions' && (
          <Card flush>
            <Table rows={sessions} rowKey={(s) => s.id} defaultSort={{ key: 'start', dir: 'desc' }} onRowClick={sessionDrawer.open}
              empty={<EmptyState icon={<UserRound />} title="No sessions in this range" />} columns={[
              { key: 'user', header: 'User', render: (s) => <span className="row"><UserRound size={14} className="muted" />{s.identified ? s.user : <span className="muted">{s.user}</span>}</span> },
              { key: 'country', header: 'Country', render: (s) => s.country },
              { key: 'device', header: 'Device', render: (s) => <span className="secondary">{s.device}</span> },
              { key: 'flow', header: 'Entry → exit', render: (s) => <span className="mono" style={{ fontSize: 'var(--text-xs)' }}>{s.entry} → {s.exit}</span> },
              { key: 'pages', header: `${pageNoun}s`, numeric: true, sortValue: (s) => s.pages, render: (s) => s.pages },
              { key: 'dur', header: 'Duration', numeric: true, sortValue: (s) => s.duration, render: (s) => duration(s.duration) },
              { key: 'err', header: 'Errors', numeric: true, sortValue: (s) => s.errors, render: (s) => s.errors ? <StatusBadge status="warning" label={String(s.errors)} /> : <span className="muted">0</span> },
              { key: 'start', header: 'Started', numeric: true, sortValue: (s) => s.startedAt, render: (s) => <span className="muted">{ago(s.startedAt)}</span> },
              { key: 'replay', header: '', render: (s) => sessionHasReplay(app.id, s)
                ? <Button size="sm" variant="ghost" icon={<PlayCircle size={14} />} onClick={(e) => { e.stopPropagation(); nav(replayUrl(s.id)); }}>Watch replay</Button>
                : null },
            ]} />
          </Card>
        )}
        {tab === 'errors' && (
          <Card flush>
            <Table rows={errors} rowKey={(e) => e.id} defaultSort={{ key: 'events', dir: 'desc' }} onRowClick={errorDrawer.open}
              empty={<EmptyState icon={<AlertTriangle />} title={mobile ? 'No crashes or errors in this range' : 'No errors in this range'} />} columns={[
              { key: 'msg', header: 'Error', render: (e) => (
                <span className="row" style={{ alignItems: 'flex-start' }}><AlertTriangle size={14} color="var(--status-serious)" aria-hidden style={{ marginTop: 3, flex: 'none' }} />
                  <span style={{ minWidth: 0 }}><div className="mono truncate" style={{ maxWidth: 480 }}>{e.message}</div>
                    <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{e.location} · {e.page}</div></span></span>) },
              { key: 'events', header: 'Events', numeric: true, sortValue: (e) => e.events, render: (e) => int(e.events) },
              { key: 'users', header: 'Users', numeric: true, sortValue: (e) => e.users, render: (e) => int(e.users) },
              { key: 'last', header: 'Last seen', numeric: true, sortValue: (e) => e.lastSeen, render: (e) => <span className="muted">{ago(e.lastSeen)}</span> },
            ]} />
          </Card>
        )}
        {tab === 'replays' && <ReplaysTab app={app} />}
        {tab === 'api' && (
          <Card flush subtitle="Backend endpoints this app calls, identified from the trace context the SDK adds to every request · click to open the endpoint">
            {calls.length === 0
              ? <div className="list"><div className="list__row muted">No backend calls traced from this app yet.</div></div>
              : <Table<AppApiCall> rows={calls} rowKey={(c) => `${c.service.id}-${c.endpoint.id}`} defaultSort={{ key: 'rpm', dir: 'desc' }}
                  onRowClick={(c) => nav(`/services/${c.service.id}/endpoints/${c.endpoint.id}`)} columns={[
                  { key: 'svc', header: 'Service', sortValue: (c) => c.service.name, render: (c) => <span className="row" style={{ gap: 'var(--space-2)' }}>{c.service.name}{c.service.status !== 'ok' && <StatusBadge status={c.service.status} />}</span> },
                  { key: 'ep', header: 'Endpoint', sortValue: (c) => c.endpoint.route, render: (c) => <span style={{ whiteSpace: 'nowrap' }}><span className="mono muted">{c.endpoint.method}</span> {c.endpoint.route}</span> },
                  { key: 'rpm', header: 'rpm', numeric: true, sortValue: (c) => c.rpm, render: (c) => compact(c.rpm) },
                  { key: 'err', header: 'Error rate', numeric: true, sortValue: (c) => c.errorRate, render: (c) => <ErrorCell rate={c.errorRate} /> },
                  { key: 'p95', header: 'p95', numeric: true, sortValue: (c) => c.p95, render: (c) => ms(c.p95) },
                  { key: 'go', header: '', render: () => <ArrowRight size={14} className="muted" aria-hidden /> },
                ]} />}
          </Card>
        )}
        {tab === 'kpis' && <ServiceKpis serviceId={app.id} />}
        {tab === 'alerts' && <AlertsBody serviceId={app.id} />}
        {tab === 'deploys' && (
          <div className="stack">
            {vercel && <div className="row" style={{ gap: 'var(--space-2)', fontSize: 'var(--text-sm)' }}><span className="muted">Deploys, logs and vitals from</span><IntegrationSourceBadge it={vercel} /></div>}
            <DeployList deploys={deploys} />
            {!vercel && deploys.length === 0 && <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>Hosted on Vercel? Connect it under Settings → Integrations to see every deploy here and on the charts above.</p>}
          </div>
        )}
        {tab === 'logs' && <LogExplorer serviceId={app.id} />}
      </Section>


      {/* ---------------- drawers ---------------- */}
      <VitalDrawer vital={vitalDrawer.item} pages={pages} pageNoun={pageNoun} onClose={vitalDrawer.close} ts={ts}
        data={vitalDrawer.item ? vitalSeries(vitalDrawer.item) : []}
        onPage={(p) => { vitalDrawer.close(); pageDrawer.open(p); }} />
      <PageDrawer app={app} page={pageDrawer.item} vitals={pageVitals} errors={errors} onClose={pageDrawer.close} ts={ts} range={range}
        onTrack={(p) => nav(`/kpis/new?app=${app.id}&page=${encodeURIComponent(p.path)}`)} />
      <SessionDrawer appId={app.id} session={sessionDrawer.item} pageNoun={pageNoun} onClose={sessionDrawer.close} onReplay={(id) => nav(replayUrl(id))} />
      <ErrorDrawer appId={app.id} error={errorDrawer.item} onClose={errorDrawer.close} onReplay={(id, off) => nav(replayUrl(id, off))} />
    </>
  );
}

// ---------------------------------------------------------------- drawers
function VitalDrawer({ vital, pages, pageNoun, onClose, ts, data, onPage }: {
  vital: Vital | null; pages: RumPage[]; pageNoun: string; onClose: () => void; ts: number[]; data: number[]; onPage: (p: RumPage) => void;
}) {
  if (!vital) return null;
  const fmt = vitalFmt(vital);
  const s = vitalStatus(vital);
  const perPage = PAGE_VITALS.includes(vital.id);
  const worst = perPage ? [...pages].sort((a, b) => pageVital(b, vital.id) - pageVital(a, vital.id)) : [];
  return (
    <Drawer open onClose={onClose} title={`${vital.label} · ${fmt(vital.value)}`} subtitle={<><StatusBadge status={s} label={STATUS_LABEL[s]} />{VITAL_DESC[vital.id]}</>}>
      <DrawerSection title="p75 over time" aside={`good ≤ ${fmt(vital.good)} · poor > ${fmt(vital.poor)}`}>
        <TimeSeries timestamps={ts} height={220} format={fmt} threshold={{ value: vital.good, label: `good ≤ ${fmt(vital.good)}` }}
          series={[{ id: vital.id, label: `${vital.label} p75`, slot: 1, data }]} />
      </DrawerSection>
      {perPage && (
        <DrawerSection title={`${pageNoun}s, worst first`} aside="click to open">
          <Card flush>
            <Table rows={worst} rowKey={(p) => p.id} onRowClick={onPage} columns={[
              { key: 'p', header: pageNoun, render: (p) => <span className="mono">{p.path}</span> },
              { key: 'v', header: `${vital.label} p75`, numeric: true, render: (p) => <VitalCell v={vital} value={pageVital(p, vital.id)} /> },
              { key: 'views', header: 'Views', numeric: true, render: (p) => compact(p.views) },
            ]} />
          </Card>
        </DrawerSection>
      )}
    </Drawer>
  );
}

function PageDrawer({ app, page, vitals, errors, onClose, ts, range, onTrack }: {
  app: Service; page: RumPage | null; vitals: Vital[]; errors: JsError[]; onClose: () => void; ts: number[]; range: TimeRange; onTrack: (p: RumPage) => void;
}) {
  if (!page) return null;
  const mobile = app.type === 'mobile';
  const pageErrors = errors.filter((e) => e.page === page.path);
  return (
    <Drawer open onClose={onClose} title={<span className="mono">{page.path}</span>}
      subtitle={<>{int(page.views)} views · {int(page.users)} users · avg. {duration(page.avgTime)} · {mobile ? 'exit' : 'bounce'} {pct(page.bounce, 0)}</>}
      actions={<Button size="sm" icon={<Target size={14} />} onClick={() => onTrack(page)}>Track as KPI</Button>}>
      <DrawerSection title="Views over time">
        <TimeSeries timestamps={ts} height={180} format={(v) => compact(v)} area
          series={[{ id: 'views', label: 'Views', slot: 1, data: appSeries(app.id, range, page.path).views }]} />
      </DrawerSection>
      <DrawerSection title={mobile ? 'Performance' : 'Web Vitals'} aside={`p75 on this ${mobile ? 'screen' : 'page'}`}>
        <KeyValues items={mobile
          ? [['Load p75', ms(page.lcp)], ['Error rate', pct(page.errorRate)]]
          : vitals.map((v) => [v.label, <VitalCell key={v.id} v={v} value={pageVital(page, v.id)} />])} />
      </DrawerSection>
      {!mobile && (page.lcp >= 2500 || page.inp >= 200) && <PagePerformanceDiagnosis app={app.name} page={page} />}
      <DrawerSection title="Errors here" aside={`error rate ${pct(page.errorRate)}`}>
        {pageErrors.length === 0
          ? <p className="muted">No errors grouped on this {mobile ? 'screen' : 'page'} in range.</p>
          : pageErrors.map((e) => (
            <div key={e.id} className="row" style={{ alignItems: 'flex-start' }}>
              <AlertTriangle size={14} color="var(--status-serious)" aria-hidden style={{ marginTop: 3, flex: 'none' }} />
              <div style={{ minWidth: 0 }}><div className="mono" style={{ overflowWrap: 'anywhere' }}>{e.message}</div>
                <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{int(e.events)} events · {int(e.users)} users</div></div>
            </div>
          ))}
      </DrawerSection>
      <div className="row">
        <Button size="sm" variant="ghost" onClick={() => onTrack(page)}>Track this {mobile ? 'screen' : 'page'} as a KPI <ArrowRight size={14} /></Button>
      </div>
    </Drawer>
  );
}

function SessionDrawer({ appId, session, pageNoun, onClose, onReplay }: { appId: string; session: Session | null; pageNoun: string; onClose: () => void; onReplay: (sessionId: string) => void }) {
  if (!session) return null;
  return (
    <Drawer open onClose={onClose} title={session.user}
      subtitle={<>{session.identified ? <Badge tone="accent">Identified</Badge> : <Badge>Anonymous</Badge>}{session.pages} {pageNoun.toLowerCase()}s · {duration(session.duration)}</>}
      actions={sessionHasReplay(appId, session) ? <Button size="sm" icon={<PlayCircle size={14} />} onClick={() => onReplay(session.id)}>Watch replay</Button> : undefined}>
      <DrawerSection title="Session">
        <KeyValues items={[
          ['Session id', <span className="mono">{session.id}</span>],
          ['Started', `${clock(session.startedAt)} · ${ago(session.startedAt)}`],
          ['Device', session.device],
          ['Country', session.country],
          [`Entry ${pageNoun.toLowerCase()}`, <span className="mono">{session.entry}</span>],
          [`Exit ${pageNoun.toLowerCase()}`, <span className="mono">{session.exit}</span>],
          ['Errors', session.errors ? <StatusBadge status="warning" label={String(session.errors)} /> : '0'],
          ['Duration', duration(session.duration)],
        ]} />
      </DrawerSection>
    </Drawer>
  );
}

function ErrorDrawer({ appId, error, onClose, onReplay }: {
  appId: string; error: JsError | null; onClose: () => void; onReplay: (sessionId: string, offsetMs: number) => void;
}) {
  if (!error) return null;
  const replay = errorReplay(appId, error.id, error.message);
  const detail = appErrorDetail(appId, error.id);
  return (
    <Drawer open onClose={onClose} title={<span className="mono">{error.message}</span>} subtitle={<>{error.location} · first seen {ago(error.firstSeen)} · last seen {ago(error.lastSeen)}</>}
      actions={replay ? <Button size="sm" icon={<PlayCircle size={14} />} onClick={() => onReplay(replay.sessionId, Math.max(0, replay.offsetMs - 2000))}>Watch replay</Button> : undefined}>
      <KeyValues items={[
        ['Events', int(error.events)],
        ['Sessions affected', int(error.users)],
        ['First seen', ago(error.firstSeen)],
        ['Last seen', ago(error.lastSeen)],
        ...(error.page ? [['Seen on', <span key="p" className="mono">{error.page}</span>] as [string, ReactNode]] : []),
      ]} />
      <DrawerSection title="Stack trace" aside={detail ? (detail.symbolicated ? 'symbolicated' : 'not symbolicated') : undefined}>
        {detail?.stack ? <CodeBlock>{detail.stack}</CodeBlock> : detail ? <p className="muted">No stack recorded for this error.</p> : <SkeletonText lines={5} label="Loading stack trace" />}
      </DrawerSection>
    </Drawer>
  );
}
