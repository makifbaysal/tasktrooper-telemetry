/**
 * AI / LLM monitoring (ADR-023, .ai/contracts.md §25): the customer's own LLM calls (OpenAI, Anthropic, Gemini, Bedrock…)
 * reported as OTel GenAI spans — calls, tokens, estimated cost, latency, recent calls and (opt-in) prompt content.
 * Not to be confused with data/usage.ts (ADR-018: our own AI credit quota).
 *
 * Api mode: `GET /ai/overview`, `GET /ai/calls`, `GET /ai/calls/{traceId}/{spanId}`, `GET|PUT|DELETE /ai/prices`.
 * Mock mode: four services (storefront-bff support assistant + search, recommendation-api embeddings + rerank, orders-api
 * fraud notes with a 429 burst, email-worker newsletters on Gemini) across 7 models — one without a known price.
 * storefront-bff and email-worker capture content; the other two do not.
 */
import { NOW, RANGE_POINTS, series, timestamps } from './mock';
import { invalidate, isApi, mutate, notifyData, qs, remote, remoteStatus, seg } from './remote';
import type { SeriesSet } from './api/apm';
import type { TimeRange } from './types';

// ---------------------------------------------------------------- types

/** `gen_ai.provider.name` — open set; known ones get a label. */
export const AI_PROVIDER_LABEL: Record<string, string> = {
  openai: 'OpenAI', anthropic: 'Anthropic', 'gcp.gemini': 'Gemini', 'gcp.vertex_ai': 'Vertex AI', 'aws.bedrock': 'Bedrock',
  'azure.ai.openai': 'Azure OpenAI', mistral_ai: 'Mistral', cohere: 'Cohere', groq: 'Groq', deepseek: 'DeepSeek', x_ai: 'xAI',
  ollama: 'Ollama', perplexity: 'Perplexity', ibm: 'IBM watsonx',
  // price families (llmprice)
  google: 'Google', amazon: 'Amazon', meta: 'Meta',
};
export const aiProviderLabel = (p: string) => AI_PROVIDER_LABEL[p] ?? p;

export interface AiTotals {
  calls: number; errors: number; errorRate: number;
  inputTokens: number; outputTokens: number; cacheReadTokens: number;
  /** USD estimate of the calls with a known price; null when no call has one */
  costUsd: number | null;
  /** some calls used a model without a price → costUsd leaves them out */
  costPartial: boolean;
  p50Ms: number; p95Ms: number;
  /** streaming calls only */
  ttftP95Ms?: number;
}
export interface AiModelRow {
  provider: string; model: string; calls: number; errors: number; inputTokens: number; outputTokens: number;
  costUsd: number | null; p95Ms: number; priceKnown: boolean;
}
export interface AiServiceRow { service: string; calls: number; inputTokens: number; outputTokens: number; costUsd: number | null; models: string[] }
export interface AiEndpointRow {
  service: string; route: string; method: string; calls: number; avgInputTokens: number; avgOutputTokens: number;
  costUsd: number | null; costPerCallUsd: number | null; p95Ms: number;
}
export interface AiOverview {
  totals: AiTotals;
  /** calls: ids calls, errors · tokens: input, output · cost: one per model (≤ 8) + other · latency: p95 */
  series: { calls: SeriesSet; tokens: SeriesSet; cost: SeriesSet; latency: SeriesSet };
  byModel: AiModelRow[];
  byService: AiServiceRow[];
  byEndpoint: AiEndpointRow[];
}
export interface AiCall {
  ts: number; traceId: string; spanId: string; service: string; route: string; provider: string; model: string; operation: string;
  inputTokens: number; outputTokens: number; costUsd: number | null; durationMs: number; ttftMs?: number;
  /** error message / type, e.g. "429 rate_limit_exceeded: …" */
  error?: string; finishReason?: string; hasContent: boolean;
}
/** `input` / `output` (and usually `system`) are JSON strings of OTel GenAI message arrays. */
export interface AiContent { system?: string; input: string; output: string }
export interface AiCallDetail { call: AiCall; attributes: Record<string, string>; content?: AiContent }
export interface AiPrice {
  provider: string; model: string; inputPerMTok: number; outputPerMTok: number; cacheReadPerMTok?: number; cacheWritePerMTok?: number;
  source: 'default' | 'org';
}
/** `llm` field of a trace span (contracts §25). */
export interface TraceLlm {
  provider: string; model: string; operation: string; inputTokens: number; outputTokens: number; costUsd: number | null; ttftMs?: number; hasContent: boolean;
}
export interface AiScope { service?: string; provider?: string; model?: string; env?: string }
export interface AiCallFilter { service?: string; provider?: string; model?: string; route?: string; errorsOnly?: boolean; limit?: number }

// ---------------------------------------------------------------- helpers

const num = (v: unknown, d = 0): number => (typeof v === 'number' && Number.isFinite(v) ? v : d);
const numOrNull = (v: unknown): number | null => (typeof v === 'number' && Number.isFinite(v) ? v : null);
const arr = <T>(v: T[] | null | undefined): T[] => (Array.isArray(v) ? v : []);
const str = (v: unknown): string => (typeof v === 'string' ? v : v == null ? '' : String(v));
const toMs = (v: unknown): number => {
  if (typeof v === 'number') return v < 1e12 ? v * 1000 : v;
  if (typeof v === 'string') { const t = Date.parse(v); return Number.isFinite(t) ? t : 0; }
  return 0;
};
const sum = (a: number[]) => a.reduce((x, v) => x + v, 0);
function hash(s: string): number {
  let h = 2166136261;
  for (let i = 0; i < s.length; i++) h = Math.imul(h ^ s.charCodeAt(i), 16777619);
  return h >>> 0;
}
const hex = (n: number, len: number) => n.toString(16).padStart(len, '0').slice(-len);

