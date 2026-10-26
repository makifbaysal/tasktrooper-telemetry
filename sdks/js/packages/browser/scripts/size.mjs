// Bundle-size report for @trooper/core + @trooper/browser (+ web-vitals), min + gzip. Budget: < 15 KB.
import { build } from 'esbuild';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

const BUDGET = 15 * 1024;
const here = (p) => fileURLToPath(new URL(p, import.meta.url));
const core = here('../../core/src/index.ts');

async function measure(name, contents) {
  const r = await build({
    stdin: { contents, resolveDir: here('..'), loader: 'ts' },
    bundle: true,
    minify: true,
    format: 'esm',
    target: 'es2020',
    write: false,
    alias: { '@trooper/core': core },
    treeShaking: true,
  });
  const code = r.outputFiles[0].contents;
  return { name, min: code.length, gzip: gzipSync(code, { level: 9 }).length };
}

const rows = [
  await measure('init only (typical app)', `export { init } from './src/index.ts';`),
  await measure('full public API', `export * from './src/index.ts';`),
];
try {
  const iife = readFileSync(here('../dist/trooper.iife.js'));
  rows.push({ name: 'dist/trooper.iife.js (script tag)', min: iife.length, gzip: gzipSync(iife, { level: 9 }).length });
} catch {
  /* not built yet */
}

const kb = (n) => (n / 1024).toFixed(2) + ' KB';
console.log('core + browser + web-vitals, minified / gzip -9');
for (const r of rows) console.log(`  ${r.name.padEnd(36)} ${kb(r.min).padStart(10)}  ${kb(r.gzip).padStart(9)}`);
const worst = Math.max(...rows.map((r) => r.gzip));
console.log(`budget ${kb(BUDGET)} gzip: ${worst <= BUDGET ? 'OK' : 'EXCEEDED'}`);
if (worst > BUDGET) process.exit(1);
