import { describe, expect, it } from 'vitest';
import { parseDsn, resolveConfig } from '../src/config';

describe('parseDsn', () => {
  it('parses key, endpoint and project', () => {
    expect(parseDsn('http://tt_dev_ingest_key@localhost:4318/1')).toEqual({
      endpoint: 'http://localhost:4318',
      key: 'tt_dev_ingest_key',
      projectId: '1',
    });
    expect(parseDsn('https://k@ingest.example.com/prefix/42')).toEqual({
      endpoint: 'https://ingest.example.com/prefix',
      key: 'k',
      projectId: '42',
    });
    expect(parseDsn('https://k@ingest.example.com').projectId).toBeUndefined();
  });

  it('rejects invalid DSNs', () => {
    expect(() => parseDsn('')).toThrow();
    expect(() => parseDsn('ftp://k@h/1')).toThrow(/scheme/);
    expect(() => parseDsn('https://host/1')).toThrow(/key/);
  });
});

describe('resolveConfig', () => {
  it('derives signal URLs and auth header from the DSN', () => {
    const c = resolveConfig({}, { TROOPER_DSN: 'http://key@localhost:4318/1', TROOPER_SERVICE: 'orders', TROOPER_ENV: 'prod' });
    expect(c.tracesUrl).toBe('http://localhost:4318/v1/traces');
    expect(c.metricsUrl).toBe('http://localhost:4318/v1/metrics');
    expect(c.logsUrl).toBe('http://localhost:4318/v1/logs');
    expect(c.headers['x-trooper-key']).toBe('key');
    expect(c.service).toBe('orders');
    expect(c.environment).toBe('prod');
    expect(c.sampleRate).toBe(1);
  });

  it('explicit > OTEL_* > TROOPER_*', () => {
    const env = {
      TROOPER_DSN: 'http://key@localhost:4318/1',
      OTEL_EXPORTER_OTLP_ENDPOINT: 'http://collector:4318/',
      OTEL_SERVICE_NAME: 'otel-name',
      TROOPER_SERVICE: 'trooper-name',
      OTEL_TRACES_SAMPLER_ARG: '0.5',
      TROOPER_SAMPLE_RATE: '0.1',
    };
    const c = resolveConfig({}, env);
    expect(c.tracesUrl).toBe('http://collector:4318/v1/traces');
    expect(c.headers['x-trooper-key']).toBe('key');
    expect(c.service).toBe('otel-name');
    expect(c.sampleRate).toBe(0.5);
    const e = resolveConfig({ service: 'explicit', sampleRate: 0.25 }, env);
    expect(e.service).toBe('explicit');
    expect(e.sampleRate).toBe(0.25);
  });

  it('no DSN → no export; invalid DSN recorded, not thrown', () => {
    expect(resolveConfig({}, {}).tracesUrl).toBeUndefined();
    const bad = resolveConfig({ dsn: 'nope' }, {});
    expect(bad.tracesUrl).toBeUndefined();
    expect(bad.dsnError).toBeTruthy();
  });

  it('TROOPER_ENABLED=false disables; invalid sample rates fall back', () => {
    expect(resolveConfig({}, { TROOPER_ENABLED: 'false' }).enabled).toBe(false);
    expect(resolveConfig({ enabled: true }, { TROOPER_ENABLED: 'false' }).enabled).toBe(true);
    expect(resolveConfig({ sampleRate: 2 }, { TROOPER_SAMPLE_RATE: 'x' }).sampleRate).toBe(1);
  });

  it('OTEL_EXPORTER_OTLP_HEADERS supplies auth without a DSN', () => {
    const c = resolveConfig({}, { OTEL_EXPORTER_OTLP_ENDPOINT: 'http://c:4318', OTEL_EXPORTER_OTLP_HEADERS: 'x-trooper-key=abc,foo=b%20r' });
    expect(c.headers).toEqual({ 'x-trooper-key': 'abc', foo: 'b r' });
  });
});