/** Price families of the price table (services/api/internal/llmprice): `gen_ai.provider.name` → family. */
export const AI_PRICE_FAMILIES = ['openai', 'anthropic', 'google', 'amazon', 'meta', 'mistral_ai', 'cohere', 'deepseek', 'x_ai', 'groq'];
const FAMILY: Record<string, string> = {
  openai: 'openai', 'azure.ai.openai': 'openai', azure_openai: 'openai', 'az.ai.openai': 'openai', anthropic: 'anthropic',
  'gcp.gemini': 'google', 'gcp.vertex_ai': 'google', 'gcp.gen_ai': 'google', google: 'google', vertex_ai: 'google', gemini: 'google',
  mistral_ai: 'mistral_ai', mistral: 'mistral_ai', mistralai: 'mistral_ai', cohere: 'cohere', deepseek: 'deepseek', x_ai: 'x_ai', xai: 'x_ai', groq: 'groq',
};
const BEDROCK_VENDOR: Record<string, string> = { anthropic: 'anthropic', amazon: 'amazon', meta: 'meta', mistral: 'mistral_ai', cohere: 'cohere', deepseek: 'deepseek', openai: 'openai' };
/** Same normalization as the api: provider → family, model lower-cased without `vendor/` and Bedrock `<region>.<vendor>.` prefixes. */
export function priceKey(provider: string, model: string): { family: string; model: string } {
  const p = provider.trim().toLowerCase();
  let m = model.trim().toLowerCase();
  let family = FAMILY[p] ?? p;
  const slash = m.lastIndexOf('/');
  if (slash >= 0) {
    const v = m.slice(0, slash);
    const vf = FAMILY[v.slice(v.lastIndexOf('/') + 1)];
    if (family === p && vf) family = vf;
    m = m.slice(slash + 1);
  }
  const parts = m.split('.');
  for (let i = 0; i < parts.length - 1 && i < 2; i++) {
    const f = BEDROCK_VENDOR[parts[i]];
    if (f) { if (p === 'aws.bedrock' || family === p) family = f; m = parts.slice(i + 1).join('.'); break; }
  }
  return { family, model: m };
}
/**
 * Price match rule (contracts §25, llmprice): exact model, else the longest row model that is a prefix ending at a
 * non-alphanumeric boundary (`claude-sonnet-4-5-20250929` → `claude-sonnet-4-5`, but `gpt-4o` never matches `gpt-4`).
 */
export function findPrice(prices: AiPrice[], provider: string, model: string): AiPrice | undefined {
  const k = priceKey(provider, model);
  const same = prices.filter((p) => p.provider === k.family);
  const exact = same.find((p) => p.model.toLowerCase() === k.model);
  if (exact) return exact;
  const at = (pre: string) => k.model.length > pre.length && k.model.startsWith(pre) && !/[a-z0-9]/.test(k.model[pre.length]);
  return same.filter((p) => at(p.model.toLowerCase())).sort((a, b) => b.model.length - a.model.length)[0];
}
export function priceCost(p: AiPrice | undefined, input: number, output: number, cacheRead = 0, cacheWrite = 0): number | null {
  if (!p) return null;
  const fresh = Math.max(0, input - cacheRead - cacheWrite);
  return (fresh * p.inputPerMTok + cacheRead * (p.cacheReadPerMTok ?? p.inputPerMTok) + cacheWrite * (p.cacheWritePerMTok ?? p.inputPerMTok)
    + output * p.outputPerMTok) / 1e6;
}

// ---------------------------------------------------------------- mock: prices

/** Code defaults (USD per 1M tokens; a subset of services/api/internal/llmprice/defaults.json). Provider = price family. */
const DEFAULT_PRICES: AiPrice[] = [
  { provider: 'openai', model: 'gpt-5', inputPerMTok: 1.25, outputPerMTok: 10, cacheReadPerMTok: 0.125, source: 'default' },
  { provider: 'openai', model: 'gpt-5-mini', inputPerMTok: 0.25, outputPerMTok: 2, cacheReadPerMTok: 0.025, source: 'default' },
  { provider: 'openai', model: 'gpt-5-nano', inputPerMTok: 0.05, outputPerMTok: 0.4, cacheReadPerMTok: 0.005, source: 'default' },
  { provider: 'openai', model: 'gpt-4.1', inputPerMTok: 2, outputPerMTok: 8, cacheReadPerMTok: 0.5, source: 'default' },
  { provider: 'openai', model: 'text-embedding-3-small', inputPerMTok: 0.02, outputPerMTok: 0, source: 'default' },
  { provider: 'openai', model: 'text-embedding-3-large', inputPerMTok: 0.13, outputPerMTok: 0, source: 'default' },
  { provider: 'anthropic', model: 'claude-opus-4-1', inputPerMTok: 15, outputPerMTok: 75, cacheReadPerMTok: 1.5, cacheWritePerMTok: 18.75, source: 'default' },
  { provider: 'anthropic', model: 'claude-sonnet-4-5', inputPerMTok: 3, outputPerMTok: 15, cacheReadPerMTok: 0.3, cacheWritePerMTok: 3.75, source: 'default' },
  { provider: 'anthropic', model: 'claude-haiku-4-5', inputPerMTok: 1, outputPerMTok: 5, cacheReadPerMTok: 0.1, cacheWritePerMTok: 1.25, source: 'default' },
  { provider: 'google', model: 'gemini-2.5-pro', inputPerMTok: 1.25, outputPerMTok: 10, cacheReadPerMTok: 0.31, source: 'default' },
  { provider: 'google', model: 'gemini-2.5-flash', inputPerMTok: 0.3, outputPerMTok: 2.5, cacheReadPerMTok: 0.075, source: 'default' },
  { provider: 'amazon', model: 'nova-pro', inputPerMTok: 0.8, outputPerMTok: 3.2, source: 'default' },
];
/** Org overrides (mock: a negotiated Gemini Flash rate). */
let mockOverrides: AiPrice[] = [
  { provider: 'google', model: 'gemini-2.5-flash', inputPerMTok: 0.27, outputPerMTok: 2.25, cacheReadPerMTok: 0.07, source: 'org' },
];
function mockPrices(): AiPrice[] {
  const key = (p: AiPrice) => `${p.provider}\u0000${p.model}`;
  const over = new Map(mockOverrides.map((p) => [key(p), p]));
  return [...DEFAULT_PRICES.filter((p) => !over.has(key(p))), ...mockOverrides]
    .sort((a, b) => a.provider.localeCompare(b.provider) || a.model.localeCompare(b.model));
}

