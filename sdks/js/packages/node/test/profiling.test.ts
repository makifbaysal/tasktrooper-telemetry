import { createServer, type IncomingMessage } from 'node:http';
import type { AddressInfo } from 'node:net';
import { gunzipSync } from 'node:zlib';
import { afterEach, describe, expect, it } from 'vitest';
import { resolveConfig, resolveProfiling } from '../src/config';
import { internalErrorCount } from '../src/errors';
import {
  ContinuousProfiler,
  cpuProfileToStacks,
  foldedText,
  frameName,
  heapProfileToStacks,
  MAX_STACK_DEPTH,
  ProfileUploader,
  profileQuery,
  relativePath,
  type CpuProfile,
  type HeapProfile,
  type ProfileMeta,
} from '../src/profiling';

const cf = (functionName: string, url = '') => ({ functionName, url, scriptId: '0', lineNumber: 1, columnNumber: 2 });

/** A small .cpuprofile: (root) → main → (anonymous) work, plus (idle), (program), (garbage collector), node internal. */
const CPU_FIXTURE: CpuProfile = {
  nodes: [
    { id: 1, callFrame: cf('(root)'), children: [2, 4, 5, 6] },
    { id: 2, callFrame: cf('main', 'file:///app/src/index.js'), children: [3, 7] },
    { id: 3, callFrame: cf('', 'file:///app/src/work.js') },
    { id: 4, callFrame: cf('(idle)') },
    { id: 5, callFrame: cf('(program)') },
    { id: 6, callFrame: cf('(garbage collector)') },
    { id: 7, callFrame: cf('readFileSync', 'node:fs') },
  ],
  startTime: 1_000,
  endTime: 1_080,
  // timestamps 1010 1020 1030 1040 1050 1060 1070 → each sample lasts 10 µs (last until endTime)
  samples: [3, 3, 4, 2, 5, 6, 7],
  timeDeltas: [10, 10, 10, 10, 10, 10, 10],
};

describe('frame names', () => {
  it('formats fn (relative/path) and maps V8 pseudo-frames', () => {
    expect(frameName(cf('handler', 'file:///app/src/routes/users.js'), '/app')).toBe('handler (src/routes/users.js)');
    expect(frameName(cf('', '/app/node_modules/express/lib/router.js'), '/app')).toBe('(anonymous) (node_modules/express/lib/router.js)');
    expect(frameName(cf('x', '/elsewhere/a.js'), '/app')).toBe('x (/elsewhere/a.js)');
    expect(frameName(cf('readFile', 'node:fs'), '/app')).toBe('readFile (node:fs)');
    expect(frameName(cf('fn;with;semis\n', 'file:///app/a.js'), '/app')).toBe('fn with semis  (a.js)');
    expect(frameName(cf('(program)'))).toBe('[program]');
    expect(frameName(cf('(garbage collector)'))).toBe('[gc]');
    expect(frameName(cf('nativeFn'))).toBe('nativeFn');
    expect(relativePath('file:///app/src/x.mjs', '/app/')).toBe('src/x.mjs');
  });
});

describe('cpuProfileToStacks', () => {
  it('folds samples root-first with sample durations in ns; idle split out', () => {
    const { stacks, idle } = cpuProfileToStacks(CPU_FIXTURE, '/app');
    expect(Object.fromEntries(stacks)).toEqual({
      'main (src/index.js);(anonymous) (src/work.js)': 20_000,
      'main (src/index.js)': 10_000,
      '[program]': 10_000,
      '[gc]': 10_000,
      'main (src/index.js);readFileSync (node:fs)': 10_000,
    });
    expect(idle).toBe(10_000);
    const text = foldedText(stacks);
    expect(text.split('\n').filter(Boolean)).toContain('main (src/index.js);(anonymous) (src/work.js) 20000');
    expect(text.endsWith('\n')).toBe(true);
  });

  it('clamps negative deltas and handles empty profiles', () => {
    const p: CpuProfile = { ...CPU_FIXTURE, samples: [3, 2], timeDeltas: [10, -5], endTime: 1_020 };
    const { stacks } = cpuProfileToStacks(p, '/app');
    // sample 0 at 1010 lasts until 1005 → 0 (dropped); sample 1 at 1005 lasts until 1020 → 15 µs
    expect(Object.fromEntries(stacks)).toEqual({ 'main (src/index.js)': 15_000 });
    expect(cpuProfileToStacks({ nodes: [], startTime: 0, endTime: 0 }).stacks.size).toBe(0);
  });

  it('truncates deep stacks on the root side', () => {
    const nodes: CpuProfile['nodes'] = [{ id: 0, callFrame: cf('(root)'), children: [1] }];
    for (let i = 1; i <= 200; i++) nodes.push({ id: i, callFrame: cf(`f${i}`, 'file:///app/a.js'), children: i < 200 ? [i + 1] : [] });
    const { stacks } = cpuProfileToStacks({ nodes, startTime: 0, endTime: 20, samples: [200], timeDeltas: [10] }, '/app');
    const [stack] = [...stacks.keys()];
    const frames = stack!.split(';');
    expect(frames).toHaveLength(MAX_STACK_DEPTH);
    expect(frames[0]).toBe('[truncated]');
    expect(frames.at(-1)).toBe('f200 (a.js)');
    expect(frames[1]).toBe('f74 (a.js)');
  });
});

