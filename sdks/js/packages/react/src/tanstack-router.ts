import { setView } from '@trooper/browser';

interface TanStackMatchLike {
  fullPath?: string;
  routeId?: string;
}

interface TanStackRouterLike {
  state: { location: { pathname: string }; matches: readonly TanStackMatchLike[] };
  routesById?: Record<string, { fullPath?: string }>;
  subscribe(event: 'onResolved', fn: () => void): () => void;
}

/** TanStack path → template: "/posts/$postId" → "/posts/:postId", "/files/$" → "/files/*", "{-$lang}" → ":lang?". */
export function tanstackTemplate(fullPath: string | undefined): string | undefined {
  if (fullPath == null) return undefined;
  let p = fullPath
    .replace(/\{-\$([A-Za-z0-9_]+)\}/g, ':$1?')
    .replace(/\{\$([A-Za-z0-9_]+)\}/g, ':$1')
    .replace(/\$([A-Za-z0-9_]+)/g, ':$1')
    .replace(/\/\$(?=\/|$)/g, '/*');
  if (p === '$') p = '/*';
  if (p.length > 1) p = p.replace(/\/+$/, '');
  return p || '/';
}

/** TanStack Router: report the deepest match's full path template after every resolved navigation. */
export function trackTanStackRouter(router: TanStackRouterLike): () => void {
  let last = '';
  const report = () => {
    try {
      const matches = router.state.matches;
      const leaf = matches[matches.length - 1];
      if (!leaf) return;
      const full = leaf.fullPath ?? (leaf.routeId ? router.routesById?.[leaf.routeId]?.fullPath : undefined);
      const name = tanstackTemplate(full);
      const path = router.state.location.pathname;
      if (!name || `${path}|${name}` === last) return;
      last = `${path}|${name}`;
      setView(name, { router: true, path });
    } catch {
      /* never break the host */
    }
  };
  report();
  try {
    return router.subscribe('onResolved', report);
  } catch {
    return () => undefined;
  }
}
