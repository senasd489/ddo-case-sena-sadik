<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../../api/client'
import { useAsync } from '../../lib/useAsync'
import { daysAgoISO, n, todayISO } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'

type Rel = { customers_served: number; customer_interruptions: number; customer_minutes_interrupted: number; saidi_minutes: number; saifi: number; caidi_minutes: number
  unplanned: { saidi_minutes: number; saifi: number }; planned: { saidi_minutes: number; saifi: number } }
const from = ref(daysAgoISO(30))
const to = ref(todayISO())
const feeder = ref<number | null>(null)
const rel = useAsync(() => api<Rel>(`/reports/reliability?from=${from.value}&to=${to.value}${feeder.value ? `&feeder_id=${feeder.value}` : ''}`))
</script>

<template>
  <h1>Güvenilirlik göstergeleri</h1>
  <section class="card">
    <form class="row" @submit.prevent="rel.run">
      <input v-model="from" type="date" /> – <input v-model="to" type="date" />
      <input v-model.number="feeder" type="number" min="1" placeholder="Besleyici/trafo id (ops.)" style="width: 14rem" />
      <button>Hesapla</button>
    </form>
    <p class="muted">IEEE 1366: SAIDI = Σ abone-dakika / hizmet verilen abone; SAIFI = Σ kesintiye uğrayan abone / hizmet verilen abone. 3 dakikadan kısa kesintiler sayılmaz.</p>
  </section>
  <StateBox :loading="rel.loading.value" :error="rel.error.value" @retry="rel.run">
    <div v-if="rel.data.value" class="kpis">
      <div class="kpi"><div class="k">SAIDI (dk/abone)</div><div class="v">{{ rel.data.value.saidi_minutes }}</div><div class="muted">plansız {{ rel.data.value.unplanned.saidi_minutes }} · planlı {{ rel.data.value.planned.saidi_minutes }}</div></div>
      <div class="kpi"><div class="k">SAIFI (kesinti/abone)</div><div class="v">{{ rel.data.value.saifi }}</div><div class="muted">plansız {{ rel.data.value.unplanned.saifi }} · planlı {{ rel.data.value.planned.saifi }}</div></div>
      <div class="kpi"><div class="k">CAIDI (dk/kesinti)</div><div class="v">{{ rel.data.value.caidi_minutes }}</div></div>
      <div class="kpi"><div class="k">Hizmet verilen abone</div><div class="v">{{ n(rel.data.value.customers_served) }}</div></div>
      <div class="kpi"><div class="k">Abone kesintisi</div><div class="v">{{ n(rel.data.value.customer_interruptions) }}</div></div>
      <div class="kpi"><div class="k">Abone-dakika</div><div class="v">{{ n(rel.data.value.customer_minutes_interrupted) }}</div></div>
    </div>
  </StateBox>
</template>
