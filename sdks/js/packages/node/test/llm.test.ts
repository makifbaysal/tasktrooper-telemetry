import { SpanKind, SpanStatusCode, trace } from '@opentelemetry/api';
import type { ReadableSpan } from '@opentelemetry/sdk-trace';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { normalizeGenAiSpan, vercelProvider } from '../src/genai';
import { instrumentAnthropic, instrumentOpenAI, openaiProvider } from '../src/llm';
import { setupTracing } from './helpers';

// ---------------------------------------------------------------------------------------------------------------------
// handwritten fakes shaped like the official SDKs (private #fields, APIPromise with withResponse, Stream, MessageStream)

class FakeAPIPromise<T> implements PromiseLike<T> {
  #p: Promise<{ data: T; response: { status: number } }>;
  constructor(fn: () => Promise<T>) {
    this.#p = Promise.resolve().then(fn).then((data) => ({ data, response: { status: 200 } }));
  }
  then<A = T, B = never>(f?: ((v: T) => A | PromiseLike<A>) | null, r?: ((e: unknown) => B | PromiseLike<B>) | null): Promise<A | B> {
    return this.#p.then((x) => x.data).then(f, r);
  }
  withResponse() {
    return this.#p;
  }
  asResponse() {
    return this.#p.then((x) => x.response);
  }
}

class FakeStream<T> {
  #items: T[];
  controller = new AbortController();
  constructor(items: T[]) {
    this.#items = items;
  }
  async *[Symbol.asyncIterator]() {
    for (const x of this.#items) {
      await new Promise((r) => setTimeout(r, 2));
      yield x;
    }
  }
}

interface Seen {
  body: unknown;
  activeSpan: string | undefined;
}

function activeName(): string | undefined {
  return (trace.getActiveSpan() as unknown as ReadableSpan | undefined)?.name;
}

class Resource {
  #client: { seen: Seen[] };
  #reply: (body: Record<string, unknown>) => unknown;
  constructor(client: { seen: Seen[] }, reply: (body: Record<string, unknown>) => unknown) {
    this.#client = client;
    this.#reply = reply;
  }
  create(body: Record<string, unknown>) {
    this.#client.seen.push({ body, activeSpan: activeName() });
    return new FakeAPIPromise(async () => {
      const r = this.#reply(body);
      if (r instanceof Error) throw r;
      return r;
    });
  }
}

const CHAT_RESPONSE = {
  id: 'chatcmpl-1',
  model: 'gpt-4o-2024-08-06',
  choices: [{ index: 0, finish_reason: 'stop', message: { role: 'assistant', content: 'Hi there' } }],
  usage: { prompt_tokens: 12, completion_tokens: 3, total_tokens: 15, prompt_tokens_details: { cached_tokens: 4 } },
};

const CHAT_CHUNKS = (withUsage: boolean) => [
  { id: 'chatcmpl-2', model: 'gpt-4o-mini', choices: [{ index: 0, delta: { role: 'assistant', content: '' } }] },
  { id: 'chatcmpl-2', model: 'gpt-4o-mini', choices: [{ index: 0, delta: { content: 'Hel' } }] },
  { id: 'chatcmpl-2', model: 'gpt-4o-mini', choices: [{ index: 0, delta: { content: 'lo' } }] },
  {
    id: 'chatcmpl-2',
    model: 'gpt-4o-mini',
    choices: [{ index: 0, delta: { tool_calls: [{ index: 0, id: 'call_1', function: { name: 'lookup', arguments: '{"q":' } }] } }],
  },
  { id: 'chatcmpl-2', model: 'gpt-4o-mini', choices: [{ index: 0, delta: { tool_calls: [{ index: 0, function: { arguments: '"x"}' } }] }, finish_reason: 'tool_calls' }] },
  ...(withUsage ? [{ id: 'chatcmpl-2', model: 'gpt-4o-mini', choices: [], usage: { prompt_tokens: 20, completion_tokens: 7 } }] : []),
];

