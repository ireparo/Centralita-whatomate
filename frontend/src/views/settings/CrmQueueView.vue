<script setup lang="ts">
import { ref, computed, onMounted } from 'vue'
import { useI18n } from 'vue-i18n'
import { Card, CardContent, CardHeader, CardTitle, CardDescription } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { PageHeader, DataTable, type Column } from '@/components/shared'
import {
  Dialog, DialogContent, DialogDescription, DialogFooter, DialogHeader, DialogTitle,
} from '@/components/ui/dialog'
import {
  AlertDialog, AlertDialogAction, AlertDialogCancel, AlertDialogContent,
  AlertDialogDescription, AlertDialogFooter, AlertDialogHeader, AlertDialogTitle,
} from '@/components/ui/alert-dialog'
import { ScrollArea } from '@/components/ui/scroll-area'
import { toast } from 'vue-sonner'
import { crmQueueService, type CRMQueueEntry, type CRMQueueDetail } from '@/services/api'
import { RotateCcw, Trash2, Eye, Inbox, AlertTriangle, CheckCircle2 } from 'lucide-vue-next'
import { formatDate } from '@/lib/utils'

const { t } = useI18n()

const entries = ref<CRMQueueEntry[]>([])
const summary = ref<Record<string, number>>({ pending: 0, delivered: 0, dead_letter: 0 })
const totalItems = ref(0)
const isLoading = ref(true)
const currentTab = ref<'dead_letter' | 'pending' | 'delivered'>('dead_letter')
const currentPage = ref(1)
const pageSize = 25

const selectedDetail = ref<CRMQueueDetail | null>(null)
const detailOpen = ref(false)
const pendingDiscardId = ref<string | null>(null)

const columns = computed<Column<CRMQueueEntry>[]>(() => [
  { key: 'created_at', label: t('crmQueue.createdAt') },
  { key: 'event_type', label: t('crmQueue.eventType') },
  { key: 'attempt_count', label: t('crmQueue.attempts') },
  { key: 'last_error', label: t('crmQueue.lastError') },
  { key: 'actions', label: '' },
])

async function fetchEntries() {
  isLoading.value = true
  try {
    const { data } = await crmQueueService.list({
      status: currentTab.value,
      page: currentPage.value,
      limit: pageSize,
    })
    const payload = (data as any).data ?? data
    entries.value = payload.queue ?? []
    summary.value = payload.summary ?? { pending: 0, delivered: 0, dead_letter: 0 }
    totalItems.value = payload.total ?? 0
  } catch (e: any) {
    toast.error(t('crmQueue.loadError'), {
      description: e.response?.data?.message || String(e),
    })
  } finally {
    isLoading.value = false
  }
}

function onTabChange(tab: typeof currentTab.value) {
  currentTab.value = tab
  currentPage.value = 1
  fetchEntries()
}

async function viewDetail(row: CRMQueueEntry) {
  try {
    const { data } = await crmQueueService.get(row.id)
    selectedDetail.value = (data as any).data ?? data
    detailOpen.value = true
  } catch (e: any) {
    toast.error(t('crmQueue.loadError'), {
      description: e.response?.data?.message || String(e),
    })
  }
}

async function replay(row: CRMQueueEntry) {
  try {
    await crmQueueService.replay(row.id)
    toast.success(t('crmQueue.replayQueued'), {
      description: t('crmQueue.replayDescription'),
    })
    fetchEntries()
  } catch (e: any) {
    toast.error(t('crmQueue.replayFailed'), {
      description: e.response?.data?.message || String(e),
    })
  }
}

async function confirmDiscard() {
  if (!pendingDiscardId.value) return
  const id = pendingDiscardId.value
  pendingDiscardId.value = null
  try {
    await crmQueueService.discard(id)
    toast.success(t('crmQueue.discarded'))
    fetchEntries()
  } catch (e: any) {
    toast.error(t('crmQueue.discardFailed'), {
      description: e.response?.data?.message || String(e),
    })
  }
}

function statusColor(status: string) {
  switch (status) {
    case 'delivered': return 'success'
    case 'pending': return 'warning'
    case 'dead_letter': return 'destructive'
    default: return 'secondary'
  }
}

