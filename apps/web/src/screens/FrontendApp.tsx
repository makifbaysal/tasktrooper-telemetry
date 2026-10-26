import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { AlertTriangle, ArrowRight, Target, UserRound } from 'lucide-react';
import { BarList, PropBar, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Card, CodeBlock, Drawer, DrawerSection, KeyValues, MetricCard, Section, StatusBadge, Table, Tabs, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  NOW, appApiCalls, appErrors, appSeries, isApi, vitalSeries as vitalSeriesOf, deployEvents, deploysOf, integrations, appSummary, rumPagesFor, series, sessionsFor, timestamps,
  type AppApiCall, type AppSummary, type RumPage, type Service, type Session, type TimeRange,
} from '@/data';
import { ago, clock, compact, duration, int, ms, pct } from '@/lib/format';
import { ErrorCell } from './apm';
import { LogExplorer } from './Logs';
import { DeployList, IntegrationSourceBadge } from './Integrations';
import { AlertCount, AlertsBody } from './Alerts';
import { KpiCount, ServiceKpis } from './Kpis';

type Tab = 'pages' | 'sessions' | 'errors' | 'api' | 'deploys' | 'kpis' | 'alerts' | 'logs';
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
/** Vitals that also exist per page / screen. */
const PAGE_VITALS = ['lcp', 'inp', 'cls', 'ttfb'];

function vitalStatus(v: Vital, value = v.value): 'ok' | 'warning' | 'critical' {
  return value <= v.good ? 'ok' : value <= v.poor ? 'warning' : 'critical';
}
const vitalFmt = (v: Vital) => (v.unit === 'ms' ? ms : v.id === 'cls' ? (x: number) => x.toFixed(2) : (x: number) => pct(x, 1));
const STATUS_LABEL = { ok: 'Good', warning: 'Needs improvement', critical: 'Poor' } as const;

function pageVital(p: RumPage, id: string): number {
  if (id === 'ttfb') return Math.round(p.lcp * 0.19);
  return id === 'inp' ? p.inp : id === 'cls' ? p.cls : p.lcp;
}

/** Plain number when good; status badge otherwise (never color-only). */
function VitalCell({ v, value }: { v: Vital; value: number }) {
  const s = vitalStatus(v, value);
  const label = vitalFmt(v)(value);
  return s === 'ok' ? <span className="num">{label}</span> : <StatusBadge status={s} label={label} />;
}

// ---------------------------------------------------------------- local mock helpers (keyed by app)
function hashStr(s: string) {
  let h = 7;
  for (let i = 0; i < s.length; i++) h = (h * 31 + s.charCodeAt(i)) >>> 0;
  return h;
}

const REFERRERS = ['google.com', 'direct', 'instagram.com', 'email · newsletter', 'facebook.com', 'bing.com'];

function pageSources(p: RumPage, pages: RumPage[], mobile: boolean) {
  const h = hashStr(p.id);
  const internal = pages.filter((x) => x.id !== p.id).slice(0, 3).map((x, i) => ({ id: `int-${x.id}`, label: x.path, w: 3 + ((h >> i) % 5) }));
  const external = (mobile ? ['push notification', 'deep link', 'app launch'] : REFERRERS.slice(0, 3)).map((r, i) => ({ id: `ext-${r}`, label: r, w: 2 + ((h >> (i + 3)) % 6) }));
  const all = [...internal, ...external];
  const sum = all.reduce((a, x) => a + x.w, 0);
  return all.map((x) => ({ ...x, share: x.w / sum })).sort((a, b) => b.share - a.share);
}

function pageNext(p: RumPage, pages: RumPage[], mobile: boolean) {
  const h = hashStr(p.path);
  const next = pages.filter((x) => x.id !== p.id).slice(1, 4).map((x, i) => ({ id: x.id, label: x.path, w: 2 + ((h >> (i * 2)) % 6) }));
  const all = [...next, { id: 'exit', label: mobile ? 'Closed the app' : 'Left the site', w: Math.round(p.bounce * 20) + 2 }];
  const sum = all.reduce((a, x) => a + x.w, 0);
  return all.map((x) => ({ ...x, share: x.w / sum })).sort((a, b) => b.share - a.share);
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
  stack: string;
}

