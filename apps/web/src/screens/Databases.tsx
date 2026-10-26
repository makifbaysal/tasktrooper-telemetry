import { useState } from 'react';
import { useNavigate, useParams } from 'react-router';
import { AlertOctagon, ArrowRight, CheckCircle2, Cloud, Database as DbIcon, Loader2, Plug, Plus, Wrench } from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Card, CodeBlock, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, Meter, MetricCard, PageHeader, Section, Segmented,
  Select, StatTile, StatusBadge, Table, useDrawer,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  COLLECTOR_LABEL, ENGINE_LABEL, PROVIDER_LABEL, databases, dbOpsPerSec, dbQueries, dbSeries, dbUsers, integrationFor, integrations, isKv, neonInfo, timestamps, useData,
  type Collector, type Database, type DbEngine, type DbQueryRow, type NeonCompute,
} from '@/data';
import { ago, compact, int, ms, pct } from '@/lib/format';
import { addDatabase, defaultPort, testConnection, useInfra } from '@/lib/infra';
import { LINE_SLOTS } from './shared';
import { IntegrationSourceBadge, IntegrationStatusBadge, ProviderMark } from './Integrations';

const meterTone = (v: number) => (v >= 0.85 ? 'critical' : v >= 0.7 ? 'warning' : undefined);
const opsUnit = (db: Database) => (isKv(db.engine) ? 'ops/s' : 'queries/s');

function UsageCell({ value, label }: { value: number; label?: string }) {
  return (
    <div style={{ minWidth: 84 }}>
      <div className="row" style={{ justifyContent: 'space-between', gap: 'var(--space-2)', fontSize: 'var(--text-sm)' }}>
        <span className="num">{Math.round(value * 100)}%</span>{label && <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{label}</span>}
      </div>
      <Meter value={value} tone={meterTone(value)} />
    </div>
  );
}

function EngineLine({ db }: { db: Database }) {
  const neon = neonInfo(db);
  return <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{ENGINE_LABEL[db.engine]} {db.version} · {db.source.split(' · ')[0]}{neon && <> · {neon.branch} · {NEON_STATE[neon.state].label.toLowerCase()}</>}</span>;
}

const NEON_STATE: Record<NeonCompute['state'], { status: 'ok' | 'muted' | 'pending'; label: string }> = {
  active: { status: 'ok', label: 'Active' }, idle: { status: 'muted', label: 'Idle' }, init: { status: 'pending', label: 'Starting' },
};

/** Neon project / branch / compute endpoint: state, autoscaling range and storage. */
function NeonCard({ n }: { n: NeonCompute }) {
  return (
    <Card title="Neon compute" subtitle={`${n.project} · branch ${n.branch}`}
      actions={<StatusBadge status={NEON_STATE[n.state].status} label={n.state === 'idle' ? 'Idle · scaled to zero' : NEON_STATE[n.state].label} />}>
      <KeyValues items={[
        ['Compute endpoint', <span key="e" className="mono">{n.endpoint}</span>],
        ['Autoscaling', `${n.cuMin}–${n.cuMax} CU`],
        ['Size now', n.state === 'active' ? `${n.cuNow} CU` : '0 CU (suspended)'],
        ['Active time this month', `${n.activeHours} h`],
        ['Storage', n.storageLimitGb ? <div key="s" style={{ maxWidth: 220 }}><div className="num" style={{ fontSize: 'var(--text-sm)' }}>{n.storageGb.toFixed(1)} of {n.storageLimitGb} GB</div><Meter value={n.storageGb / n.storageLimitGb} tone={meterTone(n.storageGb / n.storageLimitGb)} /></div> : `${n.storageGb.toFixed(1)} GB`],
      ]} />
      {n.state === 'idle' && <p className="muted" style={{ margin: 'var(--space-3) 0 0', fontSize: 'var(--text-sm)' }}>Idle computes report no CPU or connections; the first query wakes it (cold start ~500 ms).</p>}
    </Card>
  );
}

