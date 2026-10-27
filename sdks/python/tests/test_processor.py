"""Sampler, propagation and span processor: the TaskTrooper conventions of .ai/contracts.md §4."""

from opentelemetry import baggage, context, propagate, trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter
from opentelemetry.trace import NonRecordingSpan, SpanContext, SpanKind, TraceFlags
from opentelemetry.trace.span import TraceState

import tasktrooper
from tasktrooper.processor import TrooperSpanProcessor
from tasktrooper.sampler import TrooperSampler, format_rate, rate_from_trace_state

from .conftest import by_name


def _remote(ts=None, sampled=True):
    sc = SpanContext(
        trace_id=0x9 << 64, span_id=0x9, is_remote=True,
        trace_flags=TraceFlags(TraceFlags.SAMPLED if sampled else 0), trace_state=ts or TraceState(),
    )
    return trace.set_span_in_context(NonRecordingSpan(sc))


def test_root_writes_tracestate(spans, tracer):
    with tracer.start_as_current_span("root", kind=SpanKind.SERVER) as root:
        pass
    assert root.get_span_context().trace_state.get("tt") == "r:1"


def test_tracestate_rate_text():
    assert format_rate(1.0) == "1" and format_rate(0.25) == "0.25"
    assert rate_from_trace_state(TraceState([("tt", "x:1;r:0.5")])) == 0.5
    assert rate_from_trace_state(TraceState([("tt", "r:7")])) is None
    assert rate_from_trace_state(None) is None


def test_sampled_root_and_drop():
    exp = InMemorySpanExporter()
    tp = TracerProvider(sampler=TrooperSampler(0.0000001))
    tp.add_span_processor(TrooperSpanProcessor(0.0000001, [SimpleSpanProcessor(exp)]))
    t = tp.get_tracer("t")
    for i in range(20):
        with t.start_as_current_span(f"r{i}", kind=SpanKind.SERVER):
            pass
    assert len(exp.get_finished_spans()) < 20
    # parent-based: a sampled remote parent is followed regardless of the local rate
    with t.start_as_current_span("child-of-remote", context=_remote(), kind=SpanKind.SERVER) as s:
        pass
    assert s.get_span_context().trace_flags.sampled
    span = [x for x in exp.get_finished_spans() if x.name == "child-of-remote"][0]
    assert span.attributes["trooper.sample_rate"] == 0.0000001  # no tt in tracestate → own rate


def test_sample_rate_from_upstream(spans, tracer):
    """Port of sdks/go contract_test.go TestSampleRateTraceState."""
    tracer.start_span("upstream", context=_remote(TraceState([("tt", "r:0.25"), ("other", "x")])), kind=SpanKind.CONSUMER).end()
    tracer.start_span("no-tracestate", context=_remote(), kind=SpanKind.CONSUMER).end()
    with tracer.start_as_current_span("root", kind=SpanKind.SERVER):
        pass
    assert by_name(spans, "upstream").attributes["trooper.sample_rate"] == 0.25
    assert by_name(spans, "no-tracestate").attributes["trooper.sample_rate"] == 1.0
    assert by_name(spans, "root").attributes["trooper.sample_rate"] == 1.0


def test_entry_route_propagates_to_children(spans, tracer):
    with tracer.start_as_current_span("GET", kind=SpanKind.SERVER, attributes={"http.request.method": "get"}) as srv:
        with tracer.start_as_current_span("db", kind=SpanKind.CLIENT):
            with tracer.start_as_current_span("nested"):
                srv.set_attribute("http.route", "/users/{user_id}")  # resolved after the children started
        with tracer.start_as_current_span("late"):
            pass
    root = by_name(spans, "GET")
    assert root.attributes["trooper.route"] == "/users/:user_id"
    assert root.attributes["trooper.entry.route"] == "/users/:user_id"
    assert root.attributes["trooper.entry.method"] == "GET"
    for n in ("db", "nested", "late"):
        a = by_name(spans, n).attributes
        assert a["trooper.entry.route"] == "/users/:user_id", n
        assert a["trooper.entry.method"] == "GET"
    assert tasktrooper.entry.tracked_spans() == 0


def test_set_route(spans, tracer):
    with tracer.start_as_current_span("srv", kind=SpanKind.SERVER, attributes={"http.method": "POST"}):
        assert tasktrooper.set_route("/orders/{id}/pay")
        with tracer.start_as_current_span("work"):
            pass
    root = by_name(spans, "srv")
    assert root.attributes["trooper.route"] == "/orders/:id/pay"
    assert root.attributes["http.route"] == "/orders/{id}/pay"
    assert by_name(spans, "work").attributes["trooper.entry.route"] == "/orders/:id/pay"
    assert tasktrooper.set_route("/x") is False  # no active entry


