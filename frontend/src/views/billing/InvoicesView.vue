<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { kwh, money, periodISO } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'

type Inv = { id: number; invoice_no: string; subscriber_id: number; period_start: string; kind: string; status: string; consumption_wh: number; total_kurus: number }
const auth = useAuth()
const period = ref('')
const status = ref('')
const items = ref<Inv[]>([])
const next = ref<string | null>(null)
const list = useAsync(async () => {
  const r = await api<{ items: Inv[]; next_cursor: string | null }>(`/invoices?limit=30&period=${period.value}&status=${status.value}`)
  items.value = r.items
  next.value = r.next_cursor
  return r
})
async function loadMore() {
  const r = await api<{ items: Inv[]; next_cursor: string | null }>(`/invoices?limit=30&period=${period.value}&status=${status.value}&cursor=${next.value}`)
  items.value.push(...r.items)
  next.value = r.next_cursor
}
const runPeriod = ref(periodISO(-1))
const runLimit = ref(100)
const runMsg = ref('')
const running = ref(false)
async function run() {
  running.value = true
  runMsg.value = ''
  try {
    const r = await api<{ subscribers: number; result: Record<string, number> }>('/billing/runs', { body: { period: runPeriod.value, limit: runLimit.value } })
    runMsg.value = `${r.subscribers} abone: ` + Object.entries(r.result).map(([k, v]) => `${k}=${v}`).join(', ')
    list.run()
  } catch (e) {
    runMsg.value = errText(e)
  } finally {
    running.value = false
  }
}
</script>

<template>
  <h1>Faturalar</h1>
  <section v-if="auth.is('billing', 'admin')" class="card">
    <h2>Dönem faturalama</h2>
    <div class="row">
      <input v-model="runPeriod" type="month" aria-label="dönem" />
      <label>İlk N abone <input v-model.number="runLimit" type="number" min="1" max="10000" /></label>
      <button :disabled="running" @click="run">{{ running ? 'Çalışıyor…' : 'Faturala' }}</button>
    </div>
    <p class="muted">{{ runMsg || 'Aynı dönem tekrar çalıştırılırsa değişmeyen faturalar aynen kalır ("unchanged").' }}</p>
  </section>
  <section class="card">
    <form class="row" @submit.prevent="list.run">
      <input v-model="period" type="month" aria-label="dönem" />
      <select v-model="status" aria-label="durum"><option value="">Tüm durumlar</option><option value="issued">Kesildi</option><option value="paid">Ödendi</option><option value="settled">Mahsup</option><option value="cancelled">İptal</option></select>
      <button>Filtrele</button>
    </form>
    <StateBox :loading="list.loading.value" :error="list.error.value" :empty="!items.length" empty-text="Fatura yok." @retry="list.run">
      <div class="table-wrap"><table>
        <thead><tr><th>No</th><th>Abone</th><th>Dönem</th><th>Tür</th><th>Durum</th><th class="num">Tüketim</th><th class="num">Tutar</th></tr></thead>
        <tbody>
          <tr v-for="i in items" :key="i.id" class="click" @click="$router.push(`/invoices/${i.id}`)">
            <td class="mono">{{ i.invoice_no }}</td><td>{{ i.subscriber_id }}</td><td>{{ i.period_start.slice(0, 7) }}</td>
            <td><Pill :value="i.kind" /></td><td><Pill :value="i.status" /></td><td class="num">{{ kwh(i.consumption_wh) }}</td><td class="num">{{ money(i.total_kurus) }}</td>
          </tr>
        </tbody>
      </table></div>
      <button v-if="next" class="ghost" @click="loadMore">Daha fazla</button>
    </StateBox>
  </section>
</template>