class FakeOpenAI {
  baseURL = 'https://api.openai.com/v1';
  seen: Seen[] = [];
  chat = {
    completions: new Resource(this, (b) => {
      if (b.model === 'boom') return Object.assign(new Error('Rate limit'), { status: 429, type: 'requests', code: 'rate_limit_exceeded' });
      if (b.stream) return new FakeStream(CHAT_CHUNKS(!!(b.stream_options as { include_usage?: boolean } | undefined)?.include_usage));
      return CHAT_RESPONSE;
    }),
  };
  responses = new Resource(this, (b) =>
    b.stream
      ? new FakeStream([
          { type: 'response.created', response: { id: 'resp_1', model: 'gpt-5' } },
          { type: 'response.output_text.delta', delta: 'Yo' },
          {
            type: 'response.completed',
            response: {
              id: 'resp_1',
              model: 'gpt-5',
              status: 'completed',
              output: [{ type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'Yo' }] }],
              usage: { input_tokens: 9, output_tokens: 1, input_tokens_details: { cached_tokens: 0 } },
            },
          },
        ])
      : {
          id: 'resp_2',
          model: 'gpt-5',
          status: 'incomplete',
          incomplete_details: { reason: 'max_output_tokens' },
          output: [
            { type: 'function_call', call_id: 'c1', name: 'f', arguments: '{"a":1}' },
            { type: 'message', role: 'assistant', content: [{ type: 'output_text', text: 'partial' }] },
          ],
          usage: { input_tokens: 30, output_tokens: 16 },
        },
  );
  embeddings = new Resource(this, () => ({ model: 'text-embedding-3-small', data: [{ embedding: [0.1] }], usage: { prompt_tokens: 5, total_tokens: 5 } }));
  ping() {
    return this.baseURL;
  }
}

const ANTHROPIC_MESSAGE = {
  id: 'msg_1',
  model: 'claude-sonnet-4-5-20250929',
  role: 'assistant',
  stop_reason: 'end_turn',
  content: [{ type: 'text', text: 'Bonjour' }],
  usage: { input_tokens: 10, output_tokens: 5, cache_read_input_tokens: 100, cache_creation_input_tokens: 20 },
};

const ANTHROPIC_EVENTS = [
  { type: 'message_start', message: { id: 'msg_2', model: 'claude-haiku-4-5', usage: { input_tokens: 8, output_tokens: 1, cache_read_input_tokens: 2 } } },
  { type: 'content_block_start', index: 0, content_block: { type: 'text', text: '' } },
  { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'Sa' } },
  { type: 'content_block_delta', index: 0, delta: { type: 'text_delta', text: 'lut' } },
  { type: 'content_block_stop', index: 0 },
  { type: 'content_block_start', index: 1, content_block: { type: 'tool_use', id: 'tu_1', name: 'get', input: {} } },
  { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: '{"k":' } },
  { type: 'content_block_delta', index: 1, delta: { type: 'input_json_delta', partial_json: '2}' } },
  { type: 'message_delta', delta: { stop_reason: 'tool_use' }, usage: { output_tokens: 11 } },
  { type: 'message_stop' },
];

class FakeMessageStream {
  #listeners = new Map<string, ((...a: unknown[]) => void)[]>();
  #aborted = false;
  constructor(events: unknown[], abort = false) {
    setTimeout(async () => {
      for (const ev of events) {
        await new Promise((r) => setTimeout(r, 2));
        this.#emit('streamEvent', ev, {});
      }
      if (abort) this.#aborted = true;
      this.#emit('end');
    }, 0);
  }
  on(ev: string, l: (...a: unknown[]) => void) {
    this.#listeners.set(ev, [...(this.#listeners.get(ev) ?? []), l]);
    return this;
  }
  get aborted() {
    return this.#aborted;
  }
  get errored() {
    return false;
  }
  #emit(ev: string, ...a: unknown[]) {
    for (const l of this.#listeners.get(ev) ?? []) l(...a);
  }
}

