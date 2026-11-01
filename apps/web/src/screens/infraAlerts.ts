/**
 * Which infra monitors (kind `infra`, contracts §20) belong to a host, database or Kubernetes cluster page. Infra alerts
 * live under the item they watch (product.md § One place rule); a rule without that filter applies to every item, so it
 * shows on each of them.
 */
import type { ApiMonitor } from '@/data/api/ops';
import type { AlertScope } from './Alerts';

const isInfra = (m: ApiMonitor) => (m.apiKind ?? m.kind) === 'infra';
const entityOf = (m: ApiMonitor) => (typeof m.query?.entity === 'string' ? m.query.entity : '');
const filterOf = (m: ApiMonitor, k: string) => {
  const f = m.query?.filter;
  const v = f && typeof f === 'object' ? (f as Record<string, unknown>)[k] : undefined;
  return typeof v === 'string' ? v : '';
};
/** unset filter = every item */
const fits = (m: ApiMonitor, k: string, v: string | undefined) => { const f = filterOf(m, k); return !f || f === v; };
const editor = (params: Record<string, string>, back: string) =>
  `/alerts/new?${new URLSearchParams({ kind: 'infra', ...params, back }).toString()}`;

export function databaseAlertScope(db: { id: string; name: string }): AlertScope {
  return {
    label: db.name,
    match: (m) => isInfra(m) && entityOf(m) === 'database' && fits(m, 'database', db.id),
    newHref: editor({ entity: 'database', database: db.id }, `/infrastructure/databases/${encodeURIComponent(db.id)}`),
  };
}

export function hostAlertScope(host: { id: string; cluster?: string; node?: string }): AlertScope {
  return {
    label: host.id,
    match: (m) => {
      if (!isInfra(m) || !fits(m, 'cluster', host.cluster)) return false;
      if (entityOf(m) === 'host') return fits(m, 'host', host.id);
      return entityOf(m) === 'node' && !!host.node && fits(m, 'host', host.node);
    },
    newHref: editor({ entity: 'host', host: host.id }, `/infrastructure/hosts/${encodeURIComponent(host.id)}`),
  };
}

const CLUSTER_ENTITIES = new Set(['node', 'cluster', 'pod', 'container', 'workload']);

export function clusterAlertScope(cluster: string): AlertScope {
  return {
    label: cluster,
    match: (m) => isInfra(m) && (CLUSTER_ENTITIES.has(entityOf(m)) ? fits(m, 'cluster', cluster) : entityOf(m) === 'host' && filterOf(m, 'cluster') === cluster),
    newHref: editor({ entity: 'cluster', cluster }, `/infrastructure/kubernetes/${encodeURIComponent(cluster)}?tab=alerts`),
  };
}
