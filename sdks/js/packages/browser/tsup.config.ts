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
  },
  {
    // Script-tag build: core + web-vitals bundled, exposed as `window.Trooper`.
    entry: { 'trooper.iife': 'src/index.ts' },
    format: ['iife'],
    globalName: 'Trooper',
    minify: true,
    sourcemap: true,
    target: 'es2020',
    noExternal: [/.*/],
    outExtension: () => ({ js: '.js' }),
  },
]);
