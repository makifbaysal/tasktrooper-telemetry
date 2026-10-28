import { useState, type FormEvent } from 'react';
import { KeyRound, LogIn, Mail } from 'lucide-react';
import { Button, Callout, Field, Input } from '@/components/ui';
import { Logo } from '@/components/layout/Logo';
import { devLogin, loginUrl, type AuthConfig } from '@/data/auth';

/**
 * Sign-in (ADR-016). OIDC → the api's /auth/login redirect; dev login (TT_AUTH_DEV, no issuer) → e-mail only.
 * Rendered by main.tsx outside the router, before any data is loaded.
 */
export function Login({ config, expired, onSignedIn }: { config: AuthConfig; expired?: boolean; onSignedIn: () => void }) {
  const [email, setEmail] = useState('');
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState<string>();

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    if (!email.trim()) return setError('Enter your e-mail address.');
    setBusy(true); setError(undefined);
    try { await devLogin(email.trim()); onSignedIn(); } catch (err) { setError(err instanceof Error ? err.message : String(err)); setBusy(false); }
  };

  return (
    <div className="login">
      <div className="login__card">
        <div className="login__brand">
          <span className="sidebar__logo"><Logo /></span>
          <span className="sidebar__name"><strong>TaskTrooper</strong><span>Telemetry</span></span>
        </div>
        <div>
          <div className="login__title">Sign in</div>
          <p className="secondary">{expired ? 'Your session expired. Sign in again to continue.' : 'Use your company account to open the dashboard.'}</p>
        </div>

        {config.oidc && (
          <Button variant="primary" icon={<LogIn size={16} />} onClick={() => { window.location.href = loginUrl(); }}>Continue with SSO</Button>
        )}
        {config.oidc && config.devLogin && <div className="login__or">or</div>}
        {config.devLogin && (
          <form className="stack" style={{ gap: 'var(--space-3)' }} onSubmit={(e) => void submit(e)}>
            <Field label="E-mail" hint="Development sign-in — no password, only enabled when the api runs with TT_AUTH_DEV=true.">
              <Input type="email" autoComplete="email" autoFocus placeholder="you@company.com" value={email} onChange={(e) => setEmail(e.target.value)} />
            </Field>
            <Button type="submit" variant={config.oidc ? 'secondary' : 'primary'} icon={<Mail size={16} />} disabled={busy}>{busy ? 'Signing in…' : 'Sign in with e-mail'}</Button>
          </form>
        )}
        {!config.oidc && !config.devLogin && (
          <Callout icon={<KeyRound size={16} />}>Sign-in isn’t configured on this api. Set <span className="mono">TT_OIDC_ISSUER</span>, or run the dashboard with <span className="mono">VITE_API_KEY</span>.</Callout>
        )}
        {error && <Callout icon={<KeyRound size={16} color="var(--status-critical)" />}>{error}</Callout>}
      </div>
    </div>
  );
}
