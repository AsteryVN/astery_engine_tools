import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// Tauri 2 expects a fixed dev port; 1420 is the documented default.
// strictPort makes Vite fail loudly when the port is taken instead of
// silently shifting and breaking `cargo tauri dev` window boot.
export default defineConfig({
  plugins: [react()],
  clearScreen: false,
  server: {
    port: 1420,
    strictPort: true,
    host: '127.0.0.1',
    hmr: {
      protocol: 'ws',
      host: '127.0.0.1',
      port: 1421,
    },
  },
  envPrefix: ['VITE_', 'TAURI_'],
  build: {
    target: 'es2022',
    sourcemap: true,
    minify: 'esbuild',
  },
});
