import { createApp, defineComponent, h, nextTick } from 'vue';
import { createMemoryHistory, createRouter, RouterView } from 'vue-router';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { close, flush } from '@trooper/browser';
import type { RumBatch, RumEvent } from '@trooper/core';
import { Trooper, useTrooper, vueRouteTemplate } from '../src/index';

const OPTIONS = { dsn: 'http://tt_dev_ingest_key@localhost:4318/1', app: 'vue-app', persist: false, trackVitals: false, trackActions: false };

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

afterEach(async () => {
  await close();
  document.body.innerHTML = '';
});

const Page = defineComponent({ render: () => h('p', 'page') });

describe('vue-router integration', () => {
  it('vueRouteTemplate strips custom regexes', () => {
    const r = (path: string) => ({ path: '/x', matched: [{ path: '/' }, { path }] });
    expect(vueRouteTemplate(r('/users/:id(\\d+)'))).toBe('/users/:id');
    expect(vueRouteTemplate(r('/:pathMatch(.*)*'))).toBe('/:pathMatch*');
    expect(vueRouteTemplate(r('/posts/:slug?'))).toBe('/posts/:slug?');
  });

  it('names views with matched route templates', async () => {
    const events = stubIngest();
    const router = createRouter({
      history: createMemoryHistory(),
      routes: [
        { path: '/', component: Page },
        { path: '/users/:id(\\d+)', component: Page, children: [{ path: 'orders/:orderId', component: Page }] },
      ],
    });
    const app = createApp({ render: () => h(RouterView) });
    app.use(router);
    app.use(Trooper, { ...OPTIONS, router });
    await router.push('/');
    app.mount(document.createElement('div'));
    await router.push('/users/42');
    await router.push('/users/42/orders/7');
    const names = (await events()).filter((e) => e.type === 'VIEW_START').map((e) => e.view_name);
    expect(names).toEqual(['/', '/users/:id', '/users/:id/orders/:orderId']);
  });
});

describe('errorHandler', () => {
  it('captures component errors, keeps the previous handler', async () => {
    const events = stubIngest();
    const prev = vi.fn();
    const Broken = defineComponent({
      name: 'BrokenWidget',
      setup() {
        throw new RangeError('bad setup');
      },
      render: () => null,
    });
    const app = createApp({ render: () => h(Broken) });
    app.config.errorHandler = prev;
    app.use(Trooper, OPTIONS);
    app.mount(document.createElement('div'));
    await nextTick();
    const err = (await events()).find((e) => e.type === 'ERROR')!;
    expect(err.error).toMatchObject({ type: 'RangeError', message: 'bad setup', source: 'source' });
    expect(err.attributes).toMatchObject({ component: 'BrokenWidget', vue_info: 'setup function' });
    expect(prev).toHaveBeenCalledOnce();
  });

  it('useTrooper() is provided', async () => {
    const events = stubIngest();
    const C = defineComponent({
      setup() {
        useTrooper().track('mounted');
        return () => null;
      },
    });
    const app = createApp(C);
    app.use(Trooper, OPTIONS);
    app.mount(document.createElement('div'));
    expect((await events()).some((e) => e.custom?.name === 'mounted')).toBe(true);
  });
});
