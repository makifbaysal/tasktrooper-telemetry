# @trooper/angular

Angular 17+ (standalone).

```ts
bootstrapApplication(AppComponent, {
  providers: [provideRouter(routes), provideTrooper({ dsn, app: 'shop-web', version })],
});
```

- `provideTrooper()` registers `TrooperErrorHandler` as `ErrorHandler` (unwraps zone promise rejections, then logs like
  Angular's default unless `logErrors: false`), initializes the browser SDK and tracks the Router.
- Router: after every `NavigationEnd` the view is named with the route config path template (`users/:id` → `/users/:id`).
- NgModule apps: `providers: [provideTrooper({...})]` in the root module works the same.

**Build trade-off.** This package is built with tsup, not ng-packagr. It contains no decorators or Angular metadata — only plain
classes, an `InjectionToken`, factory providers and `ENVIRONMENT_INITIALIZER` — so it needs no Angular compiler (ngc / linker)
and works with Angular 17–22 DI. The cost: no `@Injectable()`/`NgModule` exports, and `TrooperErrorHandler` is created by a
factory (`new TrooperErrorHandler(options)`) rather than injected with constructor DI.
