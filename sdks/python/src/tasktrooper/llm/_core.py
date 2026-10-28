"""Shared machinery of the LLM wrappers: GenAI span lifecycle, stream proxies, attribute-path client proxies."""

from __future__ import annotations

import inspect
import json
import math
import os
import time
from typing import Any, Callable, Dict, Iterator, List, Optional, Tuple

from opentelemetry import context as otel_context
from opentelemetry import trace
from opentelemetry.trace import SpanKind, Status, StatusCode

from .._attrs import ATTR_LLM_TTFT_MS, SCOPE_NAME, VERSION
from .._errors import record_error
from ..config import LlmConfig, resolve_llm

#: Content attributes are capped like the writer does (.ai/contracts.md §25).
MAX_CONTENT = 32 * 1024

Attrs = Dict[str, Any]


# --- duck-typed access ----------------------------------------------------------------------------------------------


def g(o: Any, key: str, default: Any = None) -> Any:
    """``o[key]`` for mappings, ``o.key`` otherwise (provider SDK objects are pydantic models)."""
    if o is None:
        return default
    if isinstance(o, dict):
        return o.get(key, default)
    try:
        return getattr(o, key, default)
    except Exception:  # noqa: BLE001
        return default


def s(v: Any) -> Optional[str]:
    return v if isinstance(v, str) and v else None


def num(v: Any) -> Optional[float]:
    if isinstance(v, bool) or not isinstance(v, (int, float)):
        return None
    return v if math.isfinite(v) else None


def as_list(v: Any) -> List[Any]:
    if v is None or isinstance(v, (str, bytes, dict)):
        return []
    try:
        return list(v)
    except TypeError:
        return []


def plain(o: Any) -> Any:
    """Pydantic model → dict (for content capture)."""
    if isinstance(o, (dict, list, str, int, float, bool)) or o is None:
        return o
    dump = getattr(o, "model_dump", None)
    if callable(dump):
        try:
            return dump(exclude_none=True)
        except Exception:  # noqa: BLE001
            pass
    return o


def set_if(attrs: Attrs, key: str, v: Any) -> None:
    if v is None:
        return
    if isinstance(v, (list, tuple)) and len(v) == 0:
        return
    attrs[key] = list(v) if isinstance(v, tuple) else v


def _json_default(o: Any) -> Any:
    p = plain(o)
    if p is not o:
        return p
    return str(o)


def to_json(v: Any) -> Optional[str]:
    try:
        out = json.dumps(v, ensure_ascii=False, default=_json_default, separators=(",", ":"))
    except Exception:  # noqa: BLE001
        return None
    if len(out) > MAX_CONTENT:
        out = out[:MAX_CONTENT] + "…[truncated]"
    return out


def parse_args(v: Any) -> Any:
    if not isinstance(v, str):
        return v
    try:
        return json.loads(v)
    except ValueError:
        return v


def text_part(content: str) -> Dict[str, Any]:
    return {"type": "text", "content": content}


def media_part(modality: str, url: Any = None, mime: Any = None) -> Dict[str, Any]:
    u = s(url)
    if u and not u.startswith("data:"):
        return {"type": "uri", "modality": modality, "uri": u}
    p: Dict[str, Any] = {"type": "blob", "modality": modality}
    if s(mime):
        p["mime_type"] = mime
    return p  # inline data is never copied


def set_content(attrs: Attrs, system: List[Any], messages: List[Any]) -> None:
    if system:
        set_if(attrs, "gen_ai.system_instructions", to_json(system))
    if messages:
        set_if(attrs, "gen_ai.input.messages", to_json(messages))


# --- settings -------------------------------------------------------------------------------------------------------


def settings(capture_content: Optional[bool], inject_stream_usage: Optional[bool]) -> LlmConfig:
    try:
        from .._init import get_handle

        h = get_handle()
        base = h.config.llm if h is not None else resolve_llm(None, os.environ)
    except Exception:  # noqa: BLE001
        base = LlmConfig()
    return LlmConfig(
        capture_content=base.capture_content if capture_content is None else bool(capture_content),
        inject_stream_usage=base.inject_stream_usage if inject_stream_usage is None else bool(inject_stream_usage),
    )


# --- span lifecycle -------------------------------------------------------------------------------------------------


