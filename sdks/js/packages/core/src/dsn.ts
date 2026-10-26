export interface Dsn {
  /** Ingest key (user-info part of the DSN). */
  key: string;
  /** `http(s)://host[:port][/prefix]` without trailing slash. */
  origin: string;
  projectId: string;
  /** Full RUM endpoint: `<origin>/v1/rum`. */
  rumEndpoint: string;
}

// Regex instead of `new URL()`: React Native's URL polyfill does not parse user-info.
const DSN_RE = /^(https?):\/\/([^@/?#]+)@([^/?#]+)((?:\/[^/?#]+)*)\/([^/?#]+)\/?$/;

/** Parse `https://<key>@<host>[:port][/prefix]/<project_id>` (contracts.md §2). Throws on invalid input. */
export function parseDsn(dsn: string): Dsn {
  const m = DSN_RE.exec((dsn || '').trim());
  if (!m) throw new Error(`[trooper] invalid DSN: expected https://<key>@<host>/<project_id>`);
  const [, scheme, key, host, prefix, projectId] = m as unknown as [string, string, string, string, string, string];
  const origin = `${scheme}://${host}${prefix || ''}`;
  return { key: decodeURIComponent(key), origin, projectId, rumEndpoint: `${origin}/v1/rum` };
}
