import { useState, type ReactNode } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import { ArrowRight, CheckCircle2, Cloud, Loader2, Plug, Plus, Waypoints } from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CodeBlock, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, Meter, PageHeader, Section, Segmented,
  Select, StatusBadge, Table, Tabs,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  BROKER_LABEL, brokerNodeSeries, brokerTerms, clusterHealth, clusterOf, clusters, consumerGroups, groupHealth, groupLag, groupsReading,
  integrationFor, integrations, isQueueKind, lagSeries, readHealth, readLag, services, timestamps, topicHealth, topicInSeries, topicLag, topicOf, topics, useData,
  type Cluster, type ConsumerGroup, type Topic,
} from '@/data';
import { compact, pct } from '@/lib/format';
import { addCluster, testConnection, useInfra } from '@/lib/infra';
import { ReadLag, ageLabel, catchUpLabel } from './ConsumerLag';
import { IntegrationSourceBadge, IntegrationStatusBadge, ProviderMark } from './Integrations';
import { LagAlertList } from './LagAlerts';
import { lagMonitorsOf } from '@/lib/lagAlerts';
import { LINE_SLOTS } from './shared';

const RANK = { ok: 0, warning: 1, critical: 2 } as const;
const svcName = (id: string) => services.find((s) => s.id === id)?.name ?? id;
const meterTone = (v: number) => (v >= 0.85 ? 'critical' : v >= 0.7 ? 'warning' : undefined);
/** Oldest waiting message across reads (SQS / Pub/Sub / RabbitMQ report it; Kafka doesn't). */
const oldestOf = (gs: ConsumerGroup[], topic?: string) => {
  const xs = gs.flatMap((g) => g.reads.filter((r) => !topic || r.topic === topic).map((r) => r.oldestAgeS)).filter((x): x is number => x !== undefined);
  return xs.length ? Math.max(...xs) : undefined;
};
/** Retention for display: `-1` (collector convention, from `retentionMs` or the string) means unlimited. */
function retentionLabel(t: Topic): string {
  const ms = (t as Topic & { retentionMs?: number | null }).retentionMs;
  if (ms === -1 || /^\s*-1(\s*ms)?\s*$/.test(t.retention ?? '')) return 'Unlimited';
  return t.retention || '—';
}
/** Managed queues (SQS, Pub/Sub) have no broker nodes to show. */
const hasNodes = (c: Cluster) => c.nodes.length > 0;

/** Drawers live in the URL (`?topic=cluster/name`, `?group=cluster/id`) so other pages can deep-link to them. */
function useQueueDrawers() {
  const [params, setParams] = useSearchParams();
  const set = (k: 'topic' | 'group', v: string | null) => setParams((p) => {
    const n = new URLSearchParams(p);
    n.delete('topic'); n.delete('group');
    if (v) n.set(k, v);
    return n;
  }, { replace: true });
  const [tc, ...tn] = (params.get('topic') ?? '').split('/');
  const [gc, ...gi] = (params.get('group') ?? '').split('/');
  const topic = tn.length ? topicOf(tc, tn.join('/')) : undefined;
  const group = gi.length ? consumerGroups.find((g) => g.clusterId === gc && g.id === gi.join('/')) : undefined;
  return {
    openTopic: (t: Topic) => set('topic', `${t.clusterId}/${t.name}`),
    openGroup: (g: ConsumerGroup) => set('group', `${g.clusterId}/${g.id}`),
    drawers: <>
      {topic && <TopicDrawer t={topic} onClose={() => set('topic', null)} onGroup={(g) => set('group', `${g.clusterId}/${g.id}`)} />}
      {group && <GroupDrawer g={group} onClose={() => set('group', null)} onTopic={(t) => set('topic', `${t.clusterId}/${t.name}`)} />}
    </>,
  };
}

// =================================================================== tables
function Consumer({ g }: { g: ConsumerGroup }) {
  return g.serviceId
    ? <span>{svcName(g.serviceId)}{g.process && g.process !== g.id && <span className="muted"> · {g.process}</span>}</span>
    : <span className="muted">{g.external}</span>;
}

