import { defineConfig } from 'tsup';

export default defineConfig([
  {
    entry: ['src/index.ts'],
    format: ['esm', 'cjs'],
    dts: true,
    sourcemap: true,
    clean: true,
    treeshake: true,
    target: 'es2020',
    external: ['@rrweb/record', '@trooper/browser'],
  },
  {
    // Script-tag build: rrweb bundled, exposed as `window.TrooperReplay`. Load after trooper.iife.js:
    // Trooper.init({ ..., integrations: [TrooperReplay.replayIntegration()] }).
    entry: { 'trooper-replay.iife': 'src/index.ts' },
    format: ['iife'],
    globalName: 'TrooperReplay',
    minify: true,
    sourcemap: true,
    target: 'es2020',
    noExternal: [/.*/],
    outExtension: () => ({ js: '.js' }),
  },
]);
