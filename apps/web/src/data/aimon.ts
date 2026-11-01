/**
 * AI / LLM monitoring (ADR-023, .ai/contracts.md §25): the customer's own LLM calls (OpenAI, Anthropic, Gemini, Bedrock…)
 * reported as OTel GenAI spans — calls, tokens, estimated cost, latency, recent calls and (opt-in) prompt content.
 * Not to be confused with data/usage.ts (ADR-018: our own AI credit quota).
 *
 * Api: `GET /ai/overview`, `GET /ai/calls`, `GET /ai/calls/{traceId}/{spanId}`, `GET|PUT|DELETE /ai/prices`.
 */
import { invalidate, mutate, qs, remote, remoteStatus, seg } from './remote';
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

/** Price families of the price table (services/pkg/llmprice): `gen_ai.provider.name` → family. */
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
/** Shown until the first response lands (calls = 0 would flash the setup screen, so screens check `loading`). */
const EMPTY_SET: SeriesSet = { timestamps: [], series: [] };
export const EMPTY_AI_OVERVIEW: AiOverview = {
  totals: { calls: 0, errors: 0, errorRate: 0, inputTokens: 0, outputTokens: 0, cacheReadTokens: 0, costUsd: null, costPartial: false, p50Ms: 0, p95Ms: 0 },
  series: { calls: EMPTY_SET, tokens: EMPTY_SET, cost: EMPTY_SET, latency: EMPTY_SET }, byModel: [], byService: [], byEndpoint: [],
};

// ---------------------------------------------------------------- reads

const overviewPath = (range: TimeRange, scope: AiScope) =>
  `/ai/overview${qs({ range, env: scope.env, service: scope.service, provider: scope.provider, model: scope.model })}`;

/** `GET /ai/overview` — `loading` is true until the first response for this scope (or after an error: `error`). */
export function aiOverview(range: TimeRange, scope: AiScope = {}): { data: AiOverview; loading: boolean; error?: string } {
  const path = overviewPath(range, scope);
  const data = remote<AiOverview | undefined>(path, undefined, (r: RawOverview | null) => mapOverview(r));
  if (data) return { data, loading: false };
  const error = remoteStatus().errors.find((e) => e.path === path)?.error;
  return { data: EMPTY_AI_OVERVIEW, loading: !error, error };
}

/** `GET /ai/calls` — most recent first. */
export function aiCalls(range: TimeRange, f: AiCallFilter = {}): AiCall[] {
  const path = `/ai/calls${qs({ range, service: f.service, provider: f.provider, model: f.model, route: f.route, errorsOnly: f.errorsOnly ? 'true' : undefined, limit: f.limit ?? 50 })}`;
  return remote<AiCall[]>(path, [], (r: { calls?: Raw<AiCall>[] | null } | null) => arr(r?.calls).map(mapCall));
}

/** `GET /ai/calls/{traceId}/{spanId}` — undefined while loading / unknown. */
export function aiCall(traceId: string, spanId: string): { detail?: AiCallDetail; loading: boolean; error?: string } {
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
  return remote<AiPrice[]>('/ai/prices', [], (r: { prices?: Raw<AiPrice>[] | null } | null) => arr(r?.prices).map(mapPrice));
}

/** `PUT /ai/prices` (admin+) — org override. */
export async function setAiPrice(p: Omit<AiPrice, 'source'>): Promise<void> {
  await mutate('PUT', '/ai/prices', p, ['/ai/']);
}

/** `DELETE /ai/prices?provider=&model=` — back to the default price (or to "unknown"). */
export async function resetAiPrice(provider: string, model: string): Promise<void> {
  await mutate('DELETE', `/ai/prices${qs({ provider, model })}`, undefined, ['/ai/']);
}

/**
 * Does the service make LLM calls? (`hasAi` on `GET /services/{id}`, contracts §25). The raw response is cached unmapped so
 * other readers of the same path can pick their own fields.
 */
export function serviceHasAi(serviceId: string): boolean {
  const raw = remote<{ hasAi?: boolean } | undefined>(`/services/${seg(serviceId)}`, undefined);
  return !!raw?.hasAi;
}

/** Drop cached AI responses (e.g. after a price change made elsewhere). */
export const invalidateAi = () => invalidate('/ai/');
