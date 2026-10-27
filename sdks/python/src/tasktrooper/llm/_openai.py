"""OpenAI (and OpenAI-compatible) recorders: chat completions, responses, embeddings."""

from __future__ import annotations

import re
from typing import Any, Dict, List, Optional, Tuple

from ..config import LlmConfig
from ._core import (
    Attrs,
    LlmCall,
    Recorder,
    as_list,
    g,
    media_part,
    num,
    parse_args,
    plain,
    s,
    set_content,
    set_if,
    text_part,
    to_json,
)


def openai_parts(content: Any) -> List[Dict[str, Any]]:
    if isinstance(content, str):
        return [text_part(content)] if content else []
    out: List[Dict[str, Any]] = []
    for p in as_list(content):
        p = plain(p)
        if not isinstance(p, dict):
            continue
        t = p.get("type")
        if t in ("text", "input_text", "output_text"):
            out.append(text_part(str(p.get("text") or "")))
        elif t == "refusal":
            out.append(text_part(str(p.get("refusal") or "")))
        elif t == "image_url":
            iu = p.get("image_url")
            out.append(media_part("image", iu.get("url") if isinstance(iu, dict) else iu))
        elif t == "input_image":
            out.append(media_part("image", p.get("image_url")))
        elif t == "input_audio":
            ia = p.get("input_audio")
            out.append(media_part("audio", None, f"audio/{ia.get('format')}" if isinstance(ia, dict) else None))
        elif t in ("file", "input_file"):
            out.append({"type": "file"})
        else:
            out.append({"type": str(t or "generic")})
    return out


def _tool_calls(tcs: Any) -> List[Dict[str, Any]]:
    out = []
    for tc in as_list(tcs):
        f = g(tc, "function") or {}
        out.append({"type": "tool_call", "id": g(tc, "id"), "name": g(f, "name"), "arguments": parse_args(g(f, "arguments"))})
    return out


def chat_content(messages: Any) -> Tuple[List[Any], List[Any]]:
    system: List[Any] = []
    inp: List[Any] = []
    for m in as_list(messages):
        m = plain(m)
        if not isinstance(m, dict):
            continue
        role = str(m.get("role") or "user")
        if role in ("system", "developer"):
            system.extend(openai_parts(m.get("content")))
            continue
        parts = openai_parts(m.get("content"))
        if role == "tool":
            c = m.get("content")
            inp.append(
                {
                    "role": "tool",
                    "parts": [{"type": "tool_call_response", "id": m.get("tool_call_id"), "response": c if isinstance(c, str) else parts}],
                }
            )
            continue
        parts.extend(_tool_calls(m.get("tool_calls")))
        inp.append({"role": role, "parts": parts})
    return system, inp


def openai_usage(u: Any) -> Attrs:
    a: Attrs = {}
    if u is None:
        return a
    inp = num(g(u, "prompt_tokens"))
    if inp is None:
        inp = num(g(u, "input_tokens"))
    out = num(g(u, "completion_tokens"))
    if out is None:
        out = num(g(u, "output_tokens"))
    set_if(a, "gen_ai.usage.input_tokens", int(inp) if inp is not None else None)
    set_if(a, "gen_ai.usage.output_tokens", int(out) if out is not None else None)
    d = g(u, "prompt_tokens_details") or g(u, "input_tokens_details")
    cached = num(g(d, "cached_tokens"))
    set_if(a, "gen_ai.usage.cache_read.input_tokens", int(cached) if cached is not None else None)
    return a


def _common(k: Dict[str, Any], attrs: Attrs) -> None:
    set_if(attrs, "gen_ai.request.temperature", num(k.get("temperature")))
    set_if(attrs, "gen_ai.request.top_p", num(k.get("top_p")))


class _Choice:
    __slots__ = ("text", "finish", "tools")

    def __init__(self) -> None:
        self.text = ""
        self.finish: Optional[str] = None
        self.tools: Dict[int, Dict[str, Any]] = {}


