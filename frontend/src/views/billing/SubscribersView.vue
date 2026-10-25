<script setup lang="ts">
import { ref } from 'vue'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { label } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'

type Sub = { id: number; subscriber_no: string; full_name: string; address: string; category: string; tariff_code: string; pole_id: number }
const auth = useAuth()
const q = ref('')
const category = ref('')
const items = ref<Sub[]>([])
const next = ref<string | null>(null)
const page = useAsync(async () => {
  const r = await api<{ items: Sub[]; next_cursor: string | null }>(`/subscribers?limit=25&q=${encodeURIComponent(q.value)}&category=${category.value}`)
  items.value = r.items
  next.value = r.next_cursor
  return r
})
const more = ref(false)
async function loadMore() {
  more.value = true
  try {
    const r = await api<{ items: Sub[]; next_cursor: string | null }>(`/subscribers?limit=25&q=${encodeURIComponent(q.value)}&category=${category.value}&cursor=${next.value}`)
    items.value.push(...r.items)
    next.value = r.next_cursor
  } finally {
    more.value = false
  }
}

const showNew = ref(false)
const form = ref({ full_name: '', address: '', category: 'residential', pole_id: 3, tariff_id: 1, contract_kw: 7.5, meter_serial_no: '' })
const formMsg = ref('')
async function create() {
  formMsg.value = ''
  try {
    const r = await api<{ subscriber: Sub }>('/subscribers', { body: form.value, idempotent: true })
    formMsg.value = `Kaydedildi: ${r.subscriber.subscriber_no}`
    page.run()
  } catch (e) {
    formMsg.value = errText(e)
  }
}
</script>

<template>
  <div class="row between">
    <h1>Aboneler</h1>
    <button v-if="auth.is('billing', 'admin')" class="ghost" @click="showNew = !showNew">{{ showNew ? 'Kapat' : 'Yeni abone' }}</button>
  </div>
  <section v-if="showNew" class="card">
    <h2>Abone + sayaç kaydı</h2>
    <form @submit.prevent="create">
      <div class="grid">
        <label>Ad soyad <input v-model="form.full_name" required /></label>
        <label>Adres <input v-model="form.address" required /></label>
        <label>Kategori
          <select v-model="form.category"><option value="residential">Mesken</option><option value="commercial">Ticarethane</option><option value="industrial">Sanayi</option><option value="agricultural">Tarım</option></select>
        </label>
        <label>Tarife id (1 Mesken, 2 Ticari, 3 Sanayi, 4 Tarım) <input v-model.number="form.tariff_id" type="number" min="1" /></label>
        <label>Direk id <input v-model.number="form.pole_id" type="number" min="1" /></label>
        <label>Sözleşme gücü (kW) <input v-model.number="form.contract_kw" type="number" step="0.1" /></label>
        <label>Sayaç seri no <input v-model="form.meter_serial_no" required /></label>
      </div>
      <div class="row"><button>Kaydet</button><span>{{ formMsg }}</span></div>
    </form>
  </section>
  <section class="card">
    <form class="row" @submit.prevent="page.run">
      <input v-model="q" placeholder="Abone no veya ad" aria-label="ara" />
      <select v-model="category" aria-label="kategori"><option value="">Tüm kategoriler</option><option value="residential">Mesken</option><option value="commercial">Ticarethane</option><option value="industrial">Sanayi</option><option value="agricultural">Tarım</option></select>
      <button>Ara</button>
    </form>
    <StateBox :loading="page.loading.value" :error="page.error.value" :empty="!items.length" empty-text="Eşleşen abone yok." @retry="page.run">
      <div class="table-wrap"><table>
        <thead><tr><th>No</th><th>Ad</th><th class="hide-sm">Adres</th><th>Kategori</th><th>Tarife</th></tr></thead>
        <tbody>
          <tr v-for="s in items" :key="s.id" class="click" @click="$router.push(`/subscribers/${s.id}`)">
            <td class="mono">{{ s.subscriber_no }}</td><td>{{ s.full_name }}</td><td class="hide-sm">{{ s.address }}</td><td>{{ label(s.category) }}</td><td>{{ s.tariff_code }}</td>
          </tr>
        </tbody>
      </table></div>
      <button v-if="next" class="ghost" :disabled="more" @click="loadMore">{{ more ? 'Yükleniyor…' : 'Daha fazla' }}</button>
    </StateBox>
  </section>
</template>
