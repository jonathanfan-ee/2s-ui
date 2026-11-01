<template>
  <SettingsGroup :title="$t('subSources.title')" :desc="$t('subSources.desc')">
    <Modal :open="editing" :title="form.id ? $t('subSources.edit') : $t('subSources.create')" :width="680" @close="close">
      <div class="source-form">
        <SRow :label="$t('subSources.name')"><input v-model="form.name" class="input" maxlength="128" /></SRow>
        <SRow :label="$t('subSources.url')">
          <input v-model="form.url" class="input mono" autocomplete="off" spellcheck="false" :placeholder="form.id ? $t('subSources.urlHint') : 'https://provider.example/sub'" />
        </SRow>
        <p class="source-note">{{ $t('subSources.urlHint') }}</p>
        <ToggleRow v-model="form.enabled" :label="$t('subSources.enabled')" />
        <p class="source-note">{{ $t('subSources.policy') }}</p>
        <div class="source-label">{{ $t('subSources.clients') }}</div>
        <p class="source-note">{{ $t('subSources.clientsHint') }}</p>
        <div class="source-clients">
          <label v-for="client in clients" :key="client.id" class="source-client">
            <input v-model="form.clientIds" type="checkbox" :value="client.id" />
            <span>{{ client.name }}</span>
            <span v-if="!active(client)" class="source-note">{{ $t('subSources.inactive') }}</span>
          </label>
          <p v-if="!clients.length" class="source-note">{{ $t('subSources.noClients') }}</p>
        </div>
        <label class="source-client"><input v-model="form.adoptLegacy" type="checkbox" />{{ $t('subSources.adopt') }}</label>
        <p class="source-note">{{ $t('subSources.adoptHint') }}</p>
      </div>
      <template #footer>
        <Btn :disabled="busy" @click="close">{{ $t('no') }}</Btn>
        <Btn variant="primary" :loading="busy" :disabled="!form.name.trim() || (!form.id && !form.url.trim())" @click="save">{{ $t('actions.save') }}</Btn>
      </template>
    </Modal>
    <Modal :open="!!deleting" :title="$t('subSources.remove')" :width="480" @close="deleting = null">
      <p class="source-form">{{ $t('subSources.removeHint', { name: deleting?.name }) }}</p>
      <template #footer>
        <Btn :disabled="busy" @click="deleting = null">{{ $t('no') }}</Btn>
        <Btn :loading="busy" style="color: var(--rose)" @click="remove">{{ $t('yes') }}</Btn>
      </template>
    </Modal>

    <div class="source-actions">
      <Btn variant="primary" sm :disabled="busy || !loaded" @click="edit()">{{ $t('subSources.create') }}</Btn>
      <Btn sm :disabled="busy || loading" @click="load">{{ $t('subCache.reload') }}</Btn>
    </div>
    <p v-if="!loaded">{{ $t('loading') }}</p>
    <p v-else-if="!sources.length" class="source-note">{{ $t('subSources.empty') }}</p>
    <article v-for="source in sources" :key="source.id" class="source-card">
      <div class="source-heading">
        <strong>{{ source.name }}</strong>
        <span>{{ source.enabled ? (source.cache.ready ? $t('subCache.ready') : $t('subCache.cold')) : $t('disable') }}</span>
      </div>
      <div class="source-note mono">{{ source.host }}</div>
      <p class="source-note">{{ $t('subSources.grantCount', { n: source.clientIds.length }) }}</p>
      <p class="source-note">{{ $t('subCache.lastSuccess') }}: {{ date(source.cache.lastSuccess) }}</p>
      <p v-if="source.cache.paused" class="source-note">{{ $t('subCache.paused') }}</p>
      <p v-else-if="source.cache.refreshing || source.cache.requested" class="source-note">{{ source.cache.refreshing ? $t('subCache.refreshing') : $t('subCache.queued') }}</p>
      <div class="source-actions">
        <Btn sm :disabled="busy" @click="edit(source)">{{ $t('subSources.edit') }}</Btn>
        <Btn sm :disabled="busy || !source.enabled || source.cache.refreshing || (source.cache.requested && !source.cache.paused)" @click="queue(source)">{{ source.cache.paused ? $t('subCache.resume') : $t('subCache.refresh') }}</Btn>
        <Btn sm :disabled="busy" style="color: var(--rose)" @click="deleting = source">{{ $t('subSources.remove') }}</Btn>
      </div>
    </article>
  </SettingsGroup>
