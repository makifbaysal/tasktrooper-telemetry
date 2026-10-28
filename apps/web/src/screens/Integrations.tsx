import { useMemo, useState, type ReactNode } from 'react';
import { useNavigate, useSearchParams } from 'react-router';
import {
  AlertOctagon, ArrowRight, CheckCircle2, ChevronRight, Cloud, Database as DbIcon, ExternalLink, GitCommit, KeyRound, Loader2, Pause, Play, Plug,
  RefreshCw, SquareFunction, Trash2, Upload, Waypoints,
} from 'lucide-react';
import {
  Badge, Button, Callout, Card, ChoiceChips, CopyCode, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, PageHeader, Section, Segmented,
  Select, StatusBadge, Table, Textarea,
} from '@/components/ui';
import {
  DEPLOY_SOURCE_LABEL, PROVIDERS, PROVIDER_CAPABILITIES, PROVIDER_LABEL, VERCEL_DRAINS, clusters, connectIntegration, databases, deleteIntegration,
  deployMarkers, integrationOf, integrations, services, syncIntegration, testIntegration, updateIntegration, useData,
  type DiscoveredResource, type Integration, type IntegrationDraft, type IntegrationProvider, type IntegrationStatus, type IntegrationTestResult,
} from '@/data';
import { ago, dateTime } from '@/lib/format';

/** Public ingest host the Vercel drain posts to (contracts §2). */
const INGEST_URL = 'https://ingest.trooper.dev';
/** TaskTrooper's AWS account that assumes the customer role. */
const TT_AWS_ACCOUNT = '811122233344';

// =================================================================== small pieces
/** Provider identity: neutral tile with initials — no brand colors or logos. */
export function ProviderMark({ provider, size = 'md' }: { provider: IntegrationProvider; size?: 'sm' | 'md' }) {
  const text = { aws: 'AWS', gcp: 'GC', azure: 'AZ', vercel: 'V', neon: 'N' }[provider];
  const box = size === 'sm' ? 'var(--control-h-sm)' : 'var(--control-h-md)';
  return (
    <span aria-hidden style={{
      width: box, height: box, flex: 'none', display: 'inline-flex', alignItems: 'center', justifyContent: 'center', borderRadius: 'var(--radius-md)',
      background: 'var(--bg-sunken)', border: '1px solid var(--border)', color: 'var(--text-secondary)', fontSize: 'var(--text-xs)', fontWeight: 600,
    }}>{text}</span>
  );
}

const STATUS_VIEW: Record<IntegrationStatus, { status: 'ok' | 'critical' | 'pending' | 'muted'; label: string }> = {
  ok: { status: 'ok', label: 'Connected' },
  error: { status: 'critical', label: 'Error' },
  pending: { status: 'pending', label: 'Syncing…' },
  disabled: { status: 'muted', label: 'Disabled' },
};
export function IntegrationStatusBadge({ status }: { status: IntegrationStatus }) {
  const v = STATUS_VIEW[status];
  return <StatusBadge status={v.status} label={v.label} />;
}

/** Where a discovered resource lives in the app, if anywhere. */
function resourceLink(it: Integration, r: DiscoveredResource): string | undefined {
  if (r.refId && databases.some((d) => d.id === r.refId)) return `/infrastructure/databases/${r.refId}`;
  if (r.refId && clusters.some((c) => c.id === r.refId)) return `/infrastructure/queues/${r.refId}`;
  if (r.kind === 'function' && r.refId) return `/infrastructure/serverless/${encodeURIComponent(r.refId)}`;
  if (r.kind === 'project') {
    const map = (it.config.projectMap ?? {}) as Record<string, string>;
    const svc = services.find((s) => s.name === (map[r.name] || r.name) || s.id === (map[r.name] || r.name));
    return svc ? `/services/${svc.id}` : undefined;
  }
  return undefined;
}
const RESOURCE_ICON = { database: DbIcon, cluster: Waypoints, queue: Waypoints, subscription: Waypoints, project: Cloud, function: SquareFunction } as const;

const drainUrl = (id: string) => `${INGEST_URL}/v1/drains/vercel?key=<backend ingest key>&integration=${id}`;

function configItems(it: Integration): [ReactNode, ReactNode][] {
  const c = it.config as Record<string, unknown>;
  const list = (v: unknown) => (Array.isArray(v) && v.length ? <span className="row" style={{ gap: 'var(--space-1)', flexWrap: 'wrap' }}>{v.map((x) => <Badge key={String(x)}>{String(x)}</Badge>)}</span> : <span className="muted">all</span>);
  const mono = (v: unknown) => (v ? <span className="mono" style={{ fontSize: 'var(--text-sm)' }}>{String(v)}</span> : <span className="muted">—</span>);
  switch (it.provider) {
    case 'aws': return [
      ['Access', c.roleArn ? 'IAM role (AssumeRole)' : 'Access key'],
      ['Role ARN', mono(c.roleArn)],
      ['External id', mono(c.externalId)],
      ['Regions', list(c.regions)],
      ['Services', list(c.services)],
    ];
    case 'gcp': return [['Service account', mono(c.clientEmail)], ['Projects', list(c.projectIds)], ['Services', list(c.services)]];
    case 'azure': return [['Tenant id', mono(c.tenantId)], ['Client id', mono(c.clientId)], ['Subscriptions', list(c.subscriptionIds)], ['Resource groups', list(c.resourceGroups)], ['Environment', mono(c.env)]];
    case 'vercel': return [['Team', mono(c.teamId)], ['Drains', list(c.drains)]];
    case 'neon': return [['Organization', mono(c.orgId)], ['Projects', list(c.projectIds)]];
  }
}

