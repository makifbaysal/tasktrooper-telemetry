/**
 * AI monitoring (ADR-023, contracts §25): the customer's LLM calls — calls, tokens, estimated cost, latency, recent calls with
 * prompt / completion (when captured) and the model price table. Not the AI usage screen (Usage.tsx = our own AI quota).
 *
 * `AiMonitoringBody` is shared by the `/ai` page and the service detail **AI** tab (`service` scope).
 * `LlmSpanSection` is the GenAI block of the trace detail span panel.
 */
import { useEffect, useMemo, useState, type ReactNode } from 'react';
import { Link, useNavigate } from 'react-router';
import { BotMessageSquare, Coins, ExternalLink, Loader2, MessageSquareText, Pencil, Plus, RotateCcw, ShieldAlert, Wrench } from 'lucide-react';
import { Columns, TimeSeries } from '@/components/charts';
import {
  Badge, Button, Callout, Card, CopyCode, Drawer, DrawerSection, EmptyState, Field, Input, KeyValues, MetricCard, PageHeader, Section,
  Segmented, Select, StatusBadge, Table, Toggle, type Column,
} from '@/components/ui';
import { useApp } from '@/components/layout/context';
import { can, denied } from '@/data/auth';
import { endpointsFor, services, useData } from '@/data';
import {
  AI_PRICE_FAMILIES, aiCall, aiCalls, aiOverview, aiPrices, aiProviderLabel, findPrice, priceKey, resetAiPrice, setAiPrice,
  type AiCall, type AiEndpointRow, type AiModelRow, type AiOverview, type AiPrice, type AiServiceRow, type TraceLlm,
} from '@/data/aimon';
import { ago, compact, dateTime, int, ms, pct } from '@/lib/format';
import { usd } from './Serverless';
import { LINE_SLOTS } from './shared';
import '@/styles/aimon.css';

// ---------------------------------------------------------------- formatting & small parts

/** USD estimate with enough precision for a single LLM call ($0.0012). */
export function aiUsd(v: number): string {
  if (v === 0) return '$0';
  if (v < 0.0001) return '<$0.0001';
  if (v < 0.01) return `$${v.toFixed(4)}`;
  return usd(v);
}

/** Provider identity — neutral badge, never a brand or status color. */
export function ProviderBadge({ provider }: { provider: string }) {
  return <Badge>{aiProviderLabel(provider)}</Badge>;
}

/** Cost cell: "est." value, or "price unknown" (→ prices drawer) when the model has no price. */
function CostCell({ value, unknown, onSetPrice, fine }: { value: number | null; unknown?: boolean; onSetPrice?: () => void; fine?: boolean }) {
  if (value === null || unknown) {
    const label = <span className="muted" title="No price for this model — cost is left out of the totals">price unknown</span>;
    return onSetPrice
      ? <a href="#" onClick={(e) => { e.preventDefault(); e.stopPropagation(); onSetPrice(); }}>{label}</a>
      : label;
  }
  return <span title="Estimate from token counts × model price">{fine ? aiUsd(value) : usd(value)}</span>;
}

/** Model → categorical color: fixed by the model's position in the alphabetical model list of the page (LINE_SLOTS). */
function useModelColors(models: string[]) {
  return useMemo(() => {
    const ordered = [...new Set(models)].sort();
    return (m: string) => (m === 'other' ? 'var(--text-muted)' : `var(--series-${LINE_SLOTS[Math.max(0, ordered.indexOf(m)) % LINE_SLOTS.length]})`);
  }, [models]);
}

const svcOf = (name: string) => services.find((s) => s.id === name || s.name === name);
const shortError = (e: string) => e.split(':')[0];
const tokenPair = (i: number, o: number) => `${compact(i)} → ${compact(o)}`;

// ---------------------------------------------------------------- page

export function AiMonitoring() {
  return (
    <>
      <PageHeader title="AI monitoring"
        subtitle="LLM calls from your services — calls, tokens, estimated cost, errors and latency per provider, model, service and endpoint" />
      <AiMonitoringBody />
    </>
  );
}

type PriceFocus = { provider: string; model: string } | null;

/** The whole AI monitoring view; `service` scopes it to one service (service detail AI tab). */
export function AiMonitoringBody({ service }: { service?: string }) {
  useData();
  const { range, env } = useApp();
  const { data, loading, error } = aiOverview(range, { service, env });
  const [pricesOpen, setPricesOpen] = useState(false);
  const [priceFocus, setPriceFocus] = useState<PriceFocus>(null);
  const [callTarget, setCallTarget] = useState<{ traceId: string; spanId: string } | null>(null);
  const openPrices = (focus: PriceFocus = null) => { setPriceFocus(focus); setPricesOpen(true); };

  const drawers = (
    <>
      <AiPricesDrawer open={pricesOpen} focus={priceFocus} unknownModels={data.byModel.filter((m) => !m.priceKnown)}
        onClose={() => { setPricesOpen(false); setPriceFocus(null); }} />
      <AiCallDrawer target={callTarget} onClose={() => setCallTarget(null)} onSetPrice={(p) => openPrices(p)} />
    </>
  );

  if (loading) {
    return <Card><EmptyState icon={<Loader2 className="spin" />} title="Loading LLM calls…" /></Card>;
  }
  if (data.totals.calls === 0) {
    return (
      <>
        {error && <Callout icon={<ShieldAlert size={16} />}>Could not load AI monitoring data: {error}</Callout>}
        <AiSetup scoped={!!service} onPrices={() => openPrices()} />
        {drawers}
      </>
    );
  }
  return (
    <>
      <AiCharts data={data} service={service} onPrices={() => openPrices()} />
      <Section title="Models" subtitle="cost is an estimate from token counts × model price · click a row for its recent calls">
        <Card flush><ModelsTable rows={data.byModel} onSetPrice={(m) => openPrices({ provider: m.provider, model: m.model })} /></Card>
      </Section>
      {!service && data.byService.length > 0 && (
        <Section title="Services">
          <Card flush><ServicesTable rows={data.byService} /></Card>
        </Section>
      )}
      <Section title="Endpoints" subtitle="entry route of the request that made the LLM call · cost and tokens per call">
        <Card flush><EndpointsTable rows={data.byEndpoint} scoped={!!service} /></Card>
      </Section>
      <RecentCalls service={service} data={data} onOpen={(c) => setCallTarget({ traceId: c.traceId, spanId: c.spanId })}
        onSetPrice={(p) => openPrices(p)} />
      {drawers}
    </>
  );
}