function GroupsTable({ rows, showCluster, onOpen }: { rows: ConsumerGroup[]; showCluster?: boolean; onOpen: (g: ConsumerGroup) => void }) {
  const { range } = useApp();
  const worstRead = (g: ConsumerGroup) => g.reads.reduce((a, r) => (RANK[readHealth(g, r)] > RANK[readHealth(g, a)] ? r : a), g.reads[0]);
  const trend = (g: ConsumerGroup) => {
    const s = g.reads.map((r) => lagSeries(g, r, range).slice(-24));
    return s[0].map((_, i) => s.reduce((a, x) => a + x[i], 0));
  };
  return (
    <Card flush>
      <Table rows={rows} rowKey={(g) => `${g.clusterId}/${g.id}`} onRowClick={onOpen} defaultSort={{ key: 'status', dir: 'desc' }}
        empty="No consumer groups yet" columns={[
        { key: 'id', header: 'Group / consumer', sortValue: (g) => g.id, render: (g) => (
          <div style={{ whiteSpace: 'nowrap' }}>
            <div className="mono" style={{ fontWeight: 500 }}>{g.id}</div>
            <div style={{ fontSize: 'var(--text-xs)' }}><Consumer g={g} /></div>
          </div>) },
        ...(showCluster ? [{ key: 'cluster', header: 'Cluster', render: (g: ConsumerGroup) => <span className="secondary" style={{ whiteSpace: 'nowrap' }}>{clusterOf(g.clusterId)?.name}</span> }] : []),
        { key: 'reads', header: 'Reads', render: (g) => <div className="mono" style={{ fontSize: 'var(--text-sm)' }}>{g.reads.map((r) => <div key={r.topic}>{r.topic}</div>)}</div> },
        { key: 'status', header: 'Status', sortValue: (g) => RANK[groupHealth(g)] * 1e9 + groupLag(g), render: (g) => <StatusBadge status={groupHealth(g)} label={groupHealth(g) === 'ok' ? 'Keeping up' : undefined} /> },
        { key: 'lag', header: 'Lag', numeric: true, sortValue: groupLag, render: (g) => compact(groupLag(g)) },
        ...(rows.some((g) => oldestOf([g]) !== undefined) ? [{ key: 'oldest', header: 'Oldest msg', numeric: true, sortValue: (g: ConsumerGroup) => oldestOf([g]) ?? -1,
          render: (g: ConsumerGroup) => { const a = oldestOf([g]); return a === undefined ? <span className="muted">—</span> : ageLabel(a); } }] : []),
        { key: 'trend', header: '', render: (g) => <Sparkline data={trend(g)} width={72} height={24} /> },
        { key: 'catch', header: 'Catch-up', render: (g) => { const l = catchUpLabel(g, worstRead(g)); return <span className={l === 'not catching up' ? 'delta--bad' : 'secondary'} style={{ fontSize: 'var(--text-sm)', whiteSpace: 'nowrap' }}>{l}</span>; } },
        { key: 'members', header: 'Members', numeric: true, sortValue: (g) => g.members, render: (g) => g.members },
        { key: 'state', header: 'State', render: (g) => g.state === 'Stable' || isQueueKind(clusterOf(g.clusterId)?.kind ?? 'kafka') ? <span className="muted">{g.state === 'Empty' ? 'No consumers' : 'Stable'}</span> : <Badge tone={g.state === 'Rebalancing' ? 'warning' : 'neutral'}>{g.state}</Badge> },
      ]} />
    </Card>
  );
}

function TopicsTable({ rows, showCluster, onOpen }: { rows: Topic[]; showCluster?: boolean; onOpen: (t: Topic) => void }) {
  return (
    <Card flush>
      <Table rows={rows} rowKey={(t) => `${t.clusterId}/${t.name}`} onRowClick={onOpen} defaultSort={{ key: 'status', dir: 'desc' }}
        empty="No topics yet" columns={[
        { key: 'name', header: 'Topic / queue', sortValue: (t) => t.name, render: (t) => (
          <div style={{ whiteSpace: 'nowrap' }}>
            <div className="mono" style={{ fontWeight: 500 }}>{t.name}</div>
            <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{t.binding ?? (clusterOf(t.clusterId)?.kind === 'kafka' ? `${t.partitions} partitions · retention ${retentionLabel(t)}` : `retention ${retentionLabel(t)}`)}</div>
          </div>) },
        ...(showCluster ? [{ key: 'cluster', header: 'Cluster', render: (t: Topic) => <span className="secondary" style={{ whiteSpace: 'nowrap' }}>{clusterOf(t.clusterId)?.name}</span> }] : []),
        { key: 'status', header: 'Status', sortValue: (t) => RANK[topicHealth(t)] * 1e9 + topicLag(t), render: (t) => groupsReading(t).length ? <StatusBadge status={topicHealth(t)} label={topicHealth(t) === 'ok' ? 'Keeping up' : undefined} /> : <Badge>No readers</Badge> },
        { key: 'in', header: 'In', numeric: true, sortValue: (t) => t.inPerSec, render: (t) => <span>{t.inPerSec < 1 ? t.inPerSec.toFixed(2) : compact(t.inPerSec)} <span className="muted">msg/s</span></span> },
        { key: 'prod', header: 'Producers', render: (t) => <span className="secondary truncate" style={{ display: 'block', maxWidth: 200, fontSize: 'var(--text-sm)' }}>{t.producers.map(svcName).join(', ')}</span> },
        { key: 'groups', header: 'Read by', render: (t) => {
          const gs = groupsReading(t);
          return gs.length ? <span style={{ fontSize: 'var(--text-sm)' }}>{gs.length} <span className="muted">{gs.length === 1 ? 'group' : 'groups'}</span></span> : <span className="muted">—</span>;
        } },
        { key: 'lag', header: 'Lag (all readers)', numeric: true, sortValue: topicLag, render: (t) => compact(topicLag(t)) },
        { key: 'max', header: 'Most behind', render: (t) => {
          const gs = groupsReading(t).map((g) => ({ g, lag: readLag(g.reads.find((r) => r.topic === t.name)!) })).sort((a, b) => b.lag - a.lag);
          return gs[0] && gs[0].lag > 0 ? <span className="mono" style={{ fontSize: 'var(--text-xs)' }}>{gs[0].g.id} <span className="muted">{compact(gs[0].lag)}</span></span> : <span className="muted">—</span>;
        } },
      ]} />
    </Card>
  );
}

