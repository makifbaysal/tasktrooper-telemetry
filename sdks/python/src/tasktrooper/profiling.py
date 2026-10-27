"""Continuous profiling (ADR-022, .ai/contracts.md §24): a sampling thread reading ``sys._current_frames()``.

Folded stacks (``root;…;leaf <value>``) are uploaded to ``POST /v1/profiles?format=folded`` once per period, with
``# trooper.route=<route> trooper.method=<METHOD>`` sections for samples taken while the thread was serving an entry
span (HTTP request / consumed message). Route attribution is per **thread**: under asyncio all tasks share the loop
thread and the most recently started entry wins.

Types:

- ``cpu`` (default, nanoseconds): on-CPU time. On Linux each thread's CPU clock (``pthread_getcpuclockid``) is read at
  every sample and the delta is charged to the stack seen; elsewhere a sample counts one interval unless the thread
  looks idle (heuristic below).
- ``wall`` (opt-in, nanoseconds): every sample of every thread, idle or not, weighted by the real time between
  samples.
- ``inuse_space`` (opt-in, bytes): live Python allocations from ``tracemalloc`` at the end of each period.
  ``tracemalloc`` slows allocation-heavy code noticeably (often 10–30 %) and uses memory per traced block — enable it
  for investigations, not permanently.

Idle heuristic (non-Linux ``cpu``): a thread is idle when its leaf Python frame is a known blocking stdlib function
(``selectors.select``, ``threading.Condition.wait``, ``queue.get``, ``socket.accept``/``readinto``, ``ssl.read``,
``concurrent.futures`` worker wait …) or when the leaf frame is calling a blocking builtin on that line (``sleep``,
``acquire``, ``wait``, ``recv*``, ``accept``, ``select``, ``poll``, ``join``). C extensions that block without
releasing a recognisable frame (e.g. a DB driver's ``execute``) are counted as CPU there.

Frame names: ``qualname (path/to/file.py)`` — path relative to the working directory, ``site-packages`` or the stdlib
directory; no line numbers. Each period samples for ``cpu_duration_s`` (default 10 s of every 60 s) at ``hz``
(default 100); the profile window is the sampling window. Never raises; errors count as ``trooper.sdk.errors``
(``profiler``, ``profiler.cpu``, ``exporter.profiles``).
"""

from __future__ import annotations

import dis
import gzip
import heapq
import linecache
import os
import random
import re
import socket
import sys
import sysconfig
import threading
import time
import tracemalloc
import urllib.error
import urllib.request
import weakref
from typing import Any, Callable, Dict, List, Mapping, Optional, Tuple
from urllib.parse import urlencode

from ._errors import record_error
from .config import ProfilingConfig
from .entry import thread_entries

MAX_STACK_DEPTH = 128
MAX_STACKS = 10_000
_MAX_CACHE = 50_000

Label = Optional[Tuple[str, str]]
StackKey = Tuple[Label, Tuple[str, ...]]

# --- frame naming ---------------------------------------------------------------------------------------------------

_sep = os.sep


def _roots() -> List[str]:
    out = []
    try:
        paths = sysconfig.get_paths()
        for k in ("purelib", "platlib", "stdlib", "platstdlib"):
            p = paths.get(k)
            if p:
                out.append(os.path.normpath(p) + _sep)
    except Exception:  # noqa: BLE001
        pass
    # longest first so site-packages (inside the stdlib prefix on some layouts) wins
    return sorted(set(out), key=len, reverse=True)


_ROOTS = _roots()


def relative_path(filename: str, cwd: Optional[str] = None) -> str:
    """File path for frame names: relative to cwd, a site-packages dir or the stdlib; ``/`` separators."""
    if not filename:
        return ""
    if filename.startswith("<"):
        return filename
    p = filename
    base = (cwd if cwd is not None else os.getcwd()).rstrip(_sep) + _sep
    for marker in (_sep + "site-packages" + _sep, _sep + "dist-packages" + _sep):
        i = p.rfind(marker)
        if i >= 0:
            p = p[i + len(marker) :]
            return p.replace(_sep, "/")
    if p.startswith(base):
        p = p[len(base) :]
    else:
        for r in _ROOTS:
            if p.startswith(r):
                p = p[len(r) :]
                break
    return p.replace(_sep, "/")


