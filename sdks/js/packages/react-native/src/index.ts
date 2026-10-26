import { AppState, Dimensions, Platform } from 'react-native';
import {
  Client,
  instrumentXhr,
  KeyValueOfflineStore,
  reportInternal,
  safe,
  toStringMap,
  uuid,
  type CoreOptions,
  type Device,
  type KeyValueStorage,
  type PropagationTarget,
} from '@trooper/core';
import { callBridge, defaultNativeBridge, type NativeBridge, type NativeContext } from './bridge';
import { toErrorPayload } from './errors';

/** Captured as early as possible: import this package first in index.js for an accurate js_init app start. */
const JS_START = Date.now();

export const SDK_NAME = '@trooper/react-native';
export const SDK_VERSION = '0.1.0';

/** Subset of `react-native-device-info` used (pass the default export). */
export interface DeviceInfoLike {
  getModel?(): string;
  getBrand?(): string;
  getManufacturerSync?(): string;
  getVersion?(): string;
  getBuildNumber?(): string;
}

export interface ReactNativeOptions extends CoreOptions {
  /** URLs that receive W3C `traceparent` and `baggage: tt.svc=<app>` headers. Default: none. */
  tracePropagationTargets?: PropagationTarget[];
  /** `@react-native-async-storage/async-storage` default export: offline queue + anonymous id survive restarts. */
  storage?: KeyValueStorage;
  /** `react-native-device-info` default export: device model/manufacturer and app version. */
  deviceInfo?: DeviceInfoLike;
  /** Connection type provider, e.g. from @react-native-community/netinfo. */
  getConnection?: () => string | undefined;
  /** ErrorUtils global handler (chained). Default true. */
  trackErrors?: boolean;
  /** Hermes unhandled promise rejections. Default: true in release builds (`!__DEV__`), RN's dev warning otherwise. */
  captureUnhandledRejections?: boolean;
  /** XHR (and fetch, which RN implements on XHR) RESOURCE events. Default true. */
  trackNetwork?: boolean;
  /** Override / disable (`null`) the native bridge. Default: the `TrooperRum` native module (TurboModule / NativeModules) if linked. */
  nativeBridge?: NativeBridge | null;
}

interface Screen {
  id: string;
  name: string;
  start: number;
  emitted: boolean;
  initial: boolean;
  loading?: number;
}

const raf = (fn: () => void) => {
  const r = (globalThis as { requestAnimationFrame?: (cb: () => void) => unknown }).requestAnimationFrame;
  if (typeof r === 'function') r(fn);
  else setTimeout(fn, 0);
};

function detectDevice(info?: DeviceInfoLike): { device: Device; os: { name: string; version: string } } {
  const screen = safe('dimensions', () => Dimensions.get('screen'));
  const locale = safe('locale', () => Intl.DateTimeFormat().resolvedOptions().locale);
  return {
    device: {
      type: Platform.isPad ? 'tablet' : Platform.isTV ? 'tv' : 'mobile',
      manufacturer: safe('manufacturer', () => info?.getManufacturerSync?.() ?? info?.getBrand?.()),
      model: safe('model', () => info?.getModel?.()),
      locale,
      screen_width: screen ? Math.round(screen.width * screen.scale) : undefined,
      screen_height: screen ? Math.round(screen.height * screen.scale) : undefined,
    },
    os: { name: Platform.OS === 'ios' ? 'iOS' : Platform.OS === 'android' ? 'Android' : String(Platform.OS), version: String(Platform.Version) },
  };
}

export class ReactNativeSdk {
  readonly client: Client;
  readonly bridge: NativeBridge | undefined;
  screen: Screen | undefined;
  private lastScreenName: string | undefined;
  private appStartReported = false;
  private firstScreen = true;
  private teardown: (() => void)[] = [];

  constructor(readonly options: ReactNativeOptions) {
    const { device, os } = detectDevice(options.deviceInfo);
    this.bridge = options.nativeBridge === null ? undefined : (options.nativeBridge ?? defaultNativeBridge());
    this.client = new Client({
      ...options,
      version: options.version ?? safe('version', () => options.deviceInfo?.getVersion?.()),
      platform: 'PLATFORM_REACT_NATIVE',
      sdk: { name: SDK_NAME, version: SDK_VERSION },
      device,
      os,
      offlineStore: options.storage ? new KeyValueOfflineStore(options.storage) : undefined,
      getConnection: options.getConnection ?? (() => 'unknown'),
      onSessionStart: (_s, first) => {
        if (!first && this.screen) this.startScreen(this.screen.name, true);
        this.syncNative();
      },
    });
  }

