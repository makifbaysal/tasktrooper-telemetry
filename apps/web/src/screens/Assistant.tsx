import { useEffect, useMemo, useRef, useState, type ClipboardEvent, type DragEvent, type KeyboardEvent } from 'react';
import { useNavigate, useParams, useSearchParams } from 'react-router';
import {
  AlertTriangle, ArrowUp, Check, ChevronRight, ImagePlus, Loader2, MessageSquarePlus, Pencil, ShieldCheck, Sparkles, Square, Trash2, Wrench, X,
} from 'lucide-react';
import { Badge, Button, Markdown, SkeletonText } from '@/components/ui';
import {
  IMAGE_TYPES, MAX_IMAGES, MAX_IMAGE_BYTES, applyEvent, assistantStatus, attachmentUrl, createThread, databases, decideApproval, deleteThread,
  endpointsFor, getThread, hosts, incidents, invalidate, kpis, monitors, parseRefParam, remote, renameThread, runs, scenarios, sendMessage,
  services, uploadAttachment, useData, type AssistantMessage, type AssistantRef, type AssistantThread, type Block, type ChatMessage,
  type RefKind, type StreamEvent, type UserMessage,
} from '@/data';
import { ago } from '@/lib/format';
import { AssistantWidget, RefChip } from './assistantWidgets';

const SUGGESTIONS = [
  'Which endpoints are failing right now?',
  'Why is the newest incident firing?',
  'Create an error-rate alert for checkout',
  'Summarize the last load test run',
];

const KIND_LABEL: Record<RefKind, string> = {
  service: 'service', endpoint: 'endpoint', incident: 'incident', kpi: 'KPI', monitor: 'monitor',
  loadtest_run: 'load test run', loadtest_scenario: 'scenario', host: 'host', database: 'database',
};

interface Pending { key: string; file: File; preview: string; status: 'uploading' | 'ready' | 'error'; id?: string; error?: string }

