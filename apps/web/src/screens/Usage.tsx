import { useState } from 'react';
import { useNavigate } from 'react-router';
import { Check, Infinity as InfinityIcon, Pencil, ShieldAlert, Sparkles, X } from 'lucide-react';
import { BarList, Columns } from '@/components/charts';
import {
  Avatar, Badge, Button, Callout, Card, EmptyState, Field, Input, Meter, MetricCard, Section, Segmented, Select, Table,
} from '@/components/ui';
import { useData } from '@/data';
import { can, denied, useAuth } from '@/data/auth';
import {
  aiQuotaSettings, aiUsage, featureLabel, meAi, PLAN_CREDITS, planLabel, recentPeriods, setMemberAiQuota, setOrgAiQuota, usedRatio,
  type AiMeter, type AiUsage, type AiUserUsage,
} from '@/data/usage';
import { NotifyPicker } from './NotifyPicker';

// ---------------------------------------------------------------- formatting

/** Credits: whole numbers with separators; two decimals under 100. */
export const credits = (v: number) => (Math.abs(v) < 100 ? Number(v.toFixed(2)).toLocaleString('en-US') : Math.round(v).toLocaleString('en-US'));
const periodLabel = (p: string) => { const [y, m] = p.split('-').map(Number); return new Date(Date.UTC(y, m - 1, 1)).toLocaleDateString('en-US', { month: 'long', year: 'numeric', timeZone: 'UTC' }); };
const shortDate = (ts: number) => new Date(ts).toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
const meterTone = (r: number | undefined) => (r === undefined ? undefined : r >= 1 ? 'critical' : r >= 0.8 ? 'warning' : undefined);
const meterLabel = (m: AiMeter) => (m.limit === null ? `${credits(m.used)} · unlimited` : `${credits(m.used)} / ${credits(m.limit)}`);

// ---------------------------------------------------------------- Settings → Usage

