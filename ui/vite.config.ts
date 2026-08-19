import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// base './' so the bundle works when served from the embedded filesystem.
export default defineConfig({
  plugins: [react()],
  base: './',
  build: { outDir: 'dist' },
});
