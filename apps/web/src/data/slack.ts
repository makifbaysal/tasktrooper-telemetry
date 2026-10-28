/**
 * Slack App (ADR-017, .ai/contracts.md §17): workspace connection + channel search for notification targets.
 * Targets are stored as `slack:<channel_id>#<channel_name>`; other targets (pagerduty:, email:, webhook URLs,
 * legacy `#channel` / `slack:https://hooks…`) stay free text.
 */
import { API_URL, isApi, mutate, notifyData, qs, remote } from './remote';

export interface SlackWorkspace { teamId: string; teamName: string; connectedAt?: number }
export interface SlackChannel { id: string; name: string; isPrivate: boolean; isMember: boolean }

// ---------------------------------------------------------------- mock

let mockWorkspace: SlackWorkspace | null = { teamId: 'T0SHOPCO', teamName: 'Shopco', connectedAt: Date.now() - 30 * 86400_000 };
const MOCK_CHANNELS: SlackChannel[] = [
  { id: 'C01PAYALRT', name: 'payments-alerts', isPrivate: false, isMember: true },
  { id: 'C02ORDONC', name: 'orders-oncall', isPrivate: false, isMember: true },
  { id: 'C03WEBTEAM', name: 'web-team', isPrivate: false, isMember: true },
  { id: 'C04BILLING', name: 'billing', isPrivate: false, isMember: false },
  { id: 'G05SRE', name: 'sre-private', isPrivate: true, isMember: true },
  { id: 'C06INCID', name: 'incidents', isPrivate: false, isMember: true },
  { id: 'G07SECOPS', name: 'security-ops', isPrivate: true, isMember: true },
  { id: 'C08GENERAL', name: 'general', isPrivate: false, isMember: false },
];

// ---------------------------------------------------------------- workspace

interface RawIntegration { id: string | number; provider: string; name?: string; createdAt?: number | string; config?: { teamId?: string; teamName?: string } | null }

/** Connected workspace, `null` when Slack isn't connected, `undefined` while loading. */
export function slackWorkspace(): SlackWorkspace | null | undefined {
  if (!isApi) return mockWorkspace;
  return remote<SlackWorkspace | null | undefined>(`/integrations${qs({ provider: 'slack' })}`, undefined, (raw: RawIntegration[] | { items?: RawIntegration[] } | null) => {
    const list = Array.isArray(raw) ? raw : raw?.items ?? [];
    const it = list.find((x) => x.provider === 'slack');
    if (!it) return null;
    const created = typeof it.createdAt === 'string' ? Date.parse(it.createdAt) : it.createdAt;
    return { teamId: it.config?.teamId ?? '', teamName: it.config?.teamName ?? it.name ?? 'Slack', connectedAt: created || undefined };
  });
}

/** Full-page redirect target: api → Slack OAuth → api callback → back to `redirect`. */
export function slackInstallUrl(redirect = window.location.href): string {
  return `${API_URL}/api/v1/integrations/slack/install${qs({ redirect })}`;
}

export function connectSlack() {
  if (!isApi) { mockWorkspace = { teamId: 'T0SHOPCO', teamName: 'Shopco', connectedAt: Date.now() }; notifyData(); return; }
  window.location.href = slackInstallUrl();
}

export async function disconnectSlack(): Promise<void> {
  if (!isApi) { mockWorkspace = null; notifyData(); return; }
  await mutate('DELETE', '/integrations/slack', undefined, ['/integrations']);
}

// ---------------------------------------------------------------- channels

const mapChannel = (c: Partial<SlackChannel> & { id: string; name: string }): SlackChannel => ({
  id: c.id, name: c.name.replace(/^#/, ''), isPrivate: !!c.isPrivate, isMember: c.isMember ?? true,
});
/** Channels matching `q` (api caches 5 min). Empty when Slack isn't connected. */
export function slackChannels(q: string): SlackChannel[] {
  const needle = q.trim().replace(/^#/, '').toLowerCase();
  if (!isApi) return mockWorkspace ? MOCK_CHANNELS.filter((c) => c.name.includes(needle)) : [];
  return remote(`/integrations/slack/channels${qs({ q: needle })}`, [], (raw: SlackChannel[] | { channels?: SlackChannel[] } | null) =>
    (Array.isArray(raw) ? raw : raw?.channels ?? []).map(mapChannel));
}

// ---------------------------------------------------------------- targets

export type NotifyTarget =
  | { kind: 'slack'; id: string; name: string; raw: string }
  | { kind: 'other'; raw: string };

const SLACK_RE = /^slack:([A-Z0-9]+)#(.+)$/;
export function parseTarget(raw: string): NotifyTarget {
  const m = SLACK_RE.exec(raw);
  return m ? { kind: 'slack', id: m[1], name: m[2], raw } : { kind: 'other', raw };
}
export const slackTarget = (c: SlackChannel) => `slack:${c.id}#${c.name}`;
/** Human label: `#name` for Slack App targets, the raw value otherwise. */
export const targetLabel = (raw: string) => { const t = parseTarget(raw); return t.kind === 'slack' ? `#${t.name}` : raw; };
