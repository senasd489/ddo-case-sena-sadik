<script setup lang="ts">
// Sistemin canlı olduğunu gösterir: son okuma partisi, son 1 saatte gelen
// okuma ve bildirimler, açık kesintiler (10 sn'de bir yenilenir).
import { onBeforeUnmount, onMounted, ref } from 'vue'
import { api } from '../api/client'
import { ago, n } from '../lib/format'

type Live = {
  last_batch_at?: string; last_read_at?: string; readings_last_hour: number; signals_last_hour: number
  open_outages: number; interrupted_subscribers: number; busy_crews: number; open_suspects: number; consumption_queue: number
}
const live = ref<Live | null>(null)
const failed = ref(false)
let timer: number | undefined

async function load() {
  try {
    live.value = await api<Live>('/live')
    failed.value = false
  } catch {
    failed.value = true
  }
}
onMounted(() => {
  load()
  timer = window.setInterval(load, 10_000)
})
onBeforeUnmount(() => clearInterval(timer))

const fresh = () => !!live.value?.last_batch_at && Date.now() - new Date(live.value.last_batch_at).getTime() < 20 * 60_000
</script>

<template>
  <div class="livebar" aria-live="polite">
    <span v-if="failed" class="dot off" title="bağlantı yok"></span>
    <span v-else class="dot" :class="{ on: fresh() }" :title="fresh() ? 'veri akıyor' : 'veri akışı durgun'"></span>
    <template v-if="live">
      <span>Son parti: <b>{{ ago(live.last_batch_at) }}</b></span>
      <span class="hide-sm">1 sa okuma: <b>{{ n(live.readings_last_hour) }}</b></span>
      <span class="hide-sm">1 sa bildirim: <b>{{ n(live.signals_last_hour) }}</b></span>
      <span>Açık kesinti: <b>{{ live.open_outages }}</b> <span class="hide-sm">({{ n(live.interrupted_subscribers) }} abone)</span></span>
      <span class="hide-sm">Görevdeki ekip: <b>{{ live.busy_crews }}</b></span>
    </template>
    <span v-else-if="failed">API erişilemiyor</span>
  </div>
</template>
