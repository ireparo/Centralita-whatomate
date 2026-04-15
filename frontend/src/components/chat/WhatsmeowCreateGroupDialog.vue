<script setup lang="ts">
import { ref, computed, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
import { useRouter } from 'vue-router'
import {
  whatsmeowGroupService,
  accountsService,
} from '@/services/api'
import { getErrorMessage } from '@/lib/api-utils'
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
import { Button } from '@/components/ui/button'
import { Input } from '@/components/ui/input'
import { Label } from '@/components/ui/label'
import { Badge } from '@/components/ui/badge'
import { Users, UserPlus, X, Loader2 } from 'lucide-vue-next'

// Standalone "Create WhatsApp group" dialog.
//
// Opens from the chat sidebar (or wherever the parent chooses) to drive
// the POST /api/accounts/{id}/whatsmeow/groups endpoint. On success,
// navigates to the newly created group chat.
//
// Only shown for whatsmeow-provider accounts — creating groups is
// unsupported on the Cloud API. The account selector filters itself
// to whatsmeow accounts only; if there are none, the form shows a
// disabled state with a link back to account settings.

interface Props {
  open: boolean
}
const props = defineProps<Props>()
const emit = defineEmits<{
  (e: 'update:open', value: boolean): void
  (e: 'created', contactId: string): void
}>()

const { t } = useI18n()
const router = useRouter()

// Form state
const accountId = ref<string>('')
const subject = ref('')
const phoneInput = ref('')
const phones = ref<string[]>([])
const isCreating = ref(false)

// Account list (filtered to whatsmeow)
interface Account {
  id: string
  name: string
  provider?: string
  status?: string
}
const accounts = ref<Account[]>([])
const isLoadingAccounts = ref(false)

const whatsmeowAccounts = computed(() => accounts.value.filter(a => a.provider === 'whatsmeow'))

async function loadAccounts() {
  isLoadingAccounts.value = true
  try {
    const resp = await accountsService.list()
    const data = (resp.data as any).data || resp.data
    accounts.value = (data as any[]) as Account[]
    if (whatsmeowAccounts.value.length > 0 && !accountId.value) {
      accountId.value = whatsmeowAccounts.value[0].id
    }
  } finally {
    isLoadingAccounts.value = false
  }
}

watch(
  () => props.open,
  open => {
    if (open) {
      // Reset + reload on every open so the account list reflects any
      // recently paired accounts.
      subject.value = ''
      phoneInput.value = ''
      phones.value = []
      loadAccounts()
    }
  }
)

function normalizePhone(raw: string): string {
  return raw.replace(/\D/g, '').replace(/^00/, '')
}

function addPhone() {
  const phone = normalizePhone(phoneInput.value)
  if (!phone) return
  if (phones.value.includes(phone)) {
    toast.warning(t('whatsmeowCreateGroup.duplicatePhone'))
    return
  }
  phones.value.push(phone)
  phoneInput.value = ''
}

function removePhone(phone: string) {
  phones.value = phones.value.filter(p => p !== phone)
}

function onPhoneKeydown(ev: KeyboardEvent) {
  if (ev.key === 'Enter' || ev.key === ',') {
    ev.preventDefault()
    addPhone()
  }
}

async function submit() {
  if (!accountId.value) {
    toast.error(t('whatsmeowCreateGroup.accountRequired'))
    return
  }
  if (!subject.value.trim()) {
    toast.error(t('whatsmeowCreateGroup.subjectRequired'))
    return
  }
  // Also catch anything still in the input that the user didn't "add".
  const pending = normalizePhone(phoneInput.value)
  if (pending && !phones.value.includes(pending)) phones.value.push(pending)

  if (phones.value.length === 0) {
    toast.error(t('whatsmeowCreateGroup.participantsRequired'))
    return
  }

  isCreating.value = true
  try {
    const resp = await whatsmeowGroupService.create(accountId.value, subject.value.trim(), phones.value)
    const data = (resp.data as any).data || resp.data
    const contactId = data?.contact_id as string | undefined
    toast.success(t('whatsmeowCreateGroup.createdOk'))
    emit('update:open', false)
    if (contactId) {
      emit('created', contactId)
      // Navigate to the fresh group chat so the admin lands on it.
      router.push({ name: 'chat', query: { contactId } }).catch(() => {})
    }
  } catch (e) {
    toast.error(getErrorMessage(e, t('whatsmeowCreateGroup.createFailed')))
  } finally {
    isCreating.value = false
  }
}
</script>

<template>
  <Dialog :open="props.open" @update:open="emit('update:open', $event)">
    <DialogContent class="max-w-md">
      <DialogHeader>
        <DialogTitle class="flex items-center gap-2">
          <Users class="h-5 w-5" />
          {{ $t('whatsmeowCreateGroup.title') }}
        </DialogTitle>
        <DialogDescription>{{ $t('whatsmeowCreateGroup.description') }}</DialogDescription>
      </DialogHeader>

      <div class="space-y-4 py-2">
        <!-- Account picker (whatsmeow only) -->
        <div class="space-y-1.5">
          <Label class="text-xs">{{ $t('whatsmeowCreateGroup.account') }}</Label>
          <Select v-model="accountId" :disabled="isLoadingAccounts || whatsmeowAccounts.length === 0">
            <SelectTrigger>
              <SelectValue :placeholder="$t('whatsmeowCreateGroup.accountPlaceholder')" />
            </SelectTrigger>
            <SelectContent>
              <SelectItem v-for="acc in whatsmeowAccounts" :key="acc.id" :value="acc.id">
                {{ acc.name }}
              </SelectItem>
            </SelectContent>
          </Select>
          <p v-if="!isLoadingAccounts && whatsmeowAccounts.length === 0" class="text-[11px] text-destructive">
            {{ $t('whatsmeowCreateGroup.noWhatsmeowAccounts') }}
          </p>
        </div>

        <!-- Subject -->
        <div class="space-y-1.5">
          <Label class="text-xs">{{ $t('whatsmeowCreateGroup.subject') }}</Label>
          <Input v-model="subject" :placeholder="$t('whatsmeowCreateGroup.subjectPlaceholder')" maxlength="100" />
        </div>

        <!-- Participants -->
        <div class="space-y-1.5">
          <Label class="text-xs">{{ $t('whatsmeowCreateGroup.participants') }}</Label>
          <div class="flex gap-2">
            <Input
              v-model="phoneInput"
              placeholder="+34 666 11 22 33"
              class="flex-1"
              @keydown="onPhoneKeydown"
            />
            <Button size="sm" variant="outline" :disabled="!phoneInput.trim()" @click="addPhone">
              <UserPlus class="h-4 w-4" />
            </Button>
          </div>
          <p class="text-[11px] text-muted-foreground">
            {{ $t('whatsmeowCreateGroup.participantsHint') }}
          </p>
          <div v-if="phones.length > 0" class="flex flex-wrap gap-1.5 mt-2">
            <Badge
              v-for="p in phones"
              :key="p"
              variant="secondary"
              class="flex items-center gap-1 font-mono text-xs"
            >
              +{{ p }}
              <button
                type="button"
                class="hover:text-destructive"
                :title="$t('common.remove', 'Remove')"
                @click="removePhone(p)"
              >
                <X class="h-3 w-3" />
              </button>
            </Badge>
          </div>
        </div>
      </div>

      <DialogFooter>
        <Button variant="outline" :disabled="isCreating" @click="emit('update:open', false)">
          {{ $t('common.cancel') }}
        </Button>
        <Button
          :disabled="isCreating || whatsmeowAccounts.length === 0 || !accountId || !subject.trim() || (phones.length === 0 && !phoneInput.trim())"
          @click="submit"
        >
          <Loader2 v-if="isCreating" class="h-4 w-4 mr-2 animate-spin" />
          {{ $t('whatsmeowCreateGroup.create') }}
        </Button>
      </DialogFooter>
    </DialogContent>
  </Dialog>
</template>
