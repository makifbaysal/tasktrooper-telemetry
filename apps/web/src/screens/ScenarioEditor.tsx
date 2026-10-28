import { useData } from '@/data/remote';
import { useMemo, useState } from 'react';
import { Link, useLocation, useNavigate, useParams, useSearchParams } from 'react-router';
import { AlertTriangle, Check, ChevronDown, ChevronRight, Loader2, Play, Plus, ShieldCheck, Sparkles, Timer, Trash2, User } from 'lucide-react';
import { TimeSeries } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CodeBlock, EmptyState, Field, Input, PageHeader, Section, Segmented, Select, Textarea,
} from '@/components/ui';
import { RunBlocked, approveScenario, checkTarget, limitProblems, scenarioState, startRunChecked } from '@/data/loadtest';
import { endpointsFor, scenarios, services, type Endpoint } from '@/data';
import { compact, duration, ms, pct } from '@/lib/format';
import {
  DEFAULT_GATE, LEVEL_PRESETS, REGIONS, SPEC, aiRequestSpec, blankRequestSpec, levelsOf, levelsToStages, saveScenario,
  type Level, type LoadMode, type Progression, type RequestSpec, type ScenarioDraft,
} from '@/lib/loadtest';
import { MethodBadge } from '@/components/ui';

// a function, not a constant: in api mode `services` is filled after this module loads
const apis = () => services.filter((s) => s.type === 'http');
const keyOf = (e: Endpoint) => `${e.method} ${e.route}`;
const slug = (name: string) => name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '');
const hasBody = (method: string) => method !== 'GET' && method !== 'DELETE';
const elapsed = (sec: number) => `+${Math.floor(sec / 60)}:${String(Math.round(sec % 60)).padStart(2, '0')}`;

/** Production mix of the given steps, as whole percentages summing to 100. */
function productionMix(eps: Endpoint[], steps: string[]): Record<string, number> {
  const chosen = eps.filter((e) => steps.includes(keyOf(e)));
  const sum = chosen.reduce((a, e) => a + e.rpm, 0) || 1;
  const out = Object.fromEntries(chosen.map((e) => [keyOf(e), Math.round((e.rpm / sum) * 1000) / 10]));
  const drift = Math.round((100 - Object.values(out).reduce((a, v) => a + v, 0)) * 10) / 10;
  if (chosen[0] && drift) out[keyOf(chosen[0])] = Math.round((out[keyOf(chosen[0])] + drift) * 10) / 10;
  return out;
}

interface State extends Omit<ScenarioDraft, 'weights' | 'p95Limit' | 'errLimit'> {
  /** percent per step */
  weights: Record<string, number>;
  p95: string;
  err: string;
}

function initialState(id: string | undefined, fromTraffic: boolean, serviceParam?: string | null): State {
  const sc = id ? scenarios.find((s) => s.id === id) : undefined;
  if (sc) {
    const spec = SPEC[sc.id];
    const eps = spec.services.flatMap((x) => endpointsFor(x));
    // seeded scenarios carry request data like any saved one; generated ones keep their production source
    const fallback = (k: string) => {
      const e = eps.find((x) => keyOf(x) === k) ?? eps[0];
      const ai = aiRequestSpec(e);
      return sc.source === 'generated' ? ai : { ...ai, source: undefined };
    };
    const pctW = spec.weights ? Object.fromEntries(Object.entries(spec.weights).map(([k, v]) => [k, Math.round(v * 1000) / 10])) : productionMix(eps, spec.steps);
    const p95Th = spec.thresholds.find((t) => t.includes('duration'));
    const errTh = spec.thresholds.find((t) => t.includes('failed'));
    return {
      name: sc.name, target: sc.target, services: [...spec.services], regions: sc.regions, source: sc.source,
      mode: spec.mode ?? 'vus', levels: levelsOf(spec).map((l) => ({ ...l })), steps: [...spec.steps], weights: pctW,
      requests: Object.fromEntries(spec.steps.map((k) => [k, spec.requests?.[k] ?? fallback(k)])),
      thinkTime: spec.thinkTime ?? 1, progression: spec.progression ?? 'auto', gate: { ...(spec.gate ?? DEFAULT_GATE) },
      p95: /([\d.]+)\s*ms/.exec(p95Th ?? '')?.[1] ?? '', err: /([\d.]+)%/.exec(errTh ?? '')?.[1] ?? '1',
    };
  }
  const service = apis().find((s) => s.id === serviceParam)?.id ?? apis()[0]?.id ?? "";
  const eps = endpointsFor(service).sort((a, b) => b.rpm - a.rpm);
  const sum = eps.reduce((a, e) => a + e.rpm, 0) || 1;
  if (fromTraffic) {
    const steps = eps.filter((e) => e.rpm / sum >= 0.02).slice(0, 5).map(keyOf);
    const prodRps = eps.filter((e) => steps.includes(keyOf(e))).reduce((a, e) => a + e.rpm, 0) / 60;
    const maxP95 = Math.max(...eps.filter((e) => steps.includes(keyOf(e))).map((e) => e.p95));
    return {
      name: `${services.find((s) => s.id === service)?.name ?? service} production mix 2×`, target: 'https://staging.shop.com', services: [service], regions: ['eu-central'], source: 'generated',
      mode: 'rps', levels: LEVEL_PRESETS['Step 1× → 2× → 3×'](Math.round(prodRps * 3)), steps, weights: productionMix(eps, steps),
      requests: Object.fromEntries(eps.filter((e) => steps.includes(keyOf(e))).map((e) => [keyOf(e), aiRequestSpec(e)])),
      thinkTime: 1, p95: String(Math.ceil((maxP95 * 1.5) / 10) * 10), err: '1', progression: 'ai', gate: { ...DEFAULT_GATE },
    };
  }
  return {
    name: '', target: 'https://staging.shop.com', services: [service], regions: ['eu-central'], source: 'manual',
    mode: 'vus', levels: LEVEL_PRESETS['Ramp & hold'](100), steps: [], weights: {}, requests: {}, thinkTime: 1, p95: '', err: '1', progression: 'ai', gate: { ...DEFAULT_GATE },
  };
}

