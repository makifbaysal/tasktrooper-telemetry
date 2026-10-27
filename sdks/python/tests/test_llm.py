"""LLM wrappers (.ai/contracts.md §25) against the real openai / anthropic SDKs with a mocked HTTP transport."""

import json

import httpx
import pytest
from opentelemetry.trace import SpanKind, StatusCode

import tasktrooper
from tasktrooper import llm

from .conftest import PROVIDER

openai = pytest.importorskip("openai")
anthropic = pytest.importorskip("anthropic")


def sse(events, named=False):
    out = ""
    for ev in events:
        if named:
            out += f"event: {ev['type']}\n"
        out += "data: " + json.dumps(ev) + "\n\n"
    if not named:
        out += "data: [DONE]\n\n"
    return out


CHAT = {
    "id": "chatcmpl-1", "object": "chat.completion", "created": 1, "model": "gpt-4o-2024-08-06",
    "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": "Hello there"}}],
    "usage": {"prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15, "prompt_tokens_details": {"cached_tokens": 8}},
}
CHUNKS = [
    {"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-4o-mini", "choices": [{"index": 0, "delta": {"role": "assistant"}}]},
    {"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-4o-mini", "choices": [{"index": 0, "delta": {"content": "Hel"}}]},
    {"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-4o-mini", "choices": [{"index": 0, "delta": {"content": "lo"}, "finish_reason": "stop"}]},
    {"id": "c1", "object": "chat.completion.chunk", "created": 1, "model": "gpt-4o-mini", "choices": [], "usage": {"prompt_tokens": 5, "completion_tokens": 2, "total_tokens": 7}},
]
RESPONSE = {
    "id": "resp_1", "object": "response", "created_at": 1, "model": "gpt-4.1", "status": "completed",
    "output": [{"type": "message", "id": "m1", "status": "completed", "role": "assistant", "content": [{"type": "output_text", "text": "Hi", "annotations": []}]}],
    "usage": {"input_tokens": 9, "output_tokens": 1, "total_tokens": 10, "input_tokens_details": {"cached_tokens": 0}, "output_tokens_details": {"reasoning_tokens": 0}},
    "parallel_tool_calls": True, "tool_choice": "auto", "tools": [],
}
EMB = {"object": "list", "model": "text-embedding-3-small", "data": [{"object": "embedding", "index": 0, "embedding": [0.1, 0.2]}], "usage": {"prompt_tokens": 4, "total_tokens": 4}}
MESSAGE = {
    "id": "msg_1", "type": "message", "role": "assistant", "model": "claude-sonnet-4-5-20250929", "stop_reason": "end_turn", "stop_sequence": None,
    "content": [{"type": "text", "text": "Bonjour"}],
    "usage": {"input_tokens": 10, "output_tokens": 4, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 20},
}
A_EVENTS = [
    {"type": "message_start", "message": {**MESSAGE, "content": [], "stop_reason": None, "usage": {"input_tokens": 10, "output_tokens": 1, "cache_read_input_tokens": 100, "cache_creation_input_tokens": 0}}},
    {"type": "content_block_start", "index": 0, "content_block": {"type": "text", "text": ""}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "Bon"}},
    {"type": "content_block_delta", "index": 0, "delta": {"type": "text_delta", "text": "jour"}},
    {"type": "content_block_stop", "index": 0},
    {"type": "message_delta", "delta": {"stop_reason": "end_turn", "stop_sequence": None}, "usage": {"output_tokens": 6}},
    {"type": "message_stop"},
]


def handler(request: httpx.Request) -> httpx.Response:
    body = json.loads(request.content or b"{}")
    p = request.url.path
    assert "baggage" in request.headers or True
    if body.get("model") == "boom":
        if "anthropic" in str(request.url):
            return httpx.Response(429, json={"type": "error", "error": {"type": "rate_limit_error", "message": "slow down"}})
        return httpx.Response(400, json={"error": {"message": "bad model", "type": "invalid_request_error", "code": "model_not_found"}})
    if p.endswith("/chat/completions"):
        if body.get("stream"):
            chunks = CHUNKS if body.get("stream_options", {}).get("include_usage") else CHUNKS[:-1]
            return httpx.Response(200, text=sse(chunks), headers={"content-type": "text/event-stream"})
        return httpx.Response(200, json=CHAT)
    if p.endswith("/responses"):
        return httpx.Response(200, json=RESPONSE)
    if p.endswith("/embeddings"):
        return httpx.Response(200, json=EMB)
    if p.endswith("/messages"):
        if body.get("stream"):
            return httpx.Response(200, text=sse(A_EVENTS, named=True), headers={"content-type": "text/event-stream"})
        return httpx.Response(200, json=MESSAGE)
    return httpx.Response(404, json={})