// =================================================================== list
export function Databases() {
  useInfra();
  useData();
  const nav = useNavigate();
  const { range } = useApp();
  const [connect, setConnect] = useState(false);
  const bad = databases.filter((d) => d.status !== 'ok').sort((a, b) => (a.status === 'critical' ? -1 : 1) - (b.status === 'critical' ? -1 : 1));
  const open = (d: Database) => nav(`/infrastructure/databases/${d.id}`);

  return (
    <>
      <PageHeader title="Databases"
        subtitle="Databases and caches your services call — CPU, memory, connections and the queries behind them"
        actions={<Button variant="primary" icon={<Plus size={14} />} onClick={() => setConnect(true)}>Connect database</Button>} />

      {bad.length > 0 && (
        <Section title="Needs attention">
          <div className="grid grid-3">
            {bad.map((d) => (
              <Card key={d.id} onClick={() => open(d)} className={d.status === 'critical' ? 'card--rail-critical' : 'card--rail-warning'}>
                <div className="row" style={{ gap: 'var(--space-2)', marginBottom: 'var(--space-2)' }}>
                  <StatusBadge status={d.status} /><strong>{d.name}</strong><span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{ENGINE_LABEL[d.engine]}</span>
                </div>
                <div style={{ fontWeight: 500, marginBottom: 'var(--space-1)' }}>{d.problem?.title}</div>
                <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>
                  CPU {pct(d.cpu, 0)} · {int(d.connections)}/{int(d.maxConnections)} connections · p95 {ms(d.p95)}
                </div>
              </Card>
            ))}
          </div>
        </Section>
      )}

      <Section title="All databases" subtitle={`${databases.length} connected`}>
        <Card flush>
          <Table rows={databases} rowKey={(d) => d.id} onRowClick={open} defaultSort={{ key: 'status', dir: 'desc' }} columns={[
            { key: 'name', header: 'Database', sortValue: (d) => d.name, render: (d) => (
              <div style={{ whiteSpace: 'nowrap' }}>
                <div className="row" style={{ gap: 6, fontWeight: 500 }}>{d.name}{d.addedAt && <Badge tone="accent">new</Badge>}{(() => { const it = integrationFor(d); return it ? <IntegrationSourceBadge it={it} compact /> : null; })()}</div>
                <EngineLine db={d} />
              </div>) },
            { key: 'status', header: 'Status', sortValue: (d) => ['ok', 'warning', 'critical'].indexOf(d.status), render: (d) => <StatusBadge status={d.status} /> },
            { key: 'cpu', header: 'CPU', sortValue: (d) => d.cpu, render: (d) => (
              <div className="row" style={{ gap: 'var(--space-2)' }}><UsageCell value={d.cpu} /><Sparkline data={dbSeries(d, 'cpu', range).slice(-24)} width={56} height={22} /></div>) },
            { key: 'mem', header: 'Memory', sortValue: (d) => d.memory, render: (d) => <UsageCell value={d.memory} /> },
            { key: 'conn', header: 'Connections', sortValue: (d) => (d.maxConnections > 0 ? d.connections / d.maxConnections : 0), render: (d) => d.maxConnections > 0
              ? <UsageCell value={d.connections / d.maxConnections} label={`${int(d.connections)}/${compact(d.maxConnections)}`} />
              : <span className="num">{int(d.connections)}</span> },
            { key: 'ops', header: 'Throughput', numeric: true, sortValue: dbOpsPerSec, render: (d) => <span>{compact(dbOpsPerSec(d))} <span className="muted">{opsUnit(d)}</span></span> },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (d) => d.p95, render: (d) => ms(d.p95) },
            { key: 'users', header: 'Used by', render: (d) => {
              const u = [...new Set(dbUsers(d).map((x) => x.service.name))];
              return u.length ? <span className="secondary truncate" style={{ display: 'block', maxWidth: 150, fontSize: 'var(--text-sm)' }}>{u.slice(0, 3).join(', ')}{u.length > 3 && ` +${u.length - 3}`}</span> : <span className="muted">—</span>;
            } },
          ]} />
        </Card>
      </Section>

      {connect && <ConnectDatabase onClose={() => setConnect(false)} onDone={(id) => { setConnect(false); nav(`/infrastructure/databases/${id}`); }} />}
    </>
  );
}

