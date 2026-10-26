type Listener = (s: string) => void;
const listeners = new Set<Listener>();

export const Platform = { OS: 'ios', Version: '19.0', isPad: false, isTV: false };
export const AppState = {
  currentState: 'active' as string | null,
  addEventListener(_t: 'change', fn: Listener) {
    listeners.add(fn);
    return { remove: () => listeners.delete(fn) };
  },
  /** test helper */
  __emit(state: string) {
    AppState.currentState = state;
    listeners.forEach((l) => l(state));
  },
  __count: () => listeners.size,
};
export const Dimensions = { get: () => ({ width: 393, height: 852, scale: 3 }) };
export const NativeModules: Record<string, unknown> = {};
export const TurboModuleRegistry = {
  modules: {} as Record<string, unknown>,
  get(name: string) {
    return (TurboModuleRegistry.modules[name] as object | undefined) ?? null;
  },
  getEnforcing(name: string) {
    const m = TurboModuleRegistry.get(name);
    if (!m) throw new Error(`TurboModule ${name} not found`);
    return m;
  },
};
