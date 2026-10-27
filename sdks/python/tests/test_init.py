"""``init()`` in fresh interpreters: OTLP wire contract (decoded protobuf), idempotency, never-raise, launcher."""

import json
import os
import sys

from opentelemetry.proto.collector.logs.v1.logs_service_pb2 import ExportLogsServiceRequest
from opentelemetry.proto.collector.metrics.v1.metrics_service_pb2 import ExportMetricsServiceRequest
from opentelemetry.proto.collector.trace.v1.trace_service_pb2 import ExportTraceServiceRequest

from tasktrooper._run import build_env

from .conftest import run_py


def _attrs(kvs):
    out = {}
    for kv in kvs:
        v = kv.value
        out[kv.key] = getattr(v, v.WhichOneof("value")) if v.WhichOneof("value") else None
    return out


APP = r"""
import logging, tasktrooper
from opentelemetry import trace
from opentelemetry.trace import SpanKind
h = tasktrooper.init(service="py-contract", environment="test", version="1.2.3", sample_rate=0.999999, instrument=False)
assert tasktrooper.init() is h
t = trace.get_tracer("app")
with t.start_as_current_span("GET", kind=SpanKind.SERVER, attributes={"http.request.method": "GET", "url.full": "http://x/a?tok=1"}):
    tasktrooper.set_route("/orders/{id}")
    with t.start_as_current_span("SELECT", kind=SpanKind.CLIENT):
        pass
    logging.getLogger("app").warning("hello %s", "world")
from opentelemetry import metrics
metrics.get_meter("app").create_counter("orders").add(1)
assert h.force_flush(5)
tasktrooper.shutdown()
print("ok", h.exporting, tasktrooper.internal_error_count())
"""


def test_init_exports_otlp_contract(httpserver):
    r = run_py(APP, {"TROOPER_DSN": httpserver.url.replace("http://", "http://tt_key@") + "/1"})
    assert r.returncode == 0, r.stderr
    assert r.stdout.split()[:2] == ["ok", "True"], r.stdout
    reqs = {q["path"]: q for q in httpserver.requests}
    assert {"/v1/traces", "/v1/metrics", "/v1/logs"} <= set(reqs), list(reqs)
    for q in httpserver.requests:
        assert q["headers"]["x-trooper-key"] == "tt_key"
        assert q["headers"]["content-type"] == "application/x-protobuf"
        assert q["headers"].get("content-encoding") == "gzip"

    tr = ExportTraceServiceRequest()
    tr.ParseFromString(reqs["/v1/traces"]["body"])
    res = _attrs(tr.resource_spans[0].resource.attributes)
    assert res["service.name"] == "py-contract"
    assert res["deployment.environment.name"] == "test" and res["service.version"] == "1.2.3"
    assert res["telemetry.distro.name"] == "trooper-python" and res["telemetry.distro.version"]
    spans = {s.name: s for rs in tr.resource_spans for ss in rs.scope_spans for s in ss.spans}
    srv = _attrs(spans["GET"].attributes)
    assert srv["trooper.route"] == "/orders/:id" and srv["http.route"] == "/orders/{id}"
    assert srv["trooper.entry.route"] == "/orders/:id" and srv["trooper.entry.method"] == "GET"
    assert srv["trooper.sample_rate"] == 0.999999 and srv["url.full"] == "http://x/a?tok"
    assert spans["GET"].trace_state == "tt=r:0.999999"
    assert _attrs(spans["SELECT"].attributes)["trooper.entry.route"] == "/orders/:id"

    lg = ExportLogsServiceRequest()
    lg.ParseFromString(reqs["/v1/logs"]["body"])
    recs = [r for rl in lg.resource_logs for sl in rl.scope_logs for r in sl.log_records]
    hello = [r for r in recs if r.body.string_value == "hello world"]
    assert hello and hello[0].trace_id == spans["GET"].trace_id

    mt = ExportMetricsServiceRequest()
    mt.ParseFromString(reqs["/v1/metrics"]["body"])
    assert _attrs(mt.resource_metrics[0].resource.attributes)["telemetry.distro.name"] == "trooper-python"


def test_init_without_dsn_and_disabled():
    code = r"""
import tasktrooper
from opentelemetry import trace, propagate
h = tasktrooper.init(service="s")
c = {}
with trace.get_tracer("t").start_as_current_span("x"):
    propagate.inject(c)
print(json.dumps({"exporting": h.exporting, "baggage": c.get("baggage"), "tp": type(trace.get_tracer_provider()).__name__}))
"""
    r = run_py("import json\n" + code)
    out = json.loads(r.stdout)
    assert out == {"exporting": False, "baggage": "tt.svc=s", "tp": "TracerProvider"}, r.stderr
    r = run_py("import json\n" + code, {"TROOPER_ENABLED": "false", "TROOPER_DSN": "http://k@127.0.0.1:9/1"})
    out = json.loads(r.stdout)
    assert out["exporting"] is False and out["tp"] == "ProxyTracerProvider" and out["baggage"] is None


