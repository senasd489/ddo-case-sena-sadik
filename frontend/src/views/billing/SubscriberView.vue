<script setup lang="ts">
import { ref } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { dateTime, kwh, label, money, periodISO } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'
import ConsumptionPanel from '../../components/ConsumptionPanel.vue'

const route = useRoute()
const auth = useAuth()
const id = Number(route.params.id)
type Detail = {
  subscriber: { subscriber_no: string; full_name: string; address: string; category: string; tariff_code: string; contract_kw: number }
  meters: { id: number; serial_no: string; installed_at: string; removed_at?: string; last_read_at?: string; last_index_wh?: number }[]
  supply_path: { id: number; code: string; kind: string }[]
}
const d = useAsync(() => api<Detail>(`/subscribers/${id}`))
const invoices = useAsync(() => api<{ items: { id: number; invoice_no: string; period_start: string; kind: string; status: string; total_kurus: number }[] }>(`/subscribers/${id}/invoices`), auth.is('billing', 'admin', 'analyst'))
const period = ref(periodISO(-1))
const msg = ref('')
async function generate(reissue = false) {
  msg.value = ''
  try {
    const r = await api<{ created: boolean; invoice: { id: number; invoice_no: string; kind: string } }>('/billing/invoices/generate', { body: { subscriber_id: id, period: period.value, reissue } })
    msg.value = `${r.created ? 'Kesildi' : 'Değişmedi (aynı girdi → aynı fatura)'}: ${r.invoice.invoice_no} (${label(r.invoice.kind)})`
    invoices.run()
  } catch (e) {
    msg.value = errText(e)
  }
}
</script>

<template>
  <StateBox :loading="d.loading.value" :error="d.error.value" @retry="d.run">
    <template v-if="d.data.value">
      <h1>{{ d.data.value.subscriber.full_name }} <span class="muted mono">{{ d.data.value.subscriber.subscriber_no }}</span></h1>
      <div class="kpis">
        <div class="kpi"><div class="k">Adres</div><div>{{ d.data.value.subscriber.address }}</div></div>
        <div class="kpi"><div class="k">Kategori / tarife</div><div>{{ label(d.data.value.subscriber.category) }} · {{ d.data.value.subscriber.tariff_code }}</div></div>
        <div class="kpi"><div class="k">Besleme yolu</div><div class="mono">{{ d.data.value.supply_path.map((p) => p.code).join(' → ') }}</div></div>
      </div>
      <section class="card">
        <h2>Sayaçlar</h2>
        <table><thead><tr><th>Seri</th><th>Kurulum</th><th>Söküm</th><th>Son okuma</th><th class="num">Son endeks</th></tr></thead>
          <tbody><tr v-for="m in d.data.value.meters" :key="m.id">
            <td class="mono">{{ m.serial_no }}</td><td>{{ dateTime(m.installed_at) }}</td><td>{{ dateTime(m.removed_at) }}</td>
            <td>{{ dateTime(m.last_read_at) }}</td><td class="num">{{ m.last_index_wh != null ? kwh(m.last_index_wh) : '—' }}</td>
          </tr></tbody></table>
      </section>
      <section v-if="auth.is('billing', 'admin')" class="card">
        <h2>Fatura üret</h2>
        <div class="row">
          <input v-model="period" type="month" aria-label="dönem" />
          <button @click="generate(false)">Üret</button>
          <button class="ghost" @click="generate(true)">Girdiler değiştiyse yeniden kes</button>
        </div>
        <p>{{ msg }}</p>
        <StateBox :loading="invoices.loading.value" :error="invoices.error.value" :empty="!invoices.data.value?.items.length" @retry="invoices.run">
          <table><tbody>
            <tr v-for="i in invoices.data.value?.items" :key="i.id" class="click" @click="$router.push(`/invoices/${i.id}`)">
              <td class="mono">{{ i.invoice_no }}</td><td><Pill :value="i.kind" /></td><td><Pill :value="i.status" /></td><td class="num">{{ money(i.total_kurus) }}</td>
            </tr>
          </tbody></table>
        </StateBox>
      </section>
      <ConsumptionPanel :subscriber-id="id" />
    </template>
  </StateBox>
</template>
