import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// Build straight into the Go embed directory. The compiled assets are committed
// so `go build` works on a machine with no Node toolchain, which spec §3
// requires of a distributable binary.
export default defineConfig({
  plugins: [react()],
  build: {
    outDir: '../internal/webui/dist',
    emptyOutDir: true,
    // No sourcemaps: they would double the size of a binary that ships them.
    sourcemap: false,
  },
  server: {
    port: 5173,
    proxy: {
      // During development the UI runs on Vite and the daemon on 8737. The
      // daemon's CORS allowlist has to include http://localhost:5173 for this,
      // or start it with --allow-origin http://localhost:5173.
      '/api': 'http://127.0.0.1:8737',
      '/openapi.json': 'http://127.0.0.1:8737',
    },
  },
})
