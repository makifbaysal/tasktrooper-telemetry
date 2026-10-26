import { act, render } from '@testing-library/react';
import { createMemoryRouter, MemoryRouter, Outlet, RouterProvider, useNavigate, type RouteObject } from 'react-router';
import { afterEach, describe, expect, it } from 'vitest';
import { init } from '@trooper/browser';
import { routeTemplate, TrooperRoutes, trackReactRouter } from '../src/react-router';
import { tanstackTemplate, trackTanStackRouter } from '../src/tanstack-router';
import { appRouterTemplate, pagesRouterTemplate, TrooperNextAppRouter, useNextPagesRouterTracking } from '../src/nextjs';
import { nav } from './mocks/next-navigation';
import { router as pagesRouter } from './mocks/next-router';
import { OPTIONS, reset, stubIngest, viewStarts } from './helpers';

afterEach(reset);

const routes: RouteObject[] = [
  {
    path: '/',
    element: <Outlet />,
    children: [
      { index: true, element: <p>home</p> },
      { path: 'products/:productId', element: <p>product</p> },
      { path: 'users', children: [{ path: ':userId/orders/:orderId', element: <p>order</p> }] },
      { path: 'docs/*', element: <p>docs</p> },
    ],
  },
];

describe('react-router', () => {
  it('routeTemplate joins nested paths', () => {
    expect(routeTemplate([{ route: { path: '/' } }, { route: {} }])).toBe('/');
    expect(routeTemplate([{ route: { path: '/' } }, { route: { path: 'users' } }, { route: { path: ':id' } }])).toBe('/users/:id');
    expect(routeTemplate([{ route: { path: '/app/' } }, { route: { path: 'docs/*' } }])).toBe('/app/docs/*');
  });

  it('data router: trackReactRouter names views with route templates', async () => {
    const events = stubIngest();
    init(OPTIONS);
    const router = createMemoryRouter(routes, { initialEntries: ['/products/123'] });
    trackReactRouter(router);
    render(<RouterProvider router={router} />);
    await act(() => router.navigate('/users/7/orders/99'));
    await act(() => router.navigate('/users/8/orders/100'));
    await act(() => router.navigate('/docs/a/b'));
    await act(() => router.navigate('/'));
    const names = viewStarts(await events());
    // The initial auto view ("/" from window.location) is followed by router-reported templates.
    expect(names[0]).toBe('/');
    expect(names.slice(1)).toEqual(['/products/:productId', '/users/:userId/orders/:orderId', '/users/:userId/orders/:orderId', '/docs/*', '/']);
  });

  it('declarative router: <TrooperRoutes routes> computes templates with matchRoutes', async () => {
    const events = stubIngest();
    init(OPTIONS);
    let go: (p: string) => void = () => undefined;
    function Nav() {
      go = useNavigate();
      return null;
    }
    render(
      <MemoryRouter initialEntries={['/']}>
        <TrooperRoutes routes={routes} />
        <Nav />
      </MemoryRouter>,
    );
    act(() => go('/products/abc'));
    expect(viewStarts(await events())).toEqual(['/', '/products/:productId']);
  });
});

describe('TanStack Router', () => {
  it('converts $params and reports the leaf full path', async () => {
    expect(tanstackTemplate('/posts/$postId/')).toBe('/posts/:postId');
    expect(tanstackTemplate('/files/$')).toBe('/files/*');
    expect(tanstackTemplate('/{-$lang}/about')).toBe('/:lang?/about');
    const events = stubIngest();
    init(OPTIONS);
    let cb: () => void = () => undefined;
    const fake = {
      state: { location: { pathname: '/posts/1' }, matches: [{ routeId: '__root__', fullPath: '/' }, { routeId: '/posts/$postId', fullPath: '/posts/$postId' }] },
      subscribe: (_e: 'onResolved', f: () => void) => ((cb = f), () => undefined),
    };
    trackTanStackRouter(fake);
    fake.state = { location: { pathname: '/files/a/b' }, matches: [{ routeId: '/files/$' } as { routeId: string; fullPath: string }] };
    (fake as { routesById?: unknown }).routesById = { '/files/$': { fullPath: '/files/$' } };
    cb();
    expect(viewStarts(await events())).toEqual(['/', '/posts/:postId', '/files/*']);
  });
});

describe('Next.js', () => {
  it('App Router template from pathname + params', () => {
    expect(appRouterTemplate('/shop/shoes/42', { category: 'shoes', id: '42' })).toBe('/shop/:category/:id');
    expect(appRouterTemplate('/docs/a/b/c', { slug: ['a', 'b', 'c'] })).toBe('/docs/:slug*');
    expect(appRouterTemplate('/about', {})).toBe('/about');
    expect(appRouterTemplate('/blog/hello%20world', { slug: 'hello world' })).toBe('/blog/:slug');
  });

  it('Pages Router template from router.pathname', () => {
    expect(pagesRouterTemplate('/products/[id]')).toBe('/products/:id');
    expect(pagesRouterTemplate('/docs/[...slug]')).toBe('/docs/:slug*');
    expect(pagesRouterTemplate('/[[...all]]')).toBe('/:all*');
  });

  it('<TrooperNextAppRouter /> reports views', async () => {
    const events = stubIngest();
    init(OPTIONS);
    nav.pathname = '/shop/shoes/42';
    nav.params = { category: 'shoes', id: '42' };
    render(<TrooperNextAppRouter />);
    expect(viewStarts(await events()).at(-1)).toBe('/shop/:category/:id');
  });

  it('useNextPagesRouterTracking reports views', async () => {
    const events = stubIngest();
    init(OPTIONS);
    pagesRouter.pathname = '/products/[id]';
    pagesRouter.asPath = '/products/9?x=1';
    function App() {
      useNextPagesRouterTracking();
      return null;
    }
    render(<App />);
    expect(viewStarts(await events()).at(-1)).toBe('/products/:id');
  });
});
