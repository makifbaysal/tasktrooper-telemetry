import { useMemo, useState, type ReactNode } from 'react';
import { Link, useNavigate, useSearchParams } from 'react-router';
import { Bell, ExternalLink, ListTree, ScrollText } from 'lucide-react';
import { Columns } from '@/components/charts';
import {
  Badge, Button, Card, CodeBlock, ConnectSdk, Drawer, DrawerSection, KeyValues, SearchInput, Segmented, Table,
  useDrawer, type Tone,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { allLogs, logVolumeSeries, logsFor, searchLogs, services, timestamps, type LogLine } from '@/data';
import { can } from '@/data/auth';
import { ago, clock, compact, dateTime, int } from '@/lib/format';
import { traceUrl } from './shared';

type Level = LogLine['level'];
type View = 'lines' | 'patterns';

const LEVELS: Level[] = ['error', 'warn', 'info', 'debug'];
const LEVEL_TONE: Record<Level, Tone> = { debug: 'neutral', info: 'accent', warn: 'warning', error: 'critical' };

// ---------------------------------------------------------------- pattern masking
/** Replace variable parts (ids, numbers, quoted values) so similar lines group together. */
function maskPattern(msg: string) {
  return msg
    .replace(/\b[0-9a-f]{8}-[0-9a-f-]{27,}\b/gi, '<uuid>')
    .replace(/\b[0-9a-f]{12,}\b/gi, '<hex>')
    .replace(/(["'])[^"']*\1/g, '<str>')
    .replace(/\b\d+(\.\d+)?(ms|s|%|kb|mb)?\b/gi, '<num>');
}

interface Pattern {
  id: string;
  pattern: string;
  level: Level;
  services: string[];
  lines: LogLine[];
  last: number;
}

function groupPatterns(rows: LogLine[]): Pattern[] {
  const map = new Map<string, Pattern>();
  for (const l of rows) {
    const p = maskPattern(l.message);
    const key = `${l.level}|${p}`;
    let g = map.get(key);
    if (!g) { g = { id: key, pattern: p, level: l.level, services: [], lines: [], last: 0 }; map.set(key, g); }
    g.lines.push(l);
    if (!g.services.includes(l.service)) g.services.push(l.service);
    g.last = Math.max(g.last, l.ts);
  }
  return [...map.values()];
}

const attrText = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v));

// ---------------------------------------------------------------- screen
/** Log search + facets + lines/patterns. With `serviceId` it is scoped to one service (used on service pages). */
export function LogExplorer({ serviceId }: { serviceId?: string }) {
  const nav = useNavigate();
  const { range, dataVersion } = useApp();
  // ?q= pre-fills the search; ?pod= / ?namespace= / ?traceId= (pod page) are server-side filters (GET /logs)
  const [params, setParams] = useSearchParams();
  const pod = params.get('pod') ?? '';
  const namespace = params.get('namespace') ?? '';
  const traceId = params.get('traceId') ?? '';
  const scoped = !!(pod || namespace || traceId);
  // allLogs is hydrated in place, so the data version is part of the key.
  const logs = useMemo(() => (scoped
    ? searchLogs(range, { serviceId, pod: pod || undefined, namespace: namespace || undefined, traceId: traceId || undefined, limit: 500 })
    : serviceId ? logsFor(serviceId) : [...allLogs]), [serviceId, dataVersion, scoped, pod, namespace, traceId, range]);
  const clearScope = () => setParams((p) => { const n = new URLSearchParams(p); ['pod', 'namespace', 'traceId'].forEach((k) => n.delete(k)); return n; }, { replace: true });
  const [q, setQ] = useState(() => params.get('q') ?? '');
  const [levels, setLevels] = useState<Set<Level>>(new Set(['info', 'warn', 'error']));
  const [svc, setSvc] = useState<string | null>(null);
  const [src, setSrc] = useState<string | null>(null);
  const [view, setView] = useState<View>('lines');
  const lineDrawer = useDrawer<LogLine>();
  const patternDrawer = useDrawer<Pattern>();
  const ts = timestamps(range);

  const rows = useMemo(() => logs.filter((l) =>
    levels.has(l.level) && (!svc || l.service === svc) && (!src || l.source === src) && (!q || l.message.toLowerCase().includes(q.toLowerCase()))), [logs, q, levels, svc, src]);
  // platform sources (Vercel drains: lambda, edge, static, build …) — the facet only shows when lines carry one
  const sources = useMemo(() => [...new Set(logs.map((l) => l.source).filter((x): x is string => !!x))].sort(), [logs]);
  const patterns = useMemo(() => groupPatterns(rows), [rows]);
  const allPatterns = useMemo(() => groupPatterns(logs), [logs]);
  const patternOf = (l: LogLine) => allPatterns.find((p) => p.id === `${l.level}|${maskPattern(l.message)}`)!;

  const toggle = (l: Level) => setLevels((s) => { const n = new Set(s); n.has(l) ? n.delete(l) : n.add(l); return n; });
  const logServices = services.filter((s) => logs.some((l) => l.service === s.name));

  // volume per level from the api (GET /logs/series), same server-side scope as the lines
  const volume = logVolumeSeries(range, { serviceId, pod: pod || undefined, namespace: namespace || undefined, traceId: traceId || undefined });

  if (logs.length === 0 && !scoped) {
    return (
      <Card>
        <ConnectSdk icon={<ScrollText />} title={serviceId ? 'No logs from this service yet' : 'No logs yet'} setup={!serviceId}>
          Log lines arrive through the SDK's logging integration (slog, ILogger, console / pino, Python logging, Logback …) and are
          linked to the trace that wrote them. Search, level and service facets and log patterns show up here.
        </ConnectSdk>
      </Card>
    );
  }

  return (
    <>

      {/* 1 · Query + volume: what the search returns, over time */}
      <Card>
        <div className="stack" style={{ gap: 'var(--space-4)' }}>
          <div className="row" style={{ gap: 'var(--space-2)' }}>
            <div style={{ flex: 1 }}>
              <SearchInput mono placeholder={serviceId ? 'level:error "timeout"' : 'service:payments-api level:error "provider timeout"'} value={q} onChange={(e) => setQ(e.target.value)} />
            </div>
            {serviceId && can('write') && <Button icon={<Bell size={14} />} onClick={() => nav('/alerts/new?kind=log_match')}>Alert on this query</Button>}
          </div>
          {scoped && (
            <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap', fontSize: 'var(--text-sm)' }}>
              <span className="muted">Filtered to</span>
              {pod && <Badge>pod {namespace ? `${namespace}/` : ''}{pod}</Badge>}
              {!pod && namespace && <Badge>namespace {namespace}</Badge>}
              {traceId && <Link to={traceUrl(traceId)} title="Open the trace"><Badge>trace <span className="mono">{traceId.slice(0, 12)}</span></Badge></Link>}
              <Button size="sm" onClick={clearScope}>Clear</Button>
            </div>
          )}
          <Columns timestamps={ts} height={110} format={(v) => compact(v)} series={[
            { id: 'info', label: 'info', data: volume.info, color: 'var(--series-1)' },
            { id: 'warn', label: 'warn', data: volume.warn, color: 'var(--series-4)' },
            { id: 'error', label: 'error', data: volume.error, color: 'var(--series-8)' },
          ]} />
        </div>
      </Card>

      {/* 2 · Facets + results */}
      <div style={{ display: 'grid', gridTemplateColumns: '210px minmax(0,1fr)', gap: 'var(--space-4)', alignItems: 'start' }}>
        <Card>
          <div className="stack" style={{ gap: 4 }}>
            <div className="field__label">Level</div>
            {LEVELS.map((l) => (
              <label key={l} className="row" style={{ cursor: 'pointer', height: 26 }}>
                <input type="checkbox" checked={levels.has(l)} onChange={() => toggle(l)} />
                <Badge tone={LEVEL_TONE[l]}>{l}</Badge>
                <span className="spacer" />
                <span className="muted num">{logs.filter((x) => x.level === l).length}</span>
              </label>
            ))}
            {sources.length > 0 && <div className="field__label" style={{ marginTop: 'var(--space-3)' }}>Source</div>}
            {sources.map((x) => (
              <button key={x} className="sidebar__link" aria-current={src === x ? 'page' : undefined}
                style={{ border: 0, background: src === x ? undefined : 'none', width: '100%', cursor: 'pointer', height: 'var(--space-6)' }}
                onClick={() => setSrc(src === x ? null : x)}>
                <span className="mono truncate">{x}</span>
                <span className="spacer" />
                <span className="muted num">{logs.filter((l) => l.source === x).length}</span>
              </button>
            ))}
            {!serviceId && <div className="field__label" style={{ marginTop: 12 }}>Service</div>}
            {!serviceId && logServices.map((s) => (
              <button key={s.id} className="sidebar__link" aria-current={svc === s.name ? 'page' : undefined}
                style={{ border: 0, background: svc === s.name ? undefined : 'none', width: '100%', cursor: 'pointer', height: 28 }}
                onClick={() => setSvc(svc === s.name ? null : s.name)}>
                <span className="truncate">{s.name}</span>
                <span className="spacer" />
                <span className="muted num">{logs.filter((l) => l.service === s.name).length}</span>
              </button>
            ))}
          </div>
        </Card>

        <Card flush
          title={<span className="secondary" style={{ fontWeight: 400 }}>
            {view === 'lines' ? `${int(rows.length)} lines` : `${patterns.length} patterns · ${int(rows.length)} lines`}
          </span>}
          actions={<Segmented label="View" value={view} onChange={setView} options={[
            { id: 'lines', label: 'Lines' },
            { id: 'patterns', label: 'Patterns' },
          ]} />}>
          {view === 'lines' ? (
            <div style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)' }}>
              {rows.length === 0 && <p className="muted" style={{ padding: 16 }}>No lines match this query.</p>}
              {rows.slice(0, 80).map((l) => (
                <div key={l.id} className="list__row" role="button" tabIndex={0}
                  style={{ padding: '4px 16px', gap: 12, alignItems: 'baseline', cursor: 'pointer' }}
                  onClick={() => lineDrawer.open(l)} onKeyDown={(e) => e.key === 'Enter' && lineDrawer.open(l)}>
                  <span className="muted num" style={{ flex: 'none' }}>{clock(l.ts, true)}</span>
                  <span style={{ flex: 'none', width: 52 }}><Badge tone={LEVEL_TONE[l.level]}>{l.level}</Badge></span>
                  {!serviceId && <span className="secondary truncate" style={{ flex: 'none', width: 160 }}>{l.service}</span>}
                  {sources.length > 0 && <span style={{ flex: 'none', width: 'calc(var(--space-12) + var(--space-6))' }}>{l.source && <Badge>{l.source}</Badge>}</span>}
                  <span className="truncate" style={{ flex: 1, minWidth: 0 }}>{l.message}</span>
                  {l.traceId && <ListTree size={14} className="muted" aria-label="Has trace" style={{ flex: 'none' }} />}
                </div>
              ))}
            </div>
          ) : (
            <Table<Pattern> rows={patterns} rowKey={(p) => p.id} defaultSort={{ key: 'count', dir: 'desc' }} onRowClick={patternDrawer.open} columns={[
              { key: 'level', header: 'Level', width: 70, sortValue: (p) => LEVELS.indexOf(p.level), render: (p) => <Badge tone={LEVEL_TONE[p.level]}>{p.level}</Badge> },
              { key: 'pattern', header: 'Pattern', render: (p) => <span className="mono truncate" style={{ display: 'block', maxWidth: 520 }}>{p.pattern}</span> },
              { key: 'svc', header: 'Services', render: (p) => <span className="secondary">{p.services.length === 1 ? p.services[0] : `${p.services.length} services`}</span> },
              { key: 'count', header: 'Lines', numeric: true, sortValue: (p) => p.lines.length, render: (p) => int(p.lines.length) },
              { key: 'last', header: 'Last seen', numeric: true, sortValue: (p) => p.last, render: (p) => <span className="muted">{ago(p.last)}</span> },
            ]} />
          )}
        </Card>
      </div>

      <LineDrawer line={lineDrawer.item} onClose={lineDrawer.close}
        pattern={lineDrawer.item ? patternOf(lineDrawer.item) : null}
        onSurrounding={(l) => { lineDrawer.close(); setQ(''); setSrc(null); if (!serviceId) setSvc(l.service); setLevels(new Set(LEVELS)); setView('lines'); }}
        onPattern={(p) => { lineDrawer.close(); patternDrawer.open(p); }} />
      <PatternDrawer pattern={patternDrawer.item} onClose={patternDrawer.close}
        onLine={(l) => { patternDrawer.close(); lineDrawer.open(l); }}
        onAlert={() => nav('/alerts/new?kind=log_match')} />
    </>
  );
}

// ---------------------------------------------------------------- drawers
function LineDrawer({ line, pattern, onClose, onSurrounding, onPattern }: {
  line: LogLine | null; pattern: Pattern | null; onClose: () => void;
  onSurrounding: (l: LogLine) => void; onPattern: (p: Pattern) => void;
}) {
  const nav = useNavigate();
  if (!line) return null;
  const attrs = Object.entries(line.attrs ?? {}).sort(([a], [b]) => a.localeCompare(b));
  return (
    <Drawer open onClose={onClose} width={680}
      title={<span className="row" style={{ gap: 8 }}><Badge tone={LEVEL_TONE[line.level]}>{line.level}</Badge>{line.service}</span>}
      subtitle={<>{dateTime(line.ts)} · {ago(line.ts)}</>}
      actions={<>
        {line.traceId && <Button size="sm" icon={<ExternalLink size={14} />} onClick={() => nav(traceUrl(line.traceId!))}>View trace</Button>}
        <Button size="sm" variant="ghost" onClick={() => onSurrounding(line)}>Show surrounding logs</Button>
      </>}>
      <DrawerSection title="Message">
        <CodeBlock>{line.message}</CodeBlock>
      </DrawerSection>
      <DrawerSection title="Context">
        <KeyValues items={[
          ['Timestamp', <span className="mono">{new Date(line.ts).toISOString()}</span>],
          ['Level', <Badge tone={LEVEL_TONE[line.level]}>{line.level}</Badge>],
          ['Service', line.service],
          ...(line.source ? [['Source', <Badge>{line.source}</Badge>] as [string, ReactNode]] : []),
          ['Trace id', line.traceId ? <Link to={traceUrl(line.traceId)} className="mono">{line.traceId}</Link> : <span className="muted">—</span>],
        ]} />
      </DrawerSection>
      <DrawerSection title="Attributes">
        {attrs.length === 0
          ? <p className="muted">No attributes on this line.</p>
          : <KeyValues items={attrs.map(([k, v]) => [<span className="mono">{k}</span>, <span className="mono" style={{ overflowWrap: 'anywhere' }}>{attrText(v)}</span>])} />}
      </DrawerSection>
      {pattern && (
        <DrawerSection title="Same pattern" aside={<a href="#" onClick={(e) => { e.preventDefault(); onPattern(pattern); }}>{int(pattern.lines.length)} lines · open pattern</a>}>
          <div className="mono muted" style={{ fontSize: 'var(--text-xs)', overflowWrap: 'anywhere' }}>{pattern.pattern}</div>
        </DrawerSection>
      )}
    </Drawer>
  );
}

function PatternDrawer({ pattern, onClose, onLine, onAlert }: {
  pattern: Pattern | null; onClose: () => void; onLine: (l: LogLine) => void; onAlert: () => void;
}) {
  if (!pattern) return null;
  const recent = [...pattern.lines].sort((a, b) => b.ts - a.ts);
  return (
    <Drawer open onClose={onClose} width={680}
      title={<span className="row" style={{ gap: 8 }}><Badge tone={LEVEL_TONE[pattern.level]}>{pattern.level}</Badge>Log pattern</span>}
      subtitle={<>{int(pattern.lines.length)} lines · {pattern.services.join(', ')} · last {ago(pattern.last)}</>}
      actions={can('write') && <Button size="sm" icon={<Bell size={14} />} onClick={onAlert}>Alert on pattern</Button>}>
      <DrawerSection title="Pattern" aside="variable parts masked">
        <CodeBlock>{pattern.pattern}</CodeBlock>
      </DrawerSection>
      <DrawerSection title="Recent lines" aside={recent.length > 20 ? `latest 20 of ${recent.length}` : undefined}>
        <Card flush>
          <div style={{ fontFamily: 'var(--font-mono)', fontSize: 'var(--text-sm)' }}>
            {recent.slice(0, 20).map((l) => (
              <div key={l.id} className="list__row" role="button" tabIndex={0} style={{ padding: '4px 16px', gap: 12, alignItems: 'baseline', cursor: 'pointer' }}
                onClick={() => onLine(l)} onKeyDown={(e) => e.key === 'Enter' && onLine(l)}>
                <span className="muted num" style={{ flex: 'none' }}>{clock(l.ts, true)}</span>
                <span className="secondary truncate" style={{ flex: 'none', width: 140 }}>{l.service}</span>
                <span className="truncate" style={{ flex: 1, minWidth: 0 }}>{l.message}</span>
              </div>
            ))}
          </div>
        </Card>
      </DrawerSection>
    </Drawer>
  );
}