def _clean(s: str) -> str:
    return s.replace(";", " ").replace("\n", " ").replace("\r", " ")


class FrameNamer:
    """Caches ``qualname (path)`` per code object."""

    def __init__(self, cwd: Optional[str] = None) -> None:
        self.cwd = cwd if cwd is not None else os.getcwd()
        self._cache: Dict[Any, str] = {}

    def name(self, code: Any) -> str:
        n = self._cache.get(code)
        if n is None:
            fn = getattr(code, "co_qualname", None) or code.co_name or "(anonymous)"
            path = relative_path(code.co_filename, self.cwd)
            n = _clean(f"{fn} ({path})" if path else fn)
            if len(self._cache) >= _MAX_CACHE:
                self._cache.clear()
            self._cache[code] = n
        return n

    def stack(self, frame: Any) -> Tuple[str, ...]:
        names: List[str] = []
        f = frame
        while f is not None:
            names.append(self.name(f.f_code))
            f = f.f_back
        names.reverse()
        return cap_depth(names)


def cap_depth(frames: List[str]) -> Tuple[str, ...]:
    if len(frames) > MAX_STACK_DEPTH:
        return ("[truncated]",) + tuple(frames[len(frames) - (MAX_STACK_DEPTH - 1) :])
    return tuple(frames)


# --- idle heuristic -------------------------------------------------------------------------------------------------

#: (file suffix, function) of stdlib frames that block.
IDLE_FRAMES = {
    ("selectors.py", "select"),
    ("threading.py", "wait"),
    ("threading.py", "wait_for"),
    ("threading.py", "_wait_for_tstate_lock"),
    ("threading.py", "join"),
    ("queue.py", "get"),
    ("queue.py", "put"),
    ("socket.py", "accept"),
    ("socket.py", "readinto"),
    ("socket.py", "recv_into"),
    ("ssl.py", "read"),
    ("ssl.py", "recv"),
    ("ssl.py", "recv_into"),
    ("ssl.py", "do_handshake"),
    ("concurrent/futures/thread.py", "_worker"),
    ("concurrent/futures/_base.py", "result"),
    ("concurrent/futures/_base.py", "wait"),
    ("multiprocessing/connection.py", "_recv"),
    ("multiprocessing/connection.py", "_recv_bytes"),
    ("multiprocessing/connection.py", "wait"),
    ("multiprocessing/connection.py", "poll"),
    ("multiprocessing/pool.py", "worker"),
    ("subprocess.py", "_wait"),
    ("subprocess.py", "_try_wait"),
    ("subprocess.py", "communicate"),
    ("sched.py", "run"),
}
#: Builtins that block when called from the leaf frame's current line.
BLOCKING_CALLS = frozenset(
    {"sleep", "acquire", "wait", "recv", "recv_into", "recvfrom", "accept", "select", "poll", "join", "epoll", "kevent"}
)
_LOADS = frozenset({"LOAD_ATTR", "LOAD_METHOD", "LOAD_GLOBAL", "LOAD_NAME", "LOAD_SUPER_ATTR"})


