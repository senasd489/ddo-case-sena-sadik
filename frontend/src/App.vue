<script setup lang="ts">
import { computed, ref } from 'vue'
import { useRouter } from 'vue-router'
import { useAuth, roleLabel, type Role } from './stores/auth'
import LiveBar from './components/LiveBar.vue'

const auth = useAuth()
const router = useRouter()
const open = ref(false)

type Item = { to: string; label: string; roles: Role[] }
const items: Item[] = [
  { to: '/portal', label: 'Aboneliğim', roles: ['subscriber'] },
  { to: '/crew', label: 'Görevim', roles: ['field_crew'] },
  { to: '/outages', label: 'Kesintiler', roles: ['operator', 'admin', 'field_crew', 'analyst'] },
  { to: '/network', label: 'Şebeke', roles: ['operator', 'admin', 'field_crew', 'analyst'] },
  { to: '/crews', label: 'Ekipler', roles: ['operator', 'admin'] },
  { to: '/subscribers', label: 'Aboneler', roles: ['billing', 'admin', 'operator', 'analyst', 'field_crew'] },
  { to: '/invoices', label: 'Faturalar', roles: ['billing', 'admin', 'analyst'] },
  { to: '/loss', label: 'Kayıp-kaçak', roles: ['analyst', 'admin', 'operator'] },
  { to: '/reliability', label: 'SAIDI/SAIFI', roles: ['analyst', 'admin', 'operator'] },
  { to: '/admin/users', label: 'Kullanıcılar', roles: ['admin'] },
  { to: '/admin/audit', label: 'Denetim', roles: ['admin'] },
]
const nav = computed(() => items.filter((i) => auth.role && i.roles.includes(auth.role)))

async function logout() {
  await auth.logout()
  router.push('/login')
}
</script>

<template>
  <header v-if="auth.user" class="topbar">
    <button class="menu-btn" aria-label="menü" @click="open = !open">☰</button>
    <RouterLink to="/" class="brand">⚡ EnerjiHat</RouterLink>
    <LiveBar />
    <span class="spacer"></span>
    <span class="who hide-sm">{{ auth.user.email }} · {{ roleLabel[auth.user.role] }}</span>
    <button class="ghost" @click="logout">Çıkış</button>
  </header>
  <div :class="{ layout: !!auth.user }">
    <nav v-if="auth.user" class="sidenav" :class="{ open }" @click="open = false">
      <RouterLink v-for="i in nav" :key="i.to" :to="i.to">{{ i.label }}</RouterLink>
    </nav>
    <main class="container">
      <RouterView />
    </main>
  </div>
</template>
