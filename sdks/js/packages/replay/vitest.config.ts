import { defineConfig } from 'vitest/config';
import { alias } from '../../vitest.shared.mjs';

export default defineConfig({ resolve: { alias }, test: { environment: 'happy-dom' } });
