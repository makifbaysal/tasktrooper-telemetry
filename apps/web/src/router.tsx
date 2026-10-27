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
import { SettingsScreen } from './screens/Settings';
import { Integrations } from './screens/Integrations';
import { DesignSystem } from './screens/DesignSystem';

export const router = createBrowserRouter([
  {
    element: <AppShell />,
    children: [
      { path: '/', element: <Overview /> },
      { path: '/services', element: <Services /> },
      { path: '/services/:serviceId', element: <ServiceDetail /> },
      { path: '/services/:serviceId/endpoints/:endpointId', element: <EndpointDetail /> },
      { path: '/kpis', element: <Kpis /> },
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
      { path: '/code', element: <CodeInsights /> },
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
  return <Navigate to={svc ? `/services/${svc}?tab=logs` : '/services'} replace />;
}
