import { useEffect, useState } from 'react';
import { useNavigate } from 'react-router';
import { Check, FolderTree, RotateCcw } from 'lucide-react';
import { Badge, Button, Callout, Card, Drawer, Input, Select, StatusBadge, Table } from '@/components/ui';
import { services } from '@/data';
import { can, denied } from '@/data/auth';
import { repoServices, setRepoService, type RepoInfo, type RepoServiceMapping } from '@/data/codeintel';

const EVIDENCE: Record<RepoServiceMapping['evidence'], string> = {
  config: '.trooper/config.yaml', sdk: 'SDK init', module: 'module folder', repo: 'repository name',
};
const KIND: Record<RepoServiceMapping['kind'], string> = {
  backend: 'Backend', web: 'Web app', mobile: 'Mobile app', worker: 'Worker', library: 'Library',
};
const KEEP = '\u0000keep';
const IGNORE = '\u0000ignore';
const OTHER = '\u0000other';

/** Status of one folder: linked automatically, by the user, ignored, or waiting for the user. */
function MappingStatus({ m }: { m: RepoServiceMapping }) {
  switch (m.status) {
    case 'matched': return <StatusBadge status="ok" label="Linked automatically" />;
    case 'confirmed': return <Badge tone="accent" icon={<Check />}>Set by you</Badge>;
    case 'ignored': return <Badge>Ignored</Badge>;
    default: return <StatusBadge status="warning" label="Needs a service" />;
  }
}

/** Service picker of one folder; saving queues a full re-analysis. */
function MappingControl({ repoId, m, known, onSaved }: { repoId: string; m: RepoServiceMapping; known: string[]; onSaved: (m: RepoServiceMapping) => void }) {
  const names = known;
  const [typed, setTyped] = useState('');
  const initial = m.status === 'ignored' ? IGNORE : m.service && m.service === m.detected && !names.includes(m.detected) ? KEEP : m.service ?? '';
  const [value, setValue] = useState(initial);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const options = [
    ...(m.status === 'unmatched' ? [{ value: '', label: 'Choose a service…' }] : []),
    ...names.map((n) => ({ value: n, label: n })),
    ...(names.includes(m.detected) ? [] : [{ value: KEEP, label: `Use “${m.detected}” (not reporting yet)` }]),
    { value: OTHER, label: 'Another name…' },
    { value: IGNORE, label: 'Ignore this folder' },
  ];
  // the linked service may have stopped reporting: keep it selectable
  if (m.service && !names.includes(m.service) && m.service !== m.detected) options.splice(0, 0, { value: m.service, label: m.service });
  const save = async (d: Parameters<typeof setRepoService>[1]) => {
    setBusy(true); setError('');
    try { onSaved(await setRepoService(repoId, d)); } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
    setBusy(false);
  };
  const decide = () => {
    if (value === IGNORE) return save({ path: m.path, ignore: true });
    return save({ path: m.path, service: value === KEEP ? m.detected : value === OTHER ? typed.trim() : value });
  };
  // saving the automatic match as-is is allowed: it pins the folder ("Set by you")
  const dirty = value !== '' && (value === OTHER ? typed.trim() !== '' : value !== initial || m.status === 'unmatched' || m.status === 'matched');
  const writable = can('write');
  return (
    <div className="stack" style={{ gap: 'var(--space-1)' }}>
      <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        <Select style={{ flex: '1 1 10rem', minWidth: 0 }} aria-label={`Service for ${m.path}`} value={value}
          disabled={busy || !writable} title={writable ? undefined : denied('write')} onChange={(e) => setValue(e.target.value)} options={options} />
        {value === OTHER && (
          <Input mono style={{ flex: '1 1 10rem', minWidth: 0 }} aria-label={`Service name for ${m.path}`} placeholder="service name as the SDK reports it"
            value={typed} onChange={(e) => setTyped(e.target.value)} onKeyDown={(e) => { if (e.key === 'Enter' && dirty) void decide(); }} />
        )}
        <Button size="sm" variant="primary" disabled={busy || !dirty || !writable} onClick={() => void decide()}>{busy ? 'Saving…' : 'Save'}</Button>
        {(m.status === 'confirmed' || m.status === 'ignored') && (
          <Button size="sm" variant="ghost" icon={<RotateCcw size={12} />} disabled={busy || !writable} title="Back to automatic matching"
            onClick={() => void save({ path: m.path, reset: true })}>Auto</Button>
        )}
      </div>
      {error && <span className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="critical" label="Failed" /><span className="secondary">{error}</span></span>}
    </div>
  );
}

/**
 * Folder → service mapping of a repository (ADR-036): which folder is which service / app, how it was found, and a
 * picker for the folders codeintel could not link to a service with telemetry.
 */
