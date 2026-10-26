import type { StringMap } from './types';

export const now = (): number => Date.now();

let debugEnabled = false;
let internalErrors = 0;

export const setDebug = (on: boolean): void => {
  debugEnabled = on;
};

/** Number of errors swallowed inside the SDK since load (AGENTS.md rule 5). */
export const internalErrorCount = (): number => internalErrors;

export function reportInternal(where: string, err: unknown): void {
  internalErrors++;
  if (debugEnabled) {
    try {
      // eslint-disable-next-line no-console
      console.warn(`[trooper] internal error in ${where}`, err);
    } catch {
      /* ignore */
    }
  }
}

/** Run `fn`, swallowing (and counting) any exception. The SDK must never crash the host. */
export function safe<R>(where: string, fn: () => R): R | undefined {
  try {
    return fn();
  } catch (e) {
    reportInternal(where, e);
    return undefined;
  }
}

/** Wrap a function so it never throws. */
export function guard<A extends unknown[], R>(where: string, fn: (...a: A) => R): (...a: A) => R | undefined {
  return (...a: A) => safe(where, () => fn(...a));
}

export const truncate = (s: string, max: number): string => (s.length > max ? s.slice(0, max - 1) + '…' : s);

/** Convert arbitrary props into the proto's map<string,string>. Nested values are JSON-encoded, everything truncated. */
export function toStringMap(props: Record<string, unknown> | undefined | null, maxLen = 512): StringMap | undefined {
  if (!props || typeof props !== 'object') return undefined;
  const out: StringMap = {};
  let n = 0;
  for (const k in props) {
    if (!Object.prototype.hasOwnProperty.call(props, k)) continue;
    const v = props[k];
    if (v === undefined || typeof v === 'function') continue;
    let s: string;
    if (typeof v === 'string') s = v;
    else if (v === null || typeof v !== 'object') s = String(v);
    else {
      try {
        s = JSON.stringify(v);
      } catch {
        s = String(v);
      }
    }
    out[truncate(k, 128)] = truncate(s, maxLen);
    if (++n >= 100) break;
  }
  return n ? out : undefined;
}

export const byteLength = (s: string): number => {
  // UTF-8 length without TextEncoder (not on every RN engine).
  let n = 0;
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    n += c < 0x80 ? 1 : c < 0x800 ? 2 : c >= 0xd800 && c <= 0xdbff ? (i++, 4) : 3;
  }
  return n;
};
