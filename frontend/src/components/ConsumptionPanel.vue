<script setup lang="ts">
// Günlük tüketim (gündüz/puant/gece) ve seçilen günün 15 dk profili.
// İnterpolasyonla doldurulan slotlar farklı renkte gösterilir.
import { computed, ref, watch } from 'vue'
import type { ChartConfiguration } from 'chart.js'
import { api } from '../api/client'
import { useAsync } from '../lib/useAsync'
import { daysAgoISO, kwh, label, todayISO } from '../lib/format'
import StateBox from './StateBox.vue'
import Chart from './Chart.vue'

const props = defineProps<{ subscriberId: number }>()
type Day = { day: string; active_wh: number; day_wh: number; peak_wh: number; night_wh: number; interpolated_slots: number; missing_slots: number; max_demand_w: number }
type Slot = { slot_start: string; active_wh: number; demand_w: number; quality: string }

const from = ref(daysAgoISO(30))
const to = ref(todayISO())
const daily = useAsync(() => api<{ items: Day[] }>(`/subscribers/${props.subscriberId}/consumption?granularity=day&from=${from.value}&to=${to.value}`))
const selected = ref(daysAgoISO(1))
const slots = useAsync(() => {
  const next = new Date(selected.value + 'T00:00:00Z')
  next.setUTCDate(next.getUTCDate() + 1)
  return api<{ items: Slot[]; total_wh: number; quality_counts: Record<string, number> }>(
    `/subscribers/${props.subscriberId}/consumption?granularity=15m&from=${selected.value}&to=${next.toISOString().slice(0, 10)}`)
})
watch([from, to], () => daily.run())
watch(selected, () => slots.run())

const total = computed(() => (daily.data.value?.items ?? []).reduce((s, d) => s + d.active_wh, 0))
const dailyChart = computed<ChartConfiguration>(() => {
  const items = daily.data.value?.items ?? []
  return {
    type: 'bar',
    data: {
      labels: items.map((d) => d.day.slice(5)),
      datasets: [
        { label: 'Gündüz', data: items.map((d) => d.day_wh / 1000), backgroundColor: '#5b9bd5', stack: 's' },
        { label: 'Puant', data: items.map((d) => d.peak_wh / 1000), backgroundColor: '#ed7d31', stack: 's' },
        { label: 'Gece', data: items.map((d) => d.night_wh / 1000), backgroundColor: '#44546a', stack: 's' },
      ],
    },
    options: {
      scales: { y: { stacked: true, title: { display: true, text: 'kWh' } }, x: { stacked: true } },
      onClick: (_e, els) => { if (els[0]) selected.value = items[els[0].index].day },
    },
  }
})
const qColor: Record<string, string> = { measured: '#5b9bd5', interpolated: '#f2b134', corrected: '#a05bd5', reset: '#d9534f', meter_change: '#2ca58d' }
const slotChart = computed<ChartConfiguration>(() => {
  const items = slots.data.value?.items ?? []
  return {
    type: 'bar',
    data: {
      labels: items.map((s) => new Date(s.slot_start).toLocaleTimeString('tr-TR', { hour: '2-digit', minute: '2-digit', timeZone: 'Europe/Istanbul' })),
      datasets: [{ label: 'Wh / 15 dk', data: items.map((s) => s.active_wh), backgroundColor: items.map((s) => qColor[s.quality] ?? '#999') }],
    },
    options: { plugins: { legend: { display: false }, tooltip: { callbacks: { afterLabel: (c) => label(items[c.dataIndex]?.quality) } } } },
  }
})
</script>

<template>
  <section class="card">
    <div class="row between">
      <h2>Günlük tüketim</h2>
      <div class="row">
        <input v-model="from" type="date" aria-label="başlangıç" /> – <input v-model="to" type="date" aria-label="bitiş" />
      </div>
    </div>
    <StateBox :loading="daily.loading.value" :error="daily.error.value" :empty="!daily.data.value?.items.length" empty-text="Bu aralıkta tüketim verisi yok." @retry="daily.run">
      <p class="muted">Toplam {{ kwh(total) }} · bir çubuğa tıklayarak o günün 15 dakikalık profilini görün.</p>
      <Chart :config="dailyChart" />
    </StateBox>
  </section>
  <section class="card">
    <div class="row between">
      <h2>15 dakikalık profil</h2>
      <input v-model="selected" type="date" aria-label="gün" />
    </div>
    <StateBox :loading="slots.loading.value" :error="slots.error.value" :empty="!slots.data.value?.items.length" empty-text="Bu gün için 15 dk verisi yok." @retry="slots.run">
      <p class="row muted">
        <span>Toplam {{ kwh(slots.data.value?.total_wh ?? 0) }}</span>
        <span v-for="(c, q) in slots.data.value?.quality_counts" :key="q"><span class="pill" :style="{ background: qColor[q], color: '#fff' }">{{ label(String(q)) }}</span> {{ c }}</span>
      </p>
      <Chart :config="slotChart" :height="220" />
    </StateBox>
  </section>
</template>