function Filters({ q, setQ, cluster, setCluster }: { q: string; setQ: (v: string) => void; cluster?: string; setCluster?: (v: string) => void }) {
  return (
    <div className="row" style={{ gap: 'var(--space-2)', marginBottom: 'var(--space-3)' }}>
      <div className="input-group" style={{ flex: 1, maxWidth: 360 }}>
        <Input type="search" placeholder="Filter by topic, group or service…" value={q} onChange={(e) => setQ(e.target.value)} style={{ width: '100%' }} />
      </div>
      {setCluster && <Select aria-label="Cluster" value={cluster} onChange={(e) => setCluster(e.target.value)}
        options={[{ value: '', label: 'All clusters' }, ...clusters.map((c) => ({ value: c.id, label: c.name }))]} />}
    </div>
  );
}
const matchGroup = (q: string) => (g: ConsumerGroup) => !q || [g.id, g.serviceId, g.process, g.external, ...g.reads.map((r) => r.topic)].some((s) => s?.toLowerCase().includes(q.toLowerCase()));
const matchTopic = (q: string) => (t: Topic) => !q || [t.name, ...t.producers, ...groupsReading(t).map((g) => g.id)].some((s) => s.toLowerCase().includes(q.toLowerCase()));

// =================================================================== list
type ListTab = 'groups' | 'topics' | 'alerts';

export function Queues() {
  useInfra();
  useData();
  const nav = useNavigate();
  const d = useQueueDrawers();
  const [tab, setTab] = useState<ListTab>('groups');
  const [q, setQ] = useState('');
  const [cluster, setCluster] = useState('');
  const [connect, setConnect] = useState(false);
  const inCluster = <T extends { clusterId: string }>(x: T) => !cluster || x.clusterId === cluster;

  return (
    <>
      <PageHeader title="Message queues" subtitle="Kafka, RabbitMQ, Amazon SQS and Google Pub/Sub — lag per topic, queue and subscription, per consumer"
        actions={<Button variant="primary" icon={<Plus size={14} />} onClick={() => setConnect(true)}>Connect broker</Button>} />

      <Section title="Clusters">
        <div className="grid grid-2">{clusters.map((c) => <ClusterCard key={c.id} c={c} onClick={() => nav(`/infrastructure/queues/${c.id}`)} />)}</div>
      </Section>

      <Section>
        <Tabs value={tab} onChange={setTab} tabs={[
          { id: 'groups', label: <>Consumers <Badge>{consumerGroups.filter(inCluster).length}</Badge></> },
          { id: 'topics', label: <>Topics, queues &amp; subscriptions <Badge>{topics.filter(inCluster).length}</Badge></> },
          { id: 'alerts', label: <>Lag alerts <AlertBadge clusterIds={cluster ? [cluster] : clusters.map((c) => c.id)} /></> },
        ]} />
        {tab !== 'alerts' && <Filters q={q} setQ={setQ} cluster={cluster} setCluster={setCluster} />}
        {tab === 'alerts' && <LagAlertList clusterId={cluster || undefined} />}
        {tab === 'groups' && <GroupsTable rows={consumerGroups.filter(inCluster).filter(matchGroup(q))} showCluster onOpen={d.openGroup} />}
        {tab === 'topics' && <TopicsTable rows={topics.filter(inCluster).filter(matchTopic(q))} showCluster onOpen={d.openTopic} />}
      </Section>

      {d.drawers}
      {connect && <ConnectBroker onClose={() => setConnect(false)} onDone={(id) => { setConnect(false); nav(`/infrastructure/queues/${id}`); }} />}
    </>
  );
}

