<script setup lang="ts">
import { ref, onMounted, watch, computed } from 'vue'
import { useI18n } from 'vue-i18n'
import { crmQueueService, type CrmQueueRow, type CrmQueueListResponse } from '@/services/api'
import { useOrganizationsStore } from '@/stores/organizations'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { ScrollArea } from '@/components/ui/scroll-area'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle
} from '@/components/ui/dialog'
import {
  PageHeader,
  DataTable,
  DeleteConfirmDialog,
  ConfirmDialog,
  IconButton,
  ErrorState,
  type Column
} from '@/components/shared'
import { toast } from 'vue-sonner'
import { AlertCircle, RefreshCw, Trash2, Play, Eye, FileJson } from 'lucide-vue-next'
import { getErrorMessage } from '@/lib/api-utils'
import { formatDate } from '@/lib/utils'

// CRM Dead-Letter Queue administration view.
//
// Shows rows from the crm_event_queue table (scoped to the current org) with
// three possible states:
//
//   - pending      — awaiting next retry attempt
//   - delivered    — successfully sent to the CRM
//   - dead_letter  — exceeded MaxAttempts, needs operator intervention
//
// From this view operators can:
//   - filter by status + paginate
//   - inspect the signed payload that was built at emission time
//   - replay a row (reset to pending + fire immediate delivery attempt)
//   - permanently delete a row (for already-reconciled failures)

const { t } = useI18n()
const organizationsStore = useOrganizationsStore()

const rows = ref<CrmQueueRow[]>([])
const counters = ref({ pending: 0, dead_letter: 0, delivered: 0, total: 0 })
const isLoading = ref(false)
const error = ref(false)

// Filters + pagination
const statusFilter = ref<'' | 'pending' | 'dead_letter' | 'delivered'>('dead_letter')
const currentPage = ref(1)
const pageSize = 25
const totalItems = computed(() => {
  switch (statusFilter.value) {
    case 'pending':
      return counters.value.pending
    case 'dead_letter':
      return counters.value.dead_letter
    case 'delivered':
      return counters.value.delivered
    default:
      return counters.value.total
  }
})

// Row-level action state
const rowToReplay = ref<CrmQueueRow | null>(null)
const isReplayDialogOpen = ref(false)
const isReplaying = ref(false)

const rowToDelete = ref<CrmQueueRow | null>(null)
const isDeleteDialogOpen = ref(false)
const isDeleting = ref(false)

const rowToInspect = ref<CrmQueueRow | null>(null)
const isInspectDialogOpen = ref(false)

const columns = computed<Column<CrmQueueRow>[]>(() => [
  { key: 'event_type', label: t('crmQueue.columns.eventType') },
  { key: 'status', label: t('crmQueue.columns.status') },
  { key: 'attempt_count', label: t('crmQueue.columns.attempts') },
  { key: 'last_error', label: t('crmQueue.columns.lastError') },
  { key: 'next_attempt_at', label: t('crmQueue.columns.nextAttempt') },
  { key: 'created_at', label: t('crmQueue.columns.created') },
  { key: 'actions', label: t('common.actions'), align: 'right' }
])

async function fetchRows() {
  isLoading.value = true
  error.value = false
  try {
    const offset = (currentPage.value - 1) * pageSize
    const response = await crmQueueService.list({
      status: statusFilter.value || undefined,
      limit: pageSize,
      offset
    })
    const data = (response.data as any).data || response.data
    const body = data as CrmQueueListResponse
    rows.value = body.rows || []
    counters.value = {
      pending: body.pending ?? 0,
      dead_letter: body.dead_letter ?? 0,
      delivered: body.delivered ?? 0,
      total: body.total ?? 0
    }
  } catch (e) {
    error.value = true
    toast.error(getErrorMessage(e, t('crmQueue.fetchError')))
  } finally {
    isLoading.value = false
  }
}

function handlePageChange(page: number) {
  currentPage.value = page
  fetchRows()
}

function setStatusFilter(status: typeof statusFilter.value) {
  if (statusFilter.value === status) return
  statusFilter.value = status
  currentPage.value = 1
  fetchRows()
}

// --- Replay ----------------------------------------------------------------

function openReplayDialog(row: CrmQueueRow) {
  rowToReplay.value = row
  isReplayDialogOpen.value = true
}