def error_type(err: BaseException) -> str:
    """Low-cardinality error class: provider error type (``rate_limit_error`` …), else the exception class name."""
    body = g(err, "body")
    inner = g(body, "error") if isinstance(body, dict) else None
    t = s(g(inner, "type")) if isinstance(inner, dict) else None  # anthropic: {"type":"error","error":{"type":…}}
    if t:
        return t
    t = s(g(err, "type"))  # openai
    if t:
        return t
    if isinstance(body, dict):
        t = s(body.get("type"))
        if t and t != "error":
            return t
    return type(err).__name__ or "_OTHER"


class LlmCall:
    """One GenAI CLIENT span (``"<operation> <model>"``)."""

    def __init__(self, name: str, attrs: Attrs) -> None:
        self.span = trace.get_tracer(SCOPE_NAME, VERSION).start_span(name, kind=SpanKind.CLIENT, attributes=attrs)
        self._t0 = time.perf_counter()
        self._ttft = False
        self.ended = False

    def first_token(self) -> None:
        if self._ttft or self.ended:
            return
        self._ttft = True
        self.span.set_attribute(ATTR_LLM_TTFT_MS, (time.perf_counter() - self._t0) * 1000.0)

    def set(self, attrs: Attrs) -> None:
        if not self.ended and attrs:
            self.span.set_attributes(attrs)

    def end(self) -> None:
        if self.ended:
            return
        self.ended = True
        self.span.end()

    def fail(self, err: BaseException) -> None:
        if self.ended:
            return
        try:
            status = g(err, "status_code")
            if isinstance(status, int) and not isinstance(status, bool):
                self.span.set_attribute("http.response.status_code", status)
            self.span.set_attribute("error.type", error_type(err))
            self.span.record_exception(err)
            self.span.set_status(Status(StatusCode.ERROR, str(err)[:1024]))
        except Exception as e:  # noqa: BLE001
            record_error("llm", e)
        self.end()

    def activate(self) -> Any:
        return otel_context.attach(trace.set_span_in_context(self.span))


def deactivate(token: Any) -> None:
    try:
        otel_context.detach(token)
    except Exception:  # noqa: BLE001
        pass


def safely(fn: Callable[[], Any]) -> None:
    try:
        fn()
    except Exception as err:  # noqa: BLE001
        record_error("llm", err)


class Recorder:
    """Per-call adapter of one provider operation."""

    operation = "chat"

    def __init__(self, kwargs: Dict[str, Any], cfg: LlmConfig) -> None:
        self.kwargs = kwargs
        self.cfg = cfg
        self.attrs: Attrs = {}
        self.model: Optional[str] = s(kwargs.get("model"))
        self.stream = kwargs.get("stream") is True

    def on_response(self, resp: Any, call: LlmCall) -> None:  # final (non-stream) response
        pass

    def on_event(self, ev: Any, call: LlmCall) -> None:  # one stream event
        pass

    def on_stream_end(self, call: LlmCall) -> None:
        pass


def start_call(provider: str, rec: Recorder, server: Attrs) -> LlmCall:
    attrs: Attrs = {"gen_ai.provider.name": provider, "gen_ai.operation.name": rec.operation}
    attrs.update(server)
    attrs.update(rec.attrs)
    if rec.model:
        attrs["gen_ai.request.model"] = rec.model
    return LlmCall(f"{rec.operation} {rec.model}" if rec.model else rec.operation, attrs)


# --- streams --------------------------------------------------------------------------------------------------------


class _Delegating:
    __slots__ = ("_target",)

    def __getattr__(self, name: str) -> Any:
        return getattr(object.__getattribute__(self, "_target"), name)

    @property  # type: ignore[misc]
    def __class__(self) -> Any:  # isinstance(proxy, openai.Stream) keeps working
        return type(object.__getattribute__(self, "_target"))


