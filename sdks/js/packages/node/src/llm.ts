import { context, SpanKind, SpanStatusCode, trace, type Attributes, type AttributeValue, type Span } from '@opentelemetry/api';
import { ATTR_LLM_TTFT_MS, SCOPE_NAME, VERSION } from './attrs';
import { resolveLlm, type LlmOptions, type ResolvedLlm } from './config';
import { recordError } from './errors';
import { getHandle } from './init';

/**
 * AI / LLM monitoring (ADR-023, .ai/contracts.md §25): OpenAI / Anthropic client wrappers producing OTel GenAI CLIENT
 * spans (`"<operation> <model>"`). The client is wrapped in a Proxy — nothing global is patched, the original client keeps
 * working uninstrumented. Prompt / completion content is recorded only with `captureContent` (default off, PII).
 */

export interface LlmInstrumentOptions extends LlmOptions {
  /**
   * `gen_ai.provider.name`. Default: `anthropic` for `instrumentAnthropic`; for `instrumentOpenAI` derived from the
   * client's `baseURL` (`api.openai.com` → `openai`, `*.openai.azure.com` → `azure.ai.openai`, Groq, DeepSeek, xAI,
   * Mistral, Gemini, Ollama), else `openai`.
   */
  provider?: string;
}

/** Content attributes are capped like the writer does (.ai/contracts.md §25). */
const MAX_CONTENT = 32 * 1024;
const INSTRUMENTED = Symbol.for('trooper.llm.instrumented');

type Obj = Record<string, unknown>;
const isObj = (v: unknown): v is Obj => typeof v === 'object' && v !== null;
const str = (v: unknown): string | undefined => (typeof v === 'string' && v !== '' ? v : undefined);
const num = (v: unknown): number | undefined => (typeof v === 'number' && Number.isFinite(v) ? v : undefined);

function settings(o?: LlmInstrumentOptions): ResolvedLlm {
  let base: ResolvedLlm;
  try {
    base = getHandle()?.config.llm ?? resolveLlm(undefined);
  } catch {
    base = { captureContent: false, injectStreamUsage: false };
  }
  return {
    captureContent: o?.captureContent ?? base.captureContent,
    injectStreamUsage: o?.injectStreamUsage ?? base.injectStreamUsage,
  };
}

function json(v: unknown): string | undefined {
  try {
    const s = JSON.stringify(v);
    if (s === undefined) return undefined;
    return s.length > MAX_CONTENT ? s.slice(0, MAX_CONTENT) + '…[truncated]' : s;
  } catch {
    return undefined;
  }
}

function setIf(attrs: Attributes, key: string, v: AttributeValue | undefined): void {
  if (v === undefined || v === null) return;
  if (Array.isArray(v) && v.length === 0) return;
  attrs[key] = v;
}

// ---------------------------------------------------------------------------------------------------------------------
// call / span lifecycle

class LlmCall {
  readonly span: Span;
  private readonly t0 = performance.now();
  private ttft = false;
  ended = false;

  constructor(name: string, attrs: Attributes) {
    this.span = trace.getTracer(SCOPE_NAME, VERSION).startSpan(name, { kind: SpanKind.CLIENT, attributes: attrs });
  }

  firstToken(): void {
    if (this.ttft || this.ended) return;
    this.ttft = true;
    this.span.setAttribute(ATTR_LLM_TTFT_MS, performance.now() - this.t0);
  }

  set(attrs: Attributes): void {
    if (!this.ended) this.span.setAttributes(attrs);
  }

  end(): void {
    if (this.ended) return;
    this.ended = true;
    this.span.end();
  }

  fail(err: unknown): void {
    if (this.ended) return;
    try {
      const e = err as Obj | undefined;
      const status = num(e?.status);
      if (status !== undefined) this.span.setAttribute('http.response.status_code', status);
      this.span.setAttribute('error.type', errorType(err));
      const ex = err instanceof Error ? err : new Error(String(err));
      this.span.recordException(ex);
      this.span.setStatus({ code: SpanStatusCode.ERROR, message: ex.message });
    } catch (e) {
      recordError('llm', e);
    }
    this.end();
  }
}

/** Low-cardinality error class: provider error type (`rate_limit_error`, `invalid_request_error` …), else the class name. */
export function errorType(err: unknown): string {
  const e = err as Obj | undefined;
  const nested = isObj(e?.error) && isObj((e!.error as Obj).error) ? str(((e!.error as Obj).error as Obj).type) : undefined; // anthropic
  return (
    nested ??
    str(e?.type) ?? // openai
    (isObj(e?.error) ? str((e!.error as Obj).type) : undefined) ??
    str(e?.name === 'Error' ? undefined : e?.name) ??
    str((e as { constructor?: { name?: string } } | undefined)?.constructor?.name) ??
    '_OTHER'
  );
}

/** Per-call adapter of one provider operation. */
interface Recorder {
  operation: string;
  model?: string;
  /** Request attributes (incl. content when captured). */
  attrs: Attributes;
  /** The request body to send (may differ from the user's when `injectStreamUsage`). */
  body: unknown;
  stream: boolean;
  onResponse(resp: unknown, call: LlmCall): void;
  onEvent(ev: unknown, call: LlmCall): void;
  onStreamEnd(call: LlmCall): void;
}

