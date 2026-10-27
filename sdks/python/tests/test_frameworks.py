"""Route templates from real framework instrumentations (OTel contrib) → trooper.route / trooper.entry.route."""

import pytest
from opentelemetry.trace import SpanKind

from .conftest import PROVIDER


def _server(spans):
    srv = [s for s in spans.get_finished_spans() if s.kind == SpanKind.SERVER]
    assert len(srv) == 1, [s.name for s in spans.get_finished_spans()]
    return srv[0]


def test_fastapi_route_and_entry(spans):
    pytest.importorskip("opentelemetry.instrumentation.fastapi")
    from fastapi import FastAPI
    from fastapi.testclient import TestClient
    from opentelemetry.instrumentation.fastapi import FastAPIInstrumentor

    app = FastAPI()
    tracer = PROVIDER.get_tracer("app")

    @app.get("/items/{item_id}")
    def item(item_id: int):
        with tracer.start_as_current_span("load-item"):
            pass
        return {"id": item_id}

    @app.get("/files/{rest:path}")
    async def files(rest: str):
        return {"p": rest}

    FastAPIInstrumentor.instrument_app(app, tracer_provider=PROVIDER)
    try:
        c = TestClient(app)
        r = c.get("/items/42?secret=1", headers={"baggage": "tt.svc=shop-web"})
        assert r.status_code == 200
        srv = _server(spans)
        a = srv.attributes
        assert a["trooper.route"] == "/items/:item_id"
        assert (a["trooper.entry.route"], a["trooper.entry.method"]) == ("/items/:item_id", "GET")
        assert a["trooper.caller"] == "shop-web"
        assert a["trooper.sample_rate"] == 1.0
        assert "secret=1" not in str(dict(a))
        child = [s for s in spans.get_finished_spans() if s.name == "load-item"][0]
        assert child.attributes["trooper.entry.route"] == "/items/:item_id"
        spans.clear()
        c.get("/files/a/b/c.txt")
        assert _server(spans).attributes["trooper.route"] == "/files/*rest"
    finally:
        FastAPIInstrumentor.uninstrument_app(app)


def test_flask_route_and_entry(spans):
    pytest.importorskip("opentelemetry.instrumentation.flask")
    from flask import Flask
    from opentelemetry.instrumentation.flask import FlaskInstrumentor

    app = Flask("t")
    tracer = PROVIDER.get_tracer("app")

    @app.route("/users/<int:user_id>/orders/<order_id>", methods=["POST"])
    def order(user_id, order_id):
        with tracer.start_as_current_span("charge"):
            pass
        return "ok"

    FlaskInstrumentor.instrument_app(app, tracer_provider=PROVIDER)
    try:
        r = app.test_client().post("/users/7/orders/abc", headers={"baggage": "tt.svc=mobile-app"})
        assert r.status_code == 200
        a = _server(spans).attributes
        assert a["trooper.route"] == "/users/:user_id/orders/:order_id"
        assert (a["trooper.entry.route"], a["trooper.entry.method"]) == ("/users/:user_id/orders/:order_id", "POST")
        assert a["trooper.caller"] == "mobile-app"
        child = [s for s in spans.get_finished_spans() if s.name == "charge"][0]
        assert child.attributes["trooper.entry.method"] == "POST"
    finally:
        FlaskInstrumentor.uninstrument_app(app)


def test_django_route(spans):
    pytest.importorskip("opentelemetry.instrumentation.django")
    django = pytest.importorskip("django")
    from django.conf import settings

    if not settings.configured:
        settings.configure(
            DEBUG=False,
            ALLOWED_HOSTS=["*"],
            ROOT_URLCONF=__name__ + "_urls",
            MIDDLEWARE=[],
            SECRET_KEY="t",
            INSTALLED_APPS=[],
        )
    import sys
    import types

    from django.http import HttpResponse
    from django.urls import path, re_path

    mod = types.ModuleType(__name__ + "_urls")
    mod.urlpatterns = [
        path("api/users/<int:pk>/", lambda req, pk: HttpResponse("u")),
        re_path(r"^legacy/(?P<slug>[\w-]+)/$", lambda req, slug: HttpResponse("l")),
    ]
    sys.modules[mod.__name__] = mod
    django.setup()
    from django.test import Client
    from opentelemetry.instrumentation.django import DjangoInstrumentor

    DjangoInstrumentor().instrument(tracer_provider=PROVIDER)
    try:
        assert Client().get("/api/users/5/").status_code == 200
        assert _server(spans).attributes["trooper.route"] == "/api/users/:pk"
        spans.clear()
        assert Client().get("/legacy/hello-world/").status_code == 200
        assert _server(spans).attributes["trooper.route"] == "/legacy/:slug"
    finally:
        DjangoInstrumentor().uninstrument()
