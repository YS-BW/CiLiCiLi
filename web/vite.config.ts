import { defineConfig } from 'vite';
import react from '@vitejs/plugin-react';

// 开发时 /api 反代到本地 Go（默认 8080），生产由 nginx 同源反代。
export default defineConfig({
  plugins: [react()],
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: process.env.VITE_API_TARGET || 'http://127.0.0.1:8080',
        changeOrigin: true,
      },
    },
  },
  build: {
    outDir: 'dist',
    sourcemap: false,
    chunkSizeWarningLimit: 900,
  },
});
