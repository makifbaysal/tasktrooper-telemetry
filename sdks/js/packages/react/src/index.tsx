import { Component, createContext, useContext, useState, type ErrorInfo, type ReactNode } from 'react';
import {
  captureError,
  clearUser,
  flush,
  identify,
  init,
  setView,
  track,
  type BrowserOptions,
  type CaptureErrorOptions,
} from '@trooper/browser';

export interface TrooperApi {
  track: typeof track;
  identify: typeof identify;
  clearUser: typeof clearUser;
  captureError: typeof captureError;
  setView: typeof setView;
  flush: typeof flush;
}

const api: TrooperApi = { track, identify, clearUser, captureError, setView, flush };
const TrooperContext = createContext<TrooperApi>(api);

export interface TrooperProviderProps {
  /** Calls `init(options)` once on the client (no-op during SSR). Omit if you call `init()` yourself. */
  options?: BrowserOptions;
  children?: ReactNode;
}

/** Initializes the SDK (once) and exposes the API through `useTrooper()`. */
export function TrooperProvider({ options, children }: TrooperProviderProps) {
  // Lazy state initializer: runs once, during the first render, so errors in the first render are captured.
  useState(() => (options ? init(options) : undefined));
  return <TrooperContext.Provider value={api}>{children}</TrooperContext.Provider>;
}

/** Access `track`, `identify`, `captureError`, `setView`... Works with or without a provider. */
export const useTrooper = (): TrooperApi => useContext(TrooperContext);

export interface FallbackProps {
  error: unknown;
  resetError: () => void;
}

export interface ErrorBoundaryProps {
  children?: ReactNode;
  fallback?: ReactNode | ((props: FallbackProps) => ReactNode);
  /** Added as the `error_boundary` attribute to distinguish boundaries. */
  name?: string;
  onError?: (error: unknown, componentStack: string | undefined) => void;
  /** Mark captured errors as crashes (default false: the boundary handled them). */
  isCrash?: boolean;
}

interface ErrorBoundaryState {
  error: unknown;
  hasError: boolean;
}

/** Captures render errors (with the React component stack) and renders `fallback`. */
export class ErrorBoundary extends Component<ErrorBoundaryProps, ErrorBoundaryState> {
  override state: ErrorBoundaryState = { error: undefined, hasError: false };

  static getDerivedStateFromError(error: unknown): ErrorBoundaryState {
    return { error, hasError: true };
  }

  override componentDidCatch(error: unknown, info: ErrorInfo): void {
    const stack = info?.componentStack ?? undefined;
    captureError(error, {
      source: 'source',
      isCrash: !!this.props.isCrash,
      attributes: { component_stack: stack, error_boundary: this.props.name },
    });
    try {
      this.props.onError?.(error, stack);
    } catch {
      /* host callback errors must not break the boundary */
    }
  }

  resetError = (): void => this.setState({ error: undefined, hasError: false });

  override render(): ReactNode {
    if (!this.state.hasError) return this.props.children;
    const { fallback } = this.props;
    return typeof fallback === 'function' ? fallback({ error: this.state.error, resetError: this.resetError }) : (fallback ?? null);
  }
}

/**
 * React 19 root error callbacks: `createRoot(el, { onUncaughtError: reactErrorHandler('uncaught'), onCaughtError: reactErrorHandler('caught') })`.
 * Captures errors React handles internally (with component stack). `then` lets you keep React's default logging.
 */
export function reactErrorHandler(
  kind: 'uncaught' | 'caught' | 'recoverable' = 'uncaught',
  then?: (error: unknown, info: { componentStack?: string }) => void,
) {
  return (error: unknown, info: { componentStack?: string }): void => {
    const opts: CaptureErrorOptions = {
      source: 'source',
      isCrash: kind === 'uncaught',
      attributes: { component_stack: info?.componentStack, react_error_kind: kind },
    };
    captureError(error, opts);
    then?.(error, info);
  };
}

export { captureError, clearUser, flush, identify, init, setView, track } from '@trooper/browser';
export type { BrowserOptions, CaptureErrorOptions } from '@trooper/browser';
