/**
 * Assistant (AI chat) data layer (ADR-033, .ai/assistant.md). Threads and messages are per user and project;
 * a turn streams back as server-sent events over a POST response.
 */
import { API_URL, api, apiStream, remote, invalidate } from './remote';

export type RefKind = 'service' | 'endpoint' | 'incident' | 'kpi' | 'monitor' | 'loadtest_run' | 'loadtest_scenario' | 'host' | 'database';
export const REF_KINDS: RefKind[] = ['service', 'endpoint', 'incident', 'kpi', 'monitor', 'loadtest_run', 'loadtest_scenario', 'host', 'database'];

export interface AssistantRef { kind: RefKind; id: string; label: string; service?: string }
export interface AssistantThread { id: string; title: string; createdAt: string; updatedAt: string }
export interface AssistantStatus { provider: 'claude' | 'mock'; model: string; writeTools: boolean }

export type ChartUnit = 'ms' | 'percent' | 'count' | 'rpm' | 'bytes';
export type Widget =
  | { kind: 'service'; service: string; title?: string }
  | { kind: 'endpoint'; service: string; endpoint: string; title?: string }
  | { kind: 'incident'; id: string; title?: string }
  | { kind: 'kpi'; id: string; title?: string }
  | { kind: 'monitor'; id: string; service?: string; title?: string }
  | { kind: 'trace'; traceId: string; title?: string }
  | { kind: 'loadtest_run'; id: string; title?: string }
  | { kind: 'logs'; service?: string; query?: string; level?: string; title?: string }
  | { kind: 'chart'; title: string; unit: ChartUnit; series: { label: string; points: [number, number][] }[]; link?: string }
  | { kind: 'table'; title: string; columns: string[]; rows: string[][]; links?: string[] }
  | { kind: 'stats'; title?: string; items: { label: string; value: string; status?: 'ok' | 'warning' | 'critical' }[]; link?: string };

export type ToolState = 'running' | 'done' | 'error';
export type ApprovalState = 'pending' | 'approved' | 'rejected';
export type Block =
  | { type: 'text'; text: string }
  | { type: 'tool'; id: string; name: string; input: unknown; state: ToolState; summary?: string }
  | { type: 'widget'; widget: Widget }
  | { type: 'approval'; id: string; name: string; input: unknown; state: ApprovalState }
  | { type: 'error'; text: string };

export interface AssistantUsage { inputTokens: number; outputTokens: number }
export interface UserMessage {
  id: string; role: 'user'; createdAt: string; text: string; refs: AssistantRef[];
  attachments: { id: string; mediaType: string }[];
}
export interface AssistantMessage { id: string; role: 'assistant'; createdAt: string; blocks: Block[]; usage?: AssistantUsage }
export type ChatMessage = UserMessage | AssistantMessage;

/** Provider + model (`GET /assistant/status`). */
export const assistantStatus = () => remote<AssistantStatus | null>('/assistant/status', null);

/** The user's threads, newest first (`GET /assistant/threads`). */
export const listThreads = () => api<AssistantThread[]>('/assistant/threads').then((x) => x ?? []);

/** One thread with its messages (`GET /assistant/threads/{id}`). */
export const getThread = (id: string) =>
  api<{ thread: AssistantThread; messages: ChatMessage[] }>(`/assistant/threads/${encodeURIComponent(id)}`)
    .then((r) => ({ thread: r.thread, messages: (r.messages ?? []).map(normalize) }));

/** New empty thread (`POST /assistant/threads`). */
export const createThread = (title?: string) =>
  api<AssistantThread>('/assistant/threads', { method: 'POST', body: JSON.stringify(title ? { title } : {}) });

/** Rename (`PATCH /assistant/threads/{id}`). */
export const renameThread = (id: string, title: string) =>
  api<AssistantThread>(`/assistant/threads/${encodeURIComponent(id)}`, { method: 'PATCH', body: JSON.stringify({ title }) });

/** Delete with its messages (`DELETE /assistant/threads/{id}`). */
export const deleteThread = (id: string) =>
  api<void>(`/assistant/threads/${encodeURIComponent(id)}`, { method: 'DELETE' });

/** Image upload (`POST /assistant/attachments`, base64 body). */
export async function uploadAttachment(file: File): Promise<{ id: string; mediaType: string; size: number }> {
  const data = await toBase64(file);
  return api('/assistant/attachments', { method: 'POST', body: JSON.stringify({ mediaType: file.type, data }) });
}

/** URL of an uploaded image (`GET /assistant/attachments/{id}`, sent with the session cookie). */
export const attachmentUrl = (id: string) => `${API_URL}/api/v1/assistant/attachments/${encodeURIComponent(id)}`;

export const IMAGE_TYPES = ['image/png', 'image/jpeg', 'image/gif', 'image/webp'];
export const MAX_IMAGE_BYTES = 5 * 1024 * 1024;
export const MAX_IMAGES = 4;

function toBase64(file: File): Promise<string> {
  return new Promise((resolve, reject) => {
    const r = new FileReader();
    r.onerror = () => reject(r.error ?? new Error('could not read file'));
    r.onload = () => {
      const s = String(r.result);
      resolve(s.slice(s.indexOf(',') + 1));
    };
    r.readAsDataURL(file);
  });
}

// ── stream ──

