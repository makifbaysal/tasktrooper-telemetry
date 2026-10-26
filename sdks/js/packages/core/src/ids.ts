const HEX = '0123456789abcdef';

function randomBytes(n: number): Uint8Array {
  const b = new Uint8Array(n);
  const c = (globalThis as { crypto?: { getRandomValues?: (a: Uint8Array) => Uint8Array } }).crypto;
  if (c && typeof c.getRandomValues === 'function') {
    try {
      c.getRandomValues(b);
      return b;
    } catch {
      /* fall through */
    }
  }
  for (let i = 0; i < n; i++) b[i] = (Math.random() * 256) | 0;
  return b;
}

/** Random lowercase hex of `bytes` bytes; never all-zero (W3C trace context requirement). */
export function randomHex(bytes: number): string {
  const b = randomBytes(bytes);
  let s = '';
  let nonZero = false;
  for (let i = 0; i < bytes; i++) {
    const v = b[i]!;
    if (v) nonZero = true;
    s += HEX[v >> 4]! + HEX[v & 15]!;
  }
  return nonZero ? s : randomHex(bytes);
}

/** RFC 4122 v4 UUID. */
export function uuid(): string {
  const h = randomHex(16).split('');
  h[12] = '4';
  h[16] = HEX[(parseInt(h[16]!, 16) & 3) | 8]!;
  const s = h.join('');
  return `${s.slice(0, 8)}-${s.slice(8, 12)}-${s.slice(12, 16)}-${s.slice(16, 20)}-${s.slice(20)}`;
}

/** Deterministic 0..1 value from a string (FNV-1a), used for per-session sampling. */
export function hashToUnit(s: string): number {
  let x = 2166136261;
  for (let i = 0; i < s.length; i++) x = Math.imul(x ^ s.charCodeAt(i), 16777619);
  return (x >>> 0) / 4294967296;
}