type RecorderFactory = (body: unknown, s: ResolvedLlm) => Recorder;

interface Target {
  provider: (client: object) => string;
}

function startCall(provider: string, rec: Recorder, client: object): LlmCall {
  const attrs: Attributes = {
    'gen_ai.provider.name': provider,
    'gen_ai.operation.name': rec.operation,
    ...server(client),
    ...rec.attrs,
  };
  if (rec.model) attrs['gen_ai.request.model'] = rec.model;
  return new LlmCall(rec.model ? `${rec.operation} ${rec.model}` : rec.operation, attrs);
}

function server(client: object): Attributes {
  try {
    const base = str((client as Obj).baseURL);
    if (!base) return {};
    const u = new URL(base);
    const out: Attributes = { 'server.address': u.hostname };
    const port = u.port ? Number(u.port) : u.protocol === 'https:' ? 443 : 80;
    out['server.port'] = port;
    return out;
  } catch {
    return {};
  }
}

const safely = (fn: () => void): void => {
  try {
    fn();
  } catch (err) {
    recordError('llm', err);
  }
};

/** Wraps a stream-ish async iterable so chunks feed the recorder; the span ends when iteration ends. */
function wrapStream<T extends object>(stream: T, call: LlmCall, rec: Recorder): T {
  const finish = () => {
    safely(() => rec.onStreamEnd(call));
    call.end();
  };
  const iterate = (it: AsyncIterator<unknown>): AsyncIterableIterator<unknown> => ({
    async next(...args: [] | [unknown]) {
      let r: IteratorResult<unknown>;
      try {
        r = await it.next(...args);
      } catch (err) {
        call.fail(err);
        throw err;
      }
      if (r.done) finish();
      else safely(() => rec.onEvent(r.value, call));
      return r;
    },
    async return(value?: unknown) {
      finish();
      return it.return ? it.return(value) : { done: true, value };
    },
    async throw(err?: unknown) {
      call.fail(err);
      if (it.throw) return it.throw(err);
      throw err;
    },
    [Symbol.asyncIterator]() {
      return this;
    },
  });
  const proxy: T = new Proxy(stream, {
    get(target, prop) {
      if (prop === Symbol.asyncIterator) {
        return () => iterate((target as AsyncIterable<unknown>)[Symbol.asyncIterator]());
      }
      const v = Reflect.get(target, prop, target);
      if (typeof v !== 'function') return v;
      // toReadableStream() iterates `this` — run it against the proxy so it is measured too.
      if (prop === 'toReadableStream') return (...a: unknown[]) => (v as (...x: unknown[]) => unknown).apply(proxy, a);
      return (v as (...x: unknown[]) => unknown).bind(target);
    },
  });
  return proxy;
}

function isAsyncIterable(v: unknown): v is AsyncIterable<unknown> {
  return isObj(v) && typeof (v as { [Symbol.asyncIterator]?: unknown })[Symbol.asyncIterator] === 'function';
}

/** Maps a resolved value: streams are wrapped, final responses recorded. Idempotent per value. */
function resultMapper(call: LlmCall, rec: Recorder): (v: unknown) => unknown {
  const seen = new WeakMap<object, unknown>();
  return (v: unknown) => {
    if (isObj(v) && seen.has(v)) return seen.get(v);
    let out = v;
    try {
      if (rec.stream && isAsyncIterable(v)) out = wrapStream(v, call, rec);
      else {
        rec.onResponse(v, call);
        call.end();
      }
    } catch (err) {
      recordError('llm', err);
      call.end();
    }
    if (isObj(v)) seen.set(v, out);
    return out;
  };
}

/**
 * Observes a (possibly SDK-specific) promise without losing its extra methods: `then`/`catch`/`finally` see the mapped
 * value, `withResponse()` maps `data`, `asResponse()` ends the span with the HTTP status. Nothing is awaited eagerly.
 */
function observePromise<P extends PromiseLike<unknown>>(p: P, call: LlmCall, map: (v: unknown) => unknown): P {
  let mapped: Promise<unknown> | undefined;
  const onErr = (err: unknown) => {
    call.fail(err);
    throw err;
  };
  const getMapped = () => (mapped ??= Promise.resolve(p.then(map, onErr)));
  return new Proxy(p as P & object, {
    get(target, prop) {
      if (prop === 'then') return (f?: (v: unknown) => unknown, r?: (e: unknown) => unknown) => getMapped().then(f, r);
      if (prop === 'catch') return (r?: (e: unknown) => unknown) => getMapped().catch(r);
      if (prop === 'finally') return (f?: () => void) => getMapped().finally(f);
      const v = Reflect.get(target, prop, target);
      if (typeof v !== 'function') return v;
      if (prop === 'withResponse') {
        return (...a: unknown[]) =>
          Promise.resolve((v as (...x: unknown[]) => PromiseLike<Obj>).apply(target, a)).then((res) => {
            if (!isObj(res)) return res;
            return { ...res, data: map(res.data) };
          }, onErr);
      }
      if (prop === 'asResponse') {
        return (...a: unknown[]) =>
          Promise.resolve((v as (...x: unknown[]) => PromiseLike<unknown>).apply(target, a)).then((res) => {
            safely(() => {
              const status = num((res as Obj | undefined)?.status);
              if (status !== undefined) call.set({ 'http.response.status_code': status });
            });
            call.end();
            return res;
          }, onErr);
      }
      return (v as (...x: unknown[]) => unknown).bind(target);
    },
  }) as P;
}

