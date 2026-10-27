"""DSN parsing and configuration resolution (.ai/contracts.md §2, §24, §25).

Precedence: explicit argument > ``OTEL_*`` env (when set) > ``TROOPER_*`` env > default. Nothing here raises.
"""

from __future__ import annotations

import math
import os
from dataclasses import dataclass, field
from typing import Any, Dict, List, Mapping, Optional, Sequence, Union
from urllib.parse import unquote, urlsplit

#: Profile types the Python SDK can produce (.ai/contracts.md §24).
PROFILE_TYPES = ("cpu", "wall", "inuse_space")
#: Default Python profile types.
DEFAULT_PROFILE_TYPES = ("cpu",)


class DsnError(ValueError):
    pass


@dataclass(frozen=True)
class Dsn:
    """Parsed DSN ``https://<key>@<host>[:port][/<prefix>]/<project_id>``."""

    #: Base URL of the ingest gateway without trailing slash; signal paths (``/v1/traces`` …) are appended.
    endpoint: str
    #: Ingest key, sent as ``x-trooper-key``.
    key: str
    #: Project id (informational; the key determines the tenant).
    project_id: Optional[str] = None


def parse_dsn(dsn: str) -> Dsn:
    """Parses a DSN. Raises :class:`DsnError` on invalid input (callers swallow; ``init`` never raises)."""
    s = (dsn or "").strip()
    if not s:
        raise DsnError("trooper: empty DSN")
    try:
        u = urlsplit(s)
        port = u.port  # validates the port
    except ValueError as e:
        raise DsnError(f"trooper: invalid DSN: {e}") from None
    if u.scheme not in ("http", "https"):
        raise DsnError(f"trooper: invalid DSN scheme {u.scheme!r} (want http or https)")
    if not u.hostname:
        raise DsnError("trooper: DSN has no host")
    key = unquote(u.username or "")
    if not key:
        raise DsnError("trooper: DSN has no ingest key (https://<key>@host/<project>)")
    host = u.hostname
    if ":" in host:  # IPv6
        host = f"[{host}]"
    netloc = f"{host}:{port}" if port is not None else host
    path = u.path.strip("/")
    prefix = ""
    project: Optional[str] = None
    if path:
        i = path.rfind("/")
        if i >= 0:
            prefix = "/" + path[:i]
            project = path[i + 1 :]
        else:
            project = path
    return Dsn(endpoint=f"{u.scheme}://{netloc}{prefix}", key=key, project_id=project or None)


def _first(*vals: Optional[str]) -> Optional[str]:
    for v in vals:
        if v is not None:
            t = str(v).strip()
            if t:
                return t
    return None


def parse_bool(v: Any) -> Optional[bool]:
    if isinstance(v, bool):
        return v
    if v is None:
        return None
    t = str(v).strip().lower()
    if t in ("true", "1", "yes", "on"):
        return True
    if t in ("false", "0", "no", "off"):
        return False
    return None


def parse_rate(v: Any) -> Optional[float]:
    """A sampling probability in (0, 1], else ``None``."""
    if v is None or isinstance(v, bool):
        return None
    try:
        n = float(str(v).strip()) if not isinstance(v, (int, float)) else float(v)
    except ValueError:
        return None
    return n if math.isfinite(n) and 0 < n <= 1 else None


def _positive(*vals: Any) -> Optional[float]:
    for v in vals:
        if v is None or isinstance(v, bool):
            continue
        try:
            n = float(v)
        except (TypeError, ValueError):
            continue
        if math.isfinite(n) and n > 0:
            return n
    return None


def parse_headers(v: Optional[str]) -> Dict[str, str]:
    """``k1=v1,k2=v2`` (``OTEL_EXPORTER_OTLP_HEADERS`` format, values URL-encoded)."""
    out: Dict[str, str] = {}
    for part in (v or "").split(","):
        i = part.find("=")
        if i <= 0:
            continue
        k = unquote(part[:i].strip())
        if k:
            out[k.lower()] = unquote(part[i + 1 :].strip())
    return out


def parse_resource_attributes(v: Optional[str]) -> Dict[str, str]:
    """``OTEL_RESOURCE_ATTRIBUTES`` (``k=v,k2=v2``, values URL-encoded)."""
    out: Dict[str, str] = {}
    for part in (v or "").split(","):
        i = part.find("=")
        if i <= 0:
            continue
        out[unquote(part[:i].strip())] = unquote(part[i + 1 :].strip())
    return out


