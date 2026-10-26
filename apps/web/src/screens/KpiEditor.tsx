import { useData } from '@/data/remote';
import { useState } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import { ArrowDown, ArrowUp, GitBranch, Plus, Trash2 } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import { Button, Callout, Card, Field, Input, Meter, PageHeader, Segmented, Select, StatusBadge, Toggle } from '@/components/ui';
import { createKpi, endpointsFor, kpis, rumPagesFor, series, services, timestamps, type Kpi, type KpiSli, type Severity } from '@/data';
import { ms, pct } from '@/lib/format';
import { KPI_KIND_LABEL, SLI_LABEL, objectivePct } from './Kpis';

type Kind = Kpi['target']['kind'];
interface Rule { id: number; mult: number; long: string; short: string; severity: Severity; channels: string }

const OBJECTIVES = ['0.9', '0.95', '0.99', '0.995', '0.999'];
// functions, not constants: in api mode `services` is filled after this module loads
const clientApps = () => services.filter((s) => s.type === 'web' || s.type === 'mobile');
const backendServices = () => services.filter((s) => s.type !== 'web' && s.type !== 'mobile');
const isMobile = (id: string) => services.find((s) => s.id === id)?.type === 'mobile';

export function KpiEditor() {
  useData();
  const nav = useNavigate();
  const [params] = useSearchParams();
  const initialKind = (params.get('kind') as Kind | null) ?? (params.get('endpoint') || params.get('service') ? 'endpoint' : 'page');

  const [kind, setKind] = useState<Kind>(initialKind);
  // KPIs live under their service; return to its KPIs tab
  const owner = params.get('app') ?? params.get('service') ?? (params.get('edit') ? kpis.find((k) => k.id === params.get('edit'))?.target.serviceId : undefined);
  const back = owner ? `/services/${owner}?tab=kpis` : '/';
  const [app, setApp] = useState(params.get('app') ?? 'shop-web');
  const [page, setPage] = useState(params.get('page') ?? rumPagesFor(params.get('app') ?? 'shop-web')[0]?.id ?? '');
  const [service, setService] = useState(params.get('service') ?? 'orders-api');
  const [endpoint, setEndpoint] = useState(params.get('endpoint') ?? endpointsFor(params.get('service') ?? 'orders-api')[0]?.id ?? '');
  const [steps, setSteps] = useState(['shop-web · /cart', 'shop-web · /checkout', 'orders-api · POST /v1/orders', 'payments-api · POST /v1/charges']);
  const [sli, setSli] = useState<KpiSli>(initialKind === 'endpoint' ? 'success_rate' : 'latency');
  const [threshold, setThreshold] = useState(initialKind === 'endpoint' ? 300 : initialKind === 'app_start' ? 2000 : 2500);
  const [noError, setNoError] = useState(true);
  const [objective, setObjective] = useState(initialKind === 'endpoint' ? '0.995' : '0.95');
  const [win, setWin] = useState<'7d' | '28d'>('28d');
  const [name, setName] = useState(params.get('name') ?? '');
  const [rules, setRules] = useState<Rule[]>([
    { id: 1, mult: 6, long: '1h', short: '5m', severity: 'critical', channels: '#web-team, PagerDuty' },
    { id: 2, mult: 3, long: '6h', short: '30m', severity: 'warning', channels: '#web-team' },
  ]);

  const pages = rumPagesFor(app);
  const pageObj = pages.find((p) => p.id === page) ?? pages[0];
  const eps = endpointsFor(service);
  const ep = eps.find((e) => e.id === endpoint) ?? eps[0];
  const appSvc = services.find((s) => s.id === app);
  const metricName = kind === 'page' ? (isMobile(app) ? 'Screen load' : 'LCP') : kind === 'app_start' ? 'Cold start' : kind === 'journey' ? 'Journey duration' : 'Duration';
  const targetLabel = kind === 'page' ? `${appSvc?.name} · ${pageObj?.path}` : kind === 'endpoint' ? `${service} · ${ep ? `${ep.method} ${ep.route}` : '—'}` : kind === 'app_start' ? `${appSvc?.name} · Cold start` : `Journey · ${steps.length} steps`;
  const autoName = sli === 'latency' ? `${targetLabel} is fast` : sli === 'availability' ? `${targetLabel} is up` : `${targetLabel} succeeds`;

  // ---- live preview (mock): bad-event share over the last 7 days
  const obj = Number(objective);
  const allowed = 1 - obj;
  const errRate = kind === 'endpoint' ? ep?.errorRate ?? 0.002 : kind === 'journey' ? 0.009 : pageObj?.errorRate ?? appSvc?.errorRate ?? 0.002;
  const typical = kind === 'page' ? pageObj?.lcp ?? 2000 : kind === 'endpoint' ? (ep?.p95 ?? 200) * 0.75 : kind === 'app_start' ? (appSvc?.p95 ?? 1500) * 0.9 : 4200;
  const slowShare = sli === 'latency' ? Math.min(0.9, 0.25 * Math.pow(typical / Math.max(1, threshold), 3)) : 0;
  const base = Math.max(0.0004, slowShare + (sli === 'availability' ? errRate * 0.3 : noError || sli !== 'latency' ? errRate : 0));
  const key = `${kind}-${app}-${page}-${service}-${endpoint}-${sli}`;
  const ts = timestamps('7d');
  const bad = series(`kpi-preview-${key}`, '7d', { base, noise: 0.35, daily: 0.3, spikeAt: 0.62, spikeMul: 3.2, spikeWidth: 3 }).map((v) => Math.min(1, v));
  const fired = rules.map((r) => {
    let n = 0;
    let on = false;
    for (const b of bad) {
      const hot = b / allowed > r.mult;
      if (hot && !on) n++;
      on = hot;
    }
    return n;
  });
  const used = (bad.reduce((a, v) => a + v, 0) / bad.length / allowed) * (7 / (win === '7d' ? 7 : 28));
  const budgetLeft = Math.max(0, 1 - used);
  const windowDays = win === '7d' ? 7 : 28;

  const [saving, setSaving] = useState(false);
  const [error, setError] = useState<string>();
  const create = async () => {
    const target: Kpi['target'] = kind === 'endpoint'
      ? { kind, serviceId: service, ref: ep ? `${ep.method} ${ep.route}` : '', endpointId: ep?.id }
      : kind === 'journey'
        ? { kind, serviceId: steps[0]?.split(' · ')[0] || app, ref: `Journey: ${steps.filter(Boolean).join(' → ')}` }
        : kind === 'app_start' ? { kind, serviceId: app, ref: 'Cold start' } : { kind, serviceId: app, ref: pageObj?.path ?? '/' };
    const goodWhen = sli === 'latency' ? `${metricName} ≤ ${threshold} ms${noError ? ' and no error' : ''}` : sli === 'availability' ? 'Not 5xx / renders' : 'Succeeds (not 5xx)';
    setSaving(true); setError(undefined);
    try {
      await createKpi({
        name: name.trim() || autoName, description: goodWhen, owner: services.find((s) => s.id === target.serviceId)?.name ?? target.serviceId,
        target, sli, objective: obj, window: win,
        good: { metric: sli === 'latency' ? metricName : undefined, thresholdMs: sli === 'latency' ? threshold : undefined, noError: sli === 'latency' ? noError : true, description: goodWhen },
        alerts: rules.map((r) => ({ kind: 'burn_rate' as const, longWindow: r.long, shortWindow: r.short, threshold: r.mult, severity: r.severity, channels: r.channels.split(',').map((c) => c.trim()).filter(Boolean) })),
      });
      nav(back);
    } catch (e) {
      setError(e instanceof Error ? e.message : String(e));
      setSaving(false);
    }
  };

  const move = (i: number, d: -1 | 1) => setSteps((s) => { const c = [...s]; [c[i], c[i + d]] = [c[i + d], c[i]]; return c; });
  const setRule = (id: number, patch: Partial<Rule>) => setRules((rs) => rs.map((r) => (r.id === id ? { ...r, ...patch } : r)));

  return (
    <>
      <PageHeader title="New KPI" crumbs={[...(owner ? [{ label: services.find((s) => s.id === owner)?.name ?? owner, to: back }] : []), { label: 'New KPI' }]}
        subtitle="Define what a good experience is, how often it must happen, and when to alert"
        actions={<>
          <Button onClick={() => nav(back)}>Cancel</Button>
          <Button variant="primary" disabled={saving} onClick={() => void create()}>Create KPI</Button>
        </>} />
      {error && <Callout icon={<GitBranch size={16} color="var(--status-critical)" />}><strong>Could not create the KPI.</strong> <span className="secondary">{error}</span></Callout>}

      <div className="grid grid-3" style={{ alignItems: 'start' }}>
        <div className="stack span-2">
          <Card title="1 · What to measure">
            <div className="stack">
              <Segmented label="Target kind" value={kind} onChange={(k) => { setKind(k); if (k === 'endpoint') { setSli('success_rate'); setThreshold(300); } else { setSli('latency'); setThreshold(k === 'app_start' ? 2000 : k === 'journey' ? 8000 : 2500); } }}
                options={(['page', 'endpoint', 'journey', 'app_start'] as Kind[]).map((k) => ({ id: k, label: KPI_KIND_LABEL[k] }))} />
              {(kind === 'page' || kind === 'app_start') && (
                <div className="grid grid-2">
                  <Field label="App"><Select value={app} onChange={(e) => { setApp(e.target.value); setPage(rumPagesFor(e.target.value)[0]?.id ?? ''); }}
                    options={clientApps().filter((s) => kind === 'page' || s.type === 'mobile').map((s) => ({ value: s.id, label: `${s.name} (${s.type === 'web' ? 'web' : 'mobile'})` }))} /></Field>
                  {kind === 'page' && <Field label={isMobile(app) ? 'Screen' : 'Page'}><Select value={pageObj?.id ?? ''} onChange={(e) => setPage(e.target.value)} options={pages.map((p) => ({ value: p.id, label: p.path }))} /></Field>}
                </div>
              )}
              {kind === 'endpoint' && (
                <div className="grid grid-2">
                  <Field label="Service"><Select value={service} onChange={(e) => { setService(e.target.value); setEndpoint(endpointsFor(e.target.value)[0]?.id ?? ''); }} options={backendServices().map((s) => ({ value: s.id, label: s.name }))} /></Field>
                  <Field label="Endpoint"><Select value={ep?.id ?? ''} onChange={(e) => setEndpoint(e.target.value)} options={eps.map((e) => ({ value: e.id, label: `${e.method} ${e.route}` }))} /></Field>
                </div>
              )}
              {kind === 'journey' && (
                <Field label="Steps, in order" hint="A journey is good when every step succeeds within the session (matched by trace / session id).">
                  <div className="stack" style={{ gap: 6 }}>
                    {steps.map((s, i) => (
                      <div key={i} className="row" style={{ gap: 6 }}>
                        <span className="num muted" style={{ width: 18 }}>{i + 1}</span>
                        <Input value={s} mono onChange={(e) => setSteps(steps.map((x, j) => (j === i ? e.target.value : x)))} style={{ flex: 1 }} />
                        <Button size="sm" variant="ghost" iconOnly icon={<ArrowUp size={14} />} aria-label="Move up" disabled={i === 0} onClick={() => move(i, -1)} />
                        <Button size="sm" variant="ghost" iconOnly icon={<ArrowDown size={14} />} aria-label="Move down" disabled={i === steps.length - 1} onClick={() => move(i, 1)} />
                        <Button size="sm" variant="ghost" iconOnly icon={<Trash2 size={14} />} aria-label="Remove step" onClick={() => setSteps(steps.filter((_, j) => j !== i))} />
                      </div>
                    ))}
                    <div><Button size="sm" icon={<Plus size={12} />} onClick={() => setSteps([...steps, ''])}>Add step</Button></div>
                  </div>
                </Field>
              )}
              <Field label="Name" hint={name ? undefined : `Defaults to “${autoName}”`}>
                <Input value={name} placeholder={autoName} onChange={(e) => setName(e.target.value)} />
              </Field>
            </div>
          </Card>

          <Card title="2 · When is an event good">
            <div className="stack">
              <Segmented label="SLI type" value={sli} onChange={setSli} options={(['latency', 'availability', 'success_rate'] as KpiSli[]).map((s) => ({ id: s, label: SLI_LABEL[s] }))} />
              {sli === 'latency' && (
                <div className="grid grid-2" style={{ alignItems: 'end' }}>
                  <Field label={`${metricName} at most`} hint={`Typical now: ${ms(typical)}`}>
                    <div className="row" style={{ gap: 6 }}>
                      <Input type="number" min={1} value={threshold} onChange={(e) => setThreshold(Number(e.target.value) || 0)} style={{ width: 120 }} />
                      <span className="muted">ms</span>
                    </div>
                  </Field>
                  <div className="row" style={{ gap: 8, paddingBottom: 8 }}>
                    <Toggle checked={noError} onChange={setNoError} label="and no error" />
                    <span>and no error</span>
                  </div>
                </div>
              )}
              <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>
                Good event: <strong>{
                  sli === 'latency' ? `${kind === 'endpoint' ? 'request' : kind === 'journey' ? 'journey' : kind === 'app_start' ? 'app start' : 'page view'} with ${metricName} ≤ ${ms(threshold)}${noError ? ' and no error' : ''}`
                  : sli === 'availability' ? (kind === 'endpoint' ? 'request that is not 5xx' : 'load that renders (document 2xx, no fatal error)')
                  : kind === 'journey' ? 'journey where every step succeeds' : 'request that is not 5xx'
                }</strong>
              </div>
            </div>
          </Card>

          <Card title="3 · Objective">
            <div className="grid grid-2">
              <Field label="Share of good events" hint={`Allows ${objectivePct(allowed)} bad events — that's the error budget`}>
                <Select value={objective} onChange={(e) => setObjective(e.target.value)} options={OBJECTIVES.map((o) => ({ value: o, label: objectivePct(Number(o)) }))} />
              </Field>
              <Field label="Rolling window">
                <Segmented label="Window" value={win} onChange={setWin} options={[{ id: '7d', label: '7 days' }, { id: '28d', label: '28 days' }]} />
              </Field>
            </div>
          </Card>

          <Card title="4 · Alerts on this KPI" subtitle="Alert on how fast the error budget burns, not on single spikes"
            actions={<Button size="sm" icon={<Plus size={12} />} onClick={() => setRules([...rules, { id: Date.now(), mult: 2, long: '24h', short: '2h', severity: 'warning', channels: '' }])}>Add rule</Button>}>
            <div className="stack">
              {rules.map((r, i) => (
                <div key={r.id} className="row" style={{ gap: 8, flexWrap: 'wrap' }}>
                  <span className="secondary">Burn &gt;</span>
                  <Input type="number" min={1} step={0.5} value={r.mult} onChange={(e) => setRule(r.id, { mult: Number(e.target.value) || 1 })} style={{ width: 72 }} aria-label="Burn multiplier" />
                  <span className="secondary">× over</span>
                  <Select aria-label="Long window" value={r.long} onChange={(e) => setRule(r.id, { long: e.target.value })} options={['1h', '6h', '24h', '3d'].map((v) => ({ value: v, label: v }))} />
                  <span className="secondary">and</span>
                  <Select aria-label="Short window" value={r.short} onChange={(e) => setRule(r.id, { short: e.target.value })} options={['5m', '30m', '2h', '6h'].map((v) => ({ value: v, label: v }))} />
                  <Select aria-label="Severity" value={r.severity} onChange={(e) => setRule(r.id, { severity: e.target.value as Severity })} options={[{ value: 'critical', label: 'Critical' }, { value: 'warning', label: 'Warning' }, { value: 'info', label: 'Info' }]} />
                  <Input value={r.channels} placeholder="#channel, PagerDuty" onChange={(e) => setRule(r.id, { channels: e.target.value })} style={{ flex: 1, minWidth: 160 }} aria-label="Notify" />
                  <Button size="sm" variant="ghost" iconOnly icon={<Trash2 size={14} />} aria-label={`Remove rule ${i + 1}`} onClick={() => setRules(rules.filter((x) => x.id !== r.id))} />
                </div>
              ))}
              <div className="muted" style={{ fontSize: 'var(--text-sm)' }}>
                Burn rate = how fast bad events use the budget. {rules[0] ? `${rules[0].mult}× means the whole ${windowDays}-day budget would be gone in ${+(windowDays / rules[0].mult).toFixed(1)} days.` : '1× uses exactly the budget over the window.'} Both windows must burn, so short spikes don&apos;t page anyone.
              </div>
            </div>
          </Card>

          <Callout icon={<GitBranch size={16} />}>
            <strong>Root cause in every alert.</strong>{' '}
            <span className="secondary">When this KPI degrades we walk its call tree from traces — page → API → downstream services → database, cache or external API — and follow the branch that explains most of the change. The alert names the deepest cause, e.g. “recommendation-api&apos;s reco-db queries got 63× slower”, so you know whether it&apos;s your code or something you call.</span>
          </Callout>
        </div>

        {/* live preview — the important bit, stays in view */}
        <div style={{ position: 'sticky', top: 'calc(var(--topbar-h) + var(--space-4))' }}>
          <Card title="Preview · last 7 days" subtitle={name || autoName}>
            <div className="stack">
              <div className="row" style={{ alignItems: 'baseline', gap: 8 }}>
                <span className="num" style={{ fontSize: 'var(--text-2xl)', fontWeight: 600 }}>{fired.reduce((a, n) => a + n, 0)}</span>
                <span className="secondary">alerts would have fired</span>
              </div>
              {rules.length > 0 && (
                <div className="muted" style={{ fontSize: 'var(--text-xs)', marginTop: -8 }}>
                  {rules.map((r, i) => `${r.mult}× ${r.long}: ${fired[i]}`).join(' · ')}
                </div>
              )}
              <div>
                <div className="muted" style={{ fontSize: 'var(--text-xs)', marginBottom: 4 }}>Bad events · line = allowed {objectivePct(allowed)}</div>
                <TimeSeries timestamps={ts} height={180} area format={(v) => pct(v)}
                  threshold={{ value: allowed, label: `allowed ${objectivePct(allowed)}` }}
                  series={[{ id: 'bad', label: 'Bad events', slot: 8, data: bad }]} />
              </div>
              <div>
                <div className="row muted" style={{ fontSize: 'var(--text-xs)', marginBottom: 4 }}>
                  Error budget left ({win}) <span className="spacer" /><span className="num secondary">{Math.round(budgetLeft * 100)}%</span>
                </div>
                <Meter value={budgetLeft} tone={budgetLeft < 0.25 ? 'critical' : budgetLeft < 0.5 ? 'warning' : undefined} />
              </div>
              <div className="row" style={{ gap: 8 }}>
                {base > allowed
                  ? <StatusBadge status="warning" label="Would breach today" />
                  : <StatusBadge status="ok" label="Would be on track" />}
                <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>bad now ≈ {pct(base)}</span>
              </div>
            </div>
          </Card>
        </div>
      </div>
    </>
  );
}
