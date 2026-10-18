import { defineStore } from 'pinia'
import { ref } from 'vue'
import { me as meApi, login as loginApi, logout as logoutApi } from '@/api/auth'

export const useAuthStore = defineStore('auth', () => {
  const username = ref<string>('')
  const initialized = ref(false)

  async function refresh() {
    try {
      const m = await meApi()
      username.value = m.username
    } catch {
      username.value = ''
    } finally {
      initialized.value = true
    }
  }

  async function login(usernameVal: string, password: string) {
    await loginApi(usernameVal, password)
    username.value = usernameVal
    initialized.value = true
  }

  async function logout() {
    await logoutApi()
    username.value = ''
  }

  return { username, initialized, refresh, login, logout }
})