describe('heapProfileToStacks', () => {
  it('folds selfSize per path in bytes', () => {
    const heap: HeapProfile = {
      head: {
        id: 1,
        callFrame: cf('(root)'),
        selfSize: 0,
        children: [
          {
            id: 2,
            callFrame: cf('load', 'file:///app/src/cache.js'),
            selfSize: 1024,
            children: [{ id: 3, callFrame: cf('', 'file:///app/src/cache.js'), selfSize: 4096, children: [] }],
          },
          { id: 4, callFrame: cf('(V8 API)'), selfSize: 512, children: [] },
          { id: 5, callFrame: cf('noop', 'file:///app/x.js'), selfSize: 0, children: [] },
        ],
      },
    };
    expect(Object.fromEntries(heapProfileToStacks(heap, '/app'))).toEqual({
      'load (src/cache.js)': 1024,
      'load (src/cache.js);(anonymous) (src/cache.js)': 4096,
      '(V8 API)': 512,
    });
  });
});

const META: ProfileMeta = { service: 'orders', env: 'prod', version: 'abc', host: 'h1', type: 'cpu', unit: 'nanoseconds', start: 1000.4, end: 2000.2 };

describe('upload', () => {
  it('builds the §24 query string', () => {
    const q = new URLSearchParams(profileQuery(META));
    expect(Object.fromEntries(q)).toEqual({
      format: 'folded',
      type: 'cpu',
      unit: 'nanoseconds',
      service: 'orders',
      env: 'prod',
      version: 'abc',
      host: 'h1',
      runtime: 'node',
      start: '1000',
      end: '2001',
    });
    expect(new URLSearchParams(profileQuery({ ...META, env: undefined, version: undefined })).has('env')).toBe(false);
  });

  it('POSTs gzip folded text with the ingest key', async () => {
    const got: { url?: string; headers?: IncomingMessage['headers']; body?: string } = {};
    const srv = createServer((req, res) => {
      const chunks: Buffer[] = [];
      req.on('data', (c: Buffer) => chunks.push(c));
      req.on('end', () => {
        got.url = req.url;
        got.headers = req.headers;
        got.body = gunzipSync(Buffer.concat(chunks)).toString('utf8');
        res.writeHead(200).end();
      });
    });
    await new Promise<void>((r) => srv.listen(0, '127.0.0.1', r));
    try {
      const port = (srv.address() as AddressInfo).port;
      const up = new ProfileUploader(`http://127.0.0.1:${port}/v1/profiles`, { 'x-trooper-key': 'k1' });
      expect(await up.upload(META, 'a;b 10\n')).toBe(true);
      expect(got.url!.startsWith('/v1/profiles?format=folded&type=cpu&unit=nanoseconds&service=orders')).toBe(true);
      expect(got.headers!['x-trooper-key']).toBe('k1');
      expect(got.headers!['content-encoding']).toBe('gzip');
      expect(got.headers!['content-type']).toMatch(/^text\/plain/);
      expect(got.body).toBe('a;b 10\n');
    } finally {
      srv.close();
    }
  });

  it('backs off on 429/5xx, drops on other 4xx, never throws', async () => {
    let now = 0;
    const calls: number[] = [];
    let status = 503;
    const fetchImpl = async () => {
      calls.push(now);
      return { status, headers: { get: (h: string) => (h === 'retry-after' ? '5' : null) } };
    };
    const up = new ProfileUploader('http://x/v1/profiles', {}, 1000, fetchImpl, () => now);
    const before = internalErrorCount();
    expect(await up.upload(META, 'a 1\n')).toBe(false);
    expect(up.backingOff).toBe(true);
    expect(await up.upload(META, 'a 1\n')).toBe(false); // suppressed, no request
    expect(calls).toHaveLength(1);
    now = 5_001; // Retry-After 5 s
    status = 400;
    expect(await up.upload(META, 'a 1\n')).toBe(false);
    expect(up.backingOff).toBe(false); // 400 drops the profile without backoff
    status = 200;
    expect(await up.upload(META, 'a 1\n')).toBe(true);
    expect(calls).toHaveLength(3);
    expect(internalErrorCount()).toBe(before + 2);

    const throwing = new ProfileUploader('http://x', {}, 1000, () => {
      throw new Error('boom');
    });
    await expect(throwing.upload(META, 'a 1\n')).resolves.toBe(false);
    expect(await up.upload(META, '')).toBe(false); // empty profile: nothing sent
  });
});

