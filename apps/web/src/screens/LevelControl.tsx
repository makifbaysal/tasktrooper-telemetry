import { useState } from 'react';
import { AlertTriangle, ArrowDown, ArrowUp, Bot, Check, CheckCircle2, Clock, Pause, Play, Sparkles, Timer, User, XCircle } from 'lucide-react';
import { Badge, Button, Card } from '@/components/ui';
import type { LoadTestRun } from '@/data';
import {
  SPEC, controlRun, gateVerdict, levelState, type Check as GateCheck, type CheckStatus, type Decision, type LevelState, type Progression,
} from '@/lib/loadtest';

const MODE: Record<Progression, { label: string; icon: typeof Bot; hint: string }> = {
  ai: { label: 'AI-guided', icon: Sparkles, hint: 'AI raises the load only when the API and everything it calls stay healthy, and the next level is projected to stay within limits.' },
  manual: { label: 'Manual', icon: User, hint: 'Each level holds until you move on. The checks below tell you whether the next level is safe.' },
  auto: { label: 'Timer', icon: Timer, hint: 'Levels follow the plan on a timer. You can still hold, step back or skip ahead.' },
};

const clockOf = (sec: number) => `${Math.floor(sec / 60)}:${String(Math.floor(sec % 60)).padStart(2, '0')}`;
const elapsedOf = (run: LoadTestRun, at: number) => `+${clockOf(Math.max(0, (at - run.startedAt) / 1000))}`;

function StatusDot({ s }: { s: CheckStatus }) {
  if (s === 'bad') return <XCircle size={14} color="var(--status-critical)" aria-label="Over the limit" />;
  if (s === 'warn') return <AlertTriangle size={14} color="var(--status-warning)" aria-label="Close to the limit" />;
  return <CheckCircle2 size={14} color="var(--status-good)" aria-label="Healthy" />;
}

/**
 * Who moves the run to the next level and why: the level stepper, the AI gate verdict with its
 * reasons and advice, the health checks behind it, the controls, and a log of every decision.
 */
export function LevelControl({ run }: { run: LoadTestRun }) {
  const ls = levelState(run);
  const running = run.state === 'running';
  const unit = SPEC[run.scenarioId].mode === 'rps' ? 'req/s' : 'VUs';
  const g = running ? gateVerdict(run, ls.index) : undefined;
  const cur = ls.levels[ls.index];
  const next = ls.levels[ls.index + 1];
  const goingUp = !!next && next.target > cur.target;
  const aiHeld = running && ls.mode === 'ai' && ls.phase === 'waiting' && !ls.userHold;
  const youDecide = running && ls.mode === 'manual' && ls.phase === 'waiting' && !ls.userHold;
  const M = MODE[ls.mode];

  const phaseText = !running ? (run.state === 'aborted' ? 'stopped' : 'done')
    : ls.userHold ? 'held by you'
    : aiHeld ? 'held by AI'
    : youDecide ? 'waiting for you'
    : ls.phase === 'ramp' ? `ramping · ${clockOf(ls.left ?? 0)} left`
    : `holding · ${clockOf(ls.left ?? 0)} left`;

  return (
    <Card>
      <div className="stack" style={{ gap: 'var(--space-4)' }}>
        <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <span className="eyebrow">Level control</span>
          <Badge tone={ls.mode === 'ai' ? 'accent' : 'neutral'} icon={<M.icon />}>{M.label}</Badge>
          <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>now {Math.round(ls.now)} {unit}</span>
          <span className="spacer" />
          {running && <>
            <Button size="sm" icon={<ArrowDown size={14} />} disabled={ls.index === 0} onClick={() => controlRun(run.id, 'back')}>Step back</Button>
            {ls.userHold
              ? <Button size="sm" icon={<Play size={14} />} onClick={() => controlRun(run.id, 'resume')}>Continue</Button>
              : <Button size="sm" icon={<Pause size={14} />} disabled={aiHeld || youDecide} onClick={() => controlRun(run.id, 'hold')}>Hold here</Button>}
            {next && (
              <Button size="sm" variant={g?.verdict === 'go' || !goingUp ? 'primary' : 'secondary'} icon={goingUp ? <ArrowUp size={14} /> : <ArrowDown size={14} />}
                onClick={() => controlRun(run.id, 'next')}>
                {goingUp && g && g.verdict !== 'go' ? `Go to ${next.name} anyway` : `Go to ${next.name}`}
              </Button>
            )}
          </>}
        </div>

        <Stepper ls={ls} unit={unit} phaseText={phaseText} unsafeNext={goingUp && !!g && g.verdict !== 'go'} />

        {running && g && goingUp && <Guidance mode={ls.mode} g={g} curName={cur.name} nextName={next.name} held={aiHeld} yourCall={youDecide} />}
        {running && !goingUp && next && <div className="secondary" style={{ fontSize: 'var(--text-sm)' }}>Next is {next.name} — ramping down is always safe.</div>}
        <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{M.hint}</div>

        {g && goingUp && <Checks checks={g.checks} curName={cur.name} nextName={next.name} />}
        <DecisionLog run={run} decisions={ls.decisions} />
      </div>
    </Card>
  );
}

