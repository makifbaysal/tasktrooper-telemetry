import { useState } from 'react';
import { useLocation, useNavigate, useSearchParams } from 'react-router';
import { CheckCircle2, ChevronRight, Hash, KeyRound, Lock, MessageSquare, Plug, Send, ShieldAlert, Trash2, Unplug, UserPlus } from 'lucide-react';
import {
  Avatar, Badge, Button, Callout, Card, CopyCode, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, PageHeader,
  SdkQuickstart, SearchInput, Section, Select, StatusBadge, Table, Tabs, useDrawer,
} from '@/components/ui';
import { PROVIDERS, PROVIDER_LABEL, integrations, remoteStatus, useData } from '@/data';
import {
  apiKeys, auditLog, can, createApiKey, denied, inviteMember, membersOf, projectDsn, removeMember, revokeApiKey, setMemberRole, useAuth,
  type ApiKeyRow, type AuditEntry, type KeyScope, type Member, type Role,
} from '@/data/auth';
import { connectSlack, disconnectSlack, slackChannels, slackWorkspace, testSlack, type SlackChannel } from '@/data/slack';
import { IntegrationStatusBadge, ProviderMark } from './Integrations';
import { MemberAiCell, UsageTab } from './Usage';
import { ago, dateTime } from '@/lib/format';

type SettingsTab = 'project' | 'members' | 'usage' | 'keys' | 'audit';
const TABS: SettingsTab[] = ['project', 'members', 'usage', 'keys', 'audit'];

/** Error of a failed list read (e.g. 404 while the api has no such endpoint yet). */
function readError(prefix: string): string | undefined {
  return remoteStatus().errors.find((e) => e.path.startsWith(prefix))?.error;
}

export function SettingsScreen() {
  useData();
  const { project, org } = useAuth();
  const [params, setParams] = useSearchParams();
  const { pathname } = useLocation();
  const nav = useNavigate();
  // /settings/usage is the Usage tab's own address (AI quota notifications link to it, contracts §19)
  const onUsagePath = pathname.replace(/\/$/, '') === '/settings/usage';
  const raw = (onUsagePath ? 'usage' : params.get('tab')) as SettingsTab | null;
  const tab: SettingsTab = raw && TABS.includes(raw) ? raw : 'project';
  const setTab = (t: SettingsTab) => t === 'usage' ? nav('/settings/usage', { replace: true }) : onUsagePath ? nav(t === 'project' ? '/settings' : `/settings?tab=${t}`, { replace: true }) : setParams((p) => { const n = new URLSearchParams(p); if (t === 'project') n.delete('tab'); else n.set('tab', t); return n; }, { replace: true });

  return (
    <>
      <PageHeader title="Settings" subtitle={project ? `${org?.name ? `${org.name} · ` : ''}${project.name}` : 'Project of the api key'} />
      <Tabs value={tab} onChange={setTab} tabs={[
        { id: 'project', label: 'Project & SDK' },
        { id: 'members', label: 'Members' },
        { id: 'usage', label: 'Usage' },
        { id: 'keys', label: 'API keys' },
        { id: 'audit', label: 'Audit log' },
      ]} />
      {tab === 'project' && <ProjectTab />}
      {tab === 'members' && <MembersTab />}
      {tab === 'usage' && <UsageTab />}
      {tab === 'keys' && <KeysTab />}
      {tab === 'audit' && <AuditTab />}
    </>
  );
}

// ---------------------------------------------------------------- project & SDK

