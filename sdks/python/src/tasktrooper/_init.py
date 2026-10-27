"""``tasktrooper.init``: the TaskTrooper distribution of OpenTelemetry (ADR-011, ADR-024)."""

from __future__ import annotations

import atexit
import logging
import threading
import warnings
from typing import Any, List, Mapping, Optional, Sequence, Union

from opentelemetry import metrics as otel_metrics
from opentelemetry import propagate, trace

from ._attrs import DISTRO_NAME, SELF_METER, VERSION
from ._errors import bind_self_metrics, record_error, set_debug
from .config import Config, resolve_config
from .propagation import create_propagator

_lock = threading.RLock()
_current: Optional["TrooperHandle"] = None


class TrooperHandle:
    """Returned by :func:`init`. ``force_flush`` / ``shutdown`` never raise."""

    def __init__(
        self,
        config: Config,
        tracer_provider: Any = None,
        meter_provider: Any = None,
        logger_provider: Any = None,
        profiler: Any = None,
        instrumentations: Optional[List[str]] = None,
        log_handler: Optional[logging.Handler] = None,
    ) -> None:
        self.config = config
        self.tracer_provider = tracer_provider
        self.meter_provider = meter_provider
        self.logger_provider = logger_provider
        self.profiler = profiler
        self.instrumentations = list(instrumentations or [])
        self._log_handler = log_handler
        self._stopped = False

    @property
    def exporting(self) -> bool:
        """Whether telemetry is exported (a DSN or OTLP endpoint was configured and the SDK is enabled)."""
        return self.config.enabled and self.config.exporting and self.tracer_provider is not None

    def force_flush(self, timeout_s: float = 10.0) -> bool:
        """Flushes pending spans / metrics / logs. Returns False when something did not flush."""
        ok = True
        ms = int(timeout_s * 1000)
        for p in (self.tracer_provider, self.meter_provider, self.logger_provider):
            if p is None:
                continue
            try:
                r = p.force_flush(ms) if p is not self.meter_provider else p.force_flush(timeout_millis=ms)
                ok = ok and (r is not False)
            except Exception as err:  # noqa: BLE001
                record_error("flush", err)
                ok = False
        return ok

    def shutdown(self, timeout_s: float = 10.0) -> None:
        """Stops the profiler, flushes and shuts the providers down. Idempotent."""
        global _current
        with _lock:
            if self._stopped:
                return
            self._stopped = True
        try:
            if self.profiler is not None:
                self.profiler.stop(min(timeout_s, 5.0))
        except Exception as err:  # noqa: BLE001
            record_error("shutdown", err)
        if self._log_handler is not None:
            try:
                logging.getLogger().removeHandler(self._log_handler)
            except Exception as err:  # noqa: BLE001
                record_error("shutdown", err)
        for p in (self.tracer_provider, self.meter_provider, self.logger_provider):
            if p is None:
                continue
            try:
                if p is self.meter_provider:
                    p.shutdown(timeout_millis=timeout_s * 1000)
                else:
                    p.shutdown()
            except Exception as err:  # noqa: BLE001
                record_error("shutdown", err)
        with _lock:
            if _current is self:
                _current = None


def get_handle() -> Optional[TrooperHandle]:
    """The handle of the running SDK, if :func:`init` was called."""
    return _current


def _counting(base: type, signal: str) -> type:
    """Subclass of an OTLP exporter that counts failed exports as ``trooper.sdk.errors{component=exporter.<signal>}``."""

    class Counting(base):  # type: ignore[valid-type, misc]
        def export(self, *args: Any, **kwargs: Any) -> Any:
            try:
                r = super().export(*args, **kwargs)
            except Exception as err:  # noqa: BLE001
                record_error("exporter." + signal, err)
                raise
            name = getattr(r, "name", "")
            if name and name != "SUCCESS":
                record_error("exporter." + signal, RuntimeError(name))
            return r

    Counting.__name__ = "Trooper" + base.__name__
    return Counting


def _resource(cfg: Config) -> Any:
    from opentelemetry.sdk.resources import Resource

    attrs = dict(cfg.resource_attributes)
    attrs.update(
        {
            "service.name": cfg.service,
            "telemetry.distro.name": DISTRO_NAME,
            "telemetry.distro.version": VERSION,
        }
    )
    if cfg.version:
        attrs["service.version"] = cfg.version
    if cfg.environment:
        attrs["deployment.environment.name"] = cfg.environment
    return Resource.create(attrs)


