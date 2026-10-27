"""FastAPI + OpenAI with TaskTrooper: traces with route templates, an LLM span per completion, continuous profiling.

Run (from sdks/python, with the dev ingest stack up — `make infra && make run-gw && make run-writer`)::

    pip install -e '.[fastapi,httpx]' fastapi uvicorn openai
    TROOPER_DSN=http://tt_dev_ingest_key@localhost:4318/1 python examples/fastapi_openai/app.py
    curl localhost:8000/ask/42?q=hello

Without ``OPENAI_API_KEY`` the OpenAI client talks to an in-process fake (``httpx.MockTransport``), so the example
needs no account. Equivalent zero-code start: ``tasktrooper-run --service ask-api --profiling uvicorn app:app``.
"""

import json
import os
import time

import tasktrooper

# 1. init first: it patches FastAPI (and httpx, logging …) before the app is created.
tasktrooper.init(
    service="ask-api",
    environment=os.environ.get("TROOPER_ENV", "dev"),
    version="0.1.0",
    profiling=True,  # cpu by default; {"types": ["cpu", "wall"]} for wall-clock too
    llm={"capture_content": False},  # prompts / completions stay out unless you opt in (PII)
)

import httpx  # noqa: E402
import openai  # noqa: E402
from fastapi import FastAPI  # noqa: E402


def fake_openai(request: httpx.Request) -> httpx.Response:
    """Stands in for api.openai.com."""
    body = json.loads(request.content)
    time.sleep(0.2)
    answer = f"echo: {body['messages'][-1]['content']}"
    return httpx.Response(
        200,
        json={
            "id": "chatcmpl-example",
            "object": "chat.completion",
            "created": int(time.time()),
            "model": body["model"] + "-2025-01-01",
            "choices": [{"index": 0, "finish_reason": "stop", "message": {"role": "assistant", "content": answer}}],
            "usage": {"prompt_tokens": 42, "completion_tokens": 7, "total_tokens": 49},
        },
    )


if os.environ.get("OPENAI_API_KEY"):
    raw_client = openai.OpenAI()
else:
    raw_client = openai.OpenAI(api_key="fake", http_client=httpx.Client(transport=httpx.MockTransport(fake_openai)))

# 2. wrap the client: GenAI span per call (model, tokens, TTFT for streams, errors)
client = tasktrooper.instrument_openai(raw_client)

app = FastAPI()


@app.get("/ask/{question_id}")  # → trooper.route = /ask/:question_id
def ask(question_id: int, q: str = "hi"):
    r = client.chat.completions.create(model="gpt-4o-mini", messages=[{"role": "user", "content": q}])
    return {"id": question_id, "answer": r.choices[0].message.content}


if __name__ == "__main__":
    import uvicorn

    uvicorn.run(app, host="127.0.0.1", port=8000)
