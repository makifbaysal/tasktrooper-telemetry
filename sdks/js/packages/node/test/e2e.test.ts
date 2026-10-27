import { connect } from 'node:net';
import { createRequire } from 'node:module';
import { context, metrics, propagation, trace } from '@opentelemetry/api';
import { HttpInstrumentation } from '@opentelemetry/instrumentation-http';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';
import { init, type TrooperHandle } from '../src/init';
import { trooperExpress } from '../src/frameworks';
import { processMessage } from '../src/messaging';
import { decodeTraces, startFakeIngest, type DecodedSpan } from './otlp';

/** Raw HTTP/1.1 request over a socket (no instrumented client, so the upstream headers are exactly ours). */
function rawGet(port: number, path: string, headers: Record<string, string>): Promise<string> {
  return new Promise((resolve, reject) => {
    const sock = connect(port, '127.0.0.1', () => {
      const lines = [`GET ${path} HTTP/1.1`, 'Host: 127.0.0.1', 'Connection: close', ...Object.entries(headers).map(([k, v]) => `${k}: ${v}`)];
      sock.write(lines.join('\r\n') + '\r\n\r\n');
    });
    let data = '';
    sock.on('data', (c) => (data += c.toString()));
    sock.on('end', () => resolve(data));
    sock.on('error', reject);
  });
}

describe('e2e: init() → OTLP/HTTP protobuf to a fake ingest gateway', () => {
  let ingest: Awaited<ReturnType<typeof startFakeIngest>>;
  let handle: TrooperHandle;
  let spans: DecodedSpan[] = [];
  let traceHeaders: Record<string, unknown> = {};

  beforeAll(async () => {
    ingest = await startFakeIngest();
    handle = init({
      dsn: `http://e2e_key@127.0.0.1:${ingest.port}/1`,
      service: 'e2e-svc',
      environment: 'test',
      version: '1.2.3',
      batchDelayMs: 50,
      metricIntervalMs: 60_000,
      // the fake ingest server itself must not be traced
      disableInstrumentations: ['http'],
      instrumentations: [new HttpInstrumentation({ ignoreIncomingRequestHook: (req) => /^\/v1\/(traces|metrics|logs)/.test(req.url ?? '') })],
    });
    expect(handle.exporting).toBe(true);

    const require = createRequire(import.meta.url);
    const http = require('node:http') as typeof import('node:http');
    const express = require('express') as typeof import('express');
    const app = express();
    app.use(trooperExpress());
    const router = express.Router();
    const pino = require('pino') as typeof import('pino').default;
    const sink = { write: () => {} }; // pino output itself is irrelevant; the OTel log bridge exports the record
    const logger = pino({}, sink as never);
    router.get('/users/:id', (_req, res) => {
      trace.getTracer('app').startActiveSpan('load-user', (s) => s.end());
      logger.info({ userId: 'x' }, 'user loaded');
      res.json({ ok: true });
    });
    app.use('/v1', router);
    const server = http.createServer(app);
    await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
    const port = (server.address() as { port: number }).port;

    const resp = await rawGet(port, '/v1/users/42?token=secret', {
      traceparent: '00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01',
      tracestate: 'tt=r:0.25',
      baggage: 'tt.svc=gateway',
    });
    expect(resp).toContain('200 OK');

    await processMessage({ system: 'kafka', destination: 'orders', consumerGroup: 'billing', headers: { 'tt-attempt': '3' } }, async () => {
      traceHeaders = {};
      propagation.inject(context.active(), traceHeaders);
    });

    await handle.forceFlush();
    server.close();
    for (const r of ingest.received.filter((x) => x.path === '/v1/traces')) spans.push(...decodeTraces(r.body));
  });

  afterAll(async () => {
    await handle?.shutdown();
    await ingest?.close();
    trace.disable();
    metrics.disable();
    context.disable();
    propagation.disable();
  });

  it('posts protobuf with the ingest key', () => {
    const req = ingest.received.find((r) => r.path === '/v1/traces');
    expect(req).toBeDefined();
    expect(req!.headers['x-trooper-key']).toBe('e2e_key');
    expect(req!.headers['content-type']).toBe('application/x-protobuf');
  });

  it('resource carries service, env, version and the distro name', () => {
    const r = spans[0]!.resource;
    expect(r['service.name']).toBe('e2e-svc');
    expect(r['deployment.environment.name']).toBe('test');
    expect(r['service.version']).toBe('1.2.3');
    expect(r['telemetry.distro.name']).toBe('trooper-node');
  });

  it('server span: route template, entry route/method, caller, upstream sample rate, no query values', () => {
    const server = spans.find((s) => s.kind === 2)!;
    expect(server).toBeDefined();
    expect(server.traceId).toBe('4bf92f3577b34da6a3ce929d0e0e4736');
    expect(server.attributes['trooper.route']).toBe('/v1/users/:id');
    expect(server.attributes['trooper.entry.route']).toBe('/v1/users/:id');
    expect(server.attributes['trooper.entry.method']).toBe('GET');
    expect(server.attributes['trooper.caller']).toBe('gateway');
    expect(server.attributes['trooper.sample_rate']).toBe(0.25);
    expect(JSON.stringify(spans)).not.toContain('secret');
  });

  it('child spans carry the entry route', () => {
    const child = spans.find((s) => s.name === 'load-user')!;
    expect(child).toBeDefined();
    expect(child.attributes['trooper.entry.route']).toBe('/v1/users/:id');
    expect(child.attributes['trooper.entry.method']).toBe('GET');
  });

  it('consumer span: process, attempt, CONSUME; tracestate tt=r on the new root; tt.svc on injection', () => {
    const c = spans.find((s) => s.name === 'process orders')!;
    expect(c.kind).toBe(5);
    expect(c.attributes['trooper.process']).toBe('billing');
    expect(c.attributes['trooper.consumer.attempt']).toBe(3);
    expect(c.attributes['trooper.entry.method']).toBe('CONSUME');
    expect(c.attributes['trooper.sample_rate']).toBe(1);
    expect(c.traceState).toBe('tt=r:1');
    expect(String(traceHeaders.baggage)).toContain('tt.svc=e2e-svc');
  });

  it('pino records are exported to /v1/logs with trace correlation', () => {
    const logs = ingest.received.filter((r) => r.path === '/v1/logs');
    expect(logs.length).toBeGreaterThan(0);
    const body = Buffer.concat(logs.map((l) => Buffer.from(l.body)));
    expect(body.includes(Buffer.from('user loaded'))).toBe(true);
    expect(body.includes(Buffer.from('4bf92f3577b34da6a3ce929d0e0e4736', 'hex'))).toBe(true);
    expect(logs[0]!.headers['x-trooper-key']).toBe('e2e_key');
  });

  it('init is idempotent', () => {
    expect(init()).toBe(handle);
  });
});