// =================================================================== screen
export function Integrations() {
  useData();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const [connect, setConnect] = useState<{ provider: IntegrationProvider; existing?: Integration } | null>(null);
  const openId = params.get('id');
  const open = integrationOf(openId ?? undefined);
  const setOpen = (id: string | null) => setParams((p) => { const n = new URLSearchParams(p); if (id) n.set('id', id); else n.delete('id'); return n; }, { replace: true });
  // Slack / GitHub installs (ADR-017) live in the same api list but have their own cards
  const cloud = integrations.filter((i) => PROVIDERS.includes(i.provider));
  const broken = cloud.filter((i) => i.status === 'error');

  return (
    <>
      <PageHeader title="Cloud integrations"
        crumbs={[{ label: 'Settings', to: '/settings' }, { label: 'Integrations' }]}
        subtitle="Read-only connections to your cloud accounts. Discovered databases, queues and deploys show up next to what the SDKs report." />

      {broken.length > 0 && (
        <Section title="Needs attention">
          <div className="grid grid-2">
            {broken.map((it) => (
              <Card key={it.id} className="card--rail-critical" onClick={() => setOpen(it.id)}
                title={<span className="row" style={{ gap: 'var(--space-2)' }}><ProviderMark provider={it.provider} size="sm" />{it.name}<IntegrationStatusBadge status={it.status} /></span>}
                subtitle={`${PROVIDER_LABEL[it.provider]} · last synced ${it.lastSyncAt ? ago(it.lastSyncAt) : 'never'}`}
                actions={<Button size="sm" icon={<KeyRound size={14} />} onClick={(e) => { e.stopPropagation(); setConnect({ provider: it.provider, existing: it }); }}>Reconnect</Button>}>
                <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>{it.error}. Data from its {it.resources.length} resource{it.resources.length === 1 ? '' : 's'} is stale until then.</p>
              </Card>
            ))}
          </div>
        </Section>
      )}

      <Section title="Connected" subtitle={`${cloud.length} integration${cloud.length === 1 ? '' : 's'} · click for resources and actions`}>
        {cloud.length === 0
          ? <Card><EmptyState icon={<Plug />} title="No cloud integrations yet">Connect a provider below to pull managed database, queue and deploy data.</EmptyState></Card>
          : <Card flush>
              <Table rows={cloud} rowKey={(i) => i.id} onRowClick={(i) => setOpen(i.id)} defaultSort={{ key: 'status', dir: 'desc' }} columns={[
                { key: 'name', header: 'Integration', sortValue: (i) => i.name, render: (i) => (
                  <div className="row" style={{ gap: 'var(--space-3)', whiteSpace: 'nowrap' }}>
                    <ProviderMark provider={i.provider} size="sm" />
                    <div><div style={{ fontWeight: 500 }}>{i.name}</div><div className="muted" style={{ fontSize: 'var(--text-xs)' }}>{PROVIDER_LABEL[i.provider]}</div></div>
                  </div>) },
                { key: 'status', header: 'Status', sortValue: (i) => ['disabled', 'ok', 'pending', 'error'].indexOf(i.status), render: (i) => <IntegrationStatusBadge status={i.status} /> },
                { key: 'res', header: 'Resources', numeric: true, sortValue: (i) => i.resources.length, render: (i) => i.resources.length },
                { key: 'what', header: 'Discovered', render: (i) => <span className="secondary truncate" style={{ display: 'block', maxWidth: 260, fontSize: 'var(--text-sm)' }}>{i.resources.map((r) => r.name).join(', ') || '—'}</span> },
                { key: 'sync', header: 'Last sync', numeric: true, sortValue: (i) => i.lastSyncAt ?? 0, render: (i) => <span className="muted">{i.lastSyncAt ? ago(i.lastSyncAt) : '—'}</span> },
                { key: 'go', header: '', width: 32, render: () => <ChevronRight size={14} className="muted" aria-hidden /> },
              ]} />
            </Card>}
      </Section>

      <Section title="Add an integration">
        <div className="grid grid-4">
          {PROVIDERS.map((p) => {
            const n = integrations.filter((i) => i.provider === p).length;
            return (
              <Card key={p} title={<span className="row" style={{ gap: 'var(--space-2)' }}><ProviderMark provider={p} size="sm" />{PROVIDER_LABEL[p]}</span>}
                subtitle={n ? `${n} connected` : 'Not connected'}>
                <ul className="secondary" style={{ margin: '0 0 var(--space-3)', paddingLeft: 'var(--space-4)', fontSize: 'var(--text-sm)', display: 'flex', flexDirection: 'column', gap: 'var(--space-1)' }}>
                  {PROVIDER_CAPABILITIES[p].map((c) => <li key={c}>{c}</li>)}
                </ul>
                <Button size="sm" variant={n ? 'secondary' : 'primary'} icon={<Plug size={14} />} onClick={() => setConnect({ provider: p })}>{n ? 'Connect another' : 'Connect'}</Button>
              </Card>
            );
          })}
        </div>
      </Section>

      {open && <IntegrationDrawer it={open} onClose={() => setOpen(null)} onReconnect={() => setConnect({ provider: open.provider, existing: open })} onNav={nav} />}
      {connect && <ConnectIntegration key={connect.existing?.id ?? connect.provider} provider={connect.provider} existing={connect.existing}
        onClose={() => setConnect(null)} onDone={(id) => { setConnect(null); setOpen(id); }} />}
    </>
  );
}

