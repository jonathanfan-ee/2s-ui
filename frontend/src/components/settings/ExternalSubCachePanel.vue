<template>
  <SettingsGroup :title="$t('subCache.title')" :desc="$t('subCache.desc')">
    <p class="cache-note">{{ $t('subSources.cacheHint') }}</p>
    <p class="cache-note">{{ $t('subCache.limits') }}</p>
    <div v-if="!loaded">{{ $t('loading') }}</div>
    <template v-else>
      <ToggleRow :model-value="config.enabled" :label="$t('subCache.reads')" :desc="$t('subCache.enableHint')"
                 @update:model-value="config.enabled = $event && canEnable" />
      <ToggleRow v-model="config.autoRefresh" :label="$t('subCache.auto')" />
      <SRow :label="$t('subCache.interval')" :hint="$t('date.m')">
        <input v-model.number="config.refreshMinutes" class="input mono" type="number" min="5" max="1440" />
      </SRow>
      <p v-if="!canEnable" class="cache-note">{{ $t('subCache.coldHint') }}</p>
      <div class="cache-actions">
        <Btn variant="primary" sm :loading="busy" :disabled="!validInterval" @click="apply">{{ $t('subCache.apply') }}</Btn>
        <Btn sm :disabled="busy || !warmKeys.length" @click="queue(warmKeys)">{{ $t('subCache.warm') }}</Btn>
        <Btn sm :disabled="busy" @click="load()">{{ $t('subCache.reload') }}</Btn>
      </div>
      <p v-if="!sources.length" class="cache-note">{{ $t('subCache.empty') }}</p>
      <article v-for="source in sources" :key="source.key" class="cache-source">
        <div class="cache-heading">
          <div>
            <strong>{{ source.name || source.host }}</strong>
            <div class="cache-note mono">{{ source.host }} · {{ $t('subCache.references', { n: source.references }) }}</div>
          </div>
          <span>{{ source.ready ? $t('subCache.ready') : $t('subCache.cold') }}</span>
        </div>
        <div class="cache-note">{{ $t('subCache.universal') }}: {{ source.universalReady ? date(source.lastSuccess) : $t('subCache.cold') }}</div>
        <div class="cache-note">{{ $t('subCache.native') }}: {{ source.clashReady ? date(source.clashLastSuccess) : $t('subCache.cold') }}</div>
        <div v-if="source.nextAttempt" class="cache-note">{{ $t('subCache.nextAttempt') }}: {{ date(source.nextAttempt) }}</div>
        <div v-if="source.refreshing">{{ $t('subCache.refreshing') }}</div>
        <div v-else-if="source.paused">{{ $t('subCache.paused') }}</div>
        <div v-else-if="source.requested">{{ $t('subCache.queued') }}</div>
        <div v-if="source.lastError" class="cache-error">{{ errorText(source.lastError) }}</div>
        <Btn sm :disabled="busy || source.refreshing || (source.requested && !source.paused)"
             @click="queue([source.key], source.paused)">
          {{ source.paused ? $t('subCache.resume') : $t('subCache.refresh') }}
        </Btn>
      </article>
    </template>
  </SettingsGroup>
</template>

<script setup lang="ts">
import { computed, onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import { push } from 'notivue'
import { i18n, intlLocale } from '@/locales'
import HttpUtils from '@/plugins/httputil'
import SettingsGroup from '@/components/ui/SettingsGroup.vue'
import ToggleRow from '@/components/ui/ToggleRow.vue'
import SRow from '@/components/ui/SRow.vue'
import Btn from '@/components/ui/Btn.vue'

type Source = {
  key: string; name: string; host: string; references: number; ready: boolean
  lastSuccess: number; universalReady: boolean; clashReady: boolean; clashLastSuccess: number; nextAttempt: number; lastError: string
  paused: boolean; requested: boolean; refreshing: boolean
}
const config = reactive({ enabled: false, autoRefresh: false, refreshMinutes: 5 })
const sources = ref<Source[]>([])
const loaded = ref(false)
const busy = ref(false)
const canEnable = computed(() => loaded.value && sources.value.filter(s => s.references > 0).every(s => s.ready))
const validInterval = computed(() => Number.isInteger(config.refreshMinutes) && config.refreshMinutes >= 5 && config.refreshMinutes <= 1440)
const warmKeys = computed(() => sources.value.filter(s => !s.paused && !s.requested && !s.refreshing).map(s => s.key))
const jsonOptions = { headers: { 'Content-Type': 'application/json' } }
let timer: ReturnType<typeof setTimeout> | undefined
let disposed = false
let loading = false

// Status polling reads the panel DB only. It never requests a provider, stops
// when the tab is hidden, and cannot outlive the settings component.
function schedule() {
  clearTimeout(timer)
  if (!disposed && document.visibilityState === 'visible') timer = setTimeout(() => load(false), 15000)
}
async function load(initial = true) {
  if (loading || disposed) return
  loading = true
  try {
    const response = await HttpUtils.get('api/externalSubCache')
    if (!disposed && response.success) {
      sources.value = response.obj.sources
      if (initial || !loaded.value) Object.assign(config, response.obj.config)
      loaded.value = true
    }
  } finally {
    loading = false
    schedule()
  }
}
async function apply() {
  busy.value = true
  try {
    const response = await HttpUtils.post('api/configureExternalSubCache', { ...config }, jsonOptions)
    if (response.success) await load()
  } finally { busy.value = false }
}
async function queue(keys: string[], resume = false) {
  if (!keys.length) return
  busy.value = true
  try {
    const response = await HttpUtils.post('api/refreshExternalSubs', { keys, resume }, jsonOptions)
    if (response.success) {
      push.success({ message: i18n.global.t('subCache.queued') })
      await load(false)
    }
  } finally { busy.value = false }
}
function date(value: number) { return value ? new Date(value * 1000).toLocaleString(intlLocale()) : '—' }
function errorText(code: string) {
  if (/^source-http-\d{3}$/.test(code)) return `HTTP ${code.slice(-3)}`
  if (code === 'source-content-invalid') return i18n.global.t('subCache.invalid')
  if (code === 'request-budget-exhausted') return i18n.global.t('subCache.budget')
  return i18n.global.t('subCache.fetchFailed')
}
function visibilityChanged() {
  clearTimeout(timer)
  if (document.visibilityState === 'visible') void load(false)
}
onMounted(() => { document.addEventListener('visibilitychange', visibilityChanged); void load() })
onBeforeUnmount(() => { disposed = true; clearTimeout(timer); document.removeEventListener('visibilitychange', visibilityChanged) })
</script>

<style scoped>
.cache-note { color: var(--text-3); font-size: 12px; margin: 5px 0; overflow-wrap: anywhere; }
.cache-actions, .cache-heading { display: flex; gap: 12px; align-items: center; flex-wrap: wrap; }
.cache-actions { margin: 16px 0; }
.cache-heading { justify-content: space-between; margin-bottom: 8px; }
.cache-source { padding: 16px 0; border-top: 1px solid var(--line); }
.cache-source .btn { margin-top: 10px; }
.cache-error { color: var(--rose); font-size: 12px; margin-top: 6px; }
</style>