</template>

<script setup lang="ts">
import { onMounted, onBeforeUnmount, reactive, ref } from 'vue'
import { push } from 'notivue'
import { i18n, intlLocale } from '@/locales'
import HttpUtils from '@/plugins/httputil'
import SettingsGroup from '@/components/ui/SettingsGroup.vue'
import SRow from '@/components/ui/SRow.vue'
import ToggleRow from '@/components/ui/ToggleRow.vue'
import Btn from '@/components/ui/Btn.vue'
import Modal from '@/components/ui/Modal.vue'

type Source = {
  id: string; name: string; host: string; enabled: boolean; revision: number; clientIds: number[]
  cache: { key: string; ready: boolean; lastSuccess: number; requested: boolean; refreshing: boolean; paused: boolean }
}
type Client = { id: number; name: string; enable: boolean; expiry: number }
const sources = ref<Source[]>([])
const clients = ref<Client[]>([])
const loaded = ref(false)
const loading = ref(false)
const busy = ref(false)
const editing = ref(false)
const deleting = ref<Source | null>(null)
const form = reactive({ id: '', name: '', url: '', enabled: true, revision: 0, clientIds: [] as number[], adoptLegacy: false })
const jsonOptions = { headers: { 'Content-Type': 'application/json' } }
let timer: ReturnType<typeof setTimeout> | undefined
let disposed = false

async function load() {
  if (loading.value || disposed) return
  clearTimeout(timer)
  loading.value = true
  try {
    const response = await HttpUtils.get('api/externalSubSources')
    if (!disposed && response.success) {
      sources.value = response.obj.sources
      clients.value = response.obj.clients
      loaded.value = true
    }
  } finally {
    loading.value = false
    if (!disposed && document.visibilityState === 'visible' && sources.value.some(s => s.cache.requested || s.cache.refreshing)) timer = setTimeout(load, 15000)
  }
}
function edit(source?: Source) {
  Object.assign(form, { id: source?.id || '', name: source?.name || '', url: '', enabled: source?.enabled ?? true, revision: source?.revision || 0, clientIds: [...(source?.clientIds || [])], adoptLegacy: false })
  editing.value = true
}
function close() { if (!busy.value) editing.value = false }
async function save() {
  busy.value = true
  try {
    const response = await HttpUtils.post('api/saveExternalSubSource', { ...form, clientIds: [...form.clientIds] }, jsonOptions)
    if (response.success) { editing.value = false; await load() }
  } finally { busy.value = false }
}
async function remove() {
  if (!deleting.value) return
  busy.value = true
  try {
    const response = await HttpUtils.post('api/deleteExternalSubSource', { id: deleting.value.id, revision: deleting.value.revision }, jsonOptions)
    if (response.success) { deleting.value = null; await load() }
  } finally { busy.value = false }
}
async function queue(source: Source) {
  busy.value = true
  try {
    const response = await HttpUtils.post('api/refreshExternalSubs', { keys: [source.cache.key], resume: source.cache.paused }, jsonOptions)
    if (response.success) { push.success({ message: i18n.global.t('subCache.queued') }); await load() }
  } finally { busy.value = false }
}
function active(client: Client) { return client.enable && (!client.expiry || client.expiry > Date.now() / 1000) }
function date(value: number) { return value ? new Date(value * 1000).toLocaleString(intlLocale()) : '—' }
function visibilityChanged() { clearTimeout(timer); if (document.visibilityState === 'visible') void load() }
onMounted(() => { document.addEventListener('visibilitychange', visibilityChanged); void load() })
onBeforeUnmount(() => { disposed = true; clearTimeout(timer); document.removeEventListener('visibilitychange', visibilityChanged) })
</script>

<style scoped>
.source-form { padding: 18px 20px; }
.source-note { color: var(--text-3); font-size: 12px; margin: 6px 0; overflow-wrap: anywhere; }
.source-label { margin-top: 18px; font-weight: 600; font-size: 13px; }
.source-actions, .source-heading { display: flex; align-items: center; flex-wrap: wrap; gap: 10px; margin: 12px 0; }
.source-heading { justify-content: space-between; }
.source-card { border-top: 1px solid var(--line); padding: 8px 0; }
.source-clients { max-height: 220px; overflow-y: auto; margin: 10px 0 16px; }
.source-client { display: flex; gap: 9px; align-items: center; padding: 7px 0; font-size: 13px; }
</style>