/** Free-text chat with the AI agent (MCP tools, widgets, approvals); threads on the left (ADR-033, .ai/assistant.md). */
export function Assistant() {
  useData();
  const { threadId } = useParams();
  const nav = useNavigate();
  const [params, setParams] = useSearchParams();
  const threads = remote<AssistantThread[]>('/assistant/threads', []) ?? [];
  const status = assistantStatus();

  const [messages, setMessages] = useState<ChatMessage[]>([]);
  const [loading, setLoading] = useState(false);
  const [loadError, setLoadError] = useState<string>();
  const [streaming, setStreaming] = useState(false);
  const abort = useRef<AbortController | null>(null);
  // a thread created by the first message already holds its optimistic messages: don't reload it
  const fresh = useRef<string | undefined>(undefined);

  const [text, setText] = useState('');
  const [refs, setRefs] = useState<AssistantRef[]>([]);
  const [files, setFiles] = useState<Pending[]>([]);
  const [composerError, setComposerError] = useState<string>();

  useEffect(() => {
    // the first message of a new thread is already streaming into it: keep that stream
    if (threadId && fresh.current === threadId) { fresh.current = undefined; return; }
    abort.current?.abort();
    abort.current = null;
    setStreaming(false);
    if (!threadId) { setMessages([]); setLoadError(undefined); return; }
    let live = true;
    setLoading(true); setLoadError(undefined); setMessages([]);
    getThread(threadId)
      .then((r) => { if (live) setMessages(r.messages); })
      .catch((e: unknown) => { if (live) setLoadError(e instanceof Error ? e.message : String(e)); })
      .finally(() => { if (live) setLoading(false); });
    return () => { live = false; };
  }, [threadId]);

  // ?ref=kind:id and ?q= prefill the composer (entry points from incidents etc.)
  const candidates = useMentionCandidates();
  useEffect(() => {
    const ref = parseRefParam(params.get('ref'));
    const q = params.get('q');
    if (!ref && !q) return;
    if (ref) {
      const c = candidates.find((x) => x.kind === ref.kind && x.id === ref.id);
      setRefs((rs) => rs.some((r) => r.kind === ref.kind && r.id === ref.id) ? rs : [...rs, c ?? { ...ref, label: ref.id }]);
    }
    if (q) setText(q);
    setParams({}, { replace: true });
  }, [params, candidates, setParams]);

  const scroller = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const el = scroller.current;
    if (el && el.scrollHeight - el.scrollTop - el.clientHeight < 240) el.scrollTop = el.scrollHeight;
  }, [messages]);

  const uploading = files.some((f) => f.status === 'uploading');
  const canSend = !streaming && !uploading && (text.trim().length > 0 || files.some((f) => f.status === 'ready'));

  async function send(override?: string) {
    const body = (override ?? text).trim();
    const ready = files.filter((f) => f.status === 'ready' && f.id);
    if (streaming || uploading || (!body && !ready.length)) return;
    setComposerError(undefined);
    let id = threadId;
    try {
      if (!id) {
        const t = await createThread();
        id = t.id;
        fresh.current = id;
        nav(`/assistant/${encodeURIComponent(id)}`);
      }
    } catch (e) {
      setComposerError(e instanceof Error ? e.message : String(e));
      return;
    }
    const now = new Date().toISOString();
    const user: UserMessage = {
      id: `local-${Date.now()}`, role: 'user', createdAt: now, text: body, refs,
      attachments: ready.map((f) => ({ id: f.id!, mediaType: f.file.type })),
    };
    const reply: AssistantMessage = { id: '', role: 'assistant', createdAt: now, blocks: [] };
    // a new message rejects pending approvals (server side too)
    setMessages((ms) => [...ms.map(rejectPending), user, reply]);
    setText(''); setRefs([]); setFiles([]);
    await runStream((onEvent, signal) => sendMessage(id!, { text: body, refs: user.refs, attachments: user.attachments.map((a) => a.id) }, onEvent, signal),
      (ms) => ms.length - 1, (e, ms) => {
        if (e.event === 'message' && e.data.role === 'user') {
          const i = ms.findIndex((m) => m.id === user.id);
          if (i >= 0) ms[i] = { ...ms[i], id: e.data.id };
        }
      });
  }

  async function decide(messageId: string, toolUseId: string, approve: boolean) {
    if (!threadId || streaming) return;
    setMessages((ms) => ms.map((m) => m.role === 'assistant' && m.id === messageId
      ? { ...m, blocks: m.blocks.map((b) => b.type === 'approval' && b.id === toolUseId ? { ...b, state: approve ? 'approved' as const : 'rejected' as const } : b) } : m));
    await runStream((onEvent, signal) => decideApproval(threadId, toolUseId, approve, onEvent, signal),
      (ms) => ms.findIndex((m) => m.role === 'assistant' && m.id === messageId));
  }

  /** Runs one streamed turn, applying events to the assistant message at `target(messages)`. */
  async function runStream(start: (onEvent: (e: StreamEvent) => void, signal: AbortSignal) => Promise<void>,
    target: (ms: ChatMessage[]) => number, extra?: (e: StreamEvent, ms: ChatMessage[]) => void) {
    const ctl = new AbortController();
    abort.current = ctl;
    setStreaming(true);
    const onEvent = (e: StreamEvent) => setMessages((prev) => {
      const ms = [...prev];
      extra?.(e, ms);
      const i = target(ms);
      const m = ms[i];
      if (m?.role === 'assistant') ms[i] = applyEvent(m, e);
      return ms;
    });
    try {
      await start(onEvent, ctl.signal);
    } catch (e) {
      if (!ctl.signal.aborted) onEvent({ event: 'error', data: { message: e instanceof Error ? e.message : String(e) } });
    } finally {
      if (abort.current === ctl) { abort.current = null; setStreaming(false); }
      invalidate('/assistant/threads');
    }
  }

  function stop() {
    abort.current?.abort();
    abort.current = null;
    setStreaming(false);
    setMessages((ms) => ms.map((m) => m.role === 'assistant'
      ? { ...m, blocks: m.blocks.map((b) => b.type === 'tool' && b.state === 'running' ? { ...b, state: 'error' as const, summary: 'Stopped' } : b) } : m));
  }

  function addFiles(list: FileList | File[]) {
    setComposerError(undefined);
    const incoming = [...list];
    const room = MAX_IMAGES - files.length;
    if (incoming.length > room) setComposerError(`Up to ${MAX_IMAGES} images per message.`);
    for (const file of incoming.slice(0, Math.max(0, room))) {
      if (!IMAGE_TYPES.includes(file.type)) { setComposerError('Only PNG, JPEG, GIF or WebP images.'); continue; }
      if (file.size > MAX_IMAGE_BYTES) { setComposerError(`${file.name} is larger than 5 MB.`); continue; }
      const key = `${Date.now()}-${Math.random().toString(36).slice(2)}`;
      setFiles((fs) => [...fs, { key, file, preview: URL.createObjectURL(file), status: 'uploading' }]);
      uploadAttachment(file)
        .then((r) => setFiles((fs) => fs.map((f) => f.key === key ? { ...f, status: 'ready', id: r.id } : f)))
        .catch((e: unknown) => setFiles((fs) => fs.map((f) => f.key === key ? { ...f, status: 'error', error: e instanceof Error ? e.message : String(e) } : f)));
    }
  }

  const current = threads.find((t) => t.id === threadId);

  return (
    <div className="assistant">
      <ThreadList threads={threads} active={threadId} onNew={() => { setText(''); setRefs([]); setFiles([]); nav('/assistant'); }} />

      <section className="assistant__chat" aria-label="Chat"
        onDragOver={(e: DragEvent) => { if (e.dataTransfer.types.includes('Files')) e.preventDefault(); }}
        onDrop={(e: DragEvent) => { if (e.dataTransfer.files.length) { e.preventDefault(); addFiles(e.dataTransfer.files); } }}>
        <header className="assistant__head">
          <div className="assistant__titles">
            <h1 className="truncate">{current?.title || (threadId ? 'Chat' : 'Assistant')}</h1>
            <span className="muted">Asks the same tools as the MCP server · changes need your approval</span>
          </div>
          {status && (status.provider === 'mock'
            ? <Badge tone="warning" icon={<AlertTriangle size={12} aria-hidden />}>Offline mock</Badge>
            : <Badge tone="accent" icon={<Sparkles size={12} aria-hidden />}>{status.model || 'AI'}</Badge>)}
        </header>

        <div className="assistant__scroll" ref={scroller}>
          {loading && <SkeletonText lines={6} label="Loading chat" />}
          {loadError && <div className="assistant__empty" role="alert"><AlertTriangle size={16} aria-hidden /> {loadError}</div>}
          {!loading && !loadError && messages.length === 0 && (
            <div className="assistant__welcome">
              <Sparkles size={24} aria-hidden className="assistant__welcome-icon" />
              <h2>Ask about your services</h2>
              <p className="secondary">Mention an incident, service or KPI with <kbd>@</kbd>, attach a screenshot, or ask it to set up an alert or run a load test.</p>
              <div className="assistant__suggestions">
                {SUGGESTIONS.map((s) => <button key={s} type="button" className="assistant__suggestion" onClick={() => void send(s)}>{s}</button>)}
              </div>
            </div>
          )}
          {messages.map((m, i) => m.role === 'user'
            ? <UserBubble key={m.id || i} m={m} />
            : <AssistantReply key={m.id || `a${i}`} m={m} live={streaming && i === messages.length - 1}
              busy={streaming} onDecide={(tid, ok) => void decide(m.id, tid, ok)} />)}
        </div>

        <Composer text={text} setText={setText} refs={refs} setRefs={setRefs} files={files}
          removeFile={(key) => setFiles((fs) => fs.filter((f) => f.key !== key))} addFiles={addFiles}
          candidates={candidates} canSend={canSend} streaming={streaming} onSend={() => void send()} onStop={stop} error={composerError} />
      </section>
    </div>
  );
}

