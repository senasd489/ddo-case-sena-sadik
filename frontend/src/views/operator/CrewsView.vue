<script setup lang="ts">
import { onBeforeUnmount, onMounted } from 'vue'
import { api } from '../../api/client'
import { useAsync } from '../../lib/useAsync'
import { ago } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import Pill from '../../components/Pill.vue'

type Crew = { id: number; code: string; name: string; skills: string[]; status: string; speed_factor: number; updated_at: string; active_outage_id?: number; lat: number; lon: number }
const crews = useAsync(() => api<{ items: Crew[] }>('/crews'))
let t: number | undefined
onMounted(() => (t = window.setInterval(() => crews.run(), 10_000)))
onBeforeUnmount(() => clearInterval(t))
</script>

<template>
  <h1>Ekipler</h1>
  <section class="card">
    <StateBox :loading="crews.loading.value && !crews.data.value" :error="crews.error.value" :empty="!crews.data.value?.items.length" @retry="crews.run">
      <div class="table-wrap"><table>
        <thead><tr><th>Kod</th><th>Ad</th><th>Yetkinlik</th><th>Durum</th><th>Görev</th><th class="num">Hız katsayısı</th><th class="hide-sm">Güncelleme</th></tr></thead>
        <tbody><tr v-for="c in crews.data.value?.items" :key="c.id">
          <td class="mono">{{ c.code }}</td><td>{{ c.name }}</td><td>{{ c.skills.join(', ') }}</td><td><Pill :value="c.status" /></td>
          <td><RouterLink v-if="c.active_outage_id" :to="`/outages/${c.active_outage_id}`">#{{ c.active_outage_id }}</RouterLink></td>
          <td class="num">{{ c.speed_factor }}</td><td class="hide-sm">{{ ago(c.updated_at) }}</td>
        </tr></tbody>
      </table></div>
    </StateBox>
  </section>
</template>