class FakeMessages {
  #client: FakeAnthropic;
  constructor(c: FakeAnthropic) {
    this.#client = c;
  }
  create(body: Record<string, unknown>) {
    this.#client.seen.push({ body, activeSpan: activeName() });
    return new FakeAPIPromise(async () => {
      if (body.model === 'overloaded') {
        throw Object.assign(new Error('Overloaded'), { status: 529, error: { type: 'error', error: { type: 'overloaded_error', message: 'Overloaded' } } });
      }
      return body.stream ? new FakeStream(ANTHROPIC_EVENTS) : ANTHROPIC_MESSAGE;
    });
  }
  stream(body: Record<string, unknown>) {
    this.#client.seen.push({ body, activeSpan: activeName() });
    return new FakeMessageStream(ANTHROPIC_EVENTS, body.model === 'abort');
  }
}

class FakeAnthropic {
  baseURL = 'https://api.anthropic.com';
  seen: Seen[] = [];
  messages = new FakeMessages(this);
}

// ---------------------------------------------------------------------------------------------------------------------

let t: ReturnType<typeof setupTracing>;
beforeEach(() => {
  t = setupTracing();
});
afterEach(async () => {
  await t.teardown();
  delete process.env.TROOPER_LLM_CAPTURE_CONTENT;
});

const only = (): ReadableSpan => {
  const spans = t.spans();
  expect(spans).toHaveLength(1);
  return spans[0]!;
};

const SYSTEM = { role: 'system', content: 'You are terse.' };
const USER = { role: 'user', content: 'Hello' };

