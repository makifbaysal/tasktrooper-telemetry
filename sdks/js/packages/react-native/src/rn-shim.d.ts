// Minimal ambient types so this package builds without `react-native` installed (peer dependency).
// Only the members used by this package are declared; they are not part of the published types.
declare module 'react-native' {
  export const Platform: {
    OS: 'ios' | 'android' | 'web' | 'windows' | 'macos' | string;
    Version: string | number;
    isPad?: boolean;
    isTV?: boolean;
  };
  export const AppState: {
    currentState: string | null;
    addEventListener(type: 'change', fn: (state: string) => void): { remove(): void };
  };
  export const Dimensions: { get(dim: 'screen' | 'window'): { width: number; height: number; scale: number } };
  export const NativeModules: Record<string, unknown>;
  // eslint-disable-next-line @typescript-eslint/no-empty-object-type
  export interface TurboModule {}
  export const TurboModuleRegistry: {
    get<T extends TurboModule>(name: string): T | null;
    getEnforcing<T extends TurboModule>(name: string): T;
  };
}
