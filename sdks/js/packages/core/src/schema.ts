import type * as T from './types';

/**
 * Runtime copy of the field names in `types.ts`, used only by tests to verify against rum.proto.
 * `exhaustive` makes the compiler reject a list that misses or invents a key of the TS interface.
 * Not exported from the package entry point (tree-shaken away).
 */
type Keys<O> = readonly (keyof O)[];
const exhaustive =
  <O>() =>
  <const A extends Keys<O>>(a: A & (Exclude<keyof O, A[number]> extends never ? unknown : never)) =>
    a as readonly string[];

export const FIELDS: Record<string, readonly string[]> = {
  RumBatch: exhaustive<T.RumBatch>()(['resource', 'events']),
  Resource: exhaustive<T.Resource>()(['app', 'version', 'environment', 'platform', 'sdk', 'device', 'os', 'browser', 'attributes']),
  Sdk: exhaustive<T.Sdk>()(['name', 'version']),
  Device: exhaustive<T.Device>()(['type', 'manufacturer', 'model', 'locale', 'screen_width', 'screen_height']),
  Os: exhaustive<T.Os>()(['name', 'version']),
  Browser: exhaustive<T.Browser>()(['name', 'version']),
  User: exhaustive<T.User>()(['id', 'traits']),
  RumEvent: exhaustive<T.RumEvent>()([
    'type', 'ts_ms', 'session_id', 'view_id', 'view_name', 'anonymous_id', 'user', 'connection',
    'view', 'vital', 'error', 'resource', 'action', 'app_start', 'custom', 'attributes',
  ]),
  ViewPayload: exhaustive<T.ViewPayload>()(['url', 'referrer', 'loading_time_ms', 'time_spent_ms', 'is_initial']),
  VitalPayload: exhaustive<T.VitalPayload>()(['name', 'value', 'rating']),
  ErrorPayload: exhaustive<T.ErrorPayload>()(['type', 'message', 'stack', 'source', 'is_crash', 'thread', 'fingerprint']),
  ResourcePayload: exhaustive<T.ResourcePayload>()([
    'url', 'method', 'status_code', 'duration_ms', 'request_bytes', 'response_bytes', 'trace_id', 'span_id', 'initiator',
  ]),
  ActionPayload: exhaustive<T.ActionPayload>()(['name', 'target', 'rage', 'dead']),
  AppStartPayload: exhaustive<T.AppStartPayload>()(['kind', 'duration_ms']),
  CustomPayload: exhaustive<T.CustomPayload>()(['name', 'props', 'value']),
};

export const ENUMS: Record<string, readonly string[]> = {
  Platform: [
    'PLATFORM_UNSPECIFIED', 'PLATFORM_WEB', 'PLATFORM_ANDROID', 'PLATFORM_IOS', 'PLATFORM_FLUTTER', 'PLATFORM_REACT_NATIVE',
  ] satisfies T.Platform[],
  EventType: [
    'EVENT_TYPE_UNSPECIFIED', 'SESSION_START', 'VIEW_START', 'VIEW_END', 'VITAL', 'ERROR', 'RESOURCE', 'ACTION', 'APP_START', 'CUSTOM',
  ] satisfies T.EventType[],
};
