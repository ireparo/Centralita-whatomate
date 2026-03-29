<script setup lang="ts">
import { computed } from 'vue'
import { MousePointerClick } from 'lucide-vue-next'
import BaseNode from '@/components/shared/nodes/BaseNode.vue'

defineOptions({ inheritAttrs: false })

const props = defineProps<{ data: any }>()

const buttons = computed(() => props.data?.config?.buttons || [])

const outputHandles = computed(() => {
  return buttons.value.map((b: any) => ({
    id: b.id,
    label: b.title?.length > 12 ? b.title.slice(0, 12) + '...' : b.title || '—',
  }))
})
</script>

<template>
  <BaseNode :label="data?.label || 'Buttons'" header-class="bg-purple-600" :output-handles="outputHandles" :has-input="!data?.isEntryNode">
    <template #icon><MousePointerClick class="w-3 h-3" /></template>
    <div v-if="buttons.length > 0" class="space-y-0.5">
      <div v-for="btn in buttons" :key="btn.id" class="truncate">{{ btn.title || '—' }}</div>
    </div>
    <p v-else class="text-muted-foreground">No buttons</p>
  </BaseNode>
</template>
