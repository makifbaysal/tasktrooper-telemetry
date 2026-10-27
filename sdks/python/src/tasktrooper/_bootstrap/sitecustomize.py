"""Loaded by ``tasktrooper-run`` through PYTHONPATH: initializes the SDK before the application starts."""

import os
import sys


def _trooper_boot() -> None:
    try:
        import tasktrooper.auto  # noqa: F401
    except Exception:  # noqa: BLE001 - never break the host process
        pass
    # Chain to the next sitecustomize on the path (ours shadows it).
    here = os.path.dirname(os.path.abspath(__file__))
    for entry in sys.path:
        try:
            if not entry or os.path.abspath(entry) == here:
                continue
            candidate = os.path.join(entry, "sitecustomize.py")
            if os.path.isfile(candidate):
                import importlib.util

                spec = importlib.util.spec_from_file_location("_trooper_next_sitecustomize", candidate)
                if spec is not None and spec.loader is not None:
                    mod = importlib.util.module_from_spec(spec)
                    spec.loader.exec_module(mod)
                break
        except Exception:  # noqa: BLE001
            break


_trooper_boot()
