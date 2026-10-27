"""Route template normalization (.ai/contracts.md §4, same rules as the Go/JVM/Node SDKs and the writer).

::

    /users/{id}             -> /users/:id      (FastAPI / Starlette / OpenAPI)
    /users/{id:int}         -> /users/:id      (Starlette convertor)
    /files/{p:path}         -> /files/*p       (Starlette path convertor = catch-all)
    /users/<int:id>         -> /users/:id      (Flask / Werkzeug)
    /files/<path:p>         -> /files/*p
    users/<int:pk>/         -> /users/:pk      (Django path())
    ^users/(?P<pk>\\d+)/$    -> /users/:pk      (Django re_path())
    /files/{p...} {*p} *p   -> /files/*p       (catch-all)

Raw number / uuid / hex / ulid segments become ``:id`` (:func:`normalize_path`). gRPC method names are kept.
"""

from __future__ import annotations

import re
from typing import Optional

MAX_ROUTE_LEN = 256

_GRPC = re.compile(r"^/[\w.]+/\w+$")
_ANGLE = re.compile(r"<(?:([^:<>]+):)?([^<>]+)>")
_COLON = re.compile(r":([A-Za-z_$][\w$]*)(\((?:[^()]|\([^()]*\))*\))?[?+*]?")
_REGEX_GROUP = re.compile(r"\(\?P<([A-Za-z_]\w*)>(?:[^()]|\([^()]*\))*\)")
_UUID = re.compile(r"^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$")
_ULID = re.compile(r"^[0-9A-HJKMNP-TV-Za-hjkmnp-tv-z]{26}$")
_CATCH_ALL_CONVERTERS = {"path"}


def normalize_route(route: Optional[str]) -> Optional[str]:
    """Normalizes a framework route template to the TaskTrooper form (``/users/:id``). ``None`` for empty input."""
    if route is None:
        return None
    r = str(route).strip()
    if not r:
        return None
    if r.startswith("/") and "." in r and ":" not in r and _GRPC.match(r):
        return r  # gRPC: /pkg.Service/Method
    if "(?P<" in r or r.startswith("^") or r.endswith("$"):
        r = _regex_route(r)
    if "{" in r:
        r = _braces(r)
    if "<" in r:
        r = _ANGLE.sub(_angle, r)
    if ":" in r:
        r = _COLON.sub(lambda m: ":" + m.group(1), r)
    r = r.replace("(.*)", "*")
    if not r.startswith("/"):
        r = "/" + r  # Django routes have no leading slash
    if len(r) > 1:
        r = r.rstrip("/") or "/"
    r = re.sub(r"/{2,}", "/", r)
    return normalize_path(r, template=True)


def _angle(m: "re.Match[str]") -> str:
    conv, name = m.group(1), m.group(2)
    if conv and conv.strip() in _CATCH_ALL_CONVERTERS:
        return "*" + name
    return ":" + name


def _regex_route(r: str) -> str:
    """Django ``re_path`` patterns: named groups → ``:name``, anchors dropped."""
    r = _REGEX_GROUP.sub(lambda m: ":" + m.group(1), r)
    if r.startswith("^"):
        r = r[1:]
    if r.endswith("$"):
        r = r[:-1]
    r = r.replace("\\Z", "").replace("\\.", ".").replace("\\-", "-")
    return r


def _braces(route: str) -> str:
    out = []
    i = 0
    n = len(route)
    while i < n:
        c = route[i]
        if c != "{":
            out.append(c)
            i += 1
            continue
        depth = 0
        j = i
        while j < n:
            if route[j] == "{":
                depth += 1
            elif route[j] == "}":
                depth -= 1
                if depth == 0:
                    break
            j += 1
        if j >= n:
            out.append(route[i:])
            break
        name = route[i + 1 : j]
        conv = ""
        k = name.find(":")
        if k >= 0:
            name, conv = name[:k], name[k + 1 :]
        if name == "$":
            pass  # end anchor
        elif name.endswith("..."):
            out.append("*" + name[:-3])
        elif name.startswith("*"):
            out.append("*" + name[1:])
        elif conv.strip() in _CATCH_ALL_CONVERTERS:
            out.append("*" + name)
        else:
            out.append(":" + name)
        i = j + 1
    return "".join(out)


def normalize_path(p: str, template: bool = False) -> str:
    """Normalizes a URL path (port of the writer's ``norm.Route``).

    Query / fragment dropped, empty segments collapsed, id-like segments replaced by ``:id``. With
    ``template=True``, ``:param`` / ``*param`` segments are kept.
    """
    path = p
    for sep in ("?", "#"):
        q = path.find(sep)
        if q >= 0:
            path = path[:q]
    scheme = path.find("://")
    if scheme >= 0:
        rest = path[scheme + 3 :]
        slash = rest.find("/")
        path = rest[slash:] if slash >= 0 else "/"
    if path in ("", "/"):
        return "/"
    segs = []
    for seg in path.split("/"):
        if not seg:
            continue
        if template and (seg.startswith(":") or seg.startswith("*")):
            segs.append(seg)
        else:
            segs.append(":id" if is_id(seg) else seg)
    out = "/" + "/".join(segs)
    return out[:MAX_ROUTE_LEN]


def is_id(s: str) -> bool:
    """Whether a path segment looks like an identifier rather than a static word (writer's ``norm.IsID``)."""
    n = len(s)
    if n == 0:
        return False
    digits = hex_letters = letters = other = 0
    for ch in s:
        o = ord(ch)
        if 48 <= o <= 57:
            digits += 1
        elif 97 <= o <= 102 or 65 <= o <= 70:
            hex_letters += 1
        elif 103 <= o <= 122 or 71 <= o <= 90:
            letters += 1
        else:
            other += 1
    if digits == n:
        return True
    if n == 36 and _UUID.match(s):
        return True
    if letters == 0 and other == 0 and n >= 8 and digits > 0:
        return True
    if n == 26 and other == 0 and digits > 0 and _ULID.match(s):
        return True
    if n >= 20 and digits >= 4 and digits + hex_letters + letters >= n - 2:
        return True
    if digits >= 6 and digits * 2 >= n:
        return True
    return False


def strip_query_values(u: str) -> str:
    """Removes query values (keys kept) and the fragment: ``https://x/a?b=1&c=2#f`` → ``https://x/a?b&c``."""
    s = u
    h = s.find("#")
    if h >= 0:
        s = s[:h]
    q = s.find("?")
    if q < 0:
        return s
    keys = [kv.split("=", 1)[0] for kv in s[q + 1 :].split("&") if kv]
    return s[:q] + "?" + "&".join(keys) if keys else s[:q]


def strip_query_string(q: str) -> str:
    """``a=1&b=2`` → ``a&b``."""
    return strip_query_values("?" + q.lstrip("?"))[1:]
