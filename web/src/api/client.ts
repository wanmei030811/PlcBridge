import axios, { AxiosInstance, AxiosRequestConfig } from 'axios'
import { ElMessage } from 'element-plus'

const http: AxiosInstance = axios.create({
  baseURL: '/api/v1',
  withCredentials: true,
  timeout: 15000
})

http.interceptors.response.use(
  (resp) => resp.data,
  (err) => {
    const status = err?.response?.status
    const msg = err?.response?.data?.error || err.message || '请求失败'
    // 401 跳登录页：
    //   - 排除 /me：router beforeEach 在每个跳转都会调 refresh()→me()，
    //     失败时若再 location.href=/login 会导致循环刷新。
    //   - 排除已经在 /login 的页面。
    const isMeCheck = err?.config?.url?.endsWith('/me') === true
    if (status === 401 && !isMeCheck && location.pathname !== '/login') {
      const next = encodeURIComponent(location.pathname + location.search)
      location.href = '/login?next=' + next
    } else if (status !== 401) {
      ElMessage.error(msg)
    }
    return Promise.reject(err)
  }
)

export interface ListResp<T> {
  items: T[]
}

export async function apiGet<T = any>(url: string, config?: AxiosRequestConfig): Promise<T> {
  return http.get(url, config) as unknown as T
}
export async function apiPost<T = any>(url: string, data?: any, config?: AxiosRequestConfig): Promise<T> {
  return http.post(url, data, config) as unknown as T
}
export async function apiPut<T = any>(url: string, data?: any): Promise<T> {
  return http.put(url, data) as unknown as T
}
export async function apiDelete<T = any>(url: string): Promise<T> {
  return http.delete(url) as unknown as T
}

export default http
