<script setup lang="ts">
import { computed } from 'vue'
import { label } from '../lib/format'
const props = defineProps<{ value?: string | null }>()
const tone = computed(() => {
  const v = props.value ?? ''
  if (['resolved', 'paid', 'available', 'measured', 'actual', 'confirmed_ok'].includes(v)) return 'ok'
  if (['detected', 'busy', 'interpolated', 'estimated', 'open', 'assigned', 'scheduled', 'medium'].includes(v)) return 'warn'
  if (['confirmed', 'in_progress', 'reset', 'high', 'sudden_drop', 'night_usage'].includes(v)) return 'bad'
  return 'neutral'
})
</script>
<template><span class="pill" :class="tone">{{ label(value) }}</span></template>
