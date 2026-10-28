"""AI / LLM monitoring (ADR-023, .ai/contracts.md §25).

``instrument_openai(client)`` / ``instrument_anthropic(client)`` return a proxy of the client whose model calls
produce OTel GenAI CLIENT spans (``"<operation> <model>"``): provider, model, tokens (cache tokens included in
``gen_ai.usage.input_tokens``), finish reasons, time to first token for streams (``trooper.llm.ttft_ms``) and
``error.type``. Nothing global is patched; the original client keeps working uninstrumented. The provider SDKs are not
dependencies (duck typing). Prompt / completion content is recorded only with ``capture_content`` (default off, PII;
``TROOPER_LLM_CAPTURE_CONTENT=true`` or ``init(llm={"capture_content": True})``), as GenAI message JSON capped at 32 KB.
"""

from __future__ import annotations

from typing import Any, Dict, Optional

from ._anthropic import MessagesRecorder, wrap_message_stream
from ._core import LlmCall, Target, Wrapper, error_type, instrument, set_if, wrap_create
from ._openai import ChatRecorder, EmbeddingsRecorder, ResponsesRecorder, openai_provider


def _create(factory: Any) -> Wrapper:
    return lambda fn, target: wrap_create(fn, target, factory)


OPENAI_TABLE: Dict[str, Wrapper] = {
    "chat.completions.create": _create(ChatRecorder),
    "chat.completions.parse": _create(ChatRecorder),
    "beta.chat.completions.parse": _create(ChatRecorder),
    "responses.create": _create(ResponsesRecorder),
    "responses.parse": _create(ResponsesRecorder),
    "embeddings.create": _create(EmbeddingsRecorder),
}

ANTHROPIC_TABLE: Dict[str, Wrapper] = {
    "messages.create": _create(MessagesRecorder),
    "messages.stream": wrap_message_stream,
    "beta.messages.create": _create(MessagesRecorder),
    "beta.messages.stream": wrap_message_stream,
}


def instrument_openai(
    client: Any,
    *,
    capture_content: Optional[bool] = None,
    inject_stream_usage: Optional[bool] = None,
    provider: Optional[str] = None,
) -> Any:
    """Instruments an ``openai`` client (``OpenAI``, ``AsyncOpenAI``, ``AzureOpenAI``, OpenAI-compatible endpoints).

    Covers ``chat.completions.create`` / ``.parse``, ``responses.create`` / ``.parse`` and ``embeddings.create``,
    including ``stream=True`` (sync and async iteration). Chat streams only carry token usage with
    ``stream_options={"include_usage": True}``; ``inject_stream_usage=True`` adds it. ``provider`` overrides
    ``gen_ai.provider.name`` (default: derived from ``base_url`` — Azure, Groq, DeepSeek, xAI, Mistral, Gemini,
    OpenRouter, Ollama …, else ``openai``). Returns a proxy; never raises.
    """
    return instrument(
        client,
        OPENAI_TABLE,
        openai_provider,
        {"capture_content": capture_content, "inject_stream_usage": inject_stream_usage, "provider": provider},
    )


def _native_anthropic_traces(client: Any) -> Optional[Dict[str, Any]]:
    """The client's own OpenTelemetry trace options when the SDK traces natively (anthropic ≥ 2.x), else None."""
    opts = getattr(client, "_open_telemetry", None)
    if not isinstance(opts, dict):
        return None
    traces = opts.get("traces")
    if isinstance(traces, dict) and traces.get("enabled") is True:
        return traces
    return None


def instrument_anthropic(client: Any, *, capture_content: Optional[bool] = None, provider: Optional[str] = None) -> Any:
    """Instruments an ``anthropic`` client (``Anthropic`` / ``AsyncAnthropic``).

    Covers ``messages.create`` (incl. ``stream=True``) and ``messages.stream()`` (+ ``beta.messages.*``), sync and
    async. Returns a proxy; never raises.

    Recent ``anthropic`` SDKs emit GenAI spans themselves (``open_telemetry`` client option, on by default). Then the
    client is returned as is — no second span — and the trooper span processor maps those spans to the TaskTrooper
    names (cache write tokens, TTFT, finish reason). With content capture on, a copy of the client with
    ``content_mode="content"`` is returned.
    """
    try:
        native = _native_anthropic_traces(client)
        if native is not None:
            from ._core import settings

            if settings(capture_content, None).capture_content and native.get("content_mode") != "content" and hasattr(client, "copy"):
                opts = dict(client._open_telemetry)  # noqa: SLF001
                opts["traces"] = dict(native, content_mode="content")
                client = client.copy(open_telemetry=opts)
            return client
    except Exception as err:  # noqa: BLE001
        from .._errors import record_error

        record_error("llm", err)
    return instrument(
        client,
        ANTHROPIC_TABLE,
        lambda _c, _h: "anthropic",
        {"capture_content": capture_content, "provider": provider},
    )


class LlmSpan:
    """Manual GenAI span for providers without a wrapper (see :func:`start`)."""

    def __init__(self, call: LlmCall) -> None:
        self._call = call
        self._token: Any = None

    @property
    def span(self) -> Any:
        return self._call.span

    def first_token(self) -> None:
        self._call.first_token()

    def end(
        self,
        *,
        input_tokens: Optional[int] = None,
        output_tokens: Optional[int] = None,
        cache_read_tokens: Optional[int] = None,
        cache_write_tokens: Optional[int] = None,
        response_model: Optional[str] = None,
        finish_reasons: Optional[list] = None,
        error: Optional[BaseException] = None,
    ) -> None:
        """Ends the span. ``input_tokens`` must include cache read / write tokens (OTel GenAI semconv)."""
        try:
            a: Dict[str, Any] = {}
            set_if(a, "gen_ai.usage.input_tokens", input_tokens)
            set_if(a, "gen_ai.usage.output_tokens", output_tokens)
            set_if(a, "gen_ai.usage.cache_read.input_tokens", cache_read_tokens)
            set_if(a, "gen_ai.usage.cache_creation.input_tokens", cache_write_tokens)
            set_if(a, "gen_ai.response.model", response_model)
            set_if(a, "gen_ai.response.finish_reasons", finish_reasons)
            self._call.set(a)
            if error is not None:
                self._call.fail(error)
            else:
                self._call.end()
        except Exception:  # noqa: BLE001
            pass

    def __enter__(self) -> "LlmSpan":
        self._token = self._call.activate()
        return self

    def __exit__(self, et: Any, ev: Any, tb: Any) -> None:
        from ._core import deactivate

        if self._token is not None:
            deactivate(self._token)
        if ev is not None:
            self._call.fail(ev)
        else:
            self._call.end()


def start(provider: str, model: Optional[str] = None, operation: str = "chat", **attributes: Any) -> LlmSpan:
    """Starts a GenAI CLIENT span by hand (``with tasktrooper.llm.start("gcp.gemini", "gemini-2.5-pro") as s: …``)."""
    attrs: Dict[str, Any] = {"gen_ai.provider.name": provider, "gen_ai.operation.name": operation}
    if model:
        attrs["gen_ai.request.model"] = model
    for k, v in attributes.items():
        if v is not None:
            attrs[k if "." in k else "gen_ai.request." + k] = v
    return LlmSpan(LlmCall(f"{operation} {model}" if model else operation, attrs))


__all__ = ["instrument_openai", "instrument_anthropic", "start", "LlmSpan", "error_type", "Target"]
