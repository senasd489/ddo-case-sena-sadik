<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { ago, dateTime, label } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'

type Outage = { id: number; kind: string; status: string; fault_node_code: string; required_skill: string; started_at: string; planned_start?: string; planned_end?: string; estimated_restore_at?: string; affected_count: number; report_count: number }
const auth = useAuth()
const filter = ref<'open' | 'all'>('open')
const items = ref<Outage[]>([])
const next = ref<string | null>(null)
const list = useAsync(async () => {
  const r = await api<{ items: Outage[]; next_cursor: string | null }>(`/outages?limit=50${filter.value === 'open' ? '&open=true' : ''}`)
  items.value = r.items
  next.value = r.next_cursor
  return r
})
async function loadMore() {
  const r = await api<{ items: Outage[]; next_cursor: string | null }>(`/outages?limit=50${filter.value === 'open' ? '&open=true' : ''}&cursor=${next.value}`)
  items.value.push(...r.items)
  next.value = r.next_cursor
}
let timer: number | undefined
onMounted(() => (timer = window.setInterval(() => !list.loading.value && list.run(), 10_000)))
onBeforeUnmount(() => clearInterval(timer))

const showPlan = ref(false)
const plan = ref({ fault_node_id: 2, planned_start: '', planned_end: '', cause: 'Planlı bakım' })
const planMsg = ref('')
async function createPlanned() {
  planMsg.value = ''
  try {
    const o = await api<Outage>('/outages', { body: { kind: 'planned', fault_node_id: plan.value.fault_node_id, cause: plan.value.cause,
      planned_start: new Date(plan.value.planned_start).toISOString(), planned_end: new Date(plan.value.planned_end).toISOString() }, idempotent: true })
    planMsg.value = `Planlı kesinti #${o.id} oluşturuldu (${o.affected_count} abone).`
    list.run()
  } catch (e) {
    planMsg.value = errText(e)
  }
}
</script>

<template>
  <div class="row between">
    <h1>Kesintiler</h1>
    <div class="row">
      <select v-model="filter" aria-label="filtre" @change="list.run()"><option value="open">Açık olanlar</option><option value="all">Tümü</option></select>
      <button v-if="auth.is('operator', 'admin')" class="ghost" @click="showPlan = !showPlan">Planlı kesinti</button>
    </div>
  </div>
  <section v-if="showPlan" class="card">
    <h2>Planlı kesinti oluştur</h2>
    <form @submit.prevent="createPlanned">
      <div class="grid">
        <label>Düğüm id (besleyici/direk/trafo) <input v-model.number="plan.fault_node_id" type="number" min="1" required /></label>
        <label>Başlangıç <input v-model="plan.planned_start" type="datetime-local" required /></label>
        <label>Bitiş <input v-model="plan.planned_end" type="datetime-local" required /></label>
        <label>Neden <input v-model="plan.cause" /></label>
      </div>
      <button>Oluştur</button> <span>{{ planMsg }}</span>
      <p class="muted">Aynı besleme yolunda (üst/alt düğüm) zamanı çakışan başka planlı kesinti varsa reddedilir.</p>
    </form>
  </section>
  <section class="card">
    <p class="muted">Sayaç "son nefes" sinyalleri ve abone aramaları aynı besleyici ve 15 dk pencerede tek olaya birleştirilir. Liste 10 sn'de bir yenilenir.</p>
    <StateBox :loading="list.loading.value && !items.length" :error="list.error.value" :empty="!items.length" empty-text="Açık kesinti yok." @retry="list.run">
      <div class="table-wrap"><table>
        <thead><tr><th>#</th><th>Tür</th><th>Durum</th><th>Arıza noktası</th><th class="num">Abone</th><th class="num">Bildirim</th><th>Başlangıç</th><th class="hide-sm">Tahmini düzelme</th></tr></thead>
        <tbody>
          <tr v-for="o in items" :key="o.id" class="click" @click="$router.push(`/outages/${o.id}`)">
            <td>{{ o.id }}</td><td>{{ label(o.kind) }}</td><td><Pill :value="o.status" /></td><td class="mono">{{ o.fault_node_code }}</td>
            <td class="num">{{ o.affected_count }}</td><td class="num">{{ o.report_count }}</td>
            <td :title="dateTime(o.started_at)">{{ o.kind === 'planned' ? dateTime(o.planned_start) : ago(o.started_at) }}</td>
            <td class="hide-sm">{{ dateTime(o.estimated_restore_at ?? o.planned_end) }}</td>
          </tr>
        </tbody>
      </table></div>
      <button v-if="next" class="ghost" @click="loadMore">Daha fazla</button>
    </StateBox>
  </section>
</template>
