import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// In development the Vite server proxies API calls to the Go backend; in
// Docker the same paths are proxied by nginx (see nginx.conf).
const api = process.env.VITE_API_PROXY ?? 'http://localhost:8080'

export default defineConfig({
  plugins: [vue()],
  server: {
    port: 3000,
    proxy: { '/api': api, '/health': api },
  },
})