@dataclass
class ProfilingConfig:
    types: List[str]
    #: Seconds between uploads.
    period_s: float = 60.0
    #: Seconds of sampling at the start of each period (≤ period). The window is the profile's [start, end].
    cpu_duration_s: float = 10.0
    #: Sampling frequency of the stack sampler.
    hz: float = 100.0
    #: ``tracemalloc`` frames kept per allocation (``inuse_space``).
    tracemalloc_frames: int = 16


@dataclass
class LlmConfig:
    #: Record prompts / completions (``gen_ai.input.messages`` …). Default False (PII).
    capture_content: bool = False
    #: Add ``stream_options.include_usage`` to OpenAI chat streams so they report tokens. Default False.
    inject_stream_usage: bool = False


@dataclass
class Config:
    enabled: bool = True
    debug: bool = False
    service: str = "unknown_service:python"
    environment: Optional[str] = None
    version: Optional[str] = None
    sample_rate: float = 1.0
    traces_url: Optional[str] = None
    metrics_url: Optional[str] = None
    logs_url: Optional[str] = None
    profiles_url: Optional[str] = None
    headers: Dict[str, str] = field(default_factory=dict)
    resource_attributes: Dict[str, Any] = field(default_factory=dict)
    metric_interval_s: float = 30.0
    export_timeout_s: float = 10.0
    batch_delay_s: float = 5.0
    max_queue_size: int = 2048
    instrument: Union[bool, List[str]] = True
    disable_instrumentations: List[str] = field(default_factory=list)
    profiling: Optional[ProfilingConfig] = None
    llm: LlmConfig = field(default_factory=LlmConfig)
    dsn_error: Optional[str] = None

    @property
    def exporting(self) -> bool:
        return bool(self.traces_url or self.metrics_url or self.logs_url)


def _types(v: Any) -> Optional[List[str]]:
    if v is None:
        return None
    items: Sequence[Any] = v.split(",") if isinstance(v, str) else v if isinstance(v, (list, tuple, set)) else ()
    out: List[str] = []
    for raw in items:
        t = str(raw).strip().lower()
        if t in PROFILE_TYPES and t not in out:
            out.append(t)
    return out or None


def resolve_profiling(opt: Any, env: Mapping[str, str]) -> Optional[ProfilingConfig]:
    """Explicit option (``True`` / ``False`` / dict) > ``TROOPER_PROFILING*`` env > off.

    Dict keys: ``enabled``, ``types``, ``period_s``, ``cpu_duration_s``, ``hz``, ``tracemalloc_frames``.
    """
    o: Mapping[str, Any] = opt if isinstance(opt, Mapping) else {}
    if isinstance(opt, bool):
        on = opt
    elif isinstance(opt, Mapping):
        on = parse_bool(opt.get("enabled", True)) is not False
    else:
        on = not env.get("AWS_LAMBDA_FUNCTION_NAME") and parse_bool(env.get("TROOPER_PROFILING")) is True
    if not on:
        return None
    period = _positive(o.get("period_s"), env.get("TROOPER_PROFILING_PERIOD")) or 60.0
    cpu = _positive(o.get("cpu_duration_s"), env.get("TROOPER_PROFILING_CPU_DURATION")) or 10.0
    hz = _positive(o.get("hz"), env.get("TROOPER_PROFILING_HZ")) or 100.0
    frames = _positive(o.get("tracemalloc_frames")) or 16
    period = max(1.0, period)
    return ProfilingConfig(
        types=_types(o.get("types")) or _types(env.get("TROOPER_PROFILING_TYPES")) or list(DEFAULT_PROFILE_TYPES),
        period_s=period,
        cpu_duration_s=max(0.1, min(cpu, period)),
        hz=max(1.0, min(hz, 1000.0)),
        tracemalloc_frames=int(max(1, min(frames, 128))),
    )


def resolve_llm(opt: Any, env: Mapping[str, str]) -> LlmConfig:
    """Explicit option (dict or :class:`LlmConfig`) > ``TROOPER_LLM_CAPTURE_CONTENT`` > False."""
    if isinstance(opt, LlmConfig):
        return LlmConfig(opt.capture_content, opt.inject_stream_usage)
    o: Mapping[str, Any] = opt if isinstance(opt, Mapping) else {}
    cap = parse_bool(o.get("capture_content"))
    if cap is None:
        cap = parse_bool(env.get("TROOPER_LLM_CAPTURE_CONTENT"))
    inject = parse_bool(o.get("inject_stream_usage"))
    return LlmConfig(capture_content=bool(cap), inject_stream_usage=bool(inject))