function ProjectTab() {
  const nav = useNavigate();
  const cloud = integrations.filter((i) => PROVIDERS.includes(i.provider));
  const { project } = useAuth();
  return (
    <>
      <Section title="Install the SDK" subtitle="Data appears within a minute of the first request">
        <Card title="1. Install, 2. initialize" subtitle={<>Replace <span className="mono">&lt;ingest-key&gt;</span> with an ingest key from the API keys tab.</>}>
          <SdkQuickstart dsn={projectDsn(project?.id)} />
        </Card>
      </Section>

      <SlackCard />

      <Section title="Cloud integrations" subtitle="AWS, Google Cloud, Vercel and Neon — managed databases, queues, deploys and drains"
        actions={<Button size="sm" icon={<Plug size={12} />} onClick={() => nav('/settings/integrations')}>Manage</Button>}>
        <Card flush>
          <div className="list">
            {cloud.map((i) => (
              <div key={i.id} className="list__row list__row--clickable" onClick={() => nav(`/settings/integrations?id=${encodeURIComponent(i.id)}`)}>
                <ProviderMark provider={i.provider} size="sm" />
                <div className="list__main"><span className="list__title">{i.name}</span><span className="list__meta">{PROVIDER_LABEL[i.provider]} · {i.resources.length} resources{i.lastSyncAt ? ` · synced ${ago(i.lastSyncAt)}` : ''}</span></div>
                <IntegrationStatusBadge status={i.status} />
                <ChevronRight size={14} className="muted" aria-hidden />
              </div>
            ))}
            {cloud.length === 0 && (
              <div className="list__row list__row--clickable" onClick={() => nav('/settings/integrations')}>
                <Plug size={16} className="muted" aria-hidden />
                <div className="list__main"><span className="list__title">Connect a cloud account</span><span className="list__meta">Read-only; discovered resources join the Databases and Message queues pages</span></div>
                <ChevronRight size={14} className="muted" aria-hidden />
              </div>
            )}
          </div>
        </Card>
      </Section>
    </>
  );
}

// ---------------------------------------------------------------- slack (ADR-017)