// =================================================================== detail drawer
function IntegrationDrawer({ it, onClose, onReconnect, onNav }: { it: Integration; onClose: () => void; onReconnect: () => void; onNav: (to: string) => void }) {
  const [confirmDelete, setConfirmDelete] = useState(false);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);
  const run = async (f: () => Promise<void>) => {
    setBusy(true); setErr(null);
    try { await f(); } catch (e) { setErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  };
  const disabled = it.status === 'disabled';
  const deploys = it.provider === 'vercel' ? deployMarkers.filter((d) => d.source === 'vercel').sort((a, b) => b.ts - a.ts).slice(0, 5) : [];
  const projectMap = (it.config.projectMap ?? {}) as Record<string, string>;

  return (
    <Drawer open onClose={onClose} width={640}
      title={<span className="row" style={{ gap: 'var(--space-2)' }}><ProviderMark provider={it.provider} size="sm" />{it.name}</span>}
      subtitle={<span className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}><IntegrationStatusBadge status={it.status} /><span className="muted">{PROVIDER_LABEL[it.provider]} · last sync {it.lastSyncAt ? ago(it.lastSyncAt) : 'never'}</span></span>}
      actions={<Button size="sm" icon={busy || it.status === 'pending' ? <Loader2 size={14} className="spin" /> : <RefreshCw size={14} />} disabled={busy || disabled || it.status === 'pending'} onClick={() => run(() => syncIntegration(it.id))}>Sync now</Button>}>
      {it.status === 'error' && (
        <Card className="card--rail-critical" title={<span className="row" style={{ gap: 'var(--space-2)' }}><AlertOctagon size={14} style={{ color: 'var(--status-critical-text)' }} aria-hidden />Sync failing</span>}
          actions={<Button size="sm" variant="primary" icon={<KeyRound size={14} />} onClick={onReconnect}>Reconnect</Button>}>
          <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>{it.error}</p>
        </Card>
      )}
      {disabled && <Callout icon={<Pause size={16} />}>Disabled — nothing is synced and its resources keep their last values. Enable to resume.</Callout>}
      {err && <Callout icon={<AlertOctagon size={16} />}>{err}</Callout>}

      <DrawerSection title={`Discovered resources`} aside={`${it.resources.length}`}>
        {it.resources.length === 0
          ? <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>{it.status === 'pending' ? 'Discovery is running — resources appear in a minute.' : 'Nothing found with the current permissions and filters.'}</p>
          : <div className="card" style={{ overflow: 'hidden' }}>
              <div className="list">
                {it.resources.map((r) => {
                  const to = resourceLink(it, r);
                  const Icon = RESOURCE_ICON[r.kind];
                  const mapped = r.kind === 'project' ? (projectMap[r.name] || r.name) : undefined;
                  return (
                    <div key={`${r.kind}-${r.name}`} className={to ? 'list__row list__row--clickable' : 'list__row'} onClick={to ? () => onNav(to) : undefined}>
                      <Icon size={16} className="muted" aria-hidden />
                      <div className="list__main">
                        <span className="list__title">{r.name}</span>
                        <span className="list__meta">{r.detail}{mapped && <> · service <strong>{mapped}</strong></>}</span>
                      </div>
                      <Badge>{r.kind}</Badge>
                      {to ? <ArrowRight size={14} className="muted" aria-hidden /> : <span style={{ width: 14 }} />}
                    </div>
                  );
                })}
              </div>
            </div>}
      </DrawerSection>

      <DrawerSection title="Configuration">
        <KeyValues items={[...configItems(it), ['Connected', dateTime(it.createdAt)], ['Credentials', <span key="c" className="muted">stored encrypted · never shown again</span>]]} />
      </DrawerSection>

      {it.provider === 'vercel' && (
        <>
          <DrawerSection title="Project → service">
            <KeyValues items={Object.entries(projectMap).map(([p, s]) => [<span key={p} className="mono">{p}</span>, s || p])} />
          </DrawerSection>
          <DrawerSection title="Drain endpoint">
            <CopyCode wrap>{drainUrl(it.id)}</CopyCode>
            <p className="muted" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>Vercel → Team settings → Drains → Add drain, one per type ({VERCEL_DRAINS.map((d) => d.label).join(', ')}). Use the drain secret you set when connecting.</p>
          </DrawerSection>
          <DrawerSection title="Recent deploys">
            <DeployList deploys={deploys} onService={(s) => { const svc = services.find((x) => x.name === s || x.id === s); if (svc) onNav(`/services/${svc.id}`); }} />
          </DrawerSection>
        </>
      )}

      <DrawerSection title="Manage">
        <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
          <Button size="sm" icon={<KeyRound size={14} />} onClick={onReconnect}>Reconnect / edit</Button>
          {disabled
            ? <Button size="sm" icon={<Play size={14} />} disabled={busy} onClick={() => run(() => updateIntegration(it.id, { status: 'ok' }))}>Enable</Button>
            : <Button size="sm" icon={<Pause size={14} />} disabled={busy} onClick={() => run(() => updateIntegration(it.id, { status: 'disabled' }))}>Disable</Button>}
          {confirmDelete
            ? <>
                <Button size="sm" variant="danger" icon={<Trash2 size={14} />} disabled={busy} onClick={() => run(async () => { await deleteIntegration(it.id); onClose(); })}>Delete {it.name}</Button>
                <Button size="sm" variant="ghost" onClick={() => setConfirmDelete(false)}>Cancel</Button>
              </>
            : <Button size="sm" variant="danger" icon={<Trash2 size={14} />} onClick={() => setConfirmDelete(true)}>Delete</Button>}
        </div>
        {confirmDelete && <p className="secondary" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>Syncing stops and the stored credentials are deleted. Discovered databases and queues stay with their history, marked as no longer synced.</p>}
      </DrawerSection>
    </Drawer>
  );
}

/** Deploy markers list: version, commit message, author, source, link. Used on service and app pages too. */
export function DeployList({ deploys, onService, onOpen }: {
  deploys: typeof deployMarkers; onService?: (service: string) => void; onOpen?: (d: (typeof deployMarkers)[number]) => void;
}) {
  if (deploys.length === 0) return <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>No deploys in the last 7 days.</p>;
  return (
    <div className="card" style={{ overflow: 'hidden' }}>
      <div className="list">
        {deploys.map((d, i) => (
          <div key={`${d.service}-${d.version}-${d.ts}`} className={onOpen ? 'list__row list__row--clickable' : 'list__row'} onClick={onOpen ? () => onOpen(d) : undefined}>
            <GitCommit size={16} className="muted" aria-hidden />
            <div className="list__main">
              <span className="list__title">
                <span className="mono">{d.version}</span>
                {i === 0 && d.state !== 'error' && <> <Badge tone="accent">Current</Badge></>}
                {d.state === 'error' && <> <StatusBadge status="critical" label="Build failed" /></>}
                {d.state === 'building' && <> <StatusBadge status="pending" label="Building" /></>}
                {d.state === 'canceled' && <> <StatusBadge status="muted" label="Canceled" /></>}
              </span>
              <span className="list__meta">
                {d.commitMessage ?? 'no commit message'}{d.author && <> · {d.author}</>} · {DEPLOY_SOURCE_LABEL[d.source]}
                {onService && <> · <a href="#" onClick={(e) => { e.preventDefault(); e.stopPropagation(); onService(d.service); }}>{d.service}</a></>}
              </span>
            </div>
            <span className="muted" style={{ whiteSpace: 'nowrap', fontSize: 'var(--text-sm)' }}>{ago(d.ts)}</span>
            {d.url && <a href={d.url} target="_blank" rel="noreferrer" className="btn btn--ghost btn--sm btn--icon" aria-label={`Open deployment ${d.version}`} onClick={(e) => e.stopPropagation()}><ExternalLink size={14} /></a>}
          </div>
        ))}
      </div>
    </div>
  );
}

