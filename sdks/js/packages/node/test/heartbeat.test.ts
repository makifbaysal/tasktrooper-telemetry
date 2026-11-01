import { metrics, trace } from '@opentelemetry/api';
import { afterEach, describe, expect, it } from 'vitest';
import { heartbeatTiming, init, type TrooperHandle } from '../src/init';
import { decodeMetrics, startFakeIngest } from './otlp';

/* trooper.sdk.heartbeat (.ai/contracts.md §4): exported without any traffic, early (not a full interval later). */
describe('heartbeat', () => {
  let ingest: Awaited<ReturnType<typeof startFakeIngest>> | undefined;
  let handle: TrooperHandle | undefined;

  const heartbeats = () =>
    (ingest?.received ?? [])
      .filter((r) => r.path === '/v1/metrics')
      .flatMap((r) => decodeMetrics(r.body))
      .filter((m) => m.name === 'trooper.sdk.heartbeat');

  const start = async (metricsOn: boolean) => {
    ingest = await startFakeIngest();
    heartbeatTiming.earlyExportMs = 200;
    handle = init({
      dsn: `http://hb_key@127.0.0.1:${ingest.port}/1`,
      service: 'node-heartbeat',
      metricIntervalMs: 60_000,
      metrics: metricsOn,
      disableInstrumentations: true,
    });
  };

  afterEach(async () => {
    await handle?.shutdown();
    await ingest?.close();
    metrics.disable();
    trace.disable();
  });

  it('is exported by the early flush, with the resource service name', async () => {
    await start(true);
    const deadline = Date.now() + 5_000;
    while (Date.now() < deadline && heartbeats().length === 0) await new Promise((r) => setTimeout(r, 50));
    const hb = heartbeats();
    expect(hb.length).toBeGreaterThan(0);
    expect(hb[0]!.scope).toBe('TaskTrooper.Telemetry');
    expect(hb[0]!.resource['service.name']).toBe('node-heartbeat');
    expect(hb[0]!.points).toEqual([{ value: 1, attributes: {} }]);
  });

  it('is not sent when metrics are disabled', async () => {
    await start(false);
    await new Promise((r) => setTimeout(r, 600));
    await handle!.shutdown();
    expect(ingest!.received.filter((r) => r.path === '/v1/metrics')).toEqual([]);
  });
});
