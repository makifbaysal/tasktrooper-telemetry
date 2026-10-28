import { Navigate, createBrowserRouter, useSearchParams } from 'react-router';
import { AppShell } from './components/layout/AppShell';
import { Overview } from './screens/Overview';
import { Services } from './screens/Services';
import { ServiceDetail } from './screens/ServiceDetail';
import { EndpointDetail } from './screens/EndpointDetail';
import { KpiEditor } from './screens/KpiEditor';
import { Kpis } from './screens/Kpis';
import { KpiDetail } from './screens/KpiDetail';
import { AlertEditor } from './screens/AlertEditor';
import { Incidents } from './screens/Incidents';
import { ScenarioEditor } from './screens/ScenarioEditor';
import { LoadTests } from './screens/LoadTests';
import { LoadTestRun } from './screens/LoadTestRun';
import { LoadTestDomains } from './screens/LoadTestDomains';
import { CodeInsights } from './screens/CodeInsights';
import { DatabaseDetail, Databases } from './screens/Databases';
import { ClusterDetail, Queues } from './screens/Queues';
import { Hosts } from './screens/Hosts';
import { HostDetail } from './screens/HostDetail';
import { Kubernetes } from './screens/Kubernetes';
import { K8sClusterDetail } from './screens/ClusterDetail';
import { PodDetail } from './screens/PodDetail';
import { Serverless } from './screens/Serverless';
import { FunctionDetail } from './screens/FunctionDetail';
import { SettingsScreen } from './screens/Settings';
import { Integrations } from './screens/Integrations';
import { DesignSystem } from './screens/DesignSystem';
import { TraceDetail } from './screens/TraceDetail';
import { ReplayDetailScreen } from './screens/ReplayDetail';
import { AiMonitoring } from './screens/AiMonitoring';

export const router = createBrowserRouter([
  {
    element: <AppShell />,
    children: [
      { path: '/', element: <Overview /> },
      { path: '/services', element: <Services /> },
      { path: '/services/:serviceId', element: <ServiceDetail /> },
      { path: '/services/:serviceId/endpoints/:endpointId', element: <EndpointDetail /> },
      { path: '/traces/:traceId', element: <TraceDetail /> },
      { path: '/replays/:sessionId', element: <ReplayDetailScreen /> },
      { path: '/kpis', element: <Kpis /> },
      { path: '/ai', element: <AiMonitoring /> },
      { path: '/kpis/new', element: <KpiEditor /> },
      { path: '/kpis/:kpiId', element: <KpiDetail /> },
      // client apps live under Services now
      { path: '/frontend', element: <Navigate to="/services?type=client" replace /> },
      { path: '/logs', element: <LogsRedirect /> },
      { path: '/alerts', element: <Navigate to="/" replace /> },
      { path: '/alerts/new', element: <AlertEditor /> },
      { path: '/incidents', element: <Incidents /> },
      { path: '/load-tests', element: <LoadTests /> },
      { path: '/load-tests/runs/:runId', element: <LoadTestRun /> },
      { path: '/load-tests/domains', element: <LoadTestDomains /> },
      { path: '/load-tests/new', element: <ScenarioEditor /> },
      { path: '/load-tests/scenarios/:scenarioId/edit', element: <ScenarioEditor /> },
      { path: '/infrastructure/databases', element: <Databases /> },
      { path: '/infrastructure/databases/:databaseId', element: <DatabaseDetail /> },
      { path: '/infrastructure/queues', element: <Queues /> },
      { path: '/infrastructure/queues/:clusterId', element: <ClusterDetail /> },
      { path: '/infrastructure/hosts', element: <Hosts /> },
      { path: '/infrastructure/hosts/:hostId', element: <HostDetail /> },
      { path: '/infrastructure/kubernetes', element: <Kubernetes /> },
      { path: '/infrastructure/kubernetes/:cluster', element: <K8sClusterDetail /> },
      { path: '/infrastructure/kubernetes/:cluster/pods/:uid', element: <PodDetail /> },
      { path: '/infrastructure/serverless', element: <Serverless /> },
      { path: '/infrastructure/serverless/:functionId', element: <FunctionDetail /> },
      { path: '/settings/usage', element: <SettingsScreen /> },
      { path: '/code', element: <CodeInsights /> },
      { path: '/ai-playground', element: <Navigate to="/incidents" replace /> },
      { path: '/settings', element: <SettingsScreen /> },
      { path: '/settings/integrations', element: <Integrations /> },
      { path: '/design-system', element: <DesignSystem /> },
    ],
  },
]);

/** Logs live on each service page now; old /logs?service=x links land there. */
function LogsRedirect() {
  const [params] = useSearchParams();
  const svc = params.get('service');
  // keep the search and the server-side filters (?q=, ?pod=, ?namespace=, ?traceId=) on the service's log tab
  const keep = new URLSearchParams({ tab: 'logs' });
  for (const k of ['q', 'pod', 'namespace', 'traceId']) { const v = params.get(k); if (v) keep.set(k, v); }
  return <Navigate to={svc ? `/services/${encodeURIComponent(svc)}?${keep}` : '/services'} replace />;
}
