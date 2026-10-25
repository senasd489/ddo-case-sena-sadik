<script setup lang="ts">
// Yükleniyor / hata / boş durumlarını her ekranda aynı biçimde gösterir.
defineProps<{ loading?: boolean; error?: string; empty?: boolean; emptyText?: string }>()
defineEmits<{ retry: [] }>()
</script>

<template>
  <div v-if="loading" class="state" role="status"><span class="spinner" aria-hidden="true"></span> Yükleniyor…</div>
  <div v-else-if="error" class="state err" role="alert">
    {{ error }} <button class="link" @click="$emit('retry')">Tekrar dene</button>
  </div>
  <div v-else-if="empty" class="state muted">{{ emptyText ?? 'Kayıt yok.' }}</div>
  <slot v-else />
</template>
