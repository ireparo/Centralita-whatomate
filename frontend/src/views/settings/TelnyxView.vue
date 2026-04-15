<script setup lang="ts">
import { ref, onMounted, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  telnyxConnectionsService,
  telnyxNumbersService,
  ivrFlowsService,
  type TelnyxConnection,
  type TelnyxNumber,
  type IVRFlow
} from '@/services/api'
import { useOrganizationsStore } from '@/stores/organizations'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Switch } from '@/components/ui/switch'
import { ScrollArea } from '@/components/ui/scroll-area'
import {
  Card,
  CardContent,
  CardDescription,
  CardHeader,
  CardTitle
} from '@/components/ui/card'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from '@/components/ui/dialog'
import {
  Select,
  SelectContent,
  SelectItem,
  SelectTrigger,
  SelectValue
} from '@/components/ui/select'
import {
  PageHeader,
  DataTable,
  DeleteConfirmDialog,
  IconButton,
  type Column
} from '@/components/shared'
import { toast } from 'vue-sonner'
import {
  Phone,
  Plus,
  Trash2,
  Pencil,
  Loader2,
  PlugZap,
  CheckCircle2,
  XCircle
} from 'lucide-vue-next'
import { getErrorMessage } from '@/lib/api-utils'
import { formatDate } from '@/lib/utils'

// Telnyx PSTN settings view.
//
// One page with two cards:
//   1. Connection card — credentials + status indicator. Upsert semantics
//      (the backend enforces 1 connection per org).
//   2. Numbers card — table of DDIs under the connection with CRUD dialogs.
//
// If no connection exists yet, the numbers card is hidden and the connection
// card shows an "Add connection" empty state.

const { t } = useI18n()
const organizationsStore = useOrganizationsStore()

// ---------------- Connection state ----------------

const connection = ref<TelnyxConnection | null>(null)
const isLoadingConnection = ref(false)

const isConnDialogOpen = ref(false)
const isSavingConn = ref(false)
const isTestingConn = ref(false)
const isDeleteConnDialogOpen = ref(false)
const isDeletingConn = ref(false)

const connForm = ref({
  label: '',
  api_key: '',
  public_key: '',
  call_control_app_id: '',
  outbound_profile_id: ''
})

function openCreateConnDialog() {
  connForm.value = {
    label: '',
    api_key: '',
    public_key: '',
    call_control_app_id: '',
    outbound_profile_id: ''
  }
  isConnDialogOpen.value = true
}

function openEditConnDialog() {
  if (!connection.value) return
  connForm.value = {
    label: connection.value.label,
    api_key: '',
    public_key: '',
    call_control_app_id: connection.value.call_control_app_id,
    outbound_profile_id: connection.value.outbound_profile_id
  }
  isConnDialogOpen.value = true
}

async function fetchConnection() {
  isLoadingConnection.value = true
  try {
    const response = await telnyxConnectionsService.get()
    const data = (response.data as any).data || response.data
    connection.value = data as TelnyxConnection
  } catch (e: any) {
    // 404 is expected when no connection exists yet — treat as "empty state".
    if (e?.response?.status === 404) {
      connection.value = null
    } else {
      toast.error(getErrorMessage(e, t('telnyx.connection.fetchError')))
    }
  } finally {
    isLoadingConnection.value = false
  }
}