async function confirmReplay() {
  if (!rowToReplay.value) return
  isReplaying.value = true
  try {
    const response = await crmQueueService.replay(rowToReplay.value.id)
    const data = (response.data as any).data || response.data
    const status = data?.status || 'pending'
    if (status === 'delivered') {
      toast.success(t('crmQueue.replayDelivered'))
    } else {
      toast.success(t('crmQueue.replayPending'))
    }
    isReplayDialogOpen.value = false
    rowToReplay.value = null
    await fetchRows()
  } catch (e) {
    toast.error(getErrorMessage(e, t('crmQueue.replayError')))
  } finally {
    isReplaying.value = false
  }
}

// --- Delete ----------------------------------------------------------------

function openDeleteDialog(row: CrmQueueRow) {
  rowToDelete.value = row
  isDeleteDialogOpen.value = true
}

async function confirmDelete() {
  if (!rowToDelete.value) return
  isDeleting.value = true
  try {
    await crmQueueService.delete(rowToDelete.value.id)
    toast.success(t('crmQueue.deleteSuccess'))
    isDeleteDialogOpen.value = false
    rowToDelete.value = null
    await fetchRows()
  } catch (e) {
    toast.error(getErrorMessage(e, t('crmQueue.deleteError')))
  } finally {
    isDeleting.value = false
  }
}

// --- Inspect payload -------------------------------------------------------

function openInspectDialog(row: CrmQueueRow) {
  rowToInspect.value = row
  isInspectDialogOpen.value = true
}

function prettyJSON(raw: string): string {
  // The payload stored in the queue is already valid JSON (as a string).
  // Parse + re-stringify so the dialog shows it nicely indented. If the
  // string is truncated (ends with ...), fall back to the raw text.
  try {
    return JSON.stringify(JSON.parse(raw), null, 2)
  } catch {
    return raw
  }
}

function statusVariant(status: string): 'default' | 'secondary' | 'destructive' | 'outline' {
  switch (status) {
    case 'pending':
      return 'secondary'
    case 'delivered':
      return 'default'
    case 'dead_letter':
      return 'destructive'
    default:
      return 'outline'
  }
}

function statusLabel(status: string): string {
  switch (status) {
    case 'pending':
      return t('crmQueue.statuses.pending')
    case 'delivered':
      return t('crmQueue.statuses.delivered')
    case 'dead_letter':
      return t('crmQueue.statuses.deadLetter')
    default:
      return status
  }
}

