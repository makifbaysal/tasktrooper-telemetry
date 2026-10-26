import { act, render, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { ErrorBoundary, TrooperProvider, reactErrorHandler, useTrooper } from '../src/index';
import { OPTIONS, reset, stubIngest } from './helpers';

afterEach(reset);

function Boom(): never {
  throw new TypeError('render failed');
}

describe('TrooperProvider + useTrooper', () => {
  it('initializes once and exposes the API', async () => {
    const events = stubIngest();
    function Btn() {
      const t = useTrooper();
      return <button onClick={() => t.track('clicked', { n: 1 })}>go</button>;
    }
    const { rerender } = render(
      <TrooperProvider options={OPTIONS}>
        <Btn />
      </TrooperProvider>,
    );
    rerender(
      <TrooperProvider options={OPTIONS}>
        <Btn />
      </TrooperProvider>,
    );
    act(() => screen.getByText('go').click());
    const evs = await events();
    expect(evs.filter((e) => e.type === 'SESSION_START')).toHaveLength(1);
    expect(evs.find((e) => e.type === 'CUSTOM')!.custom).toEqual({ name: 'clicked', props: { n: '1' } });
  });
});

describe('ErrorBoundary', () => {
  it('captures render errors with the component stack and renders the fallback', async () => {
    const events = stubIngest();
    const spy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    const onError = vi.fn();
    render(
      <TrooperProvider options={OPTIONS}>
        <ErrorBoundary name="cart" onError={onError} fallback={({ error }) => <p>oops: {(error as Error).message}</p>}>
          <Boom />
        </ErrorBoundary>
      </TrooperProvider>,
    );
    expect(screen.getByText('oops: render failed')).toBeTruthy();
    const errs = (await events()).filter((e) => e.type === 'ERROR');
    expect(errs).toHaveLength(1);
    expect(errs[0]!.error).toMatchObject({ type: 'TypeError', message: 'render failed', source: 'source', is_crash: false });
    expect(errs[0]!.attributes!.component_stack).toContain('Boom');
    expect(errs[0]!.attributes!.error_boundary).toBe('cart');
    expect(onError).toHaveBeenCalledOnce();
    spy.mockRestore();
  });

  it('resetError re-renders children', () => {
    stubIngest();
    const spy = vi.spyOn(console, 'error').mockImplementation(() => undefined);
    let fail = true;
    function Maybe() {
      if (fail) throw new Error('x');
      return <p>ok</p>;
    }
    render(
      <ErrorBoundary fallback={({ resetError }) => <button onClick={resetError}>retry</button>}>
        <Maybe />
      </ErrorBoundary>,
    );
    fail = false;
    act(() => screen.getByText('retry').click());
    expect(screen.getByText('ok')).toBeTruthy();
    spy.mockRestore();
  });

  it('reactErrorHandler reports root errors', async () => {
    const events = stubIngest();
    render(<TrooperProvider options={OPTIONS} />);
    const then = vi.fn();
    reactErrorHandler('uncaught', then)(new Error('root'), { componentStack: '\n    at App' });
    const err = (await events()).find((e) => e.type === 'ERROR')!;
    expect(err.error).toMatchObject({ message: 'root', is_crash: true });
    expect(err.attributes).toMatchObject({ react_error_kind: 'uncaught' });
    expect(then).toHaveBeenCalled();
  });
});
