"""The SDK never raises into the host (AGENTS.md rule 5).

Every swallowed error is counted here and exported as ``trooper.sdk.errors{component}`` (meter
``TaskTrooper.Telemetry``, .ai/contracts.md §4) once a meter is bound.
"""

from __future__ import annotations

import logging
import threading
from typing import Any, Callable, Dict, Optional, TypeVar

_log = logging.getLogger("tasktrooper")
_lock = threading.Lock()
_count = 0
_counter: Any = None
_debug = False
_pending: Dict[str, int] = {}

T = TypeVar("T")


def set_debug(on: bool) -> None:
    global _debug
    _debug = bool(on)


def internal_error_count() -> int:
    """Number of errors swallowed by the SDK since process start."""
    return _count


def bind_self_metrics(meter: Any) -> None:
    """Binds ``trooper.sdk.errors`` to a meter; errors recorded before binding are flushed into it."""
    global _counter
    try:
        counter = meter.create_counter(
            "trooper.sdk.errors", unit="{error}", description="Errors swallowed by the trooper SDK"
        )
        with _lock:
            for component, n in _pending.items():
                counter.add(n, {"component": component})
            _pending.clear()
            _counter = counter
    except Exception:  # noqa: BLE001
        _counter = None


def reset_self_metrics() -> None:
    """Test hook."""
    global _count, _counter
    with _lock:
        _count = 0
        _counter = None
        _pending.clear()


def record_error(component: str, err: Optional[BaseException] = None) -> None:
    """Records a swallowed error. Never raises."""
    global _count
    try:
        with _lock:
            _count += 1
            counter = _counter
            if counter is None:
                _pending[component] = _pending.get(component, 0) + 1
        if counter is not None:
            counter.add(1, {"component": component})
        if _debug:
            _log.warning("[trooper] %s: %r", component, err)
    except Exception:  # noqa: BLE001
        pass


def safe(component: str, fn: Callable[[], T], fallback: T) -> T:
    """Runs ``fn``, returning ``fallback`` (and counting the error) if it raises."""
    try:
        return fn()
    except Exception as err:  # noqa: BLE001
        record_error(component, err)
        return fallback