function ClusterCard({ c, onClick }: { c: Cluster; onClick: () => void }) {
  const terms = brokerTerms(c.kind);
  const gs = consumerGroups.filter((g) => g.clusterId === c.id);
  const behind = gs.filter((g) => groupHealth(g) !== 'ok');
  const status = clusterHealth(c);
  const it = integrationFor(c);
  const oldest = oldestOf(gs);
  return (
    <Card onClick={onClick} className={status === 'critical' ? 'card--rail-critical' : status === 'warning' ? 'card--rail-warning' : undefined}
      title={<span className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>{c.name}<Badge tone="accent">{BROKER_LABEL[c.kind]}</Badge>{c.addedAt && <Badge>new</Badge>}{it && <IntegrationSourceBadge it={it} compact />}</span>}
      subtitle={`${c.version} · ${c.source}`} actions={<StatusBadge status={status} />}>
      <div className="grid grid-4" style={{ gap: 'var(--space-3)' }}>
        {hasNodes(c)
          ? <Fact label={c.kind === 'kafka' ? 'Brokers' : 'Nodes'} value={c.nodes.length} />
          : <Fact label="Oldest message" value={oldest === undefined ? '—' : ageLabel(oldest)} />}
        <Fact label={terms.topics} value={topics.filter((t) => t.clusterId === c.id).length} />
        <Fact label={terms.groups} value={gs.length} />
        <Fact label={c.kind === 'kafka' ? 'Total lag' : `${brokerTerms(c.kind).lag} msgs`} value={compact(gs.reduce((a, g) => a + groupLag(g), 0))} />
      </div>
      {behind.length > 0 && (
        <div className="secondary" style={{ fontSize: 'var(--text-sm)', marginTop: 'var(--space-3)' }}>
          Falling behind: {behind.map((g) => <span key={g.id} className="mono" style={{ marginRight: 'var(--space-2)' }}>{g.id}</span>)}
        </div>
      )}
      {c.nodes.filter((n) => n.status !== 'ok').map((n) => (
        <div key={n.id} className="secondary" style={{ fontSize: 'var(--text-sm)', marginTop: 'var(--space-1)' }}>{n.id}: {n.note?.split(' — ')[0]}</div>
      ))}
    </Card>
  );
}
function Fact({ label, value }: { label: string; value: ReactNode }) {
  return <div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{label}</div><div className="num" style={{ fontSize: 'var(--text-lg)', fontWeight: 600 }}>{value}</div></div>;
}

// =================================================================== cluster
type ClusterTab = 'groups' | 'topics' | 'nodes' | 'alerts';
const CLUSTER_TABS: ClusterTab[] = ['groups', 'topics', 'nodes', 'alerts'];

