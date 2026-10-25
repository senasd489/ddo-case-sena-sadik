<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../../api/client'
import { roleLabel, type Role } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { dateTime } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'

type U = { id: number; email: string; full_name: string; role: Role; subscriber_id?: number; crew_id?: number; disabled: boolean; created_at: string }
const items = ref<U[]>([])
const next = ref<string | null>(null)
const list = useAsync(async () => {
  const r = await api<{ items: U[]; next_cursor: string | null }>('/admin/users?limit=50')
  items.value = r.items
  next.value = r.next_cursor
  return r
})
async function more() {
  const r = await api<{ items: U[]; next_cursor: string | null }>(`/admin/users?limit=50&cursor=${next.value}`)
  items.value.push(...r.items)
  next.value = r.next_cursor
}
const form = ref({ email: '', full_name: '', password: '', role: 'analyst' as Role, subscriber_id: null as number | null, crew_id: null as number | null })
const msg = ref('')
async function create() {
  msg.value = ''
  try {
    const body: Record<string, unknown> = { ...form.value }
    if (form.value.role !== 'subscriber') delete body.subscriber_id
    if (form.value.role !== 'field_crew') delete body.crew_id
    await api('/admin/users', { body, idempotent: true })
    msg.value = 'Kullanıcı oluşturuldu.'
    list.run()
  } catch (e) {
    msg.value = errText(e)
  }
}
</script>

<template>
  <h1>Kullanıcılar</h1>
  <section class="card">
    <h2>Yeni kullanıcı</h2>
    <form @submit.prevent="create">
      <div class="grid">
        <label>E-posta <input v-model="form.email" type="email" required /></label>
        <label>Ad soyad <input v-model="form.full_name" required /></label>
        <label>Parola (≥ 8) <input v-model="form.password" type="password" minlength="8" required /></label>
        <label>Rol <select v-model="form.role"><option v-for="(l, r) in roleLabel" :key="r" :value="r">{{ l }}</option></select></label>
        <label v-if="form.role === 'subscriber'">Abone id <input v-model.number="form.subscriber_id" type="number" min="1" required /></label>
        <label v-if="form.role === 'field_crew'">Ekip id <input v-model.number="form.crew_id" type="number" min="1" /></label>
      </div>
      <button>Oluştur</button> <span>{{ msg }}</span>
    </form>
  </section>
  <section class="card">
    <StateBox :loading="list.loading.value" :error="list.error.value" :empty="!items.length" @retry="list.run">
      <div class="table-wrap"><table>
        <thead><tr><th>#</th><th>E-posta</th><th>Ad</th><th>Rol</th><th>Bağlı</th><th class="hide-sm">Oluşturma</th></tr></thead>
        <tbody><tr v-for="u in items" :key="u.id">
          <td>{{ u.id }}</td><td>{{ u.email }}</td><td>{{ u.full_name }}</td><td>{{ roleLabel[u.role] }}</td>
          <td>{{ u.subscriber_id ? `abone #${u.subscriber_id}` : u.crew_id ? `ekip #${u.crew_id}` : '' }}</td><td class="hide-sm">{{ dateTime(u.created_at) }}</td>
        </tr></tbody>
      </table></div>
      <button v-if="next" class="ghost" @click="more">Daha fazla</button>
    </StateBox>
  </section>
</template>
