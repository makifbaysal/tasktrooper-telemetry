import { afterEach, describe, expect, it } from 'vitest';
import type { RumEvent } from '@trooper/core';
import { setView } from '../src/index';
import { setup, teardown } from './helpers';

afterEach(teardown);

const viewEvents = (evs: RumEvent[]) => evs.filter((e) => e.type.startsWith('VIEW'));

describe('views', () => {
  it('records the initial view and SPA navigations via the History API', async () => {
    history.replaceState(null, '', '/products/123?ref=mail');
    const { events } = setup();
    history.pushState(null, '', '/cart');
    history.replaceState(null, '', '/cart?step=2'); // same path: no new view
    history.pushState(null, '', '/orders/550e8400-e29b-41d4-a716-446655440000');
    const evs = viewEvents(await events());
    expect(evs.map((e) => `${e.type} ${e.view_name}`)).toEqual([
      'VIEW_START /products/:id',
      'VIEW_END /products/:id',
      'VIEW_START /cart',
      'VIEW_END /cart',
      'VIEW_START /orders/:id',
    ]);
    expect(evs[0]!.view).toMatchObject({ is_initial: true });
    expect(evs[0]!.view!.url).toMatch(/\/products\/123\?ref=$/); // query value stripped
    expect(evs[1]!.view!.time_spent_ms).toBeGreaterThanOrEqual(0);
    expect(evs[1]!.view_id).toBe(evs[0]!.view_id);
    expect(evs[2]!.view_id).not.toBe(evs[0]!.view_id);
  });

  it('VIEW_START carries loading_time_ms (SPA settle) and the navigation timestamp', async () => {
    const { events } = setup();
    const t0 = Date.now();
    history.pushState(null, '', '/slow');
    setTimeout(() => document.body.append(document.createElement('main')), 60); // route renders after 60 ms
    const start = (await events()).filter((e) => e.type === 'VIEW_START').find((e) => e.view_name === '/slow')!;
    expect(start.view!.loading_time_ms).toBeGreaterThanOrEqual(50);
    expect(start.view!.loading_time_ms).toBeLessThan(250);
    expect(start.ts_ms).toBeLessThanOrEqual(t0 + 5);
  });

  it('popstate starts a new view', async () => {
    const { events } = setup();
    history.pushState(null, '', '/a');
    history.pushState(null, '', '/b');
    history.back();
    await new Promise((r) => setTimeout(r, 20));
    window.dispatchEvent(new PopStateEvent('popstate'));
    const names = viewEvents(await events()).filter((e) => e.type === 'VIEW_START').map((e) => e.view_name);
    expect(names.slice(0, 3)).toEqual(['/', '/a', '/b']);
    expect(names.length).toBeGreaterThanOrEqual(3);
  });

  it('setView renames the auto-named view including already buffered events', async () => {
    history.replaceState(null, '', '/blog/hello-world');
    const { events } = setup();
    setView('/blog/:slug');
    const evs = await events();
    const start = evs.find((e) => e.type === 'VIEW_START')!;
    expect(start.view_name).toBe('/blog/:slug');
  });

  it('router mode: integrations own view changes', async () => {
    const { events } = setup();
    setView('/', { router: true });
    history.pushState(null, '', '/users/7');
    setView('/users/:userId', { router: true });
    setView('/users/:userId', { router: true }); // duplicate call is a no-op
    const names = viewEvents(await events()).filter((e) => e.type === 'VIEW_START').map((e) => e.view_name);
    expect(names).toEqual(['/', '/users/:userId']);
  });

  it('manual setView with a new name starts a new view', async () => {
    const { events } = setup();
    setView('/');
    setView('Checkout step 2');
    const names = viewEvents(await events()).filter((e) => e.type === 'VIEW_START').map((e) => e.view_name);
    expect(names).toEqual(['/', 'Checkout step 2']);
  });
});
