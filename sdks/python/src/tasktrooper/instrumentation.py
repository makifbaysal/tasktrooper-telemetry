"""Auto-instrumentation through OpenTelemetry contrib instrumentors (ADR-011, ADR-024).

Each instrumentor is an optional extra (``pip install tasktrooper[fastapi]``). It is enabled only when both the
instrumentor package and the instrumented library are installed; everything is imported lazily and failures are
swallowed (``trooper.sdk.errors{component=instrumentation.<name>}``).
"""

from __future__ import annotations

import importlib
import importlib.util
from typing import Any, Dict, List, Optional, Sequence, Tuple

from ._errors import record_error

#: short name → (instrumentor module, class, library module that must be importable)
INSTRUMENTATIONS: Dict[str, Tuple[str, str, str]] = {
    "django": ("opentelemetry.instrumentation.django", "DjangoInstrumentor", "django"),
    "fastapi": ("opentelemetry.instrumentation.fastapi", "FastAPIInstrumentor", "fastapi"),
    "starlette": ("opentelemetry.instrumentation.starlette", "StarletteInstrumentor", "starlette"),
    "flask": ("opentelemetry.instrumentation.flask", "FlaskInstrumentor", "flask"),
    "requests": ("opentelemetry.instrumentation.requests", "RequestsInstrumentor", "requests"),
    "httpx": ("opentelemetry.instrumentation.httpx", "HTTPXClientInstrumentor", "httpx"),
    "urllib3": ("opentelemetry.instrumentation.urllib3", "URLLib3Instrumentor", "urllib3"),
    "aiohttp-client": ("opentelemetry.instrumentation.aiohttp_client", "AioHttpClientInstrumentor", "aiohttp"),
    "sqlalchemy": ("opentelemetry.instrumentation.sqlalchemy", "SQLAlchemyInstrumentor", "sqlalchemy"),
    "psycopg": ("opentelemetry.instrumentation.psycopg", "PsycopgInstrumentor", "psycopg"),
    "psycopg2": ("opentelemetry.instrumentation.psycopg2", "Psycopg2Instrumentor", "psycopg2"),
    "redis": ("opentelemetry.instrumentation.redis", "RedisInstrumentor", "redis"),
    "celery": ("opentelemetry.instrumentation.celery", "CeleryInstrumentor", "celery"),
    "kafka-python": ("opentelemetry.instrumentation.kafka", "KafkaInstrumentor", "kafka"),
    "confluent-kafka": ("opentelemetry.instrumentation.confluent_kafka", "ConfluentKafkaInstrumentor", "confluent_kafka"),
    "pika": ("opentelemetry.instrumentation.pika", "PikaInstrumentor", "pika"),
}

#: Applied in this order: the starlette instrumentor is skipped when fastapi is active (FastAPI apps are Starlette apps).
ORDER: Tuple[str, ...] = tuple(INSTRUMENTATIONS)

_active: Dict[str, Any] = {}


def _available(module: str) -> bool:
    try:
        return importlib.util.find_spec(module) is not None
    except (ImportError, ValueError):
        return False


def _load(name: str) -> Optional[Any]:
    mod, cls, lib = INSTRUMENTATIONS[name]
    if not _available(lib) or not _available(mod):
        return None
    return getattr(importlib.import_module(mod), cls)()


def instrument_libraries(
    only: Optional[Sequence[str]] = None,
    disabled: Sequence[str] = (),
    tracer_provider: Any = None,
    meter_provider: Any = None,
) -> List[str]:
    """Enables the installed instrumentors. Returns the names enabled. Never raises."""
    off = {d.strip().lower() for d in disabled}
    names = [n for n in ORDER if (only is None or n in only) and n not in off]
    if "fastapi" in names and _available("fastapi"):
        names = [n for n in names if n != "starlette"]
    enabled: List[str] = []
    for name in names:
        if name in _active:
            enabled.append(name)
            continue
        try:
            inst = _load(name)
            if inst is None:
                continue
            kwargs: Dict[str, Any] = {}
            if tracer_provider is not None:
                kwargs["tracer_provider"] = tracer_provider
            if meter_provider is not None:
                kwargs["meter_provider"] = meter_provider
            if not inst.is_instrumented_by_opentelemetry:
                inst.instrument(**kwargs)
            _active[name] = inst
            enabled.append(name)
        except Exception as err:  # noqa: BLE001
            record_error("instrumentation." + name, err)
    return enabled


def uninstrument_libraries() -> None:
    for name, inst in list(_active.items()):
        try:
            inst.uninstrument()
        except Exception as err:  # noqa: BLE001
            record_error("instrumentation." + name, err)
        _active.pop(name, None)


def active_instrumentations() -> List[str]:
    return list(_active)


def instrument_app(app: Any) -> Any:
    """Instruments an already created FastAPI / Starlette / Flask app (``init`` patches only apps created after it).

    Returns the app. Never raises.
    """
    try:
        mods = [c.__module__ for c in type(app).__mro__]
        if any(m.startswith("fastapi") for m in mods):
            from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

            if not getattr(app, "_is_instrumented_by_opentelemetry", False):
                FastAPIInstrumentor.instrument_app(app)
        elif any(m.startswith("starlette") for m in mods):
            from opentelemetry.instrumentation.starlette import StarletteInstrumentor

            if not getattr(app, "_is_instrumented_by_opentelemetry", False):
                StarletteInstrumentor.instrument_app(app)
        elif any(m.startswith("flask") for m in mods):
            from opentelemetry.instrumentation.flask import FlaskInstrumentor

            if not getattr(app, "_is_instrumented_by_opentelemetry", False):
                FlaskInstrumentor.instrument_app(app)
    except Exception as err:  # noqa: BLE001
        record_error("instrumentation.app", err)
    return app