// ---------------------------------------------------------------- mock: traffic

interface Stream {
  service: string; method: string; route: string; provider: string; model: string; operation: string;
  /** calls per minute */
  rpm: number; avgIn: number; avgOut: number; cacheRatio?: number; p50Ms: number; p95Ms: number; errorRate: number;
  ttftMs?: number; spikeAt?: number; content: boolean; finish: string;
}
const STREAMS: Stream[] = [
  { service: 'storefront-bff', method: 'POST', route: '/v1/assistant/chat', provider: 'anthropic', model: 'claude-sonnet-4-5', operation: 'chat',
    rpm: 42, avgIn: 3800, avgOut: 420, cacheRatio: 0.62, p50Ms: 4200, p95Ms: 9800, errorRate: 0.012, ttftMs: 640, content: true, finish: 'end_turn' },
  { service: 'storefront-bff', method: 'GET', route: '/v1/search', provider: 'anthropic', model: 'claude-haiku-4-5', operation: 'chat',
    rpm: 180, avgIn: 420, avgOut: 60, cacheRatio: 0.7, p50Ms: 520, p95Ms: 1100, errorRate: 0.002, content: true, finish: 'end_turn' },
  { service: 'recommendation-api', method: 'GET', route: '/v1/recommendations', provider: 'openai', model: 'text-embedding-3-small', operation: 'embeddings',
    rpm: 900, avgIn: 64, avgOut: 0, p50Ms: 110, p95Ms: 240, errorRate: 0.0008, content: false, finish: '' },
  { service: 'recommendation-api', method: 'GET', route: '/v1/recommendations', provider: 'openai', model: 'gpt-5-mini', operation: 'chat',
    rpm: 220, avgIn: 1600, avgOut: 120, cacheRatio: 0.3, p50Ms: 1400, p95Ms: 3200, errorRate: 0.004, content: false, finish: 'stop' },
  { service: 'orders-api', method: 'POST', route: '/v1/orders', provider: 'openai', model: 'gpt-5-mini', operation: 'chat',
    rpm: 70, avgIn: 900, avgOut: 150, p50Ms: 1600, p95Ms: 3800, errorRate: 0.008, spikeAt: 0.86, content: false, finish: 'stop' },
  { service: 'orders-api', method: 'POST', route: '/v1/orders/:id/cancel', provider: 'openai', model: 'gpt-5', operation: 'chat',
    rpm: 6, avgIn: 1200, avgOut: 300, p50Ms: 5200, p95Ms: 11000, errorRate: 0.004, content: false, finish: 'stop' },
  { service: 'email-worker', method: 'RABBITMQ', route: 'emails.marketing', provider: 'gcp.gemini', model: 'gemini-2.5-flash', operation: 'generate_content',
    rpm: 25, avgIn: 2200, avgOut: 900, p50Ms: 3100, p95Ms: 6400, errorRate: 0.006, ttftMs: 410, content: true, finish: 'STOP' },
  { service: 'email-worker', method: 'RABBITMQ', route: 'emails.transactional', provider: 'gcp.gemini', model: 'gemini-3-pro-preview', operation: 'generate_content',
    rpm: 4, avgIn: 1800, avgOut: 700, p50Ms: 7000, p95Ms: 15000, errorRate: 0.01, content: true, finish: 'STOP' },
];
const MOCK_AI_SERVICES = new Set(STREAMS.map((s) => s.service));

const streamKey = (s: Stream) => `${s.service}|${s.route}|${s.model}`;
interface StreamSeries { calls: number[]; errors: number[]; input: number[]; output: number[]; cacheRead: number[]; p50: number[]; p95: number[] }
function streamSeries(s: Stream, range: TimeRange): StreamSeries {
  const stepMin = RANGE_POINTS[range].stepMs / 60_000;
  const shape = s.spikeAt !== undefined ? { spikeAt: s.spikeAt, spikeWidth: 3 } : {};
  const k = streamKey(s);
  const calls = series(`ai-calls-${k}`, range, { base: s.rpm * stepMin, noise: 0.14, daily: 0.45 }).map(Math.round);
  const errors = series(`ai-err-${k}`, range, { base: s.errorRate, noise: 0.5, ...shape, spikeMul: 9 }).map((r, i) => Math.round(Math.min(0.5, r) * calls[i]));
  const inPer = series(`ai-in-${k}`, range, { base: s.avgIn, noise: 0.1 });
  const input = inPer.map((v, i) => Math.round(v * calls[i]));
  const output = series(`ai-out-${k}`, range, { base: s.avgOut, noise: 0.12 }).map((v, i) => Math.round(v * calls[i]));
  const cacheRead = input.map((v) => Math.round(v * (s.cacheRatio ?? 0)));
  const p50 = series(`ai-p50-${k}`, range, { base: s.p50Ms, noise: 0.1, ...shape, spikeMul: 1.4 });
  const p95 = p50.map((v) => v * (s.p95Ms / s.p50Ms));
  return { calls, errors, input, output, cacheRead, p50, p95 };
}

