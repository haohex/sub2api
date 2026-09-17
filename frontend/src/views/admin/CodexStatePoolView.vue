<template>
  <AppLayout>
    <div class="space-y-6 p-4 sm:p-6">
      <div class="flex flex-wrap items-start justify-between gap-4">
        <div>
          <h1 class="text-xl font-semibold text-gray-900 dark:text-gray-100">{{ t('admin.accounts.statePool.title') }}</h1>
          <p class="mt-2 max-w-4xl text-sm leading-6 text-gray-500 dark:text-gray-400">{{ t('admin.accounts.statePool.description') }}</p>
        </div>
        <RouterLink to="/admin/accounts" class="btn btn-secondary">{{ t('admin.accounts.statePool.manageAccounts') }}</RouterLink>
      </div>

      <section class="rounded-xl border border-gray-200 bg-white p-4 dark:border-dark-600 dark:bg-dark-800">
        <label class="mb-2 block text-sm font-medium">{{ t('admin.accounts.statePool.globalProxy') }}</label>
        <div class="flex flex-wrap items-center gap-3">
          <div class="w-full sm:w-80"><ProxySelector v-model="proxyId" :proxies="proxies" :disabled="saving" /></div>
          <button class="btn btn-primary" :disabled="saving || !pool || (proxyId || 0) === pool.config.proxy_id" @click="saveProxy">{{ t('common.save') }}</button>
          <span v-if="pool?.config.proxy_name" class="text-sm text-gray-500">{{ pool.config.proxy_name }}</span>
        </div>
        <p class="mt-2 text-xs leading-5 text-gray-500 dark:text-gray-400">{{ t('admin.accounts.statePool.proxyHint') }}</p>
        <p v-if="pool && !pool.config.available" role="status" class="mt-3 rounded-lg bg-amber-50 p-3 text-sm text-amber-800 dark:bg-amber-900/20 dark:text-amber-300">
          {{ pool.config.issue === 'proxy_not_configured' ? t('admin.accounts.statePool.proxyMissing') : t('admin.accounts.statePool.proxyUnavailable') }}
        </p>
      </section>

      <div class="flex flex-wrap items-center gap-3">
        <label class="text-sm" for="state-model">{{ t('admin.accounts.statePool.model') }}</label>
        <select id="state-model" v-model="modelFilter" class="input w-auto min-w-48">
          <option value="">{{ t('admin.accounts.statePool.allModels') }}</option>
          <option v-for="model in modelOptions" :key="model" :value="model">{{ model }}</option>
        </select>
        <input v-model="search" :placeholder="t('admin.accounts.statePool.search')" :aria-label="t('admin.accounts.statePool.search')" class="input w-full sm:w-64" />
        <span class="text-sm text-gray-500">{{ t('admin.accounts.statePool.summary', { valid: validCount, total: visibleRows.length }) }}</span>
        <button class="btn btn-secondary ml-auto" :disabled="loading" @click="loadPool">{{ t('admin.accounts.statePool.reload') }}</button>
      </div>
      <p v-if="error" role="alert" class="rounded-lg bg-red-50 p-3 text-sm text-red-700 dark:bg-red-900/20 dark:text-red-300">{{ error }}</p>
      <div v-if="!pool && loading" class="py-16 text-center text-gray-500">{{ t('common.loading') }}</div>
      <div v-else-if="!visibleRows.length" class="rounded-xl border border-dashed border-gray-300 p-12 text-center text-sm text-gray-500 dark:border-dark-600">
        {{ t('admin.accounts.statePool.empty') }}
        <button v-if="modelFilter" class="mt-3 block w-full text-primary-600 hover:underline" @click="modelFilter = ''">{{ t('admin.accounts.statePool.allModels') }}</button>
      </div>
      <div v-else class="grid gap-5 md:grid-cols-2 xl:grid-cols-3">
        <article v-for="row in visibleRows" :key="rowKey(row)" class="min-w-0 rounded-xl border border-l-4 bg-white p-5 shadow-sm dark:bg-dark-800" :class="isValid(row) ? 'border-gray-200 border-l-emerald-500 dark:border-dark-600 dark:border-l-emerald-500' : 'border-gray-200 border-l-amber-500 dark:border-dark-600 dark:border-l-amber-500'">
          <div class="flex flex-wrap items-start justify-between gap-2">
            <h2 class="break-all font-mono text-base font-semibold">{{ row.model }}</h2>
            <span class="rounded px-2 py-1 text-xs font-medium" :class="isValid(row) ? 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/30 dark:text-emerald-300' : 'bg-amber-100 text-amber-800 dark:bg-amber-900/30 dark:text-amber-300'">{{ stateLabel(row) }}</span>
          </div>
          <p class="mt-2 truncate text-sm text-gray-500" :title="row.account_name">#{{ row.account_id }} · {{ row.account_name }}</p>
          <p class="mt-1 text-xs text-gray-500">{{ row.plan || t('admin.accounts.statePool.unknownPlan') }} · {{ t('admin.accounts.statePool.expectedLength', { length: row.expected_length || '—' }) }}</p>

          <div class="mt-5 rounded-lg bg-gray-50 p-3 dark:bg-dark-900/60">
            <div class="flex items-center justify-between gap-2 text-xs">
              <span class="text-gray-500">{{ t('admin.accounts.statePool.remaining') }}</span>
              <span class="font-mono font-semibold tabular-nums">{{ remainingLabel(row) }}</span>
            </div>
            <div class="mt-2 h-1.5 overflow-hidden rounded-full bg-gray-200 dark:bg-dark-600"><div class="h-full rounded-full bg-emerald-500 transition-all" :style="{ width: `${remainingPercent(row)}%` }" /></div>
            <p class="mt-2 text-xs text-gray-500">{{ t('admin.accounts.statePool.expiresAt') }} {{ formatTime(row.expires_at) }}</p>
          </div>

          <div class="mt-4 space-y-2 rounded-lg border border-gray-200 p-3 text-xs dark:border-dark-600">
            <div class="flex justify-between gap-2"><span class="font-mono">X-Codex-Turn-State</span><span>{{ row.state_length || '—' }}</span></div>
            <p class="font-mono tracking-widest text-gray-400">•••• •••• •••• ••••</p>
            <p class="text-gray-500">{{ t('admin.accounts.statePool.rawHint') }}</p>
          </div>
          <div class="mt-4 min-h-14 space-y-1 text-xs text-gray-500">
            <p>{{ probeLabel(row) }}<span v-if="row.attempts > 0"> · {{ row.attempts }}/25</span></p>
            <p>{{ t('admin.accounts.statePool.lastAttempt') }} {{ formatTime(row.last_attempt_at) }}</p>
            <p v-if="row.reason" class="text-amber-600 dark:text-amber-400">{{ reasonLabel(row.reason) }}</p>
          </div>
          <div class="mt-4 flex flex-wrap justify-between gap-2">
            <button class="btn btn-secondary btn-sm" :disabled="!isValid(row) || copying === rowKey(row)" @click="copyState(row)">{{ t('admin.accounts.statePool.copy') }}</button>
            <button class="btn btn-primary btn-sm" :disabled="!row.can_refresh || refreshing.has(rowKey(row))" @click="queueRefresh(row)">{{ t('admin.accounts.statePool.refresh') }}</button>
          </div>
        </article>
      </div>
    </div>
  </AppLayout>