class IdleDetector:
    def __init__(self) -> None:
        self._frames: Dict[Any, bool] = {}
        self._calls: Dict[Tuple[Any, int], bool] = {}
        self._instrs: Dict[Any, List[Any]] = {}

    def idle(self, frame: Any) -> bool:
        code = frame.f_code
        hit = self._frames.get(code)
        if hit is None:
            fn = code.co_filename.replace(_sep, "/")
            hit = any(code.co_name == name and fn.endswith("/" + suffix) for suffix, name in IDLE_FRAMES)
            if len(self._frames) >= _MAX_CACHE:
                self._frames.clear()
            self._frames[code] = hit
        if hit:
            return True
        return self._blocking_call(code, frame.f_lasti)

    def _blocking_call(self, code: Any, lasti: int) -> bool:
        key = (code, lasti)
        hit = self._calls.get(key)
        if hit is not None:
            return hit
        hit = False
        try:
            instrs = self._instrs.get(code)
            if instrs is None:
                instrs = list(dis.get_instructions(code))
                if len(self._instrs) >= 2000:
                    self._instrs.clear()
                self._instrs[code] = instrs
            idx = -1
            for i, ins in enumerate(instrs):
                if ins.offset == lasti:
                    idx = i
                    break
                if ins.offset > lasti:
                    idx = i - 1
                    break
            if idx >= 0 and instrs[idx].opname.startswith(("CALL", "PRECALL")):
                j = idx
                while j >= 0:
                    ins = instrs[j]
                    if ins.opname in _LOADS and isinstance(ins.argval, str) and ins.argval in BLOCKING_CALLS:
                        hit = True
                        break
                    if _starts_line(ins) and j != idx:
                        break
                    j -= 1
        except Exception:  # noqa: BLE001
            hit = False
        if len(self._calls) >= _MAX_CACHE:
            self._calls.clear()
        self._calls[key] = hit
        return hit


def _starts_line(ins: Any) -> bool:
    v = getattr(ins, "starts_line", None)
    if isinstance(v, bool):  # 3.13+
        return v
    return v is not None


# --- per-thread CPU clocks (Linux) ----------------------------------------------------------------------------------


def _thread_cpu_reader() -> Optional[Callable[[int], Optional[int]]]:
    getclk = getattr(time, "pthread_getcpuclockid", None)
    if getclk is None or not sys.platform.startswith("linux"):
        return None

    def read(ident: int) -> Optional[int]:
        try:
            return time.clock_gettime_ns(getclk(ident))
        except (OSError, OverflowError, ValueError):
            return None

    try:
        if read(threading.get_ident()) is None:
            return None
    except Exception:  # noqa: BLE001
        return None
    return read


# --- collection -----------------------------------------------------------------------------------------------------


class Collector:
    """Accumulates samples of one window."""

    def __init__(self, types: List[str], interval_ns: int, cwd: Optional[str] = None, cpu_clock: Any = "auto") -> None:
        self.cpu = "cpu" in types
        self.wall = "wall" in types
        self.interval_ns = interval_ns
        self.namer = FrameNamer(cwd)
        self.idle = IdleDetector()
        self.cpu_clock = _thread_cpu_reader() if cpu_clock == "auto" else cpu_clock
        self.cpu_stacks: Dict[StackKey, int] = {}
        self.wall_stacks: Dict[StackKey, int] = {}
        self._last_cpu: Dict[int, int] = {}
        self._last_ts: Optional[int] = None
        self.samples = 0

    def sample(self, frames: Mapping[int, Any], skip: int, entries: Mapping[int, Any], now_ns: int) -> None:
        dt = self.interval_ns if self._last_ts is None else max(0, now_ns - self._last_ts)
        dt = min(dt, self.interval_ns * 10)
        self._last_ts = now_ns
        self.samples += 1
        seen = set()
        for ident, frame in frames.items():
            if ident == skip or frame is None:
                continue
            seen.add(ident)
            stack = self.namer.stack(frame)
            if not stack:
                continue
            label: Label = None
            e = entries.get(ident)
            if e is not None:
                try:
                    r = e.entry_route()
                    if r:
                        label = (r, e.method or "")
                except Exception as err:  # noqa: BLE001
                    record_error("profiler", err)
            key = (label, stack)
            if self.wall:
                self.wall_stacks[key] = self.wall_stacks.get(key, 0) + dt
            if self.cpu:
                if self.cpu_clock is not None:
                    cur = self.cpu_clock(ident)
                    if cur is None:
                        continue
                    prev = self._last_cpu.get(ident)
                    self._last_cpu[ident] = cur
                    if prev is not None and cur > prev:
                        self.cpu_stacks[key] = self.cpu_stacks.get(key, 0) + (cur - prev)
                elif not self.idle.idle(frame):
                    self.cpu_stacks[key] = self.cpu_stacks.get(key, 0) + dt
        if self.cpu_clock is not None and len(self._last_cpu) > len(seen):
            for ident in [i for i in self._last_cpu if i not in seen]:
                del self._last_cpu[ident]

    def reset(self) -> None:
        self.cpu_stacks = {}
        self.wall_stacks = {}
        self._last_ts = None
        self.samples = 0