export function ClusterDetail() {
  useInfra();
  useData();
  const { clusterId = '' } = useParams();
  const { range } = useApp();
  const [params, setParams] = useSearchParams();
  const d = useQueueDrawers();
  const [q, setQ] = useState('');
  const c = clusterOf(clusterId);
  if (!c) return <EmptyState icon={<Waypoints />} title="Cluster not found" />;
  const raw = params.get('tab') as ClusterTab | null;
  const tab: ClusterTab = raw && CLUSTER_TABS.includes(raw) && (raw !== 'nodes' || hasNodes(c)) ? raw : 'groups';
  const integration = integrationFor(c);
  const setTab = (t: ClusterTab) => setParams(t === 'groups' ? {} : { tab: t }, { replace: true });
  const terms = brokerTerms(c.kind);
  const gs = consumerGroups.filter((g) => g.clusterId === c.id);
  const ts_ = topics.filter((t) => t.clusterId === c.id);
  const ts = timestamps(range);
  const fresh = !!c.addedAt && ts_.length === 0;

  return (
    <>
      <PageHeader title={c.name}
        crumbs={[{ label: 'Message queues', to: '/infrastructure/queues' }, { label: c.name }]}
        subtitle={<span className="row" style={{ gap: 12, flexWrap: 'wrap' }}>
          <StatusBadge status={clusterHealth(c)} /><Badge tone="accent">{BROKER_LABEL[c.kind]}</Badge>
          <span className="secondary">{c.version}</span>
          <span className="muted">{c.source}{c.vhost && <> · vhost {c.vhost}</>}</span>
          {integration && <IntegrationSourceBadge it={integration} />}
        </span>} />

      {fresh && <Callout icon={<Loader2 size={16} className="spin" />}>Connected. {terms.topics} and {terms.groups.toLowerCase()} appear after the first scrape (about a minute); broker metrics are already in.</Callout>}

      <Section>
        <Tabs value={tab} onChange={setTab} tabs={[
          { id: 'groups', label: <>{terms.groups} <Badge>{gs.length}</Badge></> },
          { id: 'topics', label: <>{terms.topics} <Badge>{ts_.length}</Badge></> },
          ...(hasNodes(c) ? [{ id: 'nodes' as const, label: <>{c.kind === 'kafka' ? 'Brokers' : 'Nodes'} <Badge>{c.nodes.length}</Badge></> }] : []),
          { id: 'alerts', label: <>Lag alerts <AlertBadge clusterIds={[c.id]} /></> },
        ]} />
        {(tab === 'groups' || tab === 'topics') && <Filters q={q} setQ={setQ} />}
        {tab === 'alerts' && <LagAlertList clusterId={c.id} />}
        {tab === 'groups' && <GroupsTable rows={gs.filter(matchGroup(q))} onOpen={d.openGroup} />}
        {tab === 'topics' && <TopicsTable rows={ts_.filter(matchTopic(q))} onOpen={d.openTopic} />}
        {tab === 'nodes' && hasNodes(c) && (
          <>
            <div className="grid grid-2">
              <Card title="CPU">
                <TimeSeries timestamps={ts} height={160} yMax={1} format={(v) => pct(v, 0)}
                  series={c.nodes.map((n, i) => ({ id: n.id, label: n.id, slot: LINE_SLOTS[i % LINE_SLOTS.length], data: brokerNodeSeries(c, n, 'cpu', range) }))} />
              </Card>
              <Card title={c.kind === 'kafka' ? 'Network in' : 'Memory'}>
                <TimeSeries timestamps={ts} height={160} format={(v) => (c.kind === 'kafka' ? `${v.toFixed(1)} MB/s` : pct(v, 0))}
                  series={c.nodes.map((n, i) => ({ id: n.id, label: n.id, slot: LINE_SLOTS[i % LINE_SLOTS.length], data: brokerNodeSeries(c, n, c.kind === 'kafka' ? 'net' : 'memory', range) }))} />
              </Card>
            </div>
            <Card flush>
              <Table rows={c.nodes} rowKey={(n) => n.id} columns={[
                { key: 'id', header: c.kind === 'kafka' ? 'Broker' : 'Node', render: (n) => <div><div className="mono">{n.id}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{n.host}</div></div> },
                { key: 'st', header: 'Status', render: (n) => <StatusBadge status={n.status} /> },
                { key: 'cpu', header: 'CPU', sortValue: (n) => n.cpu, render: (n) => <UsageCell v={n.cpu} /> },
                { key: 'mem', header: 'Memory', sortValue: (n) => n.memory, render: (n) => <UsageCell v={n.memory} /> },
                { key: 'disk', header: 'Disk', sortValue: (n) => n.disk, render: (n) => <UsageCell v={n.disk} /> },
                ...(c.kind === 'kafka' ? [
                  { key: 'lead', header: 'Leaders', numeric: true, render: (n: Cluster['nodes'][number]) => n.leaders ?? '—' },
                  { key: 'urp', header: 'Under-replicated', numeric: true, render: (n: Cluster['nodes'][number]) => n.underReplicated ?? '—' },
                ] : []),
              ]} />
            </Card>
            {c.nodes.filter((n) => n.note).map((n) => <Callout key={n.id} icon={<StatusBadge status={n.status} label={n.id} />}>{n.note}</Callout>)}
          </>
        )}
      </Section>
      {d.drawers}
    </>
  );
}
function AlertBadge({ clusterIds }: { clusterIds: string[] }) {
  const ms = clusterIds.flatMap((id) => lagMonitorsOf(id));
  const hot = ms.filter((m) => m.state !== 'ok').length;
  return hot ? <Badge tone="warning">{hot} pending</Badge> : <Badge>{ms.length}</Badge>;
}
function UsageCell({ v }: { v: number }) {
  return <div style={{ minWidth: 90 }}><div className="num" style={{ fontSize: 'var(--text-sm)' }}>{pct(v, 0)}</div><Meter value={v} tone={meterTone(v)} /></div>;
}