function matchesScope(s: Stream, scope: AiScope) {
  return (!scope.service || s.service === scope.service) && (!scope.provider || s.provider === scope.provider) && (!scope.model || s.model === scope.model);
}
const weighted = (vals: number[], w: number[]) => { const tw = sum(w); return tw ? sum(vals.map((v, i) => v * w[i])) / tw : 0; };

function mockOverview(range: TimeRange, scope: AiScope): AiOverview {
  const prices = mockPrices();
  const ts = timestamps(range);
  const n = ts.length;
  const rows = STREAMS.filter((s) => matchesScope(s, scope)).map((s) => {
    const ss = streamSeries(s, range);
    const price = findPrice(prices, s.provider, s.model);
    const cost = ss.calls.map((_, i) => priceCost(price, ss.input[i], ss.output[i], ss.cacheRead[i]));
    return { s, ss, price, cost };
  });
  const set = (list: [string, string, number[]][]): SeriesSet => ({ timestamps: ts, series: list.map(([id, label, data]) => ({ id, label, data })) });
  const at = (pick: (r: (typeof rows)[number]) => number[]) => Array.from({ length: n }, (_, i) => sum(rows.map((r) => pick(r)[i])));

  const callsS = at((r) => r.ss.calls);
  const errS = at((r) => r.ss.errors);
  const p95S = Array.from({ length: n }, (_, i) => weighted(rows.map((r) => r.ss.p95[i]), rows.map((r) => r.ss.calls[i])));

  // cost by model (known prices only), ≤ 8 + Other
  const models = [...new Set(rows.filter((r) => r.price).map((r) => r.s.model))];
  const costOf = (m: string) => Array.from({ length: n }, (_, i) => sum(rows.filter((r) => r.s.model === m).map((r) => r.cost[i] ?? 0)));
  const ranked = models.map((m) => ({ m, data: costOf(m) })).sort((a, b) => sum(b.data) - sum(a.data));
  const top = ranked.slice(0, 8);
  const rest = ranked.slice(8);
  const costList: [string, string, number[]][] = top.map((x) => [x.m, x.m, x.data]);
  if (rest.length) costList.push(['other', 'Other', Array.from({ length: n }, (_, i) => sum(rest.map((x) => x.data[i])))]);

  const tot = (pick: (r: (typeof rows)[number]) => number[]) => sum(rows.map((r) => sum(pick(r))));
  const calls = tot((r) => r.ss.calls);
  const errors = tot((r) => r.ss.errors);
  const known = rows.filter((r) => r.price);
  const totalCost = known.length ? sum(known.map((r) => sum(r.cost.map((c) => c ?? 0)))) : null;
  const callW = rows.map((r) => sum(r.ss.calls));
  const streaming = rows.filter((r) => r.s.ttftMs);

  const group = <K extends string>(keyOf: (r: (typeof rows)[number]) => K) => {
    const m = new Map<K, (typeof rows)[number][]>();
    for (const r of rows) m.set(keyOf(r), [...(m.get(keyOf(r)) ?? []), r]);
    return [...m.values()];
  };
  const costSum = (rs: (typeof rows)) => (rs.some((r) => r.price) ? sum(rs.map((r) => sum(r.cost.map((c) => c ?? 0)))) : null);
  const p95Of = (rs: (typeof rows)) => weighted(rs.map((r) => Math.max(...r.ss.p95.slice(-5))), rs.map((r) => sum(r.ss.calls)));

  return {
    totals: {
      calls, errors, errorRate: calls ? errors / calls : 0,
      inputTokens: tot((r) => r.ss.input), outputTokens: tot((r) => r.ss.output), cacheReadTokens: tot((r) => r.ss.cacheRead),
      costUsd: totalCost, costPartial: rows.some((r) => !r.price && sum(r.ss.calls) > 0),
      p50Ms: weighted(rows.map((r) => r.s.p50Ms), callW), p95Ms: p95Of(rows),
      ttftP95Ms: streaming.length ? weighted(streaming.map((r) => (r.s.ttftMs ?? 0) * 1.9), streaming.map((r) => sum(r.ss.calls))) : undefined,
    },
    series: {
      calls: set([['calls', 'Calls', callsS], ['errors', 'Errors', errS]]),
      tokens: set([['input', 'Input', at((r) => r.ss.input)], ['output', 'Output', at((r) => r.ss.output)]]),
      cost: set(costList),
      latency: set([['p95', 'p95', p95S]]),
    },
    byModel: group((r) => `${r.s.provider}|${r.s.model}`).map((rs) => ({
      provider: rs[0].s.provider, model: rs[0].s.model, calls: sum(rs.map((r) => sum(r.ss.calls))), errors: sum(rs.map((r) => sum(r.ss.errors))),
      inputTokens: sum(rs.map((r) => sum(r.ss.input))), outputTokens: sum(rs.map((r) => sum(r.ss.output))),
      costUsd: costSum(rs), p95Ms: p95Of(rs), priceKnown: !!rs[0].price,
    })),
    byService: group((r) => r.s.service).map((rs) => ({
      service: rs[0].s.service, calls: sum(rs.map((r) => sum(r.ss.calls))), inputTokens: sum(rs.map((r) => sum(r.ss.input))),
      outputTokens: sum(rs.map((r) => sum(r.ss.output))), costUsd: costSum(rs), models: [...new Set(rs.map((r) => r.s.model))],
    })),
    byEndpoint: group((r) => `${r.s.service}|${r.s.method}|${r.s.route}`).map((rs) => {
      const c = sum(rs.map((r) => sum(r.ss.calls)));
      const cost = costSum(rs);
      return {
        service: rs[0].s.service, route: rs[0].s.route, method: rs[0].s.method, calls: c,
        avgInputTokens: c ? sum(rs.map((r) => sum(r.ss.input))) / c : 0, avgOutputTokens: c ? sum(rs.map((r) => sum(r.ss.output))) / c : 0,
        costUsd: cost, costPerCallUsd: cost !== null && c ? cost / c : null, p95Ms: p95Of(rs),
      };
    }),
  };
}

