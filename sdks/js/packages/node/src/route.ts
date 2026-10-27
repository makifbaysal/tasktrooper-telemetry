/**
 * Route template normalization (.ai/contracts.md §4, same rules as the Go/JVM SDKs and the writer):
 *
 *   /users/{id}            -> /users/:id      (OpenAPI / NestJS-style docs)
 *   /users/{id:[0-9]+}     -> /users/:id
 *   /users/<id>            -> /users/:id
 *   /users/:id(\\d+)       -> /users/:id      (path-to-regexp constraint)
 *   /users/:id?            -> /users/:id      (optional param)
 *   /files/{p...} {*p} *p  -> /files/*p       (catch-all)
 *   /files/*               -> /files/*        (anonymous wildcard, kept)
 *
 * Raw number / uuid / hex / ulid segments become `:id` (normalizePath). gRPC method names are returned unchanged.
 */
export function normalizeRoute(route: string | undefined | null): string | undefined {
  if (route == null) return undefined;
  let r = String(route).trim();
  if (r === '') return undefined;
  if (r.startsWith('/') && r.includes('.') && /^\/[\w.]+\/\w+$/.test(r) && !r.includes(':')) {
    return r; // gRPC: /pkg.Service/Method
  }
  if (r.includes('{')) r = braces(r);
  if (r.includes('<')) r = r.replace(/<(?:[^:<>]+:)?([^<>]+)>/g, ':$1');
  if (r.includes(':')) {
    // :id(\d+) → :id ; :id? / :id+ / :id* → :id
    r = r.replace(/:([A-Za-z_$][\w$]*)(\((?:[^()]|\([^()]*\))*\))?[?+*]?/g, ':$1');
  }
  // *name stays; "(.*)" groups → *
  r = r.replace(/\(\.\*\)/g, '*');
  if (r.length > 1 && r.endsWith('/')) r = r.replace(/\/+$/, '');
  r = r.replace(/\/{2,}/g, '/');
  if (r === '') r = '/';
  return normalizePath(r, true);
}

function braces(route: string): string {
  let out = '';
  for (let i = 0; i < route.length; ) {
    const c = route[i]!;
    if (c !== '{') {
      out += c;
      i++;
      continue;
    }
    let depth = 0;
    let j = i;
    for (; j < route.length; j++) {
      if (route[j] === '{') depth++;
      else if (route[j] === '}' && --depth === 0) break;
    }
    if (j >= route.length) {
      out += route.slice(i);
      break;
    }
    let name = route.slice(i + 1, j);
    const k = name.indexOf(':');
    if (k >= 0) name = name.slice(0, k);
    if (name === '$') {
      /* end anchor */
    } else if (name.endsWith('...')) out += '*' + name.slice(0, -3);
    else if (name.startsWith('*')) out += '*' + name.slice(1);
    else out += ':' + name;
    i = j + 1;
  }
  return out;
}

const MAX_ROUTE_LEN = 256;

/**
 * Normalizes a URL path (port of the writer's norm.Route): query/fragment dropped, empty segments collapsed,
 * id-like segments replaced by `:id`. With `template=true`, `:param` / `*param` segments are kept.
 */
export function normalizePath(p: string, template = false): string {
  let path = p;
  const q = path.search(/[?#]/);
  if (q >= 0) path = path.slice(0, q);
  const scheme = path.indexOf('://');
  if (scheme >= 0) {
    const rest = path.slice(scheme + 3);
    const slash = rest.indexOf('/');
    path = slash >= 0 ? rest.slice(slash) : '/';
  }
  if (path === '' || path === '/') return '/';
  const segs: string[] = [];
  for (const seg of path.split('/')) {
    if (seg === '') continue;
    segs.push(template && (seg.startsWith(':') || seg.startsWith('*')) ? seg : isId(seg) ? ':id' : seg);
  }
  const out = '/' + segs.join('/');
  return out.length > MAX_ROUTE_LEN ? out.slice(0, MAX_ROUTE_LEN) : out;
}

/** Whether a path segment looks like an identifier rather than a static word (writer's norm.IsID). */
export function isId(s: string): boolean {
  const n = s.length;
  if (n === 0) return false;
  let digits = 0;
  let hexLetters = 0;
  let letters = 0;
  let other = 0;
  for (let i = 0; i < n; i++) {
    const c = s.charCodeAt(i);
    if (c >= 48 && c <= 57) digits++;
    else if ((c >= 97 && c <= 102) || (c >= 65 && c <= 70)) hexLetters++;
    else if ((c >= 103 && c <= 122) || (c >= 71 && c <= 90)) letters++;
    else other++;
  }
  if (digits === n) return true;
  if (n === 36 && /^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$/.test(s)) return true;
  if (letters === 0 && other === 0 && n >= 8 && digits > 0) return true;
  if (n === 26 && other === 0 && digits > 0 && /^[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$/.test(s)) return true;
  if (n >= 20 && digits >= 4 && digits + hexLetters + letters >= n - 2) return true;
  if (digits >= 6 && digits * 2 >= n) return true;
  return false;
}

/** Removes query values (keys kept) and the fragment: `https://x/a?b=1&c=2#f` → `https://x/a?b&c`. */
export function stripQueryValues(u: string): string {
  let s = u;
  const hash = s.indexOf('#');
  if (hash >= 0) s = s.slice(0, hash);
  const q = s.indexOf('?');
  if (q < 0) return s;
  const keys = s
    .slice(q + 1)
    .split('&')
    .filter((kv) => kv !== '')
    .map((kv) => {
      const eq = kv.indexOf('=');
      return eq >= 0 ? kv.slice(0, eq) : kv;
    });
  return keys.length ? s.slice(0, q) + '?' + keys.join('&') : s.slice(0, q);
}

/** `a=1&b=2` → `a&b`. */
export function stripQueryString(q: string): string {
  return stripQueryValues('?' + q.replace(/^\?/, '')).slice(1);
}