function Stepper({ ls, unit, phaseText, unsafeNext }: { ls: LevelState; unit: string; phaseText: string; unsafeNext: boolean }) {
  return (
    <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap', alignItems: 'stretch' }}>
      {ls.levels.map((l, i) => {
        const state = i < ls.index ? 'done' : i === ls.index ? 'current' : 'next';
        const danger = unsafeNext && i === ls.index + 1;
        return (
          <div key={i} style={{
            flex: '1 1 120px', minWidth: 120, padding: 'var(--space-2) var(--space-3)', borderRadius: 'var(--radius-md)',
            border: `1px solid ${state === 'current' ? 'var(--accent)' : danger ? 'var(--status-critical)' : 'var(--border)'}`,
            background: state === 'current' ? 'var(--accent-soft)' : 'transparent', opacity: state === 'next' && !danger ? 0.7 : 1,
          }}>
            <div className="row" style={{ gap: 6, fontSize: 'var(--text-sm)', fontWeight: state === 'current' ? 600 : 500 }}>
              {state === 'done' && <Check size={12} color="var(--status-good)" aria-hidden />}
              {danger && <XCircle size={12} color="var(--status-critical)" aria-hidden />}
              <span className="truncate">{l.name}</span>
            </div>
            <div className="num" style={{ fontSize: 'var(--text-lg)', fontWeight: 600 }}>{l.target} <span className="muted" style={{ fontSize: 'var(--text-xs)', fontWeight: 400 }}>{unit}</span></div>
            <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>
              {state === 'current' ? phaseText : danger ? 'not safe yet' : `${l.ramp ? `ramp ${clockOf(l.ramp)}` : ''}${l.ramp && l.hold ? ' · ' : ''}${l.hold ? `hold ${clockOf(l.hold)}` : ''}`}
            </div>
          </div>
        );
      })}
    </div>
  );
}

function Guidance({ mode, g, curName, nextName, held, yourCall }: {
  mode: Progression; g: ReturnType<typeof gateVerdict>; curName: string; nextName: string; held: boolean; yourCall: boolean;
}) {
  const tone = g.verdict === 'go' ? 'good' : g.verdict === 'hold' ? 'warning' : 'critical';
  const title = g.verdict === 'go'
    ? `${nextName} looks safe${yourCall ? ' — your call' : mode === 'ai' ? ' — AI will raise the load when this level ends' : ''}`
    : g.verdict === 'hold'
      ? `${held ? 'AI is holding at' : 'Recommend staying at'} ${curName} — ${nextName} is projected to break something`
      : `${curName} is already hurting a dependency — ${mode === 'ai' ? 'AI steps back when this level ends' : 'step back'}`;
  return (
    <div className={`card card--rail-${tone === 'good' ? 'warning' : tone}`} style={{
      padding: 'var(--space-3) var(--space-4)', boxShadow: 'none',
      ...(tone === 'good' ? { borderLeft: '4px solid var(--status-good)' } : {}),
    }}>
      <div className="row" style={{ gap: 'var(--space-2)', fontWeight: 600 }}>
        <Sparkles size={14} color="var(--accent)" aria-hidden />{title}
      </div>
      <ul className="stack" style={{ gap: 2, margin: 'var(--space-2) 0 0', paddingLeft: 'var(--space-5)', fontSize: 'var(--text-sm)' }}>
        {g.reasons.map((r) => <li key={r} className="secondary">{r}</li>)}
      </ul>
      {g.advice.length > 0 && (
        <div className="stack" style={{ gap: 2, marginTop: 'var(--space-2)', fontSize: 'var(--text-sm)' }}>
          {g.advice.map((a) => <div key={a}><strong>→</strong> {a}</div>)}
        </div>
      )}
    </div>
  );
}

