import pytest

from tasktrooper.route import is_id, normalize_path, normalize_route, strip_query_string, strip_query_values


@pytest.mark.parametrize(
    "raw,want",
    [
        ("/users/{id}", "/users/:id"),
        ("/users/{user_id:int}", "/users/:user_id"),
        ("/files/{p:path}", "/files/*p"),
        ("/users/<int:id>", "/users/:id"),
        ("/users/<id>", "/users/:id"),
        ("/files/<path:rest>", "/files/*rest"),
        ("users/<int:pk>/", "/users/:pk"),
        ("api/v1/orders/<uuid:order_id>/items/", "/api/v1/orders/:order_id/items"),
        (r"^users/(?P<pk>\d+)/$", "/users/:pk"),
        (r"^articles/(?P<year>[0-9]{4})/(?P<slug>[\w-]+)/$", "/articles/:year/:slug"),
        ("/files/{p...}", "/files/*p"),
        ("/files/{*p}", "/files/*p"),
        ("/files/*p", "/files/*p"),
        ("/users/:id(\\d+)", "/users/:id"),
        ("/users/:id?", "/users/:id"),
        ("/orders/123/items/550e8400-e29b-41d4-a716-446655440000", "/orders/:id/items/:id"),
        ("/pkg.Service/Method", "/pkg.Service/Method"),
        ("", None),
        (None, None),
        ("/", "/"),
        ("//a//b/", "/a/b"),
    ],
)
def test_normalize_route(raw, want):
    assert normalize_route(raw) == want


@pytest.mark.parametrize(
    "seg,want",
    [("123", True), ("users", False), ("550e8400-e29b-41d4-a716-446655440000", True), ("deadbeef", False), ("deadbeef1", True), ("v1", False),
     ("01ARZ3NDEKTSV4RRFFQ69G5FAV", True), ("abc123def456", True), ("", False)],
)
def test_is_id(seg, want):
    assert is_id(seg) is want


def test_normalize_path():
    assert normalize_path("https://x.io/a/42?q=1#f") == "/a/:id"
    assert normalize_path("/a/:id/5", template=True) == "/a/:id/:id"


def test_strip_query():
    assert strip_query_values("https://x/a?b=1&c=2#f") == "https://x/a?b&c"
    assert strip_query_values("/a?") == "/a"
    assert strip_query_string("a=1&b=2") == "a&b"
