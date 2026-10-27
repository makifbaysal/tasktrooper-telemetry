"""Profiler: folded output, route sections, idle heuristic, tracemalloc, uploader (.ai/contracts.md §24)."""

import gzip
import os
import sys
import threading
import time
import tracemalloc
from urllib.parse import parse_qs, urlsplit

import pytest

from tasktrooper import entry
from tasktrooper.config import ProfilingConfig
from tasktrooper.profiling import (
    MAX_STACK_DEPTH,
    Collector,
    ContinuousProfiler,
    FrameNamer,
    FunctionIndex,
    IdleDetector,
    ProfileUploader,
    folded_text,
    profile_query,
    relative_path,
    tracemalloc_stacks,
)


def busy_leaf(stop):
    x = 0
    while not stop.is_set():
        x += 1
    return x


def busy_worker(stop):
    return busy_leaf(stop)


def sleepy_worker(stop):
    while not stop.is_set():
        time.sleep(0.01)


def lock_waiter(stop):
    stop.wait()


class FakeEntry:
    def __init__(self, route, method):
        self._r, self.method = route, method

    def entry_route(self):
        return self._r


def _run_threads(*targets):
    stop = threading.Event()
    ts = [threading.Thread(target=t, args=(stop,), daemon=True) for t in targets]
    for t in ts:
        t.start()
    time.sleep(0.05)
    return stop, ts


def test_collector_cpu_wall_and_routes():
    stop, ts = _run_threads(busy_worker, sleepy_worker, lock_waiter)
    try:
        entries = {ts[0].ident: FakeEntry("/items/:id", "GET")}
        c = Collector(["cpu", "wall"], 10_000_000, cwd=os.path.dirname(__file__), cpu_clock=None)
        me = threading.get_ident()
        for _ in range(30):
            c.sample(sys._current_frames(), me, entries, time.monotonic_ns())
            time.sleep(0.005)
    finally:
        stop.set()
        for t in ts:
            t.join()
    cpu = folded_text(c.cpu_stacks)
    wall = folded_text(c.wall_stacks)
    assert "# trooper.route=/items/:id trooper.method=GET\n" in cpu
    busy = [ln for ln in cpu.splitlines() if "busy_leaf (test_profiling.py)" in ln]
    assert any(ln.split(";")[-1].startswith("busy_leaf (test_profiling.py) ") for ln in busy)
    assert all("busy_worker (test_profiling.py);busy_leaf (test_profiling.py)" in ln for ln in busy)
    # idle threads are excluded from cpu, present in wall
    assert "sleepy_worker" not in cpu and "lock_waiter" not in cpu
    assert "sleepy_worker (test_profiling.py)" in wall and "wait (threading.py)" in wall
    # the sampling thread itself is never sampled
    assert "test_collector_cpu_wall_and_routes" not in wall
    # labelled stacks come after the unlabelled ones; values are integers
    lines = wall.splitlines()
    head = lines.index("# trooper.route=/items/:id trooper.method=GET")
    assert all("busy_leaf" not in ln for ln in lines[:head])
    for ln in lines:
        if not ln.startswith("#"):
            int(ln.rsplit(" ", 1)[1])


@pytest.mark.skipif(not sys.platform.startswith("linux"), reason="per-thread CPU clocks are Linux-only")
def test_collector_linux_cpu_clock():
    stop, ts = _run_threads(busy_worker, sleepy_worker)
    try:
        c = Collector(["cpu"], 10_000_000)
        assert c.cpu_clock is not None
        for _ in range(20):
            c.sample(sys._current_frames(), threading.get_ident(), {}, time.monotonic_ns())
            time.sleep(0.01)
    finally:
        stop.set()
    cpu = folded_text(c.cpu_stacks)
    assert "busy_leaf" in cpu


def test_idle_detector_blocking_calls():
    d = IdleDetector()
    stop, ts = _run_threads(sleepy_worker, busy_worker)
    try:
        frames = sys._current_frames()
        assert d.idle(frames[ts[0].ident])
        assert not d.idle(frames[ts[1].ident])
    finally:
        stop.set()