/** Wraps `create`-style methods: span around the call, the HTTP client span becomes its child. */
function wrapCreate(fn: (...a: unknown[]) => unknown, self: object, client: object, provider: string, make: RecorderFactory, opts?: LlmInstrumentOptions) {
  return function (this: unknown, body: unknown, ...rest: unknown[]): unknown {
    let rec: Recorder;
    let call: LlmCall;
    try {
      rec = make(body, settings(opts));
      call = startCall(provider, rec, client);
    } catch (err) {
      recordError('llm', err);
      return fn.call(self, body, ...rest);
    }
    let result: unknown;
    try {
      result = context.with(trace.setSpan(context.active(), call.span), () => fn.call(self, rec.body, ...rest));
    } catch (err) {
      call.fail(err);
      throw err;
    }
    try {
      const map = resultMapper(call, rec);
      if (isObj(result) && typeof (result as { then?: unknown }).then === 'function') {
        return observePromise(result as unknown as PromiseLike<unknown>, call, map);
      }
      return map(result);
    } catch (err) {
      recordError('llm', err);
      return result;
    }
  };
}

/** Anthropic `messages.stream()` → `MessageStream` (sync). Listens to `streamEvent` / `end` (never `error`, so the
 * stream's own unhandled-rejection behaviour is unchanged). */
function wrapMessageStream(fn: (...a: unknown[]) => unknown, self: object, client: object, provider: string, make: RecorderFactory, opts?: LlmInstrumentOptions) {
  return function (this: unknown, body: unknown, ...rest: unknown[]): unknown {
    let rec: Recorder;
    let call: LlmCall;
    try {
      rec = make(body, settings(opts));
      rec.stream = true;
      call = startCall(provider, rec, client);
    } catch (err) {
      recordError('llm', err);
      return fn.call(self, body, ...rest);
    }
    let ms: unknown;
    try {
      ms = context.with(trace.setSpan(context.active(), call.span), () => fn.call(self, rec.body, ...rest));
    } catch (err) {
      call.fail(err);
      throw err;
    }
    try {
      const s = ms as { on?: (ev: string, l: (...a: unknown[]) => void) => unknown; aborted?: boolean; errored?: boolean };
      if (typeof s?.on !== 'function') {
        call.end();
        return ms;
      }
      s.on('streamEvent', (ev: unknown) => safely(() => rec.onEvent(ev, call)));
      s.on('end', () => {
        if (call.ended) return;
        if (s.aborted) call.fail(Object.assign(new Error('stream aborted'), { name: 'AbortError' }));
        else if (s.errored) call.fail(Object.assign(new Error('stream errored'), { name: 'stream_error' }));
        else {
          safely(() => rec.onStreamEnd(call));
          call.end();
        }
      });
    } catch (err) {
      recordError('llm', err);
      call.end();
    }
    return ms;
  };
}

type Wrapper = (fn: (...a: unknown[]) => unknown, self: object, client: object, provider: string, opts?: LlmInstrumentOptions) => (...a: unknown[]) => unknown;

/** Proxy that intercepts the dotted method paths of `table` (e.g. `chat.completions.create`). */
function proxyPaths<T extends object>(obj: T, prefix: string, table: Record<string, Wrapper>, client: () => object, provider: () => string, opts?: LlmInstrumentOptions): T {
  const cache = new Map<string, { src: unknown; out: unknown }>();
  const keys = Object.keys(table);
  return new Proxy(obj, {
    get(target, prop) {
      if (prop === INSTRUMENTED && !prefix) return true;
      const v = Reflect.get(target, prop, target);
      if (typeof prop === 'string') {
        try {
          const path = prefix ? `${prefix}.${prop}` : prop;
          const hit = cache.get(path);
          if (hit && hit.src === v) return hit.out;
          let out: unknown;
          const w = table[path];
          if (w && typeof v === 'function') out = w(v as (...a: unknown[]) => unknown, target, client(), provider(), opts);
          else if (isObj(v) && keys.some((k) => k.startsWith(path + '.'))) out = proxyPaths(v, path, table, client, provider, opts);
          if (out !== undefined) {
            cache.set(path, { src: v, out });
            return out;
          }
        } catch (err) {
          recordError('llm', err);
        }
      }
      return typeof v === 'function' ? (v as (...a: unknown[]) => unknown).bind(target) : v;
    },
  });
}

