import { Navigate, createBrowserRouter, useLocation, useSearchParams } from 'react-router';
import { Landing } from './screens/Landing';
import { Docs } from './screens/Docs';
import { Login } from './screens/Login';
import { authState } from './data/auth';

/** Only same-origin app paths are allowed as a post-sign-in target. */
export function safeNext(raw: string | null): string {
  return raw && raw.startsWith('/') && !raw.startsWith('//') && !raw.startsWith('/login') ? raw : '/';
}

function LoginRoute() {
  const [params] = useSearchParams();
  const next = safeNext(params.get('next'));
  const { config } = authState();
  if (!config) return null;
  return <Login config={config} next={next} onSignedIn={() => window.location.assign(next)} />;
}

/** Any other path while signed out (a shared dashboard link): sign in first, then land there. */
function ToLogin() {
  const { pathname, search, hash } = useLocation();
  return <Navigate to={`/login?next=${encodeURIComponent(pathname + search + hash)}`} replace />;
}

/**
 * Router for signed-out visitors: landing, docs and sign-in. Signed-in users get the app router (router.tsx),
 * which also serves /docs. Signing in reloads at `next`, so boot starts the app router on the right path.
 */
export function createPublicRouter() {
  return createBrowserRouter([
    { path: '/', element: <Landing /> },
    { path: '/docs', element: <Docs /> },
    { path: '/login', element: <LoginRoute /> },
    { path: '*', element: <ToLogin /> },
  ]);
}