</template>

<script setup lang="ts">
import { computed, onMounted, onUnmounted, ref } from 'vue'
import { useI18n } from 'vue-i18n'
import AppLayout from '@/components/layout/AppLayout.vue'
import ProxySelector from '@/components/common/ProxySelector.vue'
import { useAppStore } from '@/stores/app'
import { useClipboard } from '@/composables/useClipboard'
import { getAll as getProxies } from '@/api/admin/proxies'
import { getStatePool, saveStatePoolProxy, refreshState, getStateValue, type StatePoolRow, type StatePoolResponse } from '@/api/admin/codexStatePool'
import type { Proxy } from '@/types'

const { t } = useI18n()
const app = useAppStore()
const { copyToClipboard } = useClipboard()
const pool = ref<StatePoolResponse | null>(null)
const proxies = ref<Proxy[]>([])
const proxyId = ref<number | null>(null)
const modelFilter = ref('gpt-6-astra')
const search = ref('')
const loading = ref(false)
const saving = ref(false)
const error = ref('')
const copying = ref('')
const refreshing = ref(new Set<string>())
const now = ref(Date.now())
let serverOffset = 0
let controller: AbortController | null = null
let poll: ReturnType<typeof setInterval> | undefined
let clock: ReturnType<typeof setInterval> | undefined
let disposed = false
let configLoaded = false
const modelOptions = computed(() => [...new Set(['gpt-6-astra', ...(pool.value?.models || [])])].sort())
const visibleRows = computed(() => (pool.value?.items || []).filter(row => (!modelFilter.value || row.model === modelFilter.value) && `${row.account_id} ${row.account_name} ${row.plan}`.toLowerCase().includes(search.value.trim().toLowerCase())))
const validCount = computed(() => visibleRows.value.filter(isValid).length)
const rowKey = (row: StatePoolRow) => `${row.account_id}:${row.model}`
const remaining = (row: StatePoolRow) => row.expires_at ? Math.max(0, Date.parse(row.expires_at) - now.value) : 0
const isValid = (row: StatePoolRow) => row.state_status === 'valid' && remaining(row) > 0
function remainingPercent(row: StatePoolRow) {
  if (!isValid(row) || !row.obtained_at || !row.expires_at) return 0
  const duration = Date.parse(row.expires_at) - Date.parse(row.obtained_at)
  return duration > 0 ? Math.min(100, remaining(row) / duration * 100) : 0
}
function remainingLabel(row: StatePoolRow) {
  if (!row.expires_at) return '—'
  const seconds = Math.floor(remaining(row) / 1000)
  if (!seconds) return t('admin.accounts.statePool.expired')
  return `${Math.floor(seconds / 60).toString().padStart(2, '0')}:${(seconds % 60).toString().padStart(2, '0')}`
}
function formatTime(value?: string) { return value ? new Date(value).toLocaleString() : '—' }
function stateLabel(row: StatePoolRow) {
  if (isValid(row)) return t('admin.accounts.statePool.valid', { length: row.state_length })
  if (row.state_status === 'valid' || row.state_status === 'expired') return t('admin.accounts.statePool.expired')
  if (row.state_status === 'unknown_plan') return t('admin.accounts.statePool.unknownPlan')
  if (row.state_status === 'disabled') return t('admin.accounts.statePool.disabled')
  if (row.state_status === 'invalid') return t('admin.accounts.statePool.invalid')
  return t('admin.accounts.statePool.missing')
}
function probeLabel(row: StatePoolRow) {
  if (row.probe_status === 'running') return t('admin.accounts.statePool.running')
  if (row.probe_status === 'queued') return t('admin.accounts.statePool.queued')
  if (row.probe_status === 'stopped') return t('admin.accounts.statePool.stopped')
  if (row.probe_status === 'blocked') return t('admin.accounts.statePool.blocked')
  return t('admin.accounts.statePool.idle')
}
function reasonLabel(reason: string) {
  if (reason === 'response_created_gpt_5_6_luna') return t('admin.accounts.statePool.downgraded')
  if (reason === 'server_is_overloaded') return t('admin.accounts.statePool.overloaded')
  if (reason.startsWith('state_length_') || reason === 'unexpected_state_length') return t('admin.accounts.statePool.abnormalLength')
  return t('admin.accounts.statePool.probeFailed')
}
async function loadPool() {
  if (loading.value || disposed) return
  loading.value = true
  controller = new AbortController()
  try {
    const result = await getStatePool(controller.signal)
    if (disposed) return
    pool.value = result
    serverOffset = Date.parse(result.server_time) - Date.now()
    now.value = Date.now() + serverOffset
    if (!configLoaded) { proxyId.value = result.config.proxy_id || null; configLoaded = true }
    error.value = ''
  } catch (err: any) {
    if (!disposed && err?.code !== 'ERR_CANCELED') error.value = t('admin.accounts.statePool.loadFailed')
  } finally { loading.value = false }
}
async function saveProxy() {
  saving.value = true
  try { await saveStatePoolProxy(proxyId.value || 0); app.showSuccess(t('admin.accounts.statePool.saved')); await loadPool() }
  catch { app.showError(t('admin.accounts.statePool.saveFailed')) }
  finally { saving.value = false }
}
async function queueRefresh(row: StatePoolRow) {
  refreshing.value.add(rowKey(row))
  try { await refreshState(row); app.showSuccess(t('admin.accounts.statePool.refreshQueued')); await loadPool() }
  catch { app.showError(t('admin.accounts.statePool.refreshFailed')) }
  finally { refreshing.value.delete(rowKey(row)) }
}
async function copyState(row: StatePoolRow) {
  copying.value = rowKey(row)
  try { await copyToClipboard(await getStateValue(row), t('admin.accounts.statePool.copied')) }
  catch { app.showError(t('admin.accounts.statePool.copyFailed')) }
  finally { copying.value = '' }
}
onMounted(() => {
  void loadPool()
  void getProxies().then(value => { if (!disposed) proxies.value = value }).catch(() => app.showError(t('admin.accounts.statePool.loadProxiesFailed')))
  clock = setInterval(() => { now.value = Date.now() + serverOffset }, 1000)
  poll = setInterval(() => { if (!document.hidden) void loadPool() }, 5000)
})
onUnmounted(() => { disposed = true; controller?.abort(); clearInterval(poll); clearInterval(clock) })
</script>