// ---------------------------------------------------------------- mock: recent calls + content

const ERRORS: Record<string, string[]> = {
  openai: ['429 rate_limit_exceeded: Rate limit reached for gpt-5-mini in organization org-shopco on tokens per min (TPM): Limit 2000000, Used 1998211, Requested 1342.',
    '500 server_error: The server had an error while processing your request.'],
  anthropic: ['400 invalid_request_error: prompt is too long: 204718 tokens > 200000 maximum', '529 overloaded_error: Overloaded'],
  'gcp.gemini': ['429 RESOURCE_EXHAUSTED: Quota exceeded for aiplatform.googleapis.com/generate_content_requests_per_minute_per_project_per_base_model'],
};

interface MockCall extends AiCall { stream: Stream; cacheRead: number; variant: number }
let mockCallsCache: MockCall[] | undefined;
function mockCalls(): MockCall[] {
  if (mockCallsCache) return mockCallsCache;
  const prices = mockPrices();
  const weights = STREAMS.map((s) => Math.sqrt(s.rpm));
  const total = sum(weights);
  const out: MockCall[] = [];
  let t = NOW - 8_000;
  for (let i = 0; i < 90; i++) {
    const h = hash(`ai-call-${i}`);
    let pick = ((h % 10_000) / 10_000) * total;
    let si = 0;
    while (si < STREAMS.length - 1 && pick > weights[si]) { pick -= weights[si]; si++; }
    // a few forced failures so every interesting error shows up near the top
    const forced = ({ 2: 4, 5: 0, 9: 4, 14: 6, 21: 0 } as Record<number, number>)[i];
    if (forced !== undefined) si = forced;
    const s = STREAMS[si];
    const r = (k: string) => (hash(`${k}-${i}`) % 1000) / 1000;
    const isErr = forced !== undefined || r('e') < s.errorRate * 2;
    const errs = ERRORS[s.provider] ?? ['500 internal error'];
    // anthropic: the 5th forced call is a context-length error (huge prompt, no output)
    const err = isErr ? errs[i === 5 ? 0 : (s.provider === 'anthropic' ? 1 : 0)] : undefined;
    const contextErr = !!err && err.includes('too long');
    const input = contextErr ? 204_718 : Math.round(s.avgIn * (0.5 + r('i')));
    const output = err ? 0 : s.operation === 'embeddings' ? 0 : Math.round(s.avgOut * (0.4 + r('o') * 1.3));
    const cacheRead = err ? 0 : Math.round(input * (s.cacheRatio ?? 0) * (0.6 + r('c') * 0.5));
    const durationMs = err ? (contextErr ? 380 : 120 + r('d') * 600) : s.p50Ms * (0.5 + r('d') * 1.4);
    const variant = hash(`v-${i}`) % 3;
    const finish = err ? undefined : s.finish ? (s.model === 'claude-sonnet-4-5' && variant === 1 ? 'tool_use' : s.finish) : undefined;
    out.push({
      ts: t, traceId: hex(hash(`ai-t-${i}`), 8) + hex(hash(`ai-t2-${i}`), 8) + hex(hash(`ai-t3-${i}`), 8) + hex(hash(`ai-t4-${i}`), 8),
      spanId: hex(hash(`ai-s-${i}`), 8) + hex(hash(`ai-s2-${i}`), 8),
      service: s.service, route: s.route, provider: s.provider, model: s.model, operation: s.operation,
      inputTokens: input, outputTokens: output, costUsd: priceCost(findPrice(prices, s.provider, s.model), input, output, cacheRead),
      durationMs, ttftMs: s.ttftMs && !err ? s.ttftMs * (0.6 + r('f') * 1.2) : undefined,
      error: err, finishReason: finish, hasContent: s.content && !contextErr, stream: s, cacheRead, variant,
    });
    t -= 15_000 + (h % 40_000);
  }
  mockCallsCache = out;
  return out;
}

