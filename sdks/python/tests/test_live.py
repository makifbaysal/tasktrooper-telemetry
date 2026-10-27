"""Contract test against a locally running ingest-gw + ingest-writer + ClickHouse (skipped when not reachable).

Sends spans (FastAPI request with caller baggage + DB-like child), an LLM span and a CPU profile with route sections
from a fresh interpreter, then polls ClickHouse ``spans``, ``profile_stacks`` and ``llm_1m``. Rows are deleted
afterwards. Env: ``TROOPER_E2E_DSN`` (default ``http://tt_dev_ingest_key@localhost:4318/1``), ``TROOPER_E2E_CLICKHOUSE``
(default ``http://localhost:8123``, user/password ``trooper``).
"""

import json
import os
import time
import urllib.parse
import urllib.request
import uuid

import pytest

from tasktrooper.config import parse_dsn

from .conftest import run_py

pytestmark = pytest.mark.live

DSN = os.environ.get("TROOPER_E2E_DSN", "http://tt_dev_ingest_key@localhost:4318/1")
CH = os.environ.get("TROOPER_E2E_CLICKHOUSE", "http://localhost:8123")
CH_AUTH = {"user": os.environ.get("TROOPER_E2E_CLICKHOUSE_USER", "trooper"), "password": os.environ.get("TROOPER_E2E_CLICKHOUSE_PASSWORD", "trooper")}


def _reachable(url: str) -> bool:
    try:
        with urllib.request.urlopen(url, timeout=2) as r:
            return r.status == 200
    except Exception:  # noqa: BLE001
        return False


def ch(sql: str) -> list:
    q = urllib.parse.urlencode({**CH_AUTH, "database": "telemetry", "default_format": "JSONEachRow"})
    req = urllib.request.Request(f"{CH}/?{q}", data=sql.encode(), method="POST")
    with urllib.request.urlopen(req, timeout=10) as r:
        return [json.loads(line) for line in r.read().decode().splitlines() if line.strip()]


APP = r"""
import time, tasktrooper
from fastapi import FastAPI
from fastapi.testclient import TestClient
from opentelemetry import trace
from opentelemetry.trace import SpanKind

h = tasktrooper.init(service=SERVICE, environment="e2e", version="0.0.1",
                     profiling={"types": ["cpu", "wall"], "period_s": 2, "cpu_duration_s": 1.5, "hz": 200})
from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

class Completions:
    def create(self, **kw):
        time.sleep(0.01)
        return {"id": "chatcmpl-live", "model": kw["model"] + "-2025", "choices": [{"finish_reason": "stop", "message": {"content": "ok"}}],
                "usage": {"prompt_tokens": 120, "completion_tokens": 7, "prompt_tokens_details": {"cached_tokens": 100}}}

class FakeOpenAI:
    base_url = "https://api.openai.com/v1"
    def __init__(self):
        self.chat = type("Chat", (), {"completions": Completions()})()

llm = tasktrooper.instrument_openai(FakeOpenAI())
app = FastAPI()
tracer = trace.get_tracer("live")

def burn(seconds):
    t = time.time() + seconds
    x = 0
    while time.time() < t:
        x += 1
    return x

@app.get("/live/{item_id}")
def live(item_id: int):
    with tracer.start_as_current_span("SELECT items", kind=SpanKind.CLIENT, attributes={"db.system": "postgresql", "db.namespace": "shop"}):
        pass
    llm.chat.completions.create(model="gpt-4o-mini", messages=[{"role": "user", "content": "hi"}])
    burn(1.6)
    return {"ok": item_id}

FastAPIInstrumentor.instrument_app(app)
c = TestClient(app)
assert c.get("/live/42", headers={"baggage": "tt.svc=py-live-web"}).status_code == 200
time.sleep(1.0)
tasktrooper.shutdown()
print("errors", tasktrooper.internal_error_count())
"""


