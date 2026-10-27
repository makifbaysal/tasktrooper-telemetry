"""Names shared with the other TaskTrooper SDKs and the writer (.ai/contracts.md §2, §4, §25)."""

from ._version import __version__

VERSION = __version__
#: Reported as ``telemetry.distro.name`` (.ai/contracts.md §2).
DISTRO_NAME = "trooper-python"
#: Instrumentation scope of spans created by this package.
SCOPE_NAME = "tasktrooper"
#: Meter of the SDK's own metrics (``trooper.sdk.errors``).
SELF_METER = "TaskTrooper.Telemetry"

ATTR_ROUTE = "trooper.route"
ATTR_ENTRY_ROUTE = "trooper.entry.route"
ATTR_ENTRY_METHOD = "trooper.entry.method"
ATTR_PROCESS = "trooper.process"
ATTR_CALLER = "trooper.caller"
ATTR_CONSUMER_ATTEMPT = "trooper.consumer.attempt"
ATTR_CONSUMER_DLQ = "trooper.consumer.dlq"
ATTR_SAMPLE_RATE = "trooper.sample_rate"
#: Time to first token of a streamed LLM call, ms (double).
ATTR_LLM_TTFT_MS = "trooper.llm.ttft_ms"

#: W3C baggage member carrying the calling service name.
BAGGAGE_SERVICE_KEY = "tt.svc"
#: W3C tracestate list member carrying the root's head sampling rate: ``tt=r:<rate>``.
TRACESTATE_KEY = "tt"
#: Message header carrying the delivery attempt across retry topics / re-publishes.
ATTEMPT_HEADER = "tt-attempt"

METHOD_CONSUME = "CONSUME"
METHOD_GRPC = "GRPC"