describe('instrumentOpenAI', () => {
  it('chat.completions.create: GenAI span with usage, params, parenting; no content by default', async () => {
    const raw = new FakeOpenAI();
    const client = instrumentOpenAI(raw);
    expect(instrumentOpenAI(client)).toBe(client);
    const res = await client.chat.completions.create({ model: 'gpt-4o', messages: [SYSTEM, USER], max_tokens: 50, temperature: 0.2, stop: 'END' });
    expect(res).toBe(CHAT_RESPONSE);
    const s = only();
    expect(s.name).toBe('chat gpt-4o');
    expect(s.kind).toBe(SpanKind.CLIENT);
    expect(s.attributes).toMatchObject({
      'gen_ai.provider.name': 'openai',
      'gen_ai.operation.name': 'chat',
      'gen_ai.request.model': 'gpt-4o',
      'gen_ai.response.model': 'gpt-4o-2024-08-06',
      'gen_ai.response.id': 'chatcmpl-1',
      'gen_ai.usage.input_tokens': 12,
      'gen_ai.usage.output_tokens': 3,
      'gen_ai.usage.cache_read.input_tokens': 4,
      'gen_ai.request.max_tokens': 50,
      'gen_ai.request.temperature': 0.2,
      'gen_ai.request.stop_sequences': ['END'],
      'gen_ai.response.finish_reasons': ['stop'],
      'server.address': 'api.openai.com',
      'server.port': 443,
    });
    expect(s.attributes['gen_ai.input.messages']).toBeUndefined();
    expect(s.attributes['gen_ai.output.messages']).toBeUndefined();
    expect(s.attributes['trooper.llm.ttft_ms']).toBeUndefined();
    expect(raw.seen[0]!.activeSpan).toBe('chat gpt-4o'); // the HTTP span would be its child
    expect(client.ping()).toBe('https://api.openai.com/v1'); // other members work (private-field safe binding)
  });

  it('captures content when enabled', async () => {
    const client = instrumentOpenAI(new FakeOpenAI(), { captureContent: true });
    await client.chat.completions.create({
      model: 'gpt-4o',
      messages: [
        SYSTEM,
        USER,
        { role: 'assistant', content: null, tool_calls: [{ id: 'c1', type: 'function', function: { name: 'f', arguments: '{"a":1}' } }] },
        { role: 'tool', tool_call_id: 'c1', content: '42' },
        { role: 'user', content: [{ type: 'text', text: 'see' }, { type: 'image_url', image_url: { url: 'data:image/png;base64,AAAA' } }] },
      ],
    });
    const a = only().attributes;
    expect(JSON.parse(a['gen_ai.system_instructions'] as string)).toEqual([{ type: 'text', content: 'You are terse.' }]);
    expect(JSON.parse(a['gen_ai.input.messages'] as string)).toEqual([
      { role: 'user', parts: [{ type: 'text', content: 'Hello' }] },
      { role: 'assistant', parts: [{ type: 'tool_call', id: 'c1', name: 'f', arguments: { a: 1 } }] },
      { role: 'tool', parts: [{ type: 'tool_call_response', id: 'c1', response: '42' }] },
      { role: 'user', parts: [{ type: 'text', content: 'see' }, { type: 'blob', modality: 'image' }] },
    ]);
    expect(a['gen_ai.input.messages']).not.toContain('AAAA'); // inline data never copied
    expect(JSON.parse(a['gen_ai.output.messages'] as string)).toEqual([
      { role: 'assistant', parts: [{ type: 'text', content: 'Hi there' }], finish_reason: 'stop' },
    ]);
  });

  it('honours TROOPER_LLM_CAPTURE_CONTENT at call time', async () => {
    const client = instrumentOpenAI(new FakeOpenAI());
    process.env.TROOPER_LLM_CAPTURE_CONTENT = 'true';
    await client.chat.completions.create({ model: 'gpt-4o', messages: [USER] });
    expect(only().attributes['gen_ai.input.messages']).toBeDefined();
  });

  it('streams: TTFT at first non-empty delta, accumulated output; request untouched without injectStreamUsage', async () => {
    const raw = new FakeOpenAI();
    const client = instrumentOpenAI(raw, { captureContent: true });
    const body = { model: 'gpt-4o-mini', messages: [USER], stream: true };
    const stream = await client.chat.completions.create(body);
    expect(raw.seen[0]!.body).toBe(body);
    expect(t.spans()).toHaveLength(0); // still open
    expect((stream as unknown as FakeStream<unknown>).controller).toBeInstanceOf(AbortController);
    let text = '';
    for await (const chunk of stream as AsyncIterable<{ choices: { delta?: { content?: string } }[] }>) text += chunk.choices[0]?.delta?.content ?? '';
    expect(text).toBe('Hello');
    const a = only().attributes;
    expect(a['trooper.llm.ttft_ms']).toBeTypeOf('number');
    expect(a['trooper.llm.ttft_ms']).toBeGreaterThan(0);
    expect(a).toMatchObject({ 'gen_ai.response.id': 'chatcmpl-2', 'gen_ai.response.model': 'gpt-4o-mini', 'gen_ai.response.finish_reasons': ['tool_calls'] });
    expect(a['gen_ai.usage.input_tokens']).toBeUndefined();
    expect(JSON.parse(a['gen_ai.output.messages'] as string)).toEqual([
      {
        role: 'assistant',
        parts: [
          { type: 'text', content: 'Hello' },
          { type: 'tool_call', id: 'call_1', name: 'lookup', arguments: { q: 'x' } },
        ],
        finish_reason: 'tool_calls',
      },
    ]);
  });

  it('injectStreamUsage adds stream_options.include_usage and records tokens', async () => {
    const raw = new FakeOpenAI();
    const client = instrumentOpenAI(raw, { injectStreamUsage: true });
    const stream = await client.chat.completions.create({ model: 'gpt-4o-mini', messages: [USER], stream: true, stream_options: {} });
    for await (const _ of stream as AsyncIterable<unknown>) void _;
    expect((raw.seen[0]!.body as { stream_options: unknown }).stream_options).toEqual({ include_usage: true });
    expect(only().attributes).toMatchObject({ 'gen_ai.usage.input_tokens': 20, 'gen_ai.usage.output_tokens': 7 });
  });

  it('early break ends the span', async () => {
    const client = instrumentOpenAI(new FakeOpenAI());
    const stream = await client.chat.completions.create({ model: 'gpt-4o-mini', messages: [USER], stream: true });
    for await (const _ of stream as AsyncIterable<unknown>) break;
    expect(only().status.code).not.toBe(SpanStatusCode.ERROR);
  });

  it('records errors with status and error.type, rethrowing the original', async () => {
    const client = instrumentOpenAI(new FakeOpenAI());
    await expect(client.chat.completions.create({ model: 'boom', messages: [USER] })).rejects.toThrow('Rate limit');
    const s = only();
    expect(s.status.code).toBe(SpanStatusCode.ERROR);
    expect(s.attributes).toMatchObject({ 'error.type': 'requests', 'http.response.status_code': 429 });
    expect(s.events.some((e) => e.name === 'exception')).toBe(true);
  });

  it('keeps APIPromise helpers: withResponse maps data, asResponse ends the span', async () => {
    const client = instrumentOpenAI(new FakeOpenAI());
    const { data, response } = await (client.chat.completions.create({ model: 'gpt-4o', messages: [USER] }) as unknown as FakeAPIPromise<unknown>).withResponse();
    expect(data).toBe(CHAT_RESPONSE);
    expect(response.status).toBe(200);
    expect(only().attributes['gen_ai.usage.output_tokens']).toBe(3);
    t.exporter.reset();
    await (client.chat.completions.create({ model: 'gpt-4o', messages: [USER] }) as unknown as FakeAPIPromise<unknown>).asResponse();
    expect(only().attributes['http.response.status_code']).toBe(200);
  });

  it('responses.create (non-stream + stream) and embeddings.create', async () => {
    const client = instrumentOpenAI(new FakeOpenAI(), { captureContent: true });
    await client.responses.create({ model: 'gpt-5', instructions: 'Be brief', input: 'Hey', max_output_tokens: 16 });
    let s = only();
    expect(s.name).toBe('chat gpt-5');
    expect(s.attributes).toMatchObject({
      'gen_ai.response.id': 'resp_2',
      'gen_ai.usage.input_tokens': 30,
      'gen_ai.usage.output_tokens': 16,
      'gen_ai.request.max_tokens': 16,
      'gen_ai.response.finish_reasons': ['max_output_tokens'],
    });
    expect(JSON.parse(s.attributes['gen_ai.system_instructions'] as string)).toEqual([{ type: 'text', content: 'Be brief' }]);
    expect(JSON.parse(s.attributes['gen_ai.input.messages'] as string)).toEqual([{ role: 'user', parts: [{ type: 'text', content: 'Hey' }] }]);
    expect(JSON.parse(s.attributes['gen_ai.output.messages'] as string)[0].parts).toEqual([
      { type: 'tool_call', id: 'c1', name: 'f', arguments: { a: 1 } },
      { type: 'text', content: 'partial' },
    ]);

    t.exporter.reset();
    const stream = await client.responses.create({ model: 'gpt-5', input: 'Hey', stream: true });
    for await (const _ of stream as AsyncIterable<unknown>) void _;
    s = only();
    expect(s.attributes).toMatchObject({ 'gen_ai.response.id': 'resp_1', 'gen_ai.usage.input_tokens': 9, 'gen_ai.response.finish_reasons': ['completed'] });
    expect(s.attributes['trooper.llm.ttft_ms']).toBeTypeOf('number');

    t.exporter.reset();
    await client.embeddings.create({ model: 'text-embedding-3-small', input: 'secret text' });
    s = only();
    expect(s.name).toBe('embeddings text-embedding-3-small');
    expect(s.attributes).toMatchObject({ 'gen_ai.operation.name': 'embeddings', 'gen_ai.usage.input_tokens': 5 });
    expect(JSON.stringify(s.attributes)).not.toContain('secret text');
  });

  it('derives the provider from baseURL and accepts an override', () => {
    expect(openaiProvider({ baseURL: 'https://my.openai.azure.com/openai' })).toBe('azure.ai.openai');
    expect(openaiProvider({ baseURL: 'https://api.groq.com/openai/v1' })).toBe('groq');
    expect(openaiProvider({ baseURL: 'http://localhost:11434/v1' })).toBe('ollama');
    expect(openaiProvider({})).toBe('openai');
  });

  it('never breaks the client on odd input', async () => {
    expect(instrumentOpenAI(null)).toBeNull();
    expect(instrumentOpenAI(42)).toBe(42);
    const client = instrumentOpenAI(new FakeOpenAI());
    // a body the wrapper can't read: the call still goes through, the provider's own error surfaces unchanged
    await expect(client.chat.completions.create(undefined as never)).rejects.toThrow(TypeError);
    expect(only().name).toBe('chat');
    t.exporter.reset();
    await expect(client.embeddings.create(undefined as never)).resolves.toMatchObject({ model: 'text-embedding-3-small' });
  });
});