// ---------------------------------------------------------------- charts (value + chart in one MetricCard each)

function AiCharts({ data, service, onPrices }: { data: AiOverview; service?: string; onPrices: () => void }) {
  const t = data.totals;
  const color = useModelColors([...data.byModel.map((m) => m.model), ...data.series.cost.series.map((s) => s.id)]);
  const unknown = data.byModel.filter((m) => !m.priceKnown && m.calls > 0);
  const costSeries = data.series.cost.series.map((s) => ({ ...s, color: color(s.id) }));
  const pick = (set: keyof AiOverview['series'], id: string) => data.series[set].series.find((s) => s.id === id)?.data ?? [];
  return (
    <>
      {unknown.length > 0 && (
        <Callout icon={<Coins size={16} />}>
          <span className="row" style={{ flexWrap: 'wrap', gap: 'var(--space-2)' }}>
            <span>
              No price for {unknown.map((m) => <span key={m.model} className="mono">{m.model}</span>).reduce<ReactNode[]>((a, x, i) => (i ? [...a, ', ', x] : [x]), [])}
              {' '}— {compact(unknown.reduce((a, m) => a + m.calls, 0))} calls are left out of the cost.
            </span>
            <Button size="sm" onClick={onPrices}>{can('manage') ? 'Set prices' : 'View prices'}</Button>
          </span>
        </Callout>
      )}
      <Section actions={!service ? <Button icon={<Coins size={14} />} onClick={onPrices}>Prices</Button> : undefined}>
        <div className="grid grid-2">
          <MetricCard label={<>Estimated cost{t.costPartial && <span className="muted"> · partial</span>}</>}
            value={t.costUsd === null ? '—' : usd(t.costUsd)} unit="est."
            hint={t.costPartial ? 'Calls to models without a price are not counted' : `${aiUsd(t.costUsd && t.calls ? t.costUsd / t.calls : 0)} per call on average`}
            actions={service ? <Button size="sm" variant="ghost" icon={<Coins size={14} />} onClick={onPrices}>Prices</Button> : undefined}>
            {costSeries.length
              ? <Columns timestamps={data.series.cost.timestamps} series={costSeries} height={180} format={(v) => usd(v)} />
              : <p className="muted" style={{ margin: 0 }}>No model with a known price in this range.</p>}
          </MetricCard>
          <MetricCard label="Tokens" value={compact(t.inputTokens + t.outputTokens)}
            hint={`input ${compact(t.inputTokens)}${t.cacheReadTokens ? ` (${compact(t.cacheReadTokens)} from cache)` : ''} · output ${compact(t.outputTokens)}`}>
            <TimeSeries timestamps={data.series.tokens.timestamps} height={180} format={(v) => compact(v)}
              series={[{ id: 'input', label: 'Input', slot: 1, data: pick('tokens', 'input') }, { id: 'output', label: 'Output', slot: 2, data: pick('tokens', 'output') }]} />
          </MetricCard>
          <MetricCard label="Calls" value={compact(t.calls)}
            hint={`${compact(t.errors)} errors · error rate ${pct(t.errorRate)}`}>
            <TimeSeries timestamps={data.series.calls.timestamps} height={150} format={(v) => compact(v)}
              series={[{ id: 'calls', label: 'Calls', slot: 1, data: pick('calls', 'calls') }, { id: 'errors', label: 'Errors', slot: 8, data: pick('calls', 'errors') }]} />
          </MetricCard>
          <MetricCard label="Latency · p95" value={ms(t.p95Ms)}
            hint={`p50 ${ms(t.p50Ms)}${t.ttftP95Ms !== undefined ? ` · time to first token p95 ${ms(t.ttftP95Ms)} (streaming)` : ''}`}>
            <TimeSeries timestamps={data.series.latency.timestamps} height={150} format={ms} area
              series={[{ id: 'p95', label: 'p95', slot: 1, data: pick('latency', 'p95') }]} />
          </MetricCard>
        </div>
      </Section>
    </>
  );
}

// ---------------------------------------------------------------- tables

function ModelCell({ provider, model }: { provider: string; model: string }) {
  return <span className="row" style={{ gap: 'var(--space-2)', whiteSpace: 'nowrap' }}><ProviderBadge provider={provider} /><span className="mono">{model}</span></span>;
}

function ModelsTable({ rows, onSetPrice }: { rows: AiModelRow[]; onSetPrice: (m: AiModelRow) => void }) {
  const columns: Column<AiModelRow>[] = [
    { key: 'model', header: 'Model', sortValue: (m) => m.model, render: (m) => <ModelCell provider={m.provider} model={m.model} /> },
    { key: 'calls', header: 'Calls', numeric: true, sortValue: (m) => m.calls, render: (m) => compact(m.calls) },
    { key: 'err', header: 'Error rate', numeric: true, sortValue: (m) => (m.calls ? m.errors / m.calls : 0), render: (m) => (m.calls ? pct(m.errors / m.calls) : '—') },
    { key: 'in', header: 'Input tokens', numeric: true, sortValue: (m) => m.inputTokens, render: (m) => compact(m.inputTokens) },
    { key: 'out', header: 'Output tokens', numeric: true, sortValue: (m) => m.outputTokens, render: (m) => (m.outputTokens ? compact(m.outputTokens) : <span className="muted">—</span>) },
    { key: 'p95', header: 'p95', numeric: true, sortValue: (m) => m.p95Ms, render: (m) => ms(m.p95Ms) },
    { key: 'cost', header: 'Cost (est.)', numeric: true, sortValue: (m) => m.costUsd ?? -1, render: (m) => <CostCell value={m.costUsd} unknown={!m.priceKnown} onSetPrice={() => onSetPrice(m)} /> },
  ];
  return <Table rows={rows} rowKey={(m) => `${m.provider}|${m.model}`} defaultSort={{ key: 'cost', dir: 'desc' }} columns={columns} />;
}

