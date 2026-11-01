/**
 * AI usage quota (ADR-018, .ai/contracts.md §19): monthly credits per org and member, where they went,
 * and the admin controls (org override, notify targets, per-member cap).
 *
 * `GET /usage/ai?month=`, `GET /me` (`ai`), `PUT /orgs/{org}/ai-quota`, `PUT /orgs/{org}/members/{user}/ai-quota`.
 */
import { invalidate, mutate, qs, remote, seg } from './remote';
import type { SeriesSet } from './api/apm';

// ---------------------------------------------------------------- types

/** `limit: null` = unlimited. Credits: 1 credit = 0.01 USD of model cost. */
export interface AiMeter { limit: number | null; used: number }
export interface AiUserUsage extends AiMeter { userId: string; email: string; name?: string }
export type AiFeature = 'codeintel.enrich' | 'codeintel.suggest' | 'rca' | 'other' | (string & {});
export interface AiUsage {
  /** YYYY-MM */
  period: string;
  /** epoch ms, start of next period */
  resetsAt: number;
  plan: string;
  org: AiMeter & { remaining: number | null };
  me: AiMeter;
  /** admin+ only; [] for others */
  byUser: AiUserUsage[];
  byFeature: { feature: AiFeature; credits: number; calls: number }[];
  byProject: { projectId: string; name: string; credits: number }[];
  /** id 'credits', one bucket per day of the period */
  daily: SeriesSet;
}
/** `GET /me` → `ai` */
export interface MeAi { period: string; limit: number | null; used: number; userLimit: number | null; userUsed: number }
/** Org settings as edited in the Usage tab. `monthlyCredits`: number · null = plan default · -1 = unlimited. */
export interface AiQuotaSettings { monthlyCredits: number | null; notify: string[] }

export const FEATURE_LABEL: Record<string, string> = {
  'codeintel.enrich': 'Code analysis', 'codeintel.suggest': 'Monitor suggestions', rca: 'Root cause analysis', other: 'Other',
};
export const featureLabel = (f: string) => FEATURE_LABEL[f] ?? f;
export const PLAN_CREDITS: Record<string, number | null> = { free: 100, team: 5000, enterprise: null };
export const planLabel = (p: string) => p.charAt(0).toUpperCase() + p.slice(1);

/** Ratio used of a meter; undefined when unlimited. */
export const usedRatio = (m: AiMeter | undefined): number | undefined =>
  !m || m.limit === null || m.limit <= 0 ? (m && m.limit === 0 ? 1 : undefined) : m.used / m.limit;

// ---------------------------------------------------------------- helpers

const num = (v: unknown, d = 0): number => (typeof v === 'number' && Number.isFinite(v) ? v : d);
const lim = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) && v >= 0 ? v : null);
const toMs = (v: unknown): number => {
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
  if (typeof v === 'string') { const t = Date.parse(v); return Number.isFinite(t) ? t : 0; }
  return 0;
};
const monthOf = (ts: number) => new Date(ts).toISOString().slice(0, 7);
const nextMonth = (period: string) => { const [y, m] = period.split('-').map(Number); return Date.UTC(m === 12 ? y + 1 : y, m === 12 ? 0 : m, 1); };

// ---------------------------------------------------------------- api mapping