// =================================================================== drawers
function TopicDrawer({ t, onClose, onGroup }: { t: Topic; onClose: () => void; onGroup: (g: ConsumerGroup) => void }) {
  const { range } = useApp();
  const nav = useNavigate();
  const c = clusterOf(t.clusterId)!;
  const terms = brokerTerms(c.kind);
  const ts = timestamps(range);
  const gs = groupsReading(t);
  const readOf = (g: ConsumerGroup) => g.reads.find((r) => r.topic === t.name)!;
  return (
    <Drawer open onClose={onClose} width={720} title={<span className="mono">{t.name}</span>}
      subtitle={<span className="row" style={{ gap: 'var(--space-2)' }}><Badge tone="accent">{BROKER_LABEL[c.kind]} · {c.name}</Badge>{gs.length ? <StatusBadge status={topicHealth(t)} label={topicHealth(t) === 'ok' ? 'Keeping up' : undefined} /> : <Badge>No readers</Badge>}</span>}>
      <KeyValues items={[
        ...(c.kind === 'kafka' ? [['Partitions', t.partitions] as [string, number]] : [[c.kind === 'rabbitmq' ? 'Bound to' : 'Type', t.binding ?? '—'] as [string, string]]),
        ...(oldestOf(gs, t.name) !== undefined ? [['Oldest message', ageLabel(oldestOf(gs, t.name)!)] as [string, string]] : []),
        ['Retention', retentionLabel(t) === 'Unlimited' ? '∞ Unlimited' : retentionLabel(t)],
        ['In', `${t.inPerSec < 1 ? t.inPerSec.toFixed(2) : t.inPerSec.toFixed(1)} msg/s · ${((t.inPerSec * t.avgBytes) / 1e6).toFixed(2)} MB/s`],
        ['Avg message', `${compact(t.avgBytes)} B`],
        ['Producers', <span key="p">{t.producers.map((p, i) => {
          const s = services.find((x) => x.id === p);
          return <span key={p}>{i > 0 && ', '}{s ? <a href={`/services/${s.id}`} onClick={(e) => { e.preventDefault(); nav(`/services/${s.id}`); }}>{s.name}</a> : p}</span>;
        })}</span>],
      ]} />
      <DrawerSection title="Messages in / s">
        <TimeSeries timestamps={ts} height={120} area format={(v) => v.toFixed(1)} series={[{ id: 'in', label: 'msg/s', slot: 2, data: topicInSeries(t, range) }]} />
      </DrawerSection>
      {gs.length === 0
        ? <Callout icon={<Waypoints size={16} />}>Nobody reads this {terms.topic.toLowerCase()}. Messages {retentionLabel(t) === 'Unlimited' ? 'are kept forever and keep using disk' : c.kind === 'kafka' ? `are kept for ${retentionLabel(t)} and then deleted` : c.kind === 'rabbitmq' ? 'pile up until their TTL' : `pile up and are dropped after ${retentionLabel(t)}`} — check whether a consumer is missing, or stop producing.</Callout>
        : <>
            <DrawerSection title={`${terms.lag} by ${terms.group.toLowerCase()}`}>
              <TimeSeries timestamps={ts} height={160} format={(v) => compact(v)}
                series={gs.map((g, i) => ({ id: g.id, label: g.id, slot: LINE_SLOTS[i % LINE_SLOTS.length], data: lagSeries(g, readOf(g), range) }))} />
            </DrawerSection>
            <DrawerSection title={`Read by ${gs.length} ${gs.length === 1 ? terms.group.toLowerCase() : terms.groups.toLowerCase()}`}>
              <div className="card" style={{ overflow: 'hidden' }}>
                <Table rows={gs} rowKey={(g) => g.id} onRowClick={onGroup} columns={[
                  { key: 'g', header: terms.group, render: (g) => <div><div className="mono">{g.id}</div><div style={{ fontSize: 'var(--text-xs)' }}><Consumer g={g} /></div></div> },
                  { key: 's', header: 'Status', render: (g) => <StatusBadge status={readHealth(g, readOf(g))} label={readHealth(g, readOf(g)) === 'ok' ? 'Keeping up' : undefined} /> },
                  { key: 'lag', header: terms.lag, numeric: true, render: (g) => compact(readLag(readOf(g))) },
                  { key: 'c', header: 'Catch-up', render: (g) => <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>{catchUpLabel(g, readOf(g))}</span> },
                  { key: 'go', header: '', width: 28, render: () => <ArrowRight size={14} className="muted" aria-hidden /> },
                ]} />
              </div>
            </DrawerSection>
          </>}
    </Drawer>
  );
}

