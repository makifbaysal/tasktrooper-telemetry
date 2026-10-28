"""The trooper span processor: TaskTrooper conventions on top of OTel spans (.ai/contracts.md §4)."""

from __future__ import annotations

import copy
from typing import Any, Dict, List, Optional, Sequence

from opentelemetry import context as otel_context
from opentelemetry.attributes import BoundedAttributes
from opentelemetry.sdk.trace import ReadableSpan, Span, SpanProcessor
from opentelemetry.trace import SpanKind

from ._attrs import (
    ATTR_CALLER,
    ATTR_CONSUMER_ATTEMPT,
    ATTR_ENTRY_METHOD,
    ATTR_ENTRY_ROUTE,
    ATTR_LLM_TTFT_MS,
    ATTR_PROCESS,
    ATTR_ROUTE,
    ATTR_SAMPLE_RATE,
    METHOD_CONSUME,
    METHOD_GRPC,
)
from ._errors import record_error
from .entry import Entry, bind_thread, entry_of_key, link_span, release_entry, span_key, unlink_span
from .propagation import caller_from_context
from .route import normalize_route, strip_query_string, strip_query_values
from .sampler import rate_from_trace_state

URL_ATTRS = ("url.full", "http.url", "http.target")


def _str(v: Any) -> Optional[str]:
    return v if isinstance(v, str) and v else None


def _scrubbed(attrs: Any) -> Dict[str, str]:
    """Query values never leave the process (AGENTS.md rule 6): keys are kept, values dropped."""
    out: Dict[str, str] = {}
    for k in URL_ATTRS:
        v = attrs.get(k)
        if isinstance(v, str) and "?" in v:
            s = strip_query_values(v)
            if s != v:
                out[k] = s
    q = attrs.get("url.query")
    if isinstance(q, str) and "=" in q:
        out["url.query"] = strip_query_string(q)
    return out


def process_of(name: str, attrs: Any) -> Optional[str]:
    """Consumer process: trooper.process → consumer group → Celery task → destination (→ span name)."""
    for k in (
        ATTR_PROCESS,
        "messaging.consumer.group.name",
        "messaging.kafka.consumer.group",
        "messaging.kafka.consumer_group",
        "celery.task_name",
        "messaging.destination.name",
        "messaging.destination",
    ):
        v = _str(attrs.get(k))
        if v:
            return v
    for suffix in (" process", " receive"):
        if name.endswith(suffix) and len(name) > len(suffix):
            return name[: -len(suffix)]
    return None


def _grpc_route(attrs: Any) -> Optional[str]:
    sys_ = _str(attrs.get("rpc.system"))
    if not sys_ or not sys_.lower().startswith("grpc"):
        return None
    svc = _str(attrs.get("rpc.service"))
    m = _str(attrs.get("rpc.method"))
    return f"/{svc}/{m}" if svc and m else None


def _http_method(attrs: Any) -> Optional[str]:
    m = _str(attrs.get("http.request.method")) or _str(attrs.get("http.method"))
    if not m:
        return None
    return m if m == "_OTHER" else m.upper()


def _attempt(attrs: Any) -> Optional[int]:
    for k in ("messaging.kafka.message.attempt", "celery.retries"):
        v = attrs.get(k)
        if isinstance(v, int) and not isinstance(v, bool) and v >= 0:
            return v + 1 if k == "celery.retries" else v
    return None


