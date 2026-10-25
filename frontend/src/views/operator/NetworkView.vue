<script setup lang="ts">
// Şebeke topolojisi: ağaç (trafo → besleyici → direk) + koordinatlara göre
// basit harita. Seçilen düğümün etki analizi ve yedek beslemeye anahtarlama.
import { computed, provide, ref, watch } from 'vue'
import { useRoute } from 'vue-router'
import { api } from '../../api/client'
import { useAuth } from '../../stores/auth'
import { errText, useAsync } from '../../lib/useAsync'
import { label } from '../../lib/format'
import StateBox from '../../components/StateBox.vue'
import TreeNode, { treeKey } from '../../components/TreeNode.vue'

type Node = { id: number; parent_id?: number; alt_parent_id?: number; normal_parent_id?: number; kind: string; code: string; name: string; lat?: number; lon?: number; depth: number; subscribers: number; switched: boolean }
const route = useRoute()
const auth = useAuth()
const tree = useAsync(() => api<{ items: Node[] }>('/network/tree'))
const outages = useAsync(() => api<{ items: { id: number; fault_node_id: number; status: string }[] }>('/outages?open=true&limit=200'))
const nodes = computed(() => tree.data.value?.items ?? [])
const byId = computed(() => new Map(nodes.value.map((n) => [n.id, n])))
const children = computed(() => {
  const m = new Map<number, Node[]>()
  for (const n of nodes.value) if (n.parent_id) m.set(n.parent_id, [...(m.get(n.parent_id) ?? []), n])
  return m
})
const roots = computed(() => nodes.value.filter((n) => !n.parent_id))
const faulted = computed(() => new Map((outages.data.value?.items ?? []).map((o) => [o.fault_node_id, o.id])))
// Arızalı bir düğümün altındaki her düğüm "enerjisiz" görünür.
const dark = computed(() => {
  const s = new Set<number>()
  const walk = (id: number) => { s.add(id); for (const c of children.value.get(id) ?? []) walk(c.id) }
  for (const id of faulted.value.keys()) walk(id)
  return s
})
const expanded = ref(new Set<number>())
const selected = ref<number | null>(route.query.node ? Number(route.query.node) : null)
const filter = ref('')

function toggle(id: number) {
  const s = new Set(expanded.value)
  s.has(id) ? s.delete(id) : s.add(id)
  expanded.value = s
}
function reveal(id: number) {
  const s = new Set(expanded.value)
  let n = byId.value.get(id)
  while (n?.parent_id) { s.add(n.parent_id); n = byId.value.get(n.parent_id) }
  expanded.value = s
  selected.value = id
}
watch(nodes, () => { if (selected.value) reveal(selected.value) })
provide(treeKey, { children, expanded, toggle, selected, select: (id: number) => (selected.value = id), faulted, dark })
const visibleRoots = computed(() => (filter.value ? roots.value.filter((r) => r.code.includes(filter.value.toUpperCase())) : roots.value))

const impact = useAsync(() => (selected.value ? api<{ subscribers: number; by_category: Record<string, number>; lineage: { path: number[] } }>(`/network/nodes/${selected.value}/impact`) : Promise.resolve(null)), false)
watch(selected, () => impact.run(), { immediate: true })
const sel = computed(() => (selected.value ? byId.value.get(selected.value) : undefined))
const msg = ref('')
async function switchTo(parent: number) {
  msg.value = ''
  try {
    const r = await api<{ moved_subscribers: number; restored_subscribers: number }>('/network/switch', { body: { node_id: selected.value, new_parent_id: parent } })
    msg.value = `Anahtarlama tamam: ${r.moved_subscribers} abone taşındı, ${r.restored_subscribers} abonenin enerjisi geri geldi.`
    await Promise.all([tree.run(), outages.run()])
  } catch (e) {
    msg.value = errText(e)
  }
}
async function restoreNormal() {
  msg.value = ''
  try {
    await api(`/network/nodes/${selected.value}/restore-normal`, { method: 'POST' })
    msg.value = 'Normal beslemeye dönüldü.'
    await tree.run()
  } catch (e) {
    msg.value = errText(e)
  }
}