export type StreamEvent =
  | { event: 'message'; data: { id: string; role: 'user' | 'assistant' } }
  | { event: 'text'; data: { delta: string } }
  | { event: 'tool'; data: { id: string; name: string; input: unknown; state: ToolState; summary?: string } }
  | { event: 'widget'; data: { widget: Widget } }
  | { event: 'approval'; data: { id: string; name: string; input: unknown; state?: ApprovalState } }
  | { event: 'error'; data: { message: string; code?: string } }
  | { event: 'done'; data: { messageId: string; usage?: AssistantUsage } };

/** Send a user message; events stream back (`POST /assistant/threads/{id}/messages`). */
export function sendMessage(threadId: string, body: { text: string; refs: AssistantRef[]; attachments: string[] },
  onEvent: (e: StreamEvent) => void, signal?: AbortSignal) {
  return stream(`/assistant/threads/${encodeURIComponent(threadId)}/messages`, body, onEvent, signal);
}

/** Approve or reject a pending write call; continues the same assistant message (`POST …/approvals/{toolUseId}`). */
export function decideApproval(threadId: string, toolUseId: string, approve: boolean, onEvent: (e: StreamEvent) => void, signal?: AbortSignal) {
  return stream(`/assistant/threads/${encodeURIComponent(threadId)}/approvals/${encodeURIComponent(toolUseId)}`, { approve }, onEvent, signal);
}

async function stream(path: string, body: unknown, onEvent: (e: StreamEvent) => void, signal?: AbortSignal): Promise<void> {
  const body$ = await apiStream(path, body, signal);
  const reader = body$.getReader();
  const dec = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) break;
    buf += dec.decode(value, { stream: true }).replace(/\r\n/g, '\n');
    let cut: number;
    while ((cut = buf.indexOf('\n\n')) >= 0) {
      const frame = buf.slice(0, cut);
      buf = buf.slice(cut + 2);
      const e = parseFrame(frame);
      if (e) onEvent(e);
    }
  }
  const tail = parseFrame(buf);
  if (tail) onEvent(tail);
  invalidate('/assistant');
}

/** Parses one SSE frame (`event:` + `data:` lines; comments and unknown events are dropped). */
export function parseFrame(frame: string): StreamEvent | undefined {
  let event = 'message';
  const data: string[] = [];
  for (const raw of frame.split('\n')) {
    const line = raw.replace(/\r$/, '');
    if (!line || line.startsWith(':')) continue;
    const i = line.indexOf(':');
    const field = i < 0 ? line : line.slice(0, i);
    const val = i < 0 ? '' : line.slice(i + 1).replace(/^ /, '');
    if (field === 'event') event = val;
    else if (field === 'data') data.push(val);
  }
  if (!data.length) return undefined;
  try {
    const parsed = JSON.parse(data.join('\n')) as unknown;
    if (!['message', 'text', 'tool', 'widget', 'approval', 'error', 'done'].includes(event)) return undefined;
    return { event, data: parsed } as StreamEvent;
  } catch {
    return undefined;
  }
}

/** Applies a stream event to the assistant message being built (pure; returns a new message). */
export function applyEvent(m: AssistantMessage, e: StreamEvent): AssistantMessage {
  const blocks = [...m.blocks];
  switch (e.event) {
    case 'message':
      return e.data.role === 'assistant' && !m.id ? { ...m, id: e.data.id } : m;
    case 'text': {
      const last = blocks[blocks.length - 1];
      if (last?.type === 'text') blocks[blocks.length - 1] = { ...last, text: last.text + e.data.delta };
      else blocks.push({ type: 'text', text: e.data.delta });
      return { ...m, blocks };
    }
    case 'tool': {
      const i = blocks.findIndex((b) => b.type === 'tool' && b.id === e.data.id);
      const b: Block = { type: 'tool', ...e.data };
      if (i >= 0) blocks[i] = b; else blocks.push(b);
      return { ...m, blocks };
    }
    case 'widget':
      return { ...m, blocks: [...blocks, { type: 'widget', widget: e.data.widget }] };
    case 'approval': {
      const i = blocks.findIndex((b) => b.type === 'approval' && b.id === e.data.id);
      const b: Block = { type: 'approval', ...e.data, state: e.data.state ?? 'pending' };
      if (i >= 0) blocks[i] = b; else blocks.push(b);
      return { ...m, blocks };
    }
    case 'error':
      return { ...m, blocks: [...blocks, { type: 'error', text: e.data.message }] };
    case 'done':
      return { ...m, id: m.id || e.data.messageId, usage: e.data.usage ?? m.usage };
  }
}

/** Fills missing arrays from the api (null → []). */
function normalize(m: ChatMessage): ChatMessage {
  if (m.role === 'user') return { ...m, refs: m.refs ?? [], attachments: m.attachments ?? [], text: m.text ?? '' };
  return { ...m, blocks: m.blocks ?? [] };
}

/** `kind:id` from a `?ref=` param, when the kind is known. */
export function parseRefParam(v: string | null): { kind: RefKind; id: string } | undefined {
  if (!v) return undefined;
  const i = v.indexOf(':');
  if (i <= 0) return undefined;
  const kind = v.slice(0, i) as RefKind;
  const id = v.slice(i + 1);
  return REF_KINDS.includes(kind) && id ? { kind, id } : undefined;
}