def _compression() -> Any:
    try:
        from opentelemetry.exporter.otlp.proto.http import Compression

        return Compression.Gzip
    except Exception:  # noqa: BLE001
        return None


def _setup_logs(cfg: Config, resource: Any, instrument_logging: bool) -> Any:
    from opentelemetry._logs import set_logger_provider
    from opentelemetry.exporter.otlp.proto.http._log_exporter import OTLPLogExporter
    from opentelemetry.sdk._logs import LoggerProvider
    from opentelemetry.sdk._logs.export import BatchLogRecordProcessor

    lp = LoggerProvider(resource=resource)
    exporter = _counting(OTLPLogExporter, "logs")(
        endpoint=cfg.logs_url, headers=cfg.headers, timeout=cfg.export_timeout_s, compression=_compression()
    )
    lp.add_log_record_processor(
        BatchLogRecordProcessor(
            exporter,
            schedule_delay_millis=int(cfg.batch_delay_s * 1000),
            max_queue_size=cfg.max_queue_size,
            export_timeout_millis=int(cfg.export_timeout_s * 1000),
        )
    )
    set_logger_provider(lp)
    handler: Optional[logging.Handler] = None
    if instrument_logging:
        try:
            from opentelemetry.instrumentation.logging import LoggingInstrumentor

            inst = LoggingInstrumentor()
            if not inst.is_instrumented_by_opentelemetry:
                inst.instrument(logger_provider=lp)
            if getattr(LoggingInstrumentor, "_logging_handler", None) is not None:
                return lp, None  # the instrumentation attached its own OTLP handler
        except ImportError:
            pass
        except Exception as err:  # noqa: BLE001
            record_error("instrumentation.logging", err)
    try:
        with warnings.catch_warnings():
            warnings.simplefilter("ignore", DeprecationWarning)
            from opentelemetry.sdk._logs import LoggingHandler

            handler = LoggingHandler(level=logging.NOTSET, logger_provider=lp)
        handler.addFilter(_NotOwnLogs())
        logging.getLogger().addHandler(handler)
    except Exception as err:  # noqa: BLE001
        record_error("logs", err)
        handler = None
    return lp, handler


class _NotOwnLogs(logging.Filter):
    """Keeps exporter / SDK internals out of the log export (they could loop)."""

    def filter(self, record: logging.LogRecord) -> bool:
        n = record.name
        return not (n.startswith("opentelemetry") or n.startswith("tasktrooper") or n.startswith("urllib3"))


def init(
    dsn: Optional[str] = None,
    service: Optional[str] = None,
    environment: Optional[str] = None,
    version: Optional[str] = None,
    sample_rate: Optional[float] = None,
    profiling: Union[bool, Mapping[str, Any], None] = None,
    llm: Optional[Mapping[str, Any]] = None,
    enabled: Optional[bool] = None,
    instrument: Union[bool, Sequence[str]] = True,
    *,
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
) -> TrooperHandle:
    """Starts the TaskTrooper distribution of OpenTelemetry.

    OTLP/HTTP protobuf export of traces, metrics and logs to the DSN's ingest gateway (``x-trooper-key``), trooper
    sampler / propagator / span processor, the installed contrib instrumentations (``instrument``), optional
    continuous profiling and LLM defaults. Call it before importing / creating the web app (or use
    ``tasktrooper-run`` / ``import tasktrooper.auto``).

    Idempotent (later calls return the first handle); never raises — on failure it returns a handle that does nothing.
    Without a DSN / ``OTEL_EXPORTER_OTLP_ENDPOINT`` nothing is exported but propagation and the conventions work.
    """
    global _current
    with _lock:
        if _current is not None:
            return _current
        try:
            cfg = resolve_config(
                dsn=dsn,
                service=service,
                environment=environment,
                version=version,
                sample_rate=sample_rate,
                profiling=profiling,
                llm=llm,
                enabled=enabled,
                instrument=list(instrument) if isinstance(instrument, (list, tuple, set)) else instrument,
                disable_instrumentations=disable_instrumentations,
                resource_attributes=resource_attributes,
                debug=debug,
                traces=traces,
                metrics=metrics,
                logs=logs,
                metric_interval_s=metric_interval_s,
                export_timeout_s=export_timeout_s,
                batch_delay_s=batch_delay_s,
                max_queue_size=max_queue_size,
            )
        except Exception as err:  # noqa: BLE001
            record_error("init", err)
            cfg = resolve_config(enabled=False, env={})
        set_debug(cfg.debug)
        if cfg.dsn_error:
            record_error("dsn", ValueError(cfg.dsn_error))
        if not cfg.enabled:
            _current = TrooperHandle(cfg)
            return _current
        try:
            _current = _start(cfg)
        except Exception as err:  # noqa: BLE001
            record_error("init", err)
            try:
                propagate.set_global_textmap(create_propagator(cfg.service))
            except Exception:  # noqa: BLE001
                pass
            _current = TrooperHandle(cfg)
        return _current


