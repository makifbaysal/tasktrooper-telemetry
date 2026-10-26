import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';
import { alias } from '../../vitest.shared.mjs';

export default defineConfig({
  resolve: { alias: { ...alias, 'react-native': fileURLToPath(new URL('./test/mocks/react-native.ts', import.meta.url)) } },
  test: { environment: 'node' },
});