function instrument<T>(client: T, table: Record<string, Wrapper>, target: Target, opts?: LlmInstrumentOptions): T {
  try {
    if (!isObj(client) && typeof client !== 'function') return client;
    if ((client as unknown as { [INSTRUMENTED]?: boolean })[INSTRUMENTED]) return client;
    const c = client as unknown as object;
    return proxyPaths(c, '', table, () => c, () => opts?.provider ?? target.provider(c), opts) as T;
  } catch (err) {
    recordError('llm', err);
    return client;
  }
}

// ---------------------------------------------------------------------------------------------------------------------
// GenAI message JSON (gen_ai.input.messages / output.messages / system_instructions)

type Part = Obj;
interface Message {
  role: string;
  parts: Part[];
  finish_reason?: string;
}

const text = (content: string): Part => ({ type: 'text', content });

function media(modality: string, url: unknown, mime?: unknown): Part {
  const u = str(url);
  if (u && !u.startsWith('data:')) return { type: 'uri', modality, uri: u };
  const p: Part = { type: 'blob', modality };
  if (str(mime)) p.mime_type = mime;
  return p; // inline data is never copied
}

function parseArgs(v: unknown): unknown {
  if (typeof v !== 'string') return v;
  try {
    return JSON.parse(v);
  } catch {
    return v;
  }
}

function setContent(attrs: Attributes, system: Part[], input: Message[]): void {
  if (system.length) setIf(attrs, 'gen_ai.system_instructions', json(system));
  if (input.length) setIf(attrs, 'gen_ai.input.messages', json(input));
}

// --- OpenAI -----------------------------------------------------------------------------------------------------------

function openaiParts(content: unknown): Part[] {
  if (typeof content === 'string') return content ? [text(content)] : [];
  if (!Array.isArray(content)) return [];
  const out: Part[] = [];
  for (const p of content) {
    if (!isObj(p)) continue;
    const t = p.type;
    if (t === 'text' || t === 'input_text' || t === 'output_text') out.push(text(String(p.text ?? '')));
    else if (t === 'refusal') out.push(text(String(p.refusal ?? '')));
    else if (t === 'image_url') out.push(media('image', isObj(p.image_url) ? p.image_url.url : undefined));
    else if (t === 'input_image') out.push(media('image', p.image_url));
    else if (t === 'input_audio') out.push(media('audio', undefined, isObj(p.input_audio) ? `audio/${String(p.input_audio.format)}` : undefined));
    else if (t === 'file' || t === 'input_file') out.push({ type: 'file' });
    else out.push({ type: String(t ?? 'generic') });
  }
  return out;
}

function openaiChatContent(messages: unknown): { system: Part[]; input: Message[] } {
  const system: Part[] = [];
  const input: Message[] = [];
  if (!Array.isArray(messages)) return { system, input };
  for (const m of messages) {
    if (!isObj(m)) continue;
    const role = String(m.role ?? 'user');
    if (role === 'system' || role === 'developer') {
      system.push(...openaiParts(m.content));
      continue;
    }
    const parts = openaiParts(m.content);
    if (role === 'tool') {
      input.push({ role: 'tool', parts: [{ type: 'tool_call_response', id: m.tool_call_id, response: typeof m.content === 'string' ? m.content : parts }] });
      continue;
    }
    if (Array.isArray(m.tool_calls)) {
      for (const tc of m.tool_calls) {
        if (!isObj(tc)) continue;
        const f = isObj(tc.function) ? tc.function : {};
        parts.push({ type: 'tool_call', id: tc.id, name: f.name, arguments: parseArgs(f.arguments) });
      }
    }
    input.push({ role, parts });
  }
  return { system, input };
}

function openaiUsage(u: unknown): Attributes {
  const a: Attributes = {};
  if (!isObj(u)) return a;
  setIf(a, 'gen_ai.usage.input_tokens', num(u.prompt_tokens) ?? num(u.input_tokens));
  setIf(a, 'gen_ai.usage.output_tokens', num(u.completion_tokens) ?? num(u.output_tokens));
  const d = isObj(u.prompt_tokens_details) ? u.prompt_tokens_details : isObj(u.input_tokens_details) ? u.input_tokens_details : undefined;
  setIf(a, 'gen_ai.usage.cache_read.input_tokens', num(d?.cached_tokens));
  return a;
}

function commonRequest(b: Obj, attrs: Attributes): void {
  setIf(attrs, 'gen_ai.request.temperature', num(b.temperature));
  setIf(attrs, 'gen_ai.request.top_p', num(b.top_p));
}