describe('instrumentAnthropic', () => {
  it('messages.create: total input tokens incl. cache, cache attrs, finish reason', async () => {
    const raw = new FakeAnthropic();
    const client = instrumentAnthropic(raw);
    await client.messages.create({ model: 'claude-sonnet-4-5', max_tokens: 1024, system: 'sys', messages: [USER] });
    const s = only();
    expect(s.name).toBe('chat claude-sonnet-4-5');
    expect(s.attributes).toMatchObject({
      'gen_ai.provider.name': 'anthropic',
      'gen_ai.response.model': 'claude-sonnet-4-5-20250929',
      'gen_ai.response.id': 'msg_1',
      'gen_ai.usage.input_tokens': 130,
      'gen_ai.usage.output_tokens': 5,
      'gen_ai.usage.cache_read.input_tokens': 100,
      'gen_ai.usage.cache_creation.input_tokens': 20,
      'gen_ai.request.max_tokens': 1024,
      'gen_ai.response.finish_reasons': ['end_turn'],
      'server.address': 'api.anthropic.com',
    });
    expect(s.attributes['gen_ai.system_instructions']).toBeUndefined();
    expect(raw.seen[0]!.activeSpan).toBe('chat claude-sonnet-4-5');
  });

  it('messages.create with content capture', async () => {
    const client = instrumentAnthropic(new FakeAnthropic(), { captureContent: true });
    await client.messages.create({
      model: 'claude-sonnet-4-5',
      max_tokens: 10,
      system: [{ type: 'text', text: 'sys' }],
      messages: [USER, { role: 'user', content: [{ type: 'tool_result', tool_use_id: 'tu', content: 'ok' }] }],
    });
    const a = only().attributes;
    expect(JSON.parse(a['gen_ai.system_instructions'] as string)).toEqual([{ type: 'text', content: 'sys' }]);
    expect(JSON.parse(a['gen_ai.input.messages'] as string)).toEqual([
      { role: 'user', parts: [{ type: 'text', content: 'Hello' }] },
      { role: 'tool', parts: [{ type: 'tool_call_response', id: 'tu', response: 'ok' }] },
    ]);
    expect(JSON.parse(a['gen_ai.output.messages'] as string)).toEqual([
      { role: 'assistant', parts: [{ type: 'text', content: 'Bonjour' }], finish_reason: 'end_turn' },
    ]);
  });

  it('messages.create stream: true', async () => {
    const client = instrumentAnthropic(new FakeAnthropic(), { captureContent: true });
    const stream = await client.messages.create({ model: 'claude-haiku-4-5', max_tokens: 10, messages: [USER], stream: true });
    for await (const _ of stream as AsyncIterable<unknown>) void _;
    const a = only().attributes;
    expect(a).toMatchObject({
      'gen_ai.response.id': 'msg_2',
      'gen_ai.response.model': 'claude-haiku-4-5',
      'gen_ai.usage.input_tokens': 10,
      'gen_ai.usage.output_tokens': 11,
      'gen_ai.usage.cache_read.input_tokens': 2,
      'gen_ai.response.finish_reasons': ['tool_use'],
    });
    expect(a['trooper.llm.ttft_ms']).toBeTypeOf('number');
    expect(JSON.parse(a['gen_ai.output.messages'] as string)).toEqual([
      {
        role: 'assistant',
        parts: [
          { type: 'text', content: 'Salut' },
          { type: 'tool_call', id: 'tu_1', name: 'get', arguments: { k: 2 } },
        ],
        finish_reason: 'tool_use',
      },
    ]);
  });

  it('messages.stream(): MessageStream events, span ends on end', async () => {
    const raw = new FakeAnthropic();
    const client = instrumentAnthropic(raw);
    const ms = client.messages.stream({ model: 'claude-haiku-4-5', max_tokens: 10, messages: [USER] });
    expect(ms).toBeInstanceOf(FakeMessageStream);
    await new Promise<void>((r) => ms.on('end', () => r()));
    const s = only();
    expect(s.attributes).toMatchObject({ 'gen_ai.usage.output_tokens': 11, 'gen_ai.response.finish_reasons': ['tool_use'] });
    expect(s.attributes['trooper.llm.ttft_ms']).toBeTypeOf('number');
    expect(s.attributes['gen_ai.output.messages']).toBeUndefined();
    expect(raw.seen[0]!.activeSpan).toBe('chat claude-haiku-4-5');

    t.exporter.reset();
    const aborted = client.messages.stream({ model: 'abort', max_tokens: 1, messages: [USER] });
    await new Promise<void>((r) => aborted.on('end', () => r()));
    expect(only().status.code).toBe(SpanStatusCode.ERROR);
  });

  it('records provider error types', async () => {
    const client = instrumentAnthropic(new FakeAnthropic());
    await expect(client.messages.create({ model: 'overloaded', max_tokens: 1, messages: [USER] })).rejects.toThrow('Overloaded');
    expect(only().attributes).toMatchObject({ 'error.type': 'overloaded_error', 'http.response.status_code': 529 });
  });
});