async function saveConnection() {
  if (!connForm.value.label.trim() || !connForm.value.call_control_app_id.trim()) {
    toast.error(t('telnyx.connection.requiredFields'))
    return
  }
  // On create the API key is required; on update it is optional (empty =
  // keep the existing one).
  if (!connection.value && !connForm.value.api_key.trim()) {
    toast.error(t('telnyx.connection.apiKeyRequired'))
    return
  }
  isSavingConn.value = true
  try {
    if (connection.value) {
      await telnyxConnectionsService.update(connection.value.id, {
        label: connForm.value.label,
        call_control_app_id: connForm.value.call_control_app_id,
        outbound_profile_id: connForm.value.outbound_profile_id,
        ...(connForm.value.api_key ? { api_key: connForm.value.api_key } : {}),
        ...(connForm.value.public_key ? { public_key: connForm.value.public_key } : {})
      })
      toast.success(t('telnyx.connection.updatedSuccess'))
    } else {
      await telnyxConnectionsService.create({
        label: connForm.value.label,
        api_key: connForm.value.api_key,
        public_key: connForm.value.public_key,
        call_control_app_id: connForm.value.call_control_app_id,
        outbound_profile_id: connForm.value.outbound_profile_id
      })
      toast.success(t('telnyx.connection.createdSuccess'))
    }
    isConnDialogOpen.value = false
    await fetchConnection()
    await fetchNumbers()
  } catch (e) {
    toast.error(getErrorMessage(e, t('telnyx.connection.saveError')))
  } finally {
    isSavingConn.value = false
  }
}

async function testConnection() {
  // Test the currently-typed key if present, else test the saved one.
  isTestingConn.value = true
  try {
    const response = await telnyxConnectionsService.test(
      connForm.value.api_key ? { api_key: connForm.value.api_key } : undefined
    )
    const data = (response.data as any).data || response.data
    if (data?.ok) {
      toast.success(t('telnyx.connection.testOk'))
    } else {
      toast.error(data?.error || t('telnyx.connection.testFailed'))
    }
  } catch (e) {
    toast.error(getErrorMessage(e, t('telnyx.connection.testFailed')))
  } finally {
    isTestingConn.value = false
  }
}

async function deleteConnection() {
  if (!connection.value) return
  isDeletingConn.value = true
  try {
    await telnyxConnectionsService.delete(connection.value.id)
    toast.success(t('telnyx.connection.deletedSuccess'))
    isDeleteConnDialogOpen.value = false
    connection.value = null
    numbers.value = []
  } catch (e) {
    toast.error(getErrorMessage(e, t('telnyx.connection.deleteError')))
  } finally {
    isDeletingConn.value = false
  }
}

// ---------------- Numbers state ----------------

const numbers = ref<TelnyxNumber[]>([])
const isLoadingNumbers = ref(false)

const ivrFlows = ref<IVRFlow[]>([])

const isNumberDialogOpen = ref(false)
const isSavingNumber = ref(false)
const editingNumberId = ref<string | null>(null)
const numberForm = ref({
  phone_number: '',
  label: '',
  country: 'ES',
  number_type: 'geographic',
  telnyx_number_id: '',
  ivr_flow_id: '' as string,
  is_active: true,
  recording_enabled: false
})

const isDeleteNumberDialogOpen = ref(false)
const numberToDelete = ref<TelnyxNumber | null>(null)
const isDeletingNumber = ref(false)

const numberColumns = computed<Column<TelnyxNumber>[]>(() => [
  { key: 'label', label: t('telnyx.numbers.columns.label') },
  { key: 'phone_number', label: t('telnyx.numbers.columns.phone') },
  { key: 'number_type', label: t('telnyx.numbers.columns.type') },
  { key: 'ivr_flow_name', label: t('telnyx.numbers.columns.ivrFlow') },
  { key: 'status', label: t('telnyx.numbers.columns.status') },
  { key: 'recording_enabled', label: t('telnyx.numbers.columns.recording') },
  { key: 'actions', label: t('common.actions'), align: 'right' }
])

async function fetchNumbers() {
  if (!connection.value) {
    numbers.value = []
    return
  }
  isLoadingNumbers.value = true
  try {
    const response = await telnyxNumbersService.list()
    const data = (response.data as any).data || response.data
    numbers.value = data.numbers || []
  } catch (e) {
    toast.error(getErrorMessage(e, t('telnyx.numbers.fetchError')))
  } finally {
    isLoadingNumbers.value = false
  }
}

