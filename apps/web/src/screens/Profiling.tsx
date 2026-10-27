/**
 * Continuous profiling UI (ADR-022, contracts §24): service Profiling tab, endpoint "Where CPU goes", setup empty state.
 */
import { useMemo, useRef, useState } from 'react';
import { Link, useSearchParams } from 'react-router';
import { ArrowRight, Cpu, Flame, Info, Loader2 } from 'lucide-react';
import { FlameDiffLegend, FlameGraph, TimeSeries, framePackage, signedPp } from '@/components/charts';
import { Callout, Card, CopyCode, EmptyState, Section, Segmented, Select, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  NOW, PROFILE_TYPES, RANGE_POINTS, deployEvents, diffChanges, flameGraph, formatProfileValue, isGaugeType, profileDiff, profileSeries,
  profileTypes, seriesFormat, timestamps, topFunctions,
  type DiffChange, type Endpoint, type FlameGraphData, type ProfileComparison, type ProfileFilter, type ProfileType, type ProfileTypeInfo, type Service, type TimeRange, type TopFunction,
} from '@/data';
import { LINE_SLOTS } from './shared';

const RANGE_LABEL: Record<TimeRange, string> = { '15m': '15 minutes', '30m': '30 minutes', '1h': 'hour', '6h': '6 hours', '24h': '24 hours', '7d': '7 days' };
const rangeMs = (r: TimeRange) => RANGE_POINTS[r].points * RANGE_POINTS[r].stepMs;
const pct = (v: number, of: number) => (of ? `${((v / of) * 100).toFixed(v / of >= 0.1 ? 1 : 2)}%` : '—');

/** Value formatter for a flame graph: gauge types are shown per snapshot (the api sums them over profiles). */
function valueFormat(d: Pick<FlameGraphData, 'type' | 'unit' | 'profiles'>) {
  const div = isGaugeType(d.type) ? Math.max(1, d.profiles) : 1;
  return (v: number) => formatProfileValue(d.unit, v / div);
}

function Loading({ label = 'Loading profiles…' }: { label?: string }) {
  return <div className="row muted" style={{ gap: 'var(--space-2)', padding: 'var(--space-4)' }}><Loader2 size={14} className="spin" aria-hidden />{label}</div>;
}

// ---------------------------------------------------------------- setup
const RUNTIME_NOTE: Record<string, string> = {
  Go: 'Go SDK: runtime/pprof, with trooper.route / trooper.method labels per request.',
  'Node.js': 'Node SDK: node:inspector CPU profiler and sampling heap profiler.',
  Java: 'JVM agent: JFR execution, allocation and monitor samples.',
  Kotlin: 'JVM agent: JFR execution, allocation and monitor samples.',
};

/** Shown when a service sends no profiles. */
export function ProfilingSetup({ svc, compact }: { svc: Service; compact?: boolean }) {
  const supported = svc.language in RUNTIME_NOTE;
  const env = 'TROOPER_PROFILING=true\n# optional\nTROOPER_PROFILING_TYPES=cpu,alloc_space,inuse_space' + (svc.language === 'Go' ? ',goroutines' : '') + '\nTROOPER_PROFILING_PERIOD=60        # seconds between profiles\nTROOPER_PROFILING_CPU_DURATION=10  # seconds of CPU sampling per period';
  if (compact) {
    return (
      <Callout icon={<Flame size={16} />}>
        <strong>No profiles from {svc.name}.</strong>{' '}
        {supported ? <>Set <span className="mono">TROOPER_PROFILING=true</span> on the service to see which functions burn CPU on this endpoint. </> : <>Profiling isn’t available for {svc.language} yet. </>}
        <Link to={`/services/${svc.id}?tab=profiling`}>Setup</Link>
      </Callout>
    );
  }
  return (
    <Card>
      <EmptyState icon={<Flame />} title={supported ? `No profiles from ${svc.name} yet` : `Profiling isn’t available for ${svc.language} yet`}>
        {supported
          ? 'Continuous profiling shows which functions use CPU and memory, per endpoint and per version. Overhead stays under 2% CPU.'
          : 'The Go, JVM and Node.js SDKs send profiles today; .NET and Python are next.'}
      </EmptyState>
      {supported && (
        <div className="stack" style={{ maxWidth: 640, margin: '0 auto', gap: 'var(--space-3)' }}>
          <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>Enable it with environment variables on the service, then redeploy. {RUNTIME_NOTE[svc.language]}</p>
          <CopyCode>{env}</CopyCode>
          <p className="muted" style={{ margin: 0, fontSize: 'var(--text-xs)' }}>Profiles appear about a minute after the first period. Retention 14 days.</p>
        </div>
      )}
    </Card>
  );
}

