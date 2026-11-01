<template>
  <SettingsGroup :title="$t('subDistribution.title')" :desc="$t('subDistribution.desc')">
    <Modal :open="editingTemplate" :title="$t('subDistribution.template')" :width="800" @close="close">
      <div class="policy-form">
        <SRow :label="$t('subDistribution.name')"><input v-model="templateForm.name" class="input" maxlength="128" /></SRow>
        <SRow :label="$t('subDistribution.format')"><Select v-model="templateForm.format" :disabled="!!templateForm.id"><option value="clash">Clash / Mihomo YAML</option><option value="json">sing-box JSON</option></Select></SRow>
        <ToggleRow v-model="templateForm.enabled" :label="$t('enable')" />
        <p class="policy-note">{{ $t('subDistribution.templateHint') }}</p>
        <textarea v-model="templateForm.content" class="input mono policy-editor" dir="ltr" spellcheck="false" :aria-label="$t('subDistribution.template')" />
        <p class="policy-note">{{ $t('subDistribution.assignmentHint') }}</p>
        <div class="policy-clients"><label v-for="client in clients" :key="client.id"><input v-model="templateForm.clientIds" type="checkbox" :value="client.id" />{{ client.name }} <small v-if="!client.enable">{{ $t('disable') }}</small></label></div>
      </div>
      <template #footer><Btn :disabled="busy" @click="close">{{ $t('no') }}</Btn><Btn variant="primary" :loading="busy" :disabled="!templateForm.name.trim() || !templateForm.content.trim()" @click="saveTemplate">{{ $t('actions.save') }}</Btn></template>
    </Modal>
    <Modal :open="editingAlias" :title="$t('subDistribution.alias')" :width="700" @close="close">
      <div class="policy-form">
        <SRow :label="$t('subDistribution.name')"><input v-model="aliasForm.name" class="input" maxlength="128" /></SRow>
        <SRow :label="$t('subDistribution.oldUrl')"><input v-model="aliasForm.url" class="input mono" autocomplete="off" spellcheck="false" :placeholder="aliasForm.id ? $t('subSources.urlHint') : 'https://legacy.example/token/profile'" /></SRow>
        <p class="policy-note">{{ $t('subDistribution.aliasHint') }}</p>
        <SRow :label="$t('subDistribution.client')"><Select v-model.number="aliasForm.clientId"><option :value="0" disabled>—</option><option v-for="client in clients" :key="client.id" :value="client.id">{{ client.name }}</option></Select></SRow>
        <SRow :label="$t('subDistribution.format')"><Select v-model="aliasForm.defaultFormat"><option value="auto">{{ $t('subDistribution.auto') }}</option><option value="clash">Clash / Mihomo</option><option value="json">sing-box</option><option value="base64">Base64 / Shadowrocket</option><option value="plain">URI</option></Select></SRow>
        <SRow :label="$t('subDistribution.clashOverride')"><Select v-model="aliasForm.clashTemplateId"><option value="">{{ $t('subDistribution.inherit') }}</option><option v-for="item in templates.filter(t => t.format === 'clash')" :key="item.id" :value="item.id">{{ item.name }}</option></Select></SRow>
        <SRow :label="$t('subDistribution.jsonOverride')"><Select v-model="aliasForm.jsonTemplateId"><option value="">{{ $t('subDistribution.inherit') }}</option><option v-for="item in templates.filter(t => t.format === 'json')" :key="item.id" :value="item.id">{{ item.name }}</option></Select></SRow>
        <SRow :label="$t('subDistribution.expiry')"><input v-model="expiryDate" type="datetime-local" class="input" /></SRow>
        <ToggleRow v-model="aliasForm.enabled" :label="$t('enable')" />
      </div>
      <template #footer><Btn :disabled="busy" @click="close">{{ $t('no') }}</Btn><Btn variant="primary" :loading="busy" :disabled="!aliasForm.name.trim() || !aliasForm.clientId || (!aliasForm.id && !aliasForm.url.trim())" @click="saveAlias">{{ $t('actions.save') }}</Btn></template>
    </Modal>
    <Modal :open="!!deleting" :title="$t('subDistribution.remove')" :width="480" @close="deleting = null">
      <p class="policy-form">{{ $t('subDistribution.deleteHint', { name: deleting?.name }) }}</p>
      <template #footer><Btn :disabled="busy" @click="deleting = null">{{ $t('no') }}</Btn><Btn :loading="busy" @click="remove">{{ $t('yes') }}</Btn></template>
    </Modal>
    <div class="policy-actions"><Btn variant="primary" sm :disabled="busy || !loaded" @click="editTemplate()">{{ $t('subDistribution.addTemplate') }}</Btn><Btn sm :disabled="busy || !loaded || !clients.length" @click="editAlias()">{{ $t('subDistribution.addAlias') }}</Btn><Btn sm :disabled="busy || loading" @click="load">{{ $t('subCache.reload') }}</Btn></div>
    <p class="policy-note">{{ $t('subDistribution.rolloutHint') }}</p>
    <p v-if="!loaded">{{ $t('loading') }}</p>
    <article v-for="item in templates" :key="item.id" class="policy-card">
      <strong>{{ item.name }}</strong> · {{ item.format }} · {{ item.enabled ? $t('enable') : $t('disable') }}
      <p class="policy-note">{{ $t('subSources.grantCount', { n: item.clientIds.length }) }}</p>
      <div class="policy-actions"><Btn sm :disabled="busy" @click="editTemplate(item)">{{ $t('subDistribution.edit') }}</Btn><Btn sm :disabled="busy" @click="deleting = { ...item, kind: 'template' }">{{ $t('subDistribution.remove') }}</Btn></div>
    </article>
    <article v-for="item in aliases" :key="item.id" class="policy-card">
      <strong>{{ item.name }}</strong> · {{ item.enabled ? $t('enable') : $t('disable') }}
      <p class="policy-note mono">{{ item.host }} → {{ clients.find(c => c.id === item.clientId)?.name || '—' }} · {{ item.defaultFormat }}</p>
      <div class="policy-actions"><Btn sm :disabled="busy" @click="editAlias(item)">{{ $t('subDistribution.edit') }}</Btn><Btn sm :disabled="busy" @click="deleting = { ...item, kind: 'alias' }">{{ $t('subDistribution.remove') }}</Btn></div>
    </article>
  </SettingsGroup>
