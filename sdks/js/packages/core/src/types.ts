/**
 * Hand-written mirror of `proto/rum/v1/rum.proto` (package trooper.rum.v1).
 * JSON mapping: snake_case proto field names, enums as their string names.
 * `test/proto.test.ts` parses the .proto and fails if these drift — keep `FIELDS` below in sync.
 */

export type Platform =
  | 'PLATFORM_UNSPECIFIED'
  | 'PLATFORM_WEB'
  | 'PLATFORM_ANDROID'
  | 'PLATFORM_IOS'
  | 'PLATFORM_FLUTTER'
  | 'PLATFORM_REACT_NATIVE';

export type EventType =
  | 'EVENT_TYPE_UNSPECIFIED'
  | 'SESSION_START'
  | 'VIEW_START'
  | 'VIEW_END'
  | 'VITAL'
  | 'ERROR'
  | 'RESOURCE'
  | 'ACTION'
  | 'APP_START'
  | 'CUSTOM';

export type StringMap = Record<string, string>;

export interface Sdk {
  name: string;
  version: string;
}

export interface Device {
  type?: string;
  manufacturer?: string;
  model?: string;
  locale?: string;
  screen_width?: number;
  screen_height?: number;
}

export interface Os {
  name?: string;
  version?: string;
}

export interface Browser {
  name?: string;
  version?: string;
}

export interface Resource {
  app: string;
  version?: string;
  environment?: string;
  platform: Platform;
  sdk: Sdk;
  device?: Device;
  os?: Os;
  browser?: Browser;
  attributes?: StringMap;
}

export interface User {
  id?: string;
  traits?: StringMap;
}

export interface ViewPayload {
  url?: string;
  referrer?: string;
  loading_time_ms?: number;
  time_spent_ms?: number;
  is_initial?: boolean;
}

export interface VitalPayload {
  name: string;
  value: number;
  rating?: string;
}

export interface ErrorPayload {
  type?: string;
  message?: string;
  stack?: string;
  source?: string;
  is_crash?: boolean;
  thread?: string;
  fingerprint?: string;
}

export interface ResourcePayload {
  url?: string;
  method?: string;
  status_code?: number;
  duration_ms?: number;
  request_bytes?: number;
  response_bytes?: number;
  trace_id?: string;
  span_id?: string;
  initiator?: string;
}

export interface ActionPayload {
  name?: string;
  target?: string;
  rage?: boolean;
  dead?: boolean;
}

export interface AppStartPayload {
  kind?: string;
  duration_ms?: number;
}

export interface CustomPayload {
  name: string;
  props?: StringMap;
  value?: number;
}

export interface RumEvent {
  type: EventType;
  ts_ms: number;
  session_id: string;
  view_id?: string;
  view_name?: string;
  anonymous_id: string;
  user?: User;
  connection?: string;
  // oneof payload
  view?: ViewPayload;
  vital?: VitalPayload;
  error?: ErrorPayload;
  resource?: ResourcePayload;
  action?: ActionPayload;
  app_start?: AppStartPayload;
  custom?: CustomPayload;
  attributes?: StringMap;
}

export interface RumBatch {
  resource: Resource;
  events: RumEvent[];
}

/** Payload keys of RumEvent (proto `oneof payload`). */
export type PayloadKey = 'view' | 'vital' | 'error' | 'resource' | 'action' | 'app_start' | 'custom';
