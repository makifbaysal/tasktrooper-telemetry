# Python SDK — coding standards

Scope: `sdks/python` (PyPI `tasktrooper`, import `tasktrooper`, src layout `src/tasktrooper`). Cross-cutting rules:
[`../../../.ai/coding-standards.md`](../../../.ai/coding-standards.md). SDK design: [`../../../.ai/sdk.md`](../../../.ai/sdk.md).
Config/env, headers, `trooper.*` attributes, self-metrics, LLM: [`../../../.ai/contracts.md`](../../../.ai/contracts.md) (§2, §4, §24, §25).
Behaviour must match the Go SDK (`sdks/go`) for the same contract; when they differ, the contract wins.

## What this SDK is

- An **OpenTelemetry distribution** (ADR-011, ADR-024): `tasktrooper.init()` sets up OTel providers, OTLP/HTTP
  protobuf exporters, `TrooperSampler`, `TrooperSpanProcessor`, the trooper propagator. Batching/backoff come from the
  OTel SDK exporters; don't write a custom sender.
- Framework/library instrumentation = **OTel contrib instrumentors as optional extras**, registered in
  `instrumentation.INSTRUMENTATIONS` (`name → (module, class, library)`). New one → add the table row **and** the
  `[project.optional-dependencies]` extra (+ `all` bundle) in `pyproject.toml`.
- Provider SDKs (openai, anthropic) are **not** dependencies: `llm/` uses duck typing and returns a proxy; nothing global
  is monkey-patched.

## Host safety (repo rule 5)

- `init()`, `shutdown()`, `force_flush()` and every public helper **never raise**. `init()` with garbage input returns a
  do-nothing `TrooperHandle` (see `test_init_never_raises_on_bad_input`).
- Every swallowed exception is counted via `_errors.record_error(component, err)` →
  `trooper.sdk.errors{component}`. Pattern:
  ```python
  try:
      ...
  except Exception as err:  # noqa: BLE001
      record_error("span_processor", err)
  ```
  or `_errors.safe("resolver", fn, fallback)` for one-liners. Catch `Exception`, never bare `except:` /
  `BaseException` (don't swallow `KeyboardInterrupt` / `SystemExit`).
- Errors before a meter exists are buffered in `_pending` and flushed on `bind_self_metrics`; don't bypass it.
- Component names: dotted lowercase (`exporter.traces`, `instrumentation.<name>`, `profiler.cpu`, `llm`).
  New name → document it in contracts §4.
- Logging: only via `logging.getLogger("tasktrooper")`, and only when `debug` is on. No `print`.
- Imports of optional packages are lazy (`importlib`, inside functions) so a missing extra is skipped silently.
- Background threads (profiler, early export timer) are daemon threads and fork-safe; nothing blocks the request path.

## Privacy and cardinality (repo rules 4, 6)

- Default: no headers, bodies or query values. Use `route.strip_query_values` / `strip_query_string` on URLs.
- Routes go through `route.normalize_route` (`/users/{id}`, `<int:id>`, Django regex → `/users/:id`);
  `normalize_path` falls back to id detection (`is_id`). Never put raw paths or ids in span names / metric labels.
- LLM content capture is opt-in (`TROOPER_LLM_CAPTURE_CONTENT`, `init(llm={"capture_content": True})`).

## Public API

- Everything public is re-exported from `tasktrooper/__init__.py` and listed in `__all__`. Implementation modules are
  `_private` (`_init.py`, `_errors.py`, `_attrs.py`); semi-public modules (`config`, `route`, `entry`, `llm`) have
  plain names.
- `init()` signature: common settings positional-or-keyword, the rest **keyword-only** (after `*`). New parameters go
  after `*` with a `None` default meaning "unset".
- Config precedence is resolved only in `config.resolve_config`: explicit arg > `OTEL_*` > `TROOPER_*` > default.
  Update the README config table and contracts §2 with any new env var.
- DSN parsing only in `config.parse_dsn` (raises `DsnError(ValueError)` with `trooper: ` prefix; callers swallow).
- Attribute key constants live in `_attrs.py` (`ATTR_ROUTE`, …); don't inline `"trooper.*"` strings.
- `init()` is idempotent: later calls return the first handle.

## Style and typing

- **Python ≥ 3.9** (`requires-python`). So: `from __future__ import annotations` in every module with annotations,
  `typing.Optional/Dict/List/Union` (no `X | Y` at runtime, no `match`, no 3.10+ stdlib APIs).
- Full type hints on public functions; the package ships `py.typed` ("Typing :: Typed"), so hints are API.
  Dynamic OTel objects may be `Any`.
- Docstrings: module docstring citing the ADR/contract section; reST markup (``` ``code`` ```, `:func:`).
- Lint: there is **no** ruff/mypy config or gate. Existing `# noqa: BLE001` / `N802` markers assume ruff naming;
  keep them on intentional broad excepts. Line length in practice ≤ 120 (a few lines exceed it).
- Constants with a leading `#:` comment are documented (`_EARLY_EXPORT_S`).

## Versioning and release

- Single version source: `src/tasktrooper/_version.py` (hatch reads it). Bump it and `CHANGELOG.md` together.
- Release: tag `python-vX.Y.Z` (must equal `_version.py`) → `release-python.yml` → PyPI trusted publishing.
  Procedure: `README.md` § Releasing.
- Commits: Conventional Commits, scope `sdk-python` (e.g. `feat(sdk-python): celery consumer attempt`).
