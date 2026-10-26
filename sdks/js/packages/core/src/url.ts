/**
 * PII scrubbing for URLs (AGENTS.md rule 6): query values are removed, keys kept; user-info removed.
 * `https://u:p@a.com/p?token=abc&x=1#/r?q=2` → `https://a.com/p?token=&x=#/r?q=`
 */
export function stripQueryValues(url: string | undefined | null): string {
  if (!url) return '';
  let u = String(url);
  u = u.replace(/^([a-z][a-z0-9+.-]*:\/\/)[^@/?#]*@/i, '$1');
  const hashAt = u.indexOf('#');
  const main = hashAt >= 0 ? u.slice(0, hashAt) : u;
  const hash = hashAt >= 0 ? u.slice(hashAt) : '';
  return stripPart(main) + stripPart(hash);
}

function stripPart(s: string): string {
  const q = s.indexOf('?');
  if (q < 0) return s;
  const query = s
    .slice(q + 1)
    .split('&')
    .filter(Boolean)
    .map((kv) => {
      const eq = kv.indexOf('=');
      return (eq >= 0 ? kv.slice(0, eq) : kv) + '=';
    })
    .join('&');
  return s.slice(0, q + 1) + query;
}

const ID_SEGMENT = [
  /^\d+$/, // numeric
  /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i, // uuid
  /^(?=.*\d)[0-9a-f]{8,}$/i, // hex ids / hashes (must contain a digit)
  /^(?=.*\d)(?=.*[a-z])[a-z0-9_-]{20,}$/i, // long opaque tokens (ulid, nanoid, base64url)
];

/**
 * Normalize a raw path to a low-cardinality view name (data-model rule: route templates).
 * Used only when no framework integration provides the real route template.
 */
export function normalizePath(path: string): string {
  const p = (path || '/').split(/[?#]/)[0] || '/';
  const out = p
    .split('/')
    .map((seg) => (seg && ID_SEGMENT.some((re) => re.test(seg)) ? ':id' : seg))
    .join('/');
  return out.length > 1 && out.endsWith('/') ? out.slice(0, -1) : out || '/';
}
