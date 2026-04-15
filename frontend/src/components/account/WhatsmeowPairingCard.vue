<script setup lang="ts">
import { ref, computed, onMounted, onBeforeUnmount, watch } from 'vue'
import { useI18n } from 'vue-i18n'
import { toast } from 'vue-sonner'
// qrcode is a lightweight pure-JS QR renderer. Added to package.json in
// this commit — run `npm install` if the import errors.
import QRCode from 'qrcode'
import {
  whatsmeowService,
  type WhatsmeowStatus,
  type WhatsmeowState
} from '@/services/api'
import { useAuthStore } from '@/stores/auth'
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from '@/components/ui/card'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import {
  Dialog,
  DialogContent,
  DialogDescription,
  DialogHeader,
  DialogTitle,
  DialogFooter
} from '@/components/ui/dialog'
import {
  AlertTriangle,
  QrCode,
  CheckCircle2,
  XCircle,
  Loader2,
  PowerOff,
  LogOut,
  RefreshCw
} from 'lucide-vue-next'

// WhatsmeowPairingCard
//
// Self-contained card that drives the lifecycle of one WhatsAppAccount
// whose provider is "whatsmeow". Status polling, connect button, QR
// modal, and disconnect / logout — all in one place so it can be
// slotted into the existing AccountDetailView without touching it.
//
// Usage (in AccountDetailView.vue, after loading the account):
//
//   <WhatsmeowPairingCard
//     v-if="account.provider === 'whatsmeow'"
//     :account-id="account.id"
//   />

interface Props {
  accountId: string
}
const props = defineProps<Props>()

const { t } = useI18n()
const authStore = useAuthStore()

// --- Status polling ------------------------------------------------------

const status = ref<WhatsmeowStatus>({ state: 'initialized', paired: false })
const isBusy = ref(false)
let pollHandle: ReturnType<typeof setInterval> | null = null

async function refreshStatus() {
  try {
    const resp = await whatsmeowService.status(props.accountId)
    const data = (resp.data as any).data || resp.data
    status.value = data as WhatsmeowStatus
  } catch (e) {
    // non-fatal; UI shows whatever the last known state was
  }
}

onMounted(() => {
  refreshStatus()
  // Poll every 5s so the card reflects upstream state without needing
  // a WebSocket when idle. The QR modal subscribes to its own WS while
  // open.
  pollHandle = setInterval(refreshStatus, 5000)
})
onBeforeUnmount(() => {
  if (pollHandle) clearInterval(pollHandle)
  stopQRStream()
})

// --- Connect / Disconnect / Logout --------------------------------------

async function connect() {
  isBusy.value = true
  try {
    const resp = await whatsmeowService.connect(props.accountId)
    const data = ((resp.data as any).data || resp.data) as {
      needs_qr: boolean
      state: WhatsmeowState
    }
    if (data.needs_qr) {
      openQRModal()
    } else {
      toast.success(t('whatsmeow.reconnectOk'))
    }
    await refreshStatus()
  } catch (e: any) {
    toast.error(e?.response?.data?.message || t('whatsmeow.connectFailed'))
  } finally {
    isBusy.value = false
  }
}

async function disconnect() {
  isBusy.value = true
  try {
    await whatsmeowService.disconnect(props.accountId)
    toast.success(t('whatsmeow.disconnectedOk'))
    await refreshStatus()
  } catch (e: any) {
    toast.error(e?.response?.data?.message || t('whatsmeow.disconnectFailed'))
  } finally {
    isBusy.value = false
  }
}

async function logout() {
  if (!confirm(t('whatsmeow.logoutConfirm'))) return
  isBusy.value = true
  try {
    await whatsmeowService.logout(props.accountId)
    toast.success(t('whatsmeow.logoutOk'))
    await refreshStatus()
  } catch (e: any) {
    toast.error(e?.response?.data?.message || t('whatsmeow.logoutFailed'))
  } finally {
    isBusy.value = false
  }
}

// --- QR modal + WebSocket ------------------------------------------------