export function RepoServicesDrawer({ repo, onClose }: { repo: RepoInfo; onClose: () => void }) {
  const nav = useNavigate();
  const [rows, setRows] = useState<RepoServiceMapping[] | null>(null);
  const [error, setError] = useState('');
  const [saved, setSaved] = useState(false);
  useEffect(() => {
    let live = true;
    repoServices(repo.id).then((r) => { if (live) setRows(r); }, (e: unknown) => { if (live) setError(e instanceof Error ? e.message : String(e)); });
    return () => { live = false; };
  }, [repo.id]);
  const onSaved = (m: RepoServiceMapping) => {
    setRows((rs) => rs?.map((r) => (r.path === m.path ? m : r)) ?? rs);
    setSaved(true);
  };
  const open = rows?.filter((r) => r.status === 'unmatched').length ?? 0;
  // what needs an answer first, then the biggest folders
  const sorted = [...(rows ?? [])].sort((a, b) => Number(b.status === 'unmatched') - Number(a.status === 'unmatched') || b.items - a.items || a.path.localeCompare(b.path));
  const serviceId = (name?: string) => services.find((s) => s.name === name)?.id;
  // services with telemetry in the selected range, plus those other folders are already linked to
  const known = [...new Set([...services.map((s) => s.name), ...(rows ?? []).filter((r) => r.status !== 'ignored' && r.service).map((r) => r.service!)])].sort();
  return (
    <Drawer open onClose={onClose} width={860} title={`Services in ${repo.name}`}
      subtitle={rows ? `${rows.length} folder${rows.length === 1 ? '' : 's'} · ${open ? `${open} need${open === 1 ? 's' : ''} a service` : 'all linked'}` : 'Loading…'}>
      <Callout icon={<FolderTree size={16} />}>
        Each folder below owns endpoints, consumers or pages found in the code. Folders are linked to a service with telemetry by name;
        when none fits, pick the service it reports as, keep the name from the code if it does not report yet, or ignore the folder.
        Saving re-analyzes the repository so the inventory and suggested monitors move to that service.
      </Callout>
      {saved && repo.status === 'analyzing' && <span className="secondary">Re-analysis queued.</span>}
      {error && <div className="row" style={{ gap: 'var(--space-2)' }}><StatusBadge status="critical" label="Failed" /><span className="secondary">{error}</span></div>}
      {rows && (
        <Card flush>
          <Table rows={sorted} rowKey={(r) => r.path} empty="No folders yet: they appear after the first analysis." columns={[
            { key: 'path', header: 'Folder', render: (r) => (
              <div className="stack" style={{ gap: 2, minWidth: 0 }}>
                <span className="mono" style={{ wordBreak: 'break-all' }}>{r.path === '.' ? '(repository root)' : r.path}</span>
                <span className="muted" style={{ fontSize: 'var(--text-xs)' }}>
                  {KIND[r.kind]} · {r.items} item{r.items === 1 ? '' : 's'} · named <span className="mono">{r.detected}</span> from {EVIDENCE[r.evidence]}
                </span>
              </div>
            ) },
            { key: 'status', header: 'Status', width: 170, render: (r) => (
              <div className="stack" style={{ gap: 2, alignItems: 'flex-start' }}>
                <MappingStatus m={r} />
                {r.service && serviceId(r.service) && (
                  <a className="mono" style={{ fontSize: 'var(--text-xs)' }} href={`/services/${serviceId(r.service)}`}
                    onClick={(e) => { e.preventDefault(); onClose(); nav(`/services/${serviceId(r.service)}`); }}>{r.service}</a>
                )}
              </div>
            ) },
            { key: 'service', header: 'Service', width: 300, render: (r) => <MappingControl key={`${r.path}:${r.status}:${r.service ?? ''}`} repoId={repo.id} m={r} known={known} onSaved={onSaved} /> },
          ]} />
        </Card>
      )}
    </Drawer>
  );
}

/** Code insights banner: repositories with folders no telemetry service fits. Renders nothing when all are linked. */
export function UnmappedFoldersNotice({ repos, onOpen }: { repos: RepoInfo[]; onOpen: (r: RepoInfo) => void }) {
  const todo = repos.filter((r) => r.unmappedServices > 0 && r.status !== 'disconnected');
  if (!todo.length) return null;
  return (
    <Callout icon={<FolderTree size={16} />}>
      <div className="stack" style={{ gap: 'var(--space-2)' }}>
        <span><strong>Some folders could not be linked to a service.</strong> Tell us which service they report as, so their endpoints and monitors land on the right service page.</span>
        <div className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
          {todo.map((r) => (
            <Button key={r.id} size="sm" onClick={() => onOpen(r)}>
              {r.name} · {r.unmappedServices} folder{r.unmappedServices === 1 ? '' : 's'}
            </Button>
          ))}
        </div>
      </div>
    </Callout>
  );
}
