<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRoute } from 'vue-router'
import { api, idemKey } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { kwh, money, dateTime } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'

type Line = { kind: string; description: string; quantity: string; unit_price: string; amount_kurus: number }
type Day = { day: string; active_wh: number; slots: number; interpolated_slots: number; estimated: boolean; estimated_wh: number }
type Invoice = {
  id: number; invoice_no: string; subscriber_id: number; period_start: string; period_end: string; kind: string; status: string; revision: number
  consumption_wh: number; estimated_wh: number; subtotal_kurus: number; tax_kurus: number; total_kurus: number; paid_kurus: number
  input_hash: string; settles_invoice_id?: number; created_at: string; lines: Line[]; breakdown: Day[]
}
const route = useRoute()
const auth = useAuth()
const inv = useAsync(() => api<Invoice>(`/invoices/${route.params.id}`))
const open = computed(() => (inv.data.value ? inv.data.value.total_kurus - inv.data.value.paid_kurus : 0))
const amount = ref<number | null>(null)
const payKey = ref(idemKey()) // aynı ödeme denemesi tekrar gönderilirse aynı anahtar → çift ödeme olmaz
const msg = ref('')
const busy = ref(false)
async function pay() {
  busy.value = true
  msg.value = ''
  try {
    const kurus = Math.round((amount.value ?? open.value / 100) * 100)
    await api(`/invoices/${route.params.id}/payments`, { body: { amount_kurus: kurus }, idempotent: payKey.value })
    payKey.value = idemKey()
    msg.value = 'Ödeme alındı.'
    amount.value = null
    await inv.run()
  } catch (e) {
    msg.value = errText(e)
  } finally {
    busy.value = false
  }
}
const interpolatedDays = computed(() => inv.data.value?.breakdown.filter((d) => d.interpolated_slots > 0).length ?? 0)
const estimatedDays = computed(() => inv.data.value?.breakdown.filter((d) => d.estimated).length ?? 0)
</script>

<template>
  <StateBox :loading="inv.loading.value" :error="inv.error.value" @retry="inv.run">
    <template v-if="inv.data.value">
      <div class="row between">
        <h1>Fatura {{ inv.data.value.invoice_no }}</h1>
        <div class="row"><Pill :value="inv.data.value.kind" /><Pill :value="inv.data.value.status" /></div>
      </div>
      <div class="kpis">
        <div class="kpi"><div class="k">Dönem</div><div class="v">{{ inv.data.value.period_start }} – {{ inv.data.value.period_end }}</div></div>
        <div class="kpi"><div class="k">Tüketim</div><div class="v">{{ kwh(inv.data.value.consumption_wh) }}</div><div v-if="inv.data.value.estimated_wh" class="muted">tahmin: {{ kwh(inv.data.value.estimated_wh) }}</div></div>
        <div class="kpi"><div class="k">Toplam</div><div class="v">{{ money(inv.data.value.total_kurus) }}</div><div class="muted">ödenen {{ money(inv.data.value.paid_kurus) }}</div></div>
        <div class="kpi"><div class="k">Revizyon / kesim</div><div class="v">{{ inv.data.value.revision }}</div><div class="muted">{{ dateTime(inv.data.value.created_at) }}</div></div>
      </div>
      <section class="card">
        <h2>Kalemler</h2>
        <div class="table-wrap"><table>
          <thead><tr><th>Kalem</th><th class="num">Miktar</th><th class="num">Birim (kr)</th><th class="num">Tutar</th></tr></thead>
          <tbody>
            <tr v-for="(l, i) in inv.data.value.lines" :key="i">
              <td>{{ l.description }}</td><td class="num">{{ Number(l.quantity) ? l.quantity : '' }}</td>
              <td class="num">{{ Number(l.unit_price) ? l.unit_price : '' }}</td><td class="num">{{ money(l.amount_kurus) }}</td>
            </tr>
            <tr><td><b>Ara toplam</b></td><td></td><td></td><td class="num">{{ money(inv.data.value.subtotal_kurus) }}</td></tr>
            <tr><td><b>Vergi ve fonlar</b></td><td></td><td></td><td class="num">{{ money(inv.data.value.tax_kurus) }}</td></tr>
            <tr><td><b>Genel toplam</b></td><td></td><td></td><td class="num"><b>{{ money(inv.data.value.total_kurus) }}</b></td></tr>
          </tbody>
        </table></div>
        <p class="muted mono">Girdi özeti: {{ inv.data.value.input_hash.slice(0, 16) }}… (aynı girdiyle yeniden üretim aynı faturayı verir)</p>
      </section>
      <section v-if="inv.data.value.status === 'issued' && auth.is('subscriber', 'billing', 'admin') && open > 0" class="card">
        <h2>Ödeme</h2>
        <form class="row" @submit.prevent="pay">
          <label>Tutar (TL) <input v-model.number="amount" type="number" step="0.01" min="0.01" :placeholder="(open / 100).toFixed(2)" /></label>
          <button :disabled="busy">Öde</button>
          <span :class="msg === 'Ödeme alındı.' ? 'okt' : 'err'">{{ msg }}</span>
        </form>
      </section>
      <section class="card">
        <h2>Günlük döküm</h2>
        <p class="muted">{{ interpolatedDays }} günde eksik okumalar interpolasyonla dolduruldu; {{ estimatedDays }} gün ölçülemediği için tahmin edildi.</p>
        <div class="table-wrap"><table>
          <thead><tr><th>Gün</th><th class="num">Tüketim</th><th class="num">Ölçülen slot</th><th class="num">İnterpolasyon</th><th>Not</th></tr></thead>
          <tbody>
            <tr v-for="d in inv.data.value.breakdown" :key="d.day">
              <td>{{ d.day.slice(0, 10) }}</td><td class="num">{{ kwh(d.active_wh) }}</td><td class="num">{{ d.slots }}/96</td>
              <td class="num">{{ d.interpolated_slots || '' }}</td>
              <td><Pill v-if="d.estimated" value="estimated" /> <Pill v-if="d.interpolated_slots" value="interpolated" /></td>
            </tr>
          </tbody>
        </table></div>
      </section>
    </template>
  </StateBox>
</template>