def test_frame_names_and_depth_cap():
    n = FrameNamer(cwd="/srv/app")
    assert relative_path("/srv/app/pkg/mod.py", "/srv/app") == "pkg/mod.py"
    assert relative_path("/x/lib/python3.11/site-packages/requests/api.py", "/srv") == "requests/api.py"
    assert relative_path("<frozen importlib._bootstrap>", "/srv") == "<frozen importlib._bootstrap>"
    assert relative_path(threading.__file__, "/srv") == "threading.py"

    def deep(k):
        if k == 0:
            return sys._getframe()
        return deep(k - 1)

    st = n.stack(deep(200))
    assert len(st) == MAX_STACK_DEPTH and st[0] == "[truncated]" and st[-1].split(" (")[0] in ("deep", "test_frame_names_and_depth_cap.<locals>.deep")


def test_folded_caps_unique_stacks():
    stacks = {(None, (f"f{i}",)): i + 1 for i in range(10_050)}
    out = folded_text(stacks).splitlines()
    assert len(out) == 10_000 and out[-1].startswith("[other] ")


def _alloc_here():
    return [bytearray(64 * 1024) for _ in range(20)]


class Holder:
    def make(self):
        return _alloc_here()


def test_tracemalloc_stacks_have_function_names():
    tracemalloc.start(10)
    try:
        keep = Holder().make()
        snap = tracemalloc.take_snapshot()
    finally:
        tracemalloc.stop()
    folded = folded_text(tracemalloc_stacks(snap, cwd=os.path.dirname(__file__)))
    line = [ln for ln in folded.splitlines() if "_alloc_here" in ln]
    assert line, folded[:2000]
    assert "Holder.make (test_profiling.py);" in line[0]
    assert line[0].split(" ")[-1].isdigit() and int(line[0].rsplit(" ", 1)[1]) >= 20 * 64 * 1024
    assert keep


def test_function_index_nested():
    idx = FunctionIndex()
    assert idx.qualname(__file__, Holder.make.__code__.co_firstlineno + 1) == "Holder.make"
    assert idx.qualname(__file__, 1) == "<module>"


def test_profile_query():
    q = parse_qs(profile_query({"type": "cpu", "service": "svc", "env": "prod", "version": None, "host": "h1", "start": 1000.4, "end": 2000.2}))
    assert q == {
        "format": ["folded"], "type": ["cpu"], "unit": ["nanoseconds"], "service": ["svc"], "env": ["prod"],
        "host": ["h1"], "runtime": ["python"], "start": ["1000"], "end": ["2001"],
    }
    assert parse_qs(profile_query({"type": "inuse_space", "service": "s", "start": 1, "end": 2}))["unit"] == ["bytes"]


def test_uploader_posts_gzip_folded(httpserver):
    up = ProfileUploader(httpserver.url + "/v1/profiles", {"x-trooper-key": "k"})
    assert up.upload({"type": "wall", "service": "svc", "start": 1, "end": 2}, "a;b 10\n")
    req = httpserver.requests[0]
    assert req["headers"]["x-trooper-key"] == "k"
    assert req["headers"]["content-encoding"] == "gzip"
    assert req["headers"]["content-type"].startswith("text/plain")
    assert req["body"] == b"a;b 10\n"
    q = parse_qs(urlsplit(req["path"]).query)
    assert q["format"] == ["folded"] and q["type"] == ["wall"] and q["runtime"] == ["python"]
    assert up.upload({"type": "cpu", "service": "svc", "start": 1, "end": 2}, "") is False  # nothing to send