const openaiChat: RecorderFactory = (body, s) => {
  const b: Obj = isObj(body) ? body : {};
  const stream = b.stream === true;
  let sendBody: unknown = body;
  if (stream && s.injectStreamUsage && !(isObj(b.stream_options) && b.stream_options.include_usage)) {
    sendBody = { ...b, stream_options: { ...(isObj(b.stream_options) ? b.stream_options : {}), include_usage: true } };
  }
  const attrs: Attributes = {};
  setIf(attrs, 'gen_ai.request.max_tokens', num(b.max_completion_tokens) ?? num(b.max_tokens));
  commonRequest(b, attrs);
  setIf(attrs, 'gen_ai.request.frequency_penalty', num(b.frequency_penalty));
  setIf(attrs, 'gen_ai.request.presence_penalty', num(b.presence_penalty));
  setIf(attrs, 'gen_ai.request.seed', num(b.seed));
  const n = num(b.n);
  if (n !== undefined && n !== 1) attrs['gen_ai.request.choice.count'] = n;
  if (typeof b.stop === 'string') attrs['gen_ai.request.stop_sequences'] = [b.stop];
  else if (Array.isArray(b.stop)) setIf(attrs, 'gen_ai.request.stop_sequences', b.stop.filter((x): x is string => typeof x === 'string'));
  if (s.captureContent) {
    const c = openaiChatContent(b.messages);
    setContent(attrs, c.system, c.input);
  }

  // stream accumulation
  let id: string | undefined;
  let model: string | undefined;
  let usage: Attributes = {};
  const choices = new Map<number, { text: string; finish?: string; tools: Map<number, { id?: string; name?: string; args: string }> }>();
  const choice = (i: number) => {
    let c = choices.get(i);
    if (!c) choices.set(i, (c = { text: '', tools: new Map() }));
    return c;
  };

  return {
    operation: 'chat',
    model: str(b.model),
    attrs,
    body: sendBody,
    stream,
    onResponse(resp, call) {
      if (!isObj(resp)) return;
      const out: Attributes = { ...openaiUsage(resp.usage) };
      setIf(out, 'gen_ai.response.id', str(resp.id));
      setIf(out, 'gen_ai.response.model', str(resp.model));
      const list = Array.isArray(resp.choices) ? resp.choices.filter(isObj) : [];
      setIf(out, 'gen_ai.response.finish_reasons', list.map((c) => str(c.finish_reason)).filter((x): x is string => !!x));
      if (s.captureContent) {
        const msgs: Message[] = list.map((c) => {
          const m: Obj = isObj(c.message) ? c.message : {};
          const parts = openaiParts(m.content);
          if (str(m.refusal)) parts.push(text(String(m.refusal)));
          if (Array.isArray(m.tool_calls)) {
            for (const tc of m.tool_calls) {
              if (!isObj(tc)) continue;
              const f = isObj(tc.function) ? tc.function : {};
              parts.push({ type: 'tool_call', id: tc.id, name: f.name, arguments: parseArgs(f.arguments) });
            }
          }
          const msg: Message = { role: String(m.role ?? 'assistant'), parts };
          if (str(c.finish_reason)) msg.finish_reason = String(c.finish_reason);
          return msg;
        });
        setIf(out, 'gen_ai.output.messages', json(msgs));
      }
      call.set(out);
    },
    onEvent(ev, call) {
      if (!isObj(ev)) return;
      id ??= str(ev.id);
      model ??= str(ev.model);
      if (isObj(ev.usage)) usage = openaiUsage(ev.usage);
      if (!Array.isArray(ev.choices)) return;
      for (const c of ev.choices) {
        if (!isObj(c)) continue;
        const acc = choice(num(c.index) ?? 0);
        const d: Obj = isObj(c.delta) ? c.delta : {};
        const content = str(d.content) ?? str(d.refusal);
        if (content || (Array.isArray(d.tool_calls) && d.tool_calls.length)) call.firstToken();
        if (s.captureContent) {
          if (content) acc.text += content;
          if (Array.isArray(d.tool_calls)) {
            for (const tc of d.tool_calls) {
              if (!isObj(tc)) continue;
              const k = num(tc.index) ?? 0;
              let t = acc.tools.get(k);
              if (!t) acc.tools.set(k, (t = { args: '' }));
              t.id ??= str(tc.id);
              const f = isObj(tc.function) ? tc.function : {};
              t.name ??= str(f.name);
              if (typeof f.arguments === 'string') t.args += f.arguments;
            }
          }
        }
        if (str(c.finish_reason)) acc.finish = String(c.finish_reason);
      }
    },
    onStreamEnd(call) {
      const out: Attributes = { ...usage };
      setIf(out, 'gen_ai.response.id', id);
      setIf(out, 'gen_ai.response.model', model);
      const sorted = [...choices.entries()].sort((a, z) => a[0] - z[0]).map(([, c]) => c);
      setIf(out, 'gen_ai.response.finish_reasons', sorted.map((c) => c.finish).filter((x): x is string => !!x));
      if (s.captureContent && sorted.length) {
        const msgs: Message[] = sorted.map((c) => {
          const parts: Part[] = c.text ? [text(c.text)] : [];
          for (const [, t] of [...c.tools.entries()].sort((a, z) => a[0] - z[0])) {
            parts.push({ type: 'tool_call', id: t.id, name: t.name, arguments: parseArgs(t.args) });
          }
          const m: Message = { role: 'assistant', parts };
          if (c.finish) m.finish_reason = c.finish;
          return m;
        });
        setIf(out, 'gen_ai.output.messages', json(msgs));
      }
      call.set(out);
    },
  };
};