export function UsageTab() {
  useData();
  const { org } = useAuth();
  const manage = can('manage');
  const periods = recentPeriods(6);
  const [month, setMonth] = useState(periods[0]);
  const u = aiUsage(month === periods[0] ? undefined : month);

  const picker = <Select aria-label="Month" value={month} onChange={(e) => setMonth(e.target.value)} options={periods.map((p) => ({ value: p, label: periodLabel(p) }))} />;
  if (!u) {
    return <Section title="AI usage" actions={picker}><Card><EmptyState icon={<Sparkles />} title="Loading usage…" /></Card></Section>;
  }
  const orgRatio = usedRatio(u.org);
  const meRatio = usedRatio(u.me);
  const current = month === periods[0];
  const daily = u.daily.series.find((s) => s.id === 'credits') ?? u.daily.series[0];

  return (
    <>
      <Section title="AI usage" subtitle={`${periodLabel(u.period)} · ${planLabel(u.plan)} plan · ${current ? `resets ${shortDate(u.resetsAt)}` : 'closed period'} · 1 credit = $0.01 of model cost`} actions={picker}>
        <div className="grid grid-3">
          <MetricCard className="metric--hero span-2" label="Organization credits"
            value={u.org.limit === null ? credits(u.org.used) : `${credits(u.org.used)} / ${credits(u.org.limit)}`}
            badge={u.org.limit === null ? <Badge icon={<InfinityIcon />}>Unlimited</Badge>
              : orgRatio !== undefined && orgRatio >= 1 ? <Badge tone="critical" icon={<ShieldAlert />}>Limit reached</Badge>
              : orgRatio !== undefined && orgRatio >= 0.8 ? <Badge tone="warning" icon={<ShieldAlert />}>{Math.round(orgRatio * 100)}% used</Badge> : undefined}
            hint={u.org.limit === null
              ? 'No monthly cap — usage is recorded for reporting only'
              : <>{credits(u.org.remaining ?? Math.max(0, u.org.limit - u.org.used))} credits left{current ? ` until ${shortDate(u.resetsAt)}` : ''}. When they run out, analyses continue without AI (rule-based criticality).</>}>
            {orgRatio !== undefined && <div style={{ paddingTop: 'var(--space-2)' }}><Meter value={Math.min(1, orgRatio)} tone={meterTone(orgRatio)} /></div>}
          </MetricCard>
          <MetricCard label="Your usage" value={meterLabel(u.me)}
            hint={u.me.limit === null ? 'No personal cap — the organization limit applies' : 'Personal cap set by an admin; it covers analyses you start'}>
            {meRatio !== undefined && <div style={{ paddingTop: 'var(--space-2)' }}><Meter value={Math.min(1, meRatio)} tone={meterTone(meRatio)} /></div>}
          </MetricCard>
        </div>
      </Section>

      <Section title="Credits per day">
        <Card>
          {daily && daily.data.length > 0
            ? <Columns timestamps={u.daily.timestamps} height={200} format={(v) => credits(v)} series={[{ id: 'credits', label: 'Credits', data: daily.data, color: 'var(--series-1)' }]} />
            : <EmptyState icon={<Sparkles />} title="No AI usage this month" />}
        </Card>
      </Section>

      <Section title="Where the credits went">
        <div className="grid grid-2">
          <Card title="By feature" subtitle="Credits · calls">
            {u.byFeature.length
              ? <BarList items={[...u.byFeature].sort((a, b) => b.credits - a.credits).map((f) => ({ id: f.feature, label: featureLabel(f.feature), value: f.credits, display: `${credits(f.credits)} · ${f.calls.toLocaleString('en-US')} calls` }))} />
              : <p className="muted" style={{ margin: 0 }}>Nothing yet.</p>}
          </Card>
          <Card title="By project" subtitle="Credits">
            {u.byProject.length
              ? <BarList items={[...u.byProject].sort((a, b) => b.credits - a.credits).map((p) => ({ id: p.projectId, label: p.name, value: p.credits, display: credits(p.credits) }))} />
              : <p className="muted" style={{ margin: 0 }}>Nothing yet.</p>}
          </Card>
        </div>
      </Section>

      {manage && <ByUser u={u} orgId={org?.id} editable={current} />}
      {manage && org && current && <OrgQuotaCard key={`${u.period}-${u.org.limit}`} u={u} orgId={org.id} />}
      {!manage && <Callout icon={<ShieldAlert size={16} />}>{denied('manage')} to see usage per member, change the monthly limit or the notification targets.</Callout>}
    </>
  );
}

function ByUser({ u, orgId, editable }: { u: AiUsage; orgId?: string; editable: boolean }) {
  const auto = Math.max(0, u.org.used - u.byUser.reduce((a, x) => a + x.used, 0));
  return (
    <Section title="By member" subtitle={auto >= 0.01 ? `Automatic work (push webhooks, scheduled analyses) used ${credits(auto)} credits and counts only against the organization` : 'Credits of the analyses each member started'}>
      <Card flush>
        <Table<AiUserUsage> rows={u.byUser} rowKey={(x) => x.userId} defaultSort={{ key: 'used', dir: 'desc' }} empty={<EmptyState icon={<Sparkles />} title="No member usage this month" />} columns={[
          { key: 'who', header: 'Member', sortValue: (x) => x.name ?? x.email, render: (x) => (
            <div className="row" style={{ gap: 'var(--space-3)', minWidth: 0 }}>
              <Avatar name={x.name ?? x.email} size="sm" />
              <div style={{ minWidth: 0 }}><div>{x.name ?? x.email}</div>{x.name && <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{x.email}</div>}</div>
            </div>) },
          { key: 'used', header: 'Used', numeric: true, sortValue: (x) => x.used, render: (x) => credits(x.used) },
          { key: 'cap', header: 'Monthly cap', width: 280, sortValue: (x) => x.limit ?? Number.MAX_SAFE_INTEGER, render: (x) => <AiCapEditor row={x} orgId={orgId} editable={editable} /> },
        ]} />
      </Card>
    </Section>
  );
}

/**
 * Per-member monthly AI credit cap: meter + "used / cap", pencil → inline number input (empty = no cap).
 * Used in Settings → Usage and the Members table (admin+).
 */