</template>

<script setup lang="ts">
import { computed, onBeforeUnmount, onMounted, reactive, ref } from 'vue'
import HttpUtils from '@/plugins/httputil'
import SettingsGroup from '@/components/ui/SettingsGroup.vue'
import SRow from '@/components/ui/SRow.vue'
import Select from '@/components/ui/Select.vue'
import ToggleRow from '@/components/ui/ToggleRow.vue'
import Modal from '@/components/ui/Modal.vue'
import Btn from '@/components/ui/Btn.vue'

type Template = { id: string; name: string; format: string; enabled: boolean; revision: number; clientIds: number[] }
type Alias = { id: string; name: string; host: string; clientId: number; enabled: boolean; expiry: number; defaultFormat: string; clashTemplateId: string; jsonTemplateId: string; revision: number }
const templates = ref<Template[]>([])
const aliases = ref<Alias[]>([])
const clients = ref<{ id: number; name: string; enable: boolean }[]>([])
const loaded = ref(false), loading = ref(false), busy = ref(false)
const editingTemplate = ref(false), editingAlias = ref(false)
const deleting = ref<{ id: string; name: string; revision: number; kind: 'template' | 'alias' } | null>(null)
const templateForm = reactive({ id: '', name: '', format: 'clash', content: '', enabled: true, revision: 0, clientIds: [] as number[] })
const aliasForm = reactive({ id: '', name: '', url: '', clientId: 0, enabled: true, expiry: 0, defaultFormat: 'auto', clashTemplateId: '', jsonTemplateId: '', revision: 0 })
const expiryDate = computed({
  get: () => {
    if (!aliasForm.expiry) return ''
    const date = new Date(aliasForm.expiry * 1000)
    if (!Number.isFinite(date.getTime())) return ''
    const local = new Date(date.getTime() - date.getTimezoneOffset() * 60000)
    return local.toISOString().slice(0, 16)
  },
  set: (value: string) => { aliasForm.expiry = value ? Math.floor(new Date(value).getTime() / 1000) : 0 },
})
const jsonOptions = { headers: { 'Content-Type': 'application/json' } }
let disposed = false
async function load() {
  if (loading.value || disposed) return
  loading.value = true
  try {
    const response = await HttpUtils.get('api/subscriptionDistribution')
    if (response.success && !disposed) {
      templates.value = response.obj.templates; aliases.value = response.obj.aliases; clients.value = response.obj.clients; loaded.value = true
    }
  } finally { loading.value = false }
}
async function editTemplate(item?: Template) {
  busy.value = true
  try {
    let content = 'proxy-groups:\n  - name: Proxy\n    type: select\n    proxies: [__AUTO_INSERT_NODES__, DIRECT]\nrules:\n  - MATCH,Proxy\n'
    if (item) {
      const response = await HttpUtils.get('api/subscriptionTemplate', { id: item.id })
      if (!response.success || disposed) return
      if (response.obj.revision !== item.revision) { await load(); return }
      content = response.obj.content
    }
    if (disposed) return
    Object.assign(templateForm, { id: item?.id || '', name: item?.name || '', format: item?.format || 'clash', content, enabled: item?.enabled ?? true, revision: item?.revision || 0, clientIds: [...(item?.clientIds || [])] })
    editingTemplate.value = true
  } finally { busy.value = false }
}
function editAlias(item?: Alias) {
  Object.assign(aliasForm, { id: '', name: '', clientId: 0, enabled: true, expiry: 0, defaultFormat: 'auto', clashTemplateId: '', jsonTemplateId: '', revision: 0 }, item || {}, { url: '' })
  editingAlias.value = true
}
function close() { if (!busy.value) { editingTemplate.value = false; editingAlias.value = false; templateForm.content = ''; aliasForm.url = '' } }
async function saveTemplate() {
  busy.value = true
  try { const response = await HttpUtils.post('api/saveSubscriptionTemplate', { ...templateForm }, jsonOptions); if (response.success && !disposed) { editingTemplate.value = false; templateForm.content = ''; await load() } } finally { busy.value = false }
}
async function saveAlias() {
  busy.value = true
  try { const response = await HttpUtils.post('api/saveLegacySubscriptionAlias', { ...aliasForm }, jsonOptions); if (response.success && !disposed) { editingAlias.value = false; aliasForm.url = ''; await load() } } finally { busy.value = false }
}
async function remove() {
  if (!deleting.value) return
  busy.value = true
  try {
    const action = deleting.value.kind === 'template' ? 'deleteSubscriptionTemplate' : 'deleteLegacySubscriptionAlias'
    const response = await HttpUtils.post(`api/${action}`, { id: deleting.value.id, revision: deleting.value.revision }, jsonOptions)
    if (response.success && !disposed) { deleting.value = null; await load() }
  } finally { busy.value = false }
}
onMounted(load)
onBeforeUnmount(() => { disposed = true; templateForm.content = ''; aliasForm.url = '' })
</script>

<style scoped>
.policy-form { display: grid; gap: 12px; padding: 16px 18px; }
.policy-editor { min-height: 320px; resize: vertical; }
.policy-note { font-size: 12.5px; color: var(--text-3); overflow-wrap: anywhere; }
.policy-actions { display: flex; gap: 8px; flex-wrap: wrap; margin: 12px 0; }
.policy-card { border: 1px solid var(--line); border-radius: 10px; padding: 14px; margin-top: 12px; }
.policy-clients { display: grid; gap: 8px; max-height: 220px; overflow: auto; }
.policy-clients label { display: flex; gap: 8px; align-items: center; }
</style>