function responsesItems(input: unknown): Message[] {
  if (typeof input === 'string') return input ? [{ role: 'user', parts: [text(input)] }] : [];
  if (!Array.isArray(input)) return [];
  const out: Message[] = [];
  for (const it of input) {
    if (!isObj(it)) continue;
    if (it.type === 'function_call') out.push({ role: 'assistant', parts: [{ type: 'tool_call', id: it.call_id, name: it.name, arguments: parseArgs(it.arguments) }] });
    else if (it.type === 'function_call_output') out.push({ role: 'tool', parts: [{ type: 'tool_call_response', id: it.call_id, response: it.output }] });
    else if (it.role) out.push({ role: String(it.role), parts: openaiParts(it.content) });
  }
  return out;
}

function responsesOutput(output: unknown, finish?: string): Message[] {
  if (!Array.isArray(output)) return [];
  const parts: Part[] = [];
  for (const it of output) {
    if (!isObj(it)) continue;
    if (it.type === 'message') parts.push(...openaiParts(it.content));
    else if (it.type === 'function_call') parts.push({ type: 'tool_call', id: it.call_id, name: it.name, arguments: parseArgs(it.arguments) });
  }
  if (!parts.length) return [];
  const m: Message = { role: 'assistant', parts };
  if (finish) m.finish_reason = finish;
  return [m];
}

const openaiResponses: RecorderFactory = (body, s) => {
  const b: Obj = isObj(body) ? body : {};
  const attrs: Attributes = {};
  setIf(attrs, 'gen_ai.request.max_tokens', num(b.max_output_tokens));
  commonRequest(b, attrs);
  setIf(attrs, 'gen_ai.conversation.id', str(b.conversation) ?? (isObj(b.conversation) ? str(b.conversation.id) : undefined));
  if (s.captureContent) {
    const input = responsesItems(b.input);
    const system: Part[] = [];
    if (str(b.instructions)) system.push(text(String(b.instructions)));
    for (let i = input.length - 1; i >= 0; i--) {
      const r = input[i]!.role;
      if (r === 'system' || r === 'developer') system.unshift(...input.splice(i, 1)[0]!.parts);
    }
    setContent(attrs, system, input);
  }
  let done = false;
  const final = (resp: unknown, call: LlmCall) => {
    if (!isObj(resp)) return;
    const out: Attributes = { ...openaiUsage(resp.usage) };
    setIf(out, 'gen_ai.response.id', str(resp.id));
    setIf(out, 'gen_ai.response.model', str(resp.model));
    const finish = (isObj(resp.incomplete_details) ? str(resp.incomplete_details.reason) : undefined) ?? str(resp.status);
    if (finish) out['gen_ai.response.finish_reasons'] = [finish];
    if (s.captureContent) setIf(out, 'gen_ai.output.messages', json(responsesOutput(resp.output, finish)));
    call.set(out);
    if (resp.status === 'failed') {
      const e = isObj(resp.error) ? resp.error : {};
      call.fail(Object.assign(new Error(str(e.message) ?? 'response failed'), { type: str(e.code) ?? 'response_failed' }));
    }
    done = true;
  };
  return {
    operation: 'chat',
    model: str(b.model),
    attrs,
    body,
    stream: b.stream === true,
    onResponse: final,
    onEvent(ev, call) {
      if (!isObj(ev)) return;
      const t = str(ev.type) ?? '';
      if (t.endsWith('.delta') && t !== 'response.audio.transcript.delta') call.firstToken();
      if (t === 'response.created' && isObj(ev.response)) {
        call.set({
          ...(str(ev.response.id) ? { 'gen_ai.response.id': String(ev.response.id) } : {}),
          ...(str(ev.response.model) ? { 'gen_ai.response.model': String(ev.response.model) } : {}),
        });
      }
      if (t === 'response.completed' || t === 'response.incomplete' || t === 'response.failed') final(ev.response, call);
      if (t === 'error' && !done) call.fail(Object.assign(new Error(str(ev.message) ?? 'stream error'), { type: str(ev.code) ?? 'stream_error' }));
    },
    onStreamEnd() {},
  };
};

const openaiEmbeddings: RecorderFactory = (body) => {
  const b: Obj = isObj(body) ? body : {};
  const attrs: Attributes = {};
  if (str(b.encoding_format)) attrs['gen_ai.request.encoding_formats'] = [String(b.encoding_format)];
  setIf(attrs, 'gen_ai.embeddings.dimension.count', num(b.dimensions));
  return {
    operation: 'embeddings',
    model: str(b.model),
    attrs,
    body,
    stream: false,
    onResponse(resp, call) {
      if (!isObj(resp)) return;
      const out: Attributes = {};
      setIf(out, 'gen_ai.response.model', str(resp.model));
      if (isObj(resp.usage)) setIf(out, 'gen_ai.usage.input_tokens', num(resp.usage.prompt_tokens) ?? num(resp.usage.total_tokens));
      call.set(out);
    },
    onEvent() {},
    onStreamEnd() {},
  };
};

