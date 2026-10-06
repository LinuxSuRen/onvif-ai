import { defineConfig } from 'vite'
import vue from '@vitejs/plugin-vue'

// 后端端口跟随 .env 的 PORT（start.sh 会 export），避免硬编码
const backend = `http://localhost:${process.env.PORT || '8080'}`

export default defineConfig({
  plugins: [vue()],
  server: {
    proxy: {
      '/ws': {
        target: backend,
        ws: true,
      },
      '/api': {
        target: backend,
        changeOrigin: true,
      },
    },
  },
})
