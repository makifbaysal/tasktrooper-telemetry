import { defineConfig, loadEnv } from 'vite';
import react from '@vitejs/plugin-react';
import { execSync } from 'node:child_process';
import { readFileSync } from 'node:fs';
import { fileURLToPath, URL } from 'node:url';

/** RUM release of the dashboard (ADR-027): VITE_TROOPER_VERSION, else "<package version>+<git sha>". */
function webVersion(mode: string): string {
  const explicit = loadEnv(mode, process.cwd(), 'VITE_').VITE_TROOPER_VERSION;
  if (explicit) return explicit;
  const pkg = JSON.parse(readFileSync(new URL('./package.json', import.meta.url), 'utf8')) as { version: string };
  try {
    const sha = execSync('git rev-parse --short HEAD', { stdio: ['ignore', 'pipe', 'ignore'] }).toString().trim();
    return sha ? `${pkg.version}+${sha}` : pkg.version;
  } catch {
    return pkg.version;
  }
}

export default defineConfig(({ mode }) => ({
  plugins: [react()],
  define: { __TT_WEB_VERSION__: JSON.stringify(webVersion(mode)) },
  // TT_WEB_SOURCEMAP=hidden: *.js.map without the sourceMappingURL comment, for `trooper-cli upload-sourcemaps` (deploy/demo/deploy.sh)
  build: { sourcemap: process.env.TT_WEB_SOURCEMAP === 'hidden' ? 'hidden' : false },
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
}));
