import type { Integration, IntegrationClient } from '@trooper/browser';
import { Replay, type ReplayMode, type ReplayOptions } from './replay';

export interface ReplayIntegration extends Integration {
  /** Current recording mode of this page: `off`, `buffer` (last 60 s in memory, waiting for an error) or `session`. */
  getMode(): ReplayMode;
  /** Send the open segment now. */
  flush(): Promise<void>;
  /** Stop recording for the rest of this page (also done by `close()` of `@trooper/browser`). */
  stop(): Promise<void>;
}

/**
 * Session replay for `@trooper/browser` (ADR-021, contracts.md §23):
 *
 * ```ts
 * init({ dsn, app: 'shop-web', integrations: [replayIntegration({ sampleRate: 0.1, onErrorSampleRate: 1.0 })] });
 * ```
 *
 * Never throws: every failure is swallowed and counted as an internal SDK error.
 */
export function replayIntegration(options: ReplayOptions = {}): ReplayIntegration {
  let replay: Replay | undefined;
  return {
    name: 'Replay',
    setup(ic: IntegrationClient) {
      try {
        if (options?.enabled === false || replay) return;
        if (typeof window === 'undefined' || typeof document === 'undefined') return;
        const r = new Replay(ic, options ?? {});
        replay = r;
        r.start();
        return () => void r.stop().catch(() => undefined);
      } catch (e) {
        ic.reportInternal('replay.setup', e);
        return undefined;
      }
    },
    getMode: () => replay?.currentMode ?? 'off',
    flush: () => (replay ? replay.flushSegment().catch(() => undefined) : Promise.resolve()),
    stop: () => {
      const r = replay;
      return r ? r.stop().catch(() => undefined) : Promise.resolve();
    },
  };
}

export {
  BUFFER_MS,
  CHECKOUT_MS,
  SDK_NAME,
  SDK_VERSION,
  SEGMENT_BYTES,
  SEGMENT_MS,
  type ReplayMode,
  type ReplayOptions,
  type ReplaySegmentPayload,
  type SegmentStats,
} from './replay';