class SyncStream(_Delegating):
    """Wraps a sync stream (iterable of events); the span ends when iteration ends, on ``close()`` or ``__exit__``."""

    __slots__ = ("_call", "_rec", "_it")

    def __init__(self, target: Any, call: LlmCall, rec: Recorder) -> None:
        object.__setattr__(self, "_target", target)
        object.__setattr__(self, "_call", call)
        object.__setattr__(self, "_rec", rec)
        object.__setattr__(self, "_it", None)

    def _finish(self) -> None:
        call = self._call
        if call.ended:
            return
        safely(lambda: self._rec.on_stream_end(call))
        call.end()

    def __iter__(self) -> Iterator[Any]:
        return self

    def __next__(self) -> Any:
        it = self._it
        if it is None:
            it = iter(self._target)
            object.__setattr__(self, "_it", it)
        try:
            ev = next(it)
        except StopIteration:
            self._finish()
            raise
        except BaseException as err:
            self._call.fail(err)
            raise
        if not self._call.ended:
            safely(lambda: self._rec.on_event(ev, self._call))
        return ev

    def __enter__(self) -> "SyncStream":
        enter = getattr(self._target, "__enter__", None)
        if enter is not None:
            enter()
        return self

    def __exit__(self, *exc: Any) -> Any:
        try:
            ex = getattr(self._target, "__exit__", None)
            return ex(*exc) if ex is not None else None
        finally:
            if exc and exc[1] is not None and not isinstance(exc[1], GeneratorExit):
                self._call.fail(exc[1])
            self._finish()

    def close(self) -> Any:
        try:
            c = getattr(self._target, "close", None)
            return c() if c is not None else None
        finally:
            self._finish()


class AsyncStream(_Delegating):
    """Async counterpart of :class:`SyncStream`."""

    __slots__ = ("_call", "_rec", "_it")

    def __init__(self, target: Any, call: LlmCall, rec: Recorder) -> None:
        object.__setattr__(self, "_target", target)
        object.__setattr__(self, "_call", call)
        object.__setattr__(self, "_rec", rec)
        object.__setattr__(self, "_it", None)

    def _finish(self) -> None:
        call = self._call
        if call.ended:
            return
        safely(lambda: self._rec.on_stream_end(call))
        call.end()

    def __aiter__(self) -> "AsyncStream":
        return self

    async def __anext__(self) -> Any:
        it = self._it
        if it is None:
            it = self._target.__aiter__()
            object.__setattr__(self, "_it", it)
        try:
            ev = await it.__anext__()
        except StopAsyncIteration:
            self._finish()
            raise
        except BaseException as err:
            self._call.fail(err)
            raise
        if not self._call.ended:
            safely(lambda: self._rec.on_event(ev, self._call))
        return ev

    async def __aenter__(self) -> "AsyncStream":
        enter = getattr(self._target, "__aenter__", None)
        if enter is not None:
            await enter()
        return self

    async def __aexit__(self, *exc: Any) -> Any:
        try:
            ex = getattr(self._target, "__aexit__", None)
            return (await ex(*exc)) if ex is not None else None
        finally:
            if exc and exc[1] is not None and not isinstance(exc[1], GeneratorExit):
                self._call.fail(exc[1])
            self._finish()

    async def close(self) -> Any:
        try:
            c = getattr(self._target, "close", None)
            if c is not None:
                r = c()
                if inspect.isawaitable(r):
                    r = await r
                return r
            return None
        finally:
            self._finish()


def _is_async_iterable(v: Any) -> bool:
    return hasattr(v, "__aiter__")


def _is_sync_iterable(v: Any) -> bool:
    return hasattr(v, "__iter__") and not isinstance(v, (dict, list, str, bytes)) and not hasattr(v, "model_dump")


def map_result(call: LlmCall, rec: Recorder, v: Any) -> Any:
    """Streams are wrapped; final responses recorded and the span ended."""
    try:
        if rec.stream and _is_async_iterable(v):
            return AsyncStream(v, call, rec)
        if rec.stream and _is_sync_iterable(v):
            return SyncStream(v, call, rec)
        rec.on_response(v, call)
        call.end()
    except Exception as err:  # noqa: BLE001
        record_error("llm", err)
        call.end()
    return v


# --- create-style method wrapper ------------------------------------------------------------------------------------

RecorderFactory = Callable[[Dict[str, Any], LlmConfig], Recorder]


class Target:
    def __init__(self, provider: str, server: Attrs, capture: Optional[bool], inject: Optional[bool]) -> None:
        self.provider = provider
        self.server = server
        self.capture = capture
        self.inject = inject


