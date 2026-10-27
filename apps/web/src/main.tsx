import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { RouterProvider } from 'react-router';
import './styles/tokens.css';
import './styles/base.css';
import './styles/components.css';
import './styles/charts.css';
import './styles/account.css';
import { router } from './router';
import { AppProvider } from './components/layout/context';
import { API_URL, hydrate, isApi, startPolling } from './data/remote';
import { authState, initAuth, useAuth } from './data/auth';
import { Login } from './screens/Login';

const root = createRoot(document.getElementById('root')!);

function renderApp() {
  root.render(
    <StrictMode>
      <AppProvider>
        <SessionGate />
      </AppProvider>
    </StrictMode>,
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
  if (expired && config) return <Login config={config} expired onSignedIn={() => window.location.reload()} />;
  return <RouterProvider router={router} />;
}

async function boot() {
  if (!isApi) return renderApp();
  renderStatus('Loading…');
  // ADR-016: /auth/config + /me decide between session cookie, api key and the login screen
  if ((await initAuth()) === 'login') {
    const { config } = authState();
    if (config) return root.render(<StrictMode><Login config={config} onSignedIn={() => void boot()} /></StrictMode>);
  }
  try {
    await hydrate();
    startPolling();
    renderApp();
  } catch (e) {
    renderStatus('Could not reach the TaskTrooper API', `${API_URL} — ${e instanceof Error ? e.message : String(e)}`, () => void boot());
  }
}

void boot();
