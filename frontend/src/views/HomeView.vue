<script setup lang="ts">
import { onMounted, ref } from 'vue'

type Health = { status: string; checks: { database: { status: string } } }
const health = ref<Health | null>(null)
const error = ref('')
const loading = ref(true)

onMounted(async () => {
  try {
    const res = await fetch('/health')
    health.value = await res.json()
  } catch (e) {
    error.value = 'API erişilemiyor'
  } finally {
    loading.value = false
  }
})
</script>

<template>
  <section class="card">
    <h1>Sistem durumu</h1>
    <p v-if="loading">Yükleniyor…</p>
    <p v-else-if="error" class="err">{{ error }}</p>
    <p v-else-if="health">
      API: <b>{{ health.status }}</b> · Veritabanı: <b>{{ health.checks.database.status }}</b>
    </p>
  </section>
</template>
