<script setup lang="ts">
import { ref } from 'vue'
import { useRouter, useRoute } from 'vue-router'
import { ElMessage } from 'element-plus'
import { useAuthStore } from '@/stores/auth'
import Icon from '@/components/Icon.vue'

const router = useRouter()
const route = useRoute()
const auth = useAuthStore()

const username = ref('admin')
const password = ref('')
const loading = ref(false)

async function onSubmit() {
  if (!username.value || !password.value) {
    ElMessage.warning('请输入用户名和密码')
    return
  }
  loading.value = true
  try {
    await auth.login(username.value, password.value)
    // 登录成功后回跳到原路径（仅接受站内相对路径，防止开放重定向）
    const raw = (route.query.next as string) || (route.query.redirect as string) || '/dashboard'
    const safe = safeNextPath(raw)
    router.replace(safe)
  } catch {
    // 错误已由 axios 拦截器弹 ElMessage
  } finally {
    loading.value = false
  }
}

function safeNextPath(raw: string): string {
  // 仅允许站内相对路径（以 / 开头，不含 // 与协议头）
  if (typeof raw !== 'string') return '/dashboard'
  let p = raw
  try { p = decodeURIComponent(raw) } catch { /* keep raw */ }
  if (!p.startsWith('/') || p.startsWith('//')) return '/dashboard'
  if (/^\/[^/]*:\/\//.test(p)) return '/dashboard'
  return p
}
</script>

<template>
  <div class="login-bg">
    <el-card class="login-card" shadow="always">
      <template #header>
        <div style="text-align:center; font-size:22px; font-weight:600">PlcBridge Web</div>
      </template>
      <el-form @submit.prevent="onSubmit">
        <el-form-item label="账号">
          <el-input v-model="username" autocomplete="username" />
        </el-form-item>
        <el-form-item label="密码">
          <el-input v-model="password" type="password" show-password autocomplete="current-password" />
        </el-form-item>
        <el-button type="primary" :loading="loading" class="login-submit" native-type="submit">
          <Icon name="log-in" :size="18" />
          <span style="margin-left:8px">登录</span>
        </el-button>
      </el-form>
      <div style="margin-top:12px; font-size:12px; color:#888; text-align:center">
        首次启动时，初始管理员密码会写入运行日志。
      </div>
    </el-card>
  </div>
</template>

<style scoped>
.login-bg {
  height: 100vh;
  display: flex;
  justify-content: center;
  align-items: center;
  background: linear-gradient(135deg, #19876c 0%, #1d4f8b 100%);
}
.login-card { width: 400px; }
.login-submit { width: 100%; }
</style>