describe('Vercel AI SDK spans', () => {
  it('pass through the trooper processor and get gen_ai.operation.name / provider', () => {
    const span = t.tracer.startSpan('ai.streamText.doStream', {
      attributes: {
        'ai.operationId': 'ai.streamText.doStream',
        'gen_ai.system': 'anthropic.messages',
        'gen_ai.request.model': 'claude-sonnet-4-5',
        'gen_ai.usage.input_tokens': 3,
        'ai.response.msToFirstChunk': 120.5,
      },
    });
    span.end();
    t.tracer.startSpan('ai.streamText', { attributes: { 'ai.operationId': 'ai.streamText' } }).end();
    expect(t.spans()).toHaveLength(2);
    expect(t.span('ai.streamText.doStream').attributes).toMatchObject({
      'gen_ai.operation.name': 'chat',
      'gen_ai.provider.name': 'anthropic',
      'gen_ai.usage.input_tokens': 3,
      'trooper.llm.ttft_ms': 120.5,
    });
    expect(t.span('ai.streamText').attributes['gen_ai.operation.name']).toBeUndefined();
  });

  it('maps provider ids', () => {
    expect(vercelProvider('openai.responses')).toBe('openai');
    expect(vercelProvider('google.vertex.chat')).toBe('gcp.vertex_ai');
    expect(vercelProvider('google.generative-ai')).toBe('gcp.gemini');
    expect(vercelProvider('amazon-bedrock')).toBe('aws.bedrock');
    expect(vercelProvider('custom.chat')).toBe('custom');
    const attrs: Record<string, unknown> = { 'ai.operationId': 'ai.embed.doEmbed', 'gen_ai.operation.name': 'x' };
    normalizeGenAiSpan({ attributes: attrs as never, setAttribute: (k, v) => (attrs[k] = v) });
    expect(attrs['gen_ai.operation.name']).toBe('x');
  });
});
