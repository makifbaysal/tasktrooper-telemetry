import { defineConfig } from 'tsup';

export default defineConfig({
  entry: {
    index: 'src/index.tsx',
    'react-router': 'src/react-router.tsx',
    nextjs: 'src/nextjs.tsx',
    'tanstack-router': 'src/tanstack-router.ts',
  },
  format: ['esm', 'cjs'],
  dts: true,
  sourcemap: true,
  clean: true,
  target: 'es2020',
  // Hooks/context/class components: mark as client modules for React Server Components (Next.js App Router).
  banner: { js: '"use client";' },
});
