import { backoffDelay, classifyStatus, reportInternal, safe, type Client, type RumEvent, type SendResult, type ViewRef } from '@trooper/core';

export type FetchLike = (input: string, init?: RequestInit) => Promise<Response>;

/**
 * What `@trooper/browser` hands to an integration (e.g. `@trooper/replay`). Every function is safe to call and never
 * throws. Integrations must not crash the host either: wrap work in try/catch and use `reportInternal`.
 */
export interface IntegrationClient {
  readonly config: {
    dsn: string;
    /** Ingest origin from the DSN, e.g. `https://ingest.example.com` (no trailing slash). */
    endpoint: string;
    key: string;
    projectId: string;
    /** Client app name (`init({ app })`). */
    service: string;
    environment?: string;
    version?: string;
    debug: boolean;
  };
  /** Current session id (does not count as user activity; creates a session if none exists yet). */
  sessionId(): string;
  /** RUM sampling decision of the current session. */
  sampled(): boolean;
  anonymousId(): string;
  userId(): string | undefined;
  /** Current view (id + name). */
  view(): ViewRef | undefined;
  /** Called with the new session id when the session rotates (30 min idle / 4 h / `clearUser`). Returns unsubscribe. */
  onSessionStart(fn: (sessionId: string) => void): () => void;
  /** Called for every RUM event accepted for delivery (ERROR, ACTION with `action.rage`, …). Returns unsubscribe. */
  onEvent(fn: (event: RumEvent) => void): () => void;
  /** `fetch` captured before the SDK instrumented it (requests are never reported as RESOURCE events). */
  fetch: FetchLike | undefined;
  /** 1 s → 60 s exponential backoff with jitter (contracts.md §2). */
  backoffDelay(attempt: number): number;
  /** HTTP status → ok / retry (429, 5xx, network) / drop (other 4xx). */
  classifyStatus(status: number, retryAfter?: string | null): SendResult;
  /** Count (and log in debug mode) a swallowed internal error. */
  reportInternal(where: string, err: unknown): void;
}

export interface Integration {
  /** Unique name; a second integration with the same name is ignored. */
  name: string;
  /** Called once when the SDK starts (only when enabled). May return a teardown run by `close()`. */
  setup(client: IntegrationClient): void | (() => void);
}

/** Fan-out hub for session/event notifications, created before the core client so its callbacks can reach it. */
export class Hub {
  readonly session = new Set<(id: string) => void>();
  readonly event = new Set<(e: RumEvent) => void>();

  emitSession = (id: string): void => this.session.forEach((f) => safe('integration.onSessionStart', () => f(id)));
  emitEvent = (e: RumEvent): void => this.event.forEach((f) => safe('integration.onEvent', () => f(e)));
}

const sub = <T>(set: Set<T>, fn: T): (() => void) => {
  set.add(fn);
  return () => set.delete(fn);
};

export function integrationClient(c: Client, hub: Hub, fetchImpl: FetchLike | undefined): IntegrationClient {
  const o = c.config;
  return {
    config: {
      dsn: o.dsn, endpoint: c.dsn.origin, key: c.dsn.key, projectId: c.dsn.projectId,
      service: o.app, environment: o.environment, version: o.version, debug: !!o.debug,
    },
    sessionId: () => c.sessions.peek()?.id || c.sessionId(),
    sampled: () => c.isSampled(),
    anonymousId: () => c.anonymousId,
    userId: () => c.user?.id,
    view: () => c.view,
    onSessionStart: (fn) => sub(hub.session, fn),
    onEvent: (fn) => sub(hub.event, fn),
    fetch: fetchImpl,
    backoffDelay: (n) => backoffDelay(n),
    classifyStatus,
    reportInternal,
  };
}
