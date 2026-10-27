"""Anthropic recorder: ``messages.create`` (incl. ``stream=True``) and ``messages.stream()``."""

from __future__ import annotations

from typing import Any, Callable, Dict, List, Optional

from ..config import LlmConfig
from ._core import (
    Attrs,
    LlmCall,
    Recorder,
    Target,
    as_list,
    deactivate,
    g,
    media_part,
    num,
    parse_args,
    plain,
    record_error,
    s,
    safely,
    set_content,
    set_if,
    settings,
    start_call,
    text_part,
    to_json,
)


def anthropic_parts(content: Any) -> List[Dict[str, Any]]:
    if isinstance(content, str):
        return [text_part(content)] if content else []
    out: List[Dict[str, Any]] = []
    for p in as_list(content):
        p = plain(p)
        if not isinstance(p, dict):
            continue
        t = p.get("type")
        if t == "text":
            out.append(text_part(str(p.get("text") or "")))
        elif t == "thinking":
            out.append({"type": "reasoning", "content": str(p.get("thinking") or "")})
        elif t == "redacted_thinking":
            out.append({"type": "reasoning"})
        elif t in ("tool_use", "server_tool_use"):
            out.append({"type": "tool_call", "id": p.get("id"), "name": p.get("name"), "arguments": p.get("input")})
        elif t == "tool_result":
            c = p.get("content")
            out.append({"type": "tool_call_response", "id": p.get("tool_use_id"), "response": c if isinstance(c, str) else anthropic_parts(c)})
        elif t in ("image", "document"):
            src = p.get("source") if isinstance(p.get("source"), dict) else {}
            out.append(media_part("image" if t == "image" else "document", src.get("url") if src.get("type") == "url" else None, src.get("media_type")))
        else:
            out.append({"type": str(t or "generic")})
    return out


class _Usage:
    __slots__ = ("input", "output", "read", "write")

    def __init__(self) -> None:
        self.input: Optional[int] = None
        self.output: Optional[int] = None
        self.read: Optional[int] = None
        self.write: Optional[int] = None

    def update(self, u: Any) -> None:
        if u is None:
            return
        for attr, key in (("input", "input_tokens"), ("output", "output_tokens"), ("read", "cache_read_input_tokens"), ("write", "cache_creation_input_tokens")):
            v = num(g(u, key))
            if v is not None:
                setattr(self, attr, int(v))

    def attrs(self) -> Attrs:
        """Anthropic ``input_tokens`` excludes cache reads/writes; ``gen_ai.usage.input_tokens`` is the total."""
        a: Attrs = {}
        if self.input is not None or self.read is not None or self.write is not None:
            a["gen_ai.usage.input_tokens"] = (self.input or 0) + (self.read or 0) + (self.write or 0)
        set_if(a, "gen_ai.usage.output_tokens", self.output)
        set_if(a, "gen_ai.usage.cache_read.input_tokens", self.read)
        set_if(a, "gen_ai.usage.cache_creation.input_tokens", self.write)
        return a


def _output(content: Any, finish: Optional[str]) -> List[Dict[str, Any]]:
    parts = anthropic_parts(content)
    if not parts:
        return []
    m: Dict[str, Any] = {"role": "assistant", "parts": parts}
    if finish:
        m["finish_reason"] = finish
    return [m]


class MessagesRecorder(Recorder):
    operation = "chat"

    def __init__(self, kwargs: Dict[str, Any], cfg: LlmConfig) -> None:
        super().__init__(kwargs, cfg)
        k = kwargs
        a = self.attrs
        set_if(a, "gen_ai.request.max_tokens", num(k.get("max_tokens")))
        set_if(a, "gen_ai.request.temperature", num(k.get("temperature")))
        set_if(a, "gen_ai.request.top_p", num(k.get("top_p")))
        set_if(a, "gen_ai.request.top_k", num(k.get("top_k")))
        stops = k.get("stop_sequences")
        if isinstance(stops, (list, tuple)):
            set_if(a, "gen_ai.request.stop_sequences", [x for x in stops if isinstance(x, str)])
        if cfg.capture_content:
            msgs = []
            for m in as_list(k.get("messages")):
                m = plain(m)
                if not isinstance(m, dict):
                    continue
                parts = anthropic_parts(m.get("content"))
                role = "tool" if parts and all(p.get("type") == "tool_call_response" for p in parts) else str(m.get("role") or "user")
                msgs.append({"role": role, "parts": parts})
            set_content(a, anthropic_parts(k.get("system")), msgs)
        self._id: Optional[str] = None
        self._model: Optional[str] = None
        self._stop: Optional[str] = None
        self._usage = _Usage()
        self._blocks: Dict[int, Dict[str, Any]] = {}

    def on_response(self, resp: Any, call: LlmCall) -> None:
        if resp is None:
            return
        u = _Usage()
        u.update(g(resp, "usage"))
        out = u.attrs()
        set_if(out, "gen_ai.response.id", s(g(resp, "id")))
        set_if(out, "gen_ai.response.model", s(g(resp, "model")))
        stop = s(g(resp, "stop_reason"))
        if stop:
            out["gen_ai.response.finish_reasons"] = [stop]
        if self.cfg.capture_content:
            set_if(out, "gen_ai.output.messages", to_json(_output(plain(g(resp, "content")), stop)))
        call.set(out)

    def on_event(self, ev: Any, call: LlmCall) -> None:
        t = g(ev, "type")
        if t == "message_start":
            m = g(ev, "message")
            self._id = self._id or s(g(m, "id"))
            self._model = self._model or s(g(m, "model"))
            self._usage.update(g(m, "usage"))
        elif t == "content_block_start":
            blk = plain(g(ev, "content_block"))
            blk = dict(blk) if isinstance(blk, dict) else {}
            if s(blk.get("text")):
                call.first_token()
            if self.cfg.capture_content:
                idx = num(g(ev, "index"))
                self._blocks[int(idx) if idx is not None else len(self._blocks)] = blk
        elif t == "content_block_delta":
            call.first_token()
            if not self.cfg.capture_content:
                return
            idx = num(g(ev, "index"))
            blk = self._blocks.get(int(idx) if idx is not None else 0)
            if blk is None:
                return
            d = g(ev, "delta")
            dt = g(d, "type")
            if dt == "text_delta":
                blk["text"] = str(blk.get("text") or "") + str(g(d, "text") or "")
            elif dt == "thinking_delta":
                blk["thinking"] = str(blk.get("thinking") or "") + str(g(d, "thinking") or "")
            elif dt == "input_json_delta":
                blk["_json"] = str(blk.get("_json") or "") + str(g(d, "partial_json") or "")
        elif t == "message_delta":
            self._stop = s(g(g(ev, "delta"), "stop_reason")) or self._stop
            self._usage.update(g(ev, "usage"))

    def on_stream_end(self, call: LlmCall) -> None:
        out = self._usage.attrs()
        set_if(out, "gen_ai.response.id", self._id)
        set_if(out, "gen_ai.response.model", self._model)
        if self._stop:
            out["gen_ai.response.finish_reasons"] = [self._stop]
        if self.cfg.capture_content and self._blocks:
            content = []
            for i in sorted(self._blocks):
                blk = dict(self._blocks[i])
                js = blk.pop("_json", None)
                if js is not None:
                    blk["input"] = parse_args(js)
                content.append(blk)
            set_if(out, "gen_ai.output.messages", to_json(_output(content, self._stop)))
        call.set(out)