async function fetchIvrFlows() {
  try {
    const response = await ivrFlowsService.list({ limit: 200 })
    const data = (response.data as any).data || response.data
    ivrFlows.value = data.ivr_flows || []
  } catch (e) {
    // Non-fatal: the dropdown is empty but the form still works.
    console.warn('Failed to load IVR flows', e)
  }
}

function openCreateNumberDialog() {
  editingNumberId.value = null
  numberForm.value = {
    phone_number: '',
    label: '',
    country: 'ES',
    number_type: 'geographic',
    telnyx_number_id: '',
    ivr_flow_id: '',
    is_active: true,
    recording_enabled: false
  }
  isNumberDialogOpen.value = true
}

function openEditNumberDialog(num: TelnyxNumber) {
  editingNumberId.value = num.id
  numberForm.value = {
    phone_number: num.phone_number,
    label: num.label,
    country: num.country || 'ES',
    number_type: num.number_type || 'geographic',
    telnyx_number_id: num.telnyx_number_id,
    ivr_flow_id: num.ivr_flow_id || '',
    is_active: num.is_active,
    recording_enabled: num.recording_enabled
  }
  isNumberDialogOpen.value = true
}

async function saveNumber() {
  if (!connection.value) return
  if (!numberForm.value.phone_number.trim()) {
    toast.error(t('telnyx.numbers.phoneRequired'))
    return
  }
  isSavingNumber.value = true
  try {
    if (editingNumberId.value) {
      await telnyxNumbersService.update(editingNumberId.value, {
        phone_number: numberForm.value.phone_number,
        label: numberForm.value.label,
        country: numberForm.value.country,
        number_type: numberForm.value.number_type,
        telnyx_number_id: numberForm.value.telnyx_number_id,
        ivr_flow_id: numberForm.value.ivr_flow_id || null,
        is_active: numberForm.value.is_active,
        recording_enabled: numberForm.value.recording_enabled
      })
      toast.success(t('telnyx.numbers.updatedSuccess'))
    } else {
      await telnyxNumbersService.create({
        connection_id: connection.value.id,
        phone_number: numberForm.value.phone_number,
        label: numberForm.value.label,
        country: numberForm.value.country,
        number_type: numberForm.value.number_type,
        telnyx_number_id: numberForm.value.telnyx_number_id,
        ivr_flow_id: numberForm.value.ivr_flow_id || null,
        is_active: numberForm.value.is_active,
        recording_enabled: numberForm.value.recording_enabled
      })
      toast.success(t('telnyx.numbers.createdSuccess'))
    }
    isNumberDialogOpen.value = false
    await fetchNumbers()
  } catch (e) {
    toast.error(getErrorMessage(e, t('telnyx.numbers.saveError')))
  } finally {
    isSavingNumber.value = false
  }
}

async function deleteNumber() {
  if (!numberToDelete.value) return
  isDeletingNumber.value = true
  try {
    await telnyxNumbersService.delete(numberToDelete.value.id)
    toast.success(t('telnyx.numbers.deletedSuccess'))
    isDeleteNumberDialogOpen.value = false
    numberToDelete.value = null
    await fetchNumbers()
  } catch (e) {
    toast.error(getErrorMessage(e, t('telnyx.numbers.deleteError')))
  } finally {
    isDeletingNumber.value = false
  }
}

function formatPhone(phone: string): string {
  // Pretty-print E.164-no-plus as +<country> <rest> with light spacing.
  if (!phone) return ''
  return '+' + phone
}

watch(() => organizationsStore.selectedOrgId, async () => {
  await fetchConnection()
  await fetchNumbers()
})

onMounted(async () => {
  await Promise.all([fetchConnection(), fetchIvrFlows()])
  await fetchNumbers()
})
</script>