  start(): void {
    const o = this.options;
    const c = this.client;
    if (o.trackErrors !== false) this.instrumentErrors();
    if (o.captureUnhandledRejections ?? !isDev()) this.instrumentRejections();
    if (o.trackNetwork !== false) {
      this.teardown.push(
        instrumentXhr({
          capture: (e) => c.capture(e),
          tracePropagationTargets: o.tracePropagationTargets ?? [],
          serviceName: o.app,
          ignoreUrls: [c.dsn.origin + '/v1/'],
        }),
      );
    }
    const sub = safe('appState', () =>
      AppState.addEventListener('change', (state) =>
        safe('appState.change', () => {
          if (state === 'background' || state === 'inactive') {
            this.endScreen();
            void c.flush();
          } else if (state === 'active') {
            if (this.lastScreenName && !this.screen) this.startScreen(this.lastScreenName, true);
            void c.drain();
          }
        }),
      ),
    );
    if (sub) this.teardown.push(() => sub.remove());
    void this.restoreIdentity();
    setTimeout(() => void c.drain(), 1000);
  }

  /** Persisted anonymous id, and the native SDK's session if it started first. */
  private async restoreIdentity(): Promise<void> {
    const c = this.client;
    const st = this.options.storage;
    if (st) {
      const stored = await st.getItem('trooper:anon').catch(() => null);
      if (stored) c.adoptIdentity({ anonymousId: stored });
      else await st.setItem('trooper:anon', c.anonymousId).catch(() => undefined);
    }
    const ctx = await callBridge(this.bridge?.getContext?.bind(this.bridge));
    if (ctx && (ctx.session_id || ctx.anonymous_id)) c.adoptIdentity({ sessionId: ctx.session_id, anonymousId: ctx.anonymous_id });
    this.syncNative();
  }

  syncNative(): void {
    const b = this.bridge;
    if (!b?.setContext) return;
    const c = this.client;
    const ctx: NativeContext = {
      session_id: c.sessions.peek()?.id,
      anonymous_id: c.anonymousId,
      view_id: c.view?.id,
      view_name: c.view?.name,
      user_id: c.user?.id,
    };
    void callBridge(() => b.setContext!(ctx));
  }

  private instrumentErrors(): void {
    const EU = (globalThis as { ErrorUtils?: ErrorUtilsLike }).ErrorUtils;
    if (!EU?.getGlobalHandler || !EU.setGlobalHandler) return;
    const prev = EU.getGlobalHandler();
    const handler = (error: unknown, isFatal?: boolean) => {
      safe('globalHandler', () => {
        this.captureError(error, { source: isFatal ? 'crash' : 'source', isCrash: !!isFatal });
        // The app is about to die: send now (no beacon on RN; fetch is fine).
        if (isFatal) void this.client.flush();
      });
      prev?.(error, isFatal);
    };
    EU.setGlobalHandler(handler);
    this.teardown.push(() => {
      if (EU.getGlobalHandler() === handler) EU.setGlobalHandler(prev);
    });
  }

  private instrumentRejections(): void {
    const hermes = (globalThis as { HermesInternal?: { enablePromiseRejectionTracker?: (o: unknown) => void } }).HermesInternal;
    if (!hermes?.enablePromiseRejectionTracker) return;
    safe('rejectionTracker', () =>
      hermes.enablePromiseRejectionTracker!({
        allRejections: true,
        onUnhandled: (_id: number, rejection: unknown) => this.captureError(rejection, { source: 'promise', isCrash: false }),
        onHandled: () => undefined,
      }),
    );
  }

  captureError(err: unknown, opts: CaptureErrorOptions = {}): void {
    this.client.capture({
      type: 'ERROR',
      error: { ...toErrorPayload(err), source: opts.source ?? 'source', is_crash: !!opts.isCrash },
      attributes: toStringMap(opts.attributes, 4000),
    });
  }

  /** Screen change: VIEW_END for the previous screen, VIEW_START (with time to next frame) for the new one. */
  startScreen(name: string, resumed = false): void {
    if (!resumed && this.screen?.name === name) return;
    this.endScreen();
    const s: Screen = { id: uuid(), name, start: Date.now(), emitted: false, initial: this.firstScreen };
    this.firstScreen = false;
    this.screen = s;
    this.lastScreenName = name;
    this.client.view = { id: s.id, name };
    this.syncNative();
    raf(() => {
      s.loading = Date.now() - s.start;
      this.emitScreenStart(s);
    });
  }

  private emitScreenStart(s: Screen): void {
    if (s.emitted) return;
    s.emitted = true;
    this.client.capture({
      type: 'VIEW_START',
      ts_ms: s.start,
      view_id: s.id,
      view_name: s.name,
      view: { loading_time_ms: s.loading, is_initial: s.initial },
    });
  }

  endScreen(): void {
    const s = this.screen;
    if (!s) return;
    this.screen = undefined;
    this.emitScreenStart(s);
    this.client.capture({
      type: 'VIEW_END',
      view_id: s.id,
      view_name: s.name,
      view: { time_spent_ms: Math.max(0, Date.now() - s.start) },
    });
  }

