"""Entry tracking: the HTTP route or consumer process a unit of work belongs to.

The trooper span processor creates an :class:`Entry` for every local-root SERVER / CONSUMER span. Every span started
below it is linked to it, so its route can be copied onto them as ``trooper.entry.route`` / ``trooper.entry.method``
even when the route template is resolved after the server span started. The profiler reads the per-thread map to
attribute stack samples to the entry route of the thread (.ai/contracts.md §24).
"""

from __future__ import annotations

import threading
from typing import Any, Callable, Dict, Optional, Tuple

from opentelemetry import context as otel_context
from opentelemetry import trace

from ._attrs import METHOD_GRPC
from ._errors import record_error
from .route import normalize_route

MAX_OPEN = 100_000

SpanKey = Tuple[int, int]


class Entry:
    __slots__ = ("root", "kind", "route", "method", "resolve", "attempt", "_norm_src", "_norm")

    def __init__(self, root: Any, kind: str) -> None:
        self.root = root
        #: ``server`` or ``consumer``.
        self.kind = kind
        #: Explicit route (``set_route``) or consumer process name.
        self.route: Optional[str] = None
        self.method: Optional[str] = None
        #: Lazily consulted route resolver.
        self.resolve: Optional[Callable[[], Optional[str]]] = None
        self.attempt: Optional[int] = None
        self._norm_src: Optional[str] = None
        self._norm: Optional[str] = None

    def raw_route(self) -> Tuple[Optional[str], bool]:
        """Raw (un-normalized) route known right now, and whether it came from trooper itself."""
        if self.kind == "consumer" or self.route:
            return self.route, True
        if self.resolve is not None:
            try:
                r = self.resolve()
            except Exception as err:  # noqa: BLE001
                record_error("resolver", err)
                r = None
            if r:
                return r, True
        try:
            v = self.root.attributes.get("http.route")
        except Exception:  # noqa: BLE001
            v = None
        if isinstance(v, str) and v:
            return v, False
        return None, False

    def entry_route(self) -> Optional[str]:
        """Normalized entry route (server route template, gRPC method or consumer process name)."""
        raw, _ = self.raw_route()
        if not raw:
            return None
        if self.kind == "consumer" or self.method == METHOD_GRPC:
            return raw
        if raw != self._norm_src:
            self._norm = normalize_route(raw)
            self._norm_src = raw
        return self._norm


_open: Dict[SpanKey, Entry] = {}
_threads: Dict[int, Entry] = {}


def span_key(sc: Any) -> SpanKey:
    return (sc.trace_id, sc.span_id)


def link_span(key: SpanKey, e: Entry) -> None:
    """Links an open span to its entry. Bounded: beyond MAX_OPEN (leaked, never-ended) spans nothing is tracked."""
    if len(_open) < MAX_OPEN:
        _open[key] = e


def unlink_span(key: SpanKey) -> Optional[Entry]:
    return _open.pop(key, None)


def entry_of_key(key: SpanKey) -> Optional[Entry]:
    return _open.get(key)


def tracked_spans() -> int:
    """Number of open spans currently tracked (diagnostics / tests)."""
    return len(_open)


def bind_thread(e: Entry, ident: Optional[int] = None) -> None:
    _threads[threading.get_ident() if ident is None else ident] = e


def release_entry(e: Entry) -> None:
    """Drops every thread binding of an ended entry."""
    try:
        for ident, cur in list(_threads.items()):
            if cur is e:
                _threads.pop(ident, None)
    except Exception as err:  # noqa: BLE001
        record_error("entry", err)


def thread_entries() -> Dict[int, Entry]:
    """Snapshot of thread ident → current entry (profiler)."""
    try:
        return dict(_threads)
    except Exception:  # noqa: BLE001
        return {}


def reset() -> None:
    """Test hook."""
    _open.clear()
    _threads.clear()


def current_entry(ctx: Optional[otel_context.Context] = None) -> Optional[Entry]:
    """Entry of the active span in ``ctx`` (default: the current context)."""
    sc = trace.get_current_span(ctx).get_span_context()
    if sc is None or not sc.is_valid:
        return None
    return _open.get(span_key(sc))


def set_route(route: str, method: Optional[str] = None) -> bool:
    """Sets the route template of the current request (``/users/:id``), for frameworks the SDK can't read it from.

    Returns ``False`` when there is no active entry span. Never raises.
    """
    try:
        e = current_entry()
        if e is None or not route:
            return False
        e.route = str(route)
        if method:
            e.method = str(method).upper()
        return True
    except Exception as err:  # noqa: BLE001
        record_error("set_route", err)
        return False