const qrModalOpen = ref(false)
const qrDataURL = ref<string>('')
const wsState = ref<WhatsmeowState>('connecting')
const wsErrorMsg = ref<string>('')
let socket: WebSocket | null = null

async function openQRModal() {
  qrModalOpen.value = true
  qrDataURL.value = ''
  wsState.value = 'connecting'
  wsErrorMsg.value = ''
  startQRStream()
}

function closeQRModal() {
  qrModalOpen.value = false
  stopQRStream()
}

function startQRStream() {
  stopQRStream()
  const url = whatsmeowService.qrWebSocketURL(props.accountId)
  const token = authStore.accessToken
  if (!token) {
    wsErrorMsg.value = t('whatsmeow.authMissing')
    return
  }
  socket = new WebSocket(url)

  socket.addEventListener('open', () => {
    // First message: authenticate with the same token the REST API uses.
    socket?.send(JSON.stringify({ type: 'auth', payload: { token } }))
  })

  socket.addEventListener('message', async ev => {
    let msg: { type: string; payload: string }
    try {
      msg = JSON.parse(ev.data)
    } catch {
      return
    }
    switch (msg.type) {
      case 'qr':
        // Render the raw QR string to a PNG dataURL using the qrcode lib.
        // dark/light contrast tuned for both the panel light + dark
        // themes (muted text-on-card palette).
        try {
          qrDataURL.value = await QRCode.toDataURL(msg.payload, {
            width: 280,
            margin: 1,
            color: { dark: '#111827', light: '#ffffff' }
          })
        } catch {
          wsErrorMsg.value = 'QR render failed'
        }
        break
      case 'state':
        wsState.value = msg.payload as WhatsmeowState
        if (wsState.value === 'logged_in') {
          toast.success(t('whatsmeow.pairedOk'))
          closeQRModal()
          refreshStatus()
        }
        break
      case 'error':
        wsErrorMsg.value = msg.payload
        break
    }
  })

  socket.addEventListener('close', () => {
    socket = null
  })
  socket.addEventListener('error', () => {
    wsErrorMsg.value = t('whatsmeow.wsError')
  })
}

function stopQRStream() {
  if (socket) {
    try {
      socket.close()
    } catch {
      /* noop */
    }
    socket = null
  }
}

// Re-start WS on account change (e.g. if the user navigates between accounts)
watch(() => props.accountId, () => {
  stopQRStream()
  qrModalOpen.value = false
  refreshStatus()
})

// --- UI helpers ---------------------------------------------------------

const stateVariant = computed<'default' | 'secondary' | 'destructive' | 'outline'>(() => {
  switch (status.value.state) {
    case 'logged_in':
      return 'default'
    case 'connecting':
    case 'waiting_qr':
      return 'secondary'
    case 'error':
    case 'logged_out':
      return 'destructive'
    default:
      return 'outline'
  }
})
</script>