function SlackCard() {
  const ws = slackWorkspace();
  const manage = can('manage');
  const channels = ws ? slackChannels('') : [];
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const disconnect = async () => {
    if (!window.confirm(`Disconnect the ${ws?.teamName} Slack workspace? Monitors targeting its channels stop notifying until it is reconnected.`)) return;
    setBusy(true); setError(undefined);
    try { await disconnectSlack(); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  return (
    <Section title="Slack" subtitle="Alerts post to channels you pick per monitor; resolve, reminders and acks reply in the same thread">
      <Card title={<span className="row" style={{ gap: 'var(--space-2)' }}><MessageSquare size={16} aria-hidden />{ws ? ws.teamName : 'Slack workspace'}{ws && <StatusBadge status="ok" label="Connected" />}</span>}
        subtitle={ws ? `${ws.connectedAt ? `Connected ${ago(ws.connectedAt)} · ` : ''}${channels.length} channel${channels.length === 1 ? '' : 's'} visible to the bot` : 'Install the TaskTrooper Slack App — no webhook URLs to copy.'}
        actions={ws
          ? manage && <Button size="sm" variant="ghost" icon={<Unplug size={12} />} disabled={busy} onClick={() => void disconnect()}>Disconnect</Button>
          : <Button size="sm" variant="primary" icon={<MessageSquare size={12} />} disabled={!manage} title={manage ? undefined : denied('manage')} onClick={() => { setError(undefined); connectSlack().catch((e: unknown) => setError(e instanceof Error ? e.message : String(e))); }}>Connect Slack</Button>}>
        {ws ? (
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
          <div className="row" style={{ gap: 'var(--space-1)', flexWrap: 'wrap' }}>
            {channels.slice(0, 12).map((c) => <Badge key={c.id} icon={c.isPrivate ? <Lock /> : <Hash />}>{c.name}</Badge>)}
            {channels.length > 12 && <span className="muted">+{channels.length - 12} more</span>}
          </div>
          {manage && <SlackTest channels={channels} />}
          </div>
        ) : (
          <p className="secondary" style={{ margin: 0 }}>The app asks for <span className="mono">chat:write</span>, <span className="mono">chat:write.public</span>, <span className="mono">channels:read</span> and <span className="mono">groups:read</span>. Messages carry the likely cause with its confidence and Ack / Mute buttons.</p>
        )}
        {error && <div style={{ color: 'var(--status-critical-text)', fontSize: 'var(--text-sm)', marginTop: 'var(--space-2)' }}>{error}</div>}
      </Card>
    </Section>
  );
}

/** Send a test message to one channel: the quickest way to check the whole Slack path before an alert fires. */
function SlackTest({ channels }: { channels: SlackChannel[] }) {
  const [channel, setChannel] = useState('');
  const [state, setState] = useState<{ kind: 'idle' | 'busy' } | { kind: 'ok' | 'error'; text: string }>({ kind: 'idle' });
  const picked = channel || channels.find((c) => c.isMember)?.id || channels[0]?.id || '';
  const name = channels.find((c) => c.id === picked)?.name ?? picked;
  const send = async () => {
    setState({ kind: 'busy' });
    try { await testSlack(picked); setState({ kind: 'ok', text: `Sent to #${name}. Check Slack.` }); }
    catch (e) { setState({ kind: 'error', text: e instanceof Error ? e.message : String(e) }); }
  };
  if (!channels.length) return <span className="muted" style={{ fontSize: 'var(--text-sm)' }}>No channels visible to the bot yet: invite it to a channel, then refresh.</span>;
  return (
    <div className="stack" style={{ gap: 'var(--space-1)' }}>
      <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap', alignItems: 'flex-end' }}>
        <Field label="Test the connection">
          <Select value={picked} onChange={(e) => { setChannel(e.target.value); setState({ kind: 'idle' }); }}
            options={channels.map((c) => ({ value: c.id, label: `${c.isPrivate ? '🔒 ' : '#'}${c.name}` }))} />
        </Field>
        <Button size="sm" icon={<Send size={12} />} disabled={!picked || state.kind === 'busy'} onClick={() => void send()}>
          {state.kind === 'busy' ? 'Sending…' : 'Send test message'}
        </Button>
      </div>
      {state.kind === 'ok' && <span className="row" style={{ gap: 'var(--space-1)', fontSize: 'var(--text-sm)', color: 'var(--status-good-text)' }}><CheckCircle2 size={14} aria-hidden />{state.text}</span>}
      {state.kind === 'error' && <span className="row" style={{ gap: 'var(--space-1)', fontSize: 'var(--text-sm)', color: 'var(--status-critical-text)' }}><ShieldAlert size={14} aria-hidden />{state.text}</span>}
    </div>
  );
}

// ---------------------------------------------------------------- members

const ROLE_OPTIONS: { value: Role; label: string }[] = [
  { value: 'owner', label: 'Owner' }, { value: 'admin', label: 'Admin' }, { value: 'member', label: 'Member' }, { value: 'viewer', label: 'Viewer' },
];
const ROLE_HINT: Record<Role, string> = {
  owner: 'Everything, including billing and deleting the org',
  admin: 'Manages members, api keys and integrations',
  member: 'Creates monitors, acknowledges incidents, runs load tests',
  viewer: 'Read-only',
};

function MembersTab() {
  const { org, me } = useAuth();
  const manage = can('manage');
  const members = [...membersOf(org?.id)];
  const err = readError('/orgs/');
  const [email, setEmail] = useState('');
  const [role, setRole] = useState<Role>('member');
  const [q, setQ] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const run = async (f: () => Promise<void>) => {
    setBusy(true); setError(undefined);
    try { await f(); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  const invite = () => {
    if (!org) return;
    if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email.trim())) return setError('Enter a valid e-mail address.');
    void run(async () => { await inviteMember(org.id, email.trim(), role); setEmail(''); });
  };

  if (!org) {
    return (
      <Card>
        <EmptyState icon={<KeyRound />} title="No organization selected" />
      </Card>
    );
  }

  const rows = members.filter((m) => !q || `${m.name ?? ''} ${m.email}`.toLowerCase().includes(q.toLowerCase()));
  return (
    <>
      {manage ? (
        <Card title="Invite a teammate" subtitle={`They join ${org.name} with the role you pick; the invite is a sign-in link.`}>
          <div className="row" style={{ gap: 'var(--space-2)', alignItems: 'flex-end', flexWrap: 'wrap' }}>
            <div style={{ flex: 1, minWidth: 220 }}>
              <Field label="E-mail"><Input type="email" placeholder="name@company.com" value={email} onChange={(e) => setEmail(e.target.value)} onKeyDown={(e) => e.key === 'Enter' && invite()} /></Field>
            </div>
            <Field label="Role"><Select value={role} onChange={(e) => setRole(e.target.value as Role)} options={ROLE_OPTIONS} /></Field>
            <Button variant="primary" icon={<UserPlus size={14} />} disabled={busy} onClick={invite}>Invite</Button>
          </div>
          <div className="muted" style={{ fontSize: 'var(--text-sm)', marginTop: 'var(--space-2)' }}>{ROLE_HINT[role]}</div>
        </Card>
      ) : (
        <Callout icon={<ShieldAlert size={16} />}>You are a {org.role} of {org.name}. {denied('manage')} to invite or change members.</Callout>
      )}
      {(error || err) && <Callout icon={<ShieldAlert size={16} color="var(--status-critical)" />}>{error ?? `Couldn’t load members: ${err}`}</Callout>}

      <Section title={`${members.length} member${members.length === 1 ? '' : 's'}`} actions={<div style={{ width: 240 }}><SearchInput placeholder="Filter by name or e-mail" value={q} onChange={(e) => setQ(e.target.value)} /></div>}>
        <Card flush>
          <Table<Member> rows={rows} rowKey={(m) => m.userId} defaultSort={{ key: 'role', dir: 'asc' }} empty="No members match." columns={[
            { key: 'who', header: 'Member', sortValue: (m) => m.name ?? m.email, render: (m) => (
              <div className="row" style={{ gap: 'var(--space-3)', minWidth: 0 }}>
                <Avatar name={m.name ?? m.email} size="sm" />
                <div style={{ minWidth: 0 }}>
                  <div className="row" style={{ gap: 'var(--space-2)' }}>{m.name ?? m.email}{m.userId === me?.user.id && <Badge>you</Badge>}{m.pending && <Badge tone="warning">invited</Badge>}</div>
                  {m.name && <div className="muted truncate" style={{ fontSize: 'var(--text-xs)' }}>{m.email}</div>}
                </div>
              </div>
            ) },
            { key: 'role', header: 'Role', width: 160, sortValue: (m) => ROLE_OPTIONS.findIndex((r) => r.value === m.role), render: (m) => manage && m.userId !== me?.user.id
              ? <Select aria-label={`Role of ${m.email}`} value={m.role} disabled={busy} onChange={(e) => void run(() => setMemberRole(org.id, m.userId, e.target.value as Role))} options={ROLE_OPTIONS} />
              : <Badge tone={m.role === 'owner' || m.role === 'admin' ? 'accent' : 'neutral'}>{m.role}</Badge> },
            ...(manage ? [{ key: 'ai', header: 'AI credits this month', width: 240, render: (m: Member) => m.pending ? <span className="muted">—</span> : <MemberAiCell userId={m.userId} email={m.email} orgId={org.id} /> }] : []),
            { key: 'seen', header: 'Last seen', numeric: true, sortValue: (m) => m.lastSeenAt ?? 0, render: (m) => <span className="muted">{m.lastSeenAt ? ago(m.lastSeenAt) : '—'}</span> },
            { key: 'joined', header: 'Joined', numeric: true, sortValue: (m) => m.joinedAt ?? 0, render: (m) => <span className="muted">{m.joinedAt ? ago(m.joinedAt) : '—'}</span> },
            { key: 'rm', header: '', width: 48, render: (m) => manage && m.userId !== me?.user.id
              ? <Button size="sm" variant="ghost" iconOnly icon={<Trash2 size={14} />} aria-label={`Remove ${m.email}`} disabled={busy}
                  onClick={() => { if (window.confirm(`Remove ${m.email} from ${org.name}?`)) void run(() => removeMember(org.id, m.userId)); }} />
              : null },
          ]} />
        </Card>
      </Section>
    </>
  );
}

// ---------------------------------------------------------------- api keys

const SCOPE_LABEL: Record<KeyScope, string> = { ingest: 'Ingest (write-only, for SDKs)', api: 'Management api (read + write)' };

function KeysTab() {
  useData();
  const manage = can('manage');
  const keys = [...apiKeys()];
  const err = readError('/api-keys');
  const drawer = useDrawer<ApiKeyRow>();
  const [creating, setCreating] = useState(false);
  const [name, setName] = useState('');
  const [scope, setScope] = useState<KeyScope>('ingest');
  const [origins, setOrigins] = useState('');
  const [secret, setSecret] = useState<{ name: string; value: string }>();
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const k = drawer.item ? keys.find((x) => x.id === drawer.item!.id) ?? drawer.item : undefined;

  const create = async () => {
    if (!name.trim()) return setError('Give the key a name, e.g. “Backend ingest” or “CI / Terraform”.');
    setBusy(true); setError(undefined);
    try {
      const out = await createApiKey(name.trim(), scope, origins.split(',').map((o) => o.trim()).filter(Boolean));
      setSecret({ name: out.row.name, value: out.secret });
      setCreating(false); setName(''); setOrigins('');
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  const revoke = async (key: ApiKeyRow) => {
    if (!window.confirm(`Revoke “${key.name}”? Anything using it is rejected with 401 right away.`)) return;
    setBusy(true); setError(undefined);
    try { await revokeApiKey(key.id); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };

  return (
    <>
      {secret && (
        <Card title={`Key “${secret.name}” created`} subtitle="Copy it now — it is shown only once. Only a hash is stored.">
          <div className="secret">
            {secret.value ? <CopyCode wrap label="Copy key">{secret.value}</CopyCode> : <span className="muted">The api did not return the secret.</span>}
            <div><Button size="sm" onClick={() => setSecret(undefined)}>I’ve stored it</Button></div>
          </div>
        </Card>
      )}
      {(error || err) && <Callout icon={<ShieldAlert size={16} color="var(--status-critical)" />}>{error ?? `Couldn’t load api keys: ${err}`}</Callout>}
      {!manage && <Callout icon={<ShieldAlert size={16} />}>{denied('manage')} to create or revoke keys.</Callout>}

      {creating && (
        <Card title="New api key">
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <div className="grid grid-2" style={{ gap: 'var(--space-3)' }}>
              <Field label="Name"><Input autoFocus placeholder="Backend ingest" value={name} onChange={(e) => setName(e.target.value)} /></Field>
              <Field label="Scope" hint={scope === 'ingest' ? 'SDKs and drains send telemetry with it; it cannot read anything.' : 'Automation (CI, Terraform, trooper-cli): reads and changes this project.'}>
                <Select value={scope} onChange={(e) => setScope(e.target.value as KeyScope)} options={[{ value: 'ingest', label: SCOPE_LABEL.ingest }, { value: 'api', label: SCOPE_LABEL.api }]} />
              </Field>
            </div>
            {scope === 'ingest' && (
              <Field label="Allowed origins" hint="Browser keys only — comma separated, e.g. shop.com, *.shop.com. Empty = any (server-side key).">
                <Input mono value={origins} onChange={(e) => setOrigins(e.target.value)} />
              </Field>
            )}
            <div className="row" style={{ gap: 'var(--space-2)' }}>
              <Button variant="primary" icon={<KeyRound size={14} />} disabled={busy} onClick={() => void create()}>Create key</Button>
              <Button onClick={() => { setCreating(false); setError(undefined); }}>Cancel</Button>
            </div>
          </div>
        </Card>
      )}

      <Section title="API keys" subtitle="Keys belong to the project; click one for details"
        actions={manage && !creating ? <Button size="sm" variant="primary" icon={<KeyRound size={12} />} onClick={() => { setCreating(true); setSecret(undefined); }}>Create key</Button> : undefined}>
        <Card flush>
          <Table<ApiKeyRow> rowKey={(x) => x.id} rows={keys} onRowClick={drawer.open} defaultSort={{ key: 'u', dir: 'desc' }} empty="No api keys yet." columns={[
            { key: 'n', header: 'Name', sortValue: (x) => x.name, render: (x) => <div><div>{x.name}</div><div className="mono muted" style={{ fontSize: 'var(--text-xs)' }}>{x.prefix}…</div></div> },
            { key: 'k', header: 'Scope', render: (x) => <Badge tone={x.scope === 'api' ? 'warning' : 'neutral'}>{x.scope}</Badge> },
            { key: 'c', header: 'Created', numeric: true, sortValue: (x) => x.createdAt ?? 0, render: (x) => <span className="muted">{x.createdAt ? ago(x.createdAt) : '—'}{x.createdBy ? ` · ${x.createdBy}` : ''}</span> },
            { key: 'u', header: 'Last used', numeric: true, sortValue: (x) => (x.revokedAt ? -1 : x.lastUsedAt ?? 0), render: (x) => x.revokedAt ? <StatusBadge status="muted" label="Revoked" /> : <span className="muted">{x.lastUsedAt ? ago(x.lastUsedAt) : 'never'}</span> },
            { key: 'r', header: '', width: 48, render: (x) => manage && !x.revokedAt
              ? <Button size="sm" variant="ghost" iconOnly icon={<Trash2 size={14} />} aria-label={`Revoke ${x.name}`} disabled={busy} onClick={(e) => { e.stopPropagation(); void revoke(x); }} />
              : null },
          ]} />
        </Card>
      </Section>

      <Drawer open={!!k} onClose={drawer.close} width={560}
        title={k?.name} subtitle={k && <span className="mono">{k.prefix}…</span>}
        actions={k && manage && !k.revokedAt && <Button size="sm" variant="danger" icon={<Trash2 size={14} />} disabled={busy} onClick={() => void revoke(k)}>Revoke</Button>}>
        {k && (
          <>
            {k.revokedAt && <Callout icon={<Trash2 size={16} />}>Revoked {ago(k.revokedAt)}. Requests with it are rejected with 401.</Callout>}
            <DrawerSection title="Access">
              <KeyValues items={[
                ['Scope', SCOPE_LABEL[k.scope]],
                ['Allowed origins', k.origins.length ? <span className="mono">{k.origins.join(', ')}</span> : <span className="muted">Any (server-side key)</span>],
              ]} />
            </DrawerSection>
            <DrawerSection title="History">
              <KeyValues items={[
                ['Created', k.createdAt ? `${dateTime(k.createdAt)}${k.createdBy ? ` by ${k.createdBy}` : ''}` : '—'],
                ['Last used', k.lastUsedAt ? `${ago(k.lastUsedAt)} · ${dateTime(k.lastUsedAt)}` : 'never'],
                ...(k.revokedAt ? [['Revoked', dateTime(k.revokedAt)] as [string, string]] : []),
              ]} />
            </DrawerSection>
          </>
        )}
      </Drawer>
    </>
  );
}

// ---------------------------------------------------------------- audit log

const AUDIT_TONE = (a: string) => (/delete|remove|revoke/.test(a) ? 'critical' : /create|invite|accept/.test(a) ? 'good' : 'neutral') as 'critical' | 'good' | 'neutral';

function AuditTab() {
  const entries = [...auditLog(200)];
  const err = readError('/audit-log');
  const [q, setQ] = useState('');
  const rows = entries.filter((e) => !q || `${e.actor} ${e.action} ${e.target}`.toLowerCase().includes(q.toLowerCase()));
  return (
    <>
      {err && <Callout icon={<ShieldAlert size={16} color="var(--status-critical)" />}>Couldn’t load the audit log: {err}</Callout>}
      <Section title="Audit log" subtitle="Every change made with a session or an api key, newest first"
        actions={<div style={{ width: 260 }}><SearchInput placeholder="Filter by who, action or target" value={q} onChange={(e) => setQ(e.target.value)} /></div>}>
        <Card flush>
          <Table<AuditEntry> rows={rows} rowKey={(e) => e.id} defaultSort={{ key: 'at', dir: 'desc' }} empty="No changes recorded yet." columns={[
            { key: 'at', header: 'When', width: 180, sortValue: (e) => e.at, render: (e) => <span title={dateTime(e.at)} className="muted">{ago(e.at)}</span> },
            { key: 'who', header: 'Who', sortValue: (e) => e.actor, render: (e) => <span className="truncate">{e.actor}</span> },
            { key: 'act', header: 'Action', sortValue: (e) => e.action, render: (e) => <Badge tone={AUDIT_TONE(e.action)}>{e.action}</Badge> },
            { key: 'tgt', header: 'Target', render: (e) => <span className="mono secondary" style={{ fontSize: 'var(--text-sm)' }}>{e.target || '—'}</span> },
          ]} />
        </Card>
      </Section>
    </>
  );
}
