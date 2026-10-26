import { fileURLToPath } from 'node:url';

const pkg = (p) => fileURLToPath(new URL(`./packages/${p}`, import.meta.url));

/** Resolve sibling workspace packages to their sources so tests never depend on a prior build. */
export const alias = {
  '@trooper/core': pkg('core/src/index.ts'),
  '@trooper/browser': pkg('browser/src/index.ts'),
  '@trooper/react': pkg('react/src/index.ts'),
};