<template>
  <Card>
    <CardHeader>
      <CardTitle class="flex items-center gap-2">
        <QrCode class="h-5 w-5" />
        {{ $t('whatsmeow.title') }}
      </CardTitle>
      <CardDescription>{{ $t('whatsmeow.description') }}</CardDescription>
    </CardHeader>

    <CardContent class="space-y-4">
      <!-- Disclaimer — mandatory, not collapsible. Uses destructive
           variant so operators cannot miss it. -->
      <div class="rounded-md border border-destructive/40 bg-destructive/5 p-3 flex gap-2 items-start">
        <AlertTriangle class="h-5 w-5 text-destructive flex-shrink-0 mt-0.5" />
        <div class="text-sm text-destructive">
          <p class="font-semibold mb-1">{{ $t('whatsmeow.disclaimerTitle') }}</p>
          <p>{{ $t('whatsmeow.disclaimerBody') }}</p>
        </div>
      </div>

      <!-- Status row -->
      <div class="flex items-center gap-3 text-sm">
        <span class="text-muted-foreground">{{ $t('whatsmeow.status') }}:</span>
        <Badge :variant="stateVariant" class="flex items-center gap-1 w-fit">
          <CheckCircle2 v-if="status.state === 'logged_in'" class="h-3 w-3" />
          <Loader2 v-else-if="status.state === 'connecting' || status.state === 'waiting_qr'" class="h-3 w-3 animate-spin" />
          <XCircle v-else-if="status.state === 'error' || status.state === 'logged_out'" class="h-3 w-3" />
          {{ $t('whatsmeow.states.' + status.state) }}
        </Badge>
        <span v-if="status.jid" class="font-mono text-xs text-muted-foreground ml-auto">
          {{ status.jid }}
        </span>
      </div>

      <!-- Error (if any) -->
      <div v-if="status.last_error" class="text-xs text-destructive">
        <strong>{{ $t('common.lastError') || 'Last error' }}:</strong>
        {{ status.last_error }}
      </div>

      <!-- Action buttons -->
      <div class="flex gap-2 flex-wrap">
        <Button
          v-if="!status.paired && status.state !== 'logged_in'"
          variant="default"
          size="sm"
          :disabled="isBusy"
          @click="connect"
        >
          <Loader2 v-if="isBusy" class="h-4 w-4 mr-2 animate-spin" />
          <QrCode v-else class="h-4 w-4 mr-2" />
          {{ $t('whatsmeow.pair') }}
        </Button>

        <Button
          v-if="status.paired && status.state !== 'logged_in'"
          variant="default"
          size="sm"
          :disabled="isBusy"
          @click="connect"
        >
          <Loader2 v-if="isBusy" class="h-4 w-4 mr-2 animate-spin" />
          <RefreshCw v-else class="h-4 w-4 mr-2" />
          {{ $t('whatsmeow.reconnect') }}
        </Button>

        <Button
          v-if="status.state === 'logged_in'"
          variant="outline"
          size="sm"
          :disabled="isBusy"
          @click="disconnect"
        >
          <PowerOff class="h-4 w-4 mr-2" />
          {{ $t('whatsmeow.disconnect') }}
        </Button>

        <Button
          v-if="status.paired"
          variant="outline"
          size="sm"
          class="text-destructive"
          :disabled="isBusy"
          @click="logout"
        >
          <LogOut class="h-4 w-4 mr-2" />
          {{ $t('whatsmeow.logout') }}
        </Button>
      </div>
    </CardContent>

    <!-- QR Modal — shown during pairing flow -->
    <Dialog v-model:open="qrModalOpen">
      <DialogContent class="max-w-sm">
        <DialogHeader>
          <DialogTitle class="flex items-center gap-2">
            <QrCode class="h-5 w-5" />
            {{ $t('whatsmeow.scanQRTitle') }}
          </DialogTitle>
          <DialogDescription>{{ $t('whatsmeow.scanQRDesc') }}</DialogDescription>
        </DialogHeader>

        <div class="flex flex-col items-center gap-3 py-4">
          <div v-if="qrDataURL" class="bg-white p-3 rounded-md">
            <img :src="qrDataURL" :alt="$t('whatsmeow.scanQRAlt')" class="w-[280px] h-[280px]" />
          </div>
          <div v-else-if="wsErrorMsg" class="text-sm text-destructive text-center">
            {{ wsErrorMsg }}
          </div>
          <div v-else class="flex items-center gap-2 text-muted-foreground text-sm">
            <Loader2 class="h-4 w-4 animate-spin" />
            <span>{{ $t('whatsmeow.waitingQR') }}</span>
          </div>

          <p class="text-xs text-muted-foreground text-center max-w-[280px]">
            {{ $t('whatsmeow.scanQRHint') }}
          </p>

          <Badge :variant="stateVariant" class="w-fit">
            {{ $t('whatsmeow.states.' + wsState) }}
          </Badge>
        </div>

        <DialogFooter>
          <Button variant="outline" @click="closeQRModal">{{ $t('common.cancel') }}</Button>
        </DialogFooter>
      </DialogContent>
    </Dialog>
  </Card>
</template>