function ServicesTable({ rows }: { rows: AiServiceRow[] }) {
  const nav = useNavigate();
  const columns: Column<AiServiceRow>[] = [
    { key: 'svc', header: 'Service', sortValue: (s) => s.service, render: (s) => <span style={{ fontWeight: 'var(--weight-medium)' }}>{s.service}</span> },
    { key: 'models', header: 'Models', render: (s) => (
      <span className="row" style={{ gap: 'var(--space-1)', flexWrap: 'wrap' }}>{s.models.map((m) => <Badge key={m}><span className="mono">{m}</span></Badge>)}</span>) },
    { key: 'calls', header: 'Calls', numeric: true, sortValue: (s) => s.calls, render: (s) => compact(s.calls) },
    { key: 'tok', header: 'Tokens in → out', numeric: true, sortValue: (s) => s.inputTokens + s.outputTokens, render: (s) => tokenPair(s.inputTokens, s.outputTokens) },
    { key: 'cost', header: 'Cost (est.)', numeric: true, sortValue: (s) => s.costUsd ?? -1, render: (s) => <CostCell value={s.costUsd} /> },
  ];
  return (
    <Table rows={rows} rowKey={(s) => s.service} defaultSort={{ key: 'cost', dir: 'desc' }} columns={columns}
      onRowClick={(s) => { const svc = svcOf(s.service); if (svc) nav(`/services/${encodeURIComponent(svc.id)}?tab=ai`); }} />
  );
}

function EndpointsTable({ rows, scoped }: { rows: AiEndpointRow[]; scoped: boolean }) {
  const nav = useNavigate();
  const target = (e: AiEndpointRow) => {
    const svc = svcOf(e.service);
    const ep = svc ? endpointsFor(svc.id).find((x) => x.route === e.route && (x.method === e.method || !!x.sub)) : undefined;
    return svc && ep ? `/services/${encodeURIComponent(svc.id)}/endpoints/${encodeURIComponent(ep.id)}` : undefined;
  };
  const columns: Column<AiEndpointRow>[] = [
    { key: 'ep', header: 'Endpoint', sortValue: (e) => `${e.service} ${e.route}`, render: (e) => (
      <div style={{ whiteSpace: 'nowrap' }}>
        <div className="mono" style={{ fontWeight: 'var(--weight-medium)' }}>{e.method} {e.route}</div>
        {!scoped && <span className="ai-cell-sub">{e.service}</span>}
      </div>) },
    { key: 'calls', header: 'Calls', numeric: true, sortValue: (e) => e.calls, render: (e) => compact(e.calls) },
    { key: 'tok', header: 'Tokens / call (in → out)', numeric: true, sortValue: (e) => e.avgInputTokens + e.avgOutputTokens, render: (e) => tokenPair(e.avgInputTokens, e.avgOutputTokens) },
    { key: 'p95', header: 'p95', numeric: true, sortValue: (e) => e.p95Ms, render: (e) => ms(e.p95Ms) },
    { key: 'cpc', header: 'Cost / call (est.)', numeric: true, sortValue: (e) => e.costPerCallUsd ?? -1, render: (e) => <CostCell value={e.costPerCallUsd} fine /> },
    { key: 'cost', header: 'Cost (est.)', numeric: true, sortValue: (e) => e.costUsd ?? -1, render: (e) => <CostCell value={e.costUsd} /> },
  ];
  return (
    <Table rows={rows} rowKey={(e) => `${e.service}|${e.method}|${e.route}`} defaultSort={{ key: 'cost', dir: 'desc' }} columns={columns}
      onRowClick={(e) => { const to = target(e); if (to) nav(to); }} />
  );
}

// ---------------------------------------------------------------- recent calls

