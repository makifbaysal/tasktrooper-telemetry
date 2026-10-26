import { useState } from 'react';
import { Bell, Plus, Trash2 } from 'lucide-react';
import { BarList, Columns, Sparkline, TimeSeries } from '@/components/charts';
import { PropBar } from '@/components/charts/BarList';
import {
  Badge, Button, Callout, Card, ChoiceChips, CodeBlock, CopyCode, Delta, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, Meter, MetricCard,
  PageHeader, SearchInput, Section, Segmented, Select, StatTile, StatusBadge, Table, Tabs, Toggle, useDrawer,
} from '@/components/ui';
import { series, timestamps } from '@/data';
import { compact, ms } from '@/lib/format';

const swatch = (name: string, note?: string) => (
  <div key={name} className="stack" style={{ gap: 4 }}>
    <div style={{ height: 40, borderRadius: 'var(--radius-sm)', background: `var(${name})`, border: '1px solid var(--border)' }} />
    <code style={{ fontSize: 'var(--text-xs)' }}>{name}</code>
    {note && <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>{note}</span>}
  </div>
);

/** One specimen card. */
function Spec({ title, children }: { title: string; children: React.ReactNode }) {
  return <Card title={title}><div className="stack">{children}</div></Card>;
}

export function DesignSystem() {
  const [tab, setTab] = useState<'a' | 'b' | 'c'>('a');
  const [seg, setSeg] = useState<'1h' | '24h' | '7d'>('1h');
  const [on, setOn] = useState(true);
  const [chips, setChips] = useState<string[]>(['rds', 'sqs']);
  const ts = timestamps('1h');
  const drawer = useDrawer<string>();

  return (
    <>
      <PageHeader title="Design system" subtitle="Tokens and components. Source: src/styles/tokens.css · docs: .ai/design-system.md" />

      <Section title="Foundations" subtitle="Color and type tokens">
      <Spec title="Surfaces & ink">
        <div className="grid grid-4">
          {swatch('--bg-page')}{swatch('--bg-surface', 'cards, charts')}{swatch('--bg-raised', 'inputs, popovers')}{swatch('--bg-sunken', 'wells, code')}
          {swatch('--text-primary')}{swatch('--text-secondary')}{swatch('--text-muted')}{swatch('--accent', 'brand / primary action')}
        </div>
      </Spec>

      <Spec title="Status — reserved for state, always with icon + label">
        <div className="grid grid-4">{swatch('--status-good')}{swatch('--status-warning')}{swatch('--status-serious')}{swatch('--status-critical')}</div>
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <StatusBadge status="ok" /><StatusBadge status="warning" /><StatusBadge status="pending" /><StatusBadge status="acknowledged" />
          <StatusBadge status="critical" /><StatusBadge status="firing" /><StatusBadge status="muted" /><StatusBadge status="info" />
        </div>
      </Spec>

      <Spec title="Data viz — categorical, fixed order (never cycled)">
        <div className="grid grid-4">
          {['blue', 'orange', 'aqua', 'yellow', 'magenta', 'green', 'violet', 'red'].map((n, i) => swatch(`--series-${i + 1}`, n))}
        </div>
      </Spec>

      <Spec title="Typography">
        <div className="hero">2,418</div>
        <div style={{ fontSize: 'var(--text-2xl)', fontWeight: 600 }}>Stat value · 24</div>
        <h1>Page title · 18 semibold</h1>
        <h2>Section title · 15 semibold</h2>
        <p>Body · 13 regular. Dense data UI default — tables, forms and paragraphs.</p>
        <p className="secondary" style={{ fontSize: 'var(--text-sm)' }}>Small · 12 secondary</p>
        <code>Mono · POST /v1/orders/:id</code>
      </Spec>
      </Section>

      <Section title="Controls">

      <Spec title="Buttons">
        <div className="row" style={{ flexWrap: 'wrap' }}>
          <Button variant="primary" icon={<Plus size={14} />}>Primary</Button>
          <Button>Secondary</Button>
          <Button variant="ghost">Ghost</Button>
          <Button variant="danger" icon={<Trash2 size={14} />}>Danger</Button>
          <Button disabled>Disabled</Button>
          <Button size="sm">Small</Button>
          <Button iconOnly aria-label="Alert" icon={<Bell size={14} />} />
        </div>
      </Spec>

      <Spec title="Inputs & controls">
        <div className="grid grid-3">
          <Field label="Text" hint="Helper text"><Input placeholder="orders-api" /></Field>
          <Field label="Select"><Select options={[{ value: 'p', label: 'production' }, { value: 's', label: 'staging' }]} /></Field>
          <Field label="Search"><SearchInput placeholder="Search…" /></Field>
        </div>
        <div className="row" style={{ flexWrap: 'wrap', gap: 16 }}>
          <Segmented label="demo" value={seg} onChange={setSeg} options={[{ id: '1h', label: '1h' }, { id: '24h', label: '24h' }, { id: '7d', label: '7d' }]} />
          <span className="row">Toggle <Toggle label="demo" checked={on} onChange={setOn} /></span>
          <ChoiceChips label="Services" value={chips} onChange={setChips} options={[{ id: 'rds', label: 'RDS' }, { id: 'elasticache', label: 'ElastiCache' }, { id: 'msk', label: 'MSK' }, { id: 'sqs', label: 'SQS' }]} />
          <Badge>Neutral</Badge><Badge tone="accent">Accent</Badge><span className="kbd">⌘K</span>
        </div>
        <Tabs value={tab} onChange={setTab} tabs={[{ id: 'a', label: 'Overview' }, { id: 'b', label: 'Endpoints' }, { id: 'c', label: 'Deploys' }]} />
        <div style={{ maxWidth: 320 }} className="stack"><Meter value={0.42} /><Meter value={0.71} tone="warning" /><Meter value={0.93} tone="critical" /></div>
        <Callout icon={<Bell size={16} />}>Callout — contextual hint or suggestion.</Callout>
      </Spec>
      </Section>

      <Section title="Page composition" subtitle="Order: what's broken → primary metrics → breakdowns → secondary. Each fact appears once.">
        <div className="grid grid-3">
          <MetricCard className="metric--hero span-2" label="Throughput · hero (span-2)" value="18.4K" unit="rpm" curr={18400} prev={17100}
            hint="The page's most important metric gets the most space. Value, delta and chart live in one card.">
            <TimeSeries timestamps={ts} height={160} area format={(v) => compact(v)} series={[{ id: 't', label: 'rpm', slot: 1, data: series('dsh', '1h', { base: 18000 }) }]} />
          </MetricCard>
          <MetricCard label="Error rate · MetricCard" value="0.42%" curr={0.0042} prev={0.0031} upIsGood={false} badge={<StatusBadge status="warning" />} onClick={() => drawer.open('Error rate')}
            hint="Clickable → opens a Drawer">
            <TimeSeries timestamps={ts} height={110} format={(v) => `${(v * 100).toFixed(1)}%`} series={[{ id: 'e', label: 'Error rate', slot: 8, data: series('dse', '1h', { base: 0.004, spikeAt: 0.8 }) }]} />
          </MetricCard>
        </div>
        <div className="grid grid-2">
          <Spec title="Delta — color = direction × whether up is good">
            <div className="row" style={{ flexWrap: 'wrap', gap: 'var(--space-4)' }}>
              <Delta curr={120} prev={100} />
              <Delta curr={80} prev={100} />
              <Delta curr={0.03} prev={0.01} upIsGood={false} />
              <Delta curr={100} prev={100} />
            </div>
          </Spec>
          <Spec title="Drawer — details on click, page stays in place">
            <p className="secondary" style={{ margin: 0 }}>Rows and cards with <code>onClick</code> show a chevron and hover state, then open a right-side Drawer built from DrawerSection + KeyValues.</p>
            <div><Button onClick={() => drawer.open('POST /v1/orders')}>Open sample drawer</Button></div>
          </Spec>
        </div>
      </Section>

      <Section title="Data display">

      <div className="grid grid-4">
        <StatTile label="Throughput" value="18.4K" unit="rpm" curr={18400} prev={17100} trend={series('ds1', '1h', { base: 10 }).slice(-16)} />
        <StatTile label="Error rate" value="0.42%" curr={0.0042} prev={0.0031} upIsGood={false} />
        <StatTile label="p95" value="182" unit="ms" curr={182} prev={182} />
        <Card title="Sparkline"><Sparkline data={series('ds2', '1h', { base: 10 }).slice(-20)} width={160} height={40} /></Card>
      </div>

      <div className="grid grid-2">
        <Card title="TimeSeries — line, ≥2 series → legend">
          <TimeSeries timestamps={ts} format={(v) => compact(v)} series={[
            { id: 'a', label: 'catalog-api', slot: 1, data: series('dsa', '1h', { base: 4000 }) },
            { id: 'b', label: 'orders-api', slot: 2, data: series('dsb', '1h', { base: 2400 }) },
            { id: 'c', label: 'payments-api', slot: 3, data: series('dsc', '1h', { base: 900 }) },
          ]} />
        </Card>
        <Card title="TimeSeries — area + threshold + event">
          <TimeSeries timestamps={ts} area format={ms} threshold={{ value: 300, label: 'alert 300 ms' }} events={[{ ts: ts[40], label: 'deploy' }]}
            series={[{ id: 'p95', label: 'p95', data: series('dsp', '1h', { base: 210, spikeAt: 0.75 }) }]} />
        </Card>
        <Card title="Columns — stacked, 2px gap, rounded data-end">
          <Columns timestamps={ts.slice(-30)} format={(v) => compact(v)} series={[
            { id: 'ok', label: '2xx', data: series('c1', '1h', { base: 900 }).slice(-30), color: 'var(--series-1)' },
            { id: '4', label: '4xx', data: series('c2', '1h', { base: 80 }).slice(-30), color: 'var(--series-4)' },
            { id: '5', label: '5xx', data: series('c3', '1h', { base: 40, spikeAt: 0.8 }).slice(-30), color: 'var(--series-8)' },
          ]} />
        </Card>
        <Card title="BarList & PropBar">
          <BarList items={[['/p/:slug', 61300], ['/', 48200], ['/search', 29800], ['/cart', 12400]].map(([l, v]) => ({ id: String(l), label: String(l), value: Number(v), display: compact(Number(v)) }))} />
          <div style={{ marginTop: 20 }}>
            <PropBar segments={[{ label: 'Mobile', value: 58, color: 'var(--series-1)' }, { label: 'Desktop', value: 36, color: 'var(--series-2)' }, { label: 'Tablet', value: 6, color: 'var(--series-3)' }]} />
          </div>
        </Card>
      </div>

      <div className="grid grid-2">
        <Card title="Table" flush>
          <Table rowKey={(r) => r.n} defaultSort={{ key: 'rpm', dir: 'desc' }} rows={[{ n: 'GET /v1/items', rpm: 12000, p95: 40 }, { n: 'POST /v1/orders', rpm: 2100, p95: 240 }, { n: 'GET /v1/search', rpm: 9000, p95: 120 }]} columns={[
            { key: 'n', header: 'Endpoint', render: (r) => r.n },
            { key: 'rpm', header: 'rpm', numeric: true, sortValue: (r) => r.rpm, render: (r) => compact(r.rpm) },
            { key: 'p95', header: 'p95', numeric: true, sortValue: (r) => r.p95, render: (r) => ms(r.p95) },
          ]} />
        </Card>
        <Card title="Empty state & code">
          <EmptyState icon={<Bell size={18} />} title="No monitors yet">Connect a repo and we will suggest them.</EmptyState>
          <CodeBlock>{`init({ dsn, service: 'orders-api' })`}</CodeBlock>
          <CopyCode>{`{ "Effect": "Allow", "Action": ["cloudwatch:GetMetricData"], "Resource": "*" }`}</CopyCode>
          <CopyCode wrap>{'https://ingest.trooper.dev/v1/drains/vercel?key=<backend ingest key>&integration=int-vercel'}</CopyCode>
        </Card>
      </div>
      </Section>

      <Drawer open={!!drawer.item} onClose={drawer.close} title={<span className="mono">{drawer.item}</span>} subtitle="orders-api · sample drawer"
        actions={<Button size="sm">Open full page</Button>}>
        <DrawerSection title="Traffic" aside="last hour">
          <TimeSeries timestamps={ts} height={140} area format={(v) => compact(v)} series={[{ id: 't', label: 'rpm', slot: 1, data: series('dsd', '1h', { base: 2100 }) }]} />
        </DrawerSection>
        <DrawerSection title="Details">
          <KeyValues items={[['Service', 'orders-api'], ['Owner', 'orders team'], ['Monitor', <StatusBadge key="m" status="ok" />], ['Source', <span key="s" className="mono">src/orders/create.controller.ts:42</span>]]} />
        </DrawerSection>
      </Drawer>
    </>
  );
}
