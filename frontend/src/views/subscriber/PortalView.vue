<script setup lang="ts">
import { computed, ref, watch } from 'vue'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { useAsync } from '../../lib/useAsync'
import { dateTime, kwh, label, money, periodISO } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'
import ConsumptionPanel from '../../components/ConsumptionPanel.vue'

const auth = useAuth()
const sid = computed(() => auth.user!.subscriber_id!)
type Invoice = { id: number; invoice_no: string; period_start: string; kind: string; status: string; total_kurus: number; paid_kurus: number; consumption_wh: number }
type Outage = { id: number; status: string; kind: string; started_at: string; estimated_restore_at?: string; planned_start?: string; planned_end?: string; fault_node_code: string }

const sub = useAsync(() => api<{ subscriber: { subscriber_no: string; full_name: string; address: string; category: string; tariff_code: string; contract_kw: number }; supply_path: { code: string; kind: string }[] }>(`/subscribers/${sid.value}`))
const invoices = useAsync(() => api<{ items: Invoice[] }>(`/subscribers/${sid.value}/invoices`))
const outages = useAsync(() => api<{ items: Outage[] }>('/me/outages'))
const period = ref(periodISO(0))
const preview = useAsync(() => api<{ estimated: boolean; result: { total_kurus: number; consumption_wh: number; estimated_wh: number } }>(`/subscribers/${sid.value}/invoice-preview?period=${period.value}`))
watch(period, () => preview.run())
</script>

<template>
  <h1>Aboneliğim</h1>
  <StateBox :loading="sub.loading.value" :error="sub.error.value" @retry="sub.run">
    <div v-if="sub.data.value" class="kpis">
      <div class="kpi"><div class="k">Abone no</div><div class="v">{{ sub.data.value.subscriber.subscriber_no }}</div></div>
      <div class="kpi"><div class="k">{{ sub.data.value.subscriber.full_name }}</div><div>{{ sub.data.value.subscriber.address }}</div></div>
      <div class="kpi"><div class="k">Tarife</div><div class="v">{{ sub.data.value.subscriber.tariff_code }}</div><div class="muted">{{ label(sub.data.value.subscriber.category) }} · {{ sub.data.value.subscriber.contract_kw }} kW</div></div>
      <div class="kpi"><div class="k">Besleme</div><div class="mono">{{ sub.data.value.supply_path.map((p) => p.code).join(' → ') }}</div></div>
    </div>
  </StateBox>

  <section class="card">
    <h2>Beslememdeki kesintiler</h2>
    <StateBox :loading="outages.loading.value" :error="outages.error.value" :empty="!outages.data.value?.items.length" empty-text="Şu an bölgenizde açık kesinti yok." @retry="outages.run">
      <table><tbody>
        <tr v-for="o in outages.data.value?.items" :key="o.id">
          <td><Pill :value="o.kind" /> <Pill :value="o.status" /></td>
          <td>Başlangıç: {{ dateTime(o.planned_start ?? o.started_at) }}</td>
          <td>Tahmini düzelme: <b>{{ dateTime(o.estimated_restore_at ?? o.planned_end) }}</b></td>
        </tr>
      </tbody></table>
    </StateBox>
  </section>

  <div class="grid">
    <section class="card">
      <div class="row between"><h2>Tahmini fatura</h2><input v-model="period" type="month" aria-label="dönem" /></div>
      <StateBox :loading="preview.loading.value" :error="preview.error.value" @retry="preview.run">
        <template v-if="preview.data.value">
          <div class="kpi"><div class="k">{{ period }} dönemi</div><div class="v">{{ money(preview.data.value.result.total_kurus) }}</div>
            <div class="muted">{{ kwh(preview.data.value.result.consumption_wh) }}<span v-if="preview.data.value.estimated">, bunun {{ kwh(preview.data.value.result.estimated_wh) }} kısmı tahmin (henüz ölçülmeyen günler)</span></div>
          </div>
        </template>
      </StateBox>
    </section>
    <section class="card">
      <h2>Faturalarım</h2>
      <StateBox :loading="invoices.loading.value" :error="invoices.error.value" :empty="!invoices.data.value?.items.length" empty-text="Henüz kesilmiş fatura yok." @retry="invoices.run">
        <table>
          <thead><tr><th>Dönem</th><th>Tür</th><th>Durum</th><th class="num">Tutar</th></tr></thead>
          <tbody>
            <tr v-for="i in invoices.data.value?.items" :key="i.id" class="click" @click="$router.push(`/invoices/${i.id}`)">
              <td>{{ i.period_start.slice(0, 7) }}</td><td><Pill :value="i.kind" /></td><td><Pill :value="i.status" /></td><td class="num">{{ money(i.total_kurus) }}</td>
            </tr>
          </tbody>
        </table>
      </StateBox>
    </section>
  </div>
  <ConsumptionPanel :subscriber-id="sid" />
</template>