class TrooperSpanProcessor(SpanProcessor):
    """Adds the TaskTrooper conventions to spans, then hands them to the wrapped (exporting) processors.

    - entry (local-root SERVER / CONSUMER) spans: ``trooper.sample_rate`` (own rate for roots, upstream ``tt=r:`` rate
      for remote parents), ``trooper.caller`` from baggage ``tt.svc``, ``trooper.route`` (server, from ``http.route`` /
      :func:`tasktrooper.set_route`, normalized), ``trooper.process`` (consumer), ``trooper.entry.route/method``;
    - every span below an entry: ``trooper.entry.route`` / ``trooper.entry.method``;
    - URL attributes: query values stripped.

    The Python SDK freezes span attributes before ``on_end``, so end-time attributes are applied to a copy of the
    :class:`ReadableSpan` that is passed to the wrapped processors. Never raises.
    """

    def __init__(self, sample_rate: float = 1.0, delegates: Sequence[SpanProcessor] = ()) -> None:
        self.sample_rate = sample_rate if isinstance(sample_rate, (int, float)) and 0 < sample_rate <= 1 else 1.0
        self._delegates: List[SpanProcessor] = list(delegates)

    def add_delegate(self, p: SpanProcessor) -> None:
        self._delegates.append(p)

    # -- start -------------------------------------------------------------------------------------------------------

    def on_start(self, span: Span, parent_context: Optional[otel_context.Context] = None) -> None:
        try:
            self._start(span, parent_context)
        except Exception as err:  # noqa: BLE001
            record_error("span_processor", err)
        for d in self._delegates:
            try:
                d.on_start(span, parent_context=parent_context)
            except Exception as err:  # noqa: BLE001
                record_error("span_processor", err)

    def _start(self, span: Span, parent_context: Optional[otel_context.Context]) -> None:
        attrs = span.attributes
        for k, v in _scrubbed(attrs).items():
            span.set_attribute(k, v)
        parent = span.parent
        local_root = parent is None or not parent.is_valid or parent.is_remote
        key = span_key(span.get_span_context())
        if local_root:
            if span.kind not in (SpanKind.SERVER, SpanKind.CONSUMER):
                return
            has_parent = parent is not None and parent.is_valid
            remote_state = parent.trace_state if has_parent else None
            self._start_entry(span, parent_context, remote_state, has_parent, key)
            return
        e = entry_of_key(span_key(parent))
        if e is None:
            return
        link_span(key, e)
        if getattr(e.root, "end_time", None) is None:
            bind_thread(e)

    def _start_entry(self, span: Span, parent_context: Any, remote_state: Any, has_parent: bool, key: Any) -> None:
        consumer = span.kind == SpanKind.CONSUMER
        e = Entry(span, "consumer" if consumer else "server")
        rate = rate_from_trace_state(remote_state) if has_parent else None
        span.set_attribute(ATTR_SAMPLE_RATE, float(rate if rate is not None else self.sample_rate))
        ctx = parent_context if parent_context is not None else otel_context.get_current()
        caller = caller_from_context(ctx)
        if caller:
            span.set_attribute(ATTR_CALLER, caller)
        attrs = span.attributes
        if consumer:
            e.method = METHOD_CONSUME
            e.route = process_of(span.name, attrs)
            if e.route:
                span.set_attribute(ATTR_PROCESS, e.route)
        else:
            grpc = _grpc_route(attrs)
            if grpc:
                e.route = grpc
                e.method = METHOD_GRPC
            else:
                e.method = _http_method(attrs)
        link_span(key, e)
        bind_thread(e)

    # -- end ---------------------------------------------------------------------------------------------------------

    def _on_ending(self, span: Span) -> None:  # OTel ≥ 1.30 hook; forwarded only
        for d in self._delegates:
            f = getattr(d, "_on_ending", None)
            if f is not None:
                try:
                    f(span)
                except Exception as err:  # noqa: BLE001
                    record_error("span_processor", err)

    def on_end(self, span: ReadableSpan) -> None:
        out: ReadableSpan = span
        try:
            extra = self._end_attrs(span)
            if extra:
                out = with_attributes(span, extra)
        except Exception as err:  # noqa: BLE001
            record_error("span_processor", err)
        for d in self._delegates:
            try:
                d.on_end(out)
            except Exception as err:  # noqa: BLE001
                record_error("span_processor", err)

    def _end_attrs(self, span: ReadableSpan) -> Dict[str, Any]:
        attrs = span.attributes or {}
        extra: Dict[str, Any] = dict(_scrubbed(attrs))
        if "gen_ai.operation.name" in attrs:
            genai_aliases(attrs, extra)
        e = unlink_span(span_key(span.get_span_context()))
        if e is None:
            return extra
        if e.root.get_span_context().span_id == span.get_span_context().span_id:
            self._finish_entry(span, attrs, e, extra)
            release_entry(e)
        else:
            _entry_attrs(attrs, e, extra, overwrite=False)
        return extra

    def _finish_entry(self, span: ReadableSpan, attrs: Any, e: Entry, extra: Dict[str, Any]) -> None:
        if e.kind == "consumer":
            p = _str(attrs.get(ATTR_PROCESS)) or process_of(span.name, attrs)
            if p:
                e.route = p
                if attrs.get(ATTR_PROCESS) != p:
                    extra[ATTR_PROCESS] = p
            if attrs.get(ATTR_CONSUMER_ATTEMPT) is None:
                a = e.attempt if e.attempt is not None else _attempt(attrs)
                if a is not None:
                    extra[ATTR_CONSUMER_ATTEMPT] = a
        else:
            if not e.method:
                e.method = _http_method(attrs)
            raw, own = e.raw_route()
            if raw:
                route = raw if e.method == METHOD_GRPC else normalize_route(raw)
                if route:
                    extra[ATTR_ROUTE] = route
                    if own and attrs.get("http.route") is None and e.method != METHOD_GRPC:
                        extra["http.route"] = raw
        _entry_attrs(attrs, e, extra, overwrite=True)

    def shutdown(self) -> None:
        for d in self._delegates:
            try:
                d.shutdown()
            except Exception as err:  # noqa: BLE001
                record_error("span_processor", err)

    def force_flush(self, timeout_millis: int = 30000) -> bool:
        ok = True
        for d in self._delegates:
            try:
                ok = bool(d.force_flush(timeout_millis)) and ok
            except Exception as err:  # noqa: BLE001
                record_error("span_processor", err)
                ok = False
        return ok