const text = (content: string) => ({ type: 'text', content });
const msg = (role: string, ...parts: unknown[]) => ({ role, parts });
function mockContent(c: MockCall): AiContent | undefined {
  if (!c.hasContent) return undefined;
  const J = (v: unknown) => JSON.stringify(v);
  const order = 10_400 + (c.variant * 37 + hash(c.spanId) % 90);
  switch (c.model) {
    case 'claude-sonnet-4-5': {
      const system = J([text('You are Shopco\'s customer support assistant. Answer in the customer\'s language, be brief and friendly. '
        + 'Use the tools to look up orders; never guess delivery dates. Escalate refunds over €200 to a human agent.')]);
      const user = msg('user', text(`Hi, where is my order #${order}? It was supposed to arrive yesterday.`));
      const call = msg('assistant', text('Let me check that order for you.'),
        { type: 'tool_call', id: `toolu_01${hex(hash(c.spanId), 8)}`, name: 'get_order_status', arguments: { order_id: String(order) } });
      if (c.variant === 1) {
        // the model stopped to call a tool
        return { system, input: J([user]), output: J([{ ...call, finish_reason: 'tool_use' }]) };
      }
      const result = msg('tool', { type: 'tool_call_response', id: `toolu_01${hex(hash(c.spanId), 8)}`,
        response: { status: 'in_transit', carrier: 'DHL', tracking: `JD0146000${order}`, eta: '2026-10-28', delayed_reason: 'weather — Leipzig hub' } });
      return {
        system,
        input: J([user, call, result]),
        output: J([{ ...msg('assistant', text(`Sorry for the wait! Order #${order} is on its way with DHL (tracking JD0146000${order}). `
          + 'It was held up at the Leipzig hub because of the weather and should now arrive tomorrow, 28 October. '
          + 'I\'ll keep an eye on it — is there anything else I can help with?')), finish_reason: 'end_turn' }]),
      };
    }
    case 'claude-haiku-4-5': {
      const queries = ['red running shoes under 100', 'waterproof hiking jacket women M', 'gift for 10 year old who likes lego'];
      const filters = [
        { category: 'running-shoes', color: 'red', maxPrice: 100, currency: 'EUR' },
        { category: 'jackets', features: ['waterproof'], gender: 'women', size: 'M', activity: 'hiking' },
        { category: 'toys', brand: 'LEGO', age: 10, intent: 'gift' },
      ];
      return {
        system: J([text('Rewrite the shopper\'s search query into catalog filters. Reply with JSON only: {category, color?, size?, maxPrice?, brand?, features?}.')]),
        input: J([msg('user', text(queries[c.variant]))]),
        output: J([{ ...msg('assistant', text(J(filters[c.variant]))), finish_reason: 'end_turn' }]),
      };
    }
    case 'gemini-2.5-flash':
      return {
        system: J([text('You write Shopco\'s weekly newsletter. Tone: warm, concise, no more than 120 words. Mention at most three products and end with one call to action.')]),
        input: J([msg('user', text(J({ segment: 'outdoor', locale: 'en-GB', products: [
          { sku: 'HK-2207', name: 'Ridge 2L Waterproof Jacket', price: 129, discount: 0.2 },
          { sku: 'BT-0931', name: 'Trailmaster GTX Boots', price: 159 },
          { sku: 'AC-1180', name: 'Merino Hiking Socks (3-pack)', price: 24 },
        ] })))]),
        output: J([{ ...msg('assistant', text('**Autumn is calling — are you ready?**\n\nThe trails are quieter, the colours are spectacular, and our Ridge 2L Waterproof Jacket is 20% off this week. '
          + 'Pair it with the Trailmaster GTX Boots for dry feet on muddy paths, and treat yourself to a 3-pack of Merino Hiking Socks.\n\n[Shop the autumn edit →]')), finish_reason: 'STOP' }]),
      };
    case 'gemini-3-pro-preview':
      return {
        system: J([text('Write a short personal note for an order confirmation e-mail. Use the customer\'s first name. Do not repeat the order total.')]),
        input: J([msg('user', text(J({ firstName: 'Elif', items: ['Ridge 2L Waterproof Jacket', 'Merino Hiking Socks (3-pack)'], firstOrder: true, note: 'gift wrap' })))]),
        output: J([{ ...msg('assistant', text('Hi Elif, welcome to Shopco! Your jacket and socks are being gift-wrapped right now and will be on their way soon. '
          + 'We hope whoever unwraps them has many dry, warm adventures ahead.…[truncated]')), finish_reason: 'STOP' }]),
      };
    default:
      return undefined;
  }
}

function mockAttributes(c: MockCall): Record<string, string> {
  const s = c.stream;
  const a: Record<string, string> = {
    'gen_ai.operation.name': c.operation, 'gen_ai.provider.name': c.provider, 'gen_ai.request.model': c.model,
    'gen_ai.usage.input_tokens': String(c.inputTokens), 'gen_ai.usage.output_tokens': String(c.outputTokens),
  };
  if (!c.error) a['gen_ai.response.model'] = c.provider === 'anthropic' ? `${c.model}-20250929` : c.provider === 'openai' && c.operation === 'chat' ? `${c.model}-2025-08-07` : c.model;
  if (c.cacheRead) a['gen_ai.usage.cache_read.input_tokens'] = String(c.cacheRead);
  if (c.operation !== 'embeddings') {
    a['gen_ai.request.max_tokens'] = c.provider === 'anthropic' ? '1024' : '2048';
    a['gen_ai.request.temperature'] = s.model.includes('haiku') ? '0' : '0.4';
  }
  if (c.finishReason) a['gen_ai.response.finish_reasons'] = JSON.stringify([c.finishReason]);
  if (!c.error) a['gen_ai.response.id'] = c.provider === 'anthropic' ? `msg_01${hex(hash(c.spanId), 8)}${hex(hash(c.traceId), 8)}` : `chatcmpl-${hex(hash(c.spanId), 8)}${hex(hash(c.traceId), 6)}`;
  if (c.ttftMs) a['trooper.llm.ttft_ms'] = c.ttftMs.toFixed(1);
  if (s.model === 'claude-sonnet-4-5') { a['gen_ai.conversation.id'] = `conv_${hex(hash(c.traceId), 10)}`; a['gen_ai.agent.name'] = 'support-assistant'; }
  if (c.error) a['error.type'] = c.error.split(':')[0].split(' ').slice(1).join(' ') || 'error';
  a['server.address'] = { openai: 'api.openai.com', anthropic: 'api.anthropic.com', 'gcp.gemini': 'generativelanguage.googleapis.com' }[c.provider] ?? '';
  return a;
}