  /** First screen rendered: APP_START (cold). Uses the native process start time when the bridge provides it. */
  async appReady(): Promise<void> {
    if (this.appStartReported) return;
    this.appStartReported = true;
    const ready = Date.now();
    const processStart = await callBridge(this.bridge?.getProcessStartTime?.bind(this.bridge));
    const fromProcess = typeof processStart === 'number' && processStart > 0 && processStart <= ready;
    const start = fromProcess ? processStart : JS_START;
    this.client.capture({
      type: 'APP_START',
      ts_ms: ready,
      app_start: { kind: 'cold', duration_ms: ready - start },
      attributes: { start_reference: fromProcess ? 'process' : 'js_init' },
    });
  }

  close(): Promise<void> {
    this.endScreen();
    for (const f of this.teardown.splice(0).reverse()) safe('teardown', f);
    return this.client.close();
  }
}

interface ErrorUtilsLike {
  getGlobalHandler(): (error: unknown, isFatal?: boolean) => void;
  setGlobalHandler(fn: (error: unknown, isFatal?: boolean) => void): void;
}

const isDev = (): boolean => (globalThis as { __DEV__?: boolean }).__DEV__ === true;

export interface CaptureErrorOptions {
  source?: string;
  isCrash?: boolean;
  attributes?: Record<string, unknown>;
}

// ---------------------------------------------------------------------------------------------
// Public singleton API — never throws.
// ---------------------------------------------------------------------------------------------

let sdk: ReactNativeSdk | undefined;

export function init(options: ReactNativeOptions): ReactNativeSdk | undefined {
  if (sdk) return sdk;
  try {
    const s = new ReactNativeSdk(options);
    sdk = s;
    if (s.client.enabled) s.start();
    return s;
  } catch (e) {
    reportInternal('init', e);
    if (options?.debug) console.warn('[trooper] init failed', e);
    return undefined;
  }
}

export const getSdk = (): ReactNativeSdk | undefined => sdk;

export function identify(userId: string, traits?: Record<string, unknown>): void {
  safe('identify', () => {
    sdk?.client.setUser(userId, traits);
    sdk?.syncNative();
  });
}

export function clearUser(): void {
  safe('clearUser', () => {
    sdk?.client.setUser(undefined);
    sdk?.client.sessions.reset();
    sdk?.syncNative();
  });
}

export function track(name: string, props?: Record<string, unknown>, value?: number): void {
  safe('track', () => sdk?.client.capture({ type: 'CUSTOM', custom: { name: String(name), props: toStringMap(props), value } }));
}

export function captureError(err: unknown, opts?: CaptureErrorOptions): void {
  safe('captureError', () => sdk?.captureError(err, opts));
}

/** Manually report a screen (when not using react-navigation). */
export function setScreen(name: string): void {
  safe('setScreen', () => sdk?.startScreen(String(name)));
}

/** Mark the app as ready (first screen visible) when not using `trooperNavigation`. Reports APP_START once. */
export function markAppReady(): void {
  safe('markAppReady', () => void sdk?.appReady());
}

/** Replace the native bridge at runtime (e.g. a custom native module name). */
export function setNativeBridge(bridge: NativeBridge | undefined): void {
  safe('setNativeBridge', () => {
    if (!sdk) return;
    (sdk as { bridge: NativeBridge | undefined }).bridge = bridge;
    sdk.syncNative();
  });
}

interface NavigationRefLike {
  getCurrentRoute(): { name: string } | undefined;
  isReady?(): boolean;
}

/**
 * react-navigation integration:
 * ```tsx
 * const navigationRef = createNavigationContainerRef();
 * const nav = trooperNavigation(navigationRef);
 * <NavigationContainer ref={navigationRef} onReady={nav.onReady} onStateChange={nav.onStateChange}>
 * ```
 * Screen names become view names; `onReady` also reports APP_START.
 */
export function trooperNavigation(ref: NavigationRefLike) {
  const report = () =>
    safe('navigation', () => {
      const name = ref.getCurrentRoute()?.name;
      if (name) sdk?.startScreen(name);
    });
  return {
    onReady: () => {
      report();
      safe('appReady', () => void sdk?.appReady());
    },
    onStateChange: () => report(),
  };
}

export function flush(): Promise<void> {
  return sdk ? sdk.client.flush().catch(() => undefined) : Promise.resolve();
}

export async function close(): Promise<void> {
  const s = sdk;
  sdk = undefined;
  if (s) await s.close().catch((e) => reportInternal('close', e));
}

export { NATIVE_CHANNEL, NATIVE_MODULE_NAME, type NativeBridge, type NativeContext } from './bridge';
export type { KeyValueStorage, PropagationTarget, RumEvent } from '@trooper/core';
