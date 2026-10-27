import { setRouteResolver } from './api';
import { recordError } from './errors';

/* Framework helpers capture the route template of the current request. They are structurally typed so the package
 * does not depend on any framework; each one only installs a lazy resolver and never throws into the request. */

type Next = (err?: unknown) => void;

interface ExpressLikeRequest {
  baseUrl?: string;
  route?: { path?: unknown };
}

/** Template of an express (4/5) request: `req.baseUrl + req.route.path` once the router matched. */
export function expressRoute(req: ExpressLikeRequest | undefined): string | undefined {
  const p = req?.route?.path;
  if (typeof p !== 'string') return undefined;
  const base = typeof req?.baseUrl === 'string' ? req.baseUrl : '';
  const full = (base + p).replace(/\/{2,}/g, '/');
  return full || '/';
}

interface FastifyLikeRequest {
  routeOptions?: { url?: unknown };
  routerPath?: unknown;
}

/** Template of a fastify request (`request.routeOptions.url`, older `request.routerPath`). */
export function fastifyRoute(req: FastifyLikeRequest | undefined): string | undefined {
  const u = req?.routeOptions?.url ?? req?.routerPath;
  return typeof u === 'string' && u ? u : undefined;
}

interface KoaLikeContext {
  _matchedRoute?: unknown;
  routerPath?: unknown;
}

/** Template of a koa request (`@koa/router` sets `ctx._matchedRoute`). */
export function koaRoute(ctx: KoaLikeContext | undefined): string | undefined {
  const r = ctx?._matchedRoute ?? ctx?.routerPath;
  return typeof r === 'string' && r ? r : undefined;
}

interface HonoLikeContext {
  req?: { routePath?: unknown; matchedRoutes?: { path?: unknown; method?: unknown }[] };
}

/** Template of a Hono request: the last matched non-middleware route, else `c.req.routePath`. */
export function honoRoute(c: HonoLikeContext | undefined): string | undefined {
  try {
    const routes = c?.req?.matchedRoutes;
    if (Array.isArray(routes)) {
      for (let i = routes.length - 1; i >= 0; i--) {
        const r = routes[i];
        if (r && r.method !== 'ALL' && typeof r.path === 'string' && r.path !== '/*' && r.path !== '*') return r.path;
      }
    }
    const p = c?.req?.routePath;
    return typeof p === 'string' && p && p !== '/*' && p !== '*' ? p : undefined;
  } catch (err) {
    recordError('hono', err);
    return undefined;
  }
}

/** Template from any supported request object (express, fastify, NestJS on either adapter). */
export function templateFromRequest(req: unknown): string | undefined {
  if (!req || typeof req !== 'object') return undefined;
  return fastifyRoute(req as FastifyLikeRequest) ?? expressRoute(req as ExpressLikeRequest);
}

/**
 * express: `app.use(trooperExpress())` (first middleware). Captures `req.baseUrl + req.route.path` for
 * `trooper.route` and `trooper.entry.route` (also for routers mounted with `app.use('/v1', router)`).
 */
export function trooperExpress(): (req: ExpressLikeRequest, res: unknown, next: Next) => void {
  return (req, _res, next) => {
    try {
      setRouteResolver(() => expressRoute(req));
    } catch (err) {
      recordError('express', err);
    }
    next();
  };
}

interface FastifyLike {
  addHook(name: 'onRequest', fn: (req: FastifyLikeRequest, reply: unknown, done: Next) => void): unknown;
}

/**
 * fastify plugin: `await app.register(trooperFastify)`. Uses the matched route (`request.routeOptions.url`).
 * Not encapsulated (applies to all routes, like `fastify-plugin`).
 */
export const trooperFastify = Object.assign(
  function trooperFastify(app: FastifyLike, _opts: unknown, done?: Next): void {
    try {
      app.addHook('onRequest', (req, _reply, next) => {
        try {
          setRouteResolver(() => fastifyRoute(req));
        } catch (err) {
          recordError('fastify', err);
        }
        next();
      });
    } catch (err) {
      recordError('fastify', err);
    }
    done?.();
  },
  { [Symbol.for('skip-override')]: true, [Symbol.for('fastify.display-name')]: 'trooper' },
);

/** koa: `app.use(trooperKoa())` before the router. Reads `ctx._matchedRoute` (`@koa/router`). */
export function trooperKoa(): (ctx: KoaLikeContext, next: () => Promise<unknown>) => Promise<unknown> {
  return async (ctx, next) => {
    try {
      setRouteResolver(() => koaRoute(ctx));
    } catch (err) {
      recordError('koa', err);
    }
    return next();
  };
}

/** Hono (Node adapter): `app.use(trooperHono())`. */
export function trooperHono(): (c: HonoLikeContext, next: () => Promise<void>) => Promise<void> {
  return async (c, next) => {
    try {
      setRouteResolver(() => honoRoute(c));
    } catch (err) {
      recordError('hono', err);
    }
    await next();
  };
}

interface NestExecutionContext {
  getType?: () => string;
  switchToHttp?: () => { getRequest?: () => unknown };
}

/**
 * NestJS global interceptor (express or fastify adapter): `app.useGlobalInterceptors(new TrooperInterceptor())`.
 * The template is the matched route of the underlying adapter (global prefix and versioning included).
 */
export class TrooperInterceptor {
  intercept<T>(context: NestExecutionContext, next: { handle: () => T }): T {
    try {
      if (!context.getType || context.getType() === 'http') {
        const req = context.switchToHttp?.().getRequest?.();
        if (req) setRouteResolver(() => templateFromRequest(req));
      }
    } catch (err) {
      recordError('nestjs', err);
    }
    return next.handle();
  }
}