function rejectPending(m: ChatMessage): ChatMessage {
  if (m.role !== 'assistant' || !m.blocks.some((b) => b.type === 'approval' && b.state === 'pending')) return m;
  return { ...m, blocks: m.blocks.map((b) => b.type === 'approval' && b.state === 'pending' ? { ...b, state: 'rejected' as const } : b) };
}

// ── threads ──

function ThreadList({ threads, active, onNew }: { threads: AssistantThread[]; active?: string; onNew: () => void }) {
  const nav = useNavigate();
  const [editing, setEditing] = useState<string>();
  const [title, setTitle] = useState('');
  const [confirm, setConfirm] = useState<string>();
  const [error, setError] = useState<string>();

  async function rename(id: string) {
    const t = title.trim();
    setEditing(undefined);
    if (!t) return;
    try { await renameThread(id, t); invalidate('/assistant/threads'); }
    catch (e) { setError(e instanceof Error ? e.message : String(e)); }
  }
  async function remove(id: string) {
    setConfirm(undefined);
    try {
      await deleteThread(id);
      invalidate('/assistant/threads');
      if (id === active) nav('/assistant');
    } catch (e) { setError(e instanceof Error ? e.message : String(e)); }
  }

  return (
    <aside className="assistant__threads" aria-label="Chat history">
      <Button variant="primary" icon={<MessageSquarePlus size={14} aria-hidden />} onClick={onNew}>New chat</Button>
      {error && <p className="assistant__error" role="alert">{error}</p>}
      <div className="assistant__thread-list">
        {threads.length === 0 && <p className="muted assistant__hint">Your chats show up here.</p>}
        {threads.map((t) => (
          <div key={t.id} className={`athread${t.id === active ? ' is-active' : ''}`}>
            {editing === t.id ? (
              <input className="input athread__input" autoFocus value={title} aria-label="Chat title"
                onChange={(e) => setTitle(e.target.value)} onBlur={() => void rename(t.id)}
                onKeyDown={(e) => { if (e.key === 'Enter') void rename(t.id); if (e.key === 'Escape') setEditing(undefined); }} />
            ) : (
              <button type="button" className="athread__open" onClick={() => nav(`/assistant/${encodeURIComponent(t.id)}`)}>
                <span className="athread__title truncate">{t.title || 'Untitled chat'}</span>
                <span className="athread__meta">{ago(Date.parse(t.updatedAt))}</span>
              </button>
            )}
            {confirm === t.id ? (
              <span className="athread__actions is-visible">
                <Button size="sm" variant="danger" onClick={() => void remove(t.id)}>Delete</Button>
                <Button size="sm" variant="ghost" iconOnly aria-label="Cancel" icon={<X size={14} />} onClick={() => setConfirm(undefined)} />
              </span>
            ) : editing !== t.id && (
              <span className="athread__actions">
                <Button size="sm" variant="ghost" iconOnly aria-label="Rename chat" icon={<Pencil size={14} />} onClick={() => { setEditing(t.id); setTitle(t.title); }} />
                <Button size="sm" variant="ghost" iconOnly aria-label="Delete chat" icon={<Trash2 size={14} />} onClick={() => setConfirm(t.id)} />
              </span>
            )}
          </div>
        ))}
      </div>
    </aside>
  );
}

