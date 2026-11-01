# Python SDK — testing

Cross-cutting test rules: [`../../../.ai/testing.md`](../../../.ai/testing.md). Contract under test:
[`../../../.ai/contracts.md`](../../../.ai/contracts.md).

## Gates — what actually runs

| Where | Runs Python SDK tests? |
|---|---|
| `.github/workflows/ci.yml` | **No.** |
| `make check` (repo root) | **No.** |
| `.github/workflows/release-python.yml` (on `python-v*` tag) | Yes: `pip install -e '.[dev,django]'`, `pytest -q`, `python -m build`, `twine check --strict`, on **Python 3.12 only**. |

No lint/type check runs anywhere. Python 3.9–3.11/3.13 (claimed in classifiers) are not exercised by any gate.

## Required locally before every SDK PR

```bash
cd sdks/python
uv venv .venv && uv pip install --python .venv/bin/python -e '.[dev,django]'
.venv/bin/python -m pytest -q
uv pip install --python .venv/bin/python build twine     # not in [dev]; needed for the packaging check
.venv/bin/python -m build && .venv/bin/python -m twine check --strict dist/*   # when touching pyproject/packaging
```

- If you touched typing or used newer stdlib APIs, also run the suite on a 3.9 interpreter
  (`uv venv -p 3.9 …`).
- Still run root `make check` (repo rule 11) if you touched anything outside `sdks/python`.

## Layout and fixtures (`tests/`)

- `conftest.py` installs **one** global `TracerProvider` (`TrooperSampler(1.0)` + `TrooperSpanProcessor` →
  `InMemorySpanExporter`) at import time, because OTel's global provider can be set only once per process.
- Fixtures: `spans` (clears exporter + `entry.reset()`), `tracer`, `httpserver` (threaded fake ingest on `127.0.0.1:0`,
  gunzips bodies, records path + headers; set `cap.status` / `cap.headers` to script responses).
- Helpers: `by_name(exporter, name)`; `run_py(code, env)` runs code in a **fresh interpreter** with all
  `TROOPER_*` / `OTEL_*` env stripped.
- Files: `test_config.py` (DSN/env precedence), `test_route.py`, `test_processor.py`, `test_init.py`,
  `test_frameworks.py`, `test_llm.py`, `test_profiling.py`, `test_live.py`.

## Rules

- **Anything that calls `tasktrooper.init()` runs in `run_py`**, never in-process (it would replace the shared
  provider and leak globals). Assert on stdout:
  ```python
  r = run_py(code, env={"TROOPER_DSN": f"http://k@{host}/1"})
  assert r.returncode == 0 and r.stdout.strip() == "ok True", r.stderr
  ```
- Pure functions → `@pytest.mark.parametrize` tables (`test_route.py`, `test_config.py`); this is the Python form of
  repo rule 8's table-driven tests.
- No real network: export goes to the `httpserver` fixture. Unreachable endpoints use `127.0.0.1:9` and assert the
  call returns within a bound and `internal_error_count()` grew (`test_unreachable_endpoint_counts_errors_and_does_not_block`).
- Assert host safety for every new public entry point: garbage input → no exception, handle returned, error counted.
- Assert privacy: route templates not raw paths, query values stripped, no LLM content unless capture is on.
- Optional deps: guard with `pytest.importorskip("opentelemetry.instrumentation.fastapi")` so the suite passes
  with partial extras. Platform-specific code: `@pytest.mark.skipif(not sys.platform.startswith("linux"), …)`.
- Env: `monkeypatch.setenv` in-process, or `run_py(env=…)`. Reset module state with the test hooks
  (`_errors.reset_self_metrics`, `entry.reset`), never by reaching into private globals.
- Async: `asyncio_mode = "auto"`; just write `async def test_…`.

## Live contract test (optional, needs infra)

`tests/test_live.py` (`pytestmark = pytest.mark.live`) sends spans, an LLM span and profiles from a fresh interpreter,
then polls ClickHouse (`spans`, `profile_stacks`, `llm_1m`) and deletes its rows.

```bash
make infra && make migrate        # repo root; then `make run-writer` and `make run-gw` in two terminals
TROOPER_E2E_DSN=http://<ingest-key>@localhost:4318/<project-id> .venv/bin/python -m pytest -q -m live
```

- Key/project from onboarding or `api bootstrap` (no seed key, ADR-028). `TROOPER_E2E_CLICKHOUSE` defaults to
  `http://localhost:8123` (user/password `trooper`).
- Skipped when `TROOPER_E2E_DSN` is unset or the gateway is unreachable. Plain `pytest -q` also collects it (and skips).
- Run it when changing export, resource attributes, profiling upload or LLM attributes.
