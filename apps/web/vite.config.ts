import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath, URL } from 'node:url';

export default defineConfig({
  plugins: [react()],
  resolve: { alias: { '@': fileURLToPath(new URL('./src', import.meta.url)) } },
  server: { proxy: { '/local-ai': { target: 'http://127.0.0.1:8099', rewrite: (path) => path.replace(/^\/local-ai/, '') } } },
});
