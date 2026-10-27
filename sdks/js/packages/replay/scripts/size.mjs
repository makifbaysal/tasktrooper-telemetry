// Bundle-size report for @trooper/replay, min + gzip. Our own code is budgeted (< 5 KB gzip); rrweb is reported.
import { build } from 'esbuild';
import { readFileSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { gzipSync } from 'node:zlib';

const BUDGET = 5 * 1024;
const here = (p) => fileURLToPath(new URL(p, import.meta.url));

async function measure(name, contents, external = []) {
  const r = await build({
    stdin: { contents, resolveDir: here('..'), loader: 'ts' },
    bundle: true,
    minify: true,
    format: 'esm',
    target: 'es2020',
    write: false,
    external,
    treeShaking: true,
  });
  const code = r.outputFiles[0].contents;
  return { name, min: code.length, gzip: gzipSync(code, { level: 9 }).length };
}

const own = await measure('@trooper/replay (rrweb external)', `export * from './src/index.ts';`, ['@rrweb/record']);
const rows = [own, await measure('@trooper/replay + @rrweb/record', `export * from './src/index.ts';`)];
try {
  const iife = readFileSync(here('../dist/trooper-replay.iife.js'));
  rows.push({ name: 'dist/trooper-replay.iife.js', min: iife.length, gzip: gzipSync(iife, { level: 9 }).length });
} catch {
  /* not built yet */
}

const kb = (n) => (n / 1024).toFixed(2) + ' KB';
console.log('@trooper/replay, minified / gzip -9');
for (const r of rows) console.log(`  ${r.name.padEnd(36)} ${kb(r.min).padStart(10)}  ${kb(r.gzip).padStart(9)}`);
console.log(`own code budget ${kb(BUDGET)} gzip: ${own.gzip <= BUDGET ? 'OK' : 'EXCEEDED'}`);
if (own.gzip > BUDGET) process.exit(1);
