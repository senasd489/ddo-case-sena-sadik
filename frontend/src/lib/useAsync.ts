import { ref, type Ref } from 'vue'
import { ApiError } from '../api/client'

// Her ekranın yükleniyor / hata / veri durumlarını tek tip yönetir.
export function useAsync<T>(fn: () => Promise<T>, immediate = true) {
  const data = ref<T | null>(null) as Ref<T | null>
  const loading = ref(false)
  const error = ref('')
  async function run() {
    loading.value = true
    error.value = ''
    try {
      data.value = await fn()
    } catch (e) {
      error.value = e instanceof ApiError ? e.message : String(e)
    } finally {
      loading.value = false
    }
  }
  if (immediate) run()
  return { data, loading, error, run }
}

export function errText(e: unknown): string {
  if (e instanceof ApiError) {
    const d = e.details ? ' — ' + Object.entries(e.details).map(([k, v]) => `${k}: ${typeof v === 'string' ? v : JSON.stringify(v)}`).join(', ') : ''
    return e.message + d
  }
  return String(e)
}
