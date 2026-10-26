import { truncate, type ErrorPayload } from '@trooper/core';

const seen = new WeakSet<object>();

/** Returns false when this error object was already reported (e.g. ErrorBoundary + window.onerror). */
export function firstReport(err: unknown): boolean {
  if (err && typeof err === 'object') {
    if (seen.has(err)) return false;
    seen.add(err);
  }
  return true;
}

export function toErrorPayload(err: unknown): ErrorPayload {
  if (err && typeof err === 'object' && ('message' in err || 'stack' in err)) {
    const e = err as { name?: unknown; message?: unknown; stack?: unknown; constructor?: { name?: string } };
    return {
      type: String(e.name || e.constructor?.name || 'Error'),
      message: truncate(String(e.message ?? ''), 2000),
      stack: typeof e.stack === 'string' ? truncate(e.stack, 16000) : undefined,
    };
  }
  let message: string;
  if (typeof err === 'string') message = err;
  else {
    try {
      message = JSON.stringify(err) ?? String(err);
    } catch {
      message = String(err);
    }
  }
  return { type: 'Error', message: truncate(message, 2000) };
}
