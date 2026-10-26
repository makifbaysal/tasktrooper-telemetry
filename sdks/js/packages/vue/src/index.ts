import type { App, ComponentPublicInstance, InjectionKey, Plugin } from 'vue';
import { inject } from 'vue';
import { captureError, clearUser, flush, identify, init, setView, track, type BrowserOptions } from '@trooper/browser';

interface RouteRecordLike {
  path: string;
}
interface RouteLike {
  path: string;
  matched: readonly RouteRecordLike[];
}
/** Structural subset of vue-router's Router (v4+), so vue-router stays an optional peer. */
export interface RouterLike {
  currentRoute: { value: RouteLike };
  afterEach(guard: (to: RouteLike, from: RouteLike, failure?: unknown) => void): () => void;
}

export interface TrooperVueOptions extends Partial<BrowserOptions> {
  /** vue-router instance: views are named with the matched route's path template. */
  router?: RouterLike;
  /** Keep calling a previously set `app.config.errorHandler` and log to console like Vue does. Default true. */
  logErrors?: boolean;
}

export interface TrooperApi {
  track: typeof track;
  identify: typeof identify;
  clearUser: typeof clearUser;
  captureError: typeof captureError;
  setView: typeof setView;
  flush: typeof flush;
}

const api: TrooperApi = { track, identify, clearUser, captureError, setView, flush };
export const TrooperKey: InjectionKey<TrooperApi> = Symbol('trooper');

/**
 * Route template from vue-router's matched records: "/users/:id(\\d+)" → "/users/:id",
 * "/:pathMatch(.*)*" → "/:pathMatch*". Records carry absolute paths, so the leaf is enough.
 */
export function vueRouteTemplate(route: RouteLike | undefined): string | undefined {
  const leaf = route?.matched?.[route.matched.length - 1];
  if (!leaf) return undefined;
  const p = leaf.path.replace(/(:\w+)\((?:[^()]|\([^()]*\))*\)/g, '$1');
  return p || '/';
}

/** Report the current route template on every navigation. Returns the unregister function. */
export function trackVueRouter(router: RouterLike): () => void {
  let last = '';
  const report = (to: RouteLike) => {
    try {
      const name = vueRouteTemplate(to);
      if (!name || `${to.path}|${name}` === last) return;
      last = `${to.path}|${name}`;
      setView(name, { router: true, path: to.path });
    } catch {
      /* never break the host */
    }
  };
  if (router.currentRoute?.value?.matched?.length) report(router.currentRoute.value);
  return router.afterEach((to, _from, failure) => {
    if (!failure) report(to);
  });
}

const componentName = (vm: ComponentPublicInstance | null): string | undefined => {
  const o = vm?.$options as { name?: string; __name?: string } | undefined;
  return o?.name || o?.__name;
};

/** `app.use(Trooper, { dsn, app, router })` */
export const Trooper: Plugin<[TrooperVueOptions?]> = {
  install(app: App, options: TrooperVueOptions = {}) {
    const { router, logErrors = true, ...browserOptions } = options;
    if (browserOptions.dsn && browserOptions.app) init(browserOptions as BrowserOptions);

    const prev = app.config.errorHandler;
    app.config.errorHandler = (err, instance, info) => {
      captureError(err, {
        source: 'source',
        isCrash: false,
        attributes: { vue_info: info, component: componentName(instance) },
      });
      if (prev) prev(err, instance, info);
      else if (logErrors) console.error(err);
    };

    if (router) trackVueRouter(router);
    app.provide(TrooperKey, api);
  },
};

/** Composition API access to the SDK (works without the plugin too). */
export const useTrooper = (): TrooperApi => inject(TrooperKey, api);

export { captureError, clearUser, flush, identify, init, setView, track } from '@trooper/browser';
export type { BrowserOptions } from '@trooper/browser';
