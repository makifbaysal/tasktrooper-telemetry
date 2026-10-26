import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { describe, expect, it } from 'vitest';
import { ENUMS, FIELDS } from '../src/schema';
import { Client } from '../src/client';
import type { Transport } from '../src/transport';

const PROTO = fileURLToPath(new URL('../../../../../proto/rum/v1/rum.proto', import.meta.url));

/** Tiny .proto parser: message → field names (incl. oneof members), enum → value names. */
function parseProto(src: string) {
  const text = src.replace(/\/\/.*$/gm, '');
  const messages: Record<string, string[]> = {};
  const enums: Record<string, string[]> = {};
  const blockRe = /\b(message|enum)\s+(\w+)\s*\{/g;
  let m: RegExpExecArray | null;
  while ((m = blockRe.exec(text))) {
    let depth = 1;
    let i = blockRe.lastIndex;
    while (depth && i < text.length) {
      if (text[i] === '{') depth++;
      else if (text[i] === '}') depth--;
      i++;
    }
    const body = text.slice(blockRe.lastIndex, i - 1);
    if (m[1] === 'message') {
      messages[m[2]!] = [...body.matchAll(/^\s*(?:repeated\s+)?(?:map<[^>]+>|[\w.]+)\s+(\w+)\s*=\s*\d+\s*;/gm)].map((x) => x[1]!);
    } else {
      enums[m[2]!] = [...body.matchAll(/^\s*(\w+)\s*=\s*\d+\s*;/gm)].map((x) => x[1]!);
    }
  }
  return { messages, enums };
}

const proto = parseProto(readFileSync(PROTO, 'utf8'));

describe('rum.proto ↔ TypeScript types', () => {
  it('covers every message', () => {
    expect(Object.keys(FIELDS).sort()).toEqual(Object.keys(proto.messages).sort());
  });
  for (const [name, fields] of Object.entries(proto.messages)) {
    it(`message ${name} has identical field names`, () => {
      expect([...(FIELDS[name] ?? [])].sort()).toEqual([...fields].sort());
    });
  }
  for (const [name, values] of Object.entries(proto.enums)) {
    it(`enum ${name} has identical values`, () => {
      expect(ENUMS[name]).toEqual(values);
    });
  }

  it('serialized payload only uses proto field names', async () => {
    const bodies: string[] = [];
    const transport: Transport = { send: async (b) => (bodies.push(b), { status: 'ok' }) };
    const c = new Client({
      dsn: 'http://k@localhost:4318/1', app: 'a', version: '1', environment: 'dev', platform: 'PLATFORM_WEB',
      sdk: { name: 't', version: '0' }, device: { type: 'desktop', screen_width: 1 }, os: { name: 'macOS' },
      browser: { name: 'Chrome' }, transport, getConnection: () => '4g', attributes: { a: 'b' },
    });
    c.setUser('u1', { plan: 'pro' });
    c.view = { id: 'v', name: '/x' };
    c.capture({ type: 'VIEW_START', view: { url: 'https://x/y?q=1', referrer: '', loading_time_ms: 1, is_initial: true } });
    c.capture({ type: 'VITAL', vital: { name: 'LCP', value: 1, rating: 'good' } });
    c.capture({ type: 'ERROR', error: { type: 'E', message: 'm', stack: 's', source: 'source', is_crash: true } });
    c.capture({ type: 'RESOURCE', resource: { url: 'u', method: 'GET', status_code: 200, duration_ms: 1, request_bytes: 1, response_bytes: 1, trace_id: 't', span_id: 's', initiator: 'fetch' } });
    c.capture({ type: 'ACTION', action: { name: 'n', target: 't', rage: false, dead: false } });
    c.capture({ type: 'APP_START', app_start: { kind: 'cold', duration_ms: 1 } });
    c.capture({ type: 'CUSTOM', custom: { name: 'n', props: { a: '1' }, value: 2 }, attributes: { x: 'y' } });
    await c.flush();
    const batch = JSON.parse(bodies[0]!);
    const typeOf: Record<string, string> = {
      resource: 'Resource', events: 'RumEvent', sdk: 'Sdk', device: 'Device', os: 'Os', browser: 'Browser', user: 'User',
      view: 'ViewPayload', vital: 'VitalPayload', error: 'ErrorPayload', action: 'ActionPayload', app_start: 'AppStartPayload', custom: 'CustomPayload',
    };
    const check = (obj: Record<string, unknown>, msg: string) => {
      for (const [k, v] of Object.entries(obj)) {
        expect(proto.messages[msg], `${msg}.${k}`).toContain(k);
        if (k === 'attributes' || k === 'traits' || k === 'props') continue;
        const sub = msg === 'RumEvent' && k === 'resource' ? 'ResourcePayload' : typeOf[k];
        if (Array.isArray(v)) v.forEach((x) => check(x, sub!));
        else if (v && typeof v === 'object') check(v as Record<string, unknown>, sub!);
      }
    };
    check(batch, 'RumBatch');
    expect(batch.resource.platform).toBe('PLATFORM_WEB');
    expect(batch.events.map((e: { type: string }) => e.type)).toEqual([
      'SESSION_START', 'VIEW_START', 'VITAL', 'ERROR', 'RESOURCE', 'ACTION', 'APP_START', 'CUSTOM',
    ]);
    for (const e of batch.events) expect(proto.enums.EventType).toContain(e.type);
  });
});