// ── messages ──

function UserBubble({ m }: { m: UserMessage }) {
  return (
    <div className="amsg amsg--user">
      <div className="amsg__bubble">
        {m.refs.length > 0 && <div className="amsg__refs">{m.refs.map((r) => <RefChip key={`${r.kind}:${r.id}`} kind={KIND_LABEL[r.kind] ?? r.kind} label={r.label} />)}</div>}
        {m.attachments.length > 0 && (
          <div className="amsg__images">
            {m.attachments.map((a) => <a key={a.id} href={attachmentUrl(a.id)} target="_blank" rel="noopener noreferrer"><img src={attachmentUrl(a.id)} alt="Attached image" /></a>)}
          </div>
        )}
        {m.text && <p className="amsg__text">{m.text}</p>}
      </div>
    </div>
  );
}

function AssistantReply({ m, live, busy, onDecide }: { m: AssistantMessage; live: boolean; busy: boolean; onDecide: (toolUseId: string, approve: boolean) => void }) {
  const empty = m.blocks.length === 0;
  return (
    <div className="amsg amsg--assistant">
      <span className="amsg__avatar" aria-hidden><Sparkles size={14} /></span>
      <div className="amsg__body">
        {empty && live && <span className="muted amsg__thinking"><Loader2 className="spin" size={14} aria-hidden /> Thinking…</span>}
        {m.blocks.map((b, i) => <BlockView key={blockKey(b, i)} b={b} busy={busy} onDecide={onDecide} />)}
        {m.usage && !live && <span className="amsg__usage">{m.usage.inputTokens.toLocaleString()} in · {m.usage.outputTokens.toLocaleString()} out tokens</span>}
      </div>
    </div>
  );
}