def folded_text(stacks: Mapping[StackKey, int]) -> str:
    """Folded lines; unlabelled stacks first, then one ``# trooper.route=…`` section per (route, method)."""
    items = [(k, v) for k, v in stacks.items() if v > 0 and k[1]]
    if len(items) > MAX_STACKS:
        items.sort(key=lambda kv: kv[1], reverse=True)
        other = sum(v for _, v in items[MAX_STACKS - 1 :])
        items = items[: MAX_STACKS - 1] + [((None, ("[other]",)), other)]
    groups: Dict[Label, List[Tuple[Tuple[str, ...], int]]] = {}
    for (label, stack), v in items:
        groups.setdefault(label, []).append((stack, v))
    out: List[str] = []
    for stack, v in groups.pop(None, []):
        out.append(f"{';'.join(stack)} {int(v)}\n")
    for label in sorted(groups, key=lambda x: (x[0], x[1])):  # type: ignore[index]
        route, method = label  # type: ignore[misc]
        head = f"# trooper.route={_label(route)}"
        if method:
            head += f" trooper.method={_label(method)}"
        out.append(head + "\n")
        for stack, v in groups[label]:
            out.append(f"{';'.join(stack)} {int(v)}\n")
    return "".join(out)


def _label(s: str) -> str:
    return "".join("_" if c.isspace() else c for c in s)


# --- tracemalloc ----------------------------------------------------------------------------------------------------


_DEF = re.compile(r"^([ \t]*)(?:async[ \t]+)?(def|class)[ \t]+([A-Za-z_]\w*)")
#: Allocation tracebacks processed per snapshot (largest first); the rest is summed into ``[other]``.
MAX_HEAP_TRACEBACKS = 2_000


class FunctionIndex:
    """Maps (file, line) → enclosing function qualname (tracemalloc frames carry no function names).

    Scans the source upwards from the line for ``def`` / ``class`` headers with decreasing indentation (cheap; no AST),
    cached per (file, line). Decorated / multi-line constructs resolve to the nearest enclosing header.
    """

    def __init__(self) -> None:
        self._cache: Dict[Tuple[str, int], str] = {}

    def qualname(self, filename: str, lineno: int) -> str:
        key = (filename, lineno)
        hit = self._cache.get(key)
        if hit is None:
            hit = self._scan(filename, lineno)
            if len(self._cache) >= _MAX_CACHE:
                self._cache.clear()
            self._cache[key] = hit
        return hit

    @staticmethod
    def _scan(filename: str, lineno: int) -> str:
        try:
            lines = linecache.getlines(filename)
        except Exception:  # noqa: BLE001
            return "<module>"
        if not lines or lineno < 1 or lineno > len(lines):
            return "<module>"

        def indent(s: str) -> int:
            return len(s) - len(s.lstrip(" \t"))

        cur = lines[lineno - 1]
        m = _DEF.match(cur)
        names: List[Tuple[str, str]] = []
        limit = indent(cur) if cur.strip() else 1 << 30
        if m:  # the line itself is a def header (frames point at decorators / defaults)
            names.append((m.group(2), m.group(3)))
            limit = len(m.group(1))
        for i in range(lineno - 2, -1, -1):
            if limit == 0:
                break
            ln = lines[i]
            if not ln.strip() or ln.lstrip().startswith(("#", ")", "]", "}")):
                continue
            ind = indent(ln)
            if ind >= limit:
                continue
            m = _DEF.match(ln)
            if m:
                names.append((m.group(2), m.group(3)))
            limit = ind
        if not names:
            return "<module>"
        out = []
        for kind, name in reversed(names):
            out.append(name)
            out.append("." if kind == "class" else ".<locals>.")
        if names[0][0] == "class":
            return "".join(out[:-1]) + ".<body>"
        return "".join(out[:-1])


