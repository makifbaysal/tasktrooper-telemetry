import '@angular/compiler'; // JIT facade for partially compiled Angular packages under vitest
import { createEnvironmentInjector, ErrorHandler, Injector, type EnvironmentInjector } from '@angular/core';
import { NavigationEnd, NavigationStart, Router } from '@angular/router';
import { Subject } from 'rxjs';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { close, flush } from '@trooper/browser';
import type { RumBatch, RumEvent } from '@trooper/core';
import { angularRouteTemplate, provideTrooper, TrooperErrorHandler } from '../src/index';

const OPTIONS = { dsn: 'http://tt_dev_ingest_key@localhost:4318/1', app: 'ng-app', persist: false, trackVitals: false, trackActions: false };

function stubIngest() {
  const batches: RumBatch[] = [];
  globalThis.fetch = (async (_u: unknown, init?: RequestInit) => {
    batches.push(JSON.parse(String(init?.body)));
    return new Response('{}');
  }) as typeof fetch;
  return async (): Promise<RumEvent[]> => {
    await new Promise((r) => setTimeout(r, 300));
    await flush();
    return batches.flatMap((b) => b.events);
  };
}

afterEach(() => close());

interface Snap {
  routeConfig: { path: string } | null;
  firstChild: Snap | null;
}
const snap = (...paths: string[]): Snap => {
  let node: Snap | null = null;
  for (const p of paths.reverse()) node = { routeConfig: { path: p }, firstChild: node };
  return { routeConfig: null, firstChild: node };
};

function makeInjector(router: unknown, logErrors = false) {
  return createEnvironmentInjector(
    [provideTrooper({ ...OPTIONS, logErrors }), { provide: Router, useValue: router }],
    Injector.NULL as unknown as EnvironmentInjector,
  );
}

describe('angular', () => {
  it('route template from snapshot tree', () => {
    expect(angularRouteTemplate(snap('users', ':id', 'orders/:orderId'))).toBe('/users/:id/orders/:orderId');
    expect(angularRouteTemplate(snap('', 'dashboard'))).toBe('/dashboard');
    expect(angularRouteTemplate(snap('**'))).toBe('/**');
  });

  it('provideTrooper: ErrorHandler provider captures errors (zone rejection unwrapped)', async () => {
    const events = stubIngest();
    const inj = makeInjector({ events: new Subject(), routerState: { snapshot: { root: snap() } } });
    const handler = inj.get(ErrorHandler);
    expect(handler).toBeInstanceOf(TrooperErrorHandler);
    handler.handleError(new TypeError('template failed'));
    handler.handleError({ rejection: new Error('async failed'), message: 'Uncaught (in promise)' });
    const errs = (await events()).filter((e) => e.type === 'ERROR');
    expect(errs.map((e) => e.error!.message)).toEqual(['template failed', 'async failed']);
    expect(errs[0]!.error).toMatchObject({ type: 'TypeError', is_crash: true });
  });

  it('logs like the default handler unless disabled', () => {
    stubIngest();
    const spy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    new TrooperErrorHandler().handleError(new Error('x'));
    expect(spy).toHaveBeenCalledWith('ERROR', expect.any(Error));
    spy.mockRestore();
  });

  it('router integration names views on NavigationEnd', async () => {
    const events = stubIngest();
    const routerEvents = new Subject<unknown>();
    const router = { events: routerEvents, routerState: { snapshot: { root: snap('') } } };
    const inj = makeInjector(router);
    routerEvents.next(new NavigationEnd(1, '/', '/'));
    routerEvents.next(new NavigationStart(2, '/users/5?tab=1'));
    router.routerState.snapshot.root = snap('users', ':id');
    routerEvents.next(new NavigationEnd(2, '/users/5?tab=1', '/users/5?tab=1'));
    const names = (await events()).filter((e) => e.type === 'VIEW_START').map((e) => e.view_name);
    expect(names).toEqual(['/', '/users/:id']);
    inj.destroy();
  });
});