function GroupDrawer({ g, onClose, onTopic }: { g: ConsumerGroup; onClose: () => void; onTopic: (t: Topic) => void }) {
  const nav = useNavigate();
  const c = clusterOf(g.clusterId)!;
  const terms = brokerTerms(c.kind);
  return (
    <Drawer open onClose={onClose} width={760} title={<span className="mono">{g.id}</span>}
      subtitle={<span className="row" style={{ gap: 'var(--space-2)' }}><Badge tone="accent">{BROKER_LABEL[c.kind]} · {c.name}</Badge><StatusBadge status={groupHealth(g)} label={groupHealth(g) === 'ok' ? 'Keeping up' : undefined} />{g.state !== 'Stable' && !isQueueKind(c.kind) && <Badge tone={g.state === 'Rebalancing' ? 'warning' : 'neutral'}>{g.state}</Badge>}</span>}
      actions={g.serviceId && <Button size="sm" onClick={() => nav(`/services/${g.serviceId}?tab=lag`)}>Open {svcName(g.serviceId)} <ArrowRight size={14} /></Button>}>
      <KeyValues items={[
        ['Consumer', <Consumer key="c" g={g} />],
        [c.kind === 'kafka' ? 'Members' : c.kind === 'sqs' ? 'Pollers / concurrency' : 'Consumers', g.members],
        ...(isQueueKind(c.kind)
          ? (oldestOf([g]) !== undefined ? [['Oldest message', ageLabel(oldestOf([g])!)] as [string, string]] : [])
          : [['State', `${g.state}${g.rebalancesPerHour ? ` · ${g.rebalancesPerHour} rebalances in the last hour` : ''}`] as [string, string]]),
        [`Total ${terms.lag.toLowerCase()}`, `${compact(groupLag(g))} ${terms.unit}`],
      ]} />
      <DrawerSection title="Lag alerts">
        <LagAlertList clusterId={g.clusterId} group={g.id} compact />
      </DrawerSection>
      {g.reads.map((r, i) => {
        const t = topicOf(g.clusterId, r.topic);
        return (
          <DrawerSection key={r.topic}
            title={<span className="row" style={{ gap: 'var(--space-2)' }}>
              {t ? <a href="#" className="mono" onClick={(e) => { e.preventDefault(); onTopic(t); }}>{r.topic}</a> : <span className="mono">{r.topic}</span>}
              <StatusBadge status={readHealth(g, r)} label={readHealth(g, r) === 'ok' ? 'Keeping up' : undefined} />
            </span>}
            aside={<span><strong className="num">{compact(readLag(r))}</strong> <span className="muted">{terms.unit}</span></span>}>
            <ReadLag g={g} r={r} slot={LINE_SLOTS[i % LINE_SLOTS.length]} />
          </DrawerSection>
        );
      })}
    </Drawer>
  );
}

// =================================================================== connect
// SQS and Pub/Sub are connected through a cloud integration (Settings → Integrations), not here.
type DirectKind = 'kafka' | 'rabbitmq';
const KINDS: { id: DirectKind; label: string }[] = [{ id: 'kafka', label: 'Kafka' }, { id: 'rabbitmq', label: 'RabbitMQ' }];
const PROVIDERS: Record<DirectKind, string[]> = {
  kafka: ['Self-managed', 'Amazon MSK', 'Confluent Cloud', 'Aiven', 'Redpanda'],
  rabbitmq: ['Self-managed', 'Amazon MQ', 'CloudAMQP'],
};
const PERMS: Record<DirectKind, string> = {
  kafka: "# read-only: describe cluster, topics and groups — never consumes your messages\nkafka-acls --bootstrap-server … --add --allow-principal User:trooper \\\n  --operation Describe --operation DescribeConfigs \\\n  --cluster --topic '*' --group '*'",
  rabbitmq: "rabbitmqctl add_user trooper '…'\nrabbitmqctl set_user_tags trooper monitoring\n# no configure / write / read rights on any vhost\nrabbitmqctl set_permissions -p /shop trooper '^$' '^$' '^$'",
};