def _heap_groups(snapshot: Any) -> List[Tuple[int, Tuple[Tuple[str, int], ...]]]:
    """(size, frames oldest → most recent) per distinct allocation traceback, largest first.

    Groups the raw ``Snapshot.traces._traces`` tuples directly (``Snapshot.statistics()`` builds an object per trace and takes seconds on
    import-heavy heaps); falls back to the public API when the private layout is unknown.
    """
    raw = getattr(getattr(snapshot, "traces", None), "_traces", None)
    if isinstance(raw, (list, tuple)) and (not raw or (isinstance(raw[0], tuple) and len(raw[0]) >= 3)):
        sizes: Dict[Any, int] = {}
        for t in raw:
            tb = t[2]
            sizes[tb] = sizes.get(tb, 0) + t[1]
        top = heapq.nlargest(MAX_HEAP_TRACEBACKS, sizes.items(), key=lambda kv: kv[1])
        rest = sum(sizes.values()) - sum(size for _, size in top)
        # raw frames are most recent first
        out = [(size, tuple(reversed(tb))) for tb, size in top]
        if rest > 0:
            out.append((rest, (("[other]", 0),)))
        return out
    out = []
    for i, st in enumerate(snapshot.statistics("traceback")):
        out.append((st.size, tuple((f.filename, f.lineno) for f in st.traceback) if i < MAX_HEAP_TRACEBACKS else ()))
    return out


def tracemalloc_stacks(snapshot: Any, cwd: Optional[str] = None, index: Optional[FunctionIndex] = None) -> Dict[StackKey, int]:
    idx = index or FunctionIndex()
    base = cwd if cwd is not None else os.getcwd()
    skip = (tracemalloc.__file__, __file__)
    stacks: Dict[StackKey, int] = {}
    other = 0
    for i, (size, frames) in enumerate(_heap_groups(snapshot)):
        if size <= 0:
            continue
        if i >= MAX_HEAP_TRACEBACKS or frames == (("[other]", 0),):
            other += size
            continue
        names = []
        for filename, lineno in frames:
            if filename in skip:
                continue
            names.append(_clean(f"{idx.qualname(filename, lineno)} ({relative_path(filename, base)})"))
        if not names:
            continue
        key: StackKey = (None, cap_depth(names))
        stacks[key] = stacks.get(key, 0) + size
    if other:
        stacks[(None, ("[other]",))] = other
    return stacks


# --- upload ---------------------------------------------------------------------------------------------------------

UNITS = {"cpu": "nanoseconds", "wall": "nanoseconds", "inuse_space": "bytes"}
MIN_BACKOFF_S = 1.0
MAX_BACKOFF_S = 60.0


