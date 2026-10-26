import { useNavigate } from 'react-router';
import { AlertOctagon, AlertTriangle, ArrowRight, Inbox, Info } from 'lucide-react';
import { PartitionBars, TimeSeries } from '@/components/charts';
import { Badge, Button, Card, EmptyState, KeyValues, Section, StatusBadge } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import {
  BROKER_LABEL, brokerTerms, catchUp, clusterOf, groupsOfService, lagSeries, readHealth, readInsights, readLag, timestamps, topicOf,
  type ConsumerGroup, type GroupRead,
} from '@/data';
import { compact } from '@/lib/format';
import { LINE_SLOTS } from './shared';
import { LagAlertButton } from './LagAlerts';

const span = (s: number) => (s < 90 ? `${Math.max(1, Math.round(s))} s` : s < 5400 ? `${Math.round(s / 60)} min` : `${(s / 3600).toFixed(1)} h`);

/** Age of a waiting message, e.g. "9 min". */
export const ageLabel = (s: number) => span(s);

/** Growing → "not catching up"; draining → time to zero; keeping pace → how far behind real time it reads. */
export function catchUpLabel(g: ConsumerGroup, r: GroupRead): string {
  const s = catchUp(g, r);
  if (g.trend === 'batch') return 'drains hourly';
  if (s === 0) return 'caught up';
  if (g.trend === 'growing') return 'not catching up';
  if (Number.isFinite(s)) return `drains in ${span(s)}`;
  if (r.oldestAgeS !== undefined) return `oldest ${span(r.oldestAgeS)}`;
  const t = topicOf(g.clusterId, r.topic);
  return t && t.inPerSec > 0 ? `≈ ${span(readLag(r) / t.inPerSec)} behind` : 'steady';
}

const INSIGHT_ICON = { critical: AlertOctagon, warning: AlertTriangle, info: Info } as const;
const INSIGHT_COLOR = { critical: 'var(--status-critical-text)', warning: 'var(--status-warning-text)', info: 'var(--text-muted)' } as const;