@pytest.fixture(scope="module")
def live_service():
    d = parse_dsn(DSN)
    if not _reachable(d.endpoint + "/healthz"):
        pytest.skip(f"ingest-gw not reachable at {d.endpoint}")
    try:
        ch("SELECT 1")
    except Exception as e:  # noqa: BLE001
        pytest.skip(f"ClickHouse not reachable at {CH}: {e}")
    pytest.importorskip("fastapi")
    pytest.importorskip("opentelemetry.instrumentation.fastapi")
    service = "py-live-" + uuid.uuid4().hex[:8]
    r = run_py(f"SERVICE = {service!r}\n" + APP, {"TROOPER_DSN": DSN}, timeout=120)
    assert r.returncode == 0, r.stderr
    assert r.stdout.strip() == "errors 0", r.stdout + r.stderr
    yield service
    for t in ("spans", "profile_stacks", "profiles", "llm_1m", "llm_1h", "llm_content", "red_1m", "edges_1m"):
        try:
            ch(f"ALTER TABLE {t} DELETE WHERE service = '{service}'")
        except Exception:  # noqa: BLE001
            pass


def poll(sql: str, ok, timeout: float = 30.0) -> list:
    deadline = time.time() + timeout
    rows: list = []
    while time.time() < deadline:
        rows = ch(sql)
        if ok(rows):
            return rows
        time.sleep(0.5)
    return rows


def test_live_spans(live_service):
    rows = poll(
        f"SELECT name, kind, is_entry, route, http_method, caller, entry_route, entry_method, dep, dep_kind, sample_weight "
        f"FROM spans WHERE service = '{live_service}'",
        lambda r: len(r) >= 3,
    )
    by_name = {r["name"]: r for r in rows}
    entry = [r for r in rows if r["is_entry"]]
    assert len(entry) == 1, rows
    e = entry[0]
    assert (e["route"], e["http_method"], e["caller"]) == ("/live/:item_id", "GET", "py-live-web"), e
    assert e["entry_route"] == "/live/:item_id"
    db = by_name["SELECT items"]
    assert db["entry_route"] == "/live/:item_id" and db["entry_method"] == "GET" and "postgresql" in db["dep"]
    llm = by_name["chat gpt-4o-mini"]
    assert llm["dep_kind"] == "ai" and llm["entry_route"] == "/live/:item_id", llm


def test_live_llm_rollup(live_service):
    rows = poll(
        f"SELECT provider, model, operation, route, method, sum(calls) AS calls, sum(input_tokens) AS input_tokens, "
        f"sum(output_tokens) AS output_tokens, sum(cache_read_tokens) AS cache_read FROM llm_1m WHERE service = '{live_service}' "
        f"GROUP BY provider, model, operation, route, method",
        lambda r: len(r) >= 1,
    )
    assert len(rows) == 1, rows
    r = rows[0]
    assert (r["provider"], r["operation"], r["route"], r["method"]) == ("openai", "chat", "/live/:item_id", "GET"), r
    assert r["model"] in ("gpt-4o-mini-2025", "gpt-4o-mini")
    assert (int(r["calls"]), int(r["input_tokens"]), int(r["output_tokens"]), int(r["cache_read"])) == (1, 120, 7, 100)


def test_live_profiles(live_service):
    rows = poll(
        f"SELECT type, unit, runtime, route, method, arrayStringConcat(stack, ';') AS s, value FROM profile_stacks "
        f"WHERE service = '{live_service}'",
        lambda r: any(x["route"] for x in r) and {x["type"] for x in r} >= {"cpu", "wall"},
    )
    assert rows, "no profile_stacks rows"
    assert {r["runtime"] for r in rows} == {"python"}
    assert {r["type"] for r in rows} >= {"cpu", "wall"}
    routed = [r for r in rows if r["route"] == "/live/:item_id" and r["method"] == "GET"]
    assert routed, sorted({(r["route"], r["method"]) for r in rows})
    assert any("burn (<string>)" in r["s"] for r in routed if r["type"] == "cpu"), [r["s"][-120:] for r in routed][:5]
    assert all(r["unit"] == "nanoseconds" for r in rows)