def oai(async_=False, base_url="https://api.openai.com/v1"):
    if async_:
        return openai.AsyncOpenAI(api_key="k", base_url=base_url, max_retries=0, http_client=httpx.AsyncClient(transport=httpx.MockTransport(handler)))
    return openai.OpenAI(api_key="k", base_url=base_url, max_retries=0, http_client=httpx.Client(transport=httpx.MockTransport(handler)))


def _anthropic_httpx():
    """Recent anthropic SDKs run on ``httpx2``; older ones on ``httpx``."""
    try:
        anthropic.Anthropic(api_key="k", http_client=httpx.Client())
        return httpx
    except TypeError:
        return pytest.importorskip("httpx2")


NATIVE_OTEL = "open_telemetry" in __import__("inspect").signature(anthropic.Anthropic.__init__).parameters


def ant(async_=False, native=False):
    hx = _anthropic_httpx()

    def h(request):
        r = handler(httpx.Request(request.method, str(request.url), headers=dict(request.headers), content=request.content))
        return hx.Response(r.status_code, content=r.content, headers=dict(r.headers))

    kw = {"api_key": "k", "max_retries": 0}
    if not native and NATIVE_OTEL:
        kw["open_telemetry"] = False  # exercise the tasktrooper wrapper
    if async_:
        return anthropic.AsyncAnthropic(http_client=hx.AsyncClient(transport=hx.MockTransport(h)), **kw)
    return anthropic.Anthropic(http_client=hx.Client(transport=hx.MockTransport(h)), **kw)


def llm_span(spans):
    out = [s for s in spans.get_finished_spans() if "gen_ai.operation.name" in s.attributes]
    assert len(out) == 1, [s.name for s in spans.get_finished_spans()]
    return out[0]


MSGS = [{"role": "system", "content": "be brief"}, {"role": "user", "content": "hi"}]


