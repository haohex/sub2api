import { apiClient } from '../client'

export interface StatePoolConfig {
  proxy_id: number
  proxy_name: string
  available: boolean
  issue: string
}
export interface StatePoolRow {
  account_id: number
  account_name: string
  account_status: string
  plan: string
  model: string
  enabled: boolean
  expected_length: number
  state_length: number
  state_status: 'valid' | 'expired' | 'missing' | 'invalid' | 'unknown_plan' | 'disabled'
  obtained_at?: string
  expires_at?: string
  probe_status: 'idle' | 'queued' | 'running' | 'stopped' | 'blocked'
  attempts: number
  last_attempt_at?: string
  reason: string
  can_refresh: boolean
}
export interface StatePoolResponse {
  server_time: string
  config: StatePoolConfig
  models: string[]
  items: StatePoolRow[]
}
export async function getStatePool(signal?: AbortSignal): Promise<StatePoolResponse> {
  const { data } = await apiClient.get<StatePoolResponse>('/admin/accounts/codex-state-pool', { signal })
  return data
}
export async function saveStatePoolProxy(proxyId: number): Promise<StatePoolConfig> {
  const { data } = await apiClient.put<StatePoolConfig>('/admin/accounts/codex-state-pool/config', { proxy_id: proxyId })
  return data
}
export async function refreshState(row: StatePoolRow): Promise<void> {
  await apiClient.post(`/admin/accounts/${row.account_id}/codex-turn-state-probe/refresh`, { model: row.model })
}
export async function getStateValue(row: StatePoolRow): Promise<string> {
  const { data } = await apiClient.get<{ state: string }>(`/admin/accounts/${row.account_id}/codex-turn-state-probe/state`, { params: { model: row.model } })
  return data.state
}
