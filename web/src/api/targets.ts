import { apiGet, apiPost, apiPut, apiDelete, ListResp } from './client'
import type { Target } from './devices'

export function listTargets() { return apiGet<ListResp<Target>>('/targets') }
export function getTarget(id: number) { return apiGet<Target>(`/targets/${id}`) }
export function createTarget(t: Partial<Target>) { return apiPost<Target>('/targets', t) }
export function updateTarget(id: number, t: Partial<Target>) { return apiPut<Target>(`/targets/${id}`, t) }
export function deleteTarget(id: number) { return apiDelete<{ ok: boolean }>(`/targets/${id}`) }
export function setTargetPoints(id: number, point_biz_ids: string[]) {
  return apiPut<Target>(`/targets/${id}/points`, { point_biz_ids })
}