const publicCall = (c: MockCall): AiCall => {
  const { stream: _s, cacheRead: _c, variant: _v, ...rest } = c;
  return rest;
};
function filterCalls(calls: MockCall[], f: AiCallFilter): MockCall[] {
  return calls.filter((c) => (!f.service || c.service === f.service) && (!f.provider || c.provider === f.provider) && (!f.model || c.model === f.model)
    && (!f.route || c.route === f.route) && (!f.errorsOnly || !!c.error)).slice(0, f.limit ?? 50);
}

/** Mock trace builder hook (data/traces.ts): the LLM call behind a mock trace id, with what the span needs. */
export function mockLlmCallForTrace(traceId: string): { call: AiCall; method: string; attrs: Record<string, string>; llm: TraceLlm } | undefined {
  if (isApi) return undefined;
  const c = mockCalls().find((x) => x.traceId === traceId);
  if (!c) return undefined;
  return {
    call: publicCall(c), method: c.stream.method, attrs: mockAttributes(c),
    llm: { provider: c.provider, model: c.model, operation: c.operation, inputTokens: c.inputTokens, outputTokens: c.outputTokens, costUsd: c.costUsd, ttftMs: c.ttftMs, hasContent: c.hasContent },
  };
}
/** Trace ids of the mock calls (seeds for data/traces.ts). */
export function mockLlmTraceIds(): string[] { return isApi ? [] : mockCalls().map((c) => c.traceId); }

// ---------------------------------------------------------------- api mapping

interface RawSet { timestamps?: number[] | null; series?: { id: string; label: string; data: number[] | null }[] | null }
const mapSet = (s: RawSet | null | undefined): SeriesSet => ({
  timestamps: arr(s?.timestamps).map(toMs),
  series: arr(s?.series).map((x) => ({ id: String(x.id), label: str(x.label) || String(x.id), data: arr(x.data).map((v) => num(v)) })),
});
type Raw<T> = { [K in keyof T]?: unknown };
const mapCall = (r: Raw<AiCall> | null | undefined): AiCall => ({
  ts: toMs(r?.ts), traceId: str(r?.traceId), spanId: str(r?.spanId), service: str(r?.service), route: str(r?.route),
  provider: str(r?.provider), model: str(r?.model), operation: str(r?.operation) || 'chat',
  inputTokens: num(r?.inputTokens), outputTokens: num(r?.outputTokens), costUsd: numOrNull(r?.costUsd), durationMs: num(r?.durationMs),
  ttftMs: numOrNull(r?.ttftMs) ?? undefined, error: str(r?.error) || undefined, finishReason: str(r?.finishReason) || undefined,
  hasContent: !!r?.hasContent,
});
interface RawOverview {
  totals?: Raw<AiTotals> | null;
  series?: { calls?: RawSet | null; tokens?: RawSet | null; cost?: RawSet | null; latency?: RawSet | null } | null;
  byModel?: Raw<AiModelRow>[] | null; byService?: (Raw<AiServiceRow> & { models?: string[] | null })[] | null; byEndpoint?: Raw<AiEndpointRow>[] | null;
}
function mapOverview(r: RawOverview | null): AiOverview {
  const t = r?.totals ?? {};
  return {
    totals: {
      calls: num(t.calls), errors: num(t.errors), errorRate: num(t.errorRate), inputTokens: num(t.inputTokens), outputTokens: num(t.outputTokens),
      cacheReadTokens: num(t.cacheReadTokens), costUsd: numOrNull(t.costUsd), costPartial: !!t.costPartial, p50Ms: num(t.p50Ms), p95Ms: num(t.p95Ms),
      ttftP95Ms: numOrNull(t.ttftP95Ms) ?? undefined,
    },
    series: { calls: mapSet(r?.series?.calls), tokens: mapSet(r?.series?.tokens), cost: mapSet(r?.series?.cost), latency: mapSet(r?.series?.latency) },
    byModel: arr(r?.byModel).map((m) => ({
      provider: str(m.provider), model: str(m.model), calls: num(m.calls), errors: num(m.errors), inputTokens: num(m.inputTokens),
      outputTokens: num(m.outputTokens), costUsd: numOrNull(m.costUsd), p95Ms: num(m.p95Ms), priceKnown: m.priceKnown === undefined ? m.costUsd != null : !!m.priceKnown,
    })),
    byService: arr(r?.byService).map((s) => ({
      service: str(s.service), calls: num(s.calls), inputTokens: num(s.inputTokens), outputTokens: num(s.outputTokens), costUsd: numOrNull(s.costUsd),
      models: arr(s.models as string[] | null).map(String),
    })),
    byEndpoint: arr(r?.byEndpoint).map((e) => ({
      service: str(e.service), route: str(e.route), method: str(e.method), calls: num(e.calls), avgInputTokens: num(e.avgInputTokens),
      avgOutputTokens: num(e.avgOutputTokens), costUsd: numOrNull(e.costUsd), costPerCallUsd: numOrNull(e.costPerCallUsd), p95Ms: num(e.p95Ms),
    })),
  };
}
const mapPrice = (p: Raw<AiPrice>): AiPrice => ({
  provider: str(p.provider), model: str(p.model), inputPerMTok: num(p.inputPerMTok), outputPerMTok: num(p.outputPerMTok),
  cacheReadPerMTok: numOrNull(p.cacheReadPerMTok) ?? undefined, cacheWritePerMTok: numOrNull(p.cacheWritePerMTok) ?? undefined,
  source: p.source === 'org' ? 'org' : 'default',
});
/** Shown in api mode until the first response lands (calls = 0 would flash the setup screen, so screens check `loading`). */
const EMPTY_SET: SeriesSet = { timestamps: [], series: [] };
export const EMPTY_AI_OVERVIEW: AiOverview = {
  totals: { calls: 0, errors: 0, errorRate: 0, inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, costUsd: null, costPartial: false, p50Ms: 0, p95Ms: 0 },
  series: { calls: EMPTY_SET, tokens: EMPTY_SET, cost: EMPTY_SET, latency: EMPTY_SET }, byModel: [], byService: [], byEndpoint: [],
};