function ConnectBroker({ onClose, onDone }: { onClose: () => void; onDone: (id: string) => void }) {
  const [kind, setKind] = useState<DirectKind>('kafka');
  const [name, setName] = useState('');
  const [provider, setProvider] = useState(PROVIDERS.kafka[0]);
  const [endpoint, setEndpoint] = useState('');
  const [vhost, setVhost] = useState('/');
  const [security, setSecurity] = useState('SASL_SSL · SCRAM-SHA-512');
  const [test, setTest] = useState<'idle' | 'running' | string>('idle');
  const kafka = kind === 'kafka';
  const ready = name.trim() && endpoint.trim();
  const pick = (k: DirectKind) => { setKind(k); setProvider(PROVIDERS[k][0]); setEndpoint(''); setTest('idle'); };
  const run = async () => { setTest('running'); setTest(await testConnection(kind, undefined, endpoint)); };

  return (
    <Drawer open onClose={onClose} width={600} title="Connect broker"
      subtitle="Read-only: offsets and metadata only — TaskTrooper never consumes or publishes messages."
      actions={<Button variant="primary" disabled={!ready} onClick={() => onDone(addCluster({ kind, name, endpoint, provider, vhost }))}>Connect</Button>}>
      <div className="stack">
        <CloudQueuesNote />
        <Field label="Broker"><Segmented label="Broker" options={KINDS} value={kind} onChange={pick} /></Field>
        <div className="grid grid-2">
          <Field label="Name"><Input value={name} onChange={(e) => setName(e.target.value)} placeholder={kafka ? 'kafka-events' : 'rabbit-jobs'} /></Field>
          <Field label="Hosted on"><Select value={provider} onChange={(e) => setProvider(e.target.value)} options={PROVIDERS[kind].map((p) => ({ value: p, label: p }))} /></Field>
        </div>
        {kafka ? <>
          <Field label="Bootstrap servers" hint="Comma-separated host:port; one reachable broker is enough to discover the rest.">
            <Input mono value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder="b-1.kafka.internal:9096,b-2.kafka.internal:9096" />
          </Field>
          <Field label="Security">
            <Select value={security} onChange={(e) => setSecurity(e.target.value)}
              options={['SASL_SSL · SCRAM-SHA-512', 'SASL_SSL · PLAIN', 'SASL_SSL · AWS IAM', 'SSL · mTLS', 'PLAINTEXT'].map((v) => ({ value: v, label: v }))} />
          </Field>
        </> : <>
          <div className="grid" style={{ gridTemplateColumns: '1fr 140px' }}>
            <Field label="Management API URL" hint="RabbitMQ management plugin (port 15672 by default)."><Input mono value={endpoint} onChange={(e) => setEndpoint(e.target.value)} placeholder="https://rabbit.internal:15672" /></Field>
            <Field label="Virtual host"><Input mono value={vhost} onChange={(e) => setVhost(e.target.value)} /></Field>
          </div>
        </>}
        {(!kafka || !security.includes('mTLS') && !security.includes('IAM') && security !== 'PLAINTEXT') && (
          <div className="grid grid-2">
            <Field label="User"><Input mono defaultValue="trooper" /></Field>
            <Field label="Password" hint="Stored encrypted; only the collector can read it."><Input type="password" autoComplete="new-password" /></Field>
          </div>
        )}
        <Field label="Permissions it needs"><CodeBlock>{PERMS[kind]}</CodeBlock></Field>
        <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>
          {kafka
            ? 'Every 15 s TaskTrooper reads topic end offsets and each group\'s committed offsets — lag per partition is the difference. Groups are matched to services by the group.id their SDK reports.'
            : 'Every 15 s TaskTrooper reads each queue\'s ready / unacked counts and publish / deliver rates. Consumers are matched to services by the consumer tag their SDK reports.'}
        </p>
        <div className="row" style={{ gap: 'var(--space-3)' }}>
          <Button onClick={run} disabled={test === 'running' || !endpoint.trim()} icon={test === 'running' ? <Loader2 size={14} className="spin" /> : undefined}>Test connection</Button>
          {test !== 'idle' && test !== 'running' && <span className="row" style={{ gap: 6, color: 'var(--status-good-text)', fontSize: 'var(--text-sm)' }}><CheckCircle2 size={14} aria-hidden />{test}</span>}
        </div>
      </div>
    </Drawer>
  );
}

/** SQS and Pub/Sub have no broker to connect: their queues come from the AWS / Google Cloud integration. */
function CloudQueuesNote() {
  const nav = useNavigate();
  const cloud = integrations.filter((i) => i.provider === 'aws' || i.provider === 'gcp');
  return (
    <Card title={<span className="row" style={{ gap: 'var(--space-2)' }}><Cloud size={14} aria-hidden />Amazon SQS or Google Pub/Sub?</span>}
      actions={<Button size="sm" icon={<Plug size={14} />} onClick={() => nav('/settings/integrations')}>Open integrations</Button>}>
      <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>
        They come from a cloud integration: queues and subscriptions are discovered with their depth, oldest message age and consumers. Amazon MSK clusters too.
      </p>
      {cloud.length > 0 && (
        <div className="stack" style={{ gap: 'var(--space-2)', marginTop: 'var(--space-3)' }}>
          {cloud.map((i) => (
            <div key={i.id} className="row" style={{ gap: 'var(--space-2)', fontSize: 'var(--text-sm)', flexWrap: 'wrap' }}>
              <ProviderMark provider={i.provider} size="sm" /><span style={{ fontWeight: 500 }}>{i.name}</span><IntegrationStatusBadge status={i.status} />
            </div>
          ))}
        </div>
      )}
    </Card>
  );
}
