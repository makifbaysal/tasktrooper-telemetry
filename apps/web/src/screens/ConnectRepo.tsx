import { useEffect, useState } from 'react';
import { Check, ChevronDown, ChevronRight, FolderGit2, Lock, Plus } from 'lucide-react';
import { Badge, Button, Drawer, DrawerSection, EmptyState, Field, Input, SearchInput, Segmented, StatusBadge } from '@/components/ui';
import { isApi } from '@/data';
import {
  connectRepo, githubInstallUrl, githubInstallations, githubRepos, mockInstallGithub,
  type GithubInstallation, type GithubRepoOption,
} from '@/data/codeintel';

type Advanced = 'token' | 'local';

/**
 * "Connect repository" side panel (ADR-017). Primary: GitHub App — install (full-page redirect to the api),
 * then pick repositories of the installation. Advanced: access token or a local checkout (dev).
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
  const [mode, setMode] = useState<Advanced>('token');
  const [fullName, setFullName] = useState('');
  const [branch, setBranch] = useState('main');
  const [token, setToken] = useState('');
  const [localPath, setLocalPath] = useState('');
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

  const finish = () => { setPicked([]); setFullName(''); setToken(''); setLocalPath(''); setBranch('main'); setError(''); onClose(); };
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
  const addAdvanced = () => run(() => connectRepo(mode === 'local'
    ? { provider: 'local', fullName: fullName.trim() || localPath.trim().split('/').filter(Boolean).pop() || 'local', localPath: localPath.trim(), defaultBranch: branch.trim() || undefined }
    : { provider: 'github', fullName: fullName.trim(), token: token.trim(), defaultBranch: branch.trim() || undefined }));
  const advValid = mode === 'local' ? !!localPath.trim() : /^[\w.-]+\/[\w.-]+$/.test(fullName.trim()) && !!token.trim();

  const primary = advanced
    ? <Button size="sm" variant="primary" icon={<Plus size={14} />} disabled={!advValid || busy} onClick={() => void addAdvanced()}>{busy ? 'Connecting…' : 'Connect & analyze'}</Button>
    : installation
      ? <Button size="sm" variant="primary" icon={<Plus size={14} />} disabled={!picked.length || busy} onClick={() => void addPicked()}>
          {busy ? 'Adding…' : picked.length ? `Add ${picked.length} repositor${picked.length === 1 ? 'y' : 'ies'}` : 'Add repositories'}
        </Button>
      : undefined;

  return (
    <Drawer open={open} onClose={onClose} width={560} title="Connect repository"
      subtitle="TaskTrooper reads routes, consumers and jobs — no code is executed" actions={primary}>
      <DrawerSection title="GitHub" aside={installation && <span className="row muted" style={{ gap: 4 }}><FolderGit2 size={12} aria-hidden />{installation.account}</span>}>
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
        aside={!advanced ? 'access token or local path' : undefined}>
        {advanced ? (
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <Segmented label="Advanced source" value={mode} onChange={setMode} options={[
              { id: 'token', label: 'Access token' },
              { id: 'local', label: 'Local path (dev)' },
            ]} />
            {mode === 'token' ? (
              <>
                <Field label="Repository" hint="owner/name"><Input mono value={fullName} placeholder="shopco/orders-service" onChange={(e) => setFullName(e.target.value)} /></Field>
                <Field label="Token" hint="Fine-grained token with read-only Contents and Metadata access. Stored encrypted; never shown again. No webhook: re-analyze manually.">
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
          </div>
        ) : <span className="muted">Use when the GitHub App can't be installed, or to analyze a checkout on this machine.</span>}
      </DrawerSection>

      {error && (
        <DrawerSection title="Could not connect">
          <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="critical" label="Error" /><span className="secondary">{error}</span></div>
        </DrawerSection>
      )}
    </Drawer>
  );
}