const OPENAI_HOSTS: [RegExp, string][] = [
  [/(^|\.)openai\.azure\.com$/, 'azure.ai.openai'],
  [/(^|\.)services\.ai\.azure\.com$/, 'azure.ai.openai'],
  [/(^|\.)groq\.com$/, 'groq'],
  [/(^|\.)deepseek\.com$/, 'deepseek'],
  [/(^|\.)x\.ai$/, 'x_ai'],
  [/(^|\.)mistral\.ai$/, 'mistral_ai'],
  [/(^|\.)generativelanguage\.googleapis\.com$/, 'gcp.gemini'],
  [/(^|\.)aiplatform\.googleapis\.com$/, 'gcp.vertex_ai'],
  [/(^|\.)cohere\.(ai|com)$/, 'cohere'],
  [/(^|\.)perplexity\.ai$/, 'perplexity'],
];

/** `gen_ai.provider.name` of an OpenAI-compatible client from its `baseURL`. */
export function openaiProvider(client: object): string {
  try {
    const base = str((client as Obj).baseURL);
    if (!base) return 'openai';
    const u = new URL(base);
    for (const [re, name] of OPENAI_HOSTS) if (re.test(u.hostname)) return name;
    if (u.port === '11434') return 'ollama';
  } catch {
    /* ignore */
  }
  return 'openai';
}

// --- Anthropic --------------------------------------------------------------------------------------------------------

function anthropicParts(content: unknown): Part[] {
  if (typeof content === 'string') return content ? [text(content)] : [];
  if (!Array.isArray(content)) return [];
  const out: Part[] = [];
  for (const p of content) {
    if (!isObj(p)) continue;
    switch (p.type) {
      case 'text':
        out.push(text(String(p.text ?? '')));
        break;
      case 'thinking':
        out.push({ type: 'reasoning', content: String(p.thinking ?? '') });
        break;
      case 'redacted_thinking':
        out.push({ type: 'reasoning' });
        break;
      case 'tool_use':
      case 'server_tool_use':
        out.push({ type: 'tool_call', id: p.id, name: p.name, arguments: p.input });
        break;
      case 'tool_result':
        out.push({ type: 'tool_call_response', id: p.tool_use_id, response: typeof p.content === 'string' ? p.content : anthropicParts(p.content) });
        break;
      case 'image':
      case 'document': {
        const src: Obj = isObj(p.source) ? p.source : {};
        out.push(media(p.type === 'image' ? 'image' : 'document', src.type === 'url' ? src.url : undefined, src.media_type));
        break;
      }
      default:
        out.push({ type: String(p.type ?? 'generic') });
    }
  }
  return out;
}

/** Anthropic `input_tokens` excludes cache reads/writes; `gen_ai.usage.input_tokens` is the total (OTel GenAI semconv). */
function anthropicUsage(u: unknown, prev: { input?: number; output?: number; read?: number; write?: number } = {}) {
  const r = { ...prev };
  if (!isObj(u)) return r;
  r.input = num(u.input_tokens) ?? r.input;
  r.output = num(u.output_tokens) ?? r.output;
  r.read = num(u.cache_read_input_tokens) ?? r.read;
  r.write = num(u.cache_creation_input_tokens) ?? r.write;
  return r;
}

function anthropicUsageAttrs(u: { input?: number; output?: number; read?: number; write?: number }): Attributes {
  const a: Attributes = {};
  if (u.input !== undefined || u.read !== undefined || u.write !== undefined) {
    a['gen_ai.usage.input_tokens'] = (u.input ?? 0) + (u.read ?? 0) + (u.write ?? 0);
  }
  setIf(a, 'gen_ai.usage.output_tokens', u.output);
  setIf(a, 'gen_ai.usage.cache_read.input_tokens', u.read);
  setIf(a, 'gen_ai.usage.cache_creation.input_tokens', u.write);
  return a;
}

