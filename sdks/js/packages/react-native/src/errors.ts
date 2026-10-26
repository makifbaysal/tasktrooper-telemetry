import { truncate, type ErrorPayload } from '@trooper/core';

export function toErrorPayload(err: unknown): ErrorPayload {
  if (err && typeof err === 'object' && ('message' in err || 'stack' in err)) {
    const e = err as { name?: unknown; message?: unknown; stack?: unknown };
    return {
      type: String(e.name || 'Error'),
      message: truncate(String(e.message ?? ''), 2000),
      stack: typeof e.stack === 'string' ? truncate(e.stack, 16000) : undefined,
    };
  }
  let message: string;
  try {
    message = typeof err === 'string' ? err : (JSON.stringify(err) ?? String(err));
  } catch {
    message = String(err);
  }
  return { type: 'Error', message: truncate(message, 2000) };
}
