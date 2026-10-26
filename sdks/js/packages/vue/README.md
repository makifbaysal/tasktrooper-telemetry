# @trooper/vue

```ts
import { createApp } from 'vue';
import { Trooper } from '@trooper/vue';

const app = createApp(App);
app.use(router);
app.use(Trooper, { dsn, app: 'shop-web', version, router });   // router is optional
```

- `app.config.errorHandler` → ERROR with `attributes.component` and `vue_info`; a previously set handler is still called
  (otherwise the error is logged to the console like Vue does).
- vue-router (v4+): view name = matched record path, custom regexes stripped (`/users/:id(\\d+)` → `/users/:id`).
- `useTrooper()` (inject) returns `track`, `identify`, `captureError`, `setView`, ...
- Pass no `dsn` to skip `init` if you call `init()` from `@trooper/browser` yourself.