const blockKey = (b: Block, i: number) => (b.type === 'tool' || b.type === 'approval' ? `${b.type}-${b.id}` : `${b.type}-${i}`);

function BlockView({ b, busy, onDecide }: { b: Block; busy: boolean; onDecide: (toolUseId: string, approve: boolean) => void }) {
  switch (b.type) {
    case 'text': return <Markdown text={b.text} />;
    case 'widget': return <AssistantWidget widget={b.widget} />;
    case 'error': return <p className="amsg__error" role="alert"><AlertTriangle size={14} aria-hidden /> {b.text}</p>;
    case 'tool': return <ToolRow b={b} />;
    case 'approval': return <ApprovalCard b={b} busy={busy} onDecide={onDecide} />;
  }
}

const toolLabel = (name: string) => name.replace(/^tt_/, '').replace(/_/g, ' ');

function ToolRow({ b }: { b: Extract<Block, { type: 'tool' }> }) {
  const icon = b.state === 'running' ? <Loader2 className="spin" size={12} aria-hidden />
    : b.state === 'error' ? <AlertTriangle size={12} aria-hidden className="atool__err" /> : <Check size={12} aria-hidden className="atool__ok" />;
  return (
    <details className="atool">
      <summary>
        <ChevronRight size={12} aria-hidden className="atool__chev" />
        {icon}
        <span>{b.state === 'running' ? 'Using' : b.state === 'error' ? 'Failed' : 'Used'} <span className="mono">{b.name}</span></span>
        {b.summary && <span className="muted truncate">· {b.summary}</span>}
      </summary>
      <pre className="atool__json">{pretty(b.input)}</pre>
    </details>
  );
}

function ApprovalCard({ b, busy, onDecide }: { b: Extract<Block, { type: 'approval' }>; busy: boolean; onDecide: (toolUseId: string, approve: boolean) => void }) {
  const summary = inputSummary(b.input);
  return (
    <div className={`aapproval is-${b.state}`}>
      <div className="aapproval__head">
        <ShieldCheck size={16} aria-hidden />
        <div className="aapproval__titles">
          <span className="aapproval__title">Approve: {toolLabel(b.name)}</span>
          <span className="muted">The assistant wants to make this change in your project.</span>
        </div>
        {b.state !== 'pending' && <Badge tone={b.state === 'approved' ? 'good' : 'neutral'}>{b.state === 'approved' ? 'Approved' : 'Rejected'}</Badge>}
      </div>
      {summary.length > 0 && (
        <dl className="aapproval__facts">
          {summary.map(([k, v]) => <div key={k}><dt>{k}</dt><dd className="truncate">{v}</dd></div>)}
        </dl>
      )}
      <details className="atool">
        <summary><ChevronRight size={12} aria-hidden className="atool__chev" /><Wrench size={12} aria-hidden /> Full request <span className="mono muted">{b.name}</span></summary>
        <pre className="atool__json">{pretty(b.input)}</pre>
      </details>
      {b.state === 'pending' && (
        <div className="row" style={{ gap: 'var(--space-2)' }}>
          <Button size="sm" variant="primary" disabled={busy} icon={<Check size={14} aria-hidden />} onClick={() => onDecide(b.id, true)}>Approve</Button>
          <Button size="sm" disabled={busy} icon={<X size={14} aria-hidden />} onClick={() => onDecide(b.id, false)}>Reject</Button>
        </div>
      )}
    </div>
  );
}

function pretty(v: unknown): string {
  try { return JSON.stringify(v, null, 2) ?? ''; } catch { return String(v); }
}

/** Top-level scalar fields of a tool input (confirm hidden), for the approval card. */
function inputSummary(v: unknown): [string, string][] {
  if (!v || typeof v !== 'object' || Array.isArray(v)) return [];
  return Object.entries(v as Record<string, unknown>)
    .filter(([k, x]) => k !== 'confirm' && x !== null && x !== undefined && x !== '')
    .slice(0, 8)
    .map(([k, x]) => [k, typeof x === 'object' ? JSON.stringify(x) : String(x)]);
}

// ── composer ──

type Candidate = AssistantRef & { hint?: string };

