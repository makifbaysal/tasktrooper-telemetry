import { StrictMode } from 'react';
import { createRoot } from 'react-dom/client';
import { RouterProvider } from 'react-router';
import './styles/tokens.css';
import './styles/base.css';
import './styles/components.css';
import './styles/charts.css';
import { router } from './router';
import { AppProvider } from './components/layout/context';
import { API_URL, hydrate, isApi, startPolling } from './data/remote';

const root = createRoot(document.getElementById('root')!);

function renderApp() {
  root.render(
    <StrictMode>
      <AppProvider>
        <RouterProvider router={router} />
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

async function boot() {
  if (!isApi) return renderApp();
  renderStatus('Loading…');
  try {
    await hydrate();
    startPolling();
    renderApp();
  } catch (e) {
    renderStatus('Could not reach the TaskTrooper API', `${API_URL} — ${e instanceof Error ? e.message : String(e)}`, () => void boot());
  }
}

void boot();
