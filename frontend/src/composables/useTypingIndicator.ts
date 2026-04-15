import { ref } from 'vue'

// useTypingIndicator maintains a global, reactive map of contactId →
// typing-expiry-timestamp so chat views can render the three-dot bubble.
//
// WhatsApp's "typing" presence event does NOT carry an explicit "stopped"
// for every started — the client is expected to auto-hide the indicator
// after ~10 seconds if no fresh start arrives. We implement that via a
// per-contact expiry + periodic tick.
//
// The WebSocket service (src/services/websocket.ts) calls setTyping on
// every typing_indicator event it receives. Components that render the
// indicator call isTyping(contactId) inside a computed; reactivity
// updates automatically when the entry expires or is refreshed.

const TYPING_TTL_MS = 12_000 // a hair above WhatsApp's ~10s auto-clear

// Reactive map: contactId → earliest timestamp the indicator should hide.
const typingUntil = ref<Record<string, number>>({})

// Background tick forces reactivity re-evaluation when entries expire,
// without requiring components to poll. Started lazily on first use.
let tickHandle: ReturnType<typeof setInterval> | null = null
function ensureTickRunning() {
  if (tickHandle) return
  tickHandle = setInterval(() => {
    const now = Date.now()
    let changed = false
    const next: Record<string, number> = {}
    for (const [id, exp] of Object.entries(typingUntil.value)) {
      if (exp > now) {
        next[id] = exp
      } else {
        changed = true
      }
    }
    if (changed) typingUntil.value = next
  }, 1000)
}

/**
 * setTyping updates the typing state for a contact. Called from the
 * WebSocket message handler on typing_indicator events.
 */
export function setTyping(contactId: string, isTyping: boolean) {
  ensureTickRunning()
  const current = { ...typingUntil.value }
  if (isTyping) {
    current[contactId] = Date.now() + TYPING_TTL_MS
  } else {
    delete current[contactId]
  }
  typingUntil.value = current
}

/**
 * isTyping returns true if the given contact is currently typing (i.e.
 * the most recent typing_indicator event is still within its TTL).
 *
 * Intended to be called from a Vue `computed` — the underlying ref
 * triggers re-renders as entries change.
 */
export function isTyping(contactId: string | undefined | null): boolean {
  if (!contactId) return false
  const exp = typingUntil.value[contactId]
  return !!exp && exp > Date.now()
}

/**
 * useTypingIndicator exposes the reactive map for component scripts
 * that need to watch multiple contacts (e.g. the chat list sidebar).
 */
export function useTypingIndicator() {
  return { typingUntil, setTyping, isTyping }
}