function RecentCalls({ service, data, onOpen, onSetPrice }: {
  service?: string; data: AiOverview; onOpen: (c: AiCall) => void; onSetPrice: (p: { provider: string; model: string }) => void;
}) {
  const { range } = useApp();
  const [svc, setSvc] = useState('');
  const [model, setModel] = useState('');
  const [errorsOnly, setErrorsOnly] = useState(false);
  const models = data.byModel.map((m) => m.model).sort();
  const calls = aiCalls(range, { service: service ?? (svc || undefined), model: model || undefined, errorsOnly, limit: 50 });
  const columns: Column<AiCall>[] = [
    { key: 'ts', header: 'Time', sortValue: (c) => c.ts, render: (c) => <span title={dateTime(c.ts)} style={{ whiteSpace: 'nowrap' }}>{ago(c.ts)}</span> },
    { key: 'call', header: 'Call', render: (c) => (
      <div style={{ whiteSpace: 'nowrap' }}>
        <ModelCell provider={c.provider} model={c.model} />
        <span className="ai-cell-sub">{c.operation}{service ? '' : ` · ${c.service}`} · {c.route}</span>
      </div>) },
    { key: 'tok', header: 'Tokens in → out', numeric: true, sortValue: (c) => c.inputTokens + c.outputTokens, render: (c) => tokenPair(c.inputTokens, c.outputTokens) },
    { key: 'dur', header: 'Duration', numeric: true, sortValue: (c) => c.durationMs, render: (c) => (
      <div style={{ whiteSpace: 'nowrap' }}>
        <div>{ms(c.durationMs)}</div>
        {c.ttftMs !== undefined && <span className="ai-cell-sub">first token {ms(c.ttftMs)}</span>}
      </div>) },
    { key: 'cost', header: 'Cost (est.)', numeric: true, sortValue: (c) => c.costUsd ?? -1, render: (c) => (
      <CostCell value={c.costUsd} fine onSetPrice={() => onSetPrice({ provider: c.provider, model: c.model })} />) },
    { key: 'result', header: 'Result', render: (c) => (c.error
      ? <span title={c.error}><StatusBadge status="critical" label={shortError(c.error)} /></span>
      : c.finishReason ? <Badge>{c.finishReason}</Badge> : <span className="muted">—</span>) },
    { key: 'content', header: <span className="sr-only">Content</span>, render: (c) => (c.hasContent
      ? <span title="Prompt and completion captured"><MessageSquareText size={14} className="muted" aria-label="Prompt and completion captured" /></span> : null) },
  ];
  return (
    <Section title="Recent calls" subtitle="latest 50 · click a call for its prompt, completion and trace"
      actions={<div className="ai-filters">
        {!service && <Select aria-label="Service" value={svc} onChange={(e) => setSvc(e.target.value)}
          options={[{ value: '', label: 'All services' }, ...data.byService.map((s) => ({ value: s.service, label: s.service }))]} />}
        <Select aria-label="Model" value={model} onChange={(e) => setModel(e.target.value)}
          options={[{ value: '', label: 'All models' }, ...models.map((m) => ({ value: m, label: m }))]} />
        <span className="ai-toggle"><Toggle checked={errorsOnly} onChange={setErrorsOnly} label="Errors only" />Errors only</span>
      </div>}>
      <Card flush>
        <Table rows={calls} rowKey={(c) => `${c.traceId}:${c.spanId}`} onRowClick={onOpen} defaultSort={{ key: 'ts', dir: 'desc' }} columns={columns}
          empty={<EmptyState icon={<BotMessageSquare />} title={errorsOnly ? 'No failed calls' : 'No calls match'} />} />
      </Card>
    </Section>
  );
}

// ---------------------------------------------------------------- call drawer

/** One LLM call: meta, tokens / cost, error, messages (or how to turn content capture on) and a link to its trace. */
export function AiCallDrawer({ target, onClose, onSetPrice }: {
  target: { traceId: string; spanId: string } | null; onClose: () => void; onSetPrice?: (p: { provider: string; model: string }) => void;
}) {
  useData();
  const nav = useNavigate();
  const st = target ? aiCall(target.traceId, target.spanId) : undefined;
  const d = st?.detail;
  const c = d?.call;
  const a = d?.attributes ?? {};
  const responseModel = a['gen_ai.response.model'];
  return (
    <Drawer open={!!target} onClose={onClose} width={720}
      title={c ? <span className="mono">{c.operation} {c.model}</span> : 'LLM call'}
      subtitle={c && <span className="row" style={{ gap: 'var(--space-2)', flexWrap: 'wrap' }}>
        <ProviderBadge provider={c.provider} /><span>{c.service}</span><span className="muted">{c.route}</span><span className="muted" title={dateTime(c.ts)}>{ago(c.ts)}</span>
      </span>}
      actions={target && <Button size="sm" icon={<ExternalLink size={14} />} onClick={() => nav(`/traces/${encodeURIComponent(target.traceId)}?span=${encodeURIComponent(target.spanId)}`)}>Open trace</Button>}>
      {!d && (st?.loading
        ? <EmptyState icon={<Loader2 className="spin" />} title="Loading call…" />
        : <EmptyState icon={<BotMessageSquare />} title="Call not found">{st?.error ?? 'It may be older than the retention window.'}</EmptyState>)}
      {d && c && (
        <>
          {c.error && <Callout icon={<ShieldAlert size={16} />}><strong>{shortError(c.error)}</strong><div className="mono" style={{ overflowWrap: 'anywhere' }}>{c.error}</div></Callout>}
          <DrawerSection title="Call">
            <KeyValues items={[
              ['Provider', aiProviderLabel(c.provider)],
              ['Model', <span key="m" className="mono">{c.model}{responseModel && responseModel !== c.model ? <span className="muted"> → {responseModel}</span> : null}</span>],
              ['Operation', c.operation],
              ['Service', svcOf(c.service) ? <Link key="s" to={`/services/${encodeURIComponent(svcOf(c.service)!.id)}?tab=ai`}>{c.service}</Link> : c.service],
              ['Endpoint', <span key="r" className="mono">{c.route || '—'}</span>],
              ['Time', dateTime(c.ts)],
              ['Duration', ms(c.durationMs)],
              ['Time to first token', c.ttftMs !== undefined ? ms(c.ttftMs) : <span key="t" className="muted">not streamed</span>],
              ['Finish reason', c.finishReason ?? (c.error ? 'error' : '—')],
              ...(a['gen_ai.request.temperature'] ? [['Temperature', a['gen_ai.request.temperature']] as [ReactNode, ReactNode]] : []),
              ...(a['gen_ai.request.max_tokens'] ? [['Max tokens', a['gen_ai.request.max_tokens']] as [ReactNode, ReactNode]] : []),
              ...(a['gen_ai.agent.name'] ? [['Agent', a['gen_ai.agent.name']] as [ReactNode, ReactNode]] : []),
            ]} />
          </DrawerSection>
          <DrawerSection title="Tokens & cost" aside="cost is an estimate">
            <KeyValues items={[
              ['Input tokens', int(c.inputTokens)],
              ['Output tokens', int(c.outputTokens)],
              ...(a['gen_ai.usage.cache_read.input_tokens'] ? [['Cache read', int(Number(a['gen_ai.usage.cache_read.input_tokens']))] as [ReactNode, ReactNode]] : []),
              ...(a['gen_ai.usage.cache_creation.input_tokens'] ? [['Cache write', int(Number(a['gen_ai.usage.cache_creation.input_tokens']))] as [ReactNode, ReactNode]] : []),
              ['Cost', c.costUsd === null
                ? <CostCell key="c" value={null} onSetPrice={onSetPrice ? () => onSetPrice({ provider: c.provider, model: c.model }) : undefined} />
                : <span key="c">{aiUsd(c.costUsd)} <span className="muted">est.</span></span>],
            ]} />
          </DrawerSection>
          {d.content ? <ContentSection content={d.content} /> : <ContentCaptureOff hasContent={c.hasContent} />}
          <DrawerSection title="Attributes" aside={String(Object.keys(a).length)}>
            <AttrList attrs={a} />
          </DrawerSection>
        </>
      )}
    </Drawer>
  );
}

