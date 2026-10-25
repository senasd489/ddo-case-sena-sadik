<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { dateTime, label } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'

type Detail = {
  outage: { id: number; kind: string; status: string; fault_node_id: number; fault_node_code: string; feeder_id: number; required_skill: string; cause?: string
    started_at: string; ended_at?: string; estimated_restore_at?: string; affected_count: number; report_count: number; version: number }
  history: { from_status?: string; to_status: string; changed_at: string; note?: string }[]
  affected: { interrupted: number; restored: number }
}
const route = useRoute()
const auth = useAuth()
const id = route.params.id
const d = useAsync(() => api<Detail>(`/outages/${id}`))
const signals = useAsync(() => api<{ items: { id: number; source: string; subscriber_id: number; received_at: string; duplicate: boolean }[] }>(`/outages/${id}/signals`))
const msg = ref('')
const next: Record<string, string[]> = {
  detected: ['confirmed', 'resolved', 'cancelled'], confirmed: ['resolved', 'cancelled'], assigned: ['in_progress', 'resolved', 'cancelled'],
  in_progress: ['resolved'], scheduled: ['in_progress', 'cancelled'],
}
const options = computed(() => (d.data.value ? next[d.data.value.outage.status] ?? [] : []))
async function act(fn: () => Promise<unknown>) {
  msg.value = ''
  try {
    await fn()
    await Promise.all([d.run(), signals.run()])
  } catch (e) {
    msg.value = errText(e)
  }
}
const move = (to: string) => act(() => api(`/outages/${id}/transition`, { body: { to, version: d.data.value?.outage.version } }))
const dispatch = () => act(() => api(`/outages/${id}/dispatch`, { method: 'POST' }))
const release = () => act(() => api(`/outages/${id}/release`, { method: 'POST' }))
</script>

<template>
  <StateBox :loading="d.loading.value && !d.data.value" :error="d.error.value" @retry="d.run">
    <template v-if="d.data.value">
      <div class="row between">
        <h1>Kesinti #{{ d.data.value.outage.id }} <Pill :value="d.data.value.outage.status" /></h1>
        <RouterLink :to="`/network?node=${d.data.value.outage.fault_node_id}`">Şebekede göster</RouterLink>
      </div>
      <div class="kpis">
        <div class="kpi"><div class="k">Arıza noktası</div><div class="v mono">{{ d.data.value.outage.fault_node_code }}</div><div class="muted">{{ label(d.data.value.outage.kind) }} · yetkinlik {{ d.data.value.outage.required_skill }}</div></div>
        <div class="kpi"><div class="k">Enerjisiz / geri gelen</div><div class="v">{{ d.data.value.affected.interrupted }} / {{ d.data.value.affected.restored }}</div></div>
        <div class="kpi"><div class="k">Bildirim</div><div class="v">{{ d.data.value.outage.report_count }}</div></div>
        <div class="kpi"><div class="k">Tahmini düzelme</div><div class="v">{{ dateTime(d.data.value.outage.estimated_restore_at) }}</div></div>
      </div>
      <section v-if="auth.is('operator', 'admin')" class="card">
        <h2>İşlemler</h2>
        <div class="row">
          <button v-if="d.data.value.outage.status === 'confirmed'" @click="dispatch">En yakın ekibi ata</button>
          <button v-if="d.data.value.outage.status === 'assigned'" class="ghost" @click="release">Ekibi geri çek</button>
          <button v-for="o in options" :key="o" class="ghost" @click="move(o)">→ {{ label(o) }}</button>
        </div>
        <p class="err">{{ msg }}</p>
      </section>
      <div class="grid">
        <section class="card">
          <h2>Durum geçmişi</h2>
          <table><tbody>
            <tr v-for="(h, i) in d.data.value.history" :key="i"><td>{{ dateTime(h.changed_at) }}</td><td>{{ label(h.from_status) }} → <b>{{ label(h.to_status) }}</b></td><td class="muted">{{ h.note }}</td></tr>
          </tbody></table>
        </section>
        <section class="card">
          <h2>Bildirimler</h2>
          <StateBox :loading="signals.loading.value" :error="signals.error.value" :empty="!signals.data.value?.items.length" empty-text="Bu kesintiye bağlı bildirim yok (elle açılmış)." @retry="signals.run">
            <div class="table-wrap" style="max-height: 360px; overflow-y: auto"><table>
              <thead><tr><th>Zaman</th><th>Kaynak</th><th>Abone</th><th></th></tr></thead>
              <tbody><tr v-for="s in signals.data.value?.items" :key="s.id">
                <td>{{ dateTime(s.received_at) }}</td><td>{{ s.source === 'last_gasp' ? 'Son nefes' : s.source === 'call' ? 'Arama' : 'Enerji geldi' }}</td>
                <td>{{ s.subscriber_id }}</td><td><span v-if="s.duplicate" class="pill warn">mükerrer</span></td>
              </tr></tbody>
            </table></div>
          </StateBox>
        </section>
      </div>
    </template>
  </StateBox>
</template>