// =================================================================== detail
export function DatabaseDetail() {
  useInfra();
  useData();
  const { databaseId = '' } = useParams();
  const nav = useNavigate();
  const { range } = useApp();
  const q = useDrawer<DbQueryRow>();
  const db = databases.find((d) => d.id === databaseId);
  if (!db) return <EmptyState icon={<DbIcon />} title="Database not found" />;
  const ts = timestamps(range);
  const kv = isKv(db.engine);
  const users = dbUsers(db);
  const queries = dbQueries(db);
  const replicas = db.nodes.filter((n) => n.replLagMs !== undefined);
  const events = db.since ? [{ ts: db.since, label: 'migration 0042' }] : undefined;
  const integration = integrationFor(db);
  const neon = neonInfo(db);
  const nodeSeries = (metric: 'cpu' | 'connections') => db.nodes.map((n, i) => ({ id: n.id, label: `${n.id} · ${n.role}`, slot: LINE_SLOTS[i % LINE_SLOTS.length], data: dbSeries(db, metric, range, n.id) }));

  return (
    <>
      <PageHeader title={db.name}
        crumbs={[{ label: 'Databases', to: '/infrastructure/databases' }, { label: db.name }]}
        subtitle={<span className="row" style={{ gap: 12, flexWrap: 'wrap' }}>
          <StatusBadge status={db.status} />
          <span className="secondary">{ENGINE_LABEL[db.engine]} {db.version}</span>
          <span className="muted">{db.instanceClass} · {db.nodes.length} node{db.nodes.length > 1 ? 's' : ''} · {db.source}</span>
          {integration && <IntegrationSourceBadge it={integration} />}
        </span>} />

      {integration?.status === 'error' && (
        <Card className="card--rail-critical" title={<span className="row" style={{ gap: 'var(--space-2)' }}><AlertOctagon size={14} style={{ color: 'var(--status-critical-text)' }} aria-hidden />Not syncing — values below are from {integration.lastSyncAt ? ago(integration.lastSyncAt) : 'the last sync'}</span>}
          actions={<Button size="sm" icon={<Plug size={14} />} onClick={() => nav(`/settings/integrations?id=${encodeURIComponent(integration.id)}`)}>Open {integration.name}</Button>}>
          <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>{PROVIDER_LABEL[integration.provider]} integration {integration.name}: {integration.error}</p>
        </Card>
      )}
      {neon && <Section><NeonCard n={neon} /></Section>}

      {db.problem && (
        <Card className={db.status === 'critical' ? 'card--rail-critical' : 'card--rail-warning'}
          title={<span className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status={db.status} />{db.problem.title}</span>}
          subtitle={db.since ? `since ${ago(db.since)}` : undefined}>
          <p className="secondary" style={{ margin: 0 }}>{db.problem.detail}</p>
          <div className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-start', marginTop: 'var(--space-3)' }}>
            <Wrench size={14} className="muted" style={{ marginTop: 3, flexShrink: 0 }} aria-hidden />
            <span style={{ fontSize: 'var(--text-sm)' }}>{db.problem.fix}</span>
          </div>
        </Card>
      )}

      <Section>
        <div className="grid grid-3">
          <MetricCard label="CPU" value={pct(db.cpu, 0)} curr={db.cpu} prev={db.prevCpu} upIsGood={false}
            hint={db.nodes.length > 1 ? `${db.nodes.length} nodes · busiest ${pct(Math.max(...db.nodes.map((n) => n.cpu)), 0)}` : undefined}>
            <TimeSeries timestamps={ts} height={150} format={(v) => pct(v, 0)} yMax={1} events={events}
              threshold={{ value: 0.8, label: '80%' }} series={nodeSeries('cpu')} />
          </MetricCard>
          <MetricCard label="Connections" value={int(db.connections)} unit={db.maxConnections > 0 ? `/ ${compact(db.maxConnections)}` : undefined}
            hint={db.maxConnections > 0 ? `${pct(db.connections / db.maxConnections, 0)} of ${kv ? 'maxclients' : 'max_connections'}` : undefined}>
            <TimeSeries timestamps={ts} height={150} format={(v) => compact(v)} events={events}
              threshold={kv || !(db.maxConnections > 0) ? undefined : { value: db.maxConnections, label: 'max' }} yMax={kv || !(db.maxConnections > 0) ? undefined : db.maxConnections * 1.05}
              series={[{ id: 'conn', label: 'Connections', slot: 2, data: dbSeries(db, 'connections', range) }]} />
          </MetricCard>
          <MetricCard label={kv ? 'Command latency · p95' : 'Query latency · p95'} value={ms(db.p95)} curr={db.p95} prev={db.prevP95} upIsGood={false}
            hint={kv ? 'server-side, from SLOWLOG / latency stats' : `${db.slowPerMin} slow queries / min (> 500 ms)`}>
            <TimeSeries timestamps={ts} height={150} format={ms} events={events} series={[{ id: 'p95', label: 'p95', slot: 3, data: dbSeries(db, 'p95', range) }]} />
          </MetricCard>
          <MetricCard label="Throughput" value={compact(dbOpsPerSec(db))} unit={opsUnit(db)}>
            <TimeSeries timestamps={ts} height={120} area format={(v) => compact(v)} series={[{ id: 'ops', label: opsUnit(db), slot: 1, data: dbSeries(db, 'ops', range) }]} />
          </MetricCard>
          <MetricCard label={kv ? 'Keyspace hit ratio' : 'Buffer cache hit ratio'} value={pct(db.hitRatio, 1)}
            hint={kv && db.evictionsPerMin ? `${db.evictionsPerMin} evictions / min` : undefined}>
            <TimeSeries timestamps={ts} height={120} yMax={1} format={(v) => pct(v, 0)} events={events} series={[{ id: 'hit', label: 'Hit ratio', slot: 5, data: dbSeries(db, 'hit', range) }]} />
          </MetricCard>
          {replicas.length > 0
            ? <MetricCard label="Replication lag" value={ms(Math.max(...replicas.map((r) => r.replLagMs!)))} hint="worst replica">
                <TimeSeries timestamps={ts} height={120} format={ms} events={events}
                  series={replicas.map((r, i) => ({ id: r.id, label: r.id, slot: LINE_SLOTS[(db.nodes.indexOf(r)) % LINE_SLOTS.length] ?? LINE_SLOTS[i], data: dbSeries(db, 'repl', range, r.id) }))} />
              </MetricCard>
            : <MetricCard label="Memory" value={pct(db.memory, 0)} hint={kv ? 'of maxmemory' : undefined}>
                <TimeSeries timestamps={ts} height={120} format={(v) => pct(v, 0)} yMax={1} series={[{ id: 'mem', label: 'Memory', slot: 6, data: dbSeries(db, 'memory', range) }]} />
              </MetricCard>}
        </div>
        <div className="grid grid-4">
          {replicas.length > 0 && <StatTile label="Memory" value={pct(db.memory, 0)} />}
          {!kv && <StatTile label="Disk" value={pct(db.disk, 0)} unit={`of ${db.diskGb} GB`} />}
          {!kv && <StatTile label="Slow queries" value={db.slowPerMin} unit="/ min" />}
          {kv && <StatTile label="Evictions" value={db.evictionsPerMin ?? 0} unit="/ min" />}
          <StatTile label="Collected by" value={<span style={{ fontSize: 'var(--text-md)' }}>{COLLECTOR_LABEL[db.collector]}</span>} />
        </div>
      </Section>

      <Section title="Nodes" subtitle={db.host}>
        <Card flush>
          <Table rows={db.nodes} rowKey={(n) => n.id} columns={[
            { key: 'id', header: 'Node', render: (n) => <span className="mono">{n.id}</span> },
            { key: 'role', header: 'Role', render: (n) => <Badge tone={n.role === 'primary' ? 'accent' : 'neutral'}>{n.role}</Badge> },
            { key: 'zone', header: 'Zone', render: (n) => <span className="muted">{n.zone}</span> },
            { key: 'cpu', header: 'CPU', sortValue: (n) => n.cpu, render: (n) => <UsageCell value={n.cpu} /> },
            { key: 'mem', header: 'Memory', sortValue: (n) => n.memory, render: (n) => <UsageCell value={n.memory} /> },
            { key: 'conn', header: 'Connections', numeric: true, sortValue: (n) => n.connections, render: (n) => int(n.connections) },
            { key: 'lag', header: 'Replication lag', numeric: true, render: (n) => n.replLagMs === undefined ? <span className="muted">—</span>
              : <span className={n.replLagMs > 1000 ? 'delta--bad' : undefined}>{ms(n.replLagMs)}</span> },
          ]} />
        </Card>
      </Section>

      <Section title={kv ? 'Top commands' : 'Top queries'} subtitle={`by share of ${kv ? 'server' : 'database'} time · normalized, parameters removed`}>
        <Card flush>
          <Table rows={queries} rowKey={(r) => r.id} onRowClick={q.open} columns={[
            { key: 'q', header: kv ? 'Command' : 'Query', render: (r) => (
              <div style={{ maxWidth: 300 }}>
                <div className="mono truncate" style={{ fontSize: 'var(--text-sm)' }} title={r.text}>{r.text}</div>
                {r.plan && <Badge tone="critical">{r.plan.split(' (')[0]}</Badge>}
              </div>) },
            { key: 'by', header: 'Called by', render: (r) => (
              <div style={{ whiteSpace: 'nowrap', fontSize: 'var(--text-sm)' }}>
                <div>{r.serviceId}</div><div className="muted mono" style={{ fontSize: 'var(--text-xs)' }}>{r.endpoint}</div>
              </div>) },
            { key: 'calls', header: 'Calls / min', numeric: true, sortValue: (r) => r.callsPerMin, render: (r) => compact(r.callsPerMin) },
            { key: 'avg', header: 'Avg', numeric: true, sortValue: (r) => r.avgMs, render: (r) => ms(r.avgMs) },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (r) => r.p95, render: (r) => ms(r.p95) },
            { key: 'rows', header: kv ? 'Keys' : 'Rows', numeric: true, sortValue: (r) => r.rows, render: (r) => <span className={r.rows > 100_000 ? 'delta--bad' : undefined}>{compact(r.rows)}</span> },
            { key: 'share', header: 'Time share', sortValue: (r) => r.share, render: (r) => <UsageCell value={r.share} /> },
          ]} />
        </Card>
      </Section>

      <Section title="Used by" subtitle="Instrumented services whose SDK reports calls to this database — as they see it">
        {users.length === 0
          ? <Card><EmptyState icon={<DbIcon />} title="No instrumented service calls it yet">Once a service's SDK reports calls to {db.host}, it shows up here with its calls, latency and errors.</EmptyState></Card>
          : <Card flush>
              <Table rows={users} rowKey={(u) => u.service.id} onRowClick={(u) => nav(`/services/${u.service.id}?tab=traffic`)} columns={[
                { key: 's', header: 'Service', render: (u) => <span style={{ fontWeight: 500 }}>{u.service.name}</span> },
                { key: 'calls', header: 'Calls / min', numeric: true, sortValue: (u) => u.dep.callsPerMin, render: (u) => compact(u.dep.callsPerMin) },
                { key: 'avg', header: 'Avg', numeric: true, render: (u) => ms(u.dep.avgMs) },
                { key: 'p95', header: 'p95 (client side)', numeric: true, render: (u) => ms(u.dep.p95) },
                { key: 'err', header: 'Errors', numeric: true, render: (u) => pct(u.dep.errorRate) },
                { key: 'go', header: '', width: 32, render: () => <ArrowRight size={14} className="muted" aria-hidden /> },
              ]} />
            </Card>}
      </Section>

      {q.item && (
        <Drawer open onClose={q.close} title={kv ? 'Command' : 'Query'} subtitle={<span className="muted">{db.name} · {pct(q.item.share, 0)} of {kv ? 'server' : 'database'} time</span>}
          actions={q.item.ep && <Button size="sm" onClick={() => nav(`/services/${q.item!.serviceId}/endpoints/${q.item!.ep!.id}`)}>Open endpoint <ArrowRight size={14} /></Button>}>
          <CodeBlock>{q.item.text}</CodeBlock>
          {q.item.plan && <DrawerSection title="Plan"><p className="secondary" style={{ margin: 0 }}>{q.item.plan}</p></DrawerSection>}
          <DrawerSection title="Numbers">
            <KeyValues items={[
              ['Called by', `${q.item.serviceId} · ${q.item.endpoint}`],
              ['Calls per request', `${q.item.perCall}×`],
              ['Calls / min', compact(q.item.callsPerMin)],
              ['Avg / p95', `${ms(q.item.avgMs)} / ${ms(q.item.p95)}`],
              [kv ? 'Keys per call' : 'Rows examined per call', compact(q.item.rows)],
            ]} />
          </DrawerSection>
        </Drawer>
      )}
    </>
  );
}

