"""TaskTrooper Telemetry for Python — an OpenTelemetry distribution (ADR-011, ADR-024).

::

    import tasktrooper

    tasktrooper.init(dsn="https://<ingest-key>@ingest.trooper.dev/<project>", service="orders-api")

See ``sdks/python/README.md``.
"""

from ._attrs import (
    ATTR_CALLER,
    ATTR_ENTRY_METHOD,
    ATTR_ENTRY_ROUTE,
    ATTR_LLM_TTFT_MS,
    ATTR_PROCESS,
    ATTR_ROUTE,
    ATTR_SAMPLE_RATE,
    DISTRO_NAME,
)
from ._errors import internal_error_count
from ._init import TrooperHandle, get_handle, init
from ._version import __version__
from .config import Dsn, DsnError, parse_dsn
from .entry import set_route
from .instrumentation import instrument_app
from .llm import instrument_anthropic, instrument_openai
from .route import normalize_route


def force_flush(timeout_s: float = 10.0) -> bool:
    """Flushes the running SDK (no-op without :func:`init`). Never raises."""
    h = get_handle()
    return h.force_flush(timeout_s) if h is not None else True


def shutdown(timeout_s: float = 10.0) -> None:
    """Shuts the running SDK down (no-op without :func:`init`). Never raises."""
    h = get_handle()
    if h is not None:
        h.shutdown(timeout_s)


__all__ = [
    "init",
    "shutdown",
    "force_flush",
    "get_handle",
    "TrooperHandle",
    "set_route",
    "instrument_app",
    "instrument_openai",
    "instrument_anthropic",
    "normalize_route",
    "parse_dsn",
    "Dsn",
    "DsnError",
    "internal_error_count",
    "ATTR_ROUTE",
    "ATTR_ENTRY_ROUTE",
    "ATTR_ENTRY_METHOD",
    "ATTR_PROCESS",
    "ATTR_CALLER",
    "ATTR_SAMPLE_RATE",
    "ATTR_LLM_TTFT_MS",
    "DISTRO_NAME",
    "__version__",
]
