import { useState, type FormEvent } from 'react';
import { ArrowLeft, KeyRound, LockKeyhole, LogIn, Mail } from 'lucide-react';
import { Button, Callout, Field, Input } from '@/components/ui';
import { Logo } from '@/components/layout/Logo';
import { ServiceGraph } from '@/components/public/ServiceMap';
import { devLogin, loginUrl, passwordLogin, type AuthConfig } from '@/data/auth';

/**
 * Sign-in (ADR-016), its own page at `/login`. OIDC → the api's /auth/login redirect; password login (TT_AUTH_PASSWORD,
 * ADR-035: accounts made with `api set-password`, e.g. the shared demo viewer) → e-mail + password; dev login (TT_AUTH_DEV,
 * no issuer) → e-mail only. There is no sign-up: teammates are invited; the first sign-in sets up the organisation (onboarding).
 * Also rendered outside any router when a session expires mid-use, so it links with plain anchors.
 * `next` is the in-app path to land on after signing in.
 */
export function Login({ config, expired, next = '/', onSignedIn }: { config: AuthConfig; expired?: boolean; next?: string; onSignedIn: () => void }) {
  const [email, setEmail] = useState('');
  const [pwEmail, setPwEmail] = useState('');
  const [password, setPassword] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const run = async (login: () => Promise<void>) => {
    setBusy(true); setError(undefined);
    try { await login(); onSignedIn(); } catch (err) { setError(err instanceof Error ? err.message : String(err)); setBusy(false); }
  };
  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!email.trim()) return setError('Enter your e-mail address.');
    await run(() => devLogin(email.trim()));
  };
  const submitPassword = async (e: FormEvent) => {
    e.preventDefault();
    if (!pwEmail.trim() || !password) return setError('Enter your e-mail address and password.');
    await run(() => passwordLogin(pwEmail.trim(), password));
  };

  return (
    <div className="signin">
      <aside className="signin__field">
        <a href="/" className="pnav__brand">
          <Logo className="pnav__mark" />
          <span>TaskTrooper <span className="pnav__product">Telemetry</span></span>
        </a>
        <div className="signin__pitch">
          <p className="signin__statement">Know why it’s slow, not just that it is.</p>
          <p>Backend, web and mobile in one place, with the cause named when something breaks.</p>
        </div>
        <ServiceGraph narrow className="signin__graph" />
      </aside>

      <main className="signin__main">
        <a href="/" className="signin__back"><ArrowLeft size={14} aria-hidden /> Back to the site</a>
        <div className="signin__form">
          <div>
            <h1 className="signin__title">{expired ? 'Session expired' : 'Sign in'}</h1>
            <p className="secondary">{expired ? 'Sign in again to continue where you left off.' : 'Use your company account to open the dashboard.'}</p>
          </div>

          {config.oidc && (
            <Button variant="primary" icon={<LogIn size={16} />} onClick={() => { window.location.href = loginUrl(new URL(next, window.location.origin).href); }}>Continue with SSO</Button>
          )}
          {config.oidc && config.password && <div className="signin__or">or</div>}
          {config.password && (
            <form className="signin__email" onSubmit={(e) => void submitPassword(e)}>
              <Field label="E-mail">
                <Input type="email" name="email" autoComplete="username" placeholder="you@company.com" value={pwEmail} onChange={(e) => setPwEmail(e.target.value)} />
              </Field>
              <Field label="Password">
                <Input type="password" name="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} />
              </Field>
              <Button type="submit" variant={config.oidc ? 'secondary' : 'primary'} icon={<LockKeyhole size={16} />} disabled={busy}>{busy ? 'Signing in…' : 'Sign in with password'}</Button>
            </form>
          )}
          {(config.oidc || config.password) && config.devLogin && <div className="signin__or">or</div>}
          {config.devLogin && (
            <form className="signin__email" onSubmit={(e) => void submit(e)}>
              <Field label="E-mail" hint="Development sign-in: no password. Only enabled when the api runs with TT_AUTH_DEV=true.">
                <Input type="email" autoComplete="email" autoFocus={!config.password} placeholder="you@company.com" value={email} onChange={(e) => setEmail(e.target.value)} />
              </Field>
              <Button type="submit" variant={config.oidc || config.password ? 'secondary' : 'primary'} icon={<Mail size={16} />} disabled={busy}>{busy ? 'Signing in…' : 'Sign in with e-mail'}</Button>
            </form>
          )}
          {!config.oidc && !config.devLogin && !config.password && (
            <Callout icon={<KeyRound size={16} />}>Sign-in isn’t configured on this api. Set <span className="mono">TT_OIDC_ISSUER</span> (OIDC), <span className="mono">TT_AUTH_PASSWORD=true</span> (password accounts), or <span className="mono">TT_AUTH_DEV=true</span> for local development.</Callout>
          )}
          {error && <Callout icon={<KeyRound size={16} color="var(--status-critical)" />}>{error}</Callout>}
          <p className="signin__fine">No account? Ask a teammate to invite you. The first person to sign in sets up the organisation.</p>
        </div>
      </main>
    </div>
  );
}
