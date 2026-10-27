import { afterEach, describe, expect, it, vi } from 'vitest';
import type { RumEvent } from '@trooper/core';
import { captureError, clearUser, close, identify, track, type Integration, type IntegrationClient } from '../src/index';
import { setup, teardown } from './helpers';

afterEach(teardown);

function probe(name = 'probe') {
  const state = { client: undefined as IntegrationClient | undefined, events: [] as RumEvent[], sessions: [] as string[], closed: 0, setups: 0 };
  const integration: Integration = {
    name,
    setup(c) {
      state.setups++;
      state.client = c;
      c.onEvent((e) => state.events.push(e));
      c.onSessionStart((id) => state.sessions.push(id));
      return () => state.closed++;
    },
  };
  return { integration, state };
}

describe('integrations', () => {
  it('exposes config, ids, view and transport helpers', () => {
    const { integration, state } = probe();
    const { sdk } = setup({ integrations: [integration] });
    identify('u1');
    const c = state.client!;
    expect(c.config).toEqual({
      dsn: 'http://tt_dev_ingest_key@localhost:4318/1',
      endpoint: 'http://localhost:4318',
      key: 'tt_dev_ingest_key',
      projectId: '1',
      service: 'shop-web',
      environment: 'test',
      version: '1.0.0',
      debug: false,
    });
    expect(c.sessionId()).toBe(sdk.client.sessionId());
    expect(c.sampled()).toBe(true);
    expect(c.anonymousId()).toBe(sdk.client.anonymousId);
    expect(c.userId()).toBe('u1');
    expect(c.view()).toEqual(sdk.client.view);
    expect(typeof c.fetch).toBe('function');
    expect(c.classifyStatus(503)).toEqual({ status: 'retry' });
    expect(c.classifyStatus(400)).toEqual({ status: 'drop', code: 400 });
    expect(c.backoffDelay(0)).toBeGreaterThanOrEqual(500);
  });

  it('forwards accepted events and session rotations; teardown on close', async () => {
    const { integration, state } = probe();
    const { sdk } = setup({ integrations: [integration] });
    captureError(new Error('boom'));
    expect(state.events.some((e) => e.type === 'ERROR')).toBe(true);
    const first = sdk.client.sessionId();
    clearUser();
    track('x');
    expect(state.sessions).toEqual([sdk.client.sessionId()]);
    expect(state.sessions[0]).not.toBe(first);
    await close();
    expect(state.closed).toBe(1);
  });

  it('a throwing integration does not break init; duplicates are ignored', () => {
    const bad: Integration = {
      name: 'bad',
      setup: () => {
        throw new Error('nope');
      },
    };
    const a = probe('dup');
    const b = probe('dup');
    const { sdk } = setup({ integrations: [bad, a.integration, b.integration] });
    expect(sdk).toBeDefined();
    expect(a.state.setups).toBe(1);
    expect(b.state.setups).toBe(0);
    expect(() => track('ok')).not.toThrow();
  });

  it('listener errors are swallowed', () => {
    const integration: Integration = {
      name: 'throwing-listener',
      setup(c) {
        c.onEvent(() => {
          throw new Error('listener');
        });
      },
    };
    setup({ integrations: [integration] });
    expect(() => track('x')).not.toThrow();
  });

  it('not set up when the SDK is disabled', () => {
    const setupFn = vi.fn();
    setup({ enabled: false, integrations: [{ name: 'x', setup: setupFn }] });
    expect(setupFn).not.toHaveBeenCalled();
  });
});
