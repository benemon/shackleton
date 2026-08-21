import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// base '/' so asset URLs stay absolute under nested SPA routes; the console
// is always served from the daemon's root.
export default defineConfig({
  plugins: [react()],
  base: '/',
  build: { outDir: 'dist' },
});
