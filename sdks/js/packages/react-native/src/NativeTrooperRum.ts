// TurboModule codegen spec for the `TrooperRum` native module (contracts.md §9 hybrid bridge).
// Read by React Native codegen (package.json `codegenConfig`); the SDK itself looks the module up in `bridge.ts`.
import type { TurboModule } from 'react-native';
import { TurboModuleRegistry } from 'react-native';

export interface Spec extends TurboModule {
  /** JS → native: {session_id, anonymous_id, view_id, view_name, user_id}. No-op without a native TaskTrooper SDK. */
  setContext(ctx: Object): void;
  /** Native SDK context (same keys) or null when no native SDK is running. */
  getContext(): Promise<Object | null>;
  /** Unix epoch ms of OS process start; available even without the native SDK (null if unknown). */
  getProcessStartTime(): Promise<number | null>;
  /**
   * Installs the native SDK's wireframe session replay (contracts.md §23 "Hibrit uygulamalar") with
   * {sampleRate?, onErrorSampleRate?, maskAllText?}. Resolves false when the native SDK / its replay module is absent.
   */
  startReplay(options: Object): Promise<boolean>;
  /** Stops native replay for the rest of the process. Resolves false when the native SDK cannot stop it. */
  stopReplay(): Promise<boolean>;
  /** True while the current session is recorded (not just buffered for errors). */
  isReplayRecording(): Promise<boolean>;
  /** A JS ERROR was captured: native replay flushes its on-error buffer and keeps recording (like a native ERROR). */
  notifyReplayError(): void;
}

export default TurboModuleRegistry.get<Spec>('TrooperRum');