function AttrList({ attrs }: { attrs: Record<string, string> }) {
  const keys = Object.keys(attrs).sort();
  if (!keys.length) return <p className="muted" style={{ margin: 0 }}>No attributes.</p>;
  return (
    <dl className="attrs">
      {keys.map((k) => <div key={k} className="attrs__row"><dt className="mono">{k}</dt><dd className="mono">{attrs[k]}</dd></div>)}
    </dl>
  );
}

// ---------------------------------------------------------------- messages (OTel GenAI message JSON)

type Part =
  | { kind: 'text'; text: string }
  | { kind: 'tool_call'; id?: string; name: string; args: unknown }
  | { kind: 'tool_result'; id?: string; result: unknown }
  | { kind: 'other'; value: unknown };
interface Msg { role: string; parts: Part[]; finishReason?: string }

const asObj = (v: unknown): Record<string, unknown> | undefined => (v && typeof v === 'object' && !Array.isArray(v) ? (v as Record<string, unknown>) : undefined);
const tryJson = (s: unknown): unknown => {
  if (typeof s !== 'string') return s;
  try { return JSON.parse(s); } catch { return s; }
};
function toPart(p: unknown): Part {
  if (typeof p === 'string') return { kind: 'text', text: p };
  const o = asObj(p);
  if (!o) return { kind: 'other', value: p };
  const type = String(o.type ?? '');
  if (type === 'text' || (!type && typeof (o.content ?? o.text) === 'string')) return { kind: 'text', text: String(o.content ?? o.text ?? '') };
  if (type === 'tool_call' || type === 'tool_use' || type === 'function_call') return { kind: 'tool_call', id: o.id as string | undefined, name: String(o.name ?? 'tool'), args: tryJson(o.arguments ?? o.input) };
  if (type === 'tool_call_response' || type === 'tool_result') return { kind: 'tool_result', id: (o.id ?? o.tool_use_id) as string | undefined, result: tryJson(o.response ?? o.result ?? o.content) };
  return { kind: 'other', value: o };
}
/** Accepts the OTel GenAI shape (`{role, parts[]}`), plain `{role, content}` and OpenAI `tool_calls`; anything else renders raw. */
function parseMessages(raw: string | undefined, fallbackRole: string): Msg[] | string {
  if (!raw) return [];
  const v = tryJson(raw);
  if (typeof v === 'string') return v.trim() ? [{ role: fallbackRole, parts: [{ kind: 'text', text: v }] }] : [];
  const list = Array.isArray(v) ? v : [v];
  // system instructions: a bare parts array
  if (list.every((x) => asObj(x) && !('role' in (x as object)) && 'type' in (x as object))) return [{ role: fallbackRole, parts: list.map(toPart) }];
  const out: Msg[] = [];
  for (const m of list) {
    const o = asObj(m);
    if (!o) return raw;
    const parts: Part[] = Array.isArray(o.parts) ? o.parts.map(toPart)
      : Array.isArray(o.content) ? o.content.map(toPart)
        : o.content != null ? [{ kind: 'text', text: String(o.content) }] : [];
    for (const tc of Array.isArray(o.tool_calls) ? o.tool_calls : []) {
      const t = asObj(tc); const f = asObj(t?.function);
      parts.push({ kind: 'tool_call', id: t?.id as string | undefined, name: String(f?.name ?? t?.name ?? 'tool'), args: tryJson(f?.arguments ?? t?.arguments) });
    }
    if (o.role === 'tool' && !Array.isArray(o.parts) && typeof o.content === 'string') parts.splice(0, parts.length, { kind: 'tool_result', id: o.tool_call_id as string | undefined, result: tryJson(o.content) });
    out.push({ role: String(o.role ?? fallbackRole), parts, finishReason: (o.finish_reason ?? o.finishReason) as string | undefined });
  }
  return out;
}

const pretty = (v: unknown) => (typeof v === 'string' ? v : JSON.stringify(v, null, 2));

function MessageView({ m }: { m: Msg }) {
  const role = m.role.toLowerCase();
  return (
    <div className={`llm-msg llm-msg--${role === 'system' ? 'system' : role === 'assistant' ? 'assistant' : 'other'}`}>
      <div className="llm-msg__head">
        <span className="llm-msg__role">{role}</span>
        {m.finishReason && <span>finish: <span className="mono">{m.finishReason}</span></span>}
      </div>
      {m.parts.length === 0 && <span className="muted">(empty)</span>}
      {m.parts.map((p, i) => {
        switch (p.kind) {
          case 'text': return <p key={i} className="llm-msg__text">{p.text}</p>;
          case 'tool_call': return (
            <div key={i} className="llm-msg__tool">
              <div className="llm-msg__tool-head"><Wrench size={14} aria-hidden />Tool call <span className="mono" style={{ fontWeight: 'var(--weight-medium)' }}>{p.name}</span>{p.id && <span className="muted mono">{p.id}</span>}</div>
              <pre className="code">{pretty(p.args)}</pre>
            </div>);
          case 'tool_result': return (
            <div key={i} className="llm-msg__tool">
              <div className="llm-msg__tool-head"><Wrench size={14} aria-hidden />Tool result{p.id && <span className="muted mono">{p.id}</span>}</div>
              <pre className="code">{pretty(p.result)}</pre>
            </div>);
          default: return <pre key={i} className="code">{pretty(p.value)}</pre>;
        }
      })}
    </div>
  );
}