/** Everything that can be @-mentioned, from the dashboard data layer (open incidents first). */
function useMentionCandidates(): Candidate[] {
  const v = useData();
  return useMemo(() => {
    void v;
    const out: Candidate[] = [];
    const open = [...incidents].sort((a, b) => Number(a.state === 'resolved') - Number(b.state === 'resolved') || b.startedAt - a.startedAt);
    for (const i of open) out.push({ kind: 'incident', id: i.id, label: i.title, service: i.service, hint: `${i.state} · ${i.service}` });
    for (const s of services) out.push({ kind: 'service', id: s.name, label: s.name, hint: s.type });
    for (const s of services) {
      for (const e of endpointsFor(s.id)) {
        const key = e.sub ? e.sub.process : `${e.method} ${e.route}`;
        out.push({ kind: 'endpoint', id: key, label: key, service: s.name, hint: s.name });
      }
    }
    for (const k of kpis) out.push({ kind: 'kpi', id: k.id, label: k.name, hint: k.status });
    for (const m of monitors) out.push({ kind: 'monitor', id: m.id, label: m.name, hint: m.target });
    for (const r of [...runs].sort((a, b) => b.startedAt - a.startedAt)) {
      const sc = scenarios.find((x) => x.id === r.scenarioId);
      out.push({ kind: 'loadtest_run', id: r.id, label: `${sc?.name ?? 'Run'} · ${ago(r.startedAt)}`, hint: r.state });
    }
    for (const s of scenarios) out.push({ kind: 'loadtest_scenario', id: s.id, label: s.name });
    for (const h of hosts) out.push({ kind: 'host', id: h.id, label: h.hostId, hint: h.status });
    for (const d of databases) out.push({ kind: 'database', id: d.id, label: d.name, hint: d.engine });
    return out;
  }, [v]);
}

/** The `@query` right before the caret, if any. */
function mentionAt(value: string, caret: number): { start: number; query: string } | undefined {
  const before = value.slice(0, caret);
  const at = before.lastIndexOf('@');
  if (at < 0) return undefined;
  if (at > 0 && !/\s/.test(before[at - 1])) return undefined;
  const query = before.slice(at + 1);
  if (query.includes('\n') || query.length > 40) return undefined;
  return { start: at, query };
}

