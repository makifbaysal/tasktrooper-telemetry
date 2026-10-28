import type { Attributes, AttributeValue } from '@opentelemetry/api';
import { ATTR_LLM_TTFT_MS } from './attrs';

// --- Vercel AI SDK ----------------------------------------------------------------------------------------------------

const VERCEL_OPS: Record<string, string> = {
  'ai.generateText.doGenerate': 'chat',
  'ai.streamText.doStream': 'chat',
  'ai.generateObject.doGenerate': 'chat',
  'ai.streamObject.doStream': 'chat',
  'ai.embed.doEmbed': 'embeddings',
  'ai.embedMany.doEmbed': 'embeddings',
};

const VERCEL_PROVIDERS: Record<string, string> = {
  openai: 'openai',
  anthropic: 'anthropic',
  google: 'gcp.gemini',
  'google.vertex': 'gcp.vertex_ai',
  vertex: 'gcp.vertex_ai',
  'amazon-bedrock': 'aws.bedrock',
  bedrock: 'aws.bedrock',
  'azure-openai': 'azure.ai.openai',
  azure: 'azure.ai.openai',
  mistral: 'mistral_ai',
  groq: 'groq',
  xai: 'x_ai',
  deepseek: 'deepseek',
  cohere: 'cohere',
  ollama: 'ollama',
};

/** Vercel AI SDK `gen_ai.system` (`openai.chat`, `anthropic.messages`, `google.vertex.chat` …) → `gen_ai.provider.name`. */
export function vercelProvider(system: string): string {
  const parts = system.split('.');
  const two = parts.slice(0, 2).join('.');
  return VERCEL_PROVIDERS[two] ?? VERCEL_PROVIDERS[parts[0]!] ?? parts[0]!;
}

/**
 * Vercel AI SDK (`experimental_telemetry: { isEnabled: true }`) provider-call spans carry `gen_ai.system` / usage but no
 * `gen_ai.operation.name`, which is what marks an LLM span for the writer (.ai/contracts.md §25). Fills in
 * `gen_ai.operation.name`, `gen_ai.provider.name` and `trooper.llm.ttft_ms` (from `ai.response.msToFirstChunk`) on
 * those spans; everything else passes through untouched. Called from the span processor's `onEnding`.
 */
export function normalizeGenAiSpan(span: { attributes: Attributes; setAttribute(k: string, v: AttributeValue): unknown }): void {
  const a = span.attributes;
  const opId = a['ai.operationId'];
  if (typeof opId !== 'string') return;
  const op = VERCEL_OPS[opId];
  if (!op) return;
  if (a['gen_ai.operation.name'] === undefined) span.setAttribute('gen_ai.operation.name', op);
  const system = a['gen_ai.system'] ?? a['ai.model.provider'];
  if (a['gen_ai.provider.name'] === undefined && typeof system === 'string' && system) span.setAttribute('gen_ai.provider.name', vercelProvider(system));
  if (a['gen_ai.request.model'] === undefined && typeof a['ai.model.id'] === 'string') span.setAttribute('gen_ai.request.model', a['ai.model.id']);
  const ttft = a['ai.response.msToFirstChunk'];
  if (a[ATTR_LLM_TTFT_MS] === undefined && typeof ttft === 'number') span.setAttribute(ATTR_LLM_TTFT_MS, ttft);
}