interface RawMeter { limit?: number | null; used?: number | null; remaining?: number | null }
interface RawUsage {
  period?: string; resetsAt?: number | string; plan?: string; org?: RawMeter | null; me?: RawMeter | null;
  byUser?: { userId: string | number; email: string; name?: string | null; limit?: number | null; used?: number | null }[] | null;
  byFeature?: { feature: string; credits?: number; calls?: number }[] | null;
  byProject?: { projectId: string | number; name: string; credits?: number }[] | null;
  daily?: { timestamps?: number[] | null; series?: { id: string; label: string; data: number[] | null }[] | null } | null;
}
function mapUsage(r: RawUsage | null): AiUsage {
  const period = r?.period ?? monthOf(Date.now());
  const ts = (r?.daily?.timestamps ?? []).map(toMs);
  return {
    period, resetsAt: r?.resetsAt ? toMs(r.resetsAt) : nextMonth(period), plan: r?.plan ?? 'free',
    org: { limit: lim(r?.org?.limit), used: num(r?.org?.used), remaining: lim(r?.org?.remaining) },
    me: { limit: lim(r?.me?.limit), used: num(r?.me?.used) },
    byUser: (r?.byUser ?? []).map((u) => ({ userId: String(u.userId), email: u.email, name: u.name || undefined, limit: lim(u.limit), used: num(u.used) })),
    byFeature: (r?.byFeature ?? []).map((f) => ({ feature: f.feature, credits: num(f.credits), calls: num(f.calls) })),
    byProject: (r?.byProject ?? []).map((p) => ({ projectId: String(p.projectId), name: p.name, credits: num(p.credits) })),
    daily: { timestamps: ts, series: (r?.daily?.series ?? []).map((s) => ({ id: s.id, label: s.label, data: (s.data ?? []).map((v) => num(v)) })) },
  };
}
const EMPTY: AiUsage = {
  period: monthOf(Date.now()), resetsAt: nextMonth(monthOf(Date.now())), plan: 'free', org: { limit: null, used: 0, remaining: null },
  me: { limit: null, used: 0 }, byUser: [], byFeature: [], byProject: [], daily: { timestamps: [], series: [] },
};

// ---------------------------------------------------------------- reads

/** `GET /usage/ai?month=YYYY-MM` (default: this month). Undefined while loading. */
export function aiUsage(month?: string): AiUsage | undefined {
  return remote<AiUsage | undefined>(`/usage/ai${qs({ month })}`, undefined, (r: RawUsage | null) => mapUsage(r));
}
export const emptyUsage = EMPTY;

/** `/me` → `ai` (the banner's source). Undefined when the api has no session user or no `ai` field. */
export function meAi(): MeAi | undefined {
  return remote<MeAi | undefined>('/me', undefined, (r: { ai?: Partial<MeAi> | null } | null) => r?.ai ? ({
    period: r.ai.period ?? monthOf(Date.now()), limit: lim(r.ai.limit), used: num(r.ai.used), userLimit: lim(r.ai.userLimit), userUsed: num(r.ai.userUsed),
  }) : undefined);
}

/** Current org settings. The api returns them only through the usage limit; the override is inferred from plan vs limit. */
export function aiQuotaSettings(u: AiUsage | undefined): AiQuotaSettings {
  const def = u ? PLAN_CREDITS[u.plan] ?? PLAN_CREDITS.free : null;
  const limit = u?.org.limit ?? null;
  return { monthlyCredits: !u ? null : limit === null ? (def === null ? null : -1) : limit === def ? null : limit, notify: storedNotify ?? [] };
}
// the api has no GET for notify targets (contracts §19) — remember what was saved in this session
let storedNotify: string[] | undefined;

// ---------------------------------------------------------------- writes

/** `PUT /orgs/{org}/ai-quota` (admin+). */
export async function setOrgAiQuota(orgId: string, s: AiQuotaSettings): Promise<void> {
  await mutate('PUT', `/orgs/${seg(orgId)}/ai-quota`, { monthlyCredits: s.monthlyCredits, notify: s.notify }, ['/usage/ai', '/me', '/audit-log']);
  storedNotify = [...s.notify];
}

/** `PUT /orgs/{org}/members/{user}/ai-quota` (admin+). null = no cap. */
export async function setMemberAiQuota(orgId: string, userId: string, monthlyCredits: number | null): Promise<void> {
  await mutate('PUT', `/orgs/${seg(orgId)}/members/${seg(userId)}/ai-quota`, { monthlyCredits }, ['/usage/ai', `/orgs/${seg(orgId)}/members`, '/me', '/audit-log']);
  invalidate('/usage/ai');
}

/** Last N periods for the month picker, newest first. */
export function recentPeriods(n = 6, from = Date.now()): string[] {
  const d = new Date(from);
  return Array.from({ length: n }, (_, i) => new Date(Date.UTC(d.getUTCFullYear(), d.getUTCMonth() - i, 1)).toISOString().slice(0, 7));
}