def _start(cfg: Config) -> TrooperHandle:
    from opentelemetry.sdk.metrics import MeterProvider
    from opentelemetry.sdk.metrics.export import PeriodicExportingMetricReader
    from opentelemetry.sdk.trace import TracerProvider
    from opentelemetry.sdk.trace.export import BatchSpanProcessor

    from .instrumentation import instrument_libraries
    from .processor import TrooperSpanProcessor
    from .sampler import TrooperSampler

    resource = _resource(cfg)
    compression = _compression()

    # traces
    delegates = []
    if cfg.traces_url:
        from opentelemetry.exporter.otlp.proto.http.trace_exporter import OTLPSpanExporter

        exporter = _counting(OTLPSpanExporter, "traces")(
            endpoint=cfg.traces_url, headers=cfg.headers, timeout=cfg.export_timeout_s, compression=compression
        )
        delegates.append(
            BatchSpanProcessor(
                exporter,
                schedule_delay_millis=int(cfg.batch_delay_s * 1000),
                max_queue_size=cfg.max_queue_size,
                export_timeout_millis=int(cfg.export_timeout_s * 1000),
            )
        )
    tp = TracerProvider(resource=resource, sampler=TrooperSampler(cfg.sample_rate))
    tp.add_span_processor(TrooperSpanProcessor(cfg.sample_rate, delegates))
    trace.set_tracer_provider(tp)
    if trace.get_tracer_provider() is not tp:
        # Another SDK already owns the global provider; ours is still passed to the instrumentors.
        record_error("init.tracer_provider")

    # metrics
    readers = []
    if cfg.metrics_url:
        from opentelemetry.exporter.otlp.proto.http.metric_exporter import OTLPMetricExporter

        mexp = _counting(OTLPMetricExporter, "metrics")(
            endpoint=cfg.metrics_url, headers=cfg.headers, timeout=cfg.export_timeout_s, compression=compression
        )
        readers.append(
            PeriodicExportingMetricReader(
                mexp,
                export_interval_millis=cfg.metric_interval_s * 1000,
                export_timeout_millis=min(cfg.export_timeout_s, cfg.metric_interval_s) * 1000,
            )
        )
    mp = MeterProvider(resource=resource, metric_readers=readers)
    otel_metrics.set_meter_provider(mp)
    bind_self_metrics(mp.get_meter(SELF_METER, VERSION))

    propagate.set_global_textmap(create_propagator(cfg.service))

    instr = cfg.instrument
    instrument_on = instr is not False
    disabled = [d.lower() for d in cfg.disable_instrumentations]
    lp = None
    handler = None
    if cfg.logs_url:
        try:
            lp, handler = _setup_logs(cfg, resource, instrument_on and "logging" not in disabled)
        except Exception as err:  # noqa: BLE001
            record_error("logs", err)

    enabled: List[str] = []
    if instrument_on:
        only = list(instr) if isinstance(instr, list) else None
        enabled = instrument_libraries(only, disabled, tracer_provider=tp, meter_provider=mp)

    profiler = None
    if cfg.profiling is not None and cfg.profiles_url:
        try:
            from .profiling import ContinuousProfiler, ProfileUploader

            profiler = ContinuousProfiler(
                cfg.profiling,
                ProfileUploader(cfg.profiles_url, cfg.headers, cfg.export_timeout_s),
                service=cfg.service,
                env=cfg.environment,
                version=cfg.version,
            )
            profiler.start()
        except Exception as err:  # noqa: BLE001
            record_error("profiler", err)
            profiler = None

    handle = TrooperHandle(cfg, tp, mp, lp, profiler, enabled, handler)
    atexit.register(handle.shutdown)
    return handle


def _reset_for_tests() -> None:
    """Test hook: forget the running handle (does not shut it down)."""
    global _current
    with _lock:
        _current = None
