import { defineConfig, type Plugin } from 'vite'
import vue from '@vitejs/plugin-vue'
import AutoImport from 'unplugin-auto-import/vite'
import Components from 'unplugin-vue-components/vite'
import { ElementPlusResolver } from 'unplugin-vue-components/resolvers'
import fs from 'node:fs'
import path from 'node:path'

/**
 * 保证 embed 锚点 .gitkeep 在每次 build 之后仍然存在。
 *
 * 为什么需要它（2026-10-26 修复）：
 *   internal/interface/web/embed.go 用 `//go:embed all:dist` 把前端产物编进二进制。
 *   go:embed 要求模式至少匹配一个文件，否则编译期直接报
 *   "pattern all:dist: no matching files found" —— 干净克隆直接 go build 失败。
 *
 *   而 vite 的 emptyOutDir: true 会在 build 前清空 outDir，
 *   把 .gitkeep 一起删掉（.gitkeep 只是普通文件，vite 不认识它）。
 *   结果：构建一次后锚点就没了 → 后续 git add 抓不到它 → 干净克隆编译失败。
 *
 * emptyOutDir 必须保持 true：产物文件名带 hash，清空目录才能避免上一轮的
 * 旧 chunk 残留进二进制（白白增大体积）。所以修法不是关掉 emptyOutDir，
 * 而是在 build 之后把锚点补回来。
 */
function embedAnchorPlugin(outDir: string): Plugin {
  // outDir 是相对 vite root（即 web/）的路径，运行时再解析成绝对路径，
  // 避免依赖 process.cwd()（npm scripts 的 cwd 不一定等于 vite root）。
  let resolvedOutDir = ''
  return {
    name: 'plcbridge-embed-anchor',
    configResolved(config) {
      resolvedOutDir = path.resolve(config.root, outDir)
    },
    closeBundle() {
      if (!resolvedOutDir) {
        this.error('embed anchor plugin: outDir was not resolved')
      }
      const anchor = path.resolve(resolvedOutDir, '.gitkeep')
      try {
        fs.mkdirSync(path.dirname(anchor), { recursive: true })
        if (!fs.existsSync(anchor)) {
          fs.writeFileSync(anchor, '', 'utf8')
        }
      } catch (err) {
        // 锚点补不回来会导致下一次 go build 失败，必须让构建方看到。
        this.error(`failed to restore embed anchor ${anchor}: ${err}`)
      }
    },
  }
}

export default defineConfig({
  plugins: [
    vue(),
    AutoImport({ resolvers: [ElementPlusResolver()] }),
    Components({ resolvers: [ElementPlusResolver()] }),
    embedAnchorPlugin('../internal/interface/web/dist'),
  ],
  resolve: {
    alias: {
      '@': path.resolve(__dirname, 'src')
    }
  },
  server: {
    port: 5173,
    proxy: {
      '/api': {
        target: 'http://127.0.0.1:8080',
        changeOrigin: false
      }
    }
  },
  build: {
    outDir: '../internal/interface/web/dist',
    emptyOutDir: true,
    target: 'es2020',
    chunkSizeWarningLimit: 1024,
    rollupOptions: {
      output: {
        // 拆分 vendor 把 axios / vue / pinia 拆为独立 chunk；
        // Element Plus 改为按需导入（由 unplugin-vue-components 处理），不再独立成大 chunk，
        // 避免 manifest entry 全部走 element-plus-vendor 单一文件。
        manualChunks: {
          'vue-vendor': ['vue', 'vue-router', 'pinia'],
          'axios-vendor': ['axios']
        }
      }
    }
  }
})