class ChatRecorder(Recorder):
    operation = "chat"

    def __init__(self, kwargs: Dict[str, Any], cfg: LlmConfig) -> None:
        super().__init__(kwargs, cfg)
        k = kwargs
        so = k.get("stream_options")
        if self.stream and cfg.inject_stream_usage and not (isinstance(so, dict) and so.get("include_usage")):
            self.kwargs = dict(k, stream_options=dict(so if isinstance(so, dict) else {}, include_usage=True))
        a = self.attrs
        mt = num(k.get("max_completion_tokens"))
        set_if(a, "gen_ai.request.max_tokens", mt if mt is not None else num(k.get("max_tokens")))
        _common(k, a)
        set_if(a, "gen_ai.request.frequency_penalty", num(k.get("frequency_penalty")))
        set_if(a, "gen_ai.request.presence_penalty", num(k.get("presence_penalty")))
        set_if(a, "gen_ai.request.seed", num(k.get("seed")))
        n = num(k.get("n"))
        if n is not None and n != 1:
            a["gen_ai.request.choice.count"] = n
        stop = k.get("stop")
        if isinstance(stop, str):
            a["gen_ai.request.stop_sequences"] = [stop]
        elif isinstance(stop, (list, tuple)):
            set_if(a, "gen_ai.request.stop_sequences", [x for x in stop if isinstance(x, str)])
        if cfg.capture_content:
            system, inp = chat_content(k.get("messages"))
            set_content(a, system, inp)
        self._id: Optional[str] = None
        self._model: Optional[str] = None
        self._usage: Attrs = {}
        self._choices: Dict[int, _Choice] = {}

    def on_response(self, resp: Any, call: LlmCall) -> None:
        if resp is None:
            return
        out = openai_usage(g(resp, "usage"))
        set_if(out, "gen_ai.response.id", s(g(resp, "id")))
        set_if(out, "gen_ai.response.model", s(g(resp, "model")))
        choices = as_list(g(resp, "choices"))
        set_if(out, "gen_ai.response.finish_reasons", [f for f in (s(g(c, "finish_reason")) for c in choices) if f])
        if self.cfg.capture_content:
            msgs = []
            for c in choices:
                m = g(c, "message")
                parts = openai_parts(g(m, "content"))
                if s(g(m, "refusal")):
                    parts.append(text_part(str(g(m, "refusal"))))
                parts.extend(_tool_calls(g(m, "tool_calls")))
                msg: Dict[str, Any] = {"role": str(g(m, "role") or "assistant"), "parts": parts}
                if s(g(c, "finish_reason")):
                    msg["finish_reason"] = g(c, "finish_reason")
                msgs.append(msg)
            set_if(out, "gen_ai.output.messages", to_json(msgs))
        call.set(out)

    def on_event(self, ev: Any, call: LlmCall) -> None:
        if self._id is None:
            self._id = s(g(ev, "id"))
        if self._model is None:
            self._model = s(g(ev, "model"))
        u = g(ev, "usage")
        if u is not None:
            self._usage = openai_usage(u)
        for c in as_list(g(ev, "choices")):
            idx = num(g(c, "index"))
            acc = self._choices.setdefault(int(idx) if idx is not None else 0, _Choice())
            d = g(c, "delta")
            content = s(g(d, "content")) or s(g(d, "refusal"))
            tcs = as_list(g(d, "tool_calls"))
            if content or tcs:
                call.first_token()
            if self.cfg.capture_content:
                if content:
                    acc.text += content
                for tc in tcs:
                    ti = num(g(tc, "index"))
                    t = acc.tools.setdefault(int(ti) if ti is not None else 0, {"args": ""})
                    t.setdefault("id", s(g(tc, "id")))
                    t["id"] = t["id"] or s(g(tc, "id"))
                    f = g(tc, "function")
                    t["name"] = t.get("name") or s(g(f, "name"))
                    if isinstance(g(f, "arguments"), str):
                        t["args"] += g(f, "arguments")
            if s(g(c, "finish_reason")):
                acc.finish = g(c, "finish_reason")

    def on_stream_end(self, call: LlmCall) -> None:
        out: Attrs = dict(self._usage)
        set_if(out, "gen_ai.response.id", self._id)
        set_if(out, "gen_ai.response.model", self._model)
        choices = [self._choices[i] for i in sorted(self._choices)]
        set_if(out, "gen_ai.response.finish_reasons", [c.finish for c in choices if c.finish])
        if self.cfg.capture_content and choices:
            msgs = []
            for c in choices:
                parts: List[Dict[str, Any]] = [text_part(c.text)] if c.text else []
                for i in sorted(c.tools):
                    t = c.tools[i]
                    parts.append({"type": "tool_call", "id": t.get("id"), "name": t.get("name"), "arguments": parse_args(t["args"])})
                m: Dict[str, Any] = {"role": "assistant", "parts": parts}
                if c.finish:
                    m["finish_reason"] = c.finish
                msgs.append(m)
            set_if(out, "gen_ai.output.messages", to_json(msgs))
        call.set(out)