watch(() => organizationsStore.selectedOrgId, () => {
  currentPage.value = 1
  fetchRows()
})
onMounted(() => fetchRows())
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="$t('crmQueue.title')"
      :subtitle="$t('crmQueue.subtitle')"
      :icon="AlertCircle"
      icon-gradient="bg-gradient-to-br from-orange-500 to-red-600 shadow-orange-500/20"
    >
      <template #actions>
        <Button variant="outline" size="sm" @click="fetchRows" :disabled="isLoading">
          <RefreshCw class="h-4 w-4 mr-2" :class="{ 'animate-spin': isLoading }" />
          {{ $t('common.refresh') }}
        </Button>
      </template>
    </PageHeader>

    <ErrorState
      v-if="error && !isLoading"
      :title="$t('crmQueue.fetchErrorTitle')"
      :description="$t('crmQueue.fetchErrorDescription')"
      :retry-label="$t('common.retry')"
      class="flex-1"
      @retry="fetchRows"
    />

    <ScrollArea v-else class="flex-1">
      <div class="p-6">
        <div class="max-w-6xl mx-auto space-y-4">
          <!-- Status filter chips with counts -->
          <div class="flex gap-2 flex-wrap">
            <Button
              :variant="statusFilter === 'dead_letter' ? 'destructive' : 'outline'"
              size="sm"
              @click="setStatusFilter('dead_letter')"
            >
              {{ $t('crmQueue.statuses.deadLetter') }}
              <Badge variant="secondary" class="ml-2">{{ counters.dead_letter }}</Badge>
            </Button>
            <Button
              :variant="statusFilter === 'pending' ? 'default' : 'outline'"
              size="sm"
              @click="setStatusFilter('pending')"
            >
              {{ $t('crmQueue.statuses.pending') }}
              <Badge variant="secondary" class="ml-2">{{ counters.pending }}</Badge>
            </Button>
            <Button
              :variant="statusFilter === 'delivered' ? 'default' : 'outline'"
              size="sm"
              @click="setStatusFilter('delivered')"
            >
              {{ $t('crmQueue.statuses.delivered') }}
              <Badge variant="secondary" class="ml-2">{{ counters.delivered }}</Badge>
            </Button>
            <Button
              :variant="statusFilter === '' ? 'default' : 'outline'"
              size="sm"
              @click="setStatusFilter('')"
            >
              {{ $t('crmQueue.all') }}
              <Badge variant="secondary" class="ml-2">{{ counters.total }}</Badge>
            </Button>
          </div>

          <Card>
            <CardHeader>
              <CardTitle>{{ $t('crmQueue.tableTitle') }}</CardTitle>
              <CardDescription>{{ $t('crmQueue.tableDesc') }}</CardDescription>
            </CardHeader>
            <CardContent>
              <DataTable
                :items="rows"
                :columns="columns"
                :is-loading="isLoading"
                :empty-icon="AlertCircle"
                :empty-title="$t('crmQueue.empty')"
                :empty-description="$t('crmQueue.emptyDesc')"
                server-pagination
                :current-page="currentPage"
                :total-items="totalItems"
                :page-size="pageSize"
                item-name="events"
                @page-change="handlePageChange"
              >
                <template #cell-event_type="{ item: row }">
                  <span class="font-mono text-sm">{{ row.event_type }}</span>
                </template>

                <template #cell-status="{ item: row }">
                  <Badge :variant="statusVariant(row.status)">{{ statusLabel(row.status) }}</Badge>
                </template>

                <template #cell-attempt_count="{ item: row }">
                  <span class="text-sm">{{ row.attempt_count }} / 10</span>
                </template>

                <template #cell-last_error="{ item: row }">
                  <span
                    v-if="row.last_error"
                    class="text-xs text-muted-foreground max-w-[280px] truncate block"
                    :title="row.last_error"
                  >
                    {{ row.last_error }}
                  </span>
                  <span v-else class="text-xs text-muted-foreground">—</span>
                </template>

                <template #cell-next_attempt_at="{ item: row }">
                  <span class="text-sm text-muted-foreground">
                    {{ row.next_attempt_at ? formatDate(row.next_attempt_at) : '—' }}
                  </span>
                </template>

                <template #cell-created_at="{ item: row }">
                  <span class="text-sm text-muted-foreground">{{ formatDate(row.created_at) }}</span>
                </template>

                <template #cell-actions="{ item: row }">
                  <div class="flex items-center justify-end gap-1">
                    <IconButton
                      :icon="Eye"
                      :label="$t('crmQueue.inspect')"
                      class="h-8 w-8"
                      @click="openInspectDialog(row)"
                    />
                    <IconButton
                      v-if="row.status !== 'delivered'"
                      :icon="Play"
                      :label="$t('crmQueue.replay')"
                      class="h-8 w-8"
                      @click="openReplayDialog(row)"
                    />
                    <IconButton
                      :icon="Trash2"
                      :label="$t('common.delete')"
                      class="h-8 w-8 text-destructive"
                      @click="openDeleteDialog(row)"
                    />
                  </div>
                </template>
              </DataTable>
            </CardContent>
          </Card>
        </div>
      </div>
    </ScrollArea>

    <!-- Inspect payload dialog -->
    <Dialog v-model:open="isInspectDialogOpen">
      <DialogContent class="max-w-2xl">
        <DialogHeader>
          <DialogTitle class="flex items-center gap-2">
            <FileJson class="h-5 w-5" />
            {{ $t('crmQueue.inspectTitle') }}
          </DialogTitle>
          <DialogDescription>
            <span class="font-mono text-xs">{{ rowToInspect?.event_type }}</span>
            →
            <span class="font-mono text-xs">{{ rowToInspect?.endpoint }}</span>
          </DialogDescription>
        </DialogHeader>
        <div class="py-4">
          <pre
            class="bg-muted rounded p-3 text-xs font-mono overflow-auto max-h-[60vh] whitespace-pre-wrap break-all"
          >{{ rowToInspect ? prettyJSON(rowToInspect.payload_preview) : '' }}</pre>
          <p v-if="rowToInspect?.last_error" class="mt-3 text-xs text-destructive">
            <strong>{{ $t('crmQueue.lastErrorLabel') }}:</strong> {{ rowToInspect.last_error }}
          </p>
        </div>
      </DialogContent>
    </Dialog>

    <!-- Replay confirmation -->
    <ConfirmDialog
      v-model:open="isReplayDialogOpen"
      :title="$t('crmQueue.replayConfirmTitle')"
      :description="$t('crmQueue.replayConfirmDesc')"
      :confirm-label="$t('crmQueue.replay')"
      variant="default"
      :is-submitting="isReplaying"
      @confirm="confirmReplay"
    />

    <!-- Delete confirmation -->
    <DeleteConfirmDialog
      v-model:open="isDeleteDialogOpen"
      :title="$t('crmQueue.deleteTitle')"
      :item-name="rowToDelete?.event_type"
      :is-submitting="isDeleting"
      @confirm="confirmDelete"
    />
  </div>
</template>