// ---------------------------------------------------------------- service tab
type Mode = 'single' | 'compare';
type BaseWindow = 'prev' | 'day' | 'week';

export function ProfilingTab({ svc }: { svc: Service }) {
  const { range } = useApp();
  const [params] = useSearchParams();
  const types = profileTypes(svc.id, range);
  const [type, setType] = useState<ProfileType | null>(null);
  const [host, setHost] = useState('');
  const [version, setVersion] = useState('');
  const [mode, setMode] = useState<Mode>(params.get('compare') ? 'compare' : 'single');
  const [search, setSearch] = useState('');
  const flameRef = useRef<HTMLDivElement>(null);

  if (types.loading) return <Card><Loading /></Card>;
  if (types.error) return <Card><Callout icon={<Info size={16} />}>Could not load profiles: {types.error}</Callout></Card>;
  const list = types.data ?? [];
  if (!list.length) return <ProfilingSetup svc={svc} />;
  const info = list.find((t) => t.type === type) ?? list.find((t) => t.type === 'cpu') ?? list[0];
  const filter: ProfileFilter = { host: host || undefined, version: version || undefined };
  const typeOptions = PROFILE_TYPES.filter((p) => list.some((t) => t.type === p.id)).map((p) => ({ id: p.id, label: p.label }));

  return (
    <div className="stack" style={{ gap: 'var(--space-4)' }}>
      <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
        <Segmented label="Profile type" value={info.type} onChange={(t) => { setType(t); setSearch(''); }} options={typeOptions} />
        <Select aria-label="Host" value={host} onChange={(e) => setHost(e.target.value)}
          options={[{ value: '', label: `All hosts (${info.hosts.length})` }, ...info.hosts.map((h) => ({ value: h, label: h }))]} />
        {mode === 'single' && (
          <Select aria-label="Version" value={version} onChange={(e) => setVersion(e.target.value)}
            options={[{ value: '', label: 'All versions' }, ...info.versions.map((v) => ({ value: v, label: v }))]} />
        )}
        <span className="spacer" />
        <Segmented label="Mode" value={mode} onChange={setMode} options={[{ id: 'single', label: 'Profile' }, { id: 'compare', label: 'Compare' }]} />
      </div>

      {mode === 'single' ? (
        <>
          <UsageChart svc={svc} type={info.type} range={range} filter={filter} />
          <div ref={flameRef} style={{ scrollMarginTop: 'calc(var(--topbar-h) + var(--space-4))' }}>
            <FlameCard svc={svc} info={info} range={range} filter={filter} search={search} onSearch={setSearch} />
          </div>
          <TopFunctionsCard svc={svc} type={info.type} range={range} filter={filter}
            onPick={(f) => { setSearch(f.name); flameRef.current?.scrollIntoView({ behavior: 'smooth' }); }} />
        </>
      ) : (
        <CompareView svc={svc} info={info} range={range} host={host} initial={params} />
      )}
    </div>
  );
}

