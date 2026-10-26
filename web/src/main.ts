import { createApp } from 'vue'
import { createPinia } from 'pinia'
import App from './App.vue'
import router from './router'
import './assets/global.css'

// Element Plus 通过 unplugin-vue-components 的 ElementPlusResolver 按需自动导入。
// 注意：ElMessage / ElMessageBox / ElNotification / ElLoading 是函数式 API，
// unplugin-vue-components 不会为它们自动 import CSS，会导致 position: static
// 等样式缺失（消息浮在最下方 / 没有遮罩等）。这里显式 import 它们的 CSS。
import 'element-plus/theme-chalk/el-message.css'

const app = createApp(App)
app.use(createPinia())
app.use(router)
app.mount('#app')