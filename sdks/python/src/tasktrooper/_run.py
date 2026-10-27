"""``tasktrooper-run``: runs a command with the SDK initialized (like ``opentelemetry-instrument``).

::

    tasktrooper-run --service orders-api uvicorn app:app --workers 4
    TROOPER_DSN=... tasktrooper-run gunicorn app:app
    tasktrooper-run --profiling python worker.py

Options become ``TROOPER_*`` environment variables; a ``sitecustomize`` module on ``PYTHONPATH`` runs
``import tasktrooper.auto`` in the command's Python process (and in the worker processes it starts).
"""

from __future__ import annotations

import argparse
import os
import shutil
import sys
from typing import List, Optional

_FLAGS = [
    ("--dsn", "TROOPER_DSN", "ingest DSN https://<key>@<host>/<project>"),
    ("--service", "TROOPER_SERVICE", "service.name"),
    ("--env", "TROOPER_ENV", "deployment.environment.name"),
    ("--version", "TROOPER_VERSION", "service.version (deploy marker)"),
    ("--sample-rate", "TROOPER_SAMPLE_RATE", "head sampling rate (0, 1]"),
    ("--profiling-types", "TROOPER_PROFILING_TYPES", "cpu,wall,inuse_space"),
]


def _parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(
        prog="tasktrooper-run",
        description="Run a Python command with TaskTrooper telemetry (traces, metrics, logs, profiles).",
        usage="tasktrooper-run [options] command [args ...]",
    )
    for flag, env, help_ in _FLAGS:
        p.add_argument(flag, dest=env, help=f"{help_} (env {env})")
    p.add_argument("--profiling", action="store_true", help="enable continuous profiling (TROOPER_PROFILING=true)")
    p.add_argument(
        "--capture-llm-content", action="store_true", help="record LLM prompts / completions (TROOPER_LLM_CAPTURE_CONTENT=true)"
    )
    p.add_argument("--disabled", action="store_true", help="run without telemetry (TROOPER_ENABLED=false)")
    p.add_argument("command", nargs=argparse.REMAINDER)
    return p


def build_env(argv: List[str], environ: Optional[dict] = None) -> "tuple[List[str], dict]":
    args = _parser().parse_args(argv)
    cmd = list(args.command)
    if cmd and cmd[0] == "--":
        cmd = cmd[1:]
    env = dict(os.environ if environ is None else environ)
    for _, key, _ in _FLAGS:
        v = getattr(args, key)
        if v:
            env[key] = v
    if args.profiling:
        env["TROOPER_PROFILING"] = "true"
    if args.capture_llm_content:
        env["TROOPER_LLM_CAPTURE_CONTENT"] = "true"
    if args.disabled:
        env["TROOPER_ENABLED"] = "false"
    boot = os.path.join(os.path.dirname(os.path.abspath(__file__)), "_bootstrap")
    pp = env.get("PYTHONPATH", "")
    parts = [x for x in pp.split(os.pathsep) if x and x != boot]
    env["PYTHONPATH"] = os.pathsep.join([boot] + parts)
    return cmd, env


def main(argv: Optional[List[str]] = None) -> None:
    cmd, env = build_env(sys.argv[1:] if argv is None else argv)
    if not cmd:
        _parser().print_usage(sys.stderr)
        sys.exit(2)
    exe = shutil.which(cmd[0], path=env.get("PATH")) or cmd[0]
    os.execvpe(exe, cmd, env)


if __name__ == "__main__":
    main()
