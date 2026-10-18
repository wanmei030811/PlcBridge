<script setup lang="ts">
import { ref, onMounted, onUnmounted } from 'vue'
import { useRouter } from 'vue-router'
import { ElMessage } from 'element-plus'
import { me, changePassword } from '@/api/auth'
import { useAuthStore } from '@/stores/auth'
import IconButton from '@/components/IconButton.vue'

const auth = useAuthStore()
const router = useRouter()
const username = ref('')
const oldPwd = ref('')
const newPwd = ref('')
const newPwd2 = ref('')
const loading = ref(false)

// 保存 setTimeout handle，组件卸载时清理，避免跨页面强制跳登录
let logoutTimer: ReturnType<typeof setTimeout> | null = null

async function refresh() {
  try {
    const m = await me()
    username.value = m.username
  } catch {
    username.value = auth.username || ''
  }
}
onMounted(refresh)
onUnmounted(() => {
  if (logoutTimer) {
    clearTimeout(logoutTimer)
    logoutTimer = null
  }
})

async function onSubmit() {
  if (newPwd.value.length < 8) { ElMessage.warning('新密码至少 8 位'); return }
  if (newPwd.value !== newPwd2.value) { ElMessage.warning('两次新密码不一致'); return }
  loading.value = true
  try {
    await changePassword(oldPwd.value, newPwd.value)
    ElMessage.success('密码已更新。请重新登录。')
    oldPwd.value = ''; newPwd.value = ''; newPwd2.value = ''
    logoutTimer = setTimeout(async () => {
      logoutTimer = null
      try { await auth.logout() } catch { /* ignore, 将强制跳登录 */ }
      // 用 router 跳转（保留 SPA 状态），而不是 location.href
      router.replace({ name: 'login' })
    }, 800)
  } catch (e: any) {
    ElMessage.error(e?.response?.data?.error || e?.message || '修改失败')
  } finally { loading.value = false }
}
</script>

<template>
  <div>
    <h2>账号 / 密码</h2>
    <el-card style="max-width:480px; margin-top:16px">
      <el-form label-width="120px">
        <el-form-item label="账号"><el-input v-model="username" disabled /></el-form-item>
        <el-form-item label="当前密码"><el-input v-model="oldPwd" type="password" show-password autocomplete="current-password" /></el-form-item>
        <el-form-item label="新密码"><el-input v-model="newPwd" type="password" show-password autocomplete="new-password" placeholder="至少 8 位，包含字母与数字" /></el-form-item>
        <el-form-item label="确认新密码"><el-input v-model="newPwd2" type="password" show-password autocomplete="new-password" /></el-form-item>
        <el-form-item>
          <IconButton name="check" type="primary" tip="修改密码" :loading="loading" @click="onSubmit" />
        </el-form-item>
      </el-form>
    </el-card>
  </div>
</template>