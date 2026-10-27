"""W3C trace context + baggage with ``tt.svc=<service>`` (.ai/contracts.md §4: ``trooper.caller``)."""

from __future__ import annotations

from typing import Optional, Set

from opentelemetry import baggage
from opentelemetry.baggage.propagation import W3CBaggagePropagator
from opentelemetry.context import Context
from opentelemetry.propagators.composite import CompositePropagator
from opentelemetry.propagators.textmap import (
    CarrierT,
    Getter,
    Setter,
    TextMapPropagator,
    default_getter,
    default_setter,
)
from opentelemetry.trace.propagation.tracecontext import TraceContextTextMapPropagator

from ._attrs import BAGGAGE_SERVICE_KEY
from ._errors import record_error


class TrooperBaggagePropagator(TextMapPropagator):
    """W3C baggage propagator that sets ``tt.svc=<service>`` on every injection.

    The value received from an upstream caller is overridden. The receiving trooper SDK records it as
    ``trooper.caller``.
    """

    def __init__(self, service: Optional[str]) -> None:
        self._service = service
        self._inner = W3CBaggagePropagator()

    def extract(
        self, carrier: CarrierT, context: Optional[Context] = None, getter: Getter = default_getter
    ) -> Context:
        return self._inner.extract(carrier, context=context, getter=getter)

    def inject(
        self, carrier: CarrierT, context: Optional[Context] = None, setter: Setter = default_setter
    ) -> None:
        ctx = context
        if self._service:
            try:
                ctx = baggage.set_baggage(BAGGAGE_SERVICE_KEY, self._service, context=context)
            except Exception as err:  # noqa: BLE001
                record_error("propagator", err)
        self._inner.inject(carrier, context=ctx, setter=setter)

    @property
    def fields(self) -> Set[str]:
        return self._inner.fields


def create_propagator(service: Optional[str]) -> TextMapPropagator:
    """W3C tracecontext + baggage with ``tt.svc``."""
    return CompositePropagator([TraceContextTextMapPropagator(), TrooperBaggagePropagator(service)])


def caller_from_context(ctx: Optional[Context]) -> Optional[str]:
    """Calling service from the W3C baggage member ``tt.svc``."""
    try:
        v = baggage.get_baggage(BAGGAGE_SERVICE_KEY, ctx)
    except Exception:  # noqa: BLE001
        return None
    return str(v) if v else None
