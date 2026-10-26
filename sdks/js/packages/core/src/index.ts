export type * from './types';
export { Client, MAX_BATCH, type BeforeSend, type ClientConfig, type CoreOptions, type EventInput, type ViewRef } from './client';
export { parseDsn, type Dsn } from './dsn';
export { hashToUnit, randomHex, uuid } from './ids';
export { bodySize, instrumentFetch, instrumentXhr, type NetworkInstrumentationOptions } from './network';
export { KeyValueOfflineStore, MemoryOfflineStore, OFFLINE_MAX_BYTES, type KeyValueStorage, type OfflineStore } from './offline';
export { SESSION_IDLE_MS, SESSION_MAX_MS, SessionManager, type SessionState, type SessionStore, type SessionTouch } from './session';
export {
  absoluteUrl,
  BAGGAGE_MAX_BYTES,
  BAGGAGE_SERVICE_KEY,
  createTraceContext,
  mergeBaggage,
  shouldPropagate,
  type PropagationTarget,
  type TraceContext,
} from './trace';
export {
  BACKOFF_MAX_MS,
  BACKOFF_MIN_MS,
  backoffDelay,
  classifyStatus,
  FetchTransport,
  KEEPALIVE_LIMIT,
  parseRetryAfter,
  type FetchTransportOptions,
  type SendOptions,
  type SendResult,
  type Transport,
} from './transport';
export { normalizePath, stripQueryValues } from './url';
export { byteLength, guard, internalErrorCount, now, reportInternal, safe, setDebug, toStringMap, truncate } from './util';

export const CORE_VERSION = '0.1.0';