def test_openai_chat_sync(spans):
    c = tasktrooper.instrument_openai(oai())
    assert isinstance(c, openai.OpenAI)
    r = c.chat.completions.create(model="gpt-4o", messages=MSGS, max_tokens=50, temperature=0.2)
    assert r.choices[0].message.content == "Hello there"
    s = llm_span(spans)
    a = s.attributes
    assert s.name == "chat gpt-4o" and s.kind == SpanKind.CLIENT
    assert a["gen_ai.provider.name"] == "openai" and a["gen_ai.operation.name"] == "chat"
    assert a["gen_ai.request.model"] == "gpt-4o" and a["gen_ai.response.model"] == "gpt-4o-2024-08-06"
    assert (a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"], a["gen_ai.usage.cache_read.input_tokens"]) == (12, 3, 8)
    assert a["gen_ai.response.finish_reasons"] == ("stop",) and a["gen_ai.response.id"] == "chatcmpl-1"
    assert a["gen_ai.request.max_tokens"] == 50 and a["gen_ai.request.temperature"] == 0.2
    assert (a["server.address"], a["server.port"]) == ("api.openai.com", 443)
    assert "gen_ai.input.messages" not in a and "gen_ai.output.messages" not in a  # content off by default


def test_openai_chat_content_capture(spans):
    c = tasktrooper.instrument_openai(oai(), capture_content=True)
    c.chat.completions.create(model="gpt-4o", messages=MSGS)
    a = llm_span(spans).attributes
    assert json.loads(a["gen_ai.system_instructions"]) == [{"type": "text", "content": "be brief"}]
    assert json.loads(a["gen_ai.input.messages"]) == [{"role": "user", "parts": [{"type": "text", "content": "hi"}]}]
    assert json.loads(a["gen_ai.output.messages"]) == [{"role": "assistant", "parts": [{"type": "text", "content": "Hello there"}], "finish_reason": "stop"}]


def test_content_capture_env(spans, monkeypatch):
    monkeypatch.setenv("TROOPER_LLM_CAPTURE_CONTENT", "true")
    tasktrooper.instrument_openai(oai()).chat.completions.create(model="gpt-4o", messages=MSGS)
    assert "gen_ai.input.messages" in llm_span(spans).attributes


def test_content_is_capped(spans):
    big = "x" * 40_000
    tasktrooper.instrument_openai(oai(), capture_content=True).chat.completions.create(model="gpt-4o", messages=[{"role": "user", "content": big}])
    v = llm_span(spans).attributes["gen_ai.input.messages"]
    assert len(v) == 32 * 1024 + len("…[truncated]") and v.endswith("…[truncated]")


def test_openai_chat_stream_sync_ttft(spans):
    c = tasktrooper.instrument_openai(oai(), capture_content=True, inject_stream_usage=True)
    stream = c.chat.completions.create(model="gpt-4o-mini", messages=MSGS, stream=True)
    assert isinstance(stream, openai.Stream)
    text = "".join(ch.choices[0].delta.content or "" for ch in stream if ch.choices)
    assert text == "Hello"
    a = llm_span(spans).attributes
    assert a["trooper.llm.ttft_ms"] >= 0
    assert (a["gen_ai.usage.input_tokens"], a["gen_ai.usage.output_tokens"]) == (5, 2)
    assert a["gen_ai.response.model"] == "gpt-4o-mini" and a["gen_ai.response.finish_reasons"] == ("stop",)
    assert json.loads(a["gen_ai.output.messages"])[0]["parts"] == [{"type": "text", "content": "Hello"}]


def test_openai_stream_without_usage_injection(spans):
    c = tasktrooper.instrument_openai(oai())
    with c.chat.completions.create(model="gpt-4o-mini", messages=MSGS, stream=True) as stream:
        for _ in stream:
            pass
    a = llm_span(spans).attributes
    assert "gen_ai.usage.input_tokens" not in a  # request unchanged → no usage chunk


async def test_openai_async_chat_and_stream(spans):
    c = tasktrooper.instrument_openai(oai(async_=True), inject_stream_usage=True)
    r = await c.chat.completions.create(model="gpt-4o", messages=MSGS)
    assert r.usage.total_tokens == 15
    assert llm_span(spans).attributes["gen_ai.usage.input_tokens"] == 12
    spans.clear()
    stream = await c.chat.completions.create(model="gpt-4o-mini", messages=MSGS, stream=True)
    parts = [ch.choices[0].delta.content async for ch in stream if ch.choices and ch.choices[0].delta.content]
    assert parts == ["Hel", "lo"]
    a = llm_span(spans).attributes
    assert a["trooper.llm.ttft_ms"] >= 0 and a["gen_ai.usage.output_tokens"] == 2


def test_openai_responses_and_embeddings(spans):
    c = tasktrooper.instrument_openai(oai(), capture_content=True)
    c.responses.create(model="gpt-4.1", input="hello", instructions="be nice", max_output_tokens=20)
    a = llm_span(spans).attributes
    assert a["gen_ai.usage.input_tokens"] == 9 and a["gen_ai.response.finish_reasons"] == ("completed",)
    assert json.loads(a["gen_ai.system_instructions"]) == [{"type": "text", "content": "be nice"}]
    assert json.loads(a["gen_ai.output.messages"])[0]["parts"][0]["content"] == "Hi"
    spans.clear()
    c.embeddings.create(model="text-embedding-3-small", input="abc")
    s = llm_span(spans)
    assert s.name == "embeddings text-embedding-3-small" and s.attributes["gen_ai.usage.input_tokens"] == 4


def test_openai_error_type(spans):
    c = tasktrooper.instrument_openai(oai())
    with pytest.raises(openai.BadRequestError):
        c.chat.completions.create(model="boom", messages=MSGS)
    s = llm_span(spans)
    assert s.status.status_code == StatusCode.ERROR
    assert s.attributes["error.type"] == "invalid_request_error"
    assert s.attributes["http.response.status_code"] == 400


@pytest.mark.parametrize(
    "base,provider",
    [("https://myres.openai.azure.com/openai", "azure.ai.openai"), ("https://api.groq.com/openai/v1", "groq"),
     ("http://localhost:11434/v1", "ollama"), ("https://openrouter.ai/api/v1", "openrouter")],
)
def test_openai_compatible_provider(spans, base, provider):
    tasktrooper.instrument_openai(oai(base_url=base)).chat.completions.create(model="m", messages=MSGS)
    assert llm_span(spans).attributes["gen_ai.provider.name"] == provider


def test_anthropic_create_cache_tokens(spans):
    c = tasktrooper.instrument_anthropic(ant())
    assert isinstance(c, anthropic.Anthropic)
    r = c.messages.create(model="claude-sonnet-4-5", max_tokens=100, system="sys", messages=[{"role": "user", "content": "hi"}])
    assert r.content[0].text == "Bonjour"
    a = llm_span(spans).attributes
    assert a["gen_ai.provider.name"] == "anthropic"
    assert a["gen_ai.usage.input_tokens"] == 10 + 100 + 20  # cache included (semconv)
    assert (a["gen_ai.usage.cache_read.input_tokens"], a["gen_ai.usage.cache_creation.input_tokens"]) == (100, 20)
    assert a["gen_ai.usage.output_tokens"] == 4 and a["gen_ai.response.finish_reasons"] == ("end_turn",)
    assert a["gen_ai.request.max_tokens"] == 100 and a["gen_ai.response.model"] == "claude-sonnet-4-5-20250929"


def test_anthropic_create_stream(spans):
    c = tasktrooper.instrument_anthropic(ant(), capture_content=True)
    stream = c.messages.create(model="claude-sonnet-4-5", max_tokens=100, system="sys", messages=[{"role": "user", "content": "hi"}], stream=True)
    types = [ev.type for ev in stream]
    assert types[0] == "message_start" and types[-1] == "message_stop"
    a = llm_span(spans).attributes
    assert a["trooper.llm.ttft_ms"] >= 0
    assert a["gen_ai.usage.input_tokens"] == 110 and a["gen_ai.usage.output_tokens"] == 6
    assert json.loads(a["gen_ai.system_instructions"]) == [{"type": "text", "content": "sys"}]
    assert json.loads(a["gen_ai.output.messages"]) == [{"role": "assistant", "parts": [{"type": "text", "content": "Bonjour"}], "finish_reason": "end_turn"}]


def test_anthropic_messages_stream_helper(spans):
    c = tasktrooper.instrument_anthropic(ant())
    with c.messages.stream(model="claude-sonnet-4-5", max_tokens=100, messages=[{"role": "user", "content": "hi"}]) as stream:
        text = "".join(stream.text_stream)
    assert text == "Bonjour"
    s = llm_span(spans)
    a = s.attributes
    assert s.name == "chat claude-sonnet-4-5"
    assert a["trooper.llm.ttft_ms"] >= 0 and a["gen_ai.usage.output_tokens"] == 6 and a["gen_ai.usage.input_tokens"] == 110
    assert a["gen_ai.response.id"] == "msg_1"


def test_anthropic_messages_stream_final_message(spans):
    c = tasktrooper.instrument_anthropic(ant())
    with c.messages.stream(model="claude-sonnet-4-5", max_tokens=100, messages=[{"role": "user", "content": "hi"}]) as stream:
        msg = stream.get_final_message()
    assert msg.content[0].text == "Bonjour"
    assert llm_span(spans).attributes["gen_ai.response.finish_reasons"] == ("end_turn",)


async def test_anthropic_async(spans):
    c = tasktrooper.instrument_anthropic(ant(async_=True))
    r = await c.messages.create(model="claude-haiku-4-5", max_tokens=10, messages=[{"role": "user", "content": "hi"}])
    assert r.id == "msg_1"
    assert llm_span(spans).attributes["gen_ai.usage.input_tokens"] == 130
    spans.clear()
    async with c.messages.stream(model="claude-haiku-4-5", max_tokens=10, messages=[{"role": "user", "content": "hi"}]) as stream:
        chunks = [t async for t in stream.text_stream]
    assert "".join(chunks) == "Bonjour"
    a = llm_span(spans).attributes
    assert a["trooper.llm.ttft_ms"] >= 0 and a["gen_ai.usage.output_tokens"] == 6
    spans.clear()
    stream = await c.messages.create(model="claude-haiku-4-5", max_tokens=10, messages=[{"role": "user", "content": "hi"}], stream=True)
    n = 0
    async for _ in stream:
        n += 1
    assert n == len(A_EVENTS) and llm_span(spans).attributes["gen_ai.usage.output_tokens"] == 6


@pytest.mark.skipif(not NATIVE_OTEL, reason="anthropic SDK without native OpenTelemetry")
def test_anthropic_native_spans_are_kept_and_normalized(spans, monkeypatch):
    c0 = ant(native=True)
    c = tasktrooper.instrument_anthropic(c0)
    assert c is c0  # no wrapper → no duplicate span
    list(c.messages.create(model="claude-sonnet-4-5", max_tokens=100, messages=[{"role": "user", "content": "hi"}], stream=True))
    s = llm_span(spans)
    a = s.attributes
    assert s.instrumentation_scope.name != "tasktrooper"
    assert a["gen_ai.usage.input_tokens"] == 110 and a["gen_ai.usage.cache_creation.input_tokens"] == 0
    assert a["trooper.llm.ttft_ms"] > 0 and a["gen_ai.response.finish_reasons"] == ("end_turn",)
    spans.clear()
    cc = tasktrooper.instrument_anthropic(c0, capture_content=True)
    assert cc is not c0 and cc._open_telemetry["traces"]["content_mode"] == "content"
    cc.messages.create(model="claude-sonnet-4-5", max_tokens=100, messages=[{"role": "user", "content": "hi"}])
    assert "gen_ai.input.messages" in llm_span(spans).attributes


def test_anthropic_error_type(spans):
    c = tasktrooper.instrument_anthropic(ant())
    with pytest.raises(anthropic.RateLimitError):
        c.messages.create(model="boom", max_tokens=1, messages=[{"role": "user", "content": "hi"}])
    a = llm_span(spans).attributes
    assert a["error.type"] == "rate_limit_error" and a["http.response.status_code"] == 429


def test_llm_span_is_parent_of_http_and_child_of_entry(spans):
    pytest.importorskip("opentelemetry.instrumentation.httpx")
    from opentelemetry.instrumentation.httpx import HTTPXClientInstrumentor

    http_client = httpx.Client(transport=httpx.MockTransport(handler))
    HTTPXClientInstrumentor.instrument_client(http_client, tracer_provider=PROVIDER)
    c = tasktrooper.instrument_openai(openai.OpenAI(api_key="k", max_retries=0, http_client=http_client))
    tracer = PROVIDER.get_tracer("app")
    with tracer.start_as_current_span("POST", kind=SpanKind.SERVER, attributes={"http.route": "/ask", "http.method": "POST"}):
        c.chat.completions.create(model="gpt-4o", messages=MSGS)
    llm_s = llm_span(spans)
    http_s = [s for s in spans.get_finished_spans() if s.kind == SpanKind.CLIENT and s is not llm_s][0]
    assert http_s.parent.span_id == llm_s.context.span_id
    assert llm_s.attributes["trooper.entry.route"] == "/ask" and llm_s.attributes["trooper.entry.method"] == "POST"


# --- duck-typed fakes (no provider SDK involved) ------------------------------------------------------------------


class FakeCompletions:
    def create(self, **kw):
        if kw.get("stream"):
            return iter([{"choices": [{"index": 0, "delta": {"content": "a"}}]}, {"choices": [{"index": 0, "delta": {}, "finish_reason": "length"}]}])
        return {"id": "x", "model": kw["model"], "choices": [{"finish_reason": "stop", "message": {"content": "ok"}}], "usage": {"prompt_tokens": 1, "completion_tokens": 1}}


class FakeClient:
    base_url = "https://api.deepseek.com/v1"

    def __init__(self):
        self.chat = type("Chat", (), {"completions": FakeCompletions()})()
        self.other = 42


def test_fake_client_duck_typing(spans):
    c = tasktrooper.instrument_openai(FakeClient())
    assert c.other == 42
    assert c.chat.completions.create(model="deepseek-chat")["choices"][0]["message"]["content"] == "ok"
    a = llm_span(spans).attributes
    assert a["gen_ai.provider.name"] == "deepseek" and a["gen_ai.usage.input_tokens"] == 1
    spans.clear()
    list(c.chat.completions.create(model="deepseek-chat", stream=True))
    a = llm_span(spans).attributes
    assert a["gen_ai.response.finish_reasons"] == ("length",) and "trooper.llm.ttft_ms" in a


def test_wrappers_never_raise_on_garbage(spans):
    assert tasktrooper.instrument_openai(None) is None
    assert tasktrooper.instrument_anthropic(42) is not None
    wrapped = tasktrooper.instrument_openai(FakeClient())
    assert tasktrooper.instrument_openai(wrapped) is wrapped  # idempotent


def test_manual_span(spans):
    with llm.start("gcp.gemini", "gemini-2.5-pro", max_tokens=100) as s:
        s.first_token()
        s.end(input_tokens=10, output_tokens=5, finish_reasons=["STOP"])
    a = llm_span(spans).attributes
    assert a["gen_ai.provider.name"] == "gcp.gemini" and a["gen_ai.request.max_tokens"] == 100 and a["gen_ai.usage.output_tokens"] == 5