function MessageList({ raw, role }: { raw?: string; role: string }) {
  const msgs = parseMessages(raw, role);
  if (typeof msgs === 'string') return <pre className="code" style={{ whiteSpace: 'pre-wrap' }}>{msgs}</pre>;
  if (!msgs.length) return <p className="muted" style={{ margin: 0 }}>Empty.</p>;
  return <div className="llm-msgs">{msgs.map((m, i) => <MessageView key={i} m={m} />)}</div>;
}

function ContentSection({ content }: { content: { system?: string; input: string; output: string } }) {
  return (
    <>
      {content.system && <DrawerSection title="System instructions"><MessageList raw={content.system} role="system" /></DrawerSection>}
      <DrawerSection title="Prompt" aside="input messages"><MessageList raw={content.input} role="user" /></DrawerSection>
      <DrawerSection title="Completion" aside="output messages"><MessageList raw={content.output} role="assistant" /></DrawerSection>
      <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>Captured content is kept for 7 days, up to 32 KB per field.</p>
    </>
  );
}

const CAPTURE_SNIPPETS = {
  node: `import { init } from '@trooper/node';\n\ninit({ service: 'support-bot', llm: { captureContent: true } });\n// or: TROOPER_LLM_CAPTURE_CONTENT=true`,
  go: `# environment of the service\nTROOPER_LLM_CAPTURE_CONTENT=true`,
};
/** Empty state for a call without prompt / completion. */
function ContentCaptureOff({ hasContent }: { hasContent: boolean }) {
  const [lang, setLang] = useState<'node' | 'go'>('node');
  return (
    <DrawerSection title="Prompt & completion">
      {hasContent
        ? <p className="muted" style={{ margin: 0 }}>Content was captured but is no longer available (kept for 7 days).</p>
        : (
          <div className="stack" style={{ gap: 'var(--space-3)' }}>
            <EmptyState icon={<MessageSquareText />} title="Content capture is off">
              Prompts and completions are not collected by default because they can contain personal data. Turn it on per service;
              content is stored separately for 7 days (≤ 32 KB per field) and only token counts stay on the span.
            </EmptyState>
            <Segmented label="SDK" value={lang} onChange={setLang} options={[{ id: 'node', label: 'Node.js' }, { id: 'go', label: 'Go' }]} />
            <CopyCode>{CAPTURE_SNIPPETS[lang]}</CopyCode>
          </div>
        )}
    </DrawerSection>
  );
}

// ---------------------------------------------------------------- prices drawer

interface PriceDraft { provider: string; model: string; input: string; output: string; cacheRead: string; cacheWrite: string }
const draftOf = (p?: Partial<AiPrice> & { provider: string; model: string }): PriceDraft => ({
  provider: p?.provider ?? 'openai', model: p?.model ?? '',
  input: p?.inputPerMTok !== undefined ? String(p.inputPerMTok) : '', output: p?.outputPerMTok !== undefined ? String(p.outputPerMTok) : '',
  cacheRead: p?.cacheReadPerMTok !== undefined ? String(p.cacheReadPerMTok) : '', cacheWrite: p?.cacheWritePerMTok !== undefined ? String(p.cacheWritePerMTok) : '',
});
const perM = (v?: number) => (v === undefined ? <span className="muted">—</span> : `$${v < 0.1 && v > 0 ? v.toFixed(3) : v.toFixed(2)}`);