const anthropicMessages: RecorderFactory = (body, s) => {
  const b: Obj = isObj(body) ? body : {};
  const attrs: Attributes = {};
  setIf(attrs, 'gen_ai.request.max_tokens', num(b.max_tokens));
  commonRequest(b, attrs);
  setIf(attrs, 'gen_ai.request.top_k', num(b.top_k));
  if (Array.isArray(b.stop_sequences)) setIf(attrs, 'gen_ai.request.stop_sequences', b.stop_sequences.filter((x): x is string => typeof x === 'string'));
  if (s.captureContent) {
    const input: Message[] = Array.isArray(b.messages)
      ? b.messages.filter(isObj).map((m) => {
          const parts = anthropicParts(m.content);
          const role = parts.length && parts.every((p) => p.type === 'tool_call_response') ? 'tool' : String(m.role ?? 'user');
          return { role, parts };
        })
      : [];
    setContent(attrs, anthropicParts(b.system), input);
  }

  let id: string | undefined;
  let model: string | undefined;
  let stop: string | undefined;
  let usage: ReturnType<typeof anthropicUsage> = {};
  const blocks = new Map<number, Obj & { _json?: string }>();

  const output = (content: unknown, finish?: string): Message[] => {
    const parts = anthropicParts(content);
    if (!parts.length) return [];
    const m: Message = { role: 'assistant', parts };
    if (finish) m.finish_reason = finish;
    return [m];
  };

  return {
    operation: 'chat',
    model: str(b.model),
    attrs,
    body,
    stream: b.stream === true,
    onResponse(resp, call) {
      if (!isObj(resp)) return;
      const out: Attributes = anthropicUsageAttrs(anthropicUsage(resp.usage));
      setIf(out, 'gen_ai.response.id', str(resp.id));
      setIf(out, 'gen_ai.response.model', str(resp.model));
      if (str(resp.stop_reason)) out['gen_ai.response.finish_reasons'] = [String(resp.stop_reason)];
      if (s.captureContent) setIf(out, 'gen_ai.output.messages', json(output(resp.content, str(resp.stop_reason))));
      call.set(out);
    },
    onEvent(ev, call) {
      if (!isObj(ev)) return;
      switch (ev.type) {
        case 'message_start': {
          const m: Obj = isObj(ev.message) ? ev.message : {};
          id ??= str(m.id);
          model ??= str(m.model);
          usage = anthropicUsage(m.usage, usage);
          break;
        }
        case 'content_block_start': {
          const blk: Obj = isObj(ev.content_block) ? { ...ev.content_block } : {};
          if (str(blk.text)) call.firstToken();
          if (s.captureContent) blocks.set(num(ev.index) ?? blocks.size, blk);
          break;
        }
        case 'content_block_delta': {
          const d: Obj = isObj(ev.delta) ? ev.delta : {};
          call.firstToken();
          if (!s.captureContent) break;
          const blk = blocks.get(num(ev.index) ?? 0);
          if (!blk) break;
          if (d.type === 'text_delta') blk.text = String(blk.text ?? '') + String(d.text ?? '');
          else if (d.type === 'thinking_delta') blk.thinking = String(blk.thinking ?? '') + String(d.thinking ?? '');
          else if (d.type === 'input_json_delta') blk._json = (blk._json ?? '') + String(d.partial_json ?? '');
          break;
        }
        case 'message_delta': {
          const d: Obj = isObj(ev.delta) ? ev.delta : {};
          stop = str(d.stop_reason) ?? stop;
          usage = anthropicUsage(ev.usage, usage);
          break;
        }
      }
    },
    onStreamEnd(call) {
      const out: Attributes = anthropicUsageAttrs(usage);
      setIf(out, 'gen_ai.response.id', id);
      setIf(out, 'gen_ai.response.model', model);
      if (stop) out['gen_ai.response.finish_reasons'] = [stop];
      if (s.captureContent && blocks.size) {
        const content = [...blocks.entries()]
          .sort((a, z) => a[0] - z[0])
          .map(([, blk]) => {
            const { _json, ...rest } = blk;
            if (_json !== undefined) rest.input = parseArgs(_json);
            return rest;
          });
        setIf(out, 'gen_ai.output.messages', json(output(content, stop)));
      }
      call.set(out);
    },
  };
};

// ---------------------------------------------------------------------------------------------------------------------
// public API

const create =
  (make: RecorderFactory): Wrapper =>
  (fn, self, client, provider, opts) =>
    wrapCreate(fn, self, client, provider, make, opts);

const OPENAI_TABLE: Record<string, Wrapper> = {
  'chat.completions.create': create(openaiChat),
  'chat.completions.parse': create(openaiChat),
  'responses.create': create(openaiResponses),
  'responses.parse': create(openaiResponses),
  'embeddings.create': create(openaiEmbeddings),
};

const messageStream: Wrapper = (fn, self, client, provider, opts) => wrapMessageStream(fn, self, client, provider, anthropicMessages, opts);

const ANTHROPIC_TABLE: Record<string, Wrapper> = {
  'messages.create': create(anthropicMessages),
  'messages.stream': messageStream,
  'beta.messages.create': create(anthropicMessages),
  'beta.messages.stream': messageStream,
};

/**
 * Instruments an `openai` client (`new OpenAI()`, also `AzureOpenAI` and OpenAI-compatible endpoints):
 * `chat.completions.create` / `.parse`, `responses.create` / `.parse`, `embeddings.create` — streaming included
 * (TTFT at the first delta; chat stream token usage needs `stream_options.include_usage`, see `injectStreamUsage`).
 * Returns a proxy; the passed client is not modified. Never throws.
 */
export function instrumentOpenAI<T>(client: T, options?: LlmInstrumentOptions): T {
  return instrument(client, OPENAI_TABLE, { provider: openaiProvider }, options);
}

/**
 * Instruments an `@anthropic-ai/sdk` client: `messages.create` (incl. `stream: true`) and `messages.stream()`
 * (+ `beta.messages.*`). Returns a proxy; the passed client is not modified. Never throws.
 */
export function instrumentAnthropic<T>(client: T, options?: LlmInstrumentOptions): T {
  return instrument(client, ANTHROPIC_TABLE, { provider: () => 'anthropic' }, options);
}