function UsageChart({ svc, type, range, filter }: { svc: Service; type: ProfileType; range: TimeRange; filter: ProfileFilter }) {
  const s = profileSeries(svc.id, type, range, filter);
  const ts = timestamps(range);
  const fmt = seriesFormat(type);
  const title = type === 'cpu' ? 'CPU usage' : type === 'alloc_space' ? 'Allocation rate' : type === 'inuse_space' ? 'Heap in use' : type === 'goroutines' ? 'Goroutines' : PROFILE_TYPES.find((p) => p.id === type)?.label ?? type;
  const sub = type === 'cpu' || type === 'wall' ? 'cores per host, from profiles' : 'per host, from profiles';
  return (
    <Card title={title} subtitle={sub}>
      {s.loading || !s.data
        ? <Loading />
        : <TimeSeries timestamps={s.data.timestamps.length ? s.data.timestamps : ts} height={180} format={fmt} events={deployEvents(svc.id, ts[0])}
            series={s.data.series.map((x, i) => ({ id: x.id, label: x.label, data: x.data, slot: LINE_SLOTS[i % LINE_SLOTS.length] }))} />}
    </Card>
  );
}

function FlameCard({ svc, info, range, filter, search, onSearch, title = 'Flame graph' }: {
  svc: Service; info: ProfileTypeInfo; range: TimeRange; filter: ProfileFilter; search?: string; onSearch?: (q: string) => void; title?: string;
}) {
  const fg = flameGraph(svc.id, info.type, range, filter);
  const d = fg.data;
  const label = PROFILE_TYPES.find((p) => p.id === info.type);
  return (
    <Card title={title} subtitle={d ? `${label?.hint ?? ''} · ${valueFormat(d)(d.total)}${isGaugeType(d.type) ? ' per snapshot' : ''} over the last ${RANGE_LABEL[range]} · click a frame to zoom` : label?.hint}>
      {fg.loading ? <Loading /> : !d || d.total === 0
        ? <p className="muted" style={{ margin: 0 }}>No profiles match these filters in the selected range.</p>
        : <FlameGraph root={d.root} total={d.total} format={valueFormat(d)} search={search} onSearchChange={onSearch}
            meta={`${d.profiles.toLocaleString('en-US')} profiles`} />}
    </Card>
  );
}

function TopFunctionsCard({ svc, type, range, filter, onPick }: { svc: Service; type: ProfileType; range: TimeRange; filter: ProfileFilter; onPick: (f: TopFunction) => void }) {
  const top = topFunctions(svc.id, type, range, filter);
  const fg = flameGraph(svc.id, type, range, filter).data;
  const d = top.data;
  const fmt = fg ? valueFormat(fg) : (v: number) => String(v);
  return (
    <Card title="Top functions" subtitle="Self = time in the function itself · Total = including what it calls · click to highlight in the flame graph" flush>
      {top.loading || !d ? <Loading /> : (
        <Table<TopFunction> rows={d.functions.slice(0, 25)} rowKey={(f) => f.name} defaultSort={{ key: 'self', dir: 'desc' }} onRowClick={onPick}
          empty={<span className="muted">No functions in range.</span>} columns={[
            { key: 'name', header: 'Function', sortValue: (f) => f.name, render: (f) => <FunctionCell name={f.name} /> },
            { key: 'self', header: 'Self', numeric: true, sortValue: (f) => f.self, render: (f) => fmt(f.self) },
            { key: 'selfp', header: 'Self %', numeric: true, sortValue: (f) => f.self, render: (f) => pct(f.self, d.total) },
            { key: 'total', header: 'Total', numeric: true, sortValue: (f) => f.total, render: (f) => fmt(f.total) },
            { key: 'totalp', header: 'Total %', numeric: true, sortValue: (f) => f.total, render: (f) => pct(f.total, d.total) },
          ]} />
      )}
    </Card>
  );
}

