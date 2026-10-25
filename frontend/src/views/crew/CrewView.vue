<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../../api/client'
import { errText, useAsync } from '../../lib/useAsync'
import { dateTime, label } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'

type Me = {
  crew: { code: string; name: string; status: string; skills: string[] }
  outage: null | { id: number; status: string; fault_node_code: string; affected_count: number; estimated_restore_at?: string; required_skill: string; started_at: string }
}
const me = useAsync(() => api<Me>('/crew/me'))
const note = ref('')
const msg = ref('')
async function move(to: string) {
  msg.value = ''
  try {
    await api('/crew/me/transition', { body: { to, note: note.value } })
    note.value = ''
    msg.value = to === 'resolved' ? 'Arıza giderildi olarak kapatıldı, ekip müsait.' : 'Çalışma başladı.'
    me.run()
  } catch (e) {
    msg.value = errText(e)
  }
}
const rd = ref({ meter_id: 1, read_at: '', active_index_wh: 0, reactive_index_varh: 0 })
const rdMsg = ref('')
async function reading() {
  rdMsg.value = ''
  try {
    const t = new Date(rd.value.read_at)
    t.setMinutes(Math.floor(t.getMinutes() / 15) * 15, 0, 0)
    const r = await api<{ inserted: number; corrections: number; duplicates: number }>('/readings', { body: { ...rd.value, read_at: t.toISOString() }, idempotent: true })
    rdMsg.value = `Kaydedildi (yeni ${r.inserted}, düzeltme ${r.corrections}, tekrar ${r.duplicates})`
  } catch (e) {
    rdMsg.value = errText(e)
  }
}
const mc = ref({ subscriber_id: 1, at: '', final_index_wh: 0, new_serial_no: '', initial_index_wh: 0 })
const mcMsg = ref('')
async function meterChange() {
  mcMsg.value = ''
  try {
    const t = new Date(mc.value.at)
    t.setMinutes(Math.floor(t.getMinutes() / 15) * 15, 0, 0)
    const { subscriber_id, ...body } = mc.value
    const m = await api<{ id: number; serial_no: string }>(`/subscribers/${subscriber_id}/meter-change`, { body: { ...body, at: t.toISOString() }, idempotent: true })
    mcMsg.value = `Yeni sayaç #${m.id} (${m.serial_no}) takıldı.`
  } catch (e) {
    mcMsg.value = errText(e)
  }
}
</script>

<template>
  <h1>Görevim</h1>
  <StateBox :loading="me.loading.value" :error="me.error.value" @retry="me.run">
    <template v-if="me.data.value">
      <section class="card">
        <div class="row between"><h2>{{ me.data.value.crew.name }} ({{ me.data.value.crew.code }})</h2><Pill :value="me.data.value.crew.status" /></div>
        <p class="muted">Yetkinlikler: {{ me.data.value.crew.skills.join(', ') }}</p>
        <div v-if="me.data.value.outage">
          <p><b>Kesinti #{{ me.data.value.outage.id }}</b> — arıza noktası <span class="mono">{{ me.data.value.outage.fault_node_code }}</span>, {{ me.data.value.outage.affected_count }} abone etkileniyor.
            <Pill :value="me.data.value.outage.status" /></p>
          <p>Başlangıç {{ dateTime(me.data.value.outage.started_at) }} · tahmini onarım <b>{{ dateTime(me.data.value.outage.estimated_restore_at) }}</b></p>
          <label>Not <input v-model="note" placeholder="ör. sigorta değiştirildi" /></label>
          <div class="row">
            <button v-if="me.data.value.outage.status === 'assigned'" @click="move('in_progress')">Sahaya ulaştım, çalışmaya başla</button>
            <button v-if="['assigned', 'in_progress'].includes(me.data.value.outage.status)" class="ghost" @click="move('resolved')">Arıza giderildi</button>
          </div>
        </div>
        <p v-else class="muted">Atanmış aktif bir göreviniz yok.</p>
        <p>{{ msg }}</p>
      </section>
    </template>
  </StateBox>
  <div class="grid">
    <section class="card">
      <h2>Elle okuma girişi</h2>
      <form @submit.prevent="reading">
        <label>Sayaç id <input v-model.number="rd.meter_id" type="number" min="1" required /></label>
        <label>Okuma zamanı (15 dk'ya yuvarlanır) <input v-model="rd.read_at" type="datetime-local" required /></label>
        <label>Aktif endeks (Wh) <input v-model.number="rd.active_index_wh" type="number" min="0" required /></label>
        <label>Reaktif endeks (VArh) <input v-model.number="rd.reactive_index_varh" type="number" min="0" /></label>
        <button>Kaydet</button> <span>{{ rdMsg }}</span>
      </form>
    </section>
    <section class="card">
      <h2>Sayaç değişimi</h2>
      <form @submit.prevent="meterChange">
        <label>Abone id <input v-model.number="mc.subscriber_id" type="number" min="1" required /></label>
        <label>Değişim zamanı <input v-model="mc.at" type="datetime-local" required /></label>
        <label>Eski sayaç kapanış endeksi (Wh) <input v-model.number="mc.final_index_wh" type="number" min="0" required /></label>
        <label>Yeni sayaç seri no <input v-model="mc.new_serial_no" required /></label>
        <label>Yeni sayaç açılış endeksi (Wh) <input v-model.number="mc.initial_index_wh" type="number" min="0" /></label>
        <button>Değişimi kaydet</button> <span>{{ mcMsg }}</span>
      </form>
      <p class="muted">{{ label('meter_change') }} anı iki sayaç arasında bölünür; eski ve yeni endeks arasında sahte bir fark oluşmaz.</p>
    </section>
  </div>
</template>