function prettyPayload(raw: string): string {
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

onMounted(fetchEntries)
</script>

<template>
  <div class="h-full flex flex-col">
    <PageHeader
      :title="t('crmQueue.title')"
      :description="t('crmQueue.description')"
    />

    <div class="flex-1 overflow-auto px-6 pb-6">
      <!-- Summary cards -->
      <div class="grid grid-cols-1 md:grid-cols-3 gap-4 mb-6">
        <Card
          class="cursor-pointer transition-colors"
          :class="currentTab === 'dead_letter' ? 'border-destructive/50 bg-destructive/5' : 'hover:bg-muted/40'"
          @click="onTabChange('dead_letter')"
        >
          <CardHeader class="pb-2">
            <CardDescription class="flex items-center gap-1.5">
              <AlertTriangle class="h-3.5 w-3.5 text-destructive" />
              {{ t('crmQueue.deadLetter') }}
            </CardDescription>
            <CardTitle class="text-3xl text-destructive">{{ summary.dead_letter }}</CardTitle>
          </CardHeader>
        </Card>

        <Card
          class="cursor-pointer transition-colors"
          :class="currentTab === 'pending' ? 'border-amber-500/50 bg-amber-50 dark:bg-amber-950/20' : 'hover:bg-muted/40'"
          @click="onTabChange('pending')"
        >
          <CardHeader class="pb-2">
            <CardDescription class="flex items-center gap-1.5">
              <Inbox class="h-3.5 w-3.5 text-amber-600" />
              {{ t('crmQueue.pending') }}
            </CardDescription>
            <CardTitle class="text-3xl text-amber-600">{{ summary.pending }}</CardTitle>
          </CardHeader>
        </Card>

        <Card
          class="cursor-pointer transition-colors"
          :class="currentTab === 'delivered' ? 'border-green-500/50 bg-green-50 dark:bg-green-950/20' : 'hover:bg-muted/40'"
          @click="onTabChange('delivered')"
        >
          <CardHeader class="pb-2">
            <CardDescription class="flex items-center gap-1.5">
              <CheckCircle2 class="h-3.5 w-3.5 text-green-600" />
              {{ t('crmQueue.delivered') }}
            </CardDescription>
            <CardTitle class="text-3xl text-green-600">{{ summary.delivered }}</CardTitle>
          </CardHeader>
        </Card>
      </div>

      <Card>
        <CardHeader>
          <CardTitle>{{ t(`crmQueue.tab.${currentTab}`) }}</CardTitle>
          <CardDescription>{{ t(`crmQueue.tabDesc.${currentTab}`) }}</CardDescription>
        </CardHeader>
        <CardContent>
          <DataTable
            :items="entries"
            :columns="columns"
            :loading="isLoading"
            :total-items="totalItems"
            :current-page="currentPage"
            :page-size="pageSize"
            @page-change="(page: number) => { currentPage = page; fetchEntries() }"
          >
            <template #cell-created_at="{ item }">
              {{ formatDate(item.created_at) }}
            </template>

            <template #cell-event_type="{ item }">
              <div class="flex items-center gap-2">
                <code class="text-xs">{{ item.event_type }}</code>
                <Badge :variant="statusColor(item.status) as any" class="text-xs">
                  {{ t(`crmQueue.statusLabel.${item.status}`) }}
                </Badge>
              </div>
            </template>

            <template #cell-attempt_count="{ item }">
              <span :class="item.attempt_count >= 10 ? 'text-destructive font-medium' : ''">
                {{ item.attempt_count }}
              </span>
            </template>

            <template #cell-last_error="{ item }">
              <span class="text-xs text-muted-foreground line-clamp-1 max-w-md" :title="item.last_error || ''">
                {{ item.last_error || '—' }}
              </span>
            </template>

            <template #cell-actions="{ item }">
              <div class="flex items-center justify-end gap-1">
                <Button size="sm" variant="ghost" @click="viewDetail(item)">
                  <Eye class="h-4 w-4" />
                </Button>
                <Button
                  v-if="item.status !== 'delivered'"
                  size="sm"
                  variant="ghost"
                  class="text-blue-600"
                  :title="t('crmQueue.replay')"
                  @click="replay(item)"
                >
                  <RotateCcw class="h-4 w-4" />
                </Button>
                <Button
                  size="sm"
                  variant="ghost"
                  class="text-destructive"
                  :title="t('crmQueue.discard')"
                  @click="pendingDiscardId = item.id"
                >
                  <Trash2 class="h-4 w-4" />
                </Button>
              </div>
            </template>
          </DataTable>
        </CardContent>
      </Card>
    </div>

    <!-- Detail dialog -->
    <Dialog v-model:open="detailOpen">
      <DialogContent class="max-w-3xl">
        <DialogHeader>
          <DialogTitle>{{ t('crmQueue.detailTitle') }}</DialogTitle>
          <DialogDescription v-if="selectedDetail">
            <code>{{ selectedDetail.event_type }}</code> · {{ formatDate(selectedDetail.created_at) }}
          </DialogDescription>
        </DialogHeader>
        <div v-if="selectedDetail" class="space-y-3 text-sm">
          <div class="grid grid-cols-2 gap-4">
            <div>
              <div class="text-muted-foreground text-xs mb-0.5">{{ t('crmQueue.status') }}</div>
              <Badge :variant="statusColor(selectedDetail.status) as any">
                {{ t(`crmQueue.statusLabel.${selectedDetail.status}`) }}
              </Badge>
            </div>
            <div>
              <div class="text-muted-foreground text-xs mb-0.5">{{ t('crmQueue.attempts') }}</div>
              <div>{{ selectedDetail.attempt_count }}</div>
            </div>
            <div class="col-span-2">
              <div class="text-muted-foreground text-xs mb-0.5">{{ t('crmQueue.endpoint') }}</div>
              <code class="text-xs break-all">{{ selectedDetail.endpoint }}</code>
            </div>
            <div v-if="selectedDetail.last_error" class="col-span-2">
              <div class="text-muted-foreground text-xs mb-0.5">{{ t('crmQueue.lastError') }}</div>
              <div class="text-xs text-destructive bg-destructive/5 border border-destructive/20 rounded p-2">
                {{ selectedDetail.last_error }}
              </div>
            </div>
          </div>
          <div>
            <div class="text-muted-foreground text-xs mb-0.5">{{ t('crmQueue.payload') }}</div>
            <ScrollArea class="h-64 rounded border bg-muted/40">
              <pre class="text-xs p-3 font-mono">{{ prettyPayload(selectedDetail.payload) }}</pre>
            </ScrollArea>
          </div>
        </div>
        <DialogFooter>
          <Button
            v-if="selectedDetail && selectedDetail.status !== 'delivered'"
            variant="default"
            @click="() => { if (selectedDetail) { replay(selectedDetail); detailOpen = false } }"
          >
            <RotateCcw class="h-4 w-4 mr-1.5" />
            {{ t('crmQueue.replay') }}
          </Button>
          <Button variant="outline" @click="detailOpen = false">
            {{ t('common.close') }}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- Discard confirm -->
    <AlertDialog :open="!!pendingDiscardId" @update:open="(v: boolean) => { if (!v) pendingDiscardId = null }">
      <AlertDialogContent>
        <AlertDialogHeader>
          <AlertDialogTitle>{{ t('crmQueue.discardConfirmTitle') }}</AlertDialogTitle>
          <AlertDialogDescription>
            {{ t('crmQueue.discardConfirmDesc') }}
          </AlertDialogDescription>
        </AlertDialogHeader>
        <AlertDialogFooter>
          <AlertDialogCancel>{{ t('common.cancel') }}</AlertDialogCancel>
          <AlertDialogAction class="bg-destructive text-destructive-foreground hover:bg-destructive/90" @click="confirmDiscard">
            {{ t('crmQueue.discard') }}
          </AlertDialogAction>
        </AlertDialogFooter>
      </AlertDialogContent>
    </AlertDialog>
  </div>
</template>
