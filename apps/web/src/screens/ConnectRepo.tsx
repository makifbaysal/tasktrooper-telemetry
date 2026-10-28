import { useEffect, useState } from 'react';
import { Check, ChevronDown, ChevronRight, FolderGit2, Lock, Plus, Webhook } from 'lucide-react';
import { Badge, Button, Callout, CopyCode, Drawer, DrawerSection, EmptyState, Field, Input, SearchInput, Segmented, StatusBadge } from '@/components/ui';
import { isApi } from '@/data';
import {
  baseUrlError, connectRepo, fullNameError, githubInstallUrl, githubInstallations, githubRepos, mockInstallGithub,
  type GithubInstallation, type GithubRepoOption, type RepoProvider, type RepoWebhook,
} from '@/data/codeintel';

type Advanced = 'gitlab' | 'bitbucket' | 'token' | 'local';

const PROVIDER_LABEL: Record<RepoProvider, string> = { github: 'GitHub', gitlab: 'GitLab', bitbucket: 'Bitbucket', local: 'Local' };

/** Where the provider's webhook form lives and what to tick (contracts §18a). */
const HOOK_STEPS: Record<Exclude<RepoProvider, 'local'>, string> = {
  gitlab: 'In GitLab: project → Settings → Webhooks → Add new webhook. Paste the URL, put the secret into "Secret token" and tick "Push events".',
  bitbucket: 'In Bitbucket: repository settings → Webhooks → Add webhook. Paste the URL, put the secret into "Secret" and choose the "Repository push" trigger.',
  github: 'In GitHub: repository → Settings → Webhooks → Add webhook. Paste the URL, content type application/json, put the secret into "Secret" and send "Just the push event".',
};

/** Webhook URL + secret right after connecting (shown once) and how to set it up on the provider. */
export function WebhookSetup({ provider, hook }: { provider: RepoProvider; hook: RepoWebhook }) {
  if (provider === 'local') return null;
  return (
    <div className="stack" style={{ gap: 'var(--space-3)' }}>
      <p className="secondary" style={{ margin: 0 }}>{HOOK_STEPS[provider]} Pushes to the default branch are then analyzed automatically.</p>
      <Field label="Webhook URL"><CopyCode label="Copy webhook URL" wrap>{hook.url}</CopyCode></Field>
      <Field label="Secret" hint="Shown only now. Lost it? Issue a new one from the repository row."><CopyCode label="Copy secret" wrap>{hook.secret}</CopyCode></Field>
    </div>
  );
}

/**
 * "Connect repository" side panel (ADR-017). Primary: GitHub App — install (full-page redirect to the api),
 * then pick repositories of the installation. Other sources: GitLab (gitlab.com or self-managed), Bitbucket Cloud, a
 * GitHub access token or a local checkout (dev). Token-based repos get a webhook URL + secret, shown once (contracts §18a).
 * `integrationId`: the installation the user just came back from (`?github=<id>`).
 */
