import { useEffect } from 'react';
import { useParams, usePathname } from 'next/navigation';
import { useRouter } from 'next/router';
import { setView } from '@trooper/browser';

type Params = Record<string, string | string[] | undefined>;

/**
 * App Router: rebuild the route template from the concrete pathname and the dynamic params.
 * "/shop/shoes/42" + { category: "shoes", id: "42" } → "/shop/:category/:id"; catch-all arrays → ":slug*".
 */
export function appRouterTemplate(pathname: string, params: Params | null | undefined): string {
  const segs = pathname.split('/');
  if (!params) return pathname;
  const entries = Object.entries(params).filter(([, v]) => v !== undefined) as [string, string | string[]][];
  const used = new Set<string>();
  const out: string[] = [];
  for (let i = 0; i < segs.length; i++) {
    const seg = safeDecode(segs[i]!);
    let replaced = false;
    for (const [k, v] of entries) {
      if (used.has(k)) continue;
      if (Array.isArray(v)) {
        if (v.length && v.every((x, j) => safeDecode(segs[i + j] ?? '') === x)) {
          out.push(`:${k}*`);
          i += v.length - 1;
          used.add(k);
          replaced = true;
          break;
        }
      } else if (seg && seg === v) {
        out.push(`:${k}`);
        used.add(k);
        replaced = true;
        break;
      }
    }
    if (!replaced) out.push(segs[i]!);
  }
  return out.join('/') || '/';
}

const safeDecode = (s: string): string => {
  try {
    return decodeURIComponent(s);
  } catch {
    return s;
  }
};

/** Pages Router: "/products/[id]" → "/products/:id", "[...slug]" / "[[...slug]]" → ":slug*". */
export function pagesRouterTemplate(pathname: string): string {
  return pathname.replace(/\[\[\.\.\.([^\]]+)\]\]/g, ':$1*').replace(/\[\.\.\.([^\]]+)\]/g, ':$1*').replace(/\[([^\]]+)\]/g, ':$1');
}

/** Next.js App Router: call in a client component inside the root layout (or render `<TrooperNextAppRouter />`). */
export function useNextAppRouterTracking(): void {
  const pathname = usePathname();
  const params = useParams() as Params | null;
  const key = JSON.stringify(params ?? {});
  useEffect(() => {
    if (!pathname) return;
    try {
      setView(appRouterTemplate(pathname, params), { router: true, path: pathname });
    } catch {
      /* never break the host */
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [pathname, key]);
}

export function TrooperNextAppRouter(): null {
  useNextAppRouterTracking();
  return null;
}

/** Next.js Pages Router: call in `_app.tsx`. Uses `router.pathname` (already a template). */
export function useNextPagesRouterTracking(): void {
  const router = useRouter();
  const pathname = router?.pathname;
  const asPath = router?.asPath?.split(/[?#]/)[0];
  useEffect(() => {
    if (!pathname || router?.isReady === false) return;
    try {
      setView(pagesRouterTemplate(pathname), { router: true, path: asPath });
    } catch {
      /* never break the host */
    }
  }, [pathname, asPath, router?.isReady]);
}