function FunctionCell({ name }: { name: string }) {
  const pkg = framePackage(name);
  const short = pkg && name.startsWith(pkg) ? name.slice(pkg.length).replace(/^\./, '') : name;
  return (
    <div style={{ minWidth: 0, maxWidth: 560 }}>
      <div className="mono truncate" title={name}>{short}</div>
      {pkg && <div className="muted mono truncate" style={{ fontSize: 'var(--text-xs)' }}>{pkg}</div>}
    </div>
  );
}

// ---------------------------------------------------------------- compare
function CompareView({ svc, info, range, host, initial }: { svc: Service; info: ProfileTypeInfo; range: TimeRange; host: string; initial: URLSearchParams }) {
  const versions = info.versions;
  const [kind, setKind] = useState<'version' | 'window'>(initial.get('compare') === 'window' || versions.length < 2 ? 'window' : 'version');
  const [target, setTarget] = useState(initial.get('target') && versions.includes(initial.get('target')!) ? initial.get('target')! : versions[0] ?? '');
  const [base, setBase] = useState(initial.get('base') && versions.includes(initial.get('base')!) ? initial.get('base')! : versions[1] ?? versions[0] ?? '');
  const [win, setWin] = useState<BaseWindow>('day');
  const span = rangeMs(range);
  const cmp: ProfileComparison = useMemo(() => {
    if (kind === 'version') return { kind, baseVersion: base, version: target };
    const shift = win === 'prev' ? span : win === 'day' ? 86_400_000 : 7 * 86_400_000;
    return { kind, from: NOW - span, to: NOW, baseFrom: NOW - span - shift, baseTo: NOW - shift };
  }, [kind, base, target, win, span]);
  const diff = profileDiff(svc.id, info.type, cmp, { host: host || undefined }, range);
  const d = diff.data;
  const changes = useMemo(() => (d ? diffChanges(d) : []), [d]);
  const fmt = (v: number) => formatProfileValue(info.unit, v);
  const label = PROFILE_TYPES.find((p) => p.id === info.type)?.label ?? info.type;

  return (
    <>
      <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
        <Segmented label="Compare by" value={kind} onChange={setKind} options={[{ id: 'version', label: 'Versions' }, { id: 'window', label: 'Time windows' }]} />
        {kind === 'version' ? (
          <>
            <Select aria-label="Base version" value={base} onChange={(e) => setBase(e.target.value)} options={versions.map((v) => ({ value: v, label: `Base · ${v}` }))} />
            <ArrowRight size={14} className="muted" aria-hidden />
            <Select aria-label="Target version" value={target} onChange={(e) => setTarget(e.target.value)} options={versions.map((v) => ({ value: v, label: `Target · ${v}` }))} />
          </>
        ) : (
          <>
            <Select aria-label="Base window" value={win} onChange={(e) => setWin(e.target.value as BaseWindow)} options={[
              { value: 'prev', label: `Base · previous ${RANGE_LABEL[range]}` },
              { value: 'day', label: 'Base · same time yesterday' },
              { value: 'week', label: 'Base · same time last week' },
            ]} />
            <ArrowRight size={14} className="muted" aria-hidden />
            <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>Target · last {RANGE_LABEL[range]}</span>
          </>
        )}
      </div>

      {kind === 'version' && base === target && <Callout icon={<Info size={16} />}>Pick two different versions to compare.</Callout>}

      <Card title={`${label} diff`} subtitle="Color = change in each frame’s share of the total (not absolute value) · click a frame to zoom">
        {diff.loading || !d ? <Loading label="Comparing profiles…" /> : d.total === 0 || d.baseTotal === 0
          ? <p className="muted" style={{ margin: 0 }}>{d.baseTotal === 0 ? 'No profiles for the base.' : 'No profiles for the target.'}</p>
          : (
            <div className="stack" style={{ gap: 'var(--space-2)' }}>
              <FlameDiffLegend />
              <FlameGraph root={d.root} total={d.total} baseTotal={d.baseTotal} format={fmt}
                meta={<>base {fmt(d.baseTotal)} → target {fmt(d.total)}</>} />
            </div>
          )}
      </Card>

      <Card title="Biggest changes" subtitle="Functions whose self share moved the most" flush>
        {!d ? <Loading /> : (
          <Table<DiffChange> rows={changes} rowKey={(c) => c.name} defaultSort={{ key: 'delta', dir: 'desc' }}
            empty={<span className="muted">No function changed its share by more than 0.05 pp.</span>} columns={[
              { key: 'name', header: 'Function', sortValue: (c) => c.name, render: (c) => <FunctionCell name={c.name} /> },
              { key: 'base', header: 'Base', numeric: true, sortValue: (c) => c.baseShare, render: (c) => `${(c.baseShare * 100).toFixed(2)}%` },
              { key: 'target', header: 'Target', numeric: true, sortValue: (c) => c.share, render: (c) => `${(c.share * 100).toFixed(2)}%` },
              { key: 'delta', header: 'Change', numeric: true, sortValue: (c) => Math.abs(c.delta), render: (c) => (
                <span className="row" style={{ gap: 'var(--space-2)', justifyContent: 'flex-end' }}>
                  <span className="legend__swatch" style={{ background: c.delta > 0 ? 'var(--diff-increase)' : 'var(--diff-decrease)' }} aria-hidden />
                  <span className="num">{signedPp(c.delta)}</span>
                  <span className="sr-only">{c.delta > 0 ? 'larger share' : 'smaller share'}</span>
                </span>) },
            ]} />
        )}
      </Card>
    </>
  );
}