def wrap_create(fn: Callable[..., Any], target: Target, make: RecorderFactory) -> Callable[..., Any]:
    """Span around a ``create``-style call; the HTTP client span becomes its child. Sync and async."""

    def wrapper(*args: Any, **kwargs: Any) -> Any:
        try:
            rec = make(dict(kwargs), settings(target.capture, target.inject))
            call = start_call(target.provider, rec, target.server)
            send = rec.kwargs
        except Exception as err:  # noqa: BLE001
            record_error("llm", err)
            return fn(*args, **kwargs)
        token = call.activate()
        try:
            result = fn(*args, **send)
        except BaseException as err:
            call.fail(err)
            raise
        finally:
            deactivate(token)
        if inspect.isawaitable(result):
            return _await(result, call, rec)
        return map_result(call, rec, result)

    try:
        wrapper.__wrapped__ = fn  # type: ignore[attr-defined]
        wrapper.__name__ = getattr(fn, "__name__", "create")
        wrapper.__doc__ = getattr(fn, "__doc__", None)
    except Exception:  # noqa: BLE001
        pass
    return wrapper


async def _await(aw: Any, call: LlmCall, rec: Recorder) -> Any:
    token = call.activate()
    try:
        v = await aw
    except BaseException as err:
        call.fail(err)
        raise
    finally:
        deactivate(token)
    return map_result(call, rec, v)


# --- client proxy ---------------------------------------------------------------------------------------------------

Wrapper = Callable[[Callable[..., Any], Target], Callable[..., Any]]


class ClientProxy(_Delegating):
    """Intercepts the dotted method paths of ``table`` (``chat.completions.create`` …); everything else passes through.

    Nothing global is patched and the wrapped client is not modified.
    """

    __slots__ = ("_prefix", "_table", "_tgt")

    def __init__(self, obj: Any, prefix: str, table: Dict[str, Wrapper], tgt: Target) -> None:
        object.__setattr__(self, "_target", obj)
        object.__setattr__(self, "_prefix", prefix)
        object.__setattr__(self, "_table", table)
        object.__setattr__(self, "_tgt", tgt)

    def __getattr__(self, name: str) -> Any:
        if name == "__trooper_instrumented__":
            return True
        v = getattr(object.__getattribute__(self, "_target"), name)
        prefix = object.__getattribute__(self, "_prefix")
        table = object.__getattribute__(self, "_table")
        path = prefix + name
        try:
            w = table.get(path)
            if w is not None and callable(v):
                return w(v, object.__getattribute__(self, "_tgt"))
            if not name.startswith("_") and any(k.startswith(path + ".") for k in table):
                return ClientProxy(v, path + ".", table, object.__getattribute__(self, "_tgt"))
        except Exception as err:  # noqa: BLE001
            record_error("llm", err)
        return v

    def __setattr__(self, name: str, value: Any) -> None:
        setattr(object.__getattribute__(self, "_target"), name, value)

    def __repr__(self) -> str:
        return repr(object.__getattribute__(self, "_target"))

    def __dir__(self) -> List[str]:
        return dir(object.__getattribute__(self, "_target"))


def server_attrs(client: Any) -> Tuple[Attrs, Optional[str]]:
    """``server.address`` / ``server.port`` from the client's ``base_url``; also returns the host."""
    try:
        base = g(client, "base_url")
        if base is None:
            return {}, None
        from urllib.parse import urlsplit

        u = urlsplit(str(base))
        if not u.hostname:
            return {}, None
        port = u.port or (443 if u.scheme == "https" else 80)
        return {"server.address": u.hostname, "server.port": port}, u.hostname
    except Exception:  # noqa: BLE001
        return {}, None


def instrument(client: Any, table: Dict[str, Wrapper], provider: Callable[[Any, Optional[str]], str], opts: Dict[str, Any]) -> Any:
    try:
        if client is None or getattr(client, "__trooper_instrumented__", False) is True:
            return client
        server, host = server_attrs(client)
        p = opts.get("provider") or provider(client, host)
        tgt = Target(p, server, opts.get("capture_content"), opts.get("inject_stream_usage"))
        return ClientProxy(client, "", table, tgt)
    except Exception as err:  # noqa: BLE001
        record_error("llm", err)
        return client
