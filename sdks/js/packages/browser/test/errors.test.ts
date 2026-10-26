import { afterEach, describe, expect, it } from 'vitest';
import { captureError, track, identify } from '../src/index';
import { setup, teardown } from './helpers';

afterEach(teardown);

describe('errors', () => {
  it('captures window errors and unhandled rejections as crashes', async () => {
    const { events } = setup();
    const err = new TypeError('x is undefined');
    window.dispatchEvent(new ErrorEvent('error', { error: err, message: err.message }));
    const ev = new Event('unhandledrejection') as Event & { reason: unknown };
    Object.defineProperty(ev, 'reason', { value: new Error('rejected') });
    window.dispatchEvent(ev);
    window.dispatchEvent(new Event('error')); // resource error: ignored
    const errs = (await events()).filter((e) => e.type === 'ERROR');
    expect(errs.map((e) => e.error)).toEqual([
      expect.objectContaining({ type: 'TypeError', message: 'x is undefined', source: 'source', is_crash: true }),
      expect.objectContaining({ type: 'Error', message: 'rejected', source: 'promise', is_crash: true }),
    ]);
    expect(errs[0]!.error!.stack).toContain('TypeError');
  });

  it('captureError is handled, deduplicated and accepts non-errors', async () => {
    const { events } = setup();
    const e = new Error('once');
    captureError(e, { attributes: { component: 'Cart' } });
    captureError(e);
    captureError('plain string');
    const errs = (await events()).filter((x) => x.type === 'ERROR');
    expect(errs).toHaveLength(2);
    expect(errs[0]).toMatchObject({ error: { message: 'once', is_crash: false }, attributes: { component: 'Cart' } });
    expect(errs[1]!.error!.message).toBe('plain string');
  });

  it('console.error is captured only when opted in', async () => {
    const orig = console.error;
    console.error = () => undefined;
    try {
      let s = setup();
      console.error('nope');
      expect((await s.events()).some((e) => e.type === 'ERROR')).toBe(false);
      await teardown();
      s = setup({ captureConsoleErrors: true });
      console.error('failed to load', { id: 1 });
      const errs = (await s.events()).filter((e) => e.type === 'ERROR');
      expect(errs[0]!.error).toMatchObject({ message: 'failed to load {"id":1}', source: 'console', is_crash: false });
    } finally {
      console.error = orig;
    }
  });

  it('public API never throws, even before init or with a throwing beforeSend', async () => {
    expect(() => track('x')).not.toThrow();
    const { events } = setup({
      beforeSend: () => {
        throw new Error('hook bug');
      },
    });
    expect(() => {
      identify('u1', { plan: 'pro' });
      track('checkout', { value: 1 });
    }).not.toThrow();
    expect(await events()).toEqual([]);
  });

  it('track + identify produce CUSTOM with user', async () => {
    const { events } = setup();
    identify('u1', { plan: 'pro' });
    track('checkout_started', { cart_value: 120 }, 120);
    const ev = (await events()).find((e) => e.type === 'CUSTOM')!;
    expect(ev).toMatchObject({ user: { id: 'u1', traits: { plan: 'pro' } }, custom: { name: 'checkout_started', props: { cart_value: '120' }, value: 120 } });
  });
});
