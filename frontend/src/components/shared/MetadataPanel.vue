<script setup lang="ts">
import { Card, CardContent, CardHeader, CardTitle } from '@/components/ui/card'
import { Separator } from '@/components/ui/separator'
import { formatDateTime } from '@/lib/utils'
import { Clock, UserCircle } from 'lucide-vue-next'

defineProps<{
  createdAt?: string
  updatedAt?: string
  createdByName?: string
  updatedByName?: string
}>()
</script>

<template>
  <Card>
    <CardHeader class="pb-3">
      <CardTitle class="text-sm font-medium">{{ $t('common.metadata', 'Metadata') }}</CardTitle>
    </CardHeader>
    <CardContent class="space-y-3 text-sm">
      <div v-if="createdByName" class="flex items-center gap-2">
        <UserCircle class="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <span class="text-muted-foreground">{{ $t('common.createdBy', 'Created by') }}</span>
        <span class="ml-auto font-medium">{{ createdByName }}</span>
      </div>
      <div v-if="createdAt" class="flex items-center gap-2">
        <Clock class="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <span class="text-muted-foreground">{{ $t('common.createdAt', 'Created') }}</span>
        <span class="ml-auto">{{ formatDateTime(createdAt) }}</span>
      </div>

      <Separator v-if="(createdByName || createdAt) && (updatedByName || updatedAt)" />

      <div v-if="updatedByName" class="flex items-center gap-2">
        <UserCircle class="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <span class="text-muted-foreground">{{ $t('common.updatedBy', 'Modified by') }}</span>
        <span class="ml-auto font-medium">{{ updatedByName }}</span>
      </div>
      <div v-if="updatedAt" class="flex items-center gap-2">
        <Clock class="h-3.5 w-3.5 text-muted-foreground shrink-0" />
        <span class="text-muted-foreground">{{ $t('common.lastUpdated', 'Last updated') }}</span>
        <span class="ml-auto">{{ formatDateTime(updatedAt) }}</span>
      </div>
    </CardContent>
  </Card>
</template>
