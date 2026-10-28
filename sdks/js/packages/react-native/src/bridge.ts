import { NativeModules, TurboModuleRegistry } from 'react-native';

/** Context shared with a native TaskTrooper SDK (contracts.md §9 "Hibrit uygulamalar"). */
export interface NativeContext {
  session_id?: string;
  anonymous_id?: string;
  view_id?: string;
  view_name?: string;
  user_id?: string;
  /** JS RUM sampling decision for `session_id` (JS → native only; contracts.md §9). */
  sampled?: boolean;
}

/**
 * Bridge to the native Android/iOS SDK (contracts.md §9 channel `dev.tasktrooper.telemetry/rum`, exposed in React Native as
 * the `TrooperRum` native module shipped with this package). Native crashes are reported by the native SDK with this
 * context, so JS and native share one session. Every method is optional and may reject.
 */
export interface NativeBridge {
  setContext?(ctx: NativeContext): unknown;
  getContext?(): Promise<NativeContext | null | undefined>;
  /** Unix ms of OS process start — APP_START `start_reference=process`. */
  getProcessStartTime?(): Promise<number | null | undefined>;
  /** Install native wireframe replay; resolves true when the native replay module is available. */
  startReplay?(options: NativeReplayOptions): Promise<boolean | null | undefined>;
  /** Stop native replay; resolves true when stopped. */
  stopReplay?(): Promise<boolean | null | undefined> | unknown;
  isReplayRecording?(): Promise<boolean | null | undefined>;
  /** JS ERROR captured → native on-error replay buffer is flushed. */
  notifyReplayError?(): unknown;
}

/** Options passed to the native replay module (only the keys that were set). */
export interface NativeReplayOptions {
  sampleRate?: number;
  onErrorSampleRate?: number;
  maskAllText?: boolean;
}

/** React Native module name of the bundled native module (New Arch TurboModule + legacy NativeModules). */
export const NATIVE_MODULE_NAME = 'TrooperRum';
/** Contract channel name (contracts.md §9); also looked up in `NativeModules` for back-compat with custom modules. */
export const NATIVE_CHANNEL = 'dev.tasktrooper.telemetry/rum';

function asBridge(m: unknown): NativeBridge | undefined {
  return m && typeof m === 'object' ? (m as NativeBridge) : undefined;
}

/**
 * The native module if linked; undefined otherwise (silent). Lookup order: TurboModule `TrooperRum` (New Architecture),
 * `NativeModules.TrooperRum` (legacy bridge / interop), `NativeModules['dev.tasktrooper.telemetry/rum']` (custom module).
 */
export function defaultNativeBridge(): NativeBridge | undefined {
  const lookups: Array<() => unknown> = [
    () => TurboModuleRegistry?.get?.(NATIVE_MODULE_NAME),
    () => (NativeModules as Record<string, unknown> | undefined)?.[NATIVE_MODULE_NAME],
    () => (NativeModules as Record<string, unknown> | undefined)?.[NATIVE_CHANNEL],
  ];
  for (const lookup of lookups) {
    try {
      const m = asBridge(lookup());
      if (m) return m;
    } catch {
      // not linked / registry unavailable — try the next one
    }
  }
  return undefined;
}

/** Call a bridge method, swallowing sync throws and async rejections. */
export async function callBridge<T>(fn: (() => T | Promise<T>) | undefined): Promise<T | undefined> {
  if (!fn) return undefined;
  try {
    return await fn();
  } catch {
    return undefined;
  }
}
