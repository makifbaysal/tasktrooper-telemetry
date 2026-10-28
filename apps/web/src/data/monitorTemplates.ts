/**
 * Ready-made infra / serverless / LLM monitors (contracts §20, §21, §25). Api: `GET /monitor-templates`; mock mirrors
 * `services/alerting/internal/eval/templates.go` (the source of truth — keep ids and shapes in sync).
 */
import { isApi, remote } from './remote';

export interface MonitorTemplate {
  id: string;
  name: string;
  description: string;
  kind: 'infra' | 'faas' | 'llm' | (string & {});
  severity: 'critical' | 'warning' | 'info' | (string & {});
  query: Record<string, unknown>;
  /** {op, threshold, window?, forMin?, minRequests?} */
  condition: { op?: string; threshold?: number; window?: string; forMin?: number; minRequests?: number } & Record<string, unknown>;
}

const MOCK: MonitorTemplate[] = [
  { id: 'k8s-pod-crashloop', name: 'Pod restarting (CrashLoop)', description: 'A pod restarted more than 3 times in 10 minutes', kind: 'infra', severity: 'critical',
    query: { entity: 'pod', metric: 'restarts', filter: {} }, condition: { op: '>', threshold: 3, window: '10m', forMin: 0 } },
  { id: 'k8s-node-not-ready', name: 'Node NotReady', description: 'At least one Kubernetes node is NotReady for 5 minutes', kind: 'infra', severity: 'critical',
    query: { entity: 'node', metric: 'not_ready', filter: {} }, condition: { op: '>', threshold: 0, window: '5m', forMin: 5 } },
  { id: 'host-cpu-high', name: 'Host CPU > 90%', description: "A host's CPU is above 90 % for 10 minutes", kind: 'infra', severity: 'warning',
    query: { entity: 'host', metric: 'cpu', agg: 'avg', filter: {} }, condition: { op: '>', threshold: 0.9, window: '5m', forMin: 10 } },
  { id: 'host-disk-full', name: 'Host disk > 90%', description: "A host's fullest filesystem is above 90 %", kind: 'infra', severity: 'warning',
    query: { entity: 'host', metric: 'disk', agg: 'max', filter: {} }, condition: { op: '>', threshold: 0.9, window: '5m', forMin: 5 } },
  { id: 'k8s-workload-unavailable', name: 'Workload has unavailable replicas', description: 'desired − available > 0 for 10 minutes', kind: 'infra', severity: 'warning',
    query: { entity: 'workload', metric: 'unavailable', filter: {} }, condition: { op: '>', threshold: 0, window: '5m', forMin: 10 } },
  { id: 'faas-error-rate', name: 'Function error rate > 5%', description: "A function's errors / invocations is above 5 % for 5 minutes", kind: 'faas', severity: 'critical',
    query: { metric: 'error_rate', filter: {} }, condition: { op: '>', threshold: 0.05, window: '5m', forMin: 5, minRequests: 20 } },
  { id: 'faas-throttles', name: 'Function throttled', description: 'A function was throttled in the last 5 minutes', kind: 'faas', severity: 'warning',
    query: { metric: 'throttles', filter: {} }, condition: { op: '>', threshold: 0, window: '5m', forMin: 5 } },
  { id: 'faas-near-timeout', name: 'Function p95 near timeout', description: "p95 duration above 80 % of the function's timeout", kind: 'faas', severity: 'warning',
    query: { metric: 'duration_p95', relativeToTimeout: true, filter: {} }, condition: { op: '>', threshold: 0.8, window: '5m', forMin: 5 } },
  { id: 'llm-cost-per-hour', name: 'LLM cost above $20/hour', description: 'Estimated LLM spend (priced models) above $20 over the last hour', kind: 'llm', severity: 'warning',
    query: { metric: 'cost_usd_per_hour', filter: {} }, condition: { op: '>', threshold: 20, window: '1h', forMin: 0 } },
  { id: 'llm-error-rate', name: 'LLM error rate > 5%', description: 'More than 5 % of LLM calls failed over 15 minutes (at least 20 calls)', kind: 'llm', severity: 'critical',
    query: { metric: 'error_rate', filter: {} }, condition: { op: '>', threshold: 0.05, window: '15m', forMin: 0, minRequests: 20 } },
  { id: 'llm-p95-slow', name: 'LLM p95 > 20 s', description: 'p95 LLM call duration above 20 seconds over 15 minutes', kind: 'llm', severity: 'warning',
    query: { metric: 'p95_ms', filter: {} }, condition: { op: '>', threshold: 20000, window: '15m', forMin: 0, minRequests: 20 } },
  { id: 'llm-token-spike', name: 'Token spike', description: 'More than 5M tokens (input + output) in an hour', kind: 'llm', severity: 'warning',
    query: { metric: 'tokens', filter: {} }, condition: { op: '>', threshold: 5000000, window: '1h', forMin: 0 } },
];

/** All templates; api falls back to the built-in list while loading or when the endpoint is missing. */
export function monitorTemplates(kind?: string): MonitorTemplate[] {
  const all = isApi ? remote<MonitorTemplate[]>('/monitor-templates', MOCK, (r: MonitorTemplate[] | null) => (Array.isArray(r) && r.length ? r : MOCK)) : MOCK;
  return kind ? all.filter((t) => t.kind === kind) : all;
}