def _responses_items(inp: Any) -> List[Dict[str, Any]]:
    if isinstance(inp, str):
        return [{"role": "user", "parts": [text_part(inp)]}] if inp else []
    out: List[Dict[str, Any]] = []
    for it in as_list(inp):
        it = plain(it)
        if not isinstance(it, dict):
            continue
        t = it.get("type")
        if t == "function_call":
            out.append(
                {
                    "role": "assistant",
                    "parts": [{"type": "tool_call", "id": it.get("call_id"), "name": it.get("name"), "arguments": parse_args(it.get("arguments"))}],
                }
            )
        elif t == "function_call_output":
            out.append({"role": "tool", "parts": [{"type": "tool_call_response", "id": it.get("call_id"), "response": it.get("output")}]})
        elif it.get("role"):
            out.append({"role": str(it["role"]), "parts": openai_parts(it.get("content"))})
    return out


def _responses_output(output: Any, finish: Optional[str]) -> List[Dict[str, Any]]:
    parts: List[Dict[str, Any]] = []
    for it in as_list(output):
        t = g(it, "type")
        if t == "message":
            parts.extend(openai_parts(plain(g(it, "content"))))
        elif t == "function_call":
            parts.append({"type": "tool_call", "id": g(it, "call_id"), "name": g(it, "name"), "arguments": parse_args(g(it, "arguments"))})
    if not parts:
        return []
    m: Dict[str, Any] = {"role": "assistant", "parts": parts}
    if finish:
        m["finish_reason"] = finish
    return [m]


class _ResponseFailed(Exception):
    def __init__(self, message: str, type_: str) -> None:
        super().__init__(message)
        self.type = type_


