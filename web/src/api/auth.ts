import { apiGet, apiPost, apiDelete, ListResp } from './client'

export interface Token {
  id: number
  name: string
  revoked_at?: string
  created_at: string
}

// createToken 响应：包含一次性明文 token（后端不再回显完整 token 给 list 接口）
export interface CreatedToken {
  id: number
  token: string
  name: string
}

export function listTokens() { return apiGet<ListResp<Token>>('/tokens') }
export function createToken(name: string) { return apiPost<CreatedToken>('/tokens', { name }) }
export function revokeToken(id: number) { return apiDelete<{ ok: boolean }>(`/tokens/${id}`) }

export interface AccountInfo { username: string }

export function login(username: string, password: string) {
  return apiPost<AccountInfo>('/auth/login', { username, password })
}
export function logout() { return apiPost<{ ok: boolean }>('/auth/logout', {}) }
export function me() { return apiGet<AccountInfo>('/auth/me') }
export function changePassword(old_password: string, new_password: string) {
  return apiPost<{ ok: boolean }>('/auth/password', { old_password, new_password })
}
