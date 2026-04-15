<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import {
  whatsmeowGroupService,
  type WhatsmeowGroupInfo,
  type WhatsmeowParticipantAction
} from '@/services/api'
import { toast } from 'vue-sonner'
import { getErrorMessage } from '@/lib/api-utils'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogFooter,
  DialogHeader,
  DialogTitle
} from '@/components/ui/dialog'
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Separator } from '@/components/ui/separator'
import {
  Users,
  Loader2,
  UserPlus,
  UserMinus,
  ShieldCheck,
  Shield,
  Pencil,
  LogOut
} from 'lucide-vue-next'

// Self-contained group administration panel for WhatsApp groups paired
// via whatsmeow. Opened from the chat header when is_group === true.
//
// Features:
//   - Lists participants with super-admin / admin / member badges.
//   - Add participants by phone (E.164).
//   - Remove / promote / demote per participant (via row buttons).
//   - Rename group (inline edit + save).
//   - Leave group (confirmation).
//
// Shows a warning banner at the top reminding operators that
// participant changes are visible to everyone in the group.

interface Props {
  /** Must be a Contact whose is_group is true. */
  contactId: string
  /** Controlled open state — parent binds v-model:open */
  open: boolean
}

const props = defineProps<Props>()
const emit = defineEmits<{
  (e: 'update:open', value: boolean): void
  (e: 'subject-changed', newSubject: string): void
  (e: 'left'): void
}>()

const { t } = useI18n()

const info = ref<WhatsmeowGroupInfo | null>(null)
const isLoading = ref(false)
const isBusy = ref(false)
const error = ref<string | null>(null)

// Add-participant form.
const addPhone = ref('')

// Rename form.
const editingSubject = ref(false)
const subjectInput = ref('')

async function refresh() {
  isLoading.value = true
  error.value = null
  try {
    const resp = await whatsmeowGroupService.info(props.contactId)
    const data = (resp.data as any).data || resp.data
    info.value = data as WhatsmeowGroupInfo
    subjectInput.value = info.value.subject
  } catch (e) {
    error.value = getErrorMessage(e, t('whatsmeowGroup.fetchError'))
  } finally {
    isLoading.value = false
  }
}

watch(() => props.open, open => {
  if (open) refresh()
})

function sortedParticipants() {
  if (!info.value) return []
  // Super-admins first, then admins, then members alphabetically.
  return [...info.value.participants].sort((a, b) => {
    const rank = (p: typeof a) => (p.is_super_admin ? 0 : p.is_admin ? 1 : 2)
    const ra = rank(a)
    const rb = rank(b)
    if (ra !== rb) return ra - rb
    return (a.display_name || a.phone).localeCompare(b.display_name || b.phone)
  })
}

async function applyAction(action: WhatsmeowParticipantAction, phones: string[]) {
  if (phones.length === 0) return
  isBusy.value = true
  try {
    const resp = await whatsmeowGroupService.updateParticipants(props.contactId, action, phones)
    const data = (resp.data as any).data || resp.data
    const accepted = (data?.accepted || []) as string[]
    if (accepted.length < phones.length) {
      toast.warning(t('whatsmeowGroup.partialAccepted', {
        accepted: accepted.length,
        total: phones.length
      }))
    } else {
      toast.success(t(`whatsmeowGroup.${action}Ok`))
    }
    await refresh()
  } catch (e) {
    toast.error(getErrorMessage(e, t(`whatsmeowGroup.${action}Failed`)))
  } finally {
    isBusy.value = false
  }
}

async function onAddParticipant() {
  const phone = addPhone.value.replace(/[^\d]/g, '')
  if (!phone) return
  await applyAction('add', [phone])
  addPhone.value = ''
}

async function onRenameSubmit() {
  const next = subjectInput.value.trim()
  if (!next || !info.value) return
  if (next === info.value.subject) {
    editingSubject.value = false
    return
  }
  isBusy.value = true
  try {
    await whatsmeowGroupService.setSubject(props.contactId, next)
    if (info.value) info.value.subject = next
    editingSubject.value = false
    toast.success(t('whatsmeowGroup.renameOk'))
    emit('subject-changed', next)
  } catch (e) {
    toast.error(getErrorMessage(e, t('whatsmeowGroup.renameFailed')))
  } finally {
    isBusy.value = false
  }
}

async function onLeave() {
  if (!confirm(t('whatsmeowGroup.leaveConfirm'))) return
  isBusy.value = true
  try {
    await whatsmeowGroupService.leave(props.contactId)
    toast.success(t('whatsmeowGroup.leaveOk'))
    emit('update:open', false)
    emit('left')
  } catch (e) {
    toast.error(getErrorMessage(e, t('whatsmeowGroup.leaveFailed')))
  } finally {
    isBusy.value = false
  }
}

const participantCount = computed(() => info.value?.participants.length || 0)
</script>

