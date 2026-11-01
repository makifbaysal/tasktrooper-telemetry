import { useState, type FormEvent } from 'react';
import { ArrowRight, Building2, KeyRound, LogOut, TriangleAlert } from 'lucide-react';
import { Button, Callout, CopyCode, Field, Input, SdkQuickstart } from '@/components/ui';
import { Logo } from '@/components/layout/Logo';
import { createOrg, logout, useAuth, type CreatedOrg } from '@/data/auth';

/**
 * First run (ADR-028, contracts §11): a signed-in user without any org creates one with its first project
 * (`POST /orgs`), then sees the ingest key + DSN exactly once with SDK quick starts. Rendered by main.tsx outside the router.
 */
export function Onboarding({ onDone }: { onDone: () => void }) {
  const { me } = useAuth();
  const [org, setOrg] = useState('');
  const [project, setProject] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();
  const [created, setCreated] = useState<CreatedOrg>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!org.trim()) return setError('Name your organization.');
    if (!project.trim()) return setError('Name your first project.');
    setBusy(true); setError(undefined);
    try { setCreated(await createOrg(org.trim(), project.trim())); } catch (err) { setError(err instanceof Error ? err.message : String(err)); }
    setBusy(false);
  };

  return (
    <div className="login">
      <div className={`login__card${created ? ' onboarding__card' : ''}`}>
        <div className="login__brand">
          <span className="sidebar__logo"><Logo /></span>
          <span className="sidebar__name"><strong>TaskTrooper</strong><span>Telemetry</span></span>
        </div>
        <ol className="onboarding__steps" aria-label="Setup steps" style={{ listStyle: 'none', margin: 0, padding: 0 }}>
          <li aria-current={created ? undefined : 'step'}>1 · Organization</li>
          <li aria-hidden>→</li>
          <li aria-current={created ? 'step' : undefined}>2 · Connect an SDK</li>
        </ol>

        {!created ? (
          <>
            <div>
              <div className="login__title">Create your organization</div>
              <p className="secondary">
                {me?.user.email ? <>Signed in as <strong>{me.user.email}</strong>. </> : null}
                You’ll be its owner. A project groups the services, apps and alerts of one product or environment.
              </p>
            </div>
            <form className="stack" style={{ gap: 'var(--space-3)' }} onSubmit={(e) => void submit(e)}>
              <Field label="Organization">
                <Input autoFocus placeholder="Acme Inc." value={org} onChange={(e) => setOrg(e.target.value)} maxLength={120} />
              </Field>
              <Field label="First project" hint="You can add more projects later in Settings.">
                <Input placeholder="production" value={project} onChange={(e) => setProject(e.target.value)} maxLength={120} />
              </Field>
              <Button type="submit" variant="primary" icon={<Building2 size={16} />} disabled={busy}>{busy ? 'Creating…' : 'Create organization'}</Button>
            </form>
            {error && <Callout icon={<TriangleAlert size={16} color="var(--status-critical)" />}>{error}</Callout>}
            <Button variant="ghost" icon={<LogOut size={16} />} onClick={() => void logout()}>Sign out</Button>
          </>
        ) : (
          <>
            <div>
              <div className="login__title">Connect your first service</div>
              <p className="secondary">
                <strong>{created.org.name}</strong> / <strong>{created.project.name}</strong> is ready. Add an SDK with this DSN — telemetry shows up
                in the dashboard within a minute.
              </p>
            </div>
            {created.dsn && (
              <Field label="DSN" hint="Set it as TROOPER_DSN or pass it to init().">
                <CopyCode wrap label="Copy DSN">{created.dsn}</CopyCode>
              </Field>
            )}
            {created.ingestKey.key && (
              <Field label="Ingest key">
                <CopyCode wrap label="Copy ingest key">{created.ingestKey.key}</CopyCode>
              </Field>
            )}
            <Callout icon={<KeyRound size={16} color="var(--status-serious)" />}>
              The key is shown only once. Copy it now — you can create new keys in Settings → API keys.
            </Callout>
            <SdkQuickstart dsn={created.dsn || '<dsn>'} />
            <Button variant="primary" icon={<ArrowRight size={16} />} onClick={onDone}>Continue to the dashboard</Button>
          </>
        )}
      </div>
    </div>
  );
}
