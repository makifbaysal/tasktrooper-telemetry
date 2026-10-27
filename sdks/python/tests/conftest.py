"""Shared fixtures.

The OTel global TracerProvider can be set only once per process, so the suite installs one provider (trooper sampler +
span processor → in-memory exporter) and clears the exporter between tests. ``init()`` itself is exercised in
subprocesses (``run_py``).
"""

from __future__ import annotations

import gzip
import http.server
import os
import subprocess
import sys
import threading
from typing import Any, Dict, List, Optional

import pytest
from opentelemetry import propagate, trace
from opentelemetry.sdk.trace import TracerProvider
from opentelemetry.sdk.trace.export import SimpleSpanProcessor
from opentelemetry.sdk.trace.export.in_memory_span_exporter import InMemorySpanExporter

from tasktrooper import entry
from tasktrooper.processor import TrooperSpanProcessor
from tasktrooper.propagation import create_propagator
from tasktrooper.sampler import TrooperSampler

EXPORTER = InMemorySpanExporter()
PROVIDER = TracerProvider(sampler=TrooperSampler(1.0))
PROVIDER.add_span_processor(TrooperSpanProcessor(1.0, [SimpleSpanProcessor(EXPORTER)]))
trace.set_tracer_provider(PROVIDER)
propagate.set_global_textmap(create_propagator("test-svc"))


@pytest.fixture()
def spans():
    EXPORTER.clear()
    entry.reset()
    yield EXPORTER
    EXPORTER.clear()


@pytest.fixture()
def tracer():
    return PROVIDER.get_tracer("test")


def by_name(exporter: InMemorySpanExporter, name: str) -> Any:
    for s in exporter.get_finished_spans():
        if s.name == name:
            return s
    names = [s.name for s in exporter.get_finished_spans()]
    raise AssertionError(f"span {name!r} not found in {names}")


class Captured:
    def __init__(self) -> None:
        self.requests: List[Dict[str, Any]] = []
        self.status = 200
        self.headers: Dict[str, str] = {}
        self.event = threading.Event()


class _Handler(http.server.BaseHTTPRequestHandler):
    cap: Captured

    def do_POST(self) -> None:  # noqa: N802
        if self.headers.get("transfer-encoding", "").lower() == "chunked":
            raw = b""
            while True:
                size = int(self.rfile.readline().strip() or b"0", 16)
                if size == 0:
                    self.rfile.readline()
                    break
                raw += self.rfile.read(size)
                self.rfile.readline()
        else:
            raw = self.rfile.read(int(self.headers.get("content-length") or 0))
        try:
            body = gzip.decompress(raw) if self.headers.get("content-encoding") == "gzip" else raw
        except Exception as e:  # noqa: BLE001
            body = b"<undecodable: %r>" % e
        self.cap.requests.append({"path": self.path, "headers": {k.lower(): v for k, v in self.headers.items()}, "body": body})
        self.send_response(self.cap.status)
        for k, v in self.cap.headers.items():
            self.send_header(k, v)
        self.send_header("content-length", "0")
        self.end_headers()
        self.cap.event.set()

    def log_message(self, *args: Any) -> None:
        pass


@pytest.fixture()
def httpserver():
    cap = Captured()
    handler = type("H", (_Handler,), {"cap": cap})
    srv = http.server.ThreadingHTTPServer(("127.0.0.1", 0), handler)
    t = threading.Thread(target=srv.serve_forever, daemon=True)
    t.start()
    cap.url = f"http://127.0.0.1:{srv.server_address[1]}"  # type: ignore[attr-defined]
    yield cap
    srv.shutdown()
    srv.server_close()


def run_py(code: str, env: Optional[Dict[str, str]] = None, timeout: float = 60) -> subprocess.CompletedProcess:
    e = {k: v for k, v in os.environ.items() if not k.startswith(("TROOPER_", "OTEL_"))}
    e.update(env or {})
    return subprocess.run([sys.executable, "-c", code], env=e, capture_output=True, text=True, timeout=timeout)
