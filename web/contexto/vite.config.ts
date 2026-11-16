import { defineConfig } from 'vite'
import react from '@vitejs/plugin-react'

// La web se compila dentro del paquete Go (internal/contexto/web/dist) y se embebe en el binario.
export default defineConfig({
  plugins: [react()],
  build: { outDir: '../../internal/contexto/web/dist', emptyOutDir: false, chunkSizeWarningLimit: 1200 },
  server: { proxy: { '/api': 'http://localhost:8080' } },
})
