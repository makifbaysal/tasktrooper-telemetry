import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { RouterProvider } from 'react-router';
import './styles/tokens.css';
import './styles/base.css';
import './styles/components.css';
import './styles/charts.css';
import './styles/account.css';
import './styles/trace.css';
import './styles/screens.css';
import './styles/assistant.css';
import './styles/motion.css';
import './styles/public.css';
import { router } from './router';
import { AppProvider } from './components/layout/context';
import { API_URL, hydrate, startPolling } from './data/remote';
import { authState, initAuth, useAuth } from './data/auth';
import { Login } from './screens/Login';
import { createPublicRouter } from './publicRouter';
import { Skeleton, SkeletonPage } from './components/ui';
import { Onboarding } from './screens/Onboarding';
import { rootErrorOptions, startTelemetry } from './lib/telemetry';

const root = createRoot(document.getElementById('root')!, rootErrorOptions);

function renderApp() {
  root.render(
    <StrictMode>
      <AppProvider>
        <SessionGate />
      </AppProvider>
    </StrictMode>,
  );
}

/** Remembers whether the last boot ended signed in, so the next boot shows the right outline (app shell vs. public). */
const SESSION_HINT = 'trooper-signed-in';
function hint(v?: boolean): boolean {
  try {
    if (v === true) localStorage.setItem(SESSION_HINT, '1');
    else if (v === false) localStorage.removeItem(SESSION_HINT);
    return localStorage.getItem(SESSION_HINT) === '1';
  } catch { return false; }
}

/** Boot placeholder: the app shell in shimmer while auth + data load, or the navy field for signed-out visitors. */
function renderLoading() {
  if (!hint()) return root.render(<div className="boot-field" role="status" aria-label="Loading" />);
  root.render(
    <div className="boot-shell" role="status" aria-label="Loading the dashboard">
      <aside className="boot-shell__side">
        <Skeleton w="70%" h="var(--space-6)" />
        <Skeleton w="100%" h="var(--space-10)" radius="md" />
        {Array.from({ length: 9 }, (_, i) => <Skeleton key={i} w={`${55 + ((i * 13) % 35)}%`} h="var(--space-3)" style={{ marginTop: 'var(--space-2)' }} />)}
      </aside>
      <div>
        <div className="boot-shell__top">
          <Skeleton w="min(18rem, 40%)" h="var(--control-h-sm)" radius="md" />
          <Skeleton w="var(--space-32)" h="var(--control-h-sm)" radius="md" style={{ marginLeft: 'auto' }} />
        </div>
        <div className="boot-shell__main"><SkeletonPage /></div>
      </div>
    </div>,
  );
}

function renderStatus(title: string, detail?: string, retry?: () => void) {
  root.render(
    <div className="boot">
      <div className="boot__card">
        <div className="boot__title">{title}</div>
        {detail && <div className="boot__detail">{detail}</div>}
        {retry && <button className="btn btn--primary btn--sm" onClick={retry}>Retry</button>}
      </div>
    </div>,
  );
}

/** A session that expires while the app is open (any 401 in session mode) swaps the app for the login screen. */
function SessionGate() {
  const { expired, config } = useAuth();
  if (expired && config) return <Login config={config} expired next={window.location.pathname + window.location.search} onSignedIn={() => window.location.reload()} />;
  return <RouterProvider router={router} />;
}

async function boot() {
  renderLoading();
  // ADR-016 / ADR-028: /auth/config + /me decide between the login screen, onboarding (no org yet) and the app
  let step;
  try {
    step = await initAuth();
  } catch (e) {
    return renderStatus('Could not reach the TaskTrooper API', `${API_URL} — ${e instanceof Error ? e.message : String(e)}`, () => void boot());
  }
  hint(step !== 'login');
  if (step === 'login') {
    // signed out: landing, docs and the sign-in page (publicRouter.tsx); any other path goes to /login?next=
    if (authState().config) {
      return root.render(
        <StrictMode>
          <AppProvider>
            <RouterProvider router={createPublicRouter()} />
          </AppProvider>
        </StrictMode>,
      );
    }
  }
  if (step === 'onboarding') return root.render(<StrictMode><Onboarding onDone={() => void start()} /></StrictMode>);
  await start();
}

async function start() {
  try {
    await hydrate();
    startPolling();
    renderApp();
  } catch (e) {
    renderStatus('Could not reach the TaskTrooper API', `${API_URL} — ${e instanceof Error ? e.message : String(e)}`, () => void boot());
  }
}

// ADR-027: RUM of the dashboard itself — a no-op (and no SDK chunk loaded) without VITE_TROOPER_DSN
void startTelemetry(router).finally(() => void boot());
