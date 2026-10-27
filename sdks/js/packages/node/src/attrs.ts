/** Version of the trooper-node distribution (telemetry.distro.version). */
export const VERSION = '0.1.0';
/** Reported as telemetry.distro.name (.ai/contracts.md §2). */
export const DISTRO_NAME = 'trooper-node';
/** Instrumentation scope of spans created by this package. */
export const SCOPE_NAME = '@trooper/node';
/** Meter of the SDK's own metrics (trooper.sdk.errors). */
export const SELF_METER = 'TaskTrooper.Telemetry';

/** Attribute keys of the TaskTrooper semantic conventions (.ai/contracts.md §4). */
export const ATTR_ROUTE = 'trooper.route';
export const ATTR_ENTRY_ROUTE = 'trooper.entry.route';
export const ATTR_ENTRY_METHOD = 'trooper.entry.method';
export const ATTR_PROCESS = 'trooper.process';
export const ATTR_CALLER = 'trooper.caller';
export const ATTR_CONSUMER_ATTEMPT = 'trooper.consumer.attempt';
export const ATTR_CONSUMER_DLQ = 'trooper.consumer.dlq';
export const ATTR_SAMPLE_RATE = 'trooper.sample_rate';

/** W3C baggage member carrying the calling service name. */
export const BAGGAGE_SERVICE_KEY = 'tt.svc';
/** W3C tracestate list member carrying the root's head sampling rate: `tt=r:<rate>`. */
export const TRACESTATE_KEY = 'tt';
/** Message header carrying the delivery attempt across retry topics / re-publishes (decimal string). */
export const ATTEMPT_HEADER = 'tt-attempt';

export const METHOD_CONSUME = 'CONSUME';
export const METHOD_GRPC = 'GRPC';