class ResponsesRecorder(Recorder):
    operation = "chat"

    def __init__(self, kwargs: Dict[str, Any], cfg: LlmConfig) -> None:
        super().__init__(kwargs, cfg)
        k = kwargs
        a = self.attrs
        set_if(a, "gen_ai.request.max_tokens", num(k.get("max_output_tokens")))
        _common(k, a)
        conv = k.get("conversation")
        set_if(a, "gen_ai.conversation.id", s(conv) or s(g(conv, "id")))
        if cfg.capture_content:
            items = _responses_items(k.get("input"))
            system: List[Any] = []
            if s(k.get("instructions")):
                system.append(text_part(str(k["instructions"])))
            rest = []
            for m in items:
                if m["role"] in ("system", "developer"):
                    system.extend(m["parts"])
                else:
                    rest.append(m)
            set_content(a, system, rest)
        self._done = False

    def on_response(self, resp: Any, call: LlmCall) -> None:
        if resp is None:
            return
        out = openai_usage(g(resp, "usage"))
        set_if(out, "gen_ai.response.id", s(g(resp, "id")))
        set_if(out, "gen_ai.response.model", s(g(resp, "model")))
        finish = s(g(g(resp, "incomplete_details"), "reason")) or s(g(resp, "status"))
        if finish:
            out["gen_ai.response.finish_reasons"] = [finish]
        if self.cfg.capture_content:
            set_if(out, "gen_ai.output.messages", to_json(_responses_output(g(resp, "output"), finish)))
        call.set(out)
        self._done = True
        if g(resp, "status") == "failed":
            e = g(resp, "error")
            call.fail(_ResponseFailed(s(g(e, "message")) or "response failed", s(g(e, "code")) or "response_failed"))

    def on_event(self, ev: Any, call: LlmCall) -> None:
        t = s(g(ev, "type")) or ""
        if t.endswith(".delta") and t != "response.audio.transcript.delta":
            call.first_token()
        if t == "response.created":
            r = g(ev, "response")
            out: Attrs = {}
            set_if(out, "gen_ai.response.id", s(g(r, "id")))
            set_if(out, "gen_ai.response.model", s(g(r, "model")))
            call.set(out)
        if t in ("response.completed", "response.incomplete", "response.failed"):
            self.on_response(g(ev, "response"), call)
        if t == "error" and not self._done:
            call.fail(_ResponseFailed(s(g(ev, "message")) or "stream error", s(g(ev, "code")) or "stream_error"))


class EmbeddingsRecorder(Recorder):
    operation = "embeddings"

    def __init__(self, kwargs: Dict[str, Any], cfg: LlmConfig) -> None:
        super().__init__(kwargs, cfg)
        self.stream = False
        if s(kwargs.get("encoding_format")):
            self.attrs["gen_ai.request.encoding_formats"] = [kwargs["encoding_format"]]
        set_if(self.attrs, "gen_ai.embeddings.dimension.count", num(kwargs.get("dimensions")))

    def on_response(self, resp: Any, call: LlmCall) -> None:
        out: Attrs = {}
        set_if(out, "gen_ai.response.model", s(g(resp, "model")))
        u = g(resp, "usage")
        t = num(g(u, "prompt_tokens"))
        if t is None:
            t = num(g(u, "total_tokens"))
        set_if(out, "gen_ai.usage.input_tokens", int(t) if t is not None else None)
        call.set(out)


OPENAI_HOSTS = [
    (re.compile(r"(^|\.)openai\.azure\.com$"), "azure.ai.openai"),
    (re.compile(r"(^|\.)services\.ai\.azure\.com$"), "azure.ai.openai"),
    (re.compile(r"(^|\.)groq\.com$"), "groq"),
    (re.compile(r"(^|\.)deepseek\.com$"), "deepseek"),
    (re.compile(r"(^|\.)x\.ai$"), "x_ai"),
    (re.compile(r"(^|\.)mistral\.ai$"), "mistral_ai"),
    (re.compile(r"(^|\.)generativelanguage\.googleapis\.com$"), "gcp.gemini"),
    (re.compile(r"(^|\.)aiplatform\.googleapis\.com$"), "gcp.vertex_ai"),
    (re.compile(r"(^|\.)cohere\.(ai|com)$"), "cohere"),
    (re.compile(r"(^|\.)perplexity\.ai$"), "perplexity"),
    (re.compile(r"(^|\.)openrouter\.ai$"), "openrouter"),
    (re.compile(r"(^|\.)together\.(ai|xyz)$"), "together_ai"),
    (re.compile(r"(^|\.)fireworks\.ai$"), "fireworks_ai"),
    (re.compile(r"(^|\.)cerebras\.ai$"), "cerebras"),
]


def openai_provider(client: Any, host: Optional[str]) -> str:
    """``gen_ai.provider.name`` of an OpenAI-compatible client from its ``base_url`` host."""
    if not host:
        return "openai"
    for rx, name in OPENAI_HOSTS:
        if rx.search(host):
            return name
    try:
        if ":11434" in str(g(client, "base_url")):
            return "ollama"
    except Exception:  # noqa: BLE001
        pass
    return "openai"
