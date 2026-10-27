import { describe, expect, it } from 'vitest';
import { expressRoute, fastifyRoute, honoRoute, koaRoute, templateFromRequest } from '../src/frameworks';
import { isId, normalizePath, normalizeRoute, stripQueryValues } from '../src/route';

describe('normalizeRoute', () => {
  it.each([
    ['/users/:id', '/users/:id'],
    ['/users/{id}', '/users/:id'],
    ['/users/{id:[0-9]{3}}', '/users/:id'],
    ['/users/<id>', '/users/:id'],
    ['/users/<int:id>', '/users/:id'],
    ['/users/:id(\\d+)', '/users/:id'],
    ['/users/:id?', '/users/:id'],
    ['/files/{path...}', '/files/*path'],
    ['/files/{*path}', '/files/*path'],
    ['/files/*path', '/files/*path'],
    ['/files/*', '/files/*'],
    ['/v1/orders/', '/v1/orders'],
    ['//v1//orders', '/v1/orders'],
    ['/', '/'],
    ['/orders/123', '/orders/:id'],
    ['/pkg.v1.Orders/Get', '/pkg.v1.Orders/Get'],
  ])('%s → %s', (input, want) => {
    expect(normalizeRoute(input)).toBe(want);
  });

  it('empty input', () => {
    expect(normalizeRoute('')).toBeUndefined();
    expect(normalizeRoute(undefined)).toBeUndefined();
  });
});

describe('normalizePath (writer parity)', () => {
  it.each([
    ['/users/42', '/users/:id'],
    ['/users/42/orders/7?x=1', '/users/:id/orders/:id'],
    ['/o/550e8400-e29b-41d4-a716-446655440000', '/o/:id'],
    ['/c/deadbeef01', '/c/:id'],
    ['/u/01ARZ3NDEKTSV4RRFFQ69G5FAV', '/u/:id'],
    ['/inv/INV-2024-000123', '/inv/:id'],
    ['https://x.com/a/1', '/a/:id'],
    ['/healthz', '/healthz'],
    ['/a/', '/a'],
  ])('%s → %s', (input, want) => {
    expect(normalizePath(input)).toBe(want);
  });

  it('isId keeps words', () => {
    expect(isId('users')).toBe(false);
    expect(isId('v2')).toBe(false);
    expect(isId('123')).toBe(true);
  });

  it('strips query values', () => {
    expect(stripQueryValues('https://x/a?b=1&c=2#f')).toBe('https://x/a?b&c');
    expect(stripQueryValues('/a')).toBe('/a');
  });
});

describe('framework route extraction', () => {
  it('express: baseUrl + route.path', () => {
    expect(expressRoute({ baseUrl: '/v1', route: { path: '/users/:id' } })).toBe('/v1/users/:id');
    expect(expressRoute({ route: { path: '/' } })).toBe('/');
    expect(expressRoute({ baseUrl: '/v1' })).toBeUndefined();
    expect(expressRoute({ route: { path: /re/ } })).toBeUndefined();
  });

  it('fastify: routeOptions.url / routerPath', () => {
    expect(fastifyRoute({ routeOptions: { url: '/users/:id' } })).toBe('/users/:id');
    expect(fastifyRoute({ routerPath: '/legacy/:id' })).toBe('/legacy/:id');
    expect(fastifyRoute({})).toBeUndefined();
  });

  it('koa: _matchedRoute', () => {
    expect(koaRoute({ _matchedRoute: '/items/:id' })).toBe('/items/:id');
    expect(koaRoute({})).toBeUndefined();
  });

  it('hono: last non-middleware matched route', () => {
    const c = {
      req: {
        routePath: '/*',
        matchedRoutes: [
          { method: 'ALL', path: '/*' },
          { method: 'GET', path: '/posts/:id' },
        ],
      },
    };
    expect(honoRoute(c)).toBe('/posts/:id');
    expect(honoRoute({ req: { routePath: '/a/:b' } })).toBe('/a/:b');
    expect(honoRoute({ req: { routePath: '*' } })).toBeUndefined();
  });

  it('nest: either adapter', () => {
    expect(templateFromRequest({ route: { path: '/api/cats/:id' } })).toBe('/api/cats/:id');
    expect(templateFromRequest({ routeOptions: { url: '/api/cats/:id' } })).toBe('/api/cats/:id');
    expect(templateFromRequest(null)).toBeUndefined();
  });
});
