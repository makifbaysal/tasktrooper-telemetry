# @trooper/react

```tsx
import { TrooperProvider, ErrorBoundary, useTrooper } from '@trooper/react';

<TrooperProvider options={{ dsn, app: 'shop-web', version }}>
  <ErrorBoundary name="root" fallback={({ error, resetError }) => <Crash onRetry={resetError} />}>
    <App />
  </ErrorBoundary>
</TrooperProvider>;

const { track, identify, captureError } = useTrooper();
```

- `ErrorBoundary` captures render errors with `attributes.component_stack` (and `error_boundary` = `name`).
- React 19 root callbacks: `createRoot(el, { onUncaughtError: reactErrorHandler('uncaught'), onCaughtError: reactErrorHandler('caught') })`.
- All entry points are marked `"use client"`.

## Router integrations (view name = route template)

Subpath exports; the router packages are optional peer dependencies.

```ts
// react-router v6.4+/v7 data routers
import { trackReactRouter } from '@trooper/react/react-router';
const router = createBrowserRouter(routes);
trackReactRouter(router);                       // "/users/:userId/orders/:orderId"

// declarative <BrowserRouter><Routes> — pass the route config (createRoutesFromElements for JSX)
import { TrooperRoutes } from '@trooper/react/react-router';
<TrooperRoutes routes={routes} />

// TanStack Router
import { trackTanStackRouter } from '@trooper/react/tanstack-router';
trackTanStackRouter(router);                    // "/posts/$postId" → "/posts/:postId"

// Next.js App Router (client component in the root layout)
import { TrooperNextAppRouter } from '@trooper/react/nextjs';
<TrooperNextAppRouter />                         // usePathname + useParams → "/shop/:category/:id"

// Next.js Pages Router (_app.tsx)
import { useNextPagesRouterTracking } from '@trooper/react/nextjs';
useNextPagesRouterTracking();                   // router.pathname "/products/[id]" → "/products/:id"
```

Once an integration reports a route, it owns view changes (History API changes no longer start views on their own).
App Router note: Next.js does not expose the route pattern; it is rebuilt by replacing dynamic param values in the pathname.