export function AiCapEditor({ row, orgId, editable = true }: { row: AiMeter & { userId: string; email: string }; orgId?: string; editable?: boolean }) {
  const [editing, setEditing] = useState(false);
  const [value, setValue] = useState(row.limit === null ? '' : String(row.limit));
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const ratio = usedRatio(row);
  const save = async () => {
    const v = value.trim();
    const n = v === '' ? null : Number(v);
    if (n !== null && (!Number.isFinite(n) || n < 0)) return setError('Enter a positive number of credits, or leave empty for no cap.');
    if (!orgId) return setError('No organization selected.');
    setBusy(true); setError(undefined);
    try { await setMemberAiQuota(orgId, row.userId, n === null ? null : Math.round(n)); setEditing(false); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  if (editing) {
    return (
      <div className="stack" style={{ gap: 'var(--space-1)' }}>
        <div className="row" style={{ gap: 'var(--space-1)' }}>
          <Input mono autoFocus inputMode="numeric" aria-label={`Monthly AI credit cap of ${row.email}`} placeholder="No cap" value={value} style={{ width: 'calc(var(--space-12) * 2)' }}
            onChange={(e) => setValue(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter') void save(); if (e.key === 'Escape') setEditing(false); }} />
          <Button size="sm" variant="primary" iconOnly icon={<Check size={14} />} aria-label="Save cap" disabled={busy} onClick={() => void save()} />
          <Button size="sm" variant="ghost" iconOnly icon={<X size={14} />} aria-label="Cancel" disabled={busy} onClick={() => { setEditing(false); setError(undefined); }} />
        </div>
        {error && <span style={{ color: 'var(--status-critical-text)', fontSize: 'var(--text-xs)' }}>{error}</span>}
      </div>
    );
  }
  return (
    <div className="row" style={{ gap: 'var(--space-2)' }}>
      <div style={{ flex: 1, minWidth: 0 }}>
        <div className="num" style={{ fontSize: 'var(--text-sm)' }}>{row.limit === null ? <span className="muted">No cap</span> : `${credits(row.used)} / ${credits(row.limit)}`}</div>
        {ratio !== undefined && <Meter value={Math.min(1, ratio)} tone={meterTone(ratio)} />}
      </div>
      {editable && <Button size="sm" variant="ghost" iconOnly icon={<Pencil size={14} />} aria-label={`Edit AI credit cap of ${row.email}`}
        onClick={() => { setValue(row.limit === null ? '' : String(row.limit)); setEditing(true); }} />}
    </div>
  );
}

/** Members table cell: the member's row from `GET /usage/ai` (admin+). */
export function MemberAiCell({ userId, email, orgId }: { userId: string; email: string; orgId?: string }) {
  const u = aiUsage();
  const row = u?.byUser.find((x) => x.userId === userId || x.email === email) ?? { userId, email, limit: null, used: 0 };
  return <AiCapEditor key={`${row.userId}-${row.limit}`} row={row} orgId={orgId} />;
}

type LimitMode = 'plan' | 'custom' | 'unlimited';

function OrgQuotaCard({ u, orgId }: { u: AiUsage; orgId: string }) {
  const s = aiQuotaSettings(u);
  const planDefault = PLAN_CREDITS[u.plan] ?? PLAN_CREDITS.free;
  const [mode, setMode] = useState<LimitMode>(s.monthlyCredits === null ? 'plan' : s.monthlyCredits === -1 ? 'unlimited' : 'custom');
  const [amount, setAmount] = useState(String(s.monthlyCredits !== null && s.monthlyCredits >= 0 ? s.monthlyCredits : planDefault ?? 5000));
  const [notify, setNotify] = useState<string[]>(s.notify);
  const [busy, setBusy] = useState(false);
  const [msg, setMsg] = useState<{ ok: boolean; text: string }>();
  const save = async () => {
    const n = Number(amount);
    if (mode === 'custom' && (!Number.isFinite(n) || n < 0)) return setMsg({ ok: false, text: 'Enter a positive number of credits.' });
    setBusy(true); setMsg(undefined);
    try {
      await setOrgAiQuota(orgId, { monthlyCredits: mode === 'plan' ? null : mode === 'unlimited' ? -1 : Math.round(n), notify });
      setMsg({ ok: true, text: 'Saved.' });
    } catch (e) { setMsg({ ok: false, text: e instanceof Error ? e.message : String(e) }); }
    setBusy(false);
  };
  return (
    <Section title="Limit and alerts" subtitle="Applies to every project of the organization">
      <Card>
        <div className="stack" style={{ gap: 'var(--space-4)' }}>
          <Field label="Monthly credits" hint={mode === 'plan' ? `${planLabel(u.plan)} plan default: ${planDefault === null ? 'unlimited' : `${credits(planDefault)} credits`}.` : mode === 'unlimited' ? 'No cap. Usage is still recorded.' : 'Soft limit: at most one in-flight call can go over it.'}>
            <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
              <Segmented label="Monthly credits" value={mode} onChange={setMode}
                options={[{ id: 'plan', label: 'Plan default' }, { id: 'custom', label: 'Custom' }, { id: 'unlimited', label: 'Unlimited' }]} />
              {mode === 'custom' && <Input mono inputMode="numeric" aria-label="Monthly credits" value={amount} onChange={(e) => setAmount(e.target.value)} style={{ width: 'calc(var(--space-12) * 2)' }} />}
            </div>
          </Field>
          <div className="field">
            <span className="field__label">Notify at 80% and 100%</span>
            <NotifyPicker value={notify} onChange={setNotify} />
            <span className="field__hint">One message per threshold per month, e.g. “AI usage at 80% of this month’s credits (4,012 / 5,000). Resets {shortDate(u.resetsAt)}.”</span>
          </div>
          <div className="row" style={{ gap: 'var(--space-3)' }}>
            <Button variant="primary" disabled={busy} onClick={() => void save()}>Save</Button>
            {msg && <span style={{ fontSize: 'var(--text-sm)', color: msg.ok ? 'var(--status-good-text)' : 'var(--status-critical-text)' }}>{msg.text}</span>}
          </div>
        </div>
      </Card>
    </Section>
  );
}

// ---------------------------------------------------------------- app-level banner

const dismissKey = (period: string, level: number) => `trooper-ai-banner:${period}:${level}`;
function readDismissed(key: string): boolean {
  try { return localStorage.getItem(key) === '1'; } catch { return false; }
}

/** Shown above every page when the org has used ≥ 80% of its monthly AI credits (`/me` → `ai`). Dismissed per period and threshold. */
export function AiQuotaBanner() {
  useData();
  const nav = useNavigate();
  const ai = meAi();
  const ratio = ai ? usedRatio(ai) : undefined;
  const level = ratio === undefined ? 0 : ratio >= 1 ? 100 : ratio >= 0.8 ? 80 : 0;
  const key = ai ? dismissKey(ai.period, level) : '';
  const [dismissed, setDismissed] = useState<string[]>([]);
  if (!ai || level === 0 || dismissed.includes(key) || readDismissed(key)) return null;
  const dismiss = () => {
    try { localStorage.setItem(key, '1'); } catch { /* storage unavailable: hide for this session */ }
    setDismissed((d) => [...d, key]);
  };
  return (
    <div style={{ marginBottom: 'var(--space-4)' }}>
      <Callout icon={<ShieldAlert size={16} color={level === 100 ? 'var(--status-critical)' : 'var(--status-warning)'} />}>
        <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
          <Badge tone={level === 100 ? 'critical' : 'warning'}>{level === 100 ? 'AI credits used up' : `AI credits ${Math.round((ratio ?? 0) * 100)}% used`}</Badge>
          <span className="secondary" style={{ flex: 1, minWidth: 0 }}>
            {credits(ai.used)} of {credits(ai.limit ?? 0)} credits this month.{' '}
            {level === 100 ? 'Code analyses continue without AI until the limit resets.' : 'At 100%, code analyses continue without AI.'}
          </span>
          <Button size="sm" variant="ghost" onClick={() => nav('/settings/usage')}>View usage</Button>
          <Button size="sm" variant="ghost" iconOnly icon={<X size={14} />} aria-label="Dismiss AI usage notice" onClick={dismiss} />
        </div>
      </Callout>
    </div>
  );
}
