import pytest

from tasktrooper.config import DsnError, parse_dsn, parse_headers, resolve_config, resolve_llm, resolve_profiling


@pytest.mark.parametrize(
    "dsn,endpoint,key,project",
    [
        ("http://tt_dev_ingest_key@localhost:4318/1", "http://localhost:4318", "tt_dev_ingest_key", "1"),
        ("https://abc@ingest.trooper.dev/42", "https://ingest.trooper.dev", "abc", "42"),
        ("https://abc@ingest.trooper.dev/prefix/path/42/", "https://ingest.trooper.dev/prefix/path", "abc", "42"),
        ("https://a%2Fb@h.example", "https://h.example", "a/b", None),
        ("  https://k@[::1]:9000/7 ", "https://[::1]:9000", "k", "7"),
    ],
)
def test_parse_dsn(dsn, endpoint, key, project):
    d = parse_dsn(dsn)
    assert (d.endpoint, d.key, d.project_id) == (endpoint, key, project)


@pytest.mark.parametrize("dsn", ["", "   ", "ftp://k@h/1", "https://h.example/1", "https://k@/1", "https://k@h:notaport/1"])
def test_parse_dsn_invalid(dsn):
    with pytest.raises(DsnError):
        parse_dsn(dsn)


def test_signal_urls_and_headers_from_dsn():
    c = resolve_config(env={"TROOPER_DSN": "http://key1@localhost:4318/1"})
    assert c.traces_url == "http://localhost:4318/v1/traces"
    assert c.metrics_url == "http://localhost:4318/v1/metrics"
    assert c.logs_url == "http://localhost:4318/v1/logs"
    assert c.profiles_url == "http://localhost:4318/v1/profiles"
    assert c.headers == {"x-trooper-key": "key1"}
    assert c.exporting


def test_no_dsn_no_export():
    c = resolve_config(env={})
    assert c.traces_url is None and c.profiles_url is None and not c.exporting
    assert c.service == "unknown_service:python"
    assert c.enabled and c.sample_rate == 1.0


def test_invalid_dsn_is_reported_not_raised():
    c = resolve_config(dsn="nope://x", env={})
    assert c.dsn_error and c.traces_url is None


@pytest.mark.parametrize(
    "explicit,env,want",
    [
        ("svc-explicit", {"OTEL_SERVICE_NAME": "otel", "TROOPER_SERVICE": "trooper"}, "svc-explicit"),
        (None, {"OTEL_SERVICE_NAME": "otel", "TROOPER_SERVICE": "trooper"}, "otel"),
        (None, {"OTEL_RESOURCE_ATTRIBUTES": "service.name=res", "TROOPER_SERVICE": "trooper"}, "res"),
        (None, {"TROOPER_SERVICE": "trooper"}, "trooper"),
    ],
)
def test_service_precedence(explicit, env, want):
    assert resolve_config(service=explicit, env=env).service == want


@pytest.mark.parametrize(
    "explicit,env,want",
    [
        (0.5, {"OTEL_TRACES_SAMPLER_ARG": "0.25", "TROOPER_SAMPLE_RATE": "0.1"}, 0.5),
        (None, {"OTEL_TRACES_SAMPLER_ARG": "0.25", "TROOPER_SAMPLE_RATE": "0.1"}, 0.25),
        (None, {"TROOPER_SAMPLE_RATE": "0.1"}, 0.1),
        (None, {"TROOPER_SAMPLE_RATE": "2"}, 1.0),
        (0, {"TROOPER_SAMPLE_RATE": "0.3"}, 0.3),  # invalid explicit falls through
        (None, {"TROOPER_SAMPLE_RATE": "abc"}, 1.0),
    ],
)
def test_sample_rate_precedence(explicit, env, want):
    assert resolve_config(sample_rate=explicit, env=env).sample_rate == want


def test_env_and_version_precedence():
    env = {
        "OTEL_RESOURCE_ATTRIBUTES": "deployment.environment.name=staging,service.version=v2",
        "TROOPER_ENV": "prod",
        "TROOPER_VERSION": "v1",
    }
    c = resolve_config(env=env)
    assert (c.environment, c.version) == ("staging", "v2")
    c = resolve_config(environment="dev", version="v3", env=env)
    assert (c.environment, c.version) == ("dev", "v3")
    c = resolve_config(env={"TROOPER_ENV": "prod", "TROOPER_VERSION": "v1"})
    assert (c.environment, c.version) == ("prod", "v1")


def test_otel_endpoint_wins_over_dsn_and_headers_merge():
    env = {
        "TROOPER_DSN": "https://k@ingest.example/1",
        "OTEL_EXPORTER_OTLP_ENDPOINT": "http://collector:4318/",
        "OTEL_EXPORTER_OTLP_TRACES_ENDPOINT": "http://traces:4318/v1/traces",
        "OTEL_EXPORTER_OTLP_HEADERS": "X-Extra=a%20b,bad",
    }
    c = resolve_config(env=env)
    assert c.traces_url == "http://traces:4318/v1/traces"
    assert c.metrics_url == "http://collector:4318/v1/metrics"
    assert c.headers == {"x-extra": "a b", "x-trooper-key": "k"}


def test_signals_can_be_disabled():
    c = resolve_config(dsn="http://k@h/1", logs=False, metrics=False, env={})
    assert c.logs_url is None and c.metrics_url is None and c.traces_url


@pytest.mark.parametrize(
    "explicit,env,want",
    [(None, {"TROOPER_ENABLED": "false"}, False), (True, {"TROOPER_ENABLED": "false"}, True), (False, {}, False), (None, {}, True)],
)
def test_enabled(explicit, env, want):
    assert resolve_config(enabled=explicit, env=env).enabled is want


def test_parse_headers():
    assert parse_headers("a=1, B = x%3Dy ,=z,c") == {"a": "1", "b": "x=y"}


def test_profiling_resolution():
    assert resolve_profiling(None, {}) is None
    assert resolve_profiling(False, {"TROOPER_PROFILING": "true"}) is None
    p = resolve_profiling(None, {"TROOPER_PROFILING": "true"})
    assert p and p.types == ["cpu"] and p.period_s == 60 and p.cpu_duration_s == 10 and p.hz == 100
    p = resolve_profiling(
        None,
        {"TROOPER_PROFILING": "1", "TROOPER_PROFILING_TYPES": "wall, CPU,bogus", "TROOPER_PROFILING_PERIOD": "30", "TROOPER_PROFILING_CPU_DURATION": "45"},
    )
    assert p.types == ["wall", "cpu"] and p.period_s == 30 and p.cpu_duration_s == 30  # duration capped by period
    p = resolve_profiling({"types": ["inuse_space"], "hz": 50}, {})
    assert p.types == ["inuse_space"] and p.hz == 50
    assert resolve_profiling({"enabled": False}, {}) is None
    assert resolve_profiling(None, {"TROOPER_PROFILING": "true", "AWS_LAMBDA_FUNCTION_NAME": "f"}) is None


def test_llm_resolution():
    assert resolve_llm(None, {}).capture_content is False
    assert resolve_llm(None, {"TROOPER_LLM_CAPTURE_CONTENT": "true"}).capture_content is True
    assert resolve_llm({"capture_content": False}, {"TROOPER_LLM_CAPTURE_CONTENT": "true"}).capture_content is False
    assert resolve_llm({"inject_stream_usage": True}, {}).inject_stream_usage is True
