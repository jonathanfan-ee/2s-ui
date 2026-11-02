<template>
  <ClientDrawer
    :visible="drawer.visible"
    :id="drawer.id"
    :groups="groups"
    :inbound-tags="inboundTags"
    @close="drawer.visible = false"
  />
  <ClientAddBulk
    :visible="addBulkVisible"
    :groups="groups"
    :inbound-tags="inboundTags"
    @close="addBulkVisible = false"
  />
  <ClientEditBulk
    :visible="editBulkVisible"
    :clients="clients"
    :inbound-tags="inboundTags"
    :selected="editBulkSelected"
    @close="editBulkVisible = false"
  />
  <QrModal :visible="qrcode.visible" :id="qrcode.id" @close="qrcode.visible = false" />
  <StatsModal
    :visible="stats.visible"
    :resource="stats.resource"
    :tag="stats.tag"
    @close="stats.visible = false"
  />
  <ClientIpsModal :visible="ips.visible" :name="ips.name" @close="ips.visible = false" />
  <SessionsModal :visible="sessions.visible" :name="sessions.name" @close="sessions.visible = false" />

  <!-- delete confirmation (single row or bulk) -->
  <DeleteConfirm :open="del.visible" :loading="deleting" @close="del.visible = false" @confirm="confirmDelete" />

  <div class="page-stack fade-up">
    <!-- ===================== toolbar ===================== -->
    <div class="toolbar">
      <div class="search-wrap">
        <span class="s-ico"><Ico name="search" :size="16" /></span>
        <input class="input" :placeholder="$t('ui.searchClient')" v-model="q" />
      </div>
      <Segmented v-model="groupFilter" :options="groupOptions" />
      <Segmented v-model="stateFilter" :options="stateOptions" />
      <div class="grow" />
      <template v-if="sel.size > 0">
        <span class="sel-info">{{ $t('ui.selected', { n: sel.size }) }}</span>
        <Btn sm @click="openEditBulk"><Ico name="edit" :size="14" /> {{ $t('ui.editBulk') }}</Btn>
        <Btn sm style="color: var(--rose);" :disabled="hasSystemClients([...sel])" :title="hasSystemClients([...sel]) ? $t('ui.systemSubscriptionProtected') : $t('actions.delbulk')" @click="askDelete([...sel])">
          <Ico name="trash" :size="14" />
        </Btn>
      </template>
      <Btn sm @click="addBulkVisible = true"><Ico name="upload" :size="15" /> {{ $t('ui.addBulk') }}</Btn>
      <Btn variant="primary" sm @click="openDrawer(0)"><Ico name="plus" :size="16" /> {{ $t('ui.addClient') }}</Btn>
    </div>

    <!-- ===================== table (desktop) ===================== -->
    <div class="card clients-table" style="overflow: hidden;">
      <div style="overflow-x: auto;">
        <table class="dtable" style="min-width: 920px;">
          <thead>
            <tr>
              <th style="width: 44px; padding-inline-end: 0;">
                <Check :checked="allSel" @toggle="toggleAll" />
              </th>
              <th>{{ $t('ui.colClient') }}</th>
              <th>{{ $t('ui.colGroup') }}</th>
              <th>{{ $t('ui.colProtocol') }}</th>
              <th>{{ $t('ui.colUsage') }}</th>
              <th>{{ $t('ui.colExpiry') }}</th>
              <th>{{ $t('ui.colStatus') }}</th>
              <th style="text-align: end;">{{ $t('ui.colActions') }}</th>
            </tr>
          </thead>
          <tbody>
            <tr v-for="c in pageRows" :key="c.id" class="clickable" @click="openDrawer(c.id)">
              <td @click.stop="toggleSel(c.id)">
                <Check :checked="sel.has(c.id)" />
              </td>
              <td class="pad-y">
                <div style="display: flex; align-items: center; gap: 11px;">
                  <div style="position: relative; flex: none;">
                    <Avatar :name="c.name" />
                    <span v-if="isOnline(c.name)" class="on-dot" />
                  </div>
                  <div style="min-width: 0;">
                    <div style="font-weight: 700; font-size: 13.5px; display: flex; align-items: center; gap: 7px; white-space: nowrap;">
                      {{ displayName(c) }}
                      <span v-if="!c.enable" class="chip off-chip">{{ $t('ui.off') }}</span>
                    </div>
                    <div style="font-size: 12px; color: var(--text-3);">{{ isSystemSubscriptionClient(c) ? $t('ui.systemSubscriptionPurpose') : c.desc }}</div>
                  </div>
                </div>
              </td>
              <td>
                <Chip :color="groupColor(c.group)" style="text-transform: capitalize;">{{ groupLabel(c.group) }}</Chip>
              </td>
              <td>
                <span class="mono" style="font-size: 12.5px; color: var(--text-2);" :title="inbTitle(c)">{{ protoSummary(c) }}</span>
              </td>
              <td class="usage-td">
                <div style="min-width: 150px;" :title="usageTitle(c)">
                  <div style="display: flex; justify-content: space-between; font-size: 12px; margin-bottom: 5px; white-space: nowrap; gap: 8px;">
                    <span class="mono" style="font-weight: 600;">{{ HumanReadable.sizeFormat(c.up + c.down) }}</span>
                    <span class="mono" style="color: var(--text-3);">{{ c.volume ? HumanReadable.sizeFormat(c.volume) : '∞' }}</span>
                  </div>
                  <BarMini :value="c.volume ? quotaPct(c) : 4" :color="quotaColor(c)" :height="5" />
                  <!-- Laid out on every row, filled only while the client moves
                       traffic: "online" means traffic in the last 10s window, so
                       a line that came and went with it would resize the row
                       every time the client paused. -->
                  <div class="rate mono" :title="rateTip(c)">
                    <template v-if="rateOf(c)">
                      <!-- Spaced with a character, not a margin: .rate-fig is ltr,
                           so its logical margin would land on the outside under RTL. -->
                      <span class="rate-fig"><span class="rate-dn">↓</span> {{ rateFmt(rateOf(c)!.down) }}</span>&emsp;<span class="rate-fig"><span class="rate-up">↑</span> {{ rateFmt(rateOf(c)!.up) }}</span>
                    </template>
                  </div>
                </div>
              </td>
              <td>
                <span :title="expiryTitle(c)">
                  <Chip v-if="!c.expiry" color="emerald">{{ $t('ui.noLimit') }}</Chip>
                  <Chip v-else-if="daysLeft(c.expiry) < 0" color="rose">{{ $t('ui.expired') }}</Chip>
                  <Chip v-else-if="daysLeft(c.expiry) <= 7" color="amber">{{ $t('ui.dLeft', { n: daysLeft(c.expiry) }) }}</Chip>
                  <Chip v-else><span class="mono">{{ daysLeft(c.expiry) }}</span>&nbsp;{{ $t('ui.daysWord') }}</Chip>
                </span>
                <div v-if="hasNextReset(c)" class="reset-line" :title="resetTitle(c)">
                  <Ico name="refresh" :size="11" />{{ resetLabel(c) }}
                </div>
              </td>
              <td>
                <span style="display: inline-flex; gap: 5px; align-items: center;">
                  <Chip
                    v-if="isOnline(c.name)"
                    color="emerald"
                    dot
                    style="cursor: pointer;"
                    :title="$t('ui.sessOpen')"
                    @click.stop="showSessions(c.name)"
                  >{{ $t('ui.online') }}</Chip>
                  <span v-else style="color: var(--text-3); font-size: 13px;">—</span>
                  <Chip
                    v-if="c.limitIp > 0"
                    :color="ipChipColor(c)"
                    style="cursor: pointer;"
                    :title="ipCountTitle"
                    @click.stop="showIps(c.name)"
                  >
                    <span class="mono">{{ ipCount(c) }}/{{ c.limitIp }}</span>
                  </Chip>
                </span>
              </td>
              <td class="actions-td">
                <div style="display: flex; gap: 2px; justify-content: flex-end; align-items: center;" @click.stop>
                  <Toggle
                    :model-value="c.enable"
                    :scale="0.8"
                    :title="$t(c.enable ? 'disable' : 'enable')"
                    style="margin-inline-end: 4px;"
                    @update:model-value="toggleEnable(c)"
                  />
                  <IconBtn name="qr" :title="$t('client.links')" @click="showQrCode(c.id)" />
                  <IconBtn v-if="dataStore.enableTraffic" name="chart" :title="$t('stats.graphTitle')" @click="showStats(c.name)" />
                  <IconBtn name="edit" :title="$t('actions.edit')" @click="openDrawer(c.id)" />
                  <IconBtn name="trash" danger :disabled="isSystemSubscriptionClient(c)" :title="isSystemSubscriptionClient(c) ? $t('ui.systemSubscriptionProtected') : $t('actions.del')" @click="askDelete([c.id])" />
                </div>
              </td>
            </tr>
          </tbody>
        </table>
      </div>
      <div class="t-foot">
        <span>{{ $t('ui.showingOf', { a: pageRows.length, b: clients.length }) }}</span>
        <div style="margin-inline-start: auto; display: flex; gap: 6px; align-items: center;">
          <Select v-model="perPage" class="per-page">
            <option v-for="n in PER_PAGE_OPTIONS" :key="n" :value="n">{{ n > 0 ? n : $t('all') }}</option>
          </Select>
          <template v-if="perPage > 0">
            <Btn variant="subtle" sm icon :disabled="page <= 1" @click="page--">
              <Ico name="chevron" :size="16" style="transform: rotate(180deg);" />
            </Btn>
            <span class="mono" style="font-size: 12.5px;">{{ page }} / {{ totalPages }}</span>
            <Btn variant="subtle" sm icon :disabled="page >= totalPages" @click="page++">
              <Ico name="chevron" :size="16" />
            </Btn>
          </template>
        </div>
      </div>
    </div>

    <!-- ===================== cards (mobile ≤820px) ===================== -->
    <div class="clients-cards">
      <div v-for="c in pageRows" :key="c.id" class="card m-client" @click="openDrawer(c.id)">
        <div class="m-head">
          <span style="flex: none; display: inline-flex;" @click.stop="toggleSel(c.id)">
            <Check :checked="sel.has(c.id)" />
          </span>
          <div style="position: relative; flex: none;">
            <Avatar :name="c.name" :size="34" />
            <span v-if="isOnline(c.name)" class="on-dot" />
          </div>
          <div style="flex: 1; min-width: 0;">
            <div style="font-weight: 700; font-size: 13.5px; display: flex; align-items: center; gap: 7px;">
              <span style="overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">{{ displayName(c) }}</span>
              <span v-if="!c.enable" class="chip off-chip" style="flex: none;">{{ $t('ui.off') }}</span>
            </div>
            <div style="font-size: 12px; color: var(--text-3); overflow: hidden; text-overflow: ellipsis; white-space: nowrap;">{{ isSystemSubscriptionClient(c) ? $t('ui.systemSubscriptionPurpose') : c.desc }}</div>
          </div>
          <Chip :color="groupColor(c.group)" style="flex: none; text-transform: capitalize;">{{ groupLabel(c.group) }}</Chip>
        </div>
        <div class="kv-row">
          <span class="k">{{ $t('ui.colUsage') }}</span>
          <span class="v mono">{{ HumanReadable.sizeFormat(c.up + c.down) }} / {{ c.volume ? HumanReadable.sizeFormat(c.volume) : '∞' }}</span>
        </div>
        <div style="padding: 2px 0 4px;">
          <BarMini :value="c.volume ? quotaPct(c) : 4" :color="quotaColor(c)" :height="5" />
        </div>
        <!-- Always shown, for the same reason the table reserves its line. -->
        <div class="kv-row">
          <span class="k">{{ $t('ui.colRate') }}</span>
          <span v-if="rateOf(c)" class="v mono" :title="rateTip(c)">
            <span class="rate-fig"><span class="rate-dn">↓</span> {{ rateFmt(rateOf(c)!.down) }}</span>&ensp;<span class="rate-fig"><span class="rate-up">↑</span> {{ rateFmt(rateOf(c)!.up) }}</span>
          </span>
          <span v-else class="v" style="color: var(--text-3); font-weight: 400;">—</span>
        </div>
        <div class="kv-row">
          <span class="k">{{ $t('ui.colExpiry') }}</span>
          <span class="v">
            <Chip v-if="!c.expiry" color="emerald">{{ $t('ui.noLimit') }}</Chip>
            <Chip v-else-if="daysLeft(c.expiry) < 0" color="rose">{{ $t('ui.expired') }}</Chip>
            <Chip v-else-if="daysLeft(c.expiry) <= 7" color="amber">{{ $t('ui.dLeft', { n: daysLeft(c.expiry) }) }}</Chip>
            <Chip v-else><span class="mono">{{ daysLeft(c.expiry) }}</span>&nbsp;{{ $t('ui.daysWord') }}</Chip>
          </span>
        </div>
        <!-- A row of its own: beside the expiry chip, .v's 60% cap clipped the
             longer locales to an ellipsis. -->
        <div v-if="hasNextReset(c)" class="kv-row">
          <span class="k">{{ $t('client.autoReset') }}</span>
          <span class="v" style="font-weight: 500;" :title="resetTitle(c)">{{ resetLabel(c) }}</span>
        </div>
        <div class="kv-row">
          <span class="k">{{ $t('ui.colStatus') }}</span>
          <span class="v">
            <Chip
              v-if="isOnline(c.name)"
              color="emerald"
              dot
              style="cursor: pointer;"
              :title="$t('ui.sessOpen')"
              @click.stop="showSessions(c.name)"
            >{{ $t('ui.online') }}</Chip>
            <template v-else>—</template>
            <!-- Spaced with a logical margin rather than a flex gap: .v relies on
                 text-overflow to clip a long status, which a flex container drops. -->
            <Chip
              v-if="c.limitIp > 0"
              :color="ipChipColor(c)"
              style="cursor: pointer; margin-inline-start: 5px;"
              :title="ipCountTitle"
              @click.stop="showIps(c.name)"
            >
              <span class="mono">{{ ipCount(c) }}/{{ c.limitIp }}</span>
            </Chip>
          </span>
        </div>
        <div class="m-actions" @click.stop>
          <Toggle
            :model-value="c.enable"
            :scale="0.8"
            :title="$t(c.enable ? 'disable' : 'enable')"
            style="margin-inline-end: 4px;"
            @update:model-value="toggleEnable(c)"
          />
          <IconBtn name="qr" :title="$t('client.links')" @click="showQrCode(c.id)" />
          <IconBtn v-if="dataStore.enableTraffic" name="chart" :title="$t('stats.graphTitle')" @click="showStats(c.name)" />
          <IconBtn name="edit" :title="$t('actions.edit')" @click="openDrawer(c.id)" />
          <IconBtn name="trash" danger :disabled="isSystemSubscriptionClient(c)" :title="isSystemSubscriptionClient(c) ? $t('ui.systemSubscriptionProtected') : $t('actions.del')" @click="askDelete([c.id])" />
        </div>
      </div>
      <div class="card t-foot">
        <span>{{ $t('ui.showingOf', { a: pageRows.length, b: clients.length }) }}</span>
        <div style="margin-inline-start: auto; display: flex; gap: 6px; align-items: center;">
          <Select v-model="perPage" class="per-page">
            <option v-for="n in PER_PAGE_OPTIONS" :key="n" :value="n">{{ n > 0 ? n : $t('all') }}</option>
          </Select>
          <template v-if="perPage > 0">
            <Btn variant="subtle" sm icon :disabled="page <= 1" @click="page--">
              <Ico name="chevron" :size="16" style="transform: rotate(180deg);" />
            </Btn>
            <span class="mono" style="font-size: 12.5px;">{{ page }} / {{ totalPages }}</span>
            <Btn variant="subtle" sm icon :disabled="page >= totalPages" @click="page++">
              <Ico name="chevron" :size="16" />
            </Btn>
          </template>
        </div>
      </div>
    </div>
  </div>
