import { afterEach, describe, expect, it, vi } from 'vitest';
import type { Metric } from 'web-vitals';
import { setup, teardown } from './helpers';

const cbs = vi.hoisted(() => ({} as Record<string, (m: Metric) => void>));
vi.mock('web-vitals', () => {
  const on = (name: string) => (cb: (m: Metric) => void) => { cbs[name] = cb; };
  return { onLCP: on('LCP'), onINP: on('INP'), onCLS: on('CLS'), onFCP: on('FCP'), onTTFB: on('TTFB') };
});

const fire = (name: string, value: number) => cbs[name]!({ name, value, rating: 'good' } as Metric);

afterEach(teardown);

describe('web vitals', () => {
  it('attributes LCP / FCP / TTFB to the landing view and INP / CLS to the current view', async () => {
    history.replaceState(null, '', '/landing');
    const { events } = setup({ trackVitals: true });
    history.pushState(null, '', '/settings');
    for (const [n, v] of [['LCP', 1200], ['FCP', 800], ['TTFB', 150], ['INP', 240], ['CLS', 0.123456]] as const) fire(n, v);
    const evs = await events();
    const landing = evs.find((e) => e.type === 'VIEW_START' && e.view_name === '/landing')!;
    const current = evs.find((e) => e.type === 'VIEW_START' && e.view_name === '/settings')!;
    const vital = (n: string) => evs.find((e) => e.type === 'VITAL' && e.vital?.name === n)!;
    for (const n of ['LCP', 'FCP', 'TTFB']) expect(vital(n)).toMatchObject({ view_id: landing.view_id, view_name: '/landing' });
    for (const n of ['INP', 'CLS']) expect(vital(n)).toMatchObject({ view_id: current.view_id, view_name: '/settings' });
    expect(vital('CLS').vital!.value).toBe(0.1235);
    expect(vital('INP').vital!.value).toBe(240);
  });
});