export function Insights({ g, r }: { g: ConsumerGroup; r: GroupRead }) {
  const list = readInsights(g, r);
  if (!g.note && list.length === 0) return null;
  return (
    <ul style={{ listStyle: 'none', margin: 0, padding: 0, display: 'flex', flexDirection: 'column', gap: 'var(--space-2)', fontSize: 'var(--text-sm)' }}>
      {g.note && <li className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-start' }}><Info size={14} style={{ color: 'var(--accent-text)', flexShrink: 0, marginTop: 3 }} aria-hidden /><span>{g.note}</span></li>}
      {list.map((x, i) => {
        const Icon = INSIGHT_ICON[x.tone];
        return <li key={i} className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-start' }}><Icon size={14} style={{ color: INSIGHT_COLOR[x.tone], flexShrink: 0, marginTop: 3 }} aria-label={x.tone} /><span className="secondary">{x.text}</span></li>;
      })}
    </ul>
  );
}

/** One (consumer group, topic): lag over time, lag per partition, rates and what to do. */
export function ReadLag({ g, r, slot = 1 }: { g: ConsumerGroup; r: GroupRead; slot?: number }) {
  const { range } = useApp();
  const ts = timestamps(range);
  const c = clusterOf(g.clusterId)!;
  const t = topicOf(g.clusterId, r.topic);
  const terms = brokerTerms(c.kind);
  const kafka = c.kind === 'kafka';
  return (
    <div className="stack">
      <div className="grid" style={{ gridTemplateColumns: kafka && r.partitionLag.length > 1 ? 'minmax(0, 3fr) minmax(0, 2fr)' : '1fr', alignItems: 'start' }}>
        <div>
          <div className="eyebrow" style={{ marginBottom: 'var(--space-1)' }}>{terms.lag} over time</div>
          <TimeSeries timestamps={ts} height={130} area format={(v) => compact(v)}
            series={[{ id: 'lag', label: terms.lag, slot, data: lagSeries(g, r, range) }]} />
        </div>
        {kafka && r.partitionLag.length > 1 && (
          <div>
            <div className="eyebrow" style={{ marginBottom: 'var(--space-1)' }}>Lag per partition · now</div>
            <PartitionBars values={r.partitionLag} height={112} />
          </div>
        )}
      </div>
      <KeyValues items={[
        [kafka ? 'Producers write' : c.kind === 'sqs' ? 'Sent' : 'Published', t ? `${t.inPerSec.toFixed(1)} msg/s` : '—'],
        [kafka ? 'Group consumes' : c.kind === 'sqs' ? 'Deleted (processed)' : c.kind === 'pubsub' ? 'Acked' : 'Delivered', `${r.consumePerSec.toFixed(1)} msg/s`],
        ...(r.oldestAgeS !== undefined ? [[c.kind === 'pubsub' ? 'Oldest unacked message' : 'Oldest message', span(r.oldestAgeS)] as [string, string]] : []),
        ...(r.unacked !== undefined ? [[c.kind === 'sqs' ? 'In flight (not visible)' : 'Unacked (in flight)', compact(r.unacked)] as [string, string]] : []),
        ['Catch-up', catchUpLabel(g, r)],
      ]} />
      <Insights g={g} r={r} />
    </div>
  );
}

/** Consumer service tab: every topic/queue it reads, per consumer group, with lag. */
export function ServiceLag({ serviceId }: { serviceId: string }) {
  const { range } = useApp();
  const nav = useNavigate();
  const groups = groupsOfService(serviceId);
  const reads = groups.flatMap((g) => g.reads.map((r) => ({ g, r })));
  if (reads.length === 0) {
    return <Card><EmptyState icon={<Inbox />} title="No broker reads">This service reads nothing from a connected Kafka, RabbitMQ, SQS or Pub/Sub source. Connect the broker under Infrastructure → Message queues, or SQS / Pub/Sub through Settings → Integrations.</EmptyState></Card>;
  }
  const ts = timestamps(range);
  const slotOf = (i: number) => LINE_SLOTS[i % LINE_SLOTS.length];
  const total = reads.reduce((a, x) => a + readLag(x.r), 0);

  return (
    <>
      <Card title="Lag by topic and group" subtitle={`${compact(total)} messages behind across ${reads.length} subscription${reads.length > 1 ? 's' : ''}`}>
        <TimeSeries timestamps={ts} height={200} format={(v) => compact(v)}
          series={reads.map((x, i) => ({ id: `${x.g.id}-${x.r.topic}`, label: `${x.r.topic} · ${x.g.id}`, slot: slotOf(i), data: lagSeries(x.g, x.r, range) }))} />
      </Card>
      <Section>
        {reads.map(({ g, r }, i) => {
          const c = clusterOf(g.clusterId)!;
          const terms = brokerTerms(c.kind);
          const health = readHealth(g, r);
          return (
            <Card key={`${g.id}-${r.topic}`} className={health === 'critical' ? 'card--rail-critical' : health === 'warning' ? 'card--rail-warning' : undefined}
              title={<span className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
                <span className="mono">{r.topic}</span>
                <span className="muted" style={{ fontWeight: 400 }}>{terms.group.toLowerCase()}</span>
                <span className="mono" style={{ fontWeight: 400 }}>{g.id}</span>
                <StatusBadge status={health} label={health === 'ok' ? 'Keeping up' : undefined} />
              </span>}
              subtitle={<>{g.process && <>process {g.process} · </>}{g.members} {c.kind === 'kafka' ? 'members' : 'consumers'}{c.kind === 'kafka' ? <> · {g.state} · {r.partitionLag.length} partitions</> : r.oldestAgeS !== undefined ? <> · oldest message {span(r.oldestAgeS)}</> : null}</>}
              actions={<>
                <span className="row" style={{ gap: 'var(--space-2)', marginRight: 'var(--space-2)' }}>
                  <strong className="num" style={{ fontSize: 'var(--text-lg)' }}>{compact(readLag(r))}</strong>
                  <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>{terms.unit}</span>
                </span>
                <Badge>{BROKER_LABEL[c.kind]} · {c.name}</Badge>
                <LagAlertButton scope={{ clusterId: c.id, group: g.id, topic: g.reads.length > 1 ? r.topic : undefined }} />
                <Button size="sm" variant="ghost" onClick={() => nav(`/infrastructure/queues/${c.id}?group=${encodeURIComponent(`${c.id}/${g.id}`)}`)}>Open in {c.name} <ArrowRight size={14} /></Button>
              </>}>
              <ReadLag g={g} r={r} slot={slotOf(i)} />
            </Card>
          );
        })}
      </Section>
    </>
  );
}