const WEB_ERRORS: Omit<JsError, 'id' | 'page' | 'events' | 'users' | 'firstSeen' | 'lastSeen'>[] = [
  { message: "TypeError: Cannot read properties of null (reading 'price')", location: 'Summary.tsx:41', stack: `TypeError: Cannot read properties of null (reading 'price')
    at Summary (src/components/Summary.tsx:41:28)
    at renderWithHooks (react-dom.production.js:1:18345)
    at updateFunctionComponent (react-dom.production.js:1:21233)` },
  { message: 'ChunkLoadError: Loading chunk 812 failed', location: 'bundler runtime', stack: `ChunkLoadError: Loading chunk 812 failed.
    at __webpack_require__.f.j (runtime.js:1:4120)
    at Promise.all (index 0)
    at lazy (src/routes/index.tsx:12:20)` },
  { message: 'Unhandled rejection: request timeout after 15000ms', location: 'api-client.ts:120', stack: `Error: request timeout after 15000ms
    at ApiClient.send (src/lib/api-client.ts:120:13)
    at async load (src/routes/loader.ts:58:5)` },
  { message: 'ResizeObserver loop completed with undelivered notifications', location: '—', stack: 'ResizeObserver loop completed with undelivered notifications.\n    (browser-generated, no stack)' },
];

const MOBILE_ERRORS: typeof WEB_ERRORS = [
  { message: 'Fatal: NullPointerException in ListAdapter.bind', location: 'ListAdapter.kt:88', stack: `java.lang.NullPointerException: Attempt to invoke virtual method on a null object reference
    at app.ui.ListAdapter.bind(ListAdapter.kt:88)
    at androidx.recyclerview.widget.RecyclerView$Adapter.onBindViewHolder(RecyclerView.java:7254)` },
  { message: 'Fatal: EXC_BAD_ACCESS in ImageCache.decode', location: 'ImageCache.swift:212', stack: `Thread 0 Crashed:
0  app  ImageCache.decode(_:) + 212 (ImageCache.swift:212)
1  app  ImageLoader.load(url:) + 88 (ImageLoader.swift:61)` },
  { message: 'Unhandled promise rejection: Network request failed', location: 'api.ts:44', stack: `TypeError: Network request failed
    at XMLHttpRequest.xhr.onerror (whatwg-fetch.js:612:18)
    at api.request (src/lib/api.ts:44:9)` },
];

function errorsFor(app: Service, pages: RumPage[]): JsError[] {
  const tpl = app.type === 'mobile' ? MOBILE_ERRORS : WEB_ERRORS;
  const scale = Math.max(1, app.rpm * app.errorRate * 20);
  return tpl.map((t, i) => {
    const h = hashStr(app.id + i);
    const events = Math.round(scale * (1.4 - i * 0.25) * (0.7 + (h % 60) / 100));
    return {
      ...t, id: `${app.id}-e${i}`, page: pages[h % pages.length].path,
      events, users: Math.max(1, Math.round(events * (0.5 + (h % 30) / 100))),
      firstSeen: NOW - ((h % 20) + 1) * 3600_000, lastSeen: NOW - ((h % 30) + 1) * 60_000,
    };
  });
}

function sessionTimeline(s: Session, pages: RumPage[], errors: JsError[]) {
  const h = hashStr(s.id);
  const steps = Math.min(s.pages, 8);
  let t = s.startedAt;
  const per = s.duration / steps;
  return Array.from({ length: steps }, (_, i) => {
    const page = i === 0 ? s.entry : i === steps - 1 ? s.exit : pages[(h >> i) % pages.length].path;
    const dur = Math.max(4, Math.round(per * (0.5 + (((h >> (i + 2)) & 7) / 7))));
    const at = t;
    t += dur * 1000;
    const error = s.errors > 0 && i === h % steps ? errors[h % errors.length].message : null;
    return { i, page, at, dur, error };
  });
}