def test_init_never_raises_on_bad_input():
    code = r"""
import tasktrooper
h = tasktrooper.init(dsn="::not a dsn::", sample_rate="abc", profiling={"types": 5, "hz": "x"}, llm=7, instrument=["nope"],
                     export_timeout_s=-1, resource_attributes={"a": object()})
assert h is not None
h.force_flush(1); h.shutdown(1); h.shutdown(1)
tasktrooper.force_flush(); tasktrooper.shutdown()
print("ok", tasktrooper.internal_error_count() >= 1)
"""
    r = run_py(code)
    assert r.returncode == 0 and r.stdout.strip() == "ok True", r.stderr


def test_unreachable_endpoint_counts_errors_and_does_not_block():
    code = r"""
import time, tasktrooper
from opentelemetry import trace
h = tasktrooper.init(dsn="http://k@127.0.0.1:9/1", service="s", export_timeout_s=1, instrument=False, logs=False, metrics=False)
with trace.get_tracer("t").start_as_current_span("x"):
    pass
t = time.time(); h.force_flush(3); h.shutdown(3)
print(tasktrooper.internal_error_count() >= 1, time.time() - t < 15)
"""
    r = run_py(code, timeout=60)
    assert r.stdout.split() == ["True", "True"], r.stdout + r.stderr


def test_profiling_via_env_uploads(httpserver):
    code = r"""
import time, tasktrooper
h = tasktrooper.init(instrument=False, metrics=False, logs=False, traces=False)
def spin(t):
    while time.time() < t: pass
spin(time.time() + 1.0)
tasktrooper.shutdown()
"""
    r = run_py(
        code,
        {
            "TROOPER_DSN": httpserver.url.replace("http://", "http://k@") + "/1",
            "TROOPER_SERVICE": "prof-env",
            "TROOPER_PROFILING": "true",
            "TROOPER_PROFILING_PERIOD": "1",
            "TROOPER_PROFILING_CPU_DURATION": "0.5",
        },
    )
    assert r.returncode == 0, r.stderr
    prof = [q for q in httpserver.requests if q["path"].startswith("/v1/profiles?")]
    assert prof, [q["path"] for q in httpserver.requests]
    assert "service=prof-env" in prof[0]["path"] and "runtime=python" in prof[0]["path"] and "type=cpu" in prof[0]["path"]
    assert b"spin (<string>)" in b"".join(q["body"] for q in prof)


def test_auto_import_and_launcher(tmp_path, httpserver):
    script = tmp_path / "app.py"
    script.write_text(
        "import tasktrooper\nfrom opentelemetry import trace\nh = tasktrooper.get_handle()\n"
        "print(h is not None and h.config.service, h.exporting, h.config.sample_rate)\n"
    )
    cmd, env = build_env(["--service", "launched", "--sample-rate", "0.2", "--", sys.executable, str(script)])
    env = {k: v for k, v in env.items() if not k.startswith("OTEL_")}
    env["TROOPER_DSN"] = httpserver.url.replace("http://", "http://k@") + "/1"
    assert env["PYTHONPATH"].split(os.pathsep)[0].endswith(os.path.join("tasktrooper", "_bootstrap"))
    import subprocess

    r = subprocess.run(cmd, env=env, capture_output=True, text=True, timeout=60)
    assert r.stdout.split() == ["launched", "True", "0.2"], r.stdout + r.stderr

    r = run_py("import tasktrooper.auto, tasktrooper\nprint(tasktrooper.get_handle().config.service)", {"OTEL_SERVICE_NAME": "auto-svc"})
    assert r.stdout.strip() == "auto-svc", r.stderr


def test_build_env_flags():
    cmd, env = build_env(["--profiling", "--capture-llm-content", "--disabled", "uvicorn", "app:app", "--port", "1"], environ={"PYTHONPATH": "/x"})
    assert cmd == ["uvicorn", "app:app", "--port", "1"]
    assert env["TROOPER_PROFILING"] == "true" and env["TROOPER_LLM_CAPTURE_CONTENT"] == "true" and env["TROOPER_ENABLED"] == "false"
    assert env["PYTHONPATH"].endswith(os.pathsep + "/x")