// ---------------------------------------------------------------- endpoint section
/** Endpoint page: CPU flame graph filtered by the endpoint's route label (service-wide when the SDK sends no route labels). */
export function EndpointProfile({ svc, ep }: { svc: Service; ep: Endpoint }) {
  const { range } = useApp();
  const types = profileTypes(svc.id, range);
  const [search, setSearch] = useState('');
  const list = types.data ?? [];
  const info = list.find((t) => t.type === 'cpu') ?? list[0];
  const routed = !!info && info.routes > 0 && !ep.sub;
  const filter: ProfileFilter = routed ? { route: ep.route, method: ep.method } : {};
  return (
    <Section title={<span className="row" style={{ gap: 'var(--space-2)' }}><Cpu size={16} aria-hidden />Where CPU goes</span>}
      subtitle={routed ? `CPU profile of ${ep.method} ${ep.route} only (route label) · last ${RANGE_LABEL[range]}` : `whole ${svc.name} · last ${RANGE_LABEL[range]}`}
      actions={info ? <Link to={`/services/${svc.id}?tab=profiling`} className="row" style={{ gap: 'var(--space-1)', fontSize: 'var(--text-sm)' }}>All profiles <ArrowRight size={14} /></Link> : undefined}>
      {types.loading ? <Card><Loading /></Card> : !info ? <ProfilingSetup svc={svc} compact /> : (
        <>
          {!routed && <Callout icon={<Info size={16} />}>{ep.sub ? 'Profiles are labelled by HTTP route; this process' : `${svc.name}’s profiles have no route labels, so this`} shows the whole service.</Callout>}
          <FlameCard svc={svc} info={info} range={range} filter={filter} search={search} onSearch={setSearch} title={routed ? 'CPU on this endpoint' : 'CPU of the service'} />
        </>
      )}
    </Section>
  );
}

/** Deploys tab: link to the version-vs-version profile diff. */
export function compareProfilesUrl(serviceId: string, base: string, target: string) {
  return `/services/${encodeURIComponent(serviceId)}?${new URLSearchParams({ tab: 'profiling', compare: 'version', base, target })}`;
}
export const hasProfiles = (svc: Service, range: TimeRange = '24h') => (profileTypes(svc.id, range).data ?? []).length > 0;
