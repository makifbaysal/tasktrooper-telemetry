/**
 * Preload entry: `node --import @trooper/node/register app.mjs` (ESM) or `node --require @trooper/node/register app.js`
 * (CJS). Registers the ESM loader hook OpenTelemetry needs to patch ESM modules, then calls `init()` from the
 * environment (`TROOPER_DSN`, `TROOPER_SERVICE` …). Never throws.
 *
 * `--import` uses import-in-the-middle's synchronous in-thread `module.registerHooks()` loader where the runtime
 * supports it (`module.register()` is deprecated in Node 26). The `--require` (CJS) entry keeps the asynchronous
 * `module.register()` loader: registering the synchronous hooks during the CJS preload phase breaks `require()` of
 * builtins on Node 26.
 */
import * as nodeModule from 'node:module';
import { recordError } from './errors';
import { init } from './init';

function registerSyncHooks(): boolean {
  try {
    const req = nodeModule.createRequire(import.meta.url);
    const sync = req('import-in-the-middle/register-hooks.mjs') as { register: () => void; supportsSyncHooks: () => boolean };
    if (!sync.supportsSyncHooks()) return false;
    sync.register();
    return true;
  } catch (err) {
    recordError('register', err);
    return false;
  }
}

function registerEsmHook(): void {
  const cjsPreload = import.meta.url.endsWith('.cjs'); // tsup shims import.meta.url in the CJS build
  if (!cjsPreload && registerSyncHooks()) return;
  try {
    const register = (nodeModule as { register?: (specifier: string, parentURL: string) => void }).register;
    if (typeof register === 'function') register('@opentelemetry/instrumentation/hook.mjs', import.meta.url);
  } catch (err) {
    recordError('register', err);
  }
}

registerEsmHook();
init();
