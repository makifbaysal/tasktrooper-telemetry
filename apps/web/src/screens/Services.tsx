import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { ChevronRight, Plus } from 'lucide-react';
import { Sparkline, TimeSeries } from '@/components/charts';
import { Button, Card, PageHeader, Section, SearchInput, StatusBadge, Table } from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { appSummary, serviceErrorSeries, serviceTrafficSeries, services, timestamps, type Service, type ServiceType as SType } from '@/data';
import { change, compact, ms, pct } from '@/lib/format';
import { SERVICE_CATEGORIES, ServiceType, TYPE_META } from './shared';
import { EbpfBadge } from './k8sShared';

const GROUPS: { id: 'backend' | 'async' | 'client'; title: string; types: SType[] }[] = [
  { id: 'backend', title: 'HTTP services', types: ['http'] },
  { id: 'async', title: 'Workers & consumers', types: ['worker', 'consumer'] },
  { id: 'client', title: 'Web & mobile apps', types: ['web', 'mobile'] },
];

const unitOf = (s: Service) => (s.type === 'http' ? 'rpm' : s.type === 'worker' || s.type === 'consumer' ? 'msg/min' : 'views/min');

/** The single biggest reason a service is not healthy — explains WHY, the table holds the numbers. */
function reason(s: Service): string {
  const candidates: { score: number; text: string }[] = [];
  const er = change(s.errorRate, s.prevErrorRate);
  candidates.push({ score: er.ratio * (s.errorRate >= 0.005 ? 2 : 0.5), text: `Error rate up ${er.label} to ${pct(s.errorRate)}` });
  const p = change(s.p95, s.prevP95);
  candidates.push({ score: p.ratio, text: `p95 latency up ${p.label} to ${ms(s.p95)}` });
  if (s.lag !== undefined && s.lag > 10_000) candidates.push({ score: 1.5, text: `Consumer lag ${compact(s.lag)} messages and growing` });
  return candidates.sort((a, b) => b.score - a.score)[0].text;
}