/** Model price table (USD per 1M tokens); admins override or reset, everyone else reads. */
export function AiPricesDrawer({ open, onClose, focus, unknownModels = [] }: {
  open: boolean; onClose: () => void; focus?: PriceFocus; unknownModels?: AiModelRow[];
}) {
  useData();
  const prices = aiPrices();
  const admin = can('manage');
  const [draft, setDraft] = useState<PriceDraft | null>(null);
  const [busy, setBusy] = useState(false);
  const [err, setErr] = useState<string | null>(null);

  // opening from a "price unknown" link starts the form for that model
  useEffect(() => {
    if (!open) { setDraft(null); setErr(null); return; }
    if (focus && admin) {
      // a model row carries gen_ai.provider.name (gcp.gemini); the price table is keyed by family (google)
      const k = priceKey(focus.provider, focus.model);
      setDraft(draftOf(prices.find((p) => p.provider === k.family && p.model === k.model) ?? { provider: k.family, model: k.model }));
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [open, focus]);

  const n = (s: string) => (s.trim() === '' ? undefined : Number(s));
  const valid = !!draft && draft.model.trim() !== '' && [draft.input, draft.output].every((v) => v.trim() !== '' && Number(v) >= 0)
    && [draft.cacheRead, draft.cacheWrite].every((v) => v.trim() === '' || Number(v) >= 0);
  const save = async () => {
    if (!draft || !valid) return;
    setBusy(true); setErr(null);
    try {
      await setAiPrice({ provider: draft.provider, model: draft.model.trim(), inputPerMTok: Number(draft.input), outputPerMTok: Number(draft.output),
        cacheReadPerMTok: n(draft.cacheRead), cacheWritePerMTok: n(draft.cacheWrite) });
      setDraft(null);
    } catch (e) { setErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  };
  const reset = async (p: AiPrice) => {
    setBusy(true); setErr(null);
    try { await resetAiPrice(p.provider, p.model); } catch (e) { setErr(e instanceof Error ? e.message : String(e)); } finally { setBusy(false); }
  };
  const unknown = unknownModels.filter((m) => !findPrice(prices, m.provider, m.model));

  const columns: Column<AiPrice>[] = [
    { key: 'model', header: 'Model', sortValue: (p) => `${p.provider} ${p.model}`, render: (p) => <ModelCell provider={p.provider} model={p.model} /> },
    { key: 'in', header: 'Input', numeric: true, sortValue: (p) => p.inputPerMTok, render: (p) => perM(p.inputPerMTok) },
    { key: 'out', header: 'Output', numeric: true, sortValue: (p) => p.outputPerMTok, render: (p) => perM(p.outputPerMTok) },
    { key: 'cr', header: 'Cache read', numeric: true, render: (p) => perM(p.cacheReadPerMTok) },
    { key: 'cw', header: 'Cache write', numeric: true, render: (p) => perM(p.cacheWritePerMTok) },
    { key: 'src', header: 'Source', sortValue: (p) => p.source, render: (p) => (p.source === 'org' ? <Badge tone="accent">Org override</Badge> : <Badge>Default</Badge>) },
    ...(admin ? [{ key: 'act', header: <span className="sr-only">Actions</span>, render: (p: AiPrice) => (
      <span className="row" style={{ gap: 'var(--space-1)', justifyContent: 'flex-end' }}>
        <Button size="sm" variant="ghost" iconOnly icon={<Pencil size={14} />} aria-label={`Edit price of ${p.model}`} title="Override" onClick={() => setDraft(draftOf(p))} />
        {p.source === 'org' && <Button size="sm" variant="ghost" iconOnly icon={<RotateCcw size={14} />} aria-label={`Reset ${p.model} to default`} title="Back to default" disabled={busy} onClick={() => void reset(p)} />}
      </span>) }] : []),
  ];

  return (
    <Drawer open={open} onClose={onClose} width={760} title="Model prices" subtitle="USD per 1M tokens · used to estimate cost; provider prices change, so every cost is labelled “est.”"
      actions={admin && !draft ? <Button size="sm" icon={<Plus size={14} />} onClick={() => setDraft(draftOf())}>Add price</Button> : undefined}>
      {!admin && <Callout icon={<ShieldAlert size={16} />}>Read-only. {denied('manage')} to change prices.</Callout>}
      {err && <Callout icon={<ShieldAlert size={16} />}>{err}</Callout>}
      {unknown.length > 0 && (
        <DrawerSection title="Models without a price" aside={`${unknown.length}`}>
          <div className="list">
            {unknown.map((m) => (
              <div key={`${m.provider}|${m.model}`} className="list__row">
                <div className="list__main"><ModelCell provider={m.provider} model={m.model} /><span className="list__meta">{compact(m.calls)} calls · cost not counted</span></div>
                {admin && <Button size="sm" onClick={() => setDraft(draftOf({ provider: priceKey(m.provider, m.model).family, model: priceKey(m.provider, m.model).model }))}>Set price</Button>}
              </div>
            ))}
          </div>
        </DrawerSection>
      )}
      {draft && admin && (
        <DrawerSection title={prices.some((p) => p.provider === draft.provider && p.model === draft.model && p.source === 'org') ? 'Edit override' : 'New org price'}>
          <form className="ai-price-form" onSubmit={(e) => { e.preventDefault(); void save(); }}>
            <Field label="Provider" hint="Price family: Gemini / Vertex AI → google, Azure OpenAI → openai, Bedrock → the model's vendor">
              <Select value={draft.provider} onChange={(e) => setDraft({ ...draft, provider: e.target.value })}
                options={[...new Set([...AI_PRICE_FAMILIES, draft.provider])].map((p) => ({ value: p, label: `${aiProviderLabel(p)} (${p})` }))} />
            </Field>
            <Field label="Model" hint="Exact name or a prefix — the longest matching prefix wins">
              <Input mono value={draft.model} placeholder="gpt-5-mini" onChange={(e) => setDraft({ ...draft, model: e.target.value })} />
            </Field>
            <Field label="Input · $ / 1M tokens"><Input type="number" min={0} step="any" value={draft.input} onChange={(e) => setDraft({ ...draft, input: e.target.value })} /></Field>
            <Field label="Output · $ / 1M tokens"><Input type="number" min={0} step="any" value={draft.output} onChange={(e) => setDraft({ ...draft, output: e.target.value })} /></Field>
            <Field label="Cache read · $ / 1M" hint="Optional — input price when empty"><Input type="number" min={0} step="any" value={draft.cacheRead} onChange={(e) => setDraft({ ...draft, cacheRead: e.target.value })} /></Field>
            <Field label="Cache write · $ / 1M" hint="Optional — input price when empty"><Input type="number" min={0} step="any" value={draft.cacheWrite} onChange={(e) => setDraft({ ...draft, cacheWrite: e.target.value })} /></Field>
            <div className="ai-price-form__actions">
              <Button variant="ghost" onClick={() => setDraft(null)}>Cancel</Button>
              <Button variant="primary" type="submit" disabled={!valid || busy}>{busy ? 'Saving…' : 'Save price'}</Button>
            </div>
          </form>
        </DrawerSection>
      )}
      <DrawerSection title="Price table" aside={`${prices.length} models`}>
        <Card flush>
          <Table rows={prices} rowKey={(p) => `${p.provider}|${p.model}`} defaultSort={{ key: 'model', dir: 'asc' }} columns={columns}
            empty={<EmptyState icon={<Coins />} title="No prices yet" />} />
        </Card>
      </DrawerSection>
    </Drawer>
  );
}

// ---------------------------------------------------------------- setup (no LLM data yet)

type SdkId = 'node' | 'go' | 'jvm' | 'dotnet' | 'python';
const SETUP: Record<SdkId, { label: string; note: ReactNode; code: string }> = {
  node: {
    label: 'Node.js',
    note: <>Wrap the client once; streaming responses are counted too (tokens + time to first token). Vercel AI SDK calls with <span className="mono">experimental_telemetry: {'{ isEnabled: true }'}</span> are picked up as they are.</>,
    code: `import OpenAI from 'openai';
import Anthropic from '@anthropic-ai/sdk';
import { init, instrumentOpenAI, instrumentAnthropic } from '@trooper/node';

init({ service: 'support-bot', llm: { captureContent: false } });

const openai = instrumentOpenAI(new OpenAI());
const anthropic = instrumentAnthropic(new Anthropic());`,
  },
  go: {
    label: 'Go',
    note: <><span className="mono">llm.HTTPClient</span> recognises the provider hosts and reads model and tokens from the request / response JSON without breaking SSE streams. Other clients: <span className="mono">llm.Start(ctx, llm.Request{'{…}'})</span> + <span className="mono">call.End(llm.Usage{'{…}'})</span>.</>,
    code: `import (
    "github.com/openai/openai-go"
    "github.com/openai/openai-go/option"
    "github.com/tasktrooper/telemetry-go/llm"
)

client := openai.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))
// Anthropic: anthropic.NewClient(option.WithHTTPClient(llm.HTTPClient(nil)))`,
  },
  jvm: {
    label: 'JVM',
    note: <>Spring AI and LangChain4j already emit OTel GenAI spans — the Trooper agent forwards them. For the Anthropic / OpenAI Java SDKs wrap the client.</>,
    code: `// Spring AI / LangChain4j: nothing to add with the Trooper Java agent
// -javaagent:trooper-agent.jar -Dtrooper.service=support-bot

// Anthropic / OpenAI Java SDK
var client = TrooperLlm.wrap(AnthropicOkHttpClient.fromEnv());`,
  },
  dotnet: {
    label: '.NET',
    note: <>The SDK listens to the <span className="mono">Microsoft.Extensions.AI</span> ActivitySource by default; add <span className="mono">UseOpenTelemetry()</span> to the chat client pipeline.</>,
    code: `builder.Services.AddTrooper(o => o.Service = "support-bot");

builder.Services.AddChatClient(new OpenAIClient(key).GetChatClient("gpt-5-mini").AsIChatClient())
    .UseOpenTelemetry();`,
  },
  python: {
    label: 'Python',
    note: <>There is no Trooper Python SDK — use OpenLLMetry (or any OTel GenAI instrumentation) and send OTLP to the ingest endpoint with an ingest key.</>,
    code: `pip install traceloop-sdk

from traceloop.sdk import Traceloop
Traceloop.init(app_name="support-bot", disable_batch=False)

# OTEL_EXPORTER_OTLP_ENDPOINT=https://ingest.trooper.dev
# OTEL_EXPORTER_OTLP_HEADERS=Authorization=Bearer <ingest key>
# TRACELOOP_TRACE_CONTENT=false   # prompts off by default`,
  },
};

/** Empty state: no LLM spans in the range — how to instrument per SDK. */
function AiSetup({ scoped, onPrices }: { scoped: boolean; onPrices: () => void }) {
  const [sdk, setSdk] = useState<SdkId>('node');
  const s = SETUP[sdk];
  return (
    <Card>
      <EmptyState icon={<BotMessageSquare />} title={scoped ? 'No LLM calls from this service in this range' : 'No LLM calls yet'}
        action={<Button variant="ghost" icon={<Coins size={14} />} onClick={onPrices}>Model prices</Button>}>
        Calls to OpenAI, Anthropic, Gemini, Bedrock and other providers show up here as soon as a service reports OpenTelemetry GenAI spans:
        model, tokens, estimated cost, errors and latency. Prompts and completions are only collected when you turn content capture on.
      </EmptyState>
      <div className="stack" style={{ maxWidth: 'calc(var(--space-12) * 16)', margin: '0 auto', gap: 'var(--space-3)' }}>
        <Segmented label="SDK" value={sdk} onChange={setSdk} options={(Object.keys(SETUP) as SdkId[]).map((id) => ({ id, label: SETUP[id].label }))} />
        <p className="secondary" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>{s.note}</p>
        <CopyCode>{s.code}</CopyCode>
        <p className="muted" style={{ margin: 0, fontSize: 'var(--text-sm)' }}>
          Content capture: <span className="mono">TROOPER_LLM_CAPTURE_CONTENT=true</span> (or the SDK's <span className="mono">captureContent</span> option). Off by default.
        </p>
      </div>
    </Card>
  );
}

// ---------------------------------------------------------------- trace detail: GenAI span block

/** Model, tokens, cost and TTFT of an LLM span, with its prompt in a drawer (trace detail span panel). */
export function LlmSpanSection({ traceId, spanId, llm }: { traceId: string; spanId: string; llm: TraceLlm }) {
  const [open, setOpen] = useState(false);
  return (
    <section className="drawer__section">
      <div className="drawer__section-head"><h3>LLM call</h3><span className="muted">{aiProviderLabel(llm.provider)}</span></div>
      <KeyValues items={[
        ['Model', <span key="m" className="mono">{llm.model}</span>],
        ['Operation', llm.operation],
        ['Tokens in → out', `${int(llm.inputTokens)} → ${int(llm.outputTokens)}`],
        ['Cost', llm.costUsd === null ? <span key="c" className="muted">price unknown</span> : <span key="c">{aiUsd(llm.costUsd)} <span className="muted">est.</span></span>],
        ['Time to first token', llm.ttftMs !== undefined ? ms(llm.ttftMs) : <span key="t" className="muted">not streamed</span>],
      ]} />
      <div className="row trace__links" style={{ marginTop: 'var(--space-2)' }}>
        <a href="#" className="row" onClick={(e) => { e.preventDefault(); setOpen(true); }}>
          <MessageSquareText size={14} aria-hidden />{llm.hasContent ? 'View prompt & completion' : 'Call details'}
        </a>
        <Link to="/ai" className="row"><BotMessageSquare size={14} aria-hidden />AI monitoring</Link>
      </div>
      <AiCallDrawer target={open ? { traceId, spanId } : null} onClose={() => setOpen(false)} />
    </section>
  );
}
