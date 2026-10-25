<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../../api/client'
import { useAsync } from '../../lib/useAsync'
import { dateTime } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'

type A = { id: number; at: string; actor_id?: number; actor_role?: string; action: string; entity: string; entity_id?: string; old_value: unknown; new_value: unknown }
const entity = ref('')
const items = ref<A[]>([])
const next = ref<string | null>(null)
const list = useAsync(async () => {
  const r = await api<{ items: A[]; next_cursor: string | null }>(`/admin/audit?limit=50&entity=${entity.value}`)
  items.value = r.items
  next.value = r.next_cursor
  return r
})
async function more() {
  const r = await api<{ items: A[]; next_cursor: string | null }>(`/admin/audit?limit=50&entity=${entity.value}&cursor=${next.value}`)
  items.value.push(...r.items)
  next.value = r.next_cursor
}
const short = (v: unknown) => (v ? JSON.stringify(v).slice(0, 160) : '')
</script>

<template>
  <h1>Denetim kaydı</h1>
  <section class="card">
    <p class="muted">Eklemeli-yalnız: veritabanı tetikleyicisi UPDATE/DELETE/TRUNCATE'i reddeder.</p>
    <form class="row" @submit.prevent="list.run">
      <select v-model="entity" aria-label="varlık">
        <option value="">Tümü</option><option value="invoice">Fatura</option><option value="outage">Kesinti</option><option value="network_node">Şebeke</option>
        <option value="meter">Sayaç</option><option value="subscriber">Abone</option><option value="user">Kullanıcı</option><option value="suspicion_flag">Şüpheli</option>
      </select>
      <button>Filtrele</button>
    </form>
    <StateBox :loading="list.loading.value" :error="list.error.value" :empty="!items.length" @retry="list.run">
      <div class="table-wrap"><table>
        <thead><tr><th>Zaman</th><th>Kim</th><th>İşlem</th><th>Varlık</th><th>Eski</th><th>Yeni</th></tr></thead>
        <tbody><tr v-for="a in items" :key="a.id">
          <td>{{ dateTime(a.at) }}</td><td>{{ a.actor_id ? `#${a.actor_id} ${a.actor_role}` : 'sistem' }}</td><td class="mono">{{ a.action }}</td>
          <td>{{ a.entity }} {{ a.entity_id }}</td><td class="mono muted">{{ short(a.old_value) }}</td><td class="mono">{{ short(a.new_value) }}</td>
        </tr></tbody>
      </table></div>
      <button v-if="next" class="ghost" @click="more">Daha fazla</button>
    </StateBox>
  </section>
</template>