// =================================================================== connect
const ENGINES: { id: DbEngine; label: string }[] = [
  { id: 'postgres', label: 'PostgreSQL' }, { id: 'mysql', label: 'MySQL' }, { id: 'redis', label: 'Redis' }, { id: 'mongodb', label: 'MongoDB' },
];
const COLLECTORS: { id: Collector; label: string }[] = [
  { id: 'agent', label: 'Agent' }, { id: 'cloud', label: 'Cloud' }, { id: 'direct', label: 'Direct' },
];
const GRANTS: Record<DbEngine, string> = {
  postgres: "CREATE ROLE trooper_monitor LOGIN PASSWORD '…';\nGRANT pg_monitor TO trooper_monitor;\n-- top queries need the extension:\nCREATE EXTENSION IF NOT EXISTS pg_stat_statements;",
  mysql: "CREATE USER 'trooper_monitor'@'%' IDENTIFIED BY '…';\nGRANT PROCESS, REPLICATION CLIENT, SELECT ON performance_schema.* TO 'trooper_monitor'@'%';",
  redis: 'ACL SETUSER trooper_monitor on >… +info +slowlog +latency +client|list +config|get -@all',
  mongodb: 'db.createUser({ user: "trooper_monitor", pwd: "…", roles: [{ role: "clusterMonitor", db: "admin" }] })',
};
/** Providers without a cloud integration yet: connected one database at a time. */
const CLOUD: Record<DbEngine, string[]> = {
  postgres: ['Azure Database for PostgreSQL'],
  mysql: ['Azure Database for MySQL'],
  redis: ['Azure Cache for Redis'],
  mongodb: ['MongoDB Atlas', 'AWS DocumentDB'],
};
/** Managed services a cloud integration discovers by itself (Settings → Integrations). */
const DISCOVERED: Record<DbEngine, string> = {
  postgres: 'AWS RDS / Aurora, Google Cloud SQL and Neon',
  mysql: 'AWS RDS / Aurora and Google Cloud SQL',
  redis: 'AWS ElastiCache and Google Memorystore',
  mongodb: '',
};