function Composer({ text, setText, refs, setRefs, files, removeFile, addFiles, candidates, canSend, streaming, onSend, onStop, error }: {
  text: string; setText: (v: string) => void; refs: AssistantRef[]; setRefs: (f: (rs: AssistantRef[]) => AssistantRef[]) => void;
  files: Pending[]; removeFile: (key: string) => void; addFiles: (f: FileList | File[]) => void; candidates: Candidate[];
  canSend: boolean; streaming: boolean; onSend: () => void; onStop: () => void; error?: string;
}) {
  const area = useRef<HTMLTextAreaElement>(null);
  const picker = useRef<HTMLInputElement>(null);
  const [mention, setMention] = useState<{ start: number; query: string }>();
  const [hi, setHi] = useState(0);

  const matches = useMemo(() => {
    if (!mention) return [];
    const q = mention.query.toLowerCase();
    return candidates
      .filter((c) => !refs.some((r) => r.kind === c.kind && r.id === c.id))
      .filter((c) => !q || c.label.toLowerCase().includes(q) || KIND_LABEL[c.kind].includes(q) || (c.service ?? '').toLowerCase().includes(q))
      .slice(0, 8);
  }, [mention, candidates, refs]);

  useEffect(() => {
    const el = area.current;
    if (!el) return;
    el.style.height = 'auto';
    el.style.height = `${Math.min(el.scrollHeight, 240)}px`;
  }, [text]);

  function onChange(v: string, caret: number) {
    setText(v);
    setMention(mentionAt(v, caret));
    setHi(0);
  }

  function pick(c: Candidate) {
    if (!mention) return;
    const end = mention.start + 1 + mention.query.length;
    const next = text.slice(0, mention.start) + text.slice(end);
    setText(next);
    setRefs((rs) => [...rs, { kind: c.kind, id: c.id, label: c.label, ...(c.service ? { service: c.service } : {}) }]);
    setMention(undefined);
    requestAnimationFrame(() => { area.current?.focus(); area.current?.setSelectionRange(mention.start, mention.start); });
  }

  function onKeyDown(e: KeyboardEvent<HTMLTextAreaElement>) {
    if (mention && matches.length) {
      if (e.key === 'ArrowDown') { e.preventDefault(); setHi((h) => (h + 1) % matches.length); return; }
      if (e.key === 'ArrowUp') { e.preventDefault(); setHi((h) => (h - 1 + matches.length) % matches.length); return; }
      if (e.key === 'Enter' || e.key === 'Tab') { e.preventDefault(); pick(matches[hi]); return; }
    }
    if (e.key === 'Escape' && mention) { setMention(undefined); return; }
    if (e.key === 'Enter' && !e.shiftKey && !e.nativeEvent.isComposing) {
      e.preventDefault();
      if (canSend) onSend();
    }
  }

  function onPaste(e: ClipboardEvent<HTMLTextAreaElement>) {
    const imgs = [...e.clipboardData.files].filter((f) => f.type.startsWith('image/'));
    if (imgs.length) { e.preventDefault(); addFiles(imgs); }
  }

  return (
    <div className="acomposer">
      {mention && matches.length > 0 && (
        <ul className="acomposer__picker" role="listbox" aria-label="Mention">
          {matches.map((c, i) => (
            <li key={`${c.kind}:${c.id}:${c.service ?? ''}`} role="option" aria-selected={i === hi}
              className={i === hi ? 'is-active' : undefined} onMouseDown={(e) => { e.preventDefault(); pick(c); }} onMouseEnter={() => setHi(i)}>
              <Badge tone="accent">{KIND_LABEL[c.kind]}</Badge>
              <span className="truncate">{c.label}</span>
              {c.hint && <span className="muted truncate acomposer__hint">{c.hint}</span>}
            </li>
          ))}
        </ul>
      )}
      {(refs.length > 0 || files.length > 0) && (
        <div className="acomposer__attach">
          {refs.map((r) => <RefChip key={`${r.kind}:${r.id}`} kind={KIND_LABEL[r.kind] ?? r.kind} label={r.label}
            onRemove={() => setRefs((rs) => rs.filter((x) => !(x.kind === r.kind && x.id === r.id)))} />)}
          {files.map((f) => (
            <span key={f.key} className={`athumb is-${f.status}`} title={f.error ?? f.file.name}>
              <img src={f.preview} alt={f.file.name} />
              {f.status === 'uploading' && <span className="athumb__state"><Loader2 className="spin" size={14} aria-label="Uploading" /></span>}
              {f.status === 'error' && <span className="athumb__state"><AlertTriangle size={14} aria-label={f.error ?? 'Upload failed'} /></span>}
              <button type="button" className="athumb__x" aria-label={`Remove ${f.file.name}`} onClick={() => removeFile(f.key)}><X size={12} /></button>
            </span>
          ))}
        </div>
      )}
      <div className="acomposer__box">
        <Button variant="ghost" iconOnly aria-label="Attach image" icon={<ImagePlus size={16} />} onClick={() => picker.current?.click()} />
        <input ref={picker} type="file" accept={IMAGE_TYPES.join(',')} multiple hidden
          onChange={(e) => { if (e.target.files) addFiles(e.target.files); e.target.value = ''; }} />
        <textarea ref={area} className="acomposer__input" rows={1} value={text} placeholder="Ask anything · @ to mention an incident, service or KPI"
          aria-label="Message" onChange={(e) => onChange(e.target.value, e.target.selectionStart)} onKeyDown={onKeyDown} onPaste={onPaste}
          onSelect={(e) => setMention(mentionAt(e.currentTarget.value, e.currentTarget.selectionStart))} onBlur={() => setMention(undefined)} />
        {streaming
          ? <Button variant="secondary" iconOnly aria-label="Stop" icon={<Square size={14} />} onClick={onStop} />
          : <Button variant="primary" iconOnly aria-label="Send" icon={<ArrowUp size={16} />} disabled={!canSend} onClick={onSend} />}
      </div>
      {error && <p className="assistant__error" role="alert">{error}</p>}
      <p className="acomposer__foot muted">Enter to send · Shift+Enter for a new line · paste or drop images (up to {MAX_IMAGES}, 5 MB each)</p>
    </div>
  );
}