// Harita: koordinatları SVG'ye ölçekle.
const box = computed(() => {
  const pts = nodes.value.filter((n) => n.lat && n.lon)
  const lats = pts.map((n) => n.lat!), lons = pts.map((n) => n.lon!)
  return { minLat: Math.min(...lats), maxLat: Math.max(...lats), minLon: Math.min(...lons), maxLon: Math.max(...lons) }
})
const W = 600, H = 420
const xy = (n: Node) => ({
  x: 10 + ((n.lon! - box.value.minLon) / (box.value.maxLon - box.value.minLon || 1)) * (W - 20),
  y: 10 + (1 - (n.lat! - box.value.minLat) / (box.value.maxLat - box.value.minLat || 1)) * (H - 20),
})
const edges = computed(() => nodes.value.filter((n) => n.parent_id && n.lat && byId.value.get(n.parent_id)?.lat).map((n) => ({ a: xy(byId.value.get(n.parent_id!)!), b: xy(n), dark: dark.value.has(n.id), id: n.id })))
const r = (k: string) => (k === 'transformer' ? 6 : k === 'feeder' ? 4 : 2.2)
</script>

<template>
  <h1>Şebeke</h1>
  <StateBox :loading="tree.loading.value && !nodes.length" :error="tree.error.value" :empty="!nodes.length" @retry="tree.run">
    <div class="grid" style="grid-template-columns: repeat(auto-fit, minmax(320px, 1fr))">
      <section class="card">
        <div class="row between"><h2>Topoloji</h2><input v-model="filter" placeholder="TR-0001" aria-label="trafo filtrele" style="width: 9rem" /></div>
        <p class="muted">🔴 arıza noktası · ⚫ enerjisiz · ↪ yedek beslemede</p>
        <div class="tree" style="max-height: 560px; overflow: auto">
          <ul>
            <template v-for="t in visibleRoots" :key="t.id">
              <li>
                <TreeNode :n="t" />
              </li>
            </template>
          </ul>
        </div>
      </section>
      <div>
        <section class="card">
          <h2>Harita</h2>
          <svg :viewBox="`0 0 ${W} ${H}`" style="width: 100%; height: auto; background: var(--bg); border-radius: 8px" role="img" aria-label="şebeke haritası">
            <line v-for="e in edges" :key="'e' + e.id" :x1="e.a.x" :y1="e.a.y" :x2="e.b.x" :y2="e.b.y" :stroke="e.dark ? '#d9534f' : '#8aa4c0'" stroke-width="0.8" />
            <circle v-for="n in nodes.filter((x) => x.lat)" :key="n.id" :cx="xy(n).x" :cy="xy(n).y" :r="r(n.kind) + (selected === n.id ? 3 : 0)"
              :fill="faulted.has(n.id) ? '#d9534f' : dark.has(n.id) ? '#333' : n.kind === 'transformer' ? '#0b63c4' : n.kind === 'feeder' ? '#2ca58d' : '#7f8c99'"
              style="cursor: pointer" @click="reveal(n.id)"><title>{{ n.code }} · {{ n.subscribers }} abone</title></circle>
          </svg>
        </section>
        <section v-if="sel" class="card">
          <h2>{{ label(sel.kind) }} <span class="mono">{{ sel.code }}</span></h2>
          <p>Alt ağaçta <b>{{ sel.subscribers }}</b> abone. <span v-if="sel.switched" class="pill warn">yedek beslemede</span>
            <RouterLink v-if="faulted.get(sel.id)" :to="`/outages/${faulted.get(sel.id)}`" class="pill bad">kesinti #{{ faulted.get(sel.id) }}</RouterLink></p>
          <StateBox :loading="impact.loading.value" :error="impact.error.value">
            <p v-if="impact.data.value" class="muted">Bu noktada arıza olursa etkilenecek: {{ impact.data.value.subscribers }} abone —
              <span v-for="(c, k) in impact.data.value.by_category" :key="k">{{ label(String(k)) }} {{ c }} </span></p>
          </StateBox>
          <div v-if="auth.is('operator', 'admin') && sel.kind !== 'transformer'" class="row">
            <button v-if="sel.alt_parent_id && sel.parent_id !== sel.alt_parent_id" @click="switchTo(sel.alt_parent_id!)">Yedek beslemeye aktar ({{ byId.get(sel.alt_parent_id!)?.code }})</button>
            <button v-if="sel.switched" class="ghost" @click="restoreNormal">Normal beslemeye dön</button>
          </div>
          <p>{{ msg }}</p>
        </section>
      </div>
    </div>
  </StateBox>
</template>

