import { useData } from '@/data/remote';
import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { CheckCircle2 } from 'lucide-react';
import {
  Badge, Card, EmptyState, SearchInput, Section, Select, StatusBadge, Table,
} from '@/components/ui';
import { NOW, incidents, isApi, services, type Incident } from '@/data';
import { ago, duration } from '@/lib/format';
import {
  INCIDENT_ORDER, IncidentCard, IncidentDrawer, MaintenanceWindows, ackedAtOf, monitorOf, pastIncidents, serviceIdOf, sevTone, useAcks, type PastIncident,
} from './Alerts';

/**
 * Every incident across services and apps in one list. Each service still shows its own
 * under its Alerts tab; this is the cross-service view (like the KPIs page for KPIs).
 */
export function Incidents() {
  useData();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const { ack, resolve, stateOf } = useAcks();
  const [q, setQ] = useState('');
  const [svc, setSvc] = useState('all');
  const [sev, setSev] = useState('all');

  const all: Incident[] = [...incidents, ...pastIncidents];
  const match = (i: Incident) => (svc === 'all' || serviceIdOf(i.service) === svc)
    && (sev === 'all' || i.severity === sev)
    && (!q || `${i.title} ${i.service} ${monitorOf(i).target}`.toLowerCase().includes(q.toLowerCase()));
  const open = incidents.filter((i) => stateOf(i) !== 'resolved').filter(match)
    .sort((a, b) => INCIDENT_ORDER[stateOf(a)] - INCIDENT_ORDER[stateOf(b)] || (a.severity === 'critical' ? -1 : 1) || b.startedAt - a.startedAt);
  const resolved = pastIncidents.filter(match);

  const openAll = incidents.filter((i) => stateOf(i) !== 'resolved');
  const firing = openAll.filter((i) => stateOf(i) === 'firing').length;
  const mttr = pastIncidents.reduce((a, h) => a + (h.resolvedAt - h.startedAt), 0) / (pastIncidents.length || 1);
  const ackTimes = pastIncidents.map((h) => (ackedAtOf(h) ?? 0) - h.startedAt).filter((d) => d > 0);
  const tta = ackTimes.reduce((a, b) => a + b, 0) / (ackTimes.length || 1);
  const week = all.filter((i) => i.startedAt > (isApi ? Date.now() : NOW) - 7 * 86400_000).length;

  const selected = all.find((i) => i.id === params.get('id'));
  const openDrawer = (i: Incident) => setParams((p) => { const n = new URLSearchParams(p); n.set('id', i.id); return n; });
  const closeDrawer = () => setParams((p) => { const n = new URLSearchParams(p); n.delete('id'); return n; }, { replace: true });
  const affected = [...new Set(all.map((i) => serviceIdOf(i.service)).filter(Boolean))] as string[];

  return (
    <>
      <header className="incidents-heading">
        <div className="incidents-heading__intro">
          <span className="incidents-heading__eyebrow">LIVE OPERATIONS / INCIDENTS</span>
          <h1>Incidents <span>{openAll.length} open</span></h1>
        </div>
        <div className="incidents-heading__meta" aria-label="Incident summary">
          <span><strong>{firing}</strong> firing now</span>
          <span>7d avg acknowledge <strong>{ackTimes.length ? duration(tta / 1000) : '—'}</strong></span>
          <span>7d avg resolve <strong>{duration(mttr / 1000)}</strong></span>
          <a href="#maintenance-windows">Maintenance windows</a>
        </div>
      </header>

      <div className="incidents-filterbar">
        <div className="incidents-filterbar__controls">
          <Select aria-label="Filter by service" value={svc} onChange={(e) => setSvc(e.target.value)}
            options={[{ value: 'all', label: 'All services & apps' }, ...affected.map((id) => ({ value: id, label: services.find((s) => s.id === id)?.name ?? id }))]} />
          <Select aria-label="Filter by severity" value={sev} onChange={(e) => setSev(e.target.value)}
            options={[{ value: 'all', label: 'Any severity' }, { value: 'critical', label: 'Critical' }, { value: 'warning', label: 'Warning' }]} />
          <div className="incidents-filterbar__search"><SearchInput placeholder="Search incidents" value={q} onChange={(e) => setQ(e.target.value)} /></div>
        </div>
      </div>

      <Section title="Active queue" subtitle={week ? `${week} incidents in the last 7 days` : undefined}>
        {open.length === 0 ? (
          <Card><EmptyState icon={<CheckCircle2 />} title="All clear">{q || svc !== 'all' || sev !== 'all' ? 'No open incident matches the filters.' : 'No monitor is firing or pending.'}</EmptyState></Card>
        ) : (
          <div className="incidents-workspace__feed">
            {open.map((i) => <IncidentCard key={i.id} incident={i} state={stateOf(i)} onOpen={() => openDrawer(i)} onAck={() => ack(i.id)} />)}
          </div>
        )}
      </Section>

      <Section title="Resolved" subtitle="Last 7 days">
        <Card flush>
          <Table<PastIncident> rows={resolved} rowKey={(h) => h.id} empty="No resolved incident matches the filters." onRowClick={openDrawer}
            defaultSort={{ key: 'start', dir: 'desc' }} columns={[
              { key: 'state', header: 'State', width: 120, render: () => <StatusBadge status="resolved" /> },
              { key: 'title', header: 'Incident', sortValue: (h) => h.title, render: (h) => (
                <div style={{ minWidth: 0 }}>
                  <div style={{ fontWeight: 500 }}>{h.title}</div>
                  <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{monitorOf(h).target}</div>
                </div>) },
              { key: 'svc', header: 'Service', sortValue: (h) => h.service, render: (h) => {
                const id = serviceIdOf(h.service);
                return id
                  ? <a style={{ whiteSpace: 'nowrap' }} href={`/services/${id}?tab=alerts`} onClick={(e) => { e.preventDefault(); e.stopPropagation(); nav(`/services/${id}?tab=alerts`); }}>{h.service}</a>
                  : h.service;
              } },
              { key: 'sev', header: 'Severity', sortValue: (h) => h.severity, render: (h) => <Badge tone={sevTone(h.severity)}>{h.severity}</Badge> },
              { key: 'peak', header: 'Peak', numeric: true, render: (h) => h.value },
              { key: 'ack', header: isApi ? 'Acknowledged' : 'Acknowledged by', render: (h) => {
                const at = ackedAtOf(h);
                return h.ackBy ?? (isApi && at ? <span className="muted">after {duration((at - h.startedAt) / 1000)}</span> : <span className="muted">—</span>);
              } },
              { key: 'dur', header: 'Duration', numeric: true, sortValue: (h) => h.resolvedAt - h.startedAt, render: (h) => duration((h.resolvedAt - h.startedAt) / 1000) },
              { key: 'start', header: 'Started', numeric: true, sortValue: (h) => h.startedAt, render: (h) => <span className="muted" style={{ whiteSpace: 'nowrap' }}>{ago(h.startedAt)}</span> },
            ]} />
        </Card>
      </Section>

      <details className="incidents-maintenance" id="maintenance-windows">
        <summary>Maintenance windows <span>Schedule planned work or review existing windows</span></summary>
        <MaintenanceWindows />
      </details>

      {selected && (
        <IncidentDrawer incident={selected} state={stateOf(selected)} onClose={closeDrawer} onAck={() => ack(selected.id)} onResolve={() => resolve(selected.id)}
          onOpenService={(id) => nav(`/services/${id}?tab=alerts`)} />
      )}
    </>
  );
}
