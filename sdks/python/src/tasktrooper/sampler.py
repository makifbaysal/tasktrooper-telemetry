"""ParentBased(TraceIdRatioBased) sampler that records the root's rate in the W3C tracestate (.ai/contracts.md §4)."""

from __future__ import annotations

import math
from typing import Optional, Sequence

from opentelemetry import trace
from opentelemetry.context import Context
from opentelemetry.sdk.trace.sampling import ParentBased, Sampler, SamplingResult, TraceIdRatioBased
from opentelemetry.trace import Link, SpanKind
from opentelemetry.trace.span import TraceState
from opentelemetry.util.types import Attributes

from ._attrs import TRACESTATE_KEY
from ._errors import record_error


def format_rate(r: float) -> str:
    """``1.0`` → ``1``, ``0.25`` → ``0.25`` (same text as the Go/Node SDKs)."""
    if float(r).is_integer():
        return str(int(r))
    return repr(float(r))


def rate_from_trace_state(ts: Optional[TraceState]) -> Optional[float]:
    """Parses ``tt=r:<rate>`` (sub-fields separated by ``;``)."""
    if ts is None:
        return None
    try:
        v = ts.get(TRACESTATE_KEY)
    except Exception:  # noqa: BLE001
        return None
    if not v:
        return None
    for f in v.split(";"):
        if f.startswith("r:"):
            try:
                r = float(f[2:])
            except ValueError:
                continue
            if math.isfinite(r) and 0 < r <= 1:
                return r
    return None


class TrooperSampler(Sampler):
    """``ParentBased(TraceIdRatioBased(rate))`` that writes ``tt=r:<rate>`` into the tracestate of root spans.

    Downstream entry spans read it so ``trooper.sample_rate`` reflects the rate that actually decided the trace.
    """

    def __init__(self, rate: float = 1.0) -> None:
        self.rate = rate if isinstance(rate, (int, float)) and 0 < rate <= 1 else 1.0
        self._inner = ParentBased(root=TraceIdRatioBased(self.rate))

    def should_sample(
        self,
        parent_context: Optional[Context],
        trace_id: int,
        name: str,
        kind: Optional[SpanKind] = None,
        attributes: Attributes = None,
        links: Optional[Sequence[Link]] = None,
        trace_state: Optional[TraceState] = None,
    ) -> SamplingResult:
        res = self._inner.should_sample(parent_context, trace_id, name, kind, attributes, links, trace_state)
        try:
            parent = trace.get_current_span(parent_context).get_span_context()
            if parent is None or not parent.is_valid:
                ts = res.trace_state if res.trace_state is not None else TraceState()
                value = "r:" + format_rate(self.rate)
                ts = ts.update(TRACESTATE_KEY, value) if TRACESTATE_KEY in ts else ts.add(TRACESTATE_KEY, value)
                return SamplingResult(res.decision, res.attributes, ts)
        except Exception as err:  # noqa: BLE001
            record_error("sampler", err)
        return res

    def get_description(self) -> str:
        return f"TrooperSampler{{rate={self.rate}}}"