# --- messages.stream() ----------------------------------------------------------------------------------------------


def _wrap_iterator(ms: Any, call: LlmCall, rec: MessagesRecorder, is_async: bool) -> None:
    """Observes the events of an Anthropic ``MessageStream`` by wrapping its own event iterator (instance only)."""
    it = getattr(ms, "_iterator", None)
    if it is None:
        return

    def done() -> None:
        if not call.ended:
            safely(lambda: rec.on_stream_end(call))
            call.end()

    if is_async:

        async def agen() -> Any:
            try:
                async for ev in it:
                    safely(lambda ev=ev: rec.on_event(ev, call))
                    yield ev
            except GeneratorExit:
                raise
            except BaseException as err:
                call.fail(err)
                raise
            done()

        ms._iterator = agen()  # noqa: SLF001
    else:

        def gen() -> Any:
            try:
                for ev in it:
                    safely(lambda ev=ev: rec.on_event(ev, call))
                    yield ev
            except GeneratorExit:
                raise
            except BaseException as err:
                call.fail(err)
                raise
            done()

        ms._iterator = gen()  # noqa: SLF001


def _finalize(ms: Any, call: LlmCall, rec: MessagesRecorder, exc: Any) -> None:
    if call.ended:
        return
    if exc is not None and not isinstance(exc, GeneratorExit):
        call.fail(exc)
        return
    snap = getattr(ms, "current_message_snapshot", None) if ms is not None else None
    if snap is not None:
        safely(lambda: rec.on_response(snap, call))
    else:
        safely(lambda: rec.on_stream_end(call))
    call.end()


class StreamManager:
    """Wraps ``MessageStreamManager`` / ``AsyncMessageStreamManager`` (``with client.messages.stream(...) as s``)."""

    def __init__(self, mgr: Any, call: LlmCall, rec: MessagesRecorder) -> None:
        self._mgr = mgr
        self._call = call
        self._rec = rec
        self._ms: Any = None

    def __getattr__(self, name: str) -> Any:
        return getattr(self._mgr, name)

    def __enter__(self) -> Any:
        token = self._call.activate()
        try:
            ms = self._mgr.__enter__()
        except BaseException as err:
            self._call.fail(err)
            raise
        finally:
            deactivate(token)
        self._ms = ms
        safely(lambda: _wrap_iterator(ms, self._call, self._rec, False))
        return ms

    def __exit__(self, *exc: Any) -> Any:
        try:
            return self._mgr.__exit__(*exc)
        finally:
            _finalize(self._ms, self._call, self._rec, exc[1] if exc else None)

    async def __aenter__(self) -> Any:
        token = self._call.activate()
        try:
            ms = await self._mgr.__aenter__()
        except BaseException as err:
            self._call.fail(err)
            raise
        finally:
            deactivate(token)
        self._ms = ms
        safely(lambda: _wrap_iterator(ms, self._call, self._rec, True))
        return ms

    async def __aexit__(self, *exc: Any) -> Any:
        try:
            return await self._mgr.__aexit__(*exc)
        finally:
            _finalize(self._ms, self._call, self._rec, exc[1] if exc else None)


def wrap_message_stream(fn: Callable[..., Any], target: Target) -> Callable[..., Any]:
    def wrapper(*args: Any, **kwargs: Any) -> Any:
        try:
            rec = MessagesRecorder(dict(kwargs), settings(target.capture, target.inject))
            rec.stream = True
            call = start_call(target.provider, rec, target.server)
        except Exception as err:  # noqa: BLE001
            record_error("llm", err)
            return fn(*args, **kwargs)
        try:
            mgr = fn(*args, **rec.kwargs)
        except BaseException as err:
            call.fail(err)
            raise
        if not (hasattr(mgr, "__enter__") or hasattr(mgr, "__aenter__")):
            call.end()
            return mgr
        return StreamManager(mgr, call, rec)

    wrapper.__wrapped__ = fn  # type: ignore[attr-defined]
    return wrapper