describe('profiling config', () => {
  it('is off by default, on via option or env (not in Lambda)', () => {
    expect(resolveProfiling(undefined, {})).toBeUndefined();
    expect(resolveProfiling(true, {})).toMatchObject({ types: ['cpu', 'inuse_space'], periodMs: 60_000, cpuDurationMs: 10_000, cpuSamplingIntervalUs: 10_000 });
    expect(resolveProfiling({ enabled: false }, { TROOPER_PROFILING: 'true' })).toBeUndefined();
    expect(
      resolveProfiling(undefined, {
        TROOPER_PROFILING: 'true',
        TROOPER_PROFILING_TYPES: 'wall, inuse_space,bogus',
        TROOPER_PROFILING_PERIOD: '30',
        TROOPER_PROFILING_CPU_DURATION: '45',
      }),
    ).toMatchObject({ types: ['wall', 'inuse_space'], periodMs: 30_000, cpuDurationMs: 30_000 });
    expect(resolveProfiling(undefined, { TROOPER_PROFILING: 'true', AWS_LAMBDA_FUNCTION_NAME: 'fn' })).toBeUndefined();
    expect(resolveProfiling({ periodS: 5 }, { AWS_LAMBDA_FUNCTION_NAME: 'fn' })?.periodMs).toBe(5_000);
    const cfg = resolveConfig({ dsn: 'https://key@ingest.example.com/7', profiling: true }, {});
    expect(cfg.profilesUrl).toBe('https://ingest.example.com/v1/profiles');
    expect(cfg.profiling).toBeDefined();
  });
});

describe('ContinuousProfiler', () => {
  let profiler: ContinuousProfiler | undefined;
  afterEach(async () => {
    await profiler?.stop();
    profiler = undefined;
  });

  it('never throws when the inspector is unavailable', async () => {
    const before = internalErrorCount();
    profiler = new ContinuousProfiler({
      config: { types: ['cpu', 'inuse_space'], periodMs: 1000, cpuDurationMs: 100, cpuSamplingIntervalUs: 1000, heapSamplingIntervalBytes: 1024 },
      uploader: { upload: async () => true },
      service: 's',
      sessionFactory: async () => {
        throw new Error('no inspector');
      },
    });
    await expect(profiler.start()).resolves.toBeUndefined();
    await expect(profiler.stop()).resolves.toBeUndefined();
    expect(internalErrorCount()).toBe(before + 1);
  });

  it('profiles the real process via node:inspector', async () => {
    const uploads: { meta: ProfileMeta; folded: string }[] = [];
    profiler = new ContinuousProfiler({
      config: { types: ['cpu', 'wall', 'inuse_space'], periodMs: 1000, cpuDurationMs: 300, cpuSamplingIntervalUs: 1000, heapSamplingIntervalBytes: 1024 },
      uploader: {
        upload: async (meta, folded) => {
          uploads.push({ meta, folded });
          return true;
        },
      },
      service: 'svc',
      env: 'test',
      host: 'h',
    });
    await profiler.start();
    const keep: unknown[] = [];
    const busyUntil = Date.now() + 250;
    while (Date.now() < busyUntil) keep.push({ v: Math.sqrt(keep.length) * 2, s: 'x'.repeat(64) });
    await waitFor(() => uploads.some((u) => u.meta.type === 'inuse_space'), 4000);

    const cpu = uploads.find((u) => u.meta.type === 'cpu');
    const wall = uploads.find((u) => u.meta.type === 'wall');
    const heap = uploads.find((u) => u.meta.type === 'inuse_space');
    expect(cpu && wall && heap).toBeTruthy();
    expect(cpu!.meta).toMatchObject({ unit: 'nanoseconds', service: 'svc', env: 'test', host: 'h' });
    expect(heap!.meta.unit).toBe('bytes');
    for (const u of [cpu!, wall!, heap!]) {
      for (const line of u.folded.trim().split('\n')) expect(line).toMatch(/^[^;\n]+(;[^;\n]+)* \d+$/);
    }
    expect(cpu!.folded).toContain('test/profiling.test.ts)');
    expect(cpu!.folded).not.toContain('[idle]');
    expect(cpu!.folded).not.toContain('file://');
    expect(keep.length).toBeGreaterThan(0);
  });

  it('flushes the running CPU profile on stop', async () => {
    const uploads: ProfileMeta[] = [];
    profiler = new ContinuousProfiler({
      config: { types: ['cpu'], periodMs: 60_000, cpuDurationMs: 60_000, cpuSamplingIntervalUs: 1000, heapSamplingIntervalBytes: 1024 },
      uploader: {
        upload: async (meta) => {
          uploads.push(meta);
          return true;
        },
      },
      service: 'svc',
    });
    await profiler.start();
    const until = Date.now() + 50;
    while (Date.now() < until) Math.sqrt(Math.random());
    const t0 = Date.now();
    await profiler.stop();
    expect(Date.now() - t0).toBeLessThan(5000);
    expect(uploads.map((u) => u.type)).toEqual(['cpu']);
  });
});

async function waitFor(cond: () => boolean, timeoutMs: number): Promise<void> {
  const end = Date.now() + timeoutMs;
  while (!cond()) {
    if (Date.now() > end) throw new Error('timeout');
    await new Promise((r) => setTimeout(r, 20));
  }
}
