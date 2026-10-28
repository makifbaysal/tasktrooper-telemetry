import { createServer, type IncomingHttpHeaders, type Server } from 'node:http';
import { gunzipSync } from 'node:zlib';

/* Minimal OTLP/protobuf trace decoder (only the fields the e2e test asserts) + a fake ingest gateway. */

type Field = { no: number; wire: number; bytes?: Uint8Array; num?: bigint };

function varint(buf: Uint8Array, pos: number): [bigint, number] {
  let result = 0n;
  let shift = 0n;
  for (;;) {
    const b = buf[pos++]!;
    result |= BigInt(b & 0x7f) << shift;
    if ((b & 0x80) === 0) return [result, pos];
    shift += 7n;
  }
}

function fields(buf: Uint8Array): Field[] {
  const out: Field[] = [];
  let pos = 0;
  while (pos < buf.length) {
    const [tag, p] = varint(buf, pos);
    pos = p;
    const no = Number(tag >> 3n);
    const wire = Number(tag & 7n);
    if (wire === 0) {
      const [v, p2] = varint(buf, pos);
      out.push({ no, wire, num: v });
      pos = p2;
    } else if (wire === 1) {
      out.push({ no, wire, bytes: buf.subarray(pos, pos + 8) });
      pos += 8;
    } else if (wire === 2) {
      const [len, p2] = varint(buf, pos);
      out.push({ no, wire, bytes: buf.subarray(p2, p2 + Number(len)) });
      pos = p2 + Number(len);
    } else if (wire === 5) {
      out.push({ no, wire, bytes: buf.subarray(pos, pos + 4) });
      pos += 4;
    } else throw new Error(`unsupported wire type ${wire}`);
  }
  return out;
}

const text = (b?: Uint8Array) => new TextDecoder().decode(b ?? new Uint8Array());
const hex = (b?: Uint8Array) => Buffer.from(b ?? []).toString('hex');

export type AttrValue = string | number | boolean | undefined;

function anyValue(buf: Uint8Array): AttrValue {
  for (const f of fields(buf)) {
    if (f.no === 1) return text(f.bytes);
    if (f.no === 2) return f.num === 1n;
    if (f.no === 3) return Number(BigInt.asIntN(64, f.num!));
    if (f.no === 4) return Buffer.from(f.bytes!).readDoubleLE(0);
  }
  return undefined;
}

function attributes(fs: Field[], no: number): Record<string, AttrValue> {
  const out: Record<string, AttrValue> = {};
  for (const f of fs.filter((x) => x.no === no)) {
    const kv = fields(f.bytes!);
    const key = text(kv.find((x) => x.no === 1)?.bytes);
    const v = kv.find((x) => x.no === 2);
    out[key] = v ? anyValue(v.bytes!) : undefined;
  }
  return out;
}

export interface DecodedSpan {
  traceId: string;
  spanId: string;
  parentSpanId: string;
  traceState: string;
  name: string;
  kind: number;
  attributes: Record<string, AttrValue>;
  resource: Record<string, AttrValue>;
  scope: string;
}

/** Decodes an ExportTraceServiceRequest. */
export function decodeTraces(body: Uint8Array): DecodedSpan[] {
  const spans: DecodedSpan[] = [];
  for (const rs of fields(body).filter((f) => f.no === 1)) {
    const rsf = fields(rs.bytes!);
    const resBytes = rsf.find((f) => f.no === 1)?.bytes;
    const resource = resBytes ? attributes(fields(resBytes), 1) : {};
    for (const ss of rsf.filter((f) => f.no === 2)) {
      const ssf = fields(ss.bytes!);
      const scopeBytes = ssf.find((f) => f.no === 1)?.bytes;
      const scope = scopeBytes ? text(fields(scopeBytes).find((f) => f.no === 1)?.bytes) : '';
      for (const sp of ssf.filter((f) => f.no === 2)) {
        const s = fields(sp.bytes!);
        spans.push({
          traceId: hex(s.find((f) => f.no === 1)?.bytes),
          spanId: hex(s.find((f) => f.no === 2)?.bytes),
          traceState: text(s.find((f) => f.no === 3)?.bytes),
          parentSpanId: hex(s.find((f) => f.no === 4)?.bytes),
          name: text(s.find((f) => f.no === 5)?.bytes),
          kind: Number(s.find((f) => f.no === 6)?.num ?? 0n),
          attributes: attributes(s, 9),
          resource,
          scope,
        });
      }
    }
  }
  return spans;
}

export interface Received {
  path: string;
  headers: IncomingHttpHeaders;
  body: Uint8Array;
}

/** Fake ingest-gw: records every POST and answers 200 with an empty protobuf body. */
export async function startFakeIngest(): Promise<{ port: number; received: Received[]; close: () => Promise<void>; server: Server }> {
  const received: Received[] = [];
  const server = createServer((req, res) => {
    const chunks: Buffer[] = [];
    req.on('data', (c: Buffer) => chunks.push(c));
    req.on('end', () => {
      let body: Uint8Array = Buffer.concat(chunks);
      if (req.headers['content-encoding'] === 'gzip') body = gunzipSync(body);
      received.push({ path: req.url ?? '', headers: req.headers, body });
      res.writeHead(200, { 'content-type': 'application/x-protobuf' });
      res.end();
    });
  });
  await new Promise<void>((r) => server.listen(0, '127.0.0.1', r));
  const port = (server.address() as { port: number }).port;
  return { port, received, server, close: () => new Promise((r) => server.close(() => r())) };
}