function Checks({ checks, curName, nextName }: { checks: GateCheck[]; curName: string; nextName: string }) {
  const [all, setAll] = useState(false);
  const shown = all ? checks : checks.slice(0, 6);
  const counts = (k: 'statusNow' | 'statusNext') => ({ bad: checks.filter((c) => c[k] === 'bad').length, warn: checks.filter((c) => c[k] === 'warn').length });
  const now = counts('statusNow'), nxt = counts('statusNext');
  return (
    <div>
      <div className="row" style={{ gap: 'var(--space-3)', marginBottom: 'var(--space-2)' }}>
        <span className="eyebrow">Health checks · API and its dependencies</span>
        <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>
          {curName}: {now.bad} over, {now.warn} close · {nextName} (projected): {nxt.bad} over, {nxt.warn} close
        </span>
      </div>
      <table className="table">
        <thead><tr>
          <th>Check</th><th className="is-num">{curName} (now)</th><th className="is-num">{nextName} (projected)</th><th className="is-num">Limit</th>
        </tr></thead>
        <tbody>
          {shown.map((c) => (
            <tr key={c.id}>
              <td><span className="mono" style={{ fontSize: 'var(--text-sm)' }}>{c.scope}</span> <span className="muted">{c.label}</span></td>
              <td className="is-num"><span className="row" style={{ gap: 6, justifyContent: 'flex-end' }}>{c.fmt(c.now)}<StatusDot s={c.statusNow} /></span></td>
              <td className="is-num">{c.next === undefined ? <span className="muted">—</span> : <span className="row" style={{ gap: 6, justifyContent: 'flex-end' }}>{c.fmt(c.next)}<StatusDot s={c.statusNext!} /></span>}</td>
              <td className="is-num muted">{c.fmt(c.limit)}</td>
            </tr>
          ))}
        </tbody>
      </table>
      {checks.length > 6 && <Button size="sm" variant="ghost" onClick={() => setAll(!all)}>{all ? 'Show fewer' : `Show all ${checks.length} checks`}</Button>}
    </div>
  );
}

const ACTOR: Record<Decision['actor'], { label: string; icon: typeof Bot }> = {
  ai: { label: 'AI', icon: Sparkles }, you: { label: 'You', icon: User }, timer: { label: 'Timer', icon: Clock },
};

function DecisionLog({ run, decisions }: { run: LoadTestRun; decisions: Decision[] }) {
  if (!decisions.length) return null;
  return (
    <div>
      <div className="eyebrow" style={{ marginBottom: 'var(--space-2)' }}>Decisions</div>
      <ol className="stack" style={{ gap: 'var(--space-2)', listStyle: 'none', margin: 0, padding: 0 }}>
        {[...decisions].reverse().map((d, k) => {
          const A = ACTOR[d.actor];
          return (
            <li key={k} className="row" style={{ gap: 'var(--space-3)', alignItems: 'flex-start', fontSize: 'var(--text-sm)' }}>
              <span className="mono muted" style={{ width: 52, flexShrink: 0 }}>{elapsedOf(run, d.at)}</span>
              <Badge tone={d.actor === 'ai' ? 'accent' : 'neutral'} icon={<A.icon />}>{A.label}</Badge>
              <div style={{ minWidth: 0 }}>
                <div style={{ color: d.kind === 'hold' || d.kind === 'back' ? 'var(--status-warning-text)' : undefined }}>{d.text}</div>
                {d.reasons && d.kind !== 'advance' && <div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{d.reasons.slice(0, 2).join(' · ')}{d.reasons.length > 2 ? ` · +${d.reasons.length - 2} more` : ''}</div>}
              </div>
            </li>
          );
        })}
      </ol>
    </div>
  );
}