</template>

<script lang="ts" setup>
import { computed, ref, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import Data from '@/store/modules/data'
import { HumanReadable } from '@/plugins/utils'
import { intlLocale } from '@/locales'
import Btn from '@/components/ui/Btn.vue'
import Ico from '@/components/ui/Ico.vue'
import Chip from '@/components/ui/Chip.vue'
import Check from '@/components/ui/Check.vue'
import Avatar from '@/components/ui/Avatar.vue'
import IconBtn from '@/components/ui/IconBtn.vue'
import Toggle from '@/components/ui/Toggle.vue'
import Segmented from '@/components/ui/Segmented.vue'
import Select from '@/components/ui/Select.vue'
import DeleteConfirm from '@/components/ui/DeleteConfirm.vue'
import { clientDisplayName, clientGroupLabel, isSystemSubscriptionClient } from '@/utils/clientIdentity'
import BarMini from '@/components/charts/BarMini.vue'
import ClientDrawer from '@/layouts/drawers/client/ClientDrawer.vue'
import ClientAddBulk from '@/layouts/drawers/client/ClientAddBulk.vue'
import ClientEditBulk from '@/layouts/drawers/client/ClientEditBulk.vue'
import QrModal from '@/layouts/drawers/QrModal.vue'
import StatsModal from '@/layouts/drawers/StatsModal.vue'
import ClientIpsModal from '@/layouts/drawers/ClientIpsModal.vue'
import SessionsModal from '@/layouts/drawers/SessionsModal.vue'

const { t } = useI18n({ useScope: 'global' })
const dataStore = Data()

// ---------------- store data ----------------
const clients = computed((): any[] => dataStore.clients)
const groupLabel = (group: string): string => clientGroupLabel(group, t)
const displayName = (client: any): string => clientDisplayName(client, t)
const hasSystemClients = (ids: number[]): boolean =>
  clients.value.some((client: any) => ids.includes(client.id) && isSystemSubscriptionClient(client))
const inbounds = computed((): any[] => dataStore.inbounds ?? [])
const inboundTags = computed((): { title: string; value: number }[] => {
  if (!inbounds.value) return []
  return inbounds.value.filter((i: any) => i.tag != '' && i.users).map((i: any) => ({ title: i.tag, value: i.id }))
})
const groups = computed((): string[] =>
  Array.from(new Set(clients.value.map((c: any) => String(c.group ?? '')))),
)
const isOnline = (cname: string): boolean =>
  dataStore.onlines?.user ? dataStore.onlines.user.includes(cname) : false

// ---------------- group chip colors (design clients.jsx groupColor) ----------------
const FIXED_GROUP_COLORS: Record<string, string> = {
  vip: 'var(--amber)',
  standard: 'var(--brand)',
  trial: 'var(--text-3)',
  staff: 'var(--emerald)',
  reseller: 'var(--cyan)',
}
const GROUP_PALETTE = ['var(--amber)', 'var(--brand)', 'var(--emerald)', 'var(--cyan)', 'var(--violet)']
const groupColor = (g: string): string => {
  if (!g) return 'var(--text-3)'
  if (FIXED_GROUP_COLORS[g]) return FIXED_GROUP_COLORS[g]
  let h = 0
  for (const ch of g) h = (h * 31 + ch.charCodeAt(0)) % 997
  return GROUP_PALETTE[h % GROUP_PALETTE.length]
}

// ---------------- filters ----------------
const q = ref('')
const groupFilter = ref<string | number>('all')
const stateFilter = ref<string | number>('all')

const groupOptions = computed((): [string | number, string][] => [
  ['all', t('ui.allGroups')],
  ...groups.value.map((g): [string | number, string] => [g, groupLabel(g)]),
])
const stateOptions = computed((): [string | number, string][] => [
  ['all', t('ui.any')],
  ['online', t('ui.online')],
  ['disabled', t('ui.disabled')],
  ['expired', t('ui.expired')],
])

const rows = computed((): any[] => {
  const now = Date.now() / 1000
  return clients.value.filter((c: any) => {
    if (groupFilter.value !== 'all' && c.group !== groupFilter.value) return false
    if (stateFilter.value === 'online' && !isOnline(c.name)) return false
    if (stateFilter.value === 'disabled' && c.enable) return false
    if (stateFilter.value === 'expired' && !(c.expiry > 0 && c.expiry < now)) return false
    if (q.value && !((c.name ?? '').includes(q.value) || (c.desc ?? '').includes(q.value))) return false
    return true
  })
})

// ---------------- pagination (items-per-page, default 10; 0 = all on one page) ----------------
// The key predates this selector, and its reader has always taken a negative
// value as "all" — so "all" is written back as -1, not 0.
const PER_PAGE_KEY = 'items-per-page'
const PER_PAGE_OPTIONS = [10, 20, 50, 100, 0]
const readPerPage = (): number => {
  let raw: string | null = null
  try { raw = localStorage.getItem(PER_PAGE_KEY) } catch { /* storage blocked */ }
  const v = parseInt(raw ?? '10', 10)
  return Number.isFinite(v) && v > 0 ? v : Number.isFinite(v) && v < 0 ? 0 : 10
}
const page = ref(1)
const perPage = ref(readPerPage())
watch(perPage, (n) => {
  page.value = 1
  try { localStorage.setItem(PER_PAGE_KEY, String(n > 0 ? n : -1)) } catch { /* storage blocked */ }
})
const totalPages = computed(() => (perPage.value > 0 ? Math.max(1, Math.ceil(rows.value.length / perPage.value)) : 1))
const pageRows = computed((): any[] => {
  if (perPage.value <= 0) return rows.value
  return rows.value.slice((page.value - 1) * perPage.value, page.value * perPage.value)
})
watch([q, groupFilter, stateFilter], () => { page.value = 1 })
watch(totalPages, (tp) => { if (page.value > tp) page.value = tp })

// ---------------- bulk selection ----------------
const sel = ref(new Set<number>())
const toggleSel = (id: number) => {
  const n = new Set(sel.value)
  if (n.has(id)) n.delete(id)
  else n.add(id)
  sel.value = n
}
const allSel = computed(() => pageRows.value.length > 0 && pageRows.value.every((r: any) => sel.value.has(r.id)))
const toggleAll = () => {
  const n = new Set(sel.value)
  if (allSel.value) pageRows.value.forEach((r: any) => n.delete(r.id))
  else pageRows.value.forEach((r: any) => n.add(r.id))
  sel.value = n
}
// drop selected ids that no longer exist (e.g. after delete)
watch(clients, (list: any[]) => {
  if (sel.value.size === 0) return
  const ids = new Set(list.map((c: any) => c.id))
  const n = new Set([...sel.value].filter((id) => ids.has(id)))
  if (n.size !== sel.value.size) sel.value = n
})

// ---------------- cells ----------------
const daysLeft = (exp: number): number => Math.floor((exp - Date.now() / 1000) / 86400)
const quotaPct = (c: any): number =>
  c.volume ? Math.min(100, Math.round(((c.up + c.down) * 100) / c.volume)) : 0
const quotaColor = (c: any): string => {
  if (!c.volume) return 'var(--emerald)'
  const p = quotaPct(c)
  if (p >= 100) return 'var(--rose)'
  if (p > 90) return 'var(--amber)'
  return 'var(--brand)'
}
const usageTitle = (c: any): string =>
  '↓' + HumanReadable.sizeFormat(c.down) + ' - ' + HumanReadable.sizeFormat(c.up) + '↑'
const expiryTitle = (c: any): string | undefined =>
  c.expiry > 0 ? new Date(c.expiry * 1000).toLocaleString(intlLocale()) : undefined
// Rates arrive with each stats flush, and only for clients this panel's own
// core saw move traffic -- so one served by a node alone has none to show.
const rateOf = (c: any): { up: number, down: number } | undefined => dataStore.clientRates?.[c.name]
// sizeFormat answers '-' for zero, which reads as "unknown" beside a direction
// that is moving; an idle direction is a plain 0.
const rateFmt = (v: number): string =>
  (v > 0 ? HumanReadable.sizeFormat(v, 1) : '0 ' + t('stats.B')) + '/s'
// The line clips rather than widen the column, so the figures go here in full.
const rateTip = (c: any): string | undefined => {
  const r = rateOf(c)
  return r ? '↓ ' + rateFmt(r.down) + '  ↑ ' + rateFmt(r.up) + '\n' + t('ui.rateTitle') : undefined
}
// Delay start holds nextReset at zero until the first connection, so until then
// there is no boundary to show.
const hasNextReset = (c: any): boolean => !!c.autoReset && !c.delayStart && c.nextReset > 0
const resetLabel = (c: any): string => {
  const n = daysLeft(c.nextReset)
  return n >= 1 ? t('ui.resetInDays', { n }) : t('ui.resetSoon')
}
const resetTitle = (c: any): string =>
  t('ui.nextResetAt', { date: new Date(c.nextReset * 1000).toLocaleString(intlLocale()) })
const inboundsOf = (c: any): any[] =>
  (c.inbounds ?? []).map((id: number) => inbounds.value.find((i: any) => i.id == id)).filter((i: any) => !!i)
const protoSummary = (c: any): string => {
  const types = Array.from(new Set(inboundsOf(c).map((i: any) => i.type)))
  return types.length > 0 ? types.join(', ') : '—'
}
const inbTitle = (c: any): string => inboundsOf(c).map((i: any) => i.tag).join('\n')

// ---------------- quick enable / disable ----------------
// The list rows are a projection, so the full client has to be fetched before
// saving or the unprojected fields would be written back as empty.
const toggling = ref<Record<number, boolean>>({})

const toggleEnable = async (c: any) => {
  if (toggling.value[c.id]) return
  toggling.value = { ...toggling.value, [c.id]: true }
  try {
    const full = await Data().loadClients(c.id)
    if (full?.id) {
      // The counters are server-owned and only the ones a request carries are
      // taken as authoritative, so sending the values read a moment ago would
      // roll back whatever the stats job recorded in between. Same rule as the
      // drawer, which sends them only when Reset was actually clicked.
      const payload: any = { ...full, enable: !c.enable }
      delete payload.up
      delete payload.down
      delete payload.totalUp
      delete payload.totalDown
      await Data().save('clients', 'edit', payload)
    }
  } finally {
    toggling.value = { ...toggling.value, [c.id]: false }
  }
}

// ---------------- drawers / modals ----------------
const drawer = ref({ visible: false, id: 0 })
const openDrawer = (id: number) => {
  drawer.value.id = id
  drawer.value.visible = true
}

const addBulkVisible = ref(false)

const editBulkVisible = ref(false)
const editBulkSelected = ref<number[]>([])
const openEditBulk = () => {
  editBulkSelected.value = [...sel.value]
  editBulkVisible.value = true
}

const qrcode = ref({ visible: false, id: 0 })
const showQrCode = (id: number) => {
  qrcode.value.id = id
  qrcode.value.visible = true
}

const stats = ref({ visible: false, resource: 'user', tag: '' })
const showStats = (tag: string) => {
  stats.value.tag = tag
  stats.value.visible = true
}

// ---------------- IP limit ----------------
// Only clients holding at least one admitted IP are pushed, so an absent name
// means zero rather than unknown.
const ipCount = (c: any): number => dataStore.ipCounts?.[c.name] ?? 0
const ipCountTitle = computed(() =>
  (dataStore.nodes?.length ?? 0) > 0 && !dataStore.clusterIpActive
    ? t('ui.ipCountLocalHint')
    : t('ui.ipCountHint'),
)
// Two states only. The count is how many IPs the scan admitted, and it never
// admits more than the limit, so "over quota" is not a state this number can
// reach — showing it would promise a reading the backend does not publish.
const ipChipColor = (c: any): string => (ipCount(c) >= c.limitIp ? 'amber' : 'emerald')
const ips = ref({ visible: false, name: '' })
const showIps = (name: string) => {
  ips.value.name = name
  ips.value.visible = true
}
const sessions = ref({ visible: false, name: '' })
const showSessions = (name: string) => {
  sessions.value.name = name
  sessions.value.visible = true
}

// ---------------- delete (single + bulk, with confirm) ----------------
const del = ref({ visible: false, ids: [] as number[] })
const deleting = ref(false)
const askDelete = (ids: number[]) => {
  if (hasSystemClients(ids)) return
  del.value = { visible: true, ids }
}
const confirmDelete = async () => {
  if (del.value.ids.length === 0 || hasSystemClients(del.value.ids)) return
  deleting.value = true
  const success = del.value.ids.length === 1
    ? await Data().save('clients', 'del', del.value.ids[0])
    : await Data().save('clients', 'delbulk', del.value.ids)
  if (success) {
    const removed = del.value.ids
    sel.value = new Set([...sel.value].filter((id) => !removed.includes(id)))
    del.value.visible = false
  }
  deleting.value = false
}
</script>

<style scoped>
.search-wrap {
  position: relative;
  flex: 1 1 200px;
  max-width: 280px;
  min-width: 160px;
}
.search-wrap .s-ico {
  position: absolute;
  inset-inline-start: 12px;
  top: 11px;
  color: var(--text-3);
  pointer-events: none;
  display: inline-flex;
}
.search-wrap .input { padding-inline-start: 36px; }
.sel-info {
  font-size: 12.5px;
  color: var(--text-2);
  font-weight: 600;
}
td.pad-y { padding-top: 11px; padding-bottom: 11px; }
/* tighter than pad-y: the reserved rate line has to fit in the height the
   client cell already gives the row */
td.usage-td { padding-top: 5px; padding-bottom: 5px; }
.rate {
  /* width 0 + min-width 100%: fills the cell but never widens the column as
     the figures grow */
  width: 0;
  min-width: 100%;
  height: 14px;
  margin-top: 2px;
  overflow: hidden;
  text-overflow: ellipsis;
  white-space: nowrap;
  font-size: 11px;
  line-height: 14px;
  color: var(--text-3);
}
/* Each arrow and figure keeps its own order under RTL: the unit is localised
   but "/s" is not, and the bidi algorithm otherwise pulls the two apart. */
.rate-fig { unicode-bidi: isolate; direction: ltr; }
/* the dashboard's traffic legend colours: download brand, upload emerald */
.rate-dn { color: var(--brand); }
.rate-up { color: var(--emerald); }
.reset-line {
  display: flex;
  align-items: center;
  gap: 4px;
  margin-top: 4px;
  font-size: 11.5px;
  color: var(--text-3);
  white-space: nowrap;
}
td.actions-td { padding-inline-end: 10px; }
.on-dot {
  position: absolute;
  inset-inline-end: -2px;
  bottom: -2px;
  width: 10px;
  height: 10px;
  border-radius: 50%;
  background: var(--emerald);
  border: 2px solid var(--surface);
}
.off-chip {
  height: 18px;
  font-size: 10.5px;
  padding: 0 6px;
}
.t-foot {
  display: flex;
  flex-wrap: wrap;
  align-items: center;
  gap: 8px;
  padding: 12px 18px;
  font-size: 12.5px;
  color: var(--text-3);
}
/* Select renders a fragment (trigger + teleported panel), so the scope id
   never reaches its trigger — reach it through the footer instead */
.t-foot :deep(.per-page) {
  width: auto;
  /* the option panel takes the trigger's width, so leave room for "100" + check */
  min-width: 84px;
  height: 30px;
  padding: 0 10px;
  font-size: 12.5px;
}
/* mobile card list (≤820px tables become cards) */
.clients-cards {
  display: none;
  flex-direction: column;
  gap: 10px;
}
.m-client {
  padding: 14px;
  display: flex;
  flex-direction: column;
  cursor: pointer;
}
.m-head {
  display: flex;
  align-items: center;
  gap: 10px;
  margin-bottom: 8px;
}
.m-actions {
  display: flex;
  gap: 2px;
  justify-content: flex-end;
  margin-top: 8px;
  padding-top: 6px;
  border-top: 1px solid var(--line);
}
@media (max-width: 820px) {
  .clients-table { display: none; }
  .clients-cards { display: flex; }
}
</style>