def genai_aliases(attrs: Any, extra: Dict[str, Any]) -> None:
    """Maps GenAI spans emitted natively by provider SDKs (e.g. ``anthropic`` ≥ 2.x) to the names of §25."""
    v = attrs.get("gen_ai.usage.cache_write.input_tokens")
    if v is not None and attrs.get("gen_ai.usage.cache_creation.input_tokens") is None:
        extra["gen_ai.usage.cache_creation.input_tokens"] = v
    t = attrs.get("gen_ai.response.time_to_first_chunk")  # seconds
    if isinstance(t, (int, float)) and not isinstance(t, bool) and attrs.get(ATTR_LLM_TTFT_MS) is None:
        extra[ATTR_LLM_TTFT_MS] = float(t) * 1000.0
    stop = attrs.get("anthropic.message.stop_reason")
    if isinstance(stop, str) and stop and attrs.get("gen_ai.response.finish_reasons") is None:
        extra["gen_ai.response.finish_reasons"] = [stop]


def _entry_attrs(attrs: Any, e: Entry, extra: Dict[str, Any], overwrite: bool) -> None:
    if not overwrite and attrs.get(ATTR_ENTRY_ROUTE) is not None:
        return
    route = e.entry_route()
    if not route:
        return
    extra[ATTR_ENTRY_ROUTE] = route
    if e.method:
        extra[ATTR_ENTRY_METHOD] = e.method


def with_attributes(span: ReadableSpan, extra: Dict[str, Any]) -> ReadableSpan:
    """Copy of an ended span with ``extra`` merged into its attributes."""
    merged = dict(span.attributes or {})
    merged.update(extra)
    out = copy.copy(span)
    out._attributes = BoundedAttributes(maxlen=None, attributes=merged, immutable=True)  # noqa: SLF001
    return out

