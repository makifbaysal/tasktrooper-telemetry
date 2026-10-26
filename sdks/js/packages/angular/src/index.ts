import {
  DestroyRef,
  ENVIRONMENT_INITIALIZER,
  ErrorHandler,
  inject,
  InjectionToken,
  makeEnvironmentProviders,
  type EnvironmentProviders,
} from '@angular/core';
import { NavigationEnd, Router } from '@angular/router';
import { captureError, init, setView, type BrowserOptions } from '@trooper/browser';

export interface TrooperAngularOptions extends BrowserOptions {
  /** Name views with the Angular route config path template. Default true. */
  trackRouter?: boolean;
  /** Keep Angular's default `console.error('ERROR', err)` logging. Default true. */
  logErrors?: boolean;
}

export const TROOPER_OPTIONS = new InjectionToken<TrooperAngularOptions>('TROOPER_OPTIONS');

const unwrap = (error: unknown): unknown => {
  let e = error as { rejection?: unknown; ngOriginalError?: unknown } | undefined;
  // Zone.js wraps unhandled promise rejections; Angular may wrap with ngOriginalError.
  for (let i = 0; i < 3 && e && typeof e === 'object'; i++) {
    const next = e.ngOriginalError ?? (e.rejection instanceof Error ? e.rejection : undefined);
    if (!next) break;
    e = next as typeof e;
  }
  return e;
};

/**
 * `ErrorHandler` that reports to TaskTrooper and then logs like Angular's default handler.
 * Plain class (no decorators) — provided through a factory, so no Angular compiler is needed for this package.
 */
export class TrooperErrorHandler implements ErrorHandler {
  constructor(private readonly options: Pick<TrooperAngularOptions, 'logErrors'> = {}) {}

  handleError(error: unknown): void {
    try {
      captureError(unwrap(error), { source: 'source', isCrash: true, attributes: { framework: 'angular' } });
    } catch {
      /* never break the host */
    }
    if (this.options.logErrors !== false) console.error('ERROR', error);
  }
}

interface SnapshotLike {
  routeConfig?: { path?: string } | null;
  firstChild?: SnapshotLike | null;
}

/** Route template from the activated route snapshot tree: [{path:'users'},{path:':id'}] → "/users/:id". */
export function angularRouteTemplate(root: SnapshotLike | null | undefined): string {
  const parts: string[] = [];
  for (let s = root; s; s = s.firstChild) {
    const p = s.routeConfig?.path;
    if (p) parts.push(p);
  }
  return ('/' + parts.join('/')).replace(/\/{2,}/g, '/');
}

interface RouterLike {
  events: { subscribe(fn: (e: unknown) => void): { unsubscribe(): void } };
  routerState: { snapshot: { root: SnapshotLike } };
}

/** Report the route template after every `NavigationEnd`. Returns an unsubscribe function. */
export function trackAngularRouter(router: RouterLike): () => void {
  let last = '';
  const sub = router.events.subscribe((e) => {
    if (!(e instanceof NavigationEnd)) return;
    try {
      const name = angularRouteTemplate(router.routerState.snapshot.root);
      const path = e.urlAfterRedirects.split(/[?#]/)[0] || '/';
      if (`${path}|${name}` === last) return;
      last = `${path}|${name}`;
      setView(name, { router: true, path });
    } catch {
      /* never break the host */
    }
  });
  return () => sub.unsubscribe();
}

/**
 * Standalone apps: `bootstrapApplication(App, { providers: [provideRouter(routes), provideTrooper({ dsn, app })] })`.
 * Registers the ErrorHandler, initializes the browser SDK and router view tracking.
 */
export function provideTrooper(options: TrooperAngularOptions): EnvironmentProviders {
  return makeEnvironmentProviders([
    { provide: TROOPER_OPTIONS, useValue: options },
    { provide: ErrorHandler, useFactory: () => new TrooperErrorHandler(options) },
    {
      provide: ENVIRONMENT_INITIALIZER,
      multi: true,
      useValue: () => {
        try {
          init(options);
          if (options.trackRouter === false) return;
          const router = inject(Router, { optional: true });
          if (!router) return;
          const stop = trackAngularRouter(router);
          inject(DestroyRef, { optional: true })?.onDestroy(stop);
        } catch {
          /* never break the host */
        }
      },
    },
  ]);
}

export { captureError, clearUser, flush, identify, init, setView, track } from '@trooper/browser';
export type { BrowserOptions } from '@trooper/browser';