def test_non_entry_roots_untouched(spans, tracer):
    with tracer.start_as_current_span("job"):
        with tracer.start_as_current_span("step"):
            pass
    for s in spans.get_finished_spans():
        assert not any(k.startswith("trooper.") for k in s.attributes)


def test_remote_parent_server_is_entry_local_child_server_is_not(spans, tracer):
    with tracer.start_as_current_span("in", context=_remote(), kind=SpanKind.SERVER, attributes={"http.route": "/a/<int:id>", "http.method": "GET"}):
        with tracer.start_as_current_span("inner-server", kind=SpanKind.SERVER):
            pass
    assert by_name(spans, "in").attributes["trooper.route"] == "/a/:id"
    inner = by_name(spans, "inner-server").attributes
    assert "trooper.route" not in inner and inner["trooper.entry.route"] == "/a/:id"


def test_consumer_process(spans, tracer):
    attrs = {"messaging.system": "kafka", "messaging.destination.name": "orders", "messaging.consumer.group.name": "billing"}
    with tracer.start_as_current_span("orders process", kind=SpanKind.CONSUMER, attributes=attrs):
        with tracer.start_as_current_span("handle"):
            pass
    with tracer.start_as_current_span("run/send_email", kind=SpanKind.CONSUMER, attributes={"celery.task_name": "app.send_email", "celery.retries": 2}):
        pass
    c = by_name(spans, "orders process").attributes
    assert c["trooper.process"] == "billing"
    assert (c["trooper.entry.route"], c["trooper.entry.method"]) == ("billing", "CONSUME")
    assert by_name(spans, "handle").attributes["trooper.entry.route"] == "billing"
    cel = by_name(spans, "run/send_email").attributes
    assert cel["trooper.process"] == "app.send_email" and cel["trooper.consumer.attempt"] == 3


def test_grpc_entry(spans, tracer):
    with tracer.start_as_current_span("rpc", kind=SpanKind.SERVER, attributes={"rpc.system": "grpc", "rpc.service": "pkg.Orders", "rpc.method": "Get"}):
        pass
    a = by_name(spans, "rpc").attributes
    assert (a["trooper.route"], a["trooper.entry.method"]) == ("/pkg.Orders/Get", "GRPC")


def test_caller_from_baggage_and_outgoing_baggage(spans, tracer):
    incoming = {"traceparent": "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01", "baggage": "tt.svc=checkout-web,foo=bar"}
    token = context.attach(propagate.extract(incoming))  # what server instrumentations do
    try:
        with tracer.start_as_current_span("api", kind=SpanKind.SERVER):
            out = {}
            propagate.inject(out)
    finally:
        context.detach(token)
    assert by_name(spans, "api").attributes["trooper.caller"] == "checkout-web"
    assert "tt.svc=test-svc" in out["baggage"] and "foo=bar" in out["baggage"]
    assert "checkout-web" not in out["baggage"]
    assert out["traceparent"].startswith("00-0af7651916cd43dd8448eb211c80319c-")
    # no baggage → no caller
    with tracer.start_as_current_span("api2", context=propagate.extract({"traceparent": incoming["traceparent"]}), kind=SpanKind.SERVER):
        pass
    assert "trooper.caller" not in by_name(spans, "api2").attributes


def test_inject_without_span_still_sets_service():
    out = {}
    propagate.inject(out, context=baggage.set_baggage("x", "1", context.Context()))
    assert "tt.svc=test-svc" in out["baggage"]


def test_query_values_stripped(spans, tracer):
    with tracer.start_as_current_span("c", kind=SpanKind.CLIENT, attributes={"url.full": "https://api.x/a?token=s3cret&b=2", "url.query": "q=1&r=2"}) as s:
        s.set_attribute("http.url", "https://api.x/late?pw=1")
    a = by_name(spans, "c").attributes
    assert a["url.full"] == "https://api.x/a?token&b"
    assert a["url.query"] == "q&r"
    assert a["http.url"] == "https://api.x/late?pw"


def test_processor_never_raises(spans):
    p = TrooperSpanProcessor(1.0, [])
    p.on_start(object())  # garbage in
    p.on_end(object())
    assert p.force_flush() is True
    p.shutdown()
    assert tasktrooper.internal_error_count() >= 2