<template>
  <div class="flex flex-col h-full bg-[#0a0a0b] light:bg-gray-50">
    <PageHeader
      :title="$t('telnyx.title')"
      :subtitle="$t('telnyx.subtitle')"
      :icon="Phone"
      icon-gradient="bg-gradient-to-br from-emerald-500 to-teal-600 shadow-emerald-500/20"
    />

    <ScrollArea class="flex-1">
      <div class="p-6">
        <div class="max-w-6xl mx-auto space-y-6">
          <!-- Connection card -->
          <Card>
            <CardHeader>
              <div class="flex items-center justify-between flex-wrap gap-4">
                <div>
                  <CardTitle class="flex items-center gap-2">
                    <PlugZap class="h-5 w-5" />
                    {{ $t('telnyx.connection.title') }}
                  </CardTitle>
                  <CardDescription>{{ $t('telnyx.connection.description') }}</CardDescription>
                </div>
                <div v-if="connection" class="flex gap-2">
                  <Button variant="outline" size="sm" @click="openEditConnDialog">
                    <Pencil class="h-4 w-4 mr-2" />
                    {{ $t('common.edit') }}
                  </Button>
                  <Button
                    variant="outline"
                    size="sm"
                    class="text-destructive"
                    @click="isDeleteConnDialogOpen = true"
                  >
                    <Trash2 class="h-4 w-4 mr-2" />
                    {{ $t('common.delete') }}
                  </Button>
                </div>
              </div>
            </CardHeader>

            <CardContent>
              <!-- Empty state -->
              <div v-if="!connection && !isLoadingConnection" class="text-center py-10">
                <PlugZap class="h-10 w-10 mx-auto text-muted-foreground mb-3" />
                <p class="text-muted-foreground mb-4">{{ $t('telnyx.connection.empty') }}</p>
                <Button size="sm" @click="openCreateConnDialog">
                  <Plus class="h-4 w-4 mr-2" />
                  {{ $t('telnyx.connection.add') }}
                </Button>
              </div>

              <!-- Existing connection summary -->
              <div v-else-if="connection" class="grid grid-cols-1 md:grid-cols-2 gap-4 text-sm">
                <div>
                  <Label class="text-muted-foreground">{{ $t('telnyx.connection.label') }}</Label>
                  <div class="mt-1 font-medium">{{ connection.label }}</div>
                </div>
                <div>
                  <Label class="text-muted-foreground">{{ $t('telnyx.connection.status') }}</Label>
                  <div class="mt-1">
                    <Badge
                      :variant="connection.status === 'active' ? 'default' : 'destructive'"
                      class="flex items-center gap-1 w-fit"
                    >
                      <CheckCircle2 v-if="connection.status === 'active'" class="h-3 w-3" />
                      <XCircle v-else class="h-3 w-3" />
                      {{ connection.status }}
                    </Badge>
                  </div>
                </div>
                <div>
                  <Label class="text-muted-foreground">{{ $t('telnyx.connection.callControlAppId') }}</Label>
                  <div class="mt-1 font-mono text-xs">{{ connection.call_control_app_id }}</div>
                </div>
                <div>
                  <Label class="text-muted-foreground">{{ $t('telnyx.connection.outboundProfileId') }}</Label>
                  <div class="mt-1 font-mono text-xs">{{ connection.outbound_profile_id || '—' }}</div>
                </div>
                <div>
                  <Label class="text-muted-foreground">{{ $t('telnyx.connection.apiKey') }}</Label>
                  <div class="mt-1 font-mono text-xs flex items-center gap-2">
                    <span>{{ connection.has_api_key ? '••••••••' : '—' }}</span>
                    <Badge v-if="connection.has_api_key" variant="secondary" class="text-xs">{{ $t('telnyx.connection.encrypted') }}</Badge>
                  </div>
                </div>
                <div>
                  <Label class="text-muted-foreground">{{ $t('telnyx.connection.lastVerified') }}</Label>
                  <div class="mt-1 text-xs">{{ connection.last_verified_at ? formatDate(connection.last_verified_at) : '—' }}</div>
                </div>
              </div>
            </CardContent>
          </Card>

          <!-- Numbers card (only when connection exists) -->
          <Card v-if="connection">
            <CardHeader>
              <div class="flex items-center justify-between flex-wrap gap-4">
                <div>
                  <CardTitle>{{ $t('telnyx.numbers.title') }}</CardTitle>
                  <CardDescription>{{ $t('telnyx.numbers.description') }}</CardDescription>
                </div>
                <Button variant="outline" size="sm" @click="openCreateNumberDialog">
                  <Plus class="h-4 w-4 mr-2" />
                  {{ $t('telnyx.numbers.add') }}
                </Button>
              </div>
            </CardHeader>
            <CardContent>
              <DataTable
                :items="numbers"
                :columns="numberColumns"
                :is-loading="isLoadingNumbers"
                :empty-icon="Phone"
                :empty-title="$t('telnyx.numbers.empty')"
                :empty-description="$t('telnyx.numbers.emptyDesc')"
                item-name="numbers"
              >
                <template #cell-label="{ item: num }">
                  <span class="font-medium">{{ num.label || '—' }}</span>
                </template>
                <template #cell-phone_number="{ item: num }">
                  <span class="font-mono text-sm">{{ formatPhone(num.phone_number) }}</span>
                </template>
                <template #cell-number_type="{ item: num }">
                  <Badge variant="outline" class="text-xs">{{ num.number_type || '—' }}</Badge>
                </template>
                <template #cell-ivr_flow_name="{ item: num }">
                  <span v-if="num.ivr_flow_name" class="text-sm">{{ num.ivr_flow_name }}</span>
                  <span v-else class="text-xs text-muted-foreground">{{ $t('telnyx.numbers.noFlow') }}</span>
                </template>
                <template #cell-status="{ item: num }">
                  <Badge :variant="num.is_active ? 'default' : 'secondary'">
                    {{ num.is_active ? $t('common.active') : $t('common.inactive') }}
                  </Badge>
                </template>
                <template #cell-recording_enabled="{ item: num }">
                  <Badge v-if="num.recording_enabled" variant="secondary">{{ $t('common.on') }}</Badge>
                  <span v-else class="text-xs text-muted-foreground">{{ $t('common.off') }}</span>
                </template>
                <template #cell-actions="{ item: num }">
                  <div class="flex items-center justify-end gap-1">
                    <IconButton
                      :icon="Pencil"
                      :label="$t('common.edit')"
                      class="h-8 w-8"
                      @click="openEditNumberDialog(num)"
                    />
                    <IconButton
                      :icon="Trash2"
                      :label="$t('common.delete')"
                      class="h-8 w-8 text-destructive"
                      @click="numberToDelete = num; isDeleteNumberDialogOpen = true"
                    />
                  </div>
                </template>
                <template #empty-action>
                  <Button variant="outline" size="sm" @click="openCreateNumberDialog">
                    <Plus class="h-4 w-4 mr-2" />
                    {{ $t('telnyx.numbers.add') }}
                  </Button>
                </template>
              </DataTable>
            </CardContent>
          </Card>
        </div>
      </div>
    </ScrollArea>

    <!-- Connection dialog -->
    <Dialog v-model:open="isConnDialogOpen">
      <DialogContent class="max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {{ connection ? $t('telnyx.connection.editTitle') : $t('telnyx.connection.addTitle') }}
          </DialogTitle>
          <DialogDescription>{{ $t('telnyx.connection.formDesc') }}</DialogDescription>
        </DialogHeader>
        <div class="space-y-4 py-4">
          <div class="space-y-2">
            <Label for="label">{{ $t('telnyx.connection.label') }}</Label>
            <Input id="label" v-model="connForm.label" :placeholder="$t('telnyx.connection.labelPlaceholder')" />
          </div>
          <div class="space-y-2">
            <Label for="api_key">{{ $t('telnyx.connection.apiKey') }}</Label>
            <Input
              id="api_key"
              v-model="connForm.api_key"
              type="password"
              :placeholder="connection ? $t('telnyx.connection.apiKeyEditPlaceholder') : 'KEY...'"
            />
            <p class="text-xs text-muted-foreground">
              {{ connection ? $t('telnyx.connection.apiKeyEditHint') : $t('telnyx.connection.apiKeyHint') }}
            </p>
          </div>
          <div class="space-y-2">
            <Label for="public_key">{{ $t('telnyx.connection.publicKey') }}</Label>
            <Input
              id="public_key"
              v-model="connForm.public_key"
              type="password"
              :placeholder="connection && connection.has_public_key ? $t('telnyx.connection.publicKeyEditPlaceholder') : ''"
            />
            <p class="text-xs text-muted-foreground">{{ $t('telnyx.connection.publicKeyHint') }}</p>
          </div>
          <div class="space-y-2">
            <Label for="call_control_app_id">{{ $t('telnyx.connection.callControlAppId') }}</Label>
            <Input
              id="call_control_app_id"
              v-model="connForm.call_control_app_id"
              placeholder="2a55f0c1-1234-5678-abcd-ef0123456789"
            />
          </div>
          <div class="space-y-2">
            <Label for="outbound_profile_id">{{ $t('telnyx.connection.outboundProfileId') }}</Label>
            <Input id="outbound_profile_id" v-model="connForm.outbound_profile_id" :placeholder="$t('telnyx.connection.outboundProfileOptional')" />
          </div>
        </div>
        <DialogFooter class="sm:justify-between">
          <Button
            variant="outline"
            size="sm"
            type="button"
            :disabled="isTestingConn || (!connForm.api_key && !connection)"
            @click="testConnection"
          >
            <Loader2 v-if="isTestingConn" class="h-4 w-4 mr-2 animate-spin" />
            {{ $t('telnyx.connection.testConnection') }}
          </Button>
          <div class="flex gap-2">
            <Button variant="outline" @click="isConnDialogOpen = false">{{ $t('common.cancel') }}</Button>
            <Button @click="saveConnection" :disabled="isSavingConn">
              <Loader2 v-if="isSavingConn" class="h-4 w-4 mr-2 animate-spin" />
              {{ connection ? $t('common.update') : $t('common.create') }}
            </Button>
          </div>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <!-- Number dialog -->
    <Dialog v-model:open="isNumberDialogOpen">
      <DialogContent class="max-w-lg">
        <DialogHeader>
          <DialogTitle>
            {{ editingNumberId ? $t('telnyx.numbers.editTitle') : $t('telnyx.numbers.addTitle') }}
          </DialogTitle>
          <DialogDescription>{{ $t('telnyx.numbers.formDesc') }}</DialogDescription>
        </DialogHeader>
        <div class="space-y-4 py-4">
          <div class="space-y-2">
            <Label for="n_label">{{ $t('telnyx.numbers.labelField') }}</Label>
            <Input id="n_label" v-model="numberForm.label" :placeholder="$t('telnyx.numbers.labelPlaceholder')" />
          </div>
          <div class="space-y-2">
            <Label for="phone_number">{{ $t('telnyx.numbers.phoneField') }}</Label>
            <Input id="phone_number" v-model="numberForm.phone_number" placeholder="+34 873 94 07 02" />
            <p class="text-xs text-muted-foreground">{{ $t('telnyx.numbers.phoneHint') }}</p>
          </div>
          <div class="grid grid-cols-2 gap-4">
            <div class="space-y-2">
              <Label for="country">{{ $t('telnyx.numbers.country') }}</Label>
              <Input id="country" v-model="numberForm.country" placeholder="ES" maxlength="2" />
            </div>
            <div class="space-y-2">
              <Label for="number_type">{{ $t('telnyx.numbers.type') }}</Label>
              <Select v-model="numberForm.number_type">
                <SelectTrigger id="number_type">
                  <SelectValue :placeholder="$t('telnyx.numbers.typePlaceholder')" />
                </SelectTrigger>
                <SelectContent>
                  <SelectItem value="geographic">{{ $t('telnyx.numbers.types.geographic') }}</SelectItem>
                  <SelectItem value="mobile">{{ $t('telnyx.numbers.types.mobile') }}</SelectItem>
                  <SelectItem value="toll_free">{{ $t('telnyx.numbers.types.toll_free') }}</SelectItem>
                  <SelectItem value="national">{{ $t('telnyx.numbers.types.national') }}</SelectItem>
                  <SelectItem value="virtual">{{ $t('telnyx.numbers.types.virtual') }}</SelectItem>
                </SelectContent>
              </Select>
            </div>
          </div>
          <div class="space-y-2">
            <Label for="ivr_flow">{{ $t('telnyx.numbers.ivrFlow') }}</Label>
            <Select v-model="numberForm.ivr_flow_id">
              <SelectTrigger id="ivr_flow">
                <SelectValue :placeholder="$t('telnyx.numbers.ivrFlowPlaceholder')" />
              </SelectTrigger>
              <SelectContent>
                <SelectItem value="">{{ $t('telnyx.numbers.noFlow') }}</SelectItem>
                <SelectItem v-for="f in ivrFlows" :key="f.id" :value="f.id">{{ f.name }}</SelectItem>
              </SelectContent>
            </Select>
            <p class="text-xs text-muted-foreground">{{ $t('telnyx.numbers.ivrFlowHint') }}</p>
          </div>
          <div class="space-y-2">
            <Label for="telnyx_number_id">{{ $t('telnyx.numbers.telnyxIdField') }}</Label>
            <Input id="telnyx_number_id" v-model="numberForm.telnyx_number_id" :placeholder="$t('telnyx.numbers.telnyxIdPlaceholder')" />
          </div>
          <div class="flex items-center justify-between">
            <div>
              <Label for="is_active" class="text-sm">{{ $t('telnyx.numbers.active') }}</Label>
              <p class="text-xs text-muted-foreground">{{ $t('telnyx.numbers.activeHint') }}</p>
            </div>
            <Switch id="is_active" v-model:checked="numberForm.is_active" />
          </div>
          <div class="flex items-center justify-between">
            <div>
              <Label for="recording" class="text-sm">{{ $t('telnyx.numbers.recording') }}</Label>
              <p class="text-xs text-muted-foreground">{{ $t('telnyx.numbers.recordingHint') }}</p>
            </div>
            <Switch id="recording" v-model:checked="numberForm.recording_enabled" />
          </div>
        </div>
        <DialogFooter>
          <Button variant="outline" @click="isNumberDialogOpen = false">{{ $t('common.cancel') }}</Button>
          <Button @click="saveNumber" :disabled="isSavingNumber">
            <Loader2 v-if="isSavingNumber" class="h-4 w-4 mr-2 animate-spin" />
            {{ editingNumberId ? $t('common.update') : $t('common.create') }}
          </Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>

    <DeleteConfirmDialog
      v-model:open="isDeleteConnDialogOpen"
      :title="$t('telnyx.connection.deleteTitle')"
      :item-name="connection?.label"
      :is-submitting="isDeletingConn"
      @confirm="deleteConnection"
    />

    <DeleteConfirmDialog
      v-model:open="isDeleteNumberDialogOpen"
      :title="$t('telnyx.numbers.deleteTitle')"
      :item-name="numberToDelete?.label || numberToDelete?.phone_number"
      :is-submitting="isDeletingNumber"
      @confirm="deleteNumber"
    />
  </div>
</template>