// ---------------------------------------------------------------- reads

const overviewPath = (range: TimeRange, scope: AiScope) =>
  `/ai/overview${qs({ range, env: scope.env, service: scope.service, provider: scope.provider, model: scope.model })}`;

/** `GET /ai/overview` — `loading` is true in api mode until the first response for this scope (or after an error: `error`). */
export function aiOverview(range: TimeRange, scope: AiScope = {}): { data: AiOverview; loading: boolean; error?: string } {
  if (!isApi) return { data: mockOverview(range, scope), loading: false };
  const path = overviewPath(range, scope);
  const data = remote<AiOverview | undefined>(path, undefined, (r: RawOverview | null) => mapOverview(r));
  if (data) return { data, loading: false };
  const error = remoteStatus().errors.find((e) => e.path === path)?.error;
  return { data: EMPTY_AI_OVERVIEW, loading: !error, error };
}

/** `GET /ai/calls` — most recent first. */
export function aiCalls(range: TimeRange, f: AiCallFilter = {}): AiCall[] {
  if (!isApi) return filterCalls(mockCalls(), f).map(publicCall);
  const path = `/ai/calls${qs({ range, service: f.service, provider: f.provider, model: f.model, route: f.route, errorsOnly: f.errorsOnly ? 'true' : undefined, limit: f.limit ?? 50 })}`;
  return remote<AiCall[]>(path, [], (r: { calls?: Raw<AiCall>[] | null } | null) => arr(r?.calls).map(mapCall));
}

/** `GET /ai/calls/{traceId}/{spanId}` — undefined while loading / unknown. */
export function aiCall(traceId: string, spanId: string): { detail?: AiCallDetail; loading: boolean; error?: string } {
  if (!isApi) {
    const c = mockCalls().find((x) => x.traceId === traceId && x.spanId === spanId);
    return c ? { detail: { call: publicCall(c), attributes: mockAttributes(c), content: mockContent(c) }, loading: false } : { loading: false, error: 'Call not found' };
  }
  const path = `/ai/calls/${seg(traceId)}/${seg(spanId)}`;
  const detail = remote<AiCallDetail | undefined>(path, undefined, (r: { call?: Raw<AiCall>; attributes?: Record<string, unknown> | null; content?: Raw<AiContent> | null } | null) => (r ? {
    call: mapCall(r.call),
    attributes: Object.fromEntries(Object.entries(r.attributes ?? {}).map(([k, v]) => [k, typeof v === 'string' ? v : JSON.stringify(v)])),
    content: r.content ? { system: str(r.content.system) || undefined, input: str(r.content.input), output: str(r.content.output) } : undefined,
  } : undefined));
  if (detail) return { detail, loading: false };
  const error = remoteStatus().errors.find((e) => e.path === path)?.error;
  return { loading: !error, error };
}

/** `GET /ai/prices` */
export function aiPrices(): AiPrice[] {
  if (!isApi) return mockPrices();
  return remote<AiPrice[]>('/ai/prices', [], (r: { prices?: Raw<AiPrice>[] | null } | null) => arr(r?.prices).map(mapPrice));
}

/** `PUT /ai/prices` (admin+) — org override. */
export async function setAiPrice(p: Omit<AiPrice, 'source'>): Promise<void> {
  if (!isApi) {
    mockOverrides = [...mockOverrides.filter((x) => !(x.provider === p.provider && x.model === p.model)), { ...p, source: 'org' }];
    mockCallsCache = undefined;
    notifyData();
    return;
  }
  await mutate('PUT', '/ai/prices', p, ['/ai/']);
}

/** `DELETE /ai/prices?provider=&model=` — back to the default price (or to "unknown"). */
export async function resetAiPrice(provider: string, model: string): Promise<void> {
  if (!isApi) {
    mockOverrides = mockOverrides.filter((x) => !(x.provider === provider && x.model === model));
    mockCallsCache = undefined;
    notifyData();
    return;
  }
  await mutate('DELETE', `/ai/prices${qs({ provider, model })}`, undefined, ['/ai/']);
}

/**
 * Does the service make LLM calls? (`hasAi` on `GET /services/{id}`, contracts §25). The raw response is cached unmapped so
 * other readers of the same path can pick their own fields.
 */
export function serviceHasAi(serviceId: string): boolean {
  if (!isApi) return MOCK_AI_SERVICES.has(serviceId);
  const raw = remote<{ hasAi?: boolean } | undefined>(`/services/${seg(serviceId)}`, undefined);
  return !!raw?.hasAi;
}

/** Drop cached AI responses (e.g. after a price change made elsewhere). */
export const invalidateAi = () => invalidate('/ai/');
