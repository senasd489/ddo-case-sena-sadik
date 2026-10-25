<script lang="ts">
import type { ComputedRef, InjectionKey, Ref } from 'vue'
export type TreeNodeData = { id: number; kind: string; code: string; subscribers: number; switched: boolean }
export type TreeCtx = {
  children: ComputedRef<Map<number, TreeNodeData[]>>
  expanded: Ref<Set<number>>
  toggle: (id: number) => void
  selected: Ref<number | null>
  select: (id: number) => void
  faulted: ComputedRef<Map<number, number>>
  dark: ComputedRef<Set<number>>
}
export const treeKey: InjectionKey<TreeCtx> = Symbol('tree')
</script>

<script setup lang="ts">
import { inject } from 'vue'
defineOptions({ name: 'TreeNode' })
const props = defineProps<{ n: TreeNodeData }>()
const ctx = inject(treeKey)!
const kids = () => ctx.children.value.get(props.n.id) ?? []
const icon: Record<string, string> = { transformer: '▣', feeder: '┃', pole: '•' }
</script>

<template>
  <span class="node" :class="{ sel: ctx.selected.value === n.id }">
    <button v-if="kids().length" class="link" :aria-label="ctx.expanded.value.has(n.id) ? 'daralt' : 'genişlet'" @click="ctx.toggle(n.id)">
      {{ ctx.expanded.value.has(n.id) ? '▾' : '▸' }}
    </button>
    <span v-else style="width: 0.8em; display: inline-block"></span>
    <a href="#" @click.prevent="ctx.select(n.id)">
      {{ icon[n.kind] }} <span class="mono">{{ n.code }}</span>
    </a>
    <span class="muted">{{ n.subscribers }}</span>
    <span v-if="ctx.faulted.value.has(n.id)" title="arıza noktası">🔴</span>
    <span v-else-if="ctx.dark.value.has(n.id)" title="enerjisiz">⚫</span>
    <span v-if="n.switched" title="yedek beslemede">↪</span>
  </span>
  <ul v-if="ctx.expanded.value.has(n.id) && kids().length">
    <li v-for="c in kids()" :key="c.id"><TreeNode :n="c" /></li>
  </ul>
</template>