export function Services() {
  const nav = useNavigate();
  const { range } = useApp();
  const [params] = useSearchParams();
  // legacy ?type=client|backend|async links keep working
  const LEGACY: Record<string, SType[]> = { client: ['web', 'mobile'], backend: ['http'], async: ['worker', 'consumer'] };
  const typeParam = params.get('type') ?? '';
  const cat = SERVICE_CATEGORIES.find((c) => c.id === typeParam);
  const types: SType[] | null = cat?.types ?? LEGACY[typeParam] ?? null;
  const inScope = (s: Service) => !types || types.includes(s.type);
  const scoped = services.filter(inScope);
  const noun = cat ? (cat.id === 'api' ? 'APIs' : cat.label.toLowerCase()) : 'services';
  const [q, setQ] = useState('');
  const open = (s: Service) => nav(`/services/${s.id}`);

  const attention = scoped
    .filter((s) => s.status !== 'ok')
    .sort((a, b) => (a.status === 'critical' ? -1 : 0) - (b.status === 'critical' ? -1 : 0));
  const match = (s: Service) => s.name.includes(q.toLowerCase());
  const groups = GROUPS
    .map((g) => ({ ...g, rows: scoped.filter((s) => g.types.includes(s.type) && match(s)) }))
    .filter((g) => g.rows.length > 0);

  return (
    <>
      <PageHeader title={cat?.label ?? 'Services'}
        crumbs={cat ? [{ label: 'Services', to: '/services' }, { label: cat.label }] : undefined}
        subtitle={`${scoped.length} ${noun} reporting through a Trooper SDK`}
        actions={<Button variant="primary" icon={<Plus size={14} />} onClick={() => nav('/settings')}>Add service</Button>} />

      {attention.length > 0 && (
        <Section title="Needs attention" subtitle={`${attention.length} of ${scoped.length} are degraded`}>
          <div className="grid grid-3">
            {attention.map((s) => (
              <Card key={s.id} onClick={() => open(s)}
                title={<span className="row">{s.name}<StatusBadge status={s.status} /></span>}
                subtitle={reason(s)}
                actions={<ChevronRight size={16} className="muted" aria-hidden />}>
                <div className="grid grid-2" style={{ gap: 'var(--space-3)' }}>
                  <div>
                    <div className="muted" style={{ fontSize: 'var(--text-xs)', marginBottom: 'var(--space-1)' }}>Traffic · {unitOf(s)}</div>
                    <TimeSeries timestamps={timestamps(range)} height={96} area format={(v) => compact(v)}
                      series={[{ id: 'rpm', label: 'Traffic', slot: 1, data: serviceTrafficSeries(s.id, range) }]} />
                  </div>
                  <div>
                    <div className="muted" style={{ fontSize: 'var(--text-xs)', marginBottom: 'var(--space-1)' }}>Error rate</div>
                    <TimeSeries timestamps={timestamps(range)} height={96} area format={(v) => pct(v)}
                      series={[{ id: 'err', label: 'Error rate', slot: 8, data: serviceErrorSeries(s.id, range) }]} />
                  </div>
                </div>
              </Card>
            ))}
          </div>
        </Section>
      )}

      <Section title={`All ${noun}`}
        actions={<>
          <div style={{ width: 240 }}><SearchInput placeholder="Filter by name" value={q} onChange={(e) => setQ(e.target.value)} /></div>
        </>}>
        {groups.length === 0 && <Card><p className="muted">No services match “{q}”.</p></Card>}
        {groups.map((g) => {
          const Icon = TYPE_META[g.types[0]].Icon;
          return (
            <Card key={g.id} flush title={<span className="row"><Icon size={16} className="muted" aria-hidden />{cat?.label ?? g.title}</span>} subtitle={`${g.rows.length} service${g.rows.length === 1 ? '' : 's'}`}>
              <Table rows={g.rows} rowKey={(s) => s.id} onRowClick={open} defaultSort={{ key: g.id === 'client' ? 'users' : 'rpm', dir: 'desc' }}
                columns={g.id === 'client' ? [
                  { key: 'name', header: 'App', sortValue: (s) => s.name, render: (s) => (
                    <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{s.name}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{s.language} · {s.version}</div></div>) },
                  { key: 'type', header: 'Type', render: (s) => <ServiceType type={s.type} /> },
                  { key: 'status', header: 'Health', sortValue: (s) => ['ok', 'warning', 'critical'].indexOf(s.status), render: (s) => <StatusBadge status={s.status} /> },
                  { key: 'users', header: 'Active users', numeric: true, sortValue: (s) => appSummary(s.id).activeUsers, render: (s) => compact(appSummary(s.id).activeUsers) },
                  { key: 'rpm', header: 'Views', numeric: true, sortValue: (s) => s.rpm, render: (s) => <span>{compact(s.rpm)} <span className="muted">/min</span></span> },
                  { key: 'rpm-t', header: '', render: (s) => <Sparkline data={serviceTrafficSeries(s.id, range).slice(-24)} width={72} height={24} /> },
                  { key: 'err', header: 'JS error / crash rate', numeric: true, sortValue: (s) => s.errorRate, render: (s) => <span>{pct(s.errorRate)} <span className="muted">{s.type === 'mobile' ? 'crash' : 'JS'}</span></span> },
                  { key: 'load', header: 'Load p75', numeric: true, sortValue: (s) => appSummary(s.id).vitals[0].value, render: (s) => {
                    const v = appSummary(s.id).vitals[0];
                    return <span>{ms(v.value)} <span className="muted">{v.label === 'LCP' ? 'LCP' : 'cold start'}</span></span>;
                  } },
                  { key: 'go', header: '', render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
                ] : [
                  { key: 'name', header: 'Service', sortValue: (s) => s.name, render: (s) => (
                    <div><div style={{ fontWeight: 'var(--weight-medium)' }}>{s.name}{s.source === 'ebpf' && <> <EbpfBadge /></>}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{s.language} · {s.version}</div></div>) },
                  { key: 'type', header: 'Type', render: (s) => <ServiceType type={s.type} /> },
                  { key: 'status', header: 'Health', sortValue: (s) => ['ok', 'warning', 'critical'].indexOf(s.status), render: (s) => <StatusBadge status={s.status} /> },
                  { key: 'rpm', header: 'Traffic', numeric: true, sortValue: (s) => s.rpm, render: (s) => <span>{compact(s.rpm)} <span className="muted">{unitOf(s)}</span></span> },
                  { key: 'rpm-t', header: '', render: (s) => <Sparkline data={serviceTrafficSeries(s.id, range).slice(-24)} width={72} height={24} /> },
                  { key: 'err', header: 'Error rate', numeric: true, sortValue: (s) => s.errorRate, render: (s) => pct(s.errorRate) },
                  { key: 'err-t', header: '', render: (s) => <Sparkline data={serviceErrorSeries(s.id, range).slice(-24)} width={72} height={24} /> },
                  { key: 'p95', header: 'p95', numeric: true, sortValue: (s) => s.p95, render: (s) => ms(s.p95) },
                  ...(g.id === 'async' ? [{ key: 'lag', header: 'Lag', numeric: true, sortValue: (s: Service) => s.lag ?? -1, render: (s: Service) => (s.lag !== undefined ? compact(s.lag) : '—') }] : []),
                  { key: 'go', header: '', render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
                ]} />
            </Card>
          );
        })}
      </Section>
    </>
  );
}