<template>
  <Dialog :open="props.open" @update:open="emit('update:open', $event)">
    <DialogContent class="max-w-lg">
      <DialogHeader>
        <DialogTitle class="flex items-center gap-2">
          <Users class="h-5 w-5" />
          {{ $t('whatsmeowGroup.title') }}
        </DialogTitle>
        <DialogDescription>
          {{ $t('whatsmeowGroup.description', { count: participantCount }) }}
        </DialogDescription>
      </DialogHeader>

      <div v-if="error" class="text-sm text-destructive py-2">{{ error }}</div>

      <!-- Rename row -->
      <div class="space-y-1.5">
        <Label class="text-xs">{{ $t('whatsmeowGroup.subject') }}</Label>
        <div class="flex gap-2">
          <Input v-if="editingSubject" v-model="subjectInput" :disabled="isBusy" class="flex-1" />
          <Input v-else :model-value="info?.subject || ''" readonly class="flex-1 bg-muted/20" />
          <Button
            v-if="!editingSubject"
            variant="outline"
            size="sm"
            :disabled="isLoading || isBusy"
            @click="editingSubject = true; subjectInput = info?.subject || ''"
          >
            <Pencil class="h-4 w-4" />
          </Button>
          <Button v-else size="sm" :disabled="isBusy" @click="onRenameSubmit">
            <Loader2 v-if="isBusy" class="h-4 w-4 animate-spin" />
            <span v-else>{{ $t('common.save') }}</span>
          </Button>
          <Button v-if="editingSubject" variant="ghost" size="sm" @click="editingSubject = false">
            {{ $t('common.cancel') }}
          </Button>
        </div>
      </div>

      <Separator />

      <!-- Add participant -->
      <div class="space-y-1.5">
        <Label class="text-xs">{{ $t('whatsmeowGroup.addParticipant') }}</Label>
        <div class="flex gap-2">
          <Input v-model="addPhone" placeholder="+34 666 11 22 33" class="flex-1" />
          <Button size="sm" :disabled="isBusy || !addPhone.trim()" @click="onAddParticipant">
            <UserPlus class="h-4 w-4 mr-1" />
            {{ $t('whatsmeowGroup.add') }}
          </Button>
        </div>
        <p class="text-[11px] text-muted-foreground">
          {{ $t('whatsmeowGroup.addHint') }}
        </p>
      </div>

      <Separator />

      <!-- Participant list -->
      <div class="space-y-1.5">
        <Label class="text-xs flex items-center justify-between">
          <span>{{ $t('whatsmeowGroup.participants', { count: participantCount }) }}</span>
          <Loader2 v-if="isLoading" class="h-3 w-3 animate-spin" />
        </Label>
        <div class="max-h-[280px] overflow-y-auto space-y-1">
          <div
            v-for="p in sortedParticipants()"
            :key="p.phone"
            class="flex items-center gap-2 text-sm py-1.5 px-2 rounded hover:bg-muted/30"
          >
            <div class="flex-1 min-w-0">
              <div class="flex items-center gap-1.5 flex-wrap">
                <span class="font-medium truncate">{{ p.display_name || '+' + p.phone }}</span>
                <Badge v-if="p.is_super_admin" variant="secondary" class="text-[9px] h-4 px-1.5">
                  <ShieldCheck class="h-2.5 w-2.5 mr-0.5" />
                  {{ $t('whatsmeowGroup.roles.superAdmin') }}
                </Badge>
                <Badge v-else-if="p.is_admin" variant="secondary" class="text-[9px] h-4 px-1.5">
                  <Shield class="h-2.5 w-2.5 mr-0.5" />
                  {{ $t('whatsmeowGroup.roles.admin') }}
                </Badge>
              </div>
              <div class="text-[11px] text-muted-foreground font-mono">+{{ p.phone }}</div>
            </div>
            <div class="flex items-center gap-1">
              <Button
                v-if="!p.is_admin"
                variant="ghost"
                size="sm"
                class="h-7 px-2"
                :title="$t('whatsmeowGroup.promote')"
                :disabled="isBusy"
                @click="applyAction('promote', [p.phone])"
              >
                <Shield class="h-3.5 w-3.5" />
              </Button>
              <Button
                v-else-if="!p.is_super_admin"
                variant="ghost"
                size="sm"
                class="h-7 px-2"
                :title="$t('whatsmeowGroup.demote')"
                :disabled="isBusy"
                @click="applyAction('demote', [p.phone])"
              >
                <Shield class="h-3.5 w-3.5 text-muted-foreground" />
              </Button>
              <Button
                variant="ghost"
                size="sm"
                class="h-7 px-2 text-destructive"
                :title="$t('whatsmeowGroup.remove')"
                :disabled="isBusy || p.is_super_admin"
                @click="applyAction('remove', [p.phone])"
              >
                <UserMinus class="h-3.5 w-3.5" />
              </Button>
            </div>
          </div>
          <div v-if="!isLoading && participantCount === 0" class="text-sm text-muted-foreground text-center py-4">
            {{ $t('whatsmeowGroup.noParticipants') }}
          </div>
        </div>
      </div>

      <DialogFooter class="sm:justify-between">
        <Button variant="outline" size="sm" class="text-destructive" :disabled="isBusy" @click="onLeave">
          <LogOut class="h-4 w-4 mr-2" />
          {{ $t('whatsmeowGroup.leave') }}
        </Button>
        <Button variant="outline" @click="emit('update:open', false)">{{ $t('common.close') }}</Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