def _default_service(env: Mapping[str, str]) -> str:
    # OTel's default; the script name (``uvicorn``, ``gunicorn``, ``celery``) would be misleading.
    return "unknown_service:python"


def resolve_config(
    *,
    dsn: Optional[str] = None,
    service: Optional[str] = None,
    environment: Optional[str] = None,
    version: Optional[str] = None,
    sample_rate: Optional[float] = None,
    profiling: Any = None,
    llm: Any = None,
    enabled: Optional[bool] = None,
    instrument: Union[bool, List[str], None] = True,
    disable_instrumentations: Optional[Sequence[str]] = None,
    resource_attributes: Optional[Mapping[str, Any]] = None,
    debug: Optional[bool] = None,
    traces: bool = True,
    metrics: bool = True,
    logs: bool = True,
    metric_interval_s: Optional[float] = None,
    export_timeout_s: Optional[float] = None,
    batch_delay_s: Optional[float] = None,
    max_queue_size: Optional[int] = None,
    env: Optional[Mapping[str, str]] = None,
) -> Config:
    """Resolves arguments + environment into a :class:`Config`. Never raises."""
    e: Mapping[str, str] = os.environ if env is None else env
    parsed: Optional[Dsn] = None
    dsn_error: Optional[str] = None
    dsn_str = _first(dsn, e.get("TROOPER_DSN"))
    if dsn_str:
        try:
            parsed = parse_dsn(dsn_str)
        except DsnError as err:
            dsn_error = str(err)

    base = _first(e.get("OTEL_EXPORTER_OTLP_ENDPOINT"), parsed.endpoint if parsed else None)
    base = base.rstrip("/") if base else None

    def signal(key: str, path: str, on: bool) -> Optional[str]:
        if not on:
            return None
        return _first(e.get(key)) or (base + path if base else None)

    headers = parse_headers(e.get("OTEL_EXPORTER_OTLP_HEADERS"))
    if parsed:
        headers["x-trooper-key"] = parsed.key

    res_env = parse_resource_attributes(e.get("OTEL_RESOURCE_ATTRIBUTES"))
    enabled_env = parse_bool(e.get("TROOPER_ENABLED"))
    is_enabled = not (enabled is False or (enabled is None and enabled_env is False))

    rate = parse_rate(sample_rate)
    if rate is None:
        rate = parse_rate(e.get("OTEL_TRACES_SAMPLER_ARG"))
    if rate is None:
        rate = parse_rate(e.get("TROOPER_SAMPLE_RATE"))

    dbg = debug if debug is not None else parse_bool(e.get("TROOPER_DEBUG"))
    disabled_instr = list(disable_instrumentations or [])
    disabled_instr += [x.strip() for x in (e.get("TROOPER_DISABLE_INSTRUMENTATIONS") or "").split(",") if x.strip()]
    if isinstance(instrument, (list, tuple, set)):
        instr: Union[bool, List[str]] = [str(x) for x in instrument]
    else:
        instr = instrument is not False

    return Config(
        enabled=is_enabled,
        debug=bool(dbg),
        service=_first(service, e.get("OTEL_SERVICE_NAME"), res_env.get("service.name"), e.get("TROOPER_SERVICE"))
        or _default_service(e),
        environment=_first(
            environment,
            res_env.get("deployment.environment.name"),
            res_env.get("deployment.environment"),
            e.get("TROOPER_ENV"),
        ),
        version=_first(version, res_env.get("service.version"), e.get("TROOPER_VERSION")),
        sample_rate=rate if rate is not None else 1.0,
        traces_url=signal("OTEL_EXPORTER_OTLP_TRACES_ENDPOINT", "/v1/traces", traces),
        metrics_url=signal("OTEL_EXPORTER_OTLP_METRICS_ENDPOINT", "/v1/metrics", metrics),
        logs_url=signal("OTEL_EXPORTER_OTLP_LOGS_ENDPOINT", "/v1/logs", logs),
        profiles_url=base + "/v1/profiles" if base else None,
        headers=headers,
        resource_attributes=dict(resource_attributes or {}),
        metric_interval_s=_positive(metric_interval_s) or 30.0,
        export_timeout_s=_positive(export_timeout_s) or 10.0,
        batch_delay_s=_positive(batch_delay_s) or 5.0,
        max_queue_size=int(_positive(max_queue_size) or 2048),
        instrument=instr,
        disable_instrumentations=disabled_instr,
        profiling=resolve_profiling(profiling, e),
        llm=resolve_llm(llm, e),
        dsn_error=dsn_error,
    )
