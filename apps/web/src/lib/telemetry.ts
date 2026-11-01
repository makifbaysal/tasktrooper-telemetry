/**
 * Self-observability of the dashboard (ADR-027, .ai/contracts.md §27): RUM app `tt-web` reporting to the platform's own
 * project. Off unless `VITE_TROOPER_DSN` is set — then the SDK chunks (`@tasktrooper/browser`, `@tasktrooper/react`, and
 * `@tasktrooper/replay` when a replay rate is > 0) are loaded with a dynamic import, so a build without the DSN pays nothing
 * beyond this file.
 */
import { API_URL } from '@/data/remote';
import { authState, onAuthChange } from '@/data/auth';
import type { router as appRouter } from '@/router';

type Router = typeof appRouter;
type ErrorInfo = { componentStack?: string };
type Handler = (error: unknown, info: ErrorInfo) => void;

const env = import.meta.env;
const DSN = (env.VITE_TROOPER_DSN as string | undefined)?.trim() || undefined;

/** True when the build/dev server has a DSN; nothing else in this file does anything otherwise. */
export const telemetryEnabled = DSN !== undefined;

function rate(raw: unknown, fallback: number): number {
  const n = typeof raw === 'string' && raw.trim() !== '' ? Number(raw) : NaN;
  return Number.isFinite(n) ? Math.min(1, Math.max(0, n)) : fallback;
}

/** React root error callbacks are set at `createRoot` time, before the SDK chunk arrives: forward once it is loaded. */
const sinks: Partial<Record<'uncaught' | 'caught' | 'recoverable', Handler>> = {};
function forward(kind: 'uncaught' | 'caught' | 'recoverable'): Handler {
  return (error, info) => {
    // keep React's default reporting visible in the console
    if (kind === 'recoverable') console.warn(error); else console.error(error, info?.componentStack ?? '');
    try { sinks[kind]?.(error, info); } catch { /* never break the host */ }
  };
}

/** `createRoot(el, rootErrorOptions)` — undefined (React defaults) when telemetry is off. */
export const rootErrorOptions = telemetryEnabled
  ? { onUncaughtError: forward('uncaught'), onCaughtError: forward('caught'), onRecoverableError: forward('recoverable') }
  : undefined;

function apiOrigin(): string | undefined {
  try { return new URL(API_URL, window.location.href).origin; } catch { return undefined; }
}

/**
 * Loads and starts the SDK. Awaited before boot so the first api calls (`/auth/config`, `/me`) are already instrumented;
 * any failure is swallowed — the dashboard must work without telemetry.
 */
export async function startTelemetry(router: Router): Promise<void> {
  // literal env checks so a build without the DSN (or without replay) drops the SDK chunks entirely
  if (!import.meta.env.VITE_TROOPER_DSN || !DSN) return;
  try {
    const replaySampleRate = rate(env.VITE_TROOPER_REPLAY_SAMPLE_RATE, 0);
    const replayOnErrorSampleRate = rate(env.VITE_TROOPER_REPLAY_ON_ERROR_SAMPLE_RATE, 0);
    const withReplay = replaySampleRate > 0 || replayOnErrorSampleRate > 0;
    const [react, { trackReactRouter }, replay] = await Promise.all([
      import('@tasktrooper/react'),
      import('@tasktrooper/react/react-router'),
      (import.meta.env.VITE_TROOPER_REPLAY_SAMPLE_RATE || import.meta.env.VITE_TROOPER_REPLAY_ON_ERROR_SAMPLE_RATE) && withReplay
        ? import('@tasktrooper/replay')
        : Promise.resolve(undefined),
    ]);
    const origin = apiOrigin();
    react.init({
      dsn: DSN,
      app: 'tt-web',
      environment: (env.VITE_TROOPER_ENV as string | undefined) || env.MODE,
      version: __TT_WEB_VERSION__,
      sampleRate: rate(env.VITE_TROOPER_SAMPLE_RATE, 1),
      // traceparent / tracestate / baggage only to the api (its CORS allows them), never to other origins
      // (VITE_TROOPER_TRACE_PROPAGATION=false for an api whose CORS predates ADR-027 — its preflight would fail)
      tracePropagationTargets: origin && env.VITE_TROOPER_TRACE_PROPAGATION !== 'false' ? [origin] : [],
      debug: env.DEV,
      integrations: replay
        ? [replay.replayIntegration({ sampleRate: replaySampleRate, onErrorSampleRate: replayOnErrorSampleRate, maskAllText: true, maskAllInputs: true })]
        : [],
    });
    sinks.uncaught = react.reactErrorHandler('uncaught');
    sinks.caught = react.reactErrorHandler('caught');
    sinks.recoverable = react.reactErrorHandler('recoverable');
    trackReactRouter(router); // view name = route template ("/services/:serviceId"), never the raw path

    // identify by internal user id only — no email / name (PII)
    let current: string | undefined;
    const syncUser = () => {
      const id = authState().me?.user.id || undefined;
      if (id === current) return;
      if (id) react.identify(id); else if (current) react.clearUser();
      current = id;
    };
    syncUser();
    onAuthChange(syncUser);
  } catch (e) {
    if (env.DEV) console.warn('[telemetry] disabled', e);
  }
}