function ConnectDatabase({ onClose, onDone }: { onClose: () => void; onDone: (id: string) => void }) {
  const nav = useNavigate();
  const [engine, setEngine] = useState<DbEngine>('postgres');
  const [collector, setCollector] = useState<Collector>('agent');
  const [name, setName] = useState('');
  const [host, setHost] = useState('');
  const [port, setPort] = useState('');
  const [provider, setProvider] = useState(CLOUD.postgres[0]);
  const [resource, setResource] = useState('');
  const [test, setTest] = useState<'idle' | 'running' | string>('idle');
  const target = collector === 'cloud' ? resource : host ? `${host}:${port || defaultPort(engine)}` : '';
  const ready = name.trim() && (collector === 'agent' || target);
  const pickEngine = (e: DbEngine) => { setEngine(e); setProvider(CLOUD[e][0]); setTest('idle'); };
  const run = async () => { setTest('running'); setTest(await testConnection('db', engine, target)); };

  return (
    <Drawer open onClose={onClose} width={600} title="Connect database"
      subtitle="Read-only: TaskTrooper reads statistics views, never your data."
      actions={<Button variant="primary" disabled={!ready} onClick={() => onDone(addDatabase({ engine, name, collector, target, provider: `${provider}` }))}>Connect</Button>}>
      <div className="stack">
        <Field label="Engine"><Segmented label="Engine" options={ENGINES} value={engine} onChange={pickEngine} /></Field>
        <Field label="Name" hint="Use the name your services know it by (e.g. orders-db) so their calls are matched to it.">
          <Input value={name} onChange={(e) => setName(e.target.value)} placeholder="orders-db" />
        </Field>
        <Field label="How to collect">
          <Segmented label="How to collect" options={COLLECTORS} value={collector} onChange={(c) => { setCollector(c); setTest('idle'); }} />
        </Field>
        <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>
          {collector === 'agent' && 'The TaskTrooper agent runs next to the database (k8s DaemonSet or VM) and reads statistics locally. Best for self-hosted databases; nothing is opened to the internet.'}
          {collector === 'cloud' && 'Reads CPU, memory, connections and storage from the cloud provider\'s API. Add a monitoring user too to get top queries.'}
          {collector === 'direct' && 'TaskTrooper connects from its own network with a read-only monitoring user. The host must be reachable from our egress IPs (Settings → Network).'}
        </p>
        {collector === 'cloud' && DISCOVERED[engine] && (
          <Card title={<span className="row" style={{ gap: 'var(--space-2)' }}><Cloud size={14} aria-hidden />{DISCOVERED[engine]}: use a cloud integration</span>}
            actions={<Button size="sm" variant="primary" icon={<Plug size={14} />} onClick={() => nav('/settings/integrations')}>Open integrations</Button>}>
            <p className="secondary" style={{ margin: '0 0 var(--space-3)', fontSize: 'var(--text-sm)' }}>
              Connect the account once — every instance is discovered and kept in sync, new ones included. No per-database setup.
            </p>
            {integrations.length > 0 && (
              <div className="stack" style={{ gap: 'var(--space-2)' }}>
                {integrations.filter((i) => i.provider !== 'vercel').map((i) => (
                  <div key={i.id} className="row" style={{ gap: 'var(--space-2)', fontSize: 'var(--text-sm)', flexWrap: 'wrap' }}>
                    <ProviderMark provider={i.provider} size="sm" /><span style={{ fontWeight: 500 }}>{i.name}</span>
                    <span className="muted">{i.resources.filter((r) => r.kind === 'database').length} databases</span>
                    <IntegrationStatusBadge status={i.status} />
                  </div>
                ))}
              </div>
            )}
          </Card>
        )}
        {collector === 'cloud' && <>
          <div className="eyebrow">{DISCOVERED[engine] ? 'Other providers' : 'Provider'}</div>
          <Field label="Provider"><Select value={provider} onChange={(e) => setProvider(e.target.value)} options={CLOUD[engine].map((p) => ({ value: p, label: p }))} /></Field>
          <Field label="Instance / cluster identifier" hint="ARN or instance id; the cloud integration's role needs read access to its metrics.">
            <Input mono value={resource} onChange={(e) => setResource(e.target.value)} placeholder="arn:aws:rds:eu-central-1:123456789012:cluster:orders-db" />
          </Field>
        </>}
        {collector !== 'cloud' && (
          <div className="grid" style={{ gridTemplateColumns: '1fr 120px' }}>
            <Field label="Host"><Input mono value={host} onChange={(e) => setHost(e.target.value)} placeholder={collector === 'agent' ? 'localhost' : 'orders-db.internal'} /></Field>
            <Field label="Port"><Input mono value={port} onChange={(e) => setPort(e.target.value)} placeholder={String(defaultPort(engine))} /></Field>
          </div>
        )}
        {collector === 'direct' && (
          <div className="grid grid-2">
            <Field label="User"><Input mono defaultValue="trooper_monitor" /></Field>
            <Field label="Password" hint="Stored encrypted; only the collector can read it."><Input type="password" autoComplete="new-password" /></Field>
          </div>
        )}
        <Field label="Monitoring user" hint="Run once on the database; the agent and direct modes use it, cloud mode uses it for top queries.">
          <CodeBlock>{GRANTS[engine]}</CodeBlock>
        </Field>
        {collector === 'agent' && (
          <Field label="Agent config">
            <CodeBlock>{`helm upgrade --install trooper-agent trooper/agent \\\n  --set integrations.${engine}.enabled=true \\\n  --set integrations.${engine}.address=${host || 'localhost'}:${port || defaultPort(engine)} \\\n  --set integrations.${engine}.user=trooper_monitor`}</CodeBlock>
          </Field>
        )}
        <div className="row" style={{ gap: 'var(--space-3)' }}>
          <Button onClick={run} disabled={test === 'running' || (collector !== 'agent' && !target)}
            icon={test === 'running' ? <Loader2 size={14} className="spin" /> : undefined}>Test connection</Button>
          {test !== 'idle' && test !== 'running' && <span className="row" style={{ gap: 6, color: 'var(--status-good-text)', fontSize: 'var(--text-sm)' }}><CheckCircle2 size={14} aria-hidden />{test}</span>}
        </div>
      </div>
    </Drawer>
  );
}