function audience(app: Service) {
  const h = hashStr(app.id);
  const mobile = app.type === 'mobile';
  const devices = mobile
    ? [{ label: 'Android', value: 50 + (h % 15), color: 'var(--series-1)' }, { label: 'iOS', value: 50 - (h % 15), color: 'var(--series-2)' }]
    : [
        { label: 'Mobile', value: 40 + (h % 25), color: 'var(--series-1)' },
        { label: 'Desktop', value: 54 - (h % 25), color: 'var(--series-2)' },
        { label: 'Tablet', value: 6, color: 'var(--series-3)' },
      ];
  const tr = 52 + (h % 15);
  const rest = 100 - tr;
  const countries = [['TR', tr], ['DE', Math.round(rest * 0.35)], ['NL', Math.round(rest * 0.22)], ['GB', Math.round(rest * 0.18)], ['US', Math.round(rest * 0.12)]] as const;
  return { devices, countries };
}

// ---------------------------------------------------------------- screen body
/** Body of a web / mobile app's service page (below the page header). */
export function FrontendApp({ app }: { app: Service }) {
  const { range } = useApp();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const initial = params.get('tab');
  const [tab, setTab] = useState<Tab>(initial === 'logs' || initial === 'alerts' || initial === 'kpis' || initial === 'deploys' ? initial : 'pages');
  const vitalDrawer = useDrawer<Vital>();
  const pageDrawer = useDrawer<RumPage>();
  const sessionDrawer = useDrawer<Session>();
  const errorDrawer = useDrawer<JsError>();
  const ts = timestamps(range);
  const mobile = app.type === 'mobile';
  const pages = rumPagesFor(app.id);
  const sessions = sessionsFor(app.id);
  const summary = appSummary(app.id);
  // api: grouped RUM errors (`/apps/{id}/errors`); mock: sample errors built here
  const apiErrors = isApi ? appErrors(app.id) : undefined;
  const errors: JsError[] = apiErrors ? apiErrors.map((e) => ({
    id: e.fingerprint, message: e.type && !e.message.startsWith(e.type) ? `${e.type}: ${e.message}` : e.message,
    location: e.crash ? `crash · ${e.appVersion}` : e.appVersion || '—', page: e.view, events: e.count, users: e.sessions,
    firstSeen: e.firstSeen, lastSeen: e.lastSeen, stack: e.message,
  })) : errorsFor(app, pages);
  const appS = appSeries(app.id, range);
  const calls = appApiCalls(app.id);
  const totalViews = pages.reduce((a, p) => a + p.views, 0);
  const pageNoun = mobile ? 'Screen' : 'Page';
  const errLabel = mobile ? 'Crash rate' : 'JS error rate';
  const pageVitals = summary.vitals.filter((v) => PAGE_VITALS.includes(v.id) && v.id !== 'ttfb');
  const { devices, countries } = audience(app);
  const deploys = deploysOf(app.id);
  const vercel = integrations.find((i) => i.provider === 'vercel' && Object.entries((i.config.projectMap ?? {}) as Record<string, string>).some(([p, svc]) => (svc || p) === app.name || (svc || p) === app.id));

  const vitalSeries = (v: Vital) => vitalSeriesOf(app.id, v.id, range);

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
          { id: 'errors', label: <>{mobile ? 'Crashes & errors' : 'Errors'} <Badge>{errors.length}</Badge></> },
          { id: 'api', label: <>API calls <Badge>{calls.length}</Badge></> },
          { id: 'deploys', label: <>Deploys <Badge>{deploys.length}</Badge></> },
          { id: 'kpis', label: <>KPIs <KpiCount serviceId={app.id} /></> },
          { id: 'alerts', label: <>Alerts <AlertCount serviceId={app.id} /></> },
          { id: 'logs', label: 'Logs' },
        ]} />

        {tab === 'pages' && (
          <Card flush>
            <Table<RumPage> rows={pages} rowKey={(p) => p.id} defaultSort={{ key: 'views', dir: 'desc' }} onRowClick={pageDrawer.open} columns={[
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
            <Table rows={sessions} rowKey={(s) => s.id} defaultSort={{ key: 'start', dir: 'desc' }} onRowClick={sessionDrawer.open} columns={[
              { key: 'user', header: 'User', render: (s) => <span className="row"><UserRound size={14} className="muted" />{s.identified ? s.user : <span className="muted">{s.user}</span>}</span> },
              { key: 'country', header: 'Country', render: (s) => s.country },
              { key: 'device', header: 'Device', render: (s) => <span className="secondary">{s.device}</span> },
              { key: 'flow', header: 'Entry → exit', render: (s) => <span className="mono" style={{ fontSize: 'var(--text-xs)' }}>{s.entry} → {s.exit}</span> },
              { key: 'pages', header: `${pageNoun}s`, numeric: true, sortValue: (s) => s.pages, render: (s) => s.pages },
              { key: 'dur', header: 'Duration', numeric: true, sortValue: (s) => s.duration, render: (s) => duration(s.duration) },
              { key: 'err', header: 'Errors', numeric: true, sortValue: (s) => s.errors, render: (s) => s.errors ? <StatusBadge status="warning" label={String(s.errors)} /> : <span className="muted">0</span> },
              { key: 'start', header: 'Started', numeric: true, sortValue: (s) => s.startedAt, render: (s) => <span className="muted">{ago(s.startedAt)}</span> },
            ]} />
          </Card>
        )}
        {tab === 'errors' && (
          <Card flush>
            <Table rows={errors} rowKey={(e) => e.id} defaultSort={{ key: 'events', dir: 'desc' }} onRowClick={errorDrawer.open} columns={[
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

      {/* 4 · Secondary */}
      <Section title="Audience" subtitle="Share of sessions">
        <div className="grid grid-2">
          <Card title={mobile ? 'Platforms' : 'Devices'}>
            <PropBar segments={devices} />
          </Card>
          <Card title="Countries">
            <BarList items={countries.map(([c, v]) => ({ id: c, label: c, value: v, display: `${v}%` }))} />
          </Card>
        </div>
      </Section>

      {/* ---------------- drawers ---------------- */}
      <VitalDrawer vital={vitalDrawer.item} pages={pages} pageNoun={pageNoun} onClose={vitalDrawer.close} ts={ts}
        data={vitalDrawer.item ? vitalSeries(vitalDrawer.item) : []}
        onPage={(p) => { vitalDrawer.close(); pageDrawer.open(p); }} />
      <PageDrawer app={app} page={pageDrawer.item} pages={pages} vitals={pageVitals} errors={errors} onClose={pageDrawer.close} ts={ts} range={range}
        onTrack={(p) => nav(`/kpis/new?app=${app.id}&page=${encodeURIComponent(p.path)}`)} />
      <SessionDrawer session={sessionDrawer.item} pages={pages} errors={errors} pageNoun={pageNoun} onClose={sessionDrawer.close} />
      <ErrorDrawer error={errorDrawer.item} pages={pages} onClose={errorDrawer.close} ts={ts} range={range} />
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

function PageDrawer({ app, page, pages, vitals, errors, onClose, ts, range, onTrack }: {
  app: Service; page: RumPage | null; pages: RumPage[]; vitals: Vital[]; errors: JsError[]; onClose: () => void; ts: number[]; range: TimeRange; onTrack: (p: RumPage) => void;
}) {
  if (!page) return null;
  const mobile = app.type === 'mobile';
  const sources = pageSources(page, pages, mobile);
  const next = pageNext(page, pages, mobile);
  const pageErrors = errors.filter((e) => e.page === page.path);
  return (
    <Drawer open onClose={onClose} title={<span className="mono">{page.path}</span>}
      subtitle={<>{int(page.views)} views · {int(page.users)} users · avg. {duration(page.avgTime)} · {mobile ? 'exit' : 'bounce'} {pct(page.bounce, 0)}</>}
      actions={<Button size="sm" icon={<Target size={14} />} onClick={() => onTrack(page)}>Track as KPI</Button>}>
      <DrawerSection title="Views over time">
        <TimeSeries timestamps={ts} height={180} format={(v) => compact(v)} area
          series={[{ id: 'views', label: 'Views', slot: 1, data: isApi ? appSeries(app.id, range, page.path).views : series(`page-views-${app.id}-${page.id}`, range, { base: page.views / 60, daily: 0.4 }) }]} />
      </DrawerSection>
      <DrawerSection title={mobile ? 'Performance' : 'Web Vitals'} aside={`p75 on this ${mobile ? 'screen' : 'page'}`}>
        <KeyValues items={mobile
          ? [['Load p75', ms(page.lcp)], ['Error rate', pct(page.errorRate)]]
          : vitals.map((v) => [v.label, <VitalCell key={v.id} v={v} value={pageVital(page, v.id)} />])} />
      </DrawerSection>
      <div className="grid grid-2" style={{ gap: 'var(--space-5)' }}>
        <DrawerSection title="Visitors came from">
          <BarList items={sources.map((s) => ({ id: s.id, label: <span className={s.id.startsWith('int') ? 'mono' : undefined}>{s.label}</span>, value: s.share, display: pct(s.share, 0) }))} />
        </DrawerSection>
        <DrawerSection title="Went next">
          <BarList items={next.map((s) => ({ id: s.id, label: <span className={s.id === 'exit' ? 'muted' : 'mono'}>{s.label}</span>, value: s.share, display: pct(s.share, 0) }))} />
        </DrawerSection>
      </div>
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

function SessionDrawer({ session, pages, errors, pageNoun, onClose }: { session: Session | null; pages: RumPage[]; errors: JsError[]; pageNoun: string; onClose: () => void }) {
  if (!session) return null;
  const steps = sessionTimeline(session, pages, errors);
  return (
    <Drawer open onClose={onClose} title={session.user}
      subtitle={<>{session.identified ? <Badge tone="accent">Identified</Badge> : <Badge>Anonymous</Badge>}{session.pages} {pageNoun.toLowerCase()}s · {duration(session.duration)}</>}>
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
      <DrawerSection title="Timeline" aside={steps.length < session.pages ? `first ${steps.length} of ${session.pages}` : undefined}>
        <Card flush>
          <div className="list">
            {steps.map((s) => (
              <div key={s.i} className="list__row" style={{ alignItems: 'flex-start' }}>
                <span className="num muted" style={{ width: 44, flex: 'none' }}>{clock(s.at)}</span>
                <div className="list__main">
                  <span className="list__title mono">{s.page}</span>
                  {s.error && <span className="row" style={{ gap: 4, color: 'var(--status-serious-text)', fontSize: 'var(--text-xs)' }}><AlertTriangle size={12} aria-hidden /><span className="truncate">{s.error}</span></span>}
                </div>
                <span className="num secondary">{duration(s.dur)}</span>
              </div>
            ))}
          </div>
        </Card>
      </DrawerSection>
    </Drawer>
  );
}

function ErrorDrawer({ error, pages, onClose, ts, range }: { error: JsError | null; pages: RumPage[]; onClose: () => void; ts: number[]; range: TimeRange }) {
  if (!error) return null;
  const affected = pages.filter((p) => p.path === error.page || hashStr(error.id + p.id) % 4 === 0).slice(0, 4);
  return (
    <Drawer open onClose={onClose} title={<span className="mono">{error.message}</span>} subtitle={<>{error.location} · first seen {ago(error.firstSeen)} · last seen {ago(error.lastSeen)}</>}>
      <KeyValues items={[
        ['Events', int(error.events)],
        ['Users affected', int(error.users)],
        ['First seen', ago(error.firstSeen)],
        ['Last seen', ago(error.lastSeen)],
      ]} />
      <DrawerSection title="Occurrences">
        <TimeSeries timestamps={ts} height={160} format={(v) => compact(v)} area
          series={[{ id: 'occ', label: 'Events', slot: 8, data: series(`jserr-${error.id}`, range, { base: Math.max(0.2, error.events / 60), noise: 0.4 }) }]} />
      </DrawerSection>
      <DrawerSection title="Stack trace" aside="symbolicated">
        <CodeBlock>{error.stack}</CodeBlock>
      </DrawerSection>
      <DrawerSection title="Affected">
        <div className="stack" style={{ gap: 6 }}>
          {affected.map((p) => <span key={p.id} className="mono">{p.path}</span>)}
        </div>
      </DrawerSection>
    </Drawer>
  );
}
