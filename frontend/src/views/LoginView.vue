<script setup lang="ts">
import { ref } from 'vue'
import { useRoute, useRouter } from 'vue-router'
import { useAuth } from '../stores/auth'
import { errText } from '../lib/useAsync'

const auth = useAuth()
const router = useRouter()
const route = useRoute()
const email = ref('')
const password = ref('')
const busy = ref(false)
const error = ref('')
const demo = [
  ['abone@enerjihat.local', 'Abone'], ['saha@enerjihat.local', 'Saha ekibi'], ['fatura@enerjihat.local', 'Faturalama'],
  ['operator@enerjihat.local', 'Operatör'], ['analist@enerjihat.local', 'Analist'], ['admin@enerjihat.local', 'Yönetici'],
]

async function submit() {
  busy.value = true
  error.value = ''
  try {
    await auth.login(email.value, password.value)
    router.push((route.query.next as string) || '/')
  } catch (e) {
    error.value = errText(e)
  } finally {
    busy.value = false
  }
}
function fill(e: string) {
  email.value = e
  password.value = 'Demo1234!'
}
</script>

<template>
  <div class="login">
    <section class="card">
      <h1>⚡ EnerjiHat</h1>
      <p class="muted">Akıllı sayaç, faturalama ve kesinti yönetimi</p>
      <form @submit.prevent="submit">
        <label>E-posta <input v-model="email" type="email" autocomplete="username" required /></label>
        <label>Parola <input v-model="password" type="password" autocomplete="current-password" required /></label>
        <p v-if="error" class="err" role="alert">{{ error }}</p>
        <button :disabled="busy">{{ busy ? 'Giriş yapılıyor…' : 'Giriş yap' }}</button>
      </form>
      <details>
        <summary>Demo hesapları (parola: Demo1234!)</summary>
        <div class="chips">
          <button v-for="[e, l] in demo" :key="e" type="button" class="ghost" @click="fill(e)">{{ l }}</button>
        </div>
      </details>
    </section>
  </div>
</template>
