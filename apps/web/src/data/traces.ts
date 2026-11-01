/**
 * One trace with all of its spans (Trace detail screen, `/traces/:traceId`).
 *
 * `GET /traces/{traceId}` (services/api/openapi.yaml `Trace`).
 */
import type { TraceLlm } from './aimon';
import { remote, remoteStatus, seg } from './remote';
import type { TimeCategory } from './types';

// ---------------------------------------------------------------- types
export type SpanKind = 'server' | 'client' | 'producer' | 'consumer' | 'internal';
export interface TraceSpanEvent {
  name: string;
  /** ms from trace start */
  ts: number;
  attrs: Record<string, string>;
}
export interface TraceSpan {
  spanId: string;
  /** '' for a root span */
  parentSpanId: string;
  service: string;
  name: string;
  kind: SpanKind;
  category: TimeCategory;
  /** ms from trace start */
  start: number;
  /** ms */
  dur: number;
  /** HTTP / gRPC status code, 0 when none */
  status: number;
  failed: boolean;
  /** client/producer spans: "postgres · orders-db", "HTTP · payments-api", "kafka · orders.created" */
  dep?: string;
  /** entry spans: route template */
  route?: string;
  errorType?: string;
  attrs: Record<string, string>;
  /** resource attributes: k8s.pod.name, k8s.namespace.name, k8s.node.name, host.name, service.version … */
  resource: Record<string, string>;
  events: TraceSpanEvent[];
  /** GenAI (LLM) span summary — contracts §25 */
  llm?: TraceLlm;
}
export interface TraceDetail {
  id: string;
  /** epoch ms of the first span */
  ts: number;
  /** ms */
  dur: number;
  services: string[];
  errors: number;
  spans: TraceSpan[];
}
export interface TraceState { trace?: TraceDetail; loading: boolean; error?: string; notFound?: boolean }

// ---------------------------------------------------------------- api mapping
interface RawSpan {
  spanId: string; parentSpanId?: string | null; service: string; name: string; kind: string; category: string; start: number; dur: number;
  status: number; failed: boolean; dep?: string | null; route?: string | null; errorType?: string | null;
  attrs?: Record<string, string> | null; resource?: Record<string, string> | null;
  events?: { name: string; ts: number; attrs?: Record<string, string> | null }[] | null;
  llm?: { provider?: string; model?: string; operation?: string; inputTokens?: number; outputTokens?: number; costUsd?: number | null; ttftMs?: number | null; hasContent?: boolean } | null;
}
interface RawTrace { id: string; ts: number; dur: number; services: string[] | null; errors: number; spans: RawSpan[] | null }

const KINDS: SpanKind[] = ['server', 'client', 'producer', 'consumer', 'internal'];
const CATS: TimeCategory[] = ['app', 'db', 'cache', 'http', 'queue', 'ai'];
const num = (v: unknown) => (typeof v === 'number' && Number.isFinite(v) ? v : 0);
/** Resource keys that older api versions merged into span attrs. */
const RESOURCE_PREFIXES = ['k8s.', 'host.', 'container.', 'cloud.', 'process.', 'os.', 'telemetry.sdk.', 'service.version', 'service.instance.id', 'deployment.environment', 'faas.'];
export const isResourceKey = (k: string) => RESOURCE_PREFIXES.some((p) => k.startsWith(p));

export function mapTrace(r: RawTrace): TraceDetail {
  const spans = (r.spans ?? []).map((s): TraceSpan => {
    const attrs: Record<string, string> = {};
    const resource: Record<string, string> = { ...(s.resource ?? {}) };
    for (const [k, v] of Object.entries(s.attrs ?? {})) {
      if (!s.resource && isResourceKey(k)) resource[k] = v;
      else attrs[k] = v;
    }
    return {
      spanId: s.spanId, parentSpanId: s.parentSpanId && /[^0]/.test(s.parentSpanId) ? s.parentSpanId : '', service: s.service || 'unknown',
      name: s.name || '(unnamed)', kind: KINDS.includes(s.kind as SpanKind) ? (s.kind as SpanKind) : 'internal',
      category: CATS.includes(s.category as TimeCategory) ? (s.category as TimeCategory) : 'app',
      start: num(s.start), dur: num(s.dur), status: num(s.status), failed: !!s.failed,
      ...(s.dep ? { dep: s.dep } : {}), ...(s.route ? { route: s.route } : {}), ...(s.errorType ? { errorType: s.errorType } : {}),
      attrs, resource, events: (s.events ?? []).map((e) => ({ name: e.name, ts: num(e.ts), attrs: e.attrs ?? {} })),
      ...(s.llm ? { llm: {
        provider: s.llm.provider ?? '', model: s.llm.model ?? '', operation: s.llm.operation ?? 'chat', inputTokens: num(s.llm.inputTokens), outputTokens: num(s.llm.outputTokens),
        costUsd: typeof s.llm.costUsd === 'number' ? s.llm.costUsd : null, ttftMs: typeof s.llm.ttftMs === 'number' ? s.llm.ttftMs : undefined, hasContent: !!s.llm.hasContent,
      } } : {}),
    };
  });
  return {
    id: r.id, ts: num(r.ts), dur: num(r.dur), services: r.services ?? [...new Set(spans.map((s) => s.service))],
    errors: num(r.errors) || spans.filter((s) => s.failed).length, spans,
  };
}

/** The trace, or loading / not-found / error state. */
export function traceDetail(traceId: string): TraceState {
  const path = `/traces/${seg(traceId)}`;
  const trace = remote<TraceDetail | undefined>(path, undefined, (raw: RawTrace) => mapTrace(raw));
  if (trace) return { trace, loading: false };
  const st = remoteStatus();
  const err = st.errors.find((e) => e.path === path)?.error;
  if (err) return { loading: false, error: err, notFound: /not found/i.test(err) };
  return { loading: true };
}