@pytest.mark.parametrize("status,backoff", [(503, True), (429, True), (400, False)])
def test_uploader_backoff(httpserver, status, backoff):
    httpserver.status = status
    httpserver.headers = {"Retry-After": "30"} if status == 429 else {}
    t = [100.0]
    up = ProfileUploader(httpserver.url + "/v1/profiles", {}, now=lambda: t[0])
    assert up.upload({"type": "cpu", "service": "s", "start": 1, "end": 2}, "a 1\n") is False
    assert up.backing_off is backoff
    if status == 429:
        t[0] += 29
        assert up.backing_off  # Retry-After honoured
    n = len(httpserver.requests)
    up.upload({"type": "cpu", "service": "s", "start": 1, "end": 2}, "a 1\n")
    assert len(httpserver.requests) == (n if backoff else n + 1)


def test_uploader_network_error_never_raises():
    up = ProfileUploader("http://127.0.0.1:9/v1/profiles", {}, timeout_s=0.5)
    assert up.upload({"type": "cpu", "service": "s", "start": 1, "end": 2}, "a 1\n") is False
    assert up.backing_off


def test_continuous_profiler_end_to_end(httpserver):
    stop, ts = _run_threads(busy_worker)
    e = entry.Entry(root=None, kind="server")
    e.route, e.method = "/work/:id", "POST"
    entry.bind_thread(e, ts[0].ident)
    cfg = ProfilingConfig(types=["cpu", "wall", "inuse_space"], period_s=0.6, cpu_duration_s=0.3, hz=200)
    p = ContinuousProfiler(
        cfg, ProfileUploader(httpserver.url + "/v1/profiles", {"x-trooper-key": "k"}),
        service="svc", env="test", version="1.2.3", host="h", cwd=os.path.dirname(__file__), cpu_clock=None,
    )
    try:
        p.start()
        assert p.running and tracemalloc.is_tracing()
        deadline = time.time() + 5
        while time.time() < deadline and len({parse_qs(urlsplit(r["path"]).query)["type"][0] for r in httpserver.requests}) < 3:
            time.sleep(0.05)
    finally:
        p.stop()
        stop.set()
        entry.reset()
    assert not p.running and not tracemalloc.is_tracing()
    by_type = {}
    for r in httpserver.requests:
        q = parse_qs(urlsplit(r["path"]).query)
        by_type.setdefault(q["type"][0], (q, r["body"].decode()))
    assert set(by_type) == {"cpu", "wall", "inuse_space"}
    q, body = by_type["cpu"]
    assert q["service"] == ["svc"] and q["env"] == ["test"] and q["version"] == ["1.2.3"] and q["host"] == ["h"]
    start, end = int(q["start"][0]), int(q["end"][0])
    assert 200 <= end - start <= 2000
    assert "# trooper.route=/work/:id trooper.method=POST\n" in body and "busy_leaf (test_profiling.py)" in body
    assert "tasktrooper-profiler" not in body and "_run (tasktrooper/profiling.py)" not in body


def test_profiler_stop_is_idempotent_and_safe():
    p = ContinuousProfiler(ProfilingConfig(types=["cpu"]), uploader=None, service="s")
    p.stop()
    p.start()  # uploader None → errors are swallowed in the thread
    p.stop(1)
    p.stop()


def test_fork_restarts_sampler(httpserver):
    if not hasattr(os, "fork"):
        pytest.skip("no fork")
    cfg = ProfilingConfig(types=["wall"], period_s=0.3, cpu_duration_s=0.2, hz=100)
    p = ContinuousProfiler(cfg, ProfileUploader(httpserver.url + "/v1/profiles", {}), service="parent")
    p.start()
    try:
        r, w = os.pipe()
        pid = os.fork()
        if pid == 0:  # child
            ok = any(t.name == "tasktrooper-profiler" and t.is_alive() for t in threading.enumerate())
            os.write(w, b"1" if ok else b"0")
            os._exit(0)
        os.close(w)
        got = os.read(r, 1)
        os.waitpid(pid, 0)
        assert got == b"1"
    finally:
        p.stop()


def test_gzip_roundtrip_helper():
    assert gzip.decompress(gzip.compress(b"x")) == b"x"
