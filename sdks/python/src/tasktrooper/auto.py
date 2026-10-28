"""``import tasktrooper.auto`` — initializes the SDK from the environment (``TROOPER_*`` / ``OTEL_*``).

Used by ``tasktrooper-run`` (through ``sitecustomize``) and handy as the first import of an entry module::

    import tasktrooper.auto  # noqa: F401
"""

from ._init import init

handle = init()
