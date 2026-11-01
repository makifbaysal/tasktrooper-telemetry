/**
 * Slack App (ADR-017, .ai/contracts.md §17): workspace connection + channel search for notification targets.
 * Targets are stored as `slack:<channel_id>#<channel_name>`; other targets (pagerduty:, email:, webhook URLs,
 * legacy `#channel` / `slack:https://hooks…`) stay free text.
 */
import { API_URL, ApiError, api, invalidate, mutate, qs, remote } from './remote';

export interface SlackWorkspace { teamId: string; teamName: string; connectedAt?: number }
export interface SlackChannel { id: string; name: string; isPrivate: boolean; isMember: boolean }

// ---------------------------------------------------------------- workspace

interface RawIntegration { id: string | number; provider: string; name?: string; createdAt?: number | string; config?: { teamId?: string; teamName?: string } | null }

/** Connected workspace, `null` when Slack isn't connected, `undefined` while loading. */
export function slackWorkspace(): SlackWorkspace | null | undefined {
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

/**
 * Ask the api for the Slack authorize URL first, so a server without the Slack App shows a readable message
 * instead of a raw JSON error page. Rejects with that message.
 */
export async function connectSlack(redirect = window.location.href): Promise<void> {
  try {
    const { url } = await api<{ url: string }>(`/integrations/slack/install-url${qs({ redirect })}`, { method: 'POST' });
    window.location.href = url;
  } catch (e) {
    if (e instanceof ApiError && e.status === 501) {
      throw new Error('Slack isn’t set up on this server yet: the operator has to register a Slack App and set TT_SLACK_CLIENT_ID, TT_SLACK_CLIENT_SECRET and TT_SLACK_SIGNING_SECRET on the api.');
    }
    throw e;
  }
}

/** Posts a test message to one channel; rejects with Slack's reason in plain words (bot not invited, token revoked, ...). */
export async function testSlack(channelId: string): Promise<void> {
  await api('/integrations/slack/test', { method: 'POST', body: JSON.stringify({ channel: channelId }) });
}

export async function disconnectSlack(): Promise<void> {
  await mutate('DELETE', '/integrations/slack', undefined, ['/integrations']);
}

// ---------------------------------------------------------------- channels

const mapChannel = (c: Partial<SlackChannel> & { id: string; name: string }): SlackChannel => ({
  id: c.id, name: c.name.replace(/^#/, ''), isPrivate: !!c.isPrivate, isMember: c.isMember ?? true,
});
/** Channels matching `q` (api caches 5 min). Empty when Slack isn't connected. */
export function slackChannels(q: string): SlackChannel[] {
  const needle = q.trim().replace(/^#/, '').toLowerCase();
  return remote(`/integrations/slack/channels${qs({ q: needle })}`, [], (raw: SlackChannel[] | { channels?: SlackChannel[] } | null) =>
    (Array.isArray(raw) ? raw : raw?.channels ?? []).map(mapChannel));
}

/** Re-reads channels from Slack (skips the api's 5 min cache): new channels, or private ones the bot was just invited to. */
export async function refreshSlackChannels(): Promise<void> {
  await api(`/integrations/slack/channels${qs({ refresh: 'true' })}`);
  invalidate('/integrations/slack/channels');
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