// =================================================================== connect drawer
const AWS_REGIONS = ['eu-central-1', 'eu-west-1', 'eu-north-1', 'us-east-1', 'us-west-2', 'ap-southeast-1'].map((r) => ({ id: r, label: r }));
type AwsService = 'rds' | 'elasticache' | 'msk' | 'sqs' | 'lambda';
const AWS_SERVICES: { id: AwsService; label: string; actions: string[] }[] = [
  { id: 'rds', label: 'RDS / Aurora', actions: ['rds:DescribeDBInstances', 'rds:DescribeDBClusters'] },
  { id: 'elasticache', label: 'ElastiCache', actions: ['elasticache:DescribeReplicationGroups', 'elasticache:DescribeCacheClusters'] },
  { id: 'msk', label: 'MSK', actions: ['kafka:ListClustersV2', 'kafka:DescribeClusterV2', 'kafka:ListNodes'] },
  { id: 'sqs', label: 'SQS', actions: ['sqs:ListQueues', 'sqs:GetQueueAttributes'] },
  { id: 'lambda', label: 'Lambda', actions: ['lambda:ListFunctions', 'lambda:ListTags', 'lambda:ListEventSourceMappings', 'lambda:GetFunctionConcurrency'] },
];
type GcpService = 'cloudsql' | 'memorystore' | 'pubsub';
const GCP_SERVICES: { id: GcpService; label: string; role: string }[] = [
  { id: 'cloudsql', label: 'Cloud SQL', role: 'roles/cloudsql.viewer' },
  { id: 'memorystore', label: 'Memorystore', role: 'roles/redis.viewer' },
  { id: 'pubsub', label: 'Pub/Sub', role: 'roles/pubsub.viewer' },
];

function awsPolicy(svcs: AwsService[]): string {
  const actions = ['cloudwatch:GetMetricData', 'cloudwatch:ListMetrics', 'tag:GetResources',
    ...AWS_SERVICES.filter((s) => svcs.includes(s.id)).flatMap((s) => s.id === 'rds' ? ['rds:Describe*'] : s.id === 'elasticache' ? ['elasticache:Describe*'] : s.id === 'msk' ? ['kafka:List*', 'kafka:Describe*'] : s.actions)];
  return JSON.stringify({ Version: '2012-10-17', Statement: [{ Sid: 'TaskTrooperReadOnly', Effect: 'Allow', Action: actions, Resource: '*' }] }, null, 2);
}
function awsCloudFormation(externalId: string, svcs: AwsService[]): string {
  const actions = (JSON.parse(awsPolicy(svcs)) as { Statement: { Action: string[] }[] }).Statement[0].Action;
  return `# aws cloudformation deploy --template-file tasktrooper.yaml \\
#   --stack-name tasktrooper-readonly --capabilities CAPABILITY_NAMED_IAM
Resources:
  TaskTrooperReadOnly:
    Type: AWS::IAM::Role
    Properties:
      RoleName: TaskTrooperReadOnly
      AssumeRolePolicyDocument:
        Version: '2012-10-17'
        Statement:
          - Effect: Allow
            Principal: { AWS: 'arn:aws:iam::${TT_AWS_ACCOUNT}:root' }
            Action: sts:AssumeRole
            Condition: { StringEquals: { 'sts:ExternalId': '${externalId}' } }
      Policies:
        - PolicyName: TaskTrooperReadOnly
          PolicyDocument:
            Version: '2012-10-17'
            Statement:
              - Effect: Allow
                Resource: '*'
                Action:
${actions.map((a) => `                  - ${a}`).join('\n')}
Outputs:
  RoleArn: { Value: !GetAtt TaskTrooperReadOnly.Arn }`;
}
function awsTerraform(externalId: string, svcs: AwsService[]): string {
  const actions = (JSON.parse(awsPolicy(svcs)) as { Statement: { Action: string[] }[] }).Statement[0].Action;
  return `resource "aws_iam_role" "tasktrooper" {
  name = "TaskTrooperReadOnly"
  assume_role_policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect    = "Allow"
      Principal = { AWS = "arn:aws:iam::${TT_AWS_ACCOUNT}:root" }
      Action    = "sts:AssumeRole"
      Condition = { StringEquals = { "sts:ExternalId" = "${externalId}" } }
    }]
  })
}

resource "aws_iam_role_policy" "tasktrooper" {
  role = aws_iam_role.tasktrooper.id
  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect   = "Allow"
      Resource = "*"
      Action   = [
${actions.map((a) => `        "${a}",`).join('\n')}
      ]
    }]
  })
}

output "role_arn" { value = aws_iam_role.tasktrooper.arn }`;
}
function gcpCommands(project: string, svcs: GcpService[]): string {
  const p = project || 'my-project';
  const roles = ['roles/monitoring.viewer', ...GCP_SERVICES.filter((s) => svcs.includes(s.id)).map((s) => s.role)];
  return `gcloud iam service-accounts create tasktrooper --project ${p} \\
  --display-name "TaskTrooper (read-only)"
for role in ${roles.join(' ')}; do
  gcloud projects add-iam-policy-binding ${p} \\
    --member serviceAccount:tasktrooper@${p}.iam.gserviceaccount.com --role $role
done
gcloud iam service-accounts keys create tasktrooper-key.json \\
  --iam-account tasktrooper@${p}.iam.gserviceaccount.com`;
}