export function ConnectRepoDrawer({ open, onClose, integrationId }: { open: boolean; onClose: () => void; integrationId?: string }) {
  const [installs, setInstalls] = useState<GithubInstallation[] | null>(null);
  const [inst, setInst] = useState<string>('');
  const [q, setQ] = useState('');
  const [options, setOptions] = useState<GithubRepoOption[] | null>(null);
  const [listError, setListError] = useState('');
  const [picked, setPicked] = useState<string[]>([]);
  const [advanced, setAdvanced] = useState(false);
  const [mode, setMode] = useState<Advanced>('gitlab');
  const [fullName, setFullName] = useState('');
  const [branch, setBranch] = useState('main');
  const [token, setToken] = useState('');
  const [baseUrl, setBaseUrl] = useState('');
  const [localPath, setLocalPath] = useState('');
  const [hook, setHook] = useState<{ provider: RepoProvider; name: string; webhook: RepoWebhook } | null>(null);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');

  // installations of this project
  useEffect(() => {
    if (!open) return;
    let alive = true;
    void githubInstallations().then((list) => {
      if (!alive) return;
      setInstalls(list);
      setInst((cur) => cur || (integrationId && list.some((i) => i.id === integrationId) ? integrationId : list[0]?.id ?? integrationId ?? ''));
    });
    return () => { alive = false; };
  }, [open, integrationId]);

  // repos of the chosen installation (debounced search)
  useEffect(() => {
    if (!open || !inst) return;
    let alive = true;
    setListError('');
    const t = setTimeout(() => {
      githubRepos(inst, q)
        .then((r) => { if (alive) setOptions(r); })
        .catch((e: unknown) => { if (alive) { setOptions([]); setListError(e instanceof Error ? e.message : String(e)); } });
    }, 250);
    return () => { alive = false; clearTimeout(t); };
  }, [open, inst, q]);

  const installation = installs?.find((i) => i.id === inst);
  const install = () => {
    if (!isApi) { const m = mockInstallGithub(); setInstalls([m]); setInst(m.id); return; }
    window.location.assign(githubInstallUrl());
  };
  const toggle = (name: string) => setPicked((p) => (p.includes(name) ? p.filter((x) => x !== name) : [...p, name]));

  const finish = () => {
    setPicked([]); setFullName(''); setToken(''); setLocalPath(''); setBaseUrl(''); setBranch('main'); setError(''); setHook(null); onClose();
  };
  const run = async (f: () => Promise<unknown>) => {
    setBusy(true); setError('');
    try { await f(); finish(); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  const addPicked = () => run(async () => {
    for (const name of picked) {
      const o = options?.find((x) => x.fullName === name);
      await connectRepo({ provider: 'github', fullName: name, defaultBranch: o?.defaultBranch, installationId: installation?.installationId });
    }
  });
  const provider: RepoProvider = mode === 'token' ? 'github' : mode;
  const addAdvanced = async () => {
    setBusy(true); setError('');
    try {
      const r = await connectRepo(mode === 'local'
        ? { provider: 'local', fullName: fullName.trim() || localPath.trim().split('/').filter(Boolean).pop() || 'local', localPath: localPath.trim(), defaultBranch: branch.trim() || undefined }
        : { provider, fullName: fullName.trim(), token: token.trim() || undefined, defaultBranch: branch.trim() || undefined,
            baseUrl: mode === 'gitlab' ? baseUrl.trim() || undefined : undefined });
      // the webhook secret is shown once: keep the drawer open on the setup steps
      if (r.webhook && r.provider) setHook({ provider: r.provider, name: r.name, webhook: r.webhook });
      else finish();
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  const nameErr = mode === 'local' || !fullName.trim() ? '' : fullNameError(provider, fullName);
  const urlErr = mode === 'gitlab' ? baseUrlError(baseUrl) : '';
  const advValid = mode === 'local' ? !!localPath.trim()
    : !!fullName.trim() && !nameErr && !urlErr && (mode !== 'token' || !!token.trim());

  const primary = hook
    ? <Button size="sm" variant="primary" icon={<Check size={14} />} onClick={finish}>Done</Button>
    : advanced
    ? <Button size="sm" variant="primary" icon={<Plus size={14} />} disabled={!advValid || busy} onClick={() => void addAdvanced()}>{busy ? 'Connecting…' : 'Connect & analyze'}</Button>
    : installation
      ? <Button size="sm" variant="primary" icon={<Plus size={14} />} disabled={!picked.length || busy} onClick={() => void addPicked()}>
          {busy ? 'Adding…' : picked.length ? `Add ${picked.length} repositor${picked.length === 1 ? 'y' : 'ies'}` : 'Add repositories'}
        </Button>
      : undefined;

  return (
    <Drawer open={open} onClose={onClose} width={560} title="Connect repository"
      subtitle="TaskTrooper reads routes, consumers and jobs — no code is executed" actions={primary}>
      {hook && (
        <DrawerSection title={`Set up the ${PROVIDER_LABEL[hook.provider]} webhook`} aside={<span className="mono">{hook.name}</span>}>
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="ok" label="Connected" /><span className="secondary">The first analysis has started.</span></div>
            <WebhookSetup provider={hook.provider} hook={hook.webhook} />
          </div>
        </DrawerSection>
      )}
      {!hook && (<>
      <DrawerSection title="GitHub"aside={installation && <span className="row muted" style={{ gap: 4 }}><FolderGit2 size={12} aria-hidden />{installation.account}</span>}>
        {installs === null ? (
          <span className="muted">Loading installations…</span>
        ) : !installation ? (
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <p className="secondary" style={{ margin: 0 }}>
              Install the TaskTrooper GitHub App on your account or organization and choose which repositories it can read. Pushes to the default branch are analyzed automatically.
            </p>
            <div><Button variant="primary" icon={<FolderGit2 size={14} />} onClick={install}>Connect GitHub</Button></div>
          </div>
        ) : (
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
              {installs.length > 1 && (
                <Segmented label="Installation" value={inst} onChange={(v) => { setInst(v); setPicked([]); setOptions(null); }}
                  options={installs.map((i) => ({ id: i.id, label: i.account }))} />
              )}
              <div style={{ flex: 1, minWidth: 200 }}>
                <SearchInput value={q} placeholder="Search repositories" aria-label="Search repositories" onChange={(e) => setQ(e.target.value)} />
              </div>
              <Button size="sm" variant="ghost" icon={<FolderGit2 size={14} />} onClick={install}>Add account</Button>
            </div>
            {listError && <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="critical" label="Error" /><span className="secondary">{listError}</span></div>}
            {options === null ? <span className="muted">Loading repositories…</span>
              : options.length === 0 && !listError ? <EmptyState icon={<FolderGit2 size={20} />} title={q ? 'No repository matches' : 'The installation has no repositories'}>Change the repository access of the GitHub App on GitHub.</EmptyState>
              : (
                <div className="list" role="group" aria-label="Repositories">
                  {options.map((o) => {
                    const on = picked.includes(o.fullName);
                    return (
                      <label key={o.fullName} className={`list__row${o.added ? '' : ' list__row--clickable'}`} style={{ cursor: o.added ? 'default' : 'pointer' }}>
                        <input type="checkbox" checked={o.added || on} disabled={o.added} onChange={() => toggle(o.fullName)} aria-label={o.fullName} />
                        <span className="list__main">
                          <span className="list__title mono">{o.fullName}</span>
                          <span className="list__meta">{[o.language, o.defaultBranch].filter(Boolean).join(' · ')}</span>
                        </span>
                        {o.private && <Badge icon={<Lock />}>Private</Badge>}
                        {o.added && <Badge tone="good" icon={<Check />}>Added</Badge>}
                      </label>
                    );
                  })}
                </div>
              )}
          </div>
        )}
      </DrawerSection>

      <DrawerSection title={
        <button type="button" className="row" onClick={() => setAdvanced((a) => !a)} aria-expanded={advanced}
          style={{ all: 'unset', display: 'inline-flex', alignItems: 'center', gap: 'var(--space-1)', cursor: 'pointer' }}>
          {advanced ? <ChevronDown size={14} aria-hidden /> : <ChevronRight size={14} aria-hidden />}Advanced
        </button>}
        aside={!advanced ? 'GitLab, Bitbucket, access token or local path' : undefined}>
        {advanced ? (
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <Segmented label="Source" value={mode} onChange={(m) => { setMode(m); setError(''); }} options={[
              { id: 'gitlab', label: 'GitLab' },
              { id: 'bitbucket', label: 'Bitbucket' },
              { id: 'token', label: 'GitHub token' },
              { id: 'local', label: 'Local path (dev)' },
            ]} />
            {mode === 'gitlab' ? (
              <>
                <Field label="Project" hint={nameErr || 'group/project — subgroups allowed (group/subgroup/project)'}>
                  <Input mono value={fullName} placeholder="shopco/platform/orders-service" aria-invalid={!!nameErr} onChange={(e) => setFullName(e.target.value)} />
                </Field>
                <Field label="GitLab URL" hint={urlErr || 'Self-managed GitLab only (https). Leave empty for gitlab.com.'}>
                  <Input mono value={baseUrl} placeholder="https://gitlab.example.com" aria-invalid={!!urlErr} onChange={(e) => setBaseUrl(e.target.value)} />
                </Field>
                <Field label="Access token" hint="Project, group or personal access token with the read_repository scope. Stored encrypted; never shown again. Leave empty for a public project.">
                  <Input mono type="password" autoComplete="off" value={token} placeholder="glpat-…" onChange={(e) => setToken(e.target.value)} />
                </Field>
              </>
            ) : mode === 'bitbucket' ? (
              <>
                <Field label="Repository" hint={nameErr || 'workspace/repo (Bitbucket Cloud)'}>
                  <Input mono value={fullName} placeholder="shopco/orders-service" aria-invalid={!!nameErr} onChange={(e) => setFullName(e.target.value)} />
                </Field>
                <Field label="Access token" hint="Repository, project or workspace access token with Repositories: Read — or username:app_password. Stored encrypted; never shown again. Leave empty for a public repository.">
                  <Input mono type="password" autoComplete="off" value={token} placeholder="ATCTT3x… or jane:app-password" onChange={(e) => setToken(e.target.value)} />
                </Field>
              </>
            ) : mode === 'token' ? (
              <>
                <Field label="Repository" hint={nameErr || 'owner/name'}><Input mono value={fullName} placeholder="shopco/orders-service" aria-invalid={!!nameErr} onChange={(e) => setFullName(e.target.value)} /></Field>
                <Field label="Token" hint="Fine-grained token with read-only Contents and Metadata access. Stored encrypted; never shown again.">
                  <Input mono type="password" autoComplete="off" value={token} placeholder="github_pat_…" onChange={(e) => setToken(e.target.value)} />
                </Field>
              </>
            ) : (
              <>
                <Field label="Path" hint="Absolute path readable by the codeintel service (development only)">
                  <Input mono value={localPath} placeholder="/Users/me/src/orders-service" onChange={(e) => setLocalPath(e.target.value)} />
                </Field>
                <Field label="Name" hint="optional — defaults to the folder name"><Input value={fullName} onChange={(e) => setFullName(e.target.value)} /></Field>
              </>
            )}
            <Field label="Default branch"><Input mono value={branch} onChange={(e) => setBranch(e.target.value)} /></Field>
            {mode !== 'local' && (
              <Callout icon={<Webhook size={16} aria-hidden />}>
                After connecting you get a webhook URL and secret for {PROVIDER_LABEL[provider]} so pushes to the default branch are analyzed automatically.
              </Callout>
            )}
          </div>
        ) : <span className="muted">Repositories on GitLab or Bitbucket, GitHub without the App, or a checkout on this machine.</span>}
      </DrawerSection>
      </>)}

      {error && (
        <DrawerSection title="Could not connect">
          <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="critical" label="Error" /><span className="secondary">{error}</span></div>
        </DrawerSection>
      )}
    </Drawer>
  );
}
