import type { Counter, Meter } from '@opentelemetry/api';

/**
 * The SDK never throws into the host (AGENTS.md rule 5): every swallowed error is counted here and exported as
 * `trooper.sdk.errors{component}` (meter `TaskTrooper.Telemetry`, .ai/contracts.md §4) once a meter is bound.
 */
let count = 0;
let counter: Counter | undefined;
let debug = false;
const pending = new Map<string, number>();

/** Number of errors swallowed by the SDK since process start. */
export function internalErrorCount(): number {
  return count;
}

export function setDebug(on: boolean): void {
  debug = on;
}

/** Binds `trooper.sdk.errors` to a meter; errors recorded before binding are flushed into it. */
export function bindSelfMetrics(meter: Meter): void {
  try {
    counter = meter.createCounter('trooper.sdk.errors', {
      description: 'Errors swallowed by the trooper SDK',
      unit: '{error}',
    });
    for (const [component, n] of pending) counter.add(n, { component });
    pending.clear();
  } catch {
    counter = undefined;
  }
}

/** Test hook. */
export function resetSelfMetrics(): void {
  count = 0;
  counter = undefined;
  pending.clear();
}

/** Records a swallowed error. Never throws. */
export function recordError(component: string, err?: unknown): void {
  count++;
  try {
    if (counter) counter.add(1, { component });
    else pending.set(component, (pending.get(component) ?? 0) + 1);
    if (debug) console.warn(`[trooper] ${component}:`, err);
  } catch {
    /* ignore */
  }
}

/** Runs `fn`, returning `fallback` (and counting the error) if it throws. */
export function safe<T>(component: string, fn: () => T, fallback: T): T {
  try {
    return fn();
  } catch (err) {
    recordError(component, err);
    return fallback;
  }
}
