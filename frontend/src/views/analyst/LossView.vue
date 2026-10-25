<script setup lang="ts">
import { computed, ref } from 'vue'
import type { ChartConfiguration } from 'chart.js'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { daysAgoISO, kwh, label, todayISO } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'
import Chart from '../../components/Chart.vue'

type Tr = { id: number; code: string; days: number; head_wh: number; subscribers_wh: number; loss_wh: number; excess_wh: number; loss_pct: number; tolerance_pct: number; excess_days: number; open_flags: number }
type Suspect = { id: number; subscriber_id: number; subscriber_no: string; full_name: string; rule: string; score: number; status: string; explanation: Record<string, any> }
const auth = useAuth()
const from = ref(daysAgoISO(7))
const to = ref(todayISO())
const trs = useAsync(() => api<{ items: Tr[] }>(`/analytics/loss/transformers?from=${from.value}&to=${to.value}`))
const status = ref('open')
const items = ref<Suspect[]>([])
const next = ref<string | null>(null)
const sus = useAsync(async () => {
  const r = await api<{ items: Suspect[]; next_cursor: string | null }>(`/analytics/suspects?limit=20&status=${status.value}`)
  items.value = r.items
  next.value = r.next_cursor
  return r
}, auth.is('analyst', 'admin'))
async function more() {
  const r = await api<{ items: Suspect[]; next_cursor: string | null }>(`/analytics/suspects?limit=20&status=${status.value}&cursor=${next.value}`)
  items.value.push(...r.items)
  next.value = r.next_cursor
}
const running = ref(false)
const runMsg = ref('')
async function run() {
  running.value = true
  runMsg.value = ''
  try {
    const r = await api<{ by_rule: Record<string, number>; duration_ms: number }>('/analytics/loss/run', { body: {} })
    runMsg.value = `Analiz ${r.duration_ms} ms sürdü: ` + (Object.entries(r.by_rule).map(([k, v]) => `${label(k)} ${v}`).join(', ') || 'şüpheli yok')
    await Promise.all([trs.run(), sus.run()])
  } catch (e) {
    runMsg.value = errText(e)
  } finally {
    running.value = false
  }
}
async function review(s: Suspect, st: 'confirmed' | 'dismissed') {
  try {
    await api(`/analytics/suspects/${s.id}/review`, { body: { status: st } })
    items.value = items.value.filter((x) => x.id !== s.id)
  } catch (e) {
    runMsg.value = errText(e)
  }
}
const chart = computed<ChartConfiguration>(() => {
  const list = (trs.data.value?.items ?? []).filter((t) => t.days > 0).slice(0, 25)
  return {
    type: 'bar',
    data: {
      labels: list.map((t) => t.code),
      datasets: [
        { label: 'Kayıp %', data: list.map((t) => t.loss_pct), backgroundColor: list.map((t) => (t.loss_pct > t.tolerance_pct ? '#d9534f' : '#5b9bd5')) },
        { label: 'Teknik tolerans %', data: list.map((t) => t.tolerance_pct), type: 'line', borderColor: '#f2b134', pointRadius: 2 } as any,
      ],
    },
    options: { scales: { y: { title: { display: true, text: '%' } } } },
  }
})
</script>

<template>
  <div class="row between">
    <h1>Kayıp-kaçak</h1>
    <button v-if="auth.is('analyst', 'admin')" :disabled="running" @click="run">{{ running ? 'Analiz ediliyor…' : 'Analizi çalıştır' }}</button>
  </div>
  <p v-if="runMsg">{{ runMsg }}</p>
  <section class="card">
    <div class="row between">
      <h2>Trafo dengesi (ana sayaç − abone toplamı)</h2>
      <div class="row"><input v-model="from" type="date" /> – <input v-model="to" type="date" /><button class="ghost" @click="trs.run">Göster</button></div>
    </div>
    <StateBox :loading="trs.loading.value" :error="trs.error.value" :empty="!trs.data.value?.items.some((t) => t.days)" empty-text="Bu aralık için denge hesaplanmamış. 'Analizi çalıştır' ile hesaplayın." @retry="trs.run">
      <Chart :config="chart" />
      <div class="table-wrap"><table>
        <thead><tr><th>Trafo</th><th class="num">Gün</th><th class="num">Ana sayaç</th><th class="num">Aboneler</th><th class="num">Kayıp %</th><th class="num">Tolerans %</th><th class="num">Tolerans üstü gün</th><th class="num">Açık işaret</th></tr></thead>
        <tbody><tr v-for="t in trs.data.value?.items.filter((x) => x.days)" :key="t.id">
          <td class="mono">{{ t.code }}</td><td class="num">{{ t.days }}</td><td class="num">{{ kwh(t.head_wh) }}</td><td class="num">{{ kwh(t.subscribers_wh) }}</td>
          <td class="num" :class="{ err: t.loss_pct > t.tolerance_pct }">{{ t.loss_pct }}</td><td class="num">{{ t.tolerance_pct }}</td>
          <td class="num">{{ t.excess_days }}</td><td class="num">{{ t.open_flags }}</td>
        </tr></tbody>
      </table></div>
    </StateBox>
  </section>
  <section v-if="auth.is('analyst', 'admin')" class="card">
    <div class="row between">
      <h2>Şüpheli aboneler</h2>
      <select v-model="status" aria-label="durum" @change="sus.run()"><option value="open">Açık</option><option value="confirmed">Doğrulanan</option><option value="dismissed">Yanlış alarm</option></select>
    </div>
    <p class="muted">Robust istatistik (medyan/MAD), veri kalitesi ve kesinti/sayaç değişimi günlerinin dışlanması, trafo kaybında kalıcılık ve çapraz kanıt ile yanlış alarm azaltılır.</p>
    <StateBox :loading="sus.loading.value" :error="sus.error.value" :empty="!items.length" empty-text="Bu durumda şüpheli yok." @retry="sus.run">
      <div v-for="s in items" :key="s.id" class="card" style="margin-bottom: .6rem">
        <div class="row between">
          <div class="row">
            <RouterLink :to="`/subscribers/${s.subscriber_id}`" class="mono">{{ s.subscriber_no }}</RouterLink> {{ s.full_name }}
            <Pill :value="s.rule" /> <Pill :value="s.explanation.confidence" /> <span class="muted">skor {{ s.score.toFixed(1) }}</span>
          </div>
          <div v-if="s.status === 'open'" class="row">
            <button class="ghost" @click="review(s, 'confirmed')">Doğrula</button>
            <button class="ghost" @click="review(s, 'dismissed')">Yanlış alarm</button>
          </div>
        </div>
        <p style="margin: .4rem 0 0">{{ s.explanation.reason }}</p>
      </div>
      <button v-if="next" class="ghost" @click="more">Daha fazla</button>
    </StateBox>
  </section>
</template>