const randomId = (prefix: string, n = 10) => `${prefix}${Array.from({ length: n }, () => '0123456789abcdefghijklmnopqrstuvwxyz'[Math.floor(Math.random() * 36)]).join('')}`;
const splitList = (s: string) => s.split(/[\s,]+/).map((x) => x.trim()).filter(Boolean);

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;
function azureCommands(subscription: string): string {
  return `az ad sp create-for-rbac --name tasktrooper-readonly \\
  --role Reader --scopes /subscriptions/${subscription}
az role assignment create --assignee <appId> \\
  --role "Monitoring Reader" --scope /subscriptions/${subscription}`;
}
interface MapRow { project: string; service: string }

function ConnectIntegration({ provider, existing, onClose, onDone }: { provider: IntegrationProvider; existing?: Integration; onClose: () => void; onDone: (id: string) => void }) {
  const cfg = (existing?.config ?? {}) as Record<string, unknown>;
  const arr = (k: string, d: string[] = []) => (Array.isArray(cfg[k]) ? (cfg[k] as string[]) : d);
  const [name, setName] = useState(existing?.name ?? { aws: 'aws-prod', gcp: 'gcp-prod', azure: 'azure-prod', vercel: 'vercel', neon: 'neon' }[provider]);
  const [secret, setSecret] = useState('');
  const [test, setTest] = useState<'idle' | 'running' | IntegrationTestResult>('idle');
  const [saving, setSaving] = useState(false);
  const [saveErr, setSaveErr] = useState<string | null>(null);

  // aws
  const [awsAuth, setAwsAuth] = useState<'role' | 'keys'>(existing && !cfg.roleArn ? 'keys' : 'role');
  const externalId = useMemo(() => (cfg.externalId as string | undefined) ?? randomId('tt-shopco-', 6), [cfg.externalId]);
  const [roleArn, setRoleArn] = useState((cfg.roleArn as string | undefined) ?? '');
  const [accessKeyId, setAccessKeyId] = useState((cfg.accessKeyId as string | undefined) ?? '');
  const [regions, setRegions] = useState<string[]>(arr('regions', ['eu-central-1']));
  const [awsSvcs, setAwsSvcs] = useState<AwsService[]>(arr('services', ['rds', 'elasticache', 'msk', 'sqs', 'lambda']) as AwsService[]);
  const [snippet, setSnippet] = useState<'cfn' | 'tf' | 'policy'>('cfn');
  // gcp
  const [projectIds, setProjectIds] = useState(arr('projectIds').join(', '));
  const [gcpSvcs, setGcpSvcs] = useState<GcpService[]>(arr('services', ['cloudsql', 'memorystore', 'pubsub']) as GcpService[]);
  const [keyInfo, setKeyInfo] = useState<{ email?: string; project?: string; error?: string } | null>(cfg.clientEmail ? { email: String(cfg.clientEmail) } : null);
  // vercel
  const [teamId, setTeamId] = useState((cfg.teamId as string | undefined) ?? '');
  const [mapRows, setMapRows] = useState<MapRow[]>(Object.entries((cfg.projectMap ?? {}) as Record<string, string>).map(([project, service]) => ({ project, service })));
  const [drains, setDrains] = useState<string[]>(arr('drains', ['logs', 'speed_insights', 'web_analytics']));
  const [drainSecret, setDrainSecret] = useState('');
  // azure
  const [tenantId, setTenantId] = useState((cfg.tenantId as string | undefined) ?? '');
  const [clientId, setClientId] = useState((cfg.clientId as string | undefined) ?? '');
  const [subscriptionIds, setSubscriptionIds] = useState(arr('subscriptionIds').join(', '));
  const [resourceGroups, setResourceGroups] = useState(arr('resourceGroups').join(', '));
  const [azEnv, setAzEnv] = useState((cfg.env as string | undefined) ?? '');
  // neon
  const [orgId, setOrgId] = useState((cfg.orgId as string | undefined) ?? '');
  const [neonProjects, setNeonProjects] = useState<string[]>(arr('projectIds'));
  const [neonFound, setNeonFound] = useState<string[]>(arr('projectIds'));

  const config = (): Record<string, unknown> => {
    switch (provider) {
      case 'aws': return awsAuth === 'role' ? { roleArn: roleArn.trim(), externalId, regions, services: awsSvcs } : { accessKeyId: accessKeyId.trim(), regions, services: awsSvcs };
      case 'gcp': return { projectIds: splitList(projectIds), services: gcpSvcs, ...(keyInfo?.email ? { clientEmail: keyInfo.email } : {}) };
      case 'vercel': return { teamId: teamId.trim(), projectMap: Object.fromEntries(mapRows.filter((r) => r.project).map((r) => [r.project, r.service])), drains, ...(drainSecret ? { drainSecret: '(set)' } : {}) };
      case 'azure': return { tenantId: tenantId.trim(), clientId: clientId.trim(), subscriptionIds: splitList(subscriptionIds), ...(splitList(resourceGroups).length ? { resourceGroups: splitList(resourceGroups) } : {}), ...(azEnv.trim() ? { env: azEnv.trim() } : {}) };
      case 'neon': return { orgId: orgId.trim(), projectIds: neonProjects };
    }
  };
  // vercel drain secret travels with the token; the api stores it as its own secret (drainSecretRef)
  const secretValue = provider === 'aws' && awsAuth === 'role' ? undefined
    : provider === 'vercel' && drainSecret ? JSON.stringify({ token: secret, drainSecret }) : secret || undefined;
  const draft = (): IntegrationDraft => ({ provider, name: name.trim(), config: config(), secret: secretValue, ...(provider === 'azure' && secretValue ? { credentials: { clientSecret: secretValue } } : {}) });

  const credsReady = provider === 'aws'
    ? (awsAuth === 'role' ? /^arn:aws:iam::\d{12}:role\/.+/.test(roleArn.trim()) : !!accessKeyId.trim() && !!secret)
    : !!secret || !!existing;
  const scopeReady = provider === 'aws' ? regions.length > 0 && awsSvcs.length > 0 : provider === 'gcp' ? splitList(projectIds).length > 0 && gcpSvcs.length > 0
    : provider === 'azure' ? UUID.test(tenantId.trim()) && UUID.test(clientId.trim()) : true;
  const ready = !!name.trim() && credsReady && scopeReady;

  const runTest = async () => {
    setTest('running');
    try {
      const r = await testIntegration(draft());
      setTest(r);
      if (r.ok && provider === 'vercel' && mapRows.length === 0) {
        setMapRows((r.resources ?? []).filter((x) => x.kind === 'project').map((x) => ({ project: x.name, service: services.find((s) => s.name === x.name)?.name ?? '' })));
      }
      if (r.ok && provider === 'neon') {
        const found = (r.resources ?? []).map((x) => x.name.split('/')[0]);
        setNeonFound([...new Set([...neonFound, ...found])]);
      }
    } catch (e) {
      setTest({ ok: false, message: e instanceof Error ? e.message : String(e) });
    }
  };
  const save = async () => {
    setSaving(true); setSaveErr(null);
    try {
      if (existing) { await updateIntegration(existing.id, { name: name.trim(), config: config(), ...(secretValue ? { secret: secretValue } : {}) }); onDone(existing.id); }
      else onDone((await connectIntegration(draft())).id);
    } catch (e) { setSaveErr(e instanceof Error ? e.message : String(e)); setSaving(false); }
  };

  const readKeyFile = (f: File | undefined) => { if (f) void f.text().then(onKeyText); };
  function onKeyText(text: string) {
    setSecret(text);
    if (!text.trim()) { setKeyInfo(null); return; }
    try {
      const j = JSON.parse(text) as { client_email?: string; project_id?: string; type?: string };
      if (j.type !== 'service_account' || !j.client_email) { setKeyInfo({ error: 'Not a service account key — expected "type": "service_account"' }); return; }
      setKeyInfo({ email: j.client_email, project: j.project_id });
      if (j.project_id && !projectIds.trim()) setProjectIds(j.project_id);
    } catch { setKeyInfo({ error: 'Not valid JSON' }); }
  }

  const title = existing ? `Reconnect ${existing.name}` : `Connect ${PROVIDER_LABEL[provider]}`;
  const subtitle = {
    aws: 'Read-only: CloudWatch metrics and Describe calls. TaskTrooper never reads your data.',
    gcp: 'Read-only: Cloud Monitoring time series and resource metadata.',
    azure: 'Read-only: Function Apps from Resource Manager and their Azure Monitor metrics.',
    vercel: 'Deploys become markers on your charts; drains stream logs, traces and Web Vitals.',
    neon: 'Projects, branches and computes become databases; compute state and storage every minute.',
  }[provider];

  return (
    <Drawer open onClose={onClose} width={680} title={<span className="row" style={{ gap: 'var(--space-2)' }}><ProviderMark provider={provider} size="sm" />{title}</span>} subtitle={subtitle}
      actions={<Button variant="primary" disabled={!ready || saving} icon={saving ? <Loader2 size={14} className="spin" /> : undefined} onClick={save}>{existing ? 'Save & reconnect' : 'Connect'}</Button>}>
      <div className="stack">
        <Field label="Name" hint="Shown on every resource it discovers, e.g. “AWS · aws-prod”.">
          <Input value={name} onChange={(e) => setName(e.target.value)} />
        </Field>

        {provider === 'aws' && <>
          <Field label="Access">
            <Segmented label="AWS access" value={awsAuth} onChange={(v) => { setAwsAuth(v); setTest('idle'); }}
              options={[{ id: 'role', label: 'IAM role · recommended' }, { id: 'keys', label: 'Access key' }]} />
          </Field>
          <Field label="Regions"><ChoiceChips label="Regions" options={AWS_REGIONS} value={regions} onChange={setRegions} /></Field>
          <Field label="Services" hint="Only these are discovered; the policy below lists exactly what they need.">
            <ChoiceChips label="AWS services" options={AWS_SERVICES.map((s) => ({ id: s.id, label: s.label }))} value={awsSvcs} onChange={setAwsSvcs} />
          </Field>
          {awsAuth === 'role' ? <>
            <Step n={1} title="Create a read-only role in your account">
              <p className="secondary" style={{ margin: '0 0 var(--space-2)', fontSize: 'var(--text-sm)' }}>
                TaskTrooper's account <span className="mono">{TT_AWS_ACCOUNT}</span> assumes it with the external id <span className="mono">{externalId}</span> — nobody else can.
              </p>
              <Segmented label="Snippet" value={snippet} onChange={setSnippet} options={[{ id: 'cfn', label: 'CloudFormation' }, { id: 'tf', label: 'Terraform' }, { id: 'policy', label: 'IAM policy JSON' }]} />
              <div style={{ marginTop: 'var(--space-2)', maxHeight: 'calc(var(--space-6) * 14)', overflow: 'auto' }}>
                <CopyCode>{snippet === 'cfn' ? awsCloudFormation(externalId, awsSvcs) : snippet === 'tf' ? awsTerraform(externalId, awsSvcs) : awsPolicy(awsSvcs)}</CopyCode>
              </div>
            </Step>
            <Step n={2} title="Paste the role ARN">
              <Input mono value={roleArn} onChange={(e) => { setRoleArn(e.target.value); setTest('idle'); }} placeholder="arn:aws:iam::123456789012:role/TaskTrooperReadOnly" aria-label="Role ARN" />
            </Step>
          </> : <>
            <Callout icon={<KeyRound size={16} />}>Long-lived keys work, but a role is safer: nothing to rotate or leak. Attach the IAM policy below to a dedicated user.</Callout>
            <div className="grid grid-2">
              <Field label="Access key id"><Input mono value={accessKeyId} onChange={(e) => setAccessKeyId(e.target.value)} placeholder="AKIA…" /></Field>
              <Field label="Secret access key" hint={existing ? 'Leave empty to keep the current one.' : 'Stored encrypted; never shown again.'}>
                <Input mono type="password" autoComplete="new-password" value={secret} onChange={(e) => setSecret(e.target.value)} />
              </Field>
            </div>
            <Field label="IAM policy"><CopyCode>{awsPolicy(awsSvcs)}</CopyCode></Field>
          </>}
        </>}

        {provider === 'gcp' && <>
          <Field label="Services"><ChoiceChips label="Google Cloud services" options={GCP_SERVICES.map((s) => ({ id: s.id, label: s.label }))} value={gcpSvcs} onChange={setGcpSvcs} /></Field>
          <Step n={1} title="Create a service account with viewer roles">
            <CopyCode>{gcpCommands(splitList(projectIds)[0] ?? '', gcpSvcs)}</CopyCode>
            <p className="muted" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>Run it in every project you list below (or grant the roles at folder / organization level). Workload identity federation works too — ask us.</p>
          </Step>
          <Step n={2} title="Upload or paste the JSON key">
            <div className="stack" style={{ gap: 'var(--space-2)' }}>
              <label className="btn btn--secondary btn--sm" style={{ width: 'fit-content', cursor: 'pointer' }}>
                <Upload size={14} aria-hidden />Upload key file
                <input type="file" accept="application/json,.json" className="sr-only" onChange={(e) => readKeyFile(e.target.files?.[0])} />
              </label>
              <Textarea mono rows={5} aria-label="Service account JSON key" value={secret} onChange={(e) => onKeyText(e.target.value)}
                placeholder={existing ? 'Leave empty to keep the current key' : '{ "type": "service_account", "project_id": "…", "private_key": "…", "client_email": "…" }'} />
              {keyInfo && (keyInfo.error
                ? <span className="row" style={{ gap: 6, color: 'var(--status-critical-text)', fontSize: 'var(--text-sm)' }}><AlertOctagon size={14} aria-hidden />{keyInfo.error}</span>
                : <span className="row" style={{ gap: 6, color: 'var(--status-good-text)', fontSize: 'var(--text-sm)' }}><CheckCircle2 size={14} aria-hidden /><span className="mono">{keyInfo.email}</span></span>)}
            </div>
          </Step>
          <Field label="Project ids" hint="Comma-separated. Metrics and resources of these projects are read.">
            <Input mono value={projectIds} onChange={(e) => setProjectIds(e.target.value)} placeholder="shop-prod, shop-analytics-prod" />
          </Field>
        </>}

        {provider === 'vercel' && <>
          <div className="grid grid-2">
            <Field label="Access token" hint={existing ? 'Leave empty to keep the current one.' : 'Vercel → Account settings → Tokens, scoped to the team, read access.'}>
              <Input mono type="password" autoComplete="new-password" value={secret} onChange={(e) => { setSecret(e.target.value); setTest('idle'); }} />
            </Field>
            <Field label="Team id or slug" hint="Empty for a personal account."><Input mono value={teamId} onChange={(e) => setTeamId(e.target.value)} placeholder="team_shop" /></Field>
          </div>
          <Field label="Project → service" hint="Deploys, logs and vitals of a project land on this service. Same name by default.">
            {mapRows.length === 0
              ? <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>Test the connection to load your projects.</p>
              : <div className="card" style={{ overflow: 'hidden' }}>
                  <div className="list">
                    {mapRows.map((r) => (
                      <div key={r.project} className="list__row" style={{ flexWrap: 'wrap' }}>
                        <span className="mono" style={{ flex: '1 1 8rem', minWidth: 0, overflowWrap: 'anywhere' }}>{r.project}</span>
                        <ArrowRight size={14} className="muted" aria-hidden />
                        <Select aria-label={`Service for ${r.project}`} value={r.service} style={{ flex: '1 1 10rem', minWidth: 0 }}
                          onChange={(e) => setMapRows((rows) => rows.map((x) => (x.project === r.project ? { ...x, service: e.target.value } : x)))}
                          options={[{ value: '', label: `New service “${r.project}”` }, ...services.map((s) => ({ value: s.name, label: s.name }))]} />
                      </div>
                    ))}
                  </div>
                </div>}
          </Field>
          <Step n={1} title="Drains">
            <ChoiceChips label="Drain types" options={VERCEL_DRAINS.map((d) => ({ id: d.id, label: d.label, hint: d.detail }))} value={drains} onChange={setDrains} />
            <ul className="muted" style={{ margin: 'var(--space-2) 0 0', paddingLeft: 'var(--space-4)', fontSize: 'var(--text-sm)' }}>
              {VERCEL_DRAINS.filter((d) => drains.includes(d.id)).map((d) => <li key={d.id}><strong className="secondary">{d.label}</strong> — {d.detail}</li>)}
            </ul>
          </Step>
          <Step n={2} title="Drain secret">
            <div className="row" style={{ gap: 'var(--space-2)' }}>
              <Input mono type="password" autoComplete="new-password" aria-label="Drain secret" value={drainSecret} onChange={(e) => setDrainSecret(e.target.value)} placeholder="secret Vercel signs each request with" style={{ flex: 1, minWidth: 0 }} />
              <Button size="sm" onClick={() => setDrainSecret(randomId('', 32))}>Generate</Button>
            </div>
            <p className="muted" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>Vercel signs every drain request with it (<span className="mono">x-vercel-signature</span>, HMAC-SHA1); unsigned requests are dropped.</p>
          </Step>
          <Step n={3} title="Add the drains in Vercel">
            <CopyCode wrap>{drainUrl(existing?.id ?? '<integration id>')}</CopyCode>
            <p className="muted" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>
              Team settings → Drains → Add drain: one per type, format JSON, this URL{existing ? '' : ' (the integration id is filled in after Connect — it is shown on the integration)'}, and the secret above.
            </p>
          </Step>
        </>}

        {provider === 'azure' && <>
          <Step n={1} title="Create a service principal with read-only roles">
            <CopyCode>{azureCommands(splitList(subscriptionIds)[0] ?? '<subscription id>')}</CopyCode>
            <p className="muted" style={{ margin: 'var(--space-2) 0 0', fontSize: 'var(--text-sm)' }}>Reader + Monitoring Reader on each subscription (or only on the resource groups below). The output has the tenant id, client id (appId) and secret (password).</p>
          </Step>
          <Step n={2} title="Paste the credentials">
            <div className="stack" style={{ gap: 'var(--space-3)' }}>
              <div className="grid grid-2">
                <Field label="Tenant id"><Input mono value={tenantId} onChange={(e) => { setTenantId(e.target.value); setTest('idle'); }} placeholder="00000000-0000-0000-0000-000000000000" /></Field>
                <Field label="Client id (appId)"><Input mono value={clientId} onChange={(e) => { setClientId(e.target.value); setTest('idle'); }} placeholder="00000000-0000-0000-0000-000000000000" /></Field>
              </div>
              <Field label="Client secret" hint={existing ? 'Leave empty to keep the current one.' : 'Stored encrypted; never shown again.'}>
                <Input mono type="password" autoComplete="new-password" value={secret} onChange={(e) => { setSecret(e.target.value); setTest('idle'); }} />
              </Field>
            </div>
          </Step>
          <Field label="Subscription ids · optional" hint="Comma-separated. Empty = every subscription the service principal can see.">
            <Input mono value={subscriptionIds} onChange={(e) => setSubscriptionIds(e.target.value)} placeholder="3f2a9c4e-0d6b-4b7a-8e15-9c1d2f3a4b5c" />
          </Field>
          <Field label="Resource groups · optional" hint="Comma-separated. Empty = every resource group the principal can read.">
            <Input mono value={resourceGroups} onChange={(e) => setResourceGroups(e.target.value)} placeholder="rg-shop-prod" />
          </Field>
          <Field label="Environment · optional" hint="Tag for the discovered functions, e.g. production.">
            <Input mono value={azEnv} onChange={(e) => setAzEnv(e.target.value)} placeholder="production" />
          </Field>
          <Callout icon={<SquareFunction size={16} />}>Azure reports metrics per Function App, so each app is one row on the Serverless page with its functions listed inside. Cold starts and traces need the Functions OpenTelemetry mode sending to TaskTrooper.</Callout>
        </>}

        {provider === 'neon' && <>
          <div className="grid grid-2">
            <Field label="API key" hint={existing ? 'Leave empty to keep the current one.' : 'Neon console → Account settings → API keys. Org keys see every project.'}>
              <Input mono type="password" autoComplete="new-password" value={secret} onChange={(e) => { setSecret(e.target.value); setTest('idle'); }} />
            </Field>
            <Field label="Organization id" hint="Empty for a personal account."><Input mono value={orgId} onChange={(e) => setOrgId(e.target.value)} placeholder="org-shopco" /></Field>
          </div>
          <Field label="Projects" hint="None selected = every project the key can see, including new ones.">
            {neonFound.length
              ? <ChoiceChips label="Neon projects" options={neonFound.map((p) => ({ id: p, label: p }))} value={neonProjects} onChange={setNeonProjects} />
              : <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>Test the connection to list projects.</p>}
          </Field>
          <Callout icon={<DbIcon size={16} />}>Each compute endpoint becomes a database with its branch, compute state (active / idle), size and storage. For top queries, add a direct connection with a Neon role that has <span className="mono">pg_monitor</span>.</Callout>
        </>}

        <div className="row" style={{ gap: 'var(--space-3)', flexWrap: 'wrap' }}>
          <Button onClick={runTest} disabled={test === 'running' || !credsReady} icon={test === 'running' ? <Loader2 size={14} className="spin" /> : undefined}>Test connection</Button>
          {typeof test === 'object' && (
            <span className="row" style={{ gap: 6, fontSize: 'var(--text-sm)', color: test.ok ? 'var(--status-good-text)' : 'var(--status-critical-text)', minWidth: 0 }}>
              {test.ok ? <CheckCircle2 size={14} aria-hidden /> : <AlertOctagon size={14} aria-hidden />}{test.ok ? '' : 'Failed: '}{test.message}
            </span>
          )}
        </div>
        {typeof test === 'object' && test.ok && test.resources && test.resources.length > 0 && provider !== 'vercel' && (
          <DrawerSection title="Will discover" aside={`${test.resources.length}`}>
            <KeyValues items={test.resources.slice(0, 8).map((r) => [r.name, r.detail])} />
          </DrawerSection>
        )}
        {saveErr && <Callout icon={<AlertOctagon size={16} />}>{saveErr}</Callout>}
      </div>
    </Drawer>
  );
}

