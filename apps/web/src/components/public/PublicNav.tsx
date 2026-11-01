import { useEffect } from 'react';
import { Link, NavLink, useLocation } from 'react-router';
import { Logo } from '@/components/layout/Logo';
import { useAuth } from '@/data/auth';
import { REPO } from './docsContent';

/**
 * Top bar of the public pages. `tone="field"` sits on the navy hero; `tone="page"` on the normal page ground.
 * Signed-in visitors get "Open dashboard" instead of "Sign in".
 */
export function PublicNav({ tone = 'page' }: { tone?: 'field' | 'page' }) {
  const { me } = useAuth();
  return (
    <header className={`pnav pnav--${tone}`}>
      <Link to="/" className="pnav__brand" aria-label="TaskTrooper Telemetry home">
        <Logo className="pnav__mark" />
        <span>TaskTrooper <span className="pnav__product">Telemetry</span></span>
      </Link>
      <nav className="pnav__links" aria-label="Site">
        {!me && <Link to={{ pathname: '/', hash: 'product' }}>Product</Link>}
        {!me && <Link to={{ pathname: '/', hash: 'sdks' }}>SDKs</Link>}
        <NavLink to="/docs">Docs</NavLink>
        <a href={REPO} target="_blank" rel="noreferrer">GitHub</a>
      </nav>
      <span className="pnav__spacer" />
      {me
        ? <Link to="/" className="pbtn pbtn--quiet">Open dashboard</Link>
        : <Link to="/login" className="pbtn pbtn--quiet">Sign in</Link>}
    </header>
  );
}

export function PublicFooter() {
  return (
    <footer className="pfoot">
      <span className="pfoot__brand"><Logo className="pnav__mark" /> TaskTrooper Telemetry</span>
      <nav className="pfoot__links" aria-label="Footer">
        <Link to="/docs">Docs</Link>
        <Link to="/docs#mcp">MCP server</Link>
        <a href={REPO} target="_blank" rel="noreferrer">GitHub</a>
        <Link to="/login">Sign in</Link>
      </nav>
    </footer>
  );
}

/** Client-side navigation does not scroll to `#hash`; this does, after the target renders. */
export function useHashScroll() {
  const { hash, pathname } = useLocation();
  useEffect(() => {
    if (!hash) { window.scrollTo(0, 0); return; }
    const el = document.getElementById(decodeURIComponent(hash.slice(1)));
    el?.scrollIntoView({ block: 'start' });
  }, [hash, pathname]);
}
