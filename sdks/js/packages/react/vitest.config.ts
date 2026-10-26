import { fileURLToPath } from 'node:url';
import { defineConfig } from 'vitest/config';
import { alias } from '../../vitest.shared.mjs';

const mock = (p: string) => fileURLToPath(new URL(`./test/mocks/${p}`, import.meta.url));

export default defineConfig({
  resolve: { alias: { ...alias, 'next/navigation': mock('next-navigation.ts'), 'next/router': mock('next-router.ts') } },
  test: { environment: 'happy-dom' },
});
