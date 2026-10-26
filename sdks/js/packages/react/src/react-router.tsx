import { useEffect } from 'react';
import { matchRoutes, useLocation, type RouteObject } from 'react-router';
import { setView } from '@trooper/browser';

interface MatchLike {
  route: { path?: string };
}

/** Join matched route paths into a template: [{path:'/users'},{path:':id'}] → "/users/:id". */
export function routeTemplate(matches: readonly MatchLike[] | null | undefined): string | undefined {
  if (!matches?.length) return undefined;
  let out = '';
  for (const m of matches) {
    const p = m.route?.path;
    if (!p) continue; // index / layout routes
    out = p.startsWith('/') ? p : `${out.replace(/\/+$/, '')}/${p}`;
  }
  out = out.replace(/\/{2,}/g, '/');
  if (out.length > 1) out = out.replace(/\/+$/, '');
  return out || '/';
}

interface DataRouterLike {
  state: { location: { pathname: string }; matches: readonly MatchLike[] };
  subscribe(fn: (state: DataRouterLike['state']) => void): () => void;
}

/**
 * Data routers (`createBrowserRouter`, `createMemoryRouter`, v6.4+ / v7): report the route template on every navigation.
 * Call once right after creating the router. Returns an unsubscribe function.
 */
export function trackReactRouter(router: DataRouterLike): () => void {
  let last = '';
  const report = (state: DataRouterLike['state']) => {
    const name = routeTemplate(state.matches);
    const key = `${state.location.pathname}|${name}`;
    if (!name || key === last) return;
    last = key;
    setView(name, { router: true, path: state.location.pathname });
  };
  try {
    report(router.state);
    return router.subscribe(report);
  } catch {
    return () => undefined;
  }
}

/**
 * Declarative routers (`<BrowserRouter>` + `<Routes>`): pass the same route config to compute templates.
 * With JSX routes use `createRoutesFromElements(...)` once and pass the result.
 */
export function useTrooperRoutes(routes: RouteObject[]): void {
  const location = useLocation();
  useEffect(() => {
    try {
      const name = routeTemplate(matchRoutes(routes, location));
      if (name) setView(name, { router: true, path: location.pathname });
    } catch {
      /* never break the host */
    }
  }, [routes, location.pathname]);
}

/** Component form of `useTrooperRoutes` — render it inside the router. */
export function TrooperRoutes({ routes }: { routes: RouteObject[] }): null {
  useTrooperRoutes(routes);
  return null;
}
