<script setup lang="ts">
import { onBeforeUnmount, onMounted, ref, watch } from 'vue'
import {
  Chart, BarController, BarElement, LineController, LineElement, PointElement, CategoryScale, LinearScale, Legend, Tooltip, Filler,
  type ChartConfiguration,
} from 'chart.js'

Chart.register(BarController, BarElement, LineController, LineElement, PointElement, CategoryScale, LinearScale, Legend, Tooltip, Filler)

const props = defineProps<{ config: ChartConfiguration; height?: number }>()
const canvas = ref<HTMLCanvasElement>()
let chart: Chart | null = null

function draw() {
  chart?.destroy()
  if (canvas.value) chart = new Chart(canvas.value, { ...props.config, options: { responsive: true, maintainAspectRatio: false, animation: false, ...props.config.options } })
}
onMounted(draw)
watch(() => props.config, draw, { deep: true })
onBeforeUnmount(() => chart?.destroy())
</script>

<template>
  <div class="chart" :style="{ height: (height ?? 260) + 'px' }"><canvas ref="canvas" role="img" aria-label="grafik"></canvas></div>
</template>