def profile_query(meta: Mapping[str, Any]) -> str:
    """Query string of ``POST /v1/profiles`` (.ai/contracts.md §24)."""
    q: List[Tuple[str, str]] = [
        ("format", "folded"),
        ("type", meta["type"]),
        ("unit", UNITS[meta["type"]]),
        ("service", meta["service"]),
    ]
    for k in ("env", "version", "host"):
        if meta.get(k):
            q.append((k, str(meta[k])))
    q.append(("runtime", "python"))
    q.append(("start", str(int(meta["start"]))))
    q.append(("end", str(int(-(-meta["end"] // 1)))))
    return urlencode(q)


Sender = Callable[[str, bytes, Dict[str, str], float], Tuple[int, Optional[str]]]


def _urllib_send(url: str, body: bytes, headers: Dict[str, str], timeout: float) -> Tuple[int, Optional[str]]:
    req = urllib.request.Request(url, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:  # noqa: S310 - URL from config
            resp.read()
            return resp.status, None
    except urllib.error.HTTPError as e:
        return e.code, e.headers.get("Retry-After") if e.headers else None


class ProfileUploader:
    """Uploads folded profiles. No queue (.ai/contracts.md §24): a failed profile is dropped.

    429 / 5xx / network errors start an exponential backoff (1 s → 60 s, jitter, ``Retry-After`` honoured) during
    which profiles are dropped; other 4xx drop the profile. Never raises.
    """

    def __init__(
        self,
        url: str,
        headers: Mapping[str, str],
        timeout_s: float = 10.0,
        sender: Optional[Sender] = None,
        now: Callable[[], float] = time.monotonic,
    ) -> None:
        self.url = url
        self.headers = dict(headers)
        self.timeout_s = timeout_s
        self._send = sender or _urllib_send
        self._now = now
        self._backoff = 0.0
        self._until = 0.0

    @property
    def backing_off(self) -> bool:
        return self._now() < self._until

    def upload(self, meta: Mapping[str, Any], folded: str) -> bool:
        if not folded or self.backing_off:
            return False
        try:
            from opentelemetry.context import _SUPPRESS_INSTRUMENTATION_KEY, attach, detach, set_value

            token = attach(set_value(_SUPPRESS_INSTRUMENTATION_KEY, True))
        except Exception:  # noqa: BLE001
            token = None
        try:
            body = gzip.compress(folded.encode("utf-8"))
            headers = {**self.headers, "content-type": "text/plain; charset=utf-8", "content-encoding": "gzip"}
            status, retry_after = self._send(f"{self.url}?{profile_query(meta)}", body, headers, self.timeout_s)
            if 200 <= status < 300:
                self._backoff = 0.0
                return True
            if status == 429 or status >= 500:
                self._start_backoff(retry_after)
            record_error("exporter.profiles", RuntimeError(f"HTTP {status}"))
            return False
        except Exception as err:  # noqa: BLE001
            self._start_backoff(None)
            record_error("exporter.profiles", err)
            return False
        finally:
            if token is not None:
                try:
                    detach(token)
                except Exception:  # noqa: BLE001
                    pass

    def _start_backoff(self, retry_after: Optional[str]) -> None:
        self._backoff = min(MAX_BACKOFF_S, self._backoff * 2 if self._backoff else MIN_BACKOFF_S)
        wait = self._backoff * (0.5 + random.random() * 0.5)  # noqa: S311
        try:
            ra = float(retry_after) if retry_after else 0.0
        except ValueError:
            ra = 0.0
        if ra > 0:
            wait = max(wait, min(ra, MAX_BACKOFF_S))
        self._until = self._now() + wait


# --- profiler -------------------------------------------------------------------------------------------------------

_profilers: "weakref.WeakSet[ContinuousProfiler]" = weakref.WeakSet()
_fork_hook = False


def _after_fork_in_child() -> None:
    for p in list(_profilers):
        try:
            p._after_fork()  # noqa: SLF001
        except Exception as err:  # noqa: BLE001
            record_error("profiler", err)


def _host() -> Optional[str]:
    try:
        return socket.gethostname() or None
    except Exception:  # noqa: BLE001
        return None


class ContinuousProfiler:
    """Background sampling thread; one folded profile per type per period. Never raises."""

    def __init__(
        self,
        config: ProfilingConfig,
        uploader: Any,
        service: str,
        env: Optional[str] = None,
        version: Optional[str] = None,
        host: Optional[str] = None,
        cwd: Optional[str] = None,
        cpu_clock: Any = "auto",
    ) -> None:
        self.config = config
        self.uploader = uploader
        self.meta_base = {"service": service, "env": env, "version": version, "host": host or _host()}
        self.cwd = cwd if cwd is not None else os.getcwd()
        self.types = [t for t in config.types if t in UNITS]
        self._cpu_clock = cpu_clock
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        self._started_tracemalloc = False
        self._running = False
        self._index = FunctionIndex()
        self._lock = threading.Lock()

    @property
    def running(self) -> bool:
        return self._running

    def start(self) -> None:
        global _fork_hook
        try:
            with self._lock:
                if self._running or not self.types:
                    return
                if "inuse_space" in self.types and not tracemalloc.is_tracing():
                    tracemalloc.start(self.config.tracemalloc_frames)
                    self._started_tracemalloc = True
                self._stop.clear()
                self._running = True
                self._spawn()
            _profilers.add(self)
            if not _fork_hook and hasattr(os, "register_at_fork"):
                os.register_at_fork(after_in_child=_after_fork_in_child)
                _fork_hook = True
        except Exception as err:  # noqa: BLE001
            self._running = False
            record_error("profiler", err)

    def _spawn(self) -> None:
        t = threading.Thread(target=self._run, name="tasktrooper-profiler", daemon=True)
        self._thread = t
        t.start()

    def _after_fork(self) -> None:
        # Threads do not survive fork(): restart sampling in the child with fresh state.
        if not self._running:
            return
        self._lock = threading.Lock()
        self._stop = threading.Event()
        self._spawn()

    def stop(self, timeout_s: float = 5.0) -> None:
        """Stops sampling; the running window is cut short and uploaded (bounded by ``timeout_s``)."""
        try:
            if not self._running:
                return
            self._running = False
            self._stop.set()
            t = self._thread
            if t is not None and t is not threading.current_thread():
                t.join(timeout_s)
            if self._started_tracemalloc and tracemalloc.is_tracing():
                tracemalloc.stop()
                self._started_tracemalloc = False
        except Exception as err:  # noqa: BLE001
            record_error("profiler", err)

    # -- loop --------------------------------------------------------------------------------------------------------

    def _run(self) -> None:
        cfg = self.config
        interval = 1.0 / cfg.hz
        sample_types = [t for t in self.types if t in ("cpu", "wall")]
        collector = Collector(sample_types, int(interval * 1e9), self.cwd, self._cpu_clock) if sample_types else None
        me = threading.get_ident()
        while True:
            period_start = time.time()
            period_mono = time.monotonic()
            stopped = False
            try:
                if collector is not None:
                    collector.reset()
                    window_end = period_mono + cfg.cpu_duration_s
                    nxt = period_mono
                    while time.monotonic() < window_end:
                        try:
                            frames = sys._current_frames()  # noqa: SLF001
                            collector.sample(frames, me, thread_entries(), time.monotonic_ns())
                            del frames
                        except Exception as err:  # noqa: BLE001
                            record_error("profiler.cpu", err)
                        nxt += interval
                        delay = nxt - time.monotonic()
                        if delay < 0:
                            nxt, delay = time.monotonic(), 0.0
                        if self._stop.wait(delay):
                            break
                    self._upload_samples(collector, period_start, time.time())
                stopped = self._stop.is_set()
                if not stopped:
                    rest = period_mono + cfg.period_s - time.monotonic()
                    stopped = rest > 0 and self._stop.wait(rest)
                if "inuse_space" in self.types:
                    self._upload_heap(period_start)
            except Exception as err:  # noqa: BLE001
                record_error("profiler", err)
                stopped = self._stop.wait(1.0)
            if stopped:
                break

    def _meta(self, type_: str, start_s: float, end_s: float) -> Dict[str, Any]:
        return {**self.meta_base, "type": type_, "start": start_s * 1000.0, "end": end_s * 1000.0}

    def _upload_samples(self, c: Collector, start: float, end: float) -> None:
        if c.samples == 0 or end <= start:
            return
        if c.cpu:
            self.uploader.upload(self._meta("cpu", start, end), folded_text(c.cpu_stacks))
        if c.wall:
            self.uploader.upload(self._meta("wall", start, end), folded_text(c.wall_stacks))

    def _upload_heap(self, start: float) -> None:
        try:
            if not tracemalloc.is_tracing():
                return
            snap = tracemalloc.take_snapshot()
            end = time.time()
            folded = folded_text(tracemalloc_stacks(snap, self.cwd, self._index))
            self.uploader.upload(self._meta("inuse_space", start, max(end, start + 0.001)), folded)
        except Exception as err:  # noqa: BLE001
            record_error("profiler", err)