function Step({ n, title, children }: { n: number; title: string; children: ReactNode }) {
  return (
    <div className="stack" style={{ gap: 'var(--space-2)' }}>
      <div className="row" style={{ gap: 'var(--space-2)', fontWeight: 500 }}>
        <span aria-hidden style={{ width: 'var(--space-5)', height: 'var(--space-5)', borderRadius: 'var(--radius-full)', background: 'var(--accent-soft)', color: 'var(--accent-text)', display: 'inline-flex', alignItems: 'center', justifyContent: 'center', fontSize: 'var(--text-xs)', flex: 'none' }}>{n}</span>
        {title}
      </div>
      <div style={{ minWidth: 0 }}>{children}</div>
    </div>
  );
}

/** Small link-badge "AWS · aws-prod" for rows discovered by an integration. */
export function IntegrationSourceBadge({ it, compact }: { it: Integration; compact?: boolean }) {
  const nav = useNavigate();
  return (
    <a href={`/settings/integrations?id=${encodeURIComponent(it.id)}`} onClick={(e) => { e.preventDefault(); e.stopPropagation(); nav(`/settings/integrations?id=${encodeURIComponent(it.id)}`); }}
      title={`Discovered by ${PROVIDER_LABEL[it.provider]} integration ${it.name}`} style={{ textDecoration: 'none' }}>
      <Badge tone={it.status === 'error' ? 'critical' : 'neutral'} icon={it.status === 'error' ? <AlertOctagon aria-hidden /> : <Cloud aria-hidden />}>
        {{ aws: 'AWS', gcp: 'Google Cloud', azure: 'Azure', vercel: 'Vercel', neon: 'Neon' }[it.provider]}{!compact && <> · {it.name}</>}{it.status === 'error' ? (compact ? ' · stale' : ' · sync failing') : ''}
      </Badge>
    </a>
  );
}