export function ScenarioEditor() {
  useData();
  const nav = useNavigate();
  const { scenarioId } = useParams();
  const [params] = useSearchParams();
  const editing = scenarioId ? scenarios.find((s) => s.id === scenarioId) : undefined;
  const [st, setSt] = useState<State>(() => initialState(scenarioId, params.get('from') === 'traffic', params.get('service')));
  const [open, setOpen] = useState<Record<string, boolean>>({});
  const [busy, setBusy] = useState<Record<string, boolean>>({});
  const set = (patch: Partial<State>) => setSt((s) => ({ ...s, ...patch }));

  const eps = useMemo(() => st.services.flatMap((x) => endpointsFor(x)).sort((a, b) => b.rpm - a.rpm), [st.services]);
  const rpmSum = eps.reduce((a, e) => a + e.rpm, 0) || 1;
  const chosen = eps.filter((e) => st.steps.includes(keyOf(e)));
  const unit = st.mode === 'rps' ? 'req/s' : 'VUs';
  const peak = Math.max(0, ...st.levels.map((l) => l.target));
  const stages = levelsToStages(st.levels);
  const total = stages.reduce((a, x) => a + x.duration, 0);
  const weightSum = Math.round(st.steps.reduce((a, k) => a + (st.weights[k] ?? 0), 0) * 10) / 10;
  // request rate at peak: open model = the target; closed model ≈ VUs / (latency + think time)
  const avgLatency = chosen.reduce((a, e) => a + e.p95 * 0.5 * ((st.weights[keyOf(e)] ?? 0) / 100), 0) / 1000;
  const peakRps = st.mode === 'rps' ? peak : peak / Math.max(0.05, avgLatency + st.thinkTime);
  const p95Limit = st.p95 ? Number(st.p95) : undefined;
  const missingData = chosen.filter((e) => { const r = st.requests[keyOf(e)]; return !r || /:\s*$/m.test(r.params) || /\{\s*\}/.test(r.body); });
  const valid = st.name.trim() && chosen.length > 0 && st.regions.length > 0 && peak > 0 && Math.abs(weightSum - 100) < 0.5 && total > 0;

  // ---- actions
  const toggleService = (id: string) => setSt((s) => {
    if (s.services.includes(id)) {
      if (s.services.length === 1) return s;
      const gone = new Set(endpointsFor(id).map(keyOf));
      const steps = s.steps.filter((k) => !gone.has(k));
      const all = s.services.filter((x) => x !== id).flatMap((x) => endpointsFor(x));
      return { ...s, services: s.services.filter((x) => x !== id), steps, weights: productionMix(all, steps) };
    }
    return { ...s, services: [...s.services, id] };
  });
  const toggleStep = (e: Endpoint) => {
    const k = keyOf(e);
    const steps = st.steps.includes(k) ? st.steps.filter((x) => x !== k) : [...st.steps, k];
    const requests = { ...st.requests };
    if (!requests[k]) requests[k] = blankRequestSpec(e);
    set({ steps, requests, weights: productionMix(eps, steps) });
  };
  const setLevel = (i: number, patch: Partial<Level>) => set({ levels: st.levels.map((l, k) => (k === i ? { ...l, ...patch } : l)) });
  const setReq = (k: string, patch: Partial<RequestSpec>) => set({ requests: { ...st.requests, [k]: { ...st.requests[k], ...patch } } });
  const generate = (list: Endpoint[]) => {
    const ids = Object.fromEntries(list.map((e) => [keyOf(e), true]));
    setBusy((b) => ({ ...b, ...ids }));
    setOpen((o) => ({ ...o, ...ids }));
    setTimeout(() => {
      setSt((s) => ({ ...s, requests: { ...s.requests, ...Object.fromEntries(list.map((e) => [keyOf(e), aiRequestSpec(e)])) } }));
      setBusy((b) => Object.fromEntries(Object.entries(b).filter(([k]) => !ids[k])));
    }, 900);
  };
  const draft = (): ScenarioDraft => ({
    ...st, name: st.name.trim(),
    weights: Object.fromEntries(st.steps.map((k) => [k, (st.weights[k] ?? 0) / (weightSum || 1)])),
    requests: Object.fromEntries(st.steps.map((k) => [k, st.requests[k]])),
    p95Limit, errLimit: Number(st.err || 1) / 100,
  });
  const back = params.get('service') ? `/services/${params.get('service')}?tab=loadtests` : '/load-tests';
  const isDraft = !!editing && scenarioState(editing.id) === 'draft';
  const target = checkTarget(st.target);
  const peakVusPlanned = st.mode === 'rps' ? Math.max(1, Math.ceil(peak * (0.2 + st.thinkTime))) : peak;
  const limits = limitProblems(peakVusPlanned, total);
  const runnable = target.ok && limits.length === 0 && !isDraft;
  const [starting, setStarting] = useState(false);
  const loc = useLocation();
  const [runErr, setRunErr] = useState<RunBlocked | null>(() => {
    const e = (loc.state as { runErr?: { kind: RunBlocked['kind']; message: string } } | null)?.runErr;
    return e ? new RunBlocked(e.kind, e.message) : null;
  });
  const afterSave = (id: string) => (params.get('service') ? back : `/load-tests?scenario=${id}`);
  const save = async (run: boolean) => {
    const id = saveScenario(draft(), editing?.id);
    if (!run) { nav(afterSave(id)); return; }
    setStarting(true); setRunErr(null);
    try {
      nav(`/load-tests/runs/${await startRunChecked(id, true)}`);
    } catch (e) {
      // saved, but the server refused the run (limits, domain): stay and say why; "Create" again would duplicate
      const err = e instanceof RunBlocked ? e : new RunBlocked('api', String(e));
      setRunErr(err);
      if (!editing) nav(`/load-tests/scenarios/${id}/edit`, { replace: true, state: { runErr: { kind: err.kind, message: err.message } } });
    }
    setStarting(false);
  };
  const saveApprove = async () => {
    const id = saveScenario(draft(), editing?.id);
    await approveScenario(id).catch((e: unknown) => setRunErr(new RunBlocked('api', e instanceof Error ? e.message : String(e))));
    nav(afterSave(id));
  };

  // ---- planned profile (one point per 5% of the timeline, plus every level boundary)
  const profile = useMemo(() => {
    const pts: { t: number; v: number }[] = [{ t: 0, v: 0 }];
    let t = 0;
    for (const s of stages) { t += s.duration; pts.push({ t, v: s.target }); }
    const step = Math.max(1, total / 60);
    const ts: number[] = []; const vs: number[] = [];
    for (let x = 0; x <= total + 0.001; x += step) {
      const k = pts.findIndex((p) => p.t >= x);
      const a = pts[Math.max(0, k - 1)], b = pts[Math.max(0, k)];
      vs.push(b.t === a.t ? b.v : a.v + ((b.v - a.v) * (x - a.t)) / (b.t - a.t));
      ts.push(x * 1000);
    }
    return { ts, vs };
  }, [stages, total]);

  const yaml = [
    `name: ${st.name || 'untitled'}`,
    `target: ${st.target}`,
    `regions: [${st.regions.join(', ')}]`,
    `executor: ${st.mode === 'rps' ? 'ramping-arrival-rate   # requests / sec' : `ramping-vus            # think time ${st.thinkTime}s`}`,
    `progression: ${st.progression}${st.progression !== 'auto' ? `   # gate: dependency p95 ≤ ${st.gate.latencyX}× prod, cpu ≤ ${Math.round(st.gate.cpu * 100)}%` : ''}`,
    'levels:',
    ...st.levels.map((l) => `  - { name: ${l.name}, ramp: ${duration(l.ramp)}, hold: ${duration(l.hold)}, target: ${l.target} }`),
    'mix:',
    ...st.steps.map((k) => `  - { request: "${k}", weight: ${st.weights[k] ?? 0}% }`),
    'requests:',
    ...st.steps.flatMap((k) => {
      const r = st.requests[k];
      if (!r) return [];
      const indent = (txt: string, n: number) => txt.split('\n').filter(Boolean).map((l) => `${' '.repeat(n)}${l}`);
      return [
        `  "${k}":`,
        ...(r.params ? ['    params:', ...indent(r.params, 6)] : []),
        ...(r.headers ? ['    headers:', ...indent(r.headers, 6)] : []),
        ...(r.body ? ['    body: |', ...indent(r.body, 6)] : []),
      ];
    }),
    'thresholds:',
    `  - "http_req_failed rate < ${st.err || 1}%"`,
    ...(p95Limit ? [`  - "http_req_duration p(95) < ${p95Limit} ms"`] : []),
  ].join('\n');

  return (
    <>
      <PageHeader title={editing ? `Edit ${editing.name}` : params.get('from') === 'traffic' ? 'Generate scenario from traffic' : 'New scenario'}
        crumbs={[params.get('service') ? { label: services.find((s) => s.id === params.get('service'))?.name ?? 'Service', to: back } : { label: 'Load tests', to: '/load-tests' }, { label: editing ? editing.name : 'New scenario' }]}
        subtitle={params.get('from') === 'traffic' && !editing ? 'Steps, mix, levels and request data were derived from the last 7 days of production traffic — review and adjust' : 'Load levels, traffic mix per endpoint and what each request carries'}
        actions={<>
          <Button onClick={() => nav(back)}>Cancel</Button>
          <Button disabled={!valid} onClick={() => void save(false)}>{editing ? 'Save' : 'Create scenario'}</Button>
          {isDraft
            ? <Button variant="primary" icon={<Check size={14} />} disabled={!valid} onClick={() => void saveApprove()}>Save & approve</Button>
            : <Button variant="primary" icon={<Play size={14} />} disabled={!valid || !runnable || starting} title={!target.ok ? target.reason : limits[0]}
                onClick={() => void save(true)}>{starting ? 'Starting…' : editing ? 'Save & run' : 'Create & run'}</Button>}
        </>} />

      {isDraft && (
        <Callout icon={<Sparkles size={16} aria-hidden />}>
          <strong>Draft generated by code analysis.</strong> <span className="secondary">Review the target, levels and request data — it can run once approved.</span>
        </Callout>
      )}
      {runErr && (
        <Callout icon={<AlertTriangle size={16} aria-hidden />}>
          <strong>Saved, but the run did not start.</strong> <span className="secondary">{runErr.message}</span>
          {runErr.kind === 'domain' && <> <Link to="/load-tests/domains">Verify domain →</Link></>}
        </Callout>
      )}
      {limits.length > 0 && (
        <Callout icon={<AlertTriangle size={16} aria-hidden />}>
          <strong>Over your plan limits.</strong> <span className="secondary">{limits.join(' ')}</span>
        </Callout>
      )}

      {/* 1 · target */}
      <Card title="Target">
        <div className="grid grid-3" style={{ gap: 'var(--space-3)' }}>
          <Field label="Name"><Input value={st.name} placeholder="e.g. Checkout flow" onChange={(e) => set({ name: e.target.value })} /></Field>
          <Field label="Base URL" hint={target.ok
            ? <span className="row" style={{ gap: 4 }}><ShieldCheck size={12} aria-hidden />{target.local ? 'localhost — no verification needed' : `${target.host} is verified`}</span>
            : <span>{target.reason} <Link to="/load-tests/domains">Domains →</Link></span>}>
            <Input mono value={st.target} aria-invalid={!target.ok} onChange={(e) => set({ target: e.target.value })} />
          </Field>
          <Field label="Regions">
            <div className="row" style={{ gap: 'var(--space-3)', minHeight: 'var(--control-h-md)', flexWrap: 'wrap' }}>
              {REGIONS.map((r) => (
                <label key={r} className="row" style={{ gap: 6, cursor: 'pointer', fontSize: 'var(--text-sm)' }}>
                  <input type="checkbox" checked={st.regions.includes(r)} onChange={() => set({ regions: st.regions.includes(r) ? st.regions.filter((x) => x !== r) : [...st.regions, r] })} />{r}
                </label>
              ))}
            </div>
          </Field>
        </div>
        <div style={{ marginTop: 'var(--space-3)' }}>
          <Field label="Services under test" hint="endpoints of every ticked API show up in the traffic mix">
            <div className="row" style={{ gap: 'var(--space-4)', flexWrap: 'wrap', minHeight: 'var(--control-h-md)' }}>
              {apis().map((a) => (
                <label key={a.id} className="row" style={{ gap: 6, cursor: 'pointer', fontSize: 'var(--text-sm)' }}>
                  <input type="checkbox" checked={st.services.includes(a.id)} onChange={() => toggleService(a.id)} />{a.name}
                </label>
              ))}
            </div>
          </Field>
        </div>
      </Card>

      {/* 2 · load levels */}
      <Section title="Load levels" subtitle={`${st.levels.length} levels · ${duration(total)} · peak ${compact(peak)} ${unit}${st.mode === 'vus' ? ` ≈ ${compact(peakRps)} req/s` : ''}`}
        actions={<>
          <Segmented label="Load model" value={st.mode} onChange={(m: LoadMode) => set({ mode: m })}
            options={[{ id: 'vus', label: 'Virtual users' }, { id: 'rps', label: 'Requests / sec' }]} />
          <Select aria-label="Apply a preset" value="" onChange={(e) => e.target.value && set({ levels: LEVEL_PRESETS[e.target.value](peak || 100) })}
            options={[{ value: '', label: 'Apply preset…' }, ...Object.keys(LEVEL_PRESETS).map((k) => ({ value: k, label: k }))]} />
          <Button size="sm" icon={<Plus size={14} />} onClick={() => {
            const cool = st.levels.findIndex((l) => l.target === 0);
            const lvl: Level = { name: `Level ${st.levels.length + 1}`, ramp: 60, hold: 120, target: peak || 100 };
            set({ levels: cool >= 0 ? [...st.levels.slice(0, cool), lvl, ...st.levels.slice(cool)] : [...st.levels, lvl] });
          }}>Add level</Button>
        </>}>
        <div className="grid grid-3">
          <Card className="span-2" flush>
            <table className="table">
              <thead><tr>
                <th style={{ width: 32 }}>#</th><th>Level</th>
                <th className="is-num">Ramp (s)</th><th className="is-num">Hold (s)</th><th className="is-num">Target ({unit})</th>
                {st.mode === 'vus' && <th className="is-num">≈ req/s</th>}
                <th style={{ width: 40 }} />
              </tr></thead>
              <tbody>
                {st.levels.map((l, i) => (
                  <tr key={i}>
                    <td className="is-num muted">{i + 1}</td>
                    <td style={{ minWidth: 130 }}><Input value={l.name} onChange={(e) => setLevel(i, { name: e.target.value })} style={{ width: '100%' }} /></td>
                    <td className="is-num"><Input type="number" min={0} value={l.ramp} onChange={(e) => setLevel(i, { ramp: Math.max(0, Number(e.target.value) || 0) })} style={{ width: 72, textAlign: 'right' }} /></td>
                    <td className="is-num"><Input type="number" min={0} value={l.hold} onChange={(e) => setLevel(i, { hold: Math.max(0, Number(e.target.value) || 0) })} style={{ width: 72, textAlign: 'right' }} /></td>
                    <td className="is-num"><Input type="number" min={0} value={l.target} onChange={(e) => setLevel(i, { target: Math.max(0, Number(e.target.value) || 0) })} style={{ width: 80, textAlign: 'right' }} /></td>
                    {st.mode === 'vus' && <td className="is-num muted">{compact(l.target / Math.max(0.05, avgLatency + st.thinkTime))}</td>}
                    <td><Button size="sm" variant="ghost" aria-label={`Remove ${l.name}`} icon={<Trash2 size={14} />} disabled={st.levels.length <= 1}
                      onClick={() => set({ levels: st.levels.filter((_, k) => k !== i) })} /></td>
                  </tr>
                ))}
              </tbody>
            </table>
            {st.mode === 'vus' && (
              <div className="row" style={{ gap: 'var(--space-3)', padding: 'var(--space-3) var(--space-4)', borderTop: '1px solid var(--border)' }}>
                <span className="secondary" style={{ fontSize: 'var(--text-sm)' }}>Think time between a user's requests</span>
                <Input type="number" min={0} step="0.5" value={st.thinkTime} onChange={(e) => set({ thinkTime: Math.max(0, Number(e.target.value) || 0) })} style={{ width: 80, textAlign: 'right' }} />
                <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>s</span>
              </div>
            )}
          </Card>
          <Card title="Planned load" subtitle={`${unit} over the run`}>
            {total > 0
              ? <TimeSeries timestamps={profile.ts} height={200} area formatX={(t) => elapsed(t / 1000)} format={(v) => compact(v)}
                  series={[{ id: 'load', label: st.mode === 'rps' ? 'Requests / sec' : 'Virtual users', slot: 1, data: profile.vs }]} />
              : <EmptyState icon={<Plus />} title="Give a level a duration" />}
          </Card>
        </div>
      </Section>

      {/* 2b · who moves between levels */}
      <Card title="Level transitions" subtitle="Who decides when the load goes up to the next level">
        <div className="grid grid-3" style={{ gap: 'var(--space-3)' }}>
          {([
            { id: 'ai', icon: Sparkles, title: 'AI-guided', text: 'At the end of each level the AI checks the API and every system it calls (databases, caches, other services, third-party APIs). It raises the load only if all are healthy and the next level is projected to stay within limits; otherwise it holds — or steps back — and tells you why and what to fix.' },
            { id: 'manual', icon: User, title: 'Manual', text: 'Each level holds until you move on. The run page shows the same health checks and a recommendation, so you decide with the data in front of you.' },
            { id: 'auto', icon: Timer, title: 'Timer', text: 'Levels follow the plan on a timer, whatever happens. You can still hold, step back or skip ahead while it runs.' },
          ] as { id: Progression; icon: typeof Timer; title: string; text: string }[]).map((o) => (
            <label key={o.id} className="card" style={{
              padding: 'var(--space-3) var(--space-4)', cursor: 'pointer', boxShadow: 'none',
              borderColor: st.progression === o.id ? 'var(--accent)' : undefined, background: st.progression === o.id ? 'var(--accent-soft)' : undefined,
            }}>
              <div className="row" style={{ gap: 'var(--space-2)', fontWeight: 600 }}>
                <input type="radio" name="progression" checked={st.progression === o.id} onChange={() => set({ progression: o.id })} />
                <o.icon size={14} aria-hidden />{o.title}
              </div>
              <div className="secondary" style={{ fontSize: 'var(--text-sm)', marginTop: 'var(--space-1)' }}>{o.text}</div>
            </label>
          ))}
        </div>
        {st.progression !== 'auto' && (
          <div className="row" style={{ gap: 'var(--space-5)', marginTop: 'var(--space-4)', flexWrap: 'wrap', alignItems: 'flex-end' }}>
            <Field label="Dependency p95 may grow up to" hint="× its production p95 — databases, caches, called services, third-party APIs">
              <span className="row" style={{ gap: 6 }}><Input type="number" min={1.2} step="0.5" value={st.gate.latencyX} onChange={(e) => set({ gate: { ...st.gate, latencyX: Math.max(1.2, Number(e.target.value) || 3) } })} style={{ width: 80, textAlign: 'right' }} /><span className="muted">×</span></span>
            </Field>
            <Field label="Service CPU up to" hint="per service under test">
              <span className="row" style={{ gap: 6 }}><Input type="number" min={30} max={100} value={Math.round(st.gate.cpu * 100)} onChange={(e) => set({ gate: { ...st.gate, cpu: Math.min(1, Math.max(0.3, (Number(e.target.value) || 85) / 100)) } })} style={{ width: 80, textAlign: 'right' }} /><span className="muted">%</span></span>
            </Field>
            <span className="muted" style={{ fontSize: 'var(--text-sm)', maxWidth: 420 }}>
              Plus the run thresholds below (failed requests, p95). Dependency errors above 10% always block the next level.
            </span>
          </div>
        )}
      </Card>

      {/* 3 · traffic mix */}
      <Section title="Traffic mix" subtitle={`Pick the endpoints and how much of the load each gets · at peak ≈ ${compact(peakRps)} req/s`}
        actions={<>
          <Button size="sm" disabled={!chosen.length} onClick={() => set({ weights: productionMix(eps, st.steps) })}>Match production</Button>
          <Button size="sm" disabled={!chosen.length} onClick={() => set({ weights: Object.fromEntries(st.steps.map((k) => [k, Math.round((100 / st.steps.length) * 10) / 10])) })}>Spread evenly</Button>
        </>}>
        <Card flush>
          <table className="table">
            <thead><tr>
              <th style={{ width: 32 }} /><th>Endpoint</th><th className="is-num">Production share</th><th className="is-num">Weight (%)</th><th className="is-num">At peak (req/s)</th><th>Request data</th>
            </tr></thead>
            <tbody>
              {eps.map((e) => {
                const k = keyOf(e);
                const on = st.steps.includes(k);
                const w = st.weights[k] ?? 0;
                const req = st.requests[k];
                return (
                  <tr key={k} style={on ? undefined : { opacity: 0.6 }}>
                    <td><input type="checkbox" aria-label={`Include ${k}`} checked={on} onChange={() => toggleStep(e)} /></td>
                    <td><div className="row" style={{ gap: 'var(--space-2)', whiteSpace: 'nowrap' }}><MethodBadge method={e.method} /><span className="mono">{e.route}</span>{st.services.length > 1 && <span className="muted" style={{ fontSize: 'var(--text-xs)', whiteSpace: 'nowrap' }}>{e.serviceId}</span>}</div></td>
                    <td className="is-num muted">{pct(e.rpm / rpmSum, 1)}</td>
                    <td className="is-num">{on
                      ? <Input type="number" min={0} max={100} step="0.1" value={w} onChange={(ev) => set({ weights: { ...st.weights, [k]: Math.max(0, Number(ev.target.value) || 0) } })} style={{ width: 88, textAlign: 'right' }} />
                      : <span className="muted">—</span>}</td>
                    <td className="is-num">{on
                      ? <span className="row" style={{ gap: 6, justifyContent: 'flex-end' }}>
                          <Input type="number" min={0} aria-label={`Requests per second for ${k} at peak`} value={Math.round((peakRps * w) / 100)}
                            onChange={(ev) => set({ weights: { ...st.weights, [k]: peakRps ? Math.round(((Math.max(0, Number(ev.target.value) || 0) / peakRps) * 100) * 10) / 10 : 0 } })}
                            style={{ width: 88, textAlign: 'right' }} />
                          <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>req/s</span>
                        </span>
                      : <span className="muted">—</span>}</td>
                    <td>{!on ? <span className="muted">—</span>
                      : busy[k] ? <span className="row muted" style={{ gap: 6, fontSize: 'var(--text-sm)' }}><Loader2 size={12} className="spin" />Generating…</span>
                      : req?.source ? <Badge tone="accent" icon={<Sparkles />}>From production</Badge>
                      : missingData.includes(e) ? <Badge tone="warning">Needs values</Badge> : <Badge>Manual</Badge>}</td>
                  </tr>
                );
              })}
            </tbody>
            {chosen.length > 0 && (
              <tfoot><tr>
                <td /><td className="secondary">{chosen.length} endpoints</td><td />
                <td className="is-num" style={{ fontWeight: 600, color: Math.abs(weightSum - 100) < 0.5 ? undefined : 'var(--status-critical-text)' }}>{weightSum}%</td>
                <td className="is-num">{compact(peakRps)} req/s</td>
                <td className="muted" style={{ fontSize: 'var(--text-xs)' }}>{Math.abs(weightSum - 100) < 0.5 ? '' : 'weights must add up to 100%'}</td>
              </tr></tfoot>
            )}
          </table>
        </Card>
      </Section>

      {/* 4 · request data */}
      <Section title="Request data" subtitle={<>What each request carries. Values can use <span className="mono">{'{{pool.order_ids}}'}</span>, <span className="mono">{'{{weighted a:70 b:30}}'}</span>, <span className="mono">{'{{int 1-3}}'}</span> or a previous step's response <span className="mono">{'{{prev "POST /v1/orders" .id}}'}</span></>}
        actions={<Button size="sm" variant="primary" icon={<Sparkles size={14} />} disabled={!chosen.length || chosen.every((e) => busy[keyOf(e)])} onClick={() => generate(chosen)}>Generate all from production</Button>}>
        {chosen.length === 0 && <Card><EmptyState icon={<Plus />} title="No endpoints yet">Pick endpoints in the traffic mix above.</EmptyState></Card>}
        <div className="stack" style={{ gap: 'var(--space-3)' }}>
          {chosen.map((e) => {
            const k = keyOf(e);
            const r = st.requests[k];
            const expanded = open[k] ?? false;
            return (
              <Card key={k} flush>
                <div className="row" style={{ gap: 'var(--space-3)', padding: 'var(--space-3) var(--space-4)', cursor: 'pointer' }} onClick={() => setOpen({ ...open, [k]: !expanded })}>
                  {expanded ? <ChevronDown size={16} className="muted" /> : <ChevronRight size={16} className="muted" />}
                  <MethodBadge method={e.method} /><span className="mono">{e.route}</span>
                  <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>{st.weights[k] ?? 0}% of requests</span>
                  <span className="spacer" />
                  {busy[k]
                    ? <span className="row muted" style={{ gap: 6, fontSize: 'var(--text-sm)' }}><Loader2 size={12} className="spin" />Reading {compact(Math.round(e.rpm * 60 * 24 * 7 * 0.01))} sampled production requests…</span>
                    : r?.source
                      ? <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>From {compact(r.source.samples)} production requests · {r.source.window}</span>
                      : missingData.includes(e) ? <Badge tone="warning">Needs values</Badge> : null}
                  <Button size="sm" icon={<Sparkles size={12} />} disabled={busy[k]} onClick={(ev) => { ev.stopPropagation(); generate([e]); }}>
                    {r?.source ? 'Regenerate' : 'Generate from production'}
                  </Button>
                </div>
                {expanded && r && (
                  <div className="stack" style={{ gap: 'var(--space-3)', padding: '0 var(--space-4) var(--space-4)', borderTop: '1px solid var(--border)', paddingTop: 'var(--space-3)' }}>
                    <div className={hasBody(e.method) ? 'grid grid-3' : 'grid grid-2'} style={{ gap: 'var(--space-3)' }}>
                      <Field label="Path & query params" hint="name: value, one per line">
                        <Textarea mono wrap="off" rows={8} value={r.params} placeholder="id: {{pool.order_ids}}" onChange={(ev) => setReq(k, { params: ev.target.value })} />
                      </Field>
                      <Field label="Headers" hint="Header: value, one per line">
                        <Textarea mono wrap="off" rows={8} value={r.headers} onChange={(ev) => setReq(k, { headers: ev.target.value })} />
                      </Field>
                      {hasBody(e.method) && (
                        <Field label="Body (JSON template)">
                          <Textarea mono wrap="off" rows={8} value={r.body} onChange={(ev) => setReq(k, { body: ev.target.value })} />
                        </Field>
                      )}
                    </div>
                    {(r.pools.length > 0 || r.source) && (
                      <div className="grid grid-2" style={{ gap: 'var(--space-3)' }}>
                        {r.pools.length > 0 && (
                          <div>
                            <div className="eyebrow" style={{ marginBottom: 'var(--space-1)' }}>Data pools</div>
                            <ul className="stack" style={{ gap: 2, listStyle: 'none', margin: 0, padding: 0, fontSize: 'var(--text-sm)' }}>
                              {r.pools.map((p) => <li key={p.name}><span className="mono">{p.name}</span> <span className="muted">— {p.detail}</span></li>)}
                            </ul>
                          </div>
                        )}
                        {r.source && (
                          <div>
                            <div className="eyebrow" style={{ marginBottom: 'var(--space-1)' }}>How it was generated</div>
                            <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>
                              Distributions for params, headers and body fields come from {compact(r.source.samples)} production requests ({r.source.window}).
                              Never copied: <span className="mono">{r.source.masked.join(', ')}</span> — those fields use staging data pools instead. Production p95 for this endpoint: {ms(e.p95)}.
                            </div>
                          </div>
                        )}
                      </div>
                    )}
                  </div>
                )}
              </Card>
            );
          })}
        </div>
      </Section>

      {/* 5 · thresholds + file */}
      <div className="grid grid-3">
        <Card title="Thresholds" subtitle="the run fails if any is crossed">
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <Field label="Failed requests below (%)"><Input type="number" step="0.1" min={0} value={st.err} onChange={(e) => set({ err: e.target.value })} /></Field>
            <Field label="p95 below (ms)" hint={chosen.length ? `production p95 of the slowest step: ${ms(Math.max(...chosen.map((e) => e.p95)))}` : 'optional'}>
              <Input type="number" min={1} value={st.p95} placeholder="optional" onChange={(e) => set({ p95: e.target.value })} />
            </Field>
          </div>
        </Card>
        <Card className="span-2" title="Scenario file" subtitle={`loadtests/${slug(st.name) || 'untitled'}.yaml`}>
          <CodeBlock>{yaml}</CodeBlock>
        </Card>
      </div>
    </>
  );
}
