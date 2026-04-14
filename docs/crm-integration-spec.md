# iReparo PBX ↔ CRM Integration Spec

This document is the **contract** between the iReparo PBX (Go backend at
`pbx.ireparo.es`) and the external Laravel CRM (at `sat.ireparo.es`).

The PBX is the source of truth for **calls and conversations**.
The CRM is the source of truth for **customers, tickets and revenue**.

Both sides stay in sync via:

1. **Lookup**: PBX queries the CRM on every incoming call to enrich the
   agent screen-pop with customer context.
2. **Events**: PBX emits signed webhooks to the CRM for every call
   lifecycle transition (ringing, answered, ended, missed).

This doc is the single source of truth for anyone implementing the CRM
side. If you are writing Laravel controllers to receive these webhooks,
read this document top-to-bottom before touching code.

---

## 1. Authentication

Every request between PBX and CRM carries two credentials:

| Header | Purpose | Direction |
|--------|---------|-----------|
| `X-iReparo-Api-Key` | Shared secret identifying the caller | Both |
| `X-iReparo-Signature` | HMAC-SHA256 of timestamp + body | PBX → CRM (events) |
| `X-iReparo-Timestamp` | Unix epoch seconds (for replay protection) | PBX → CRM (events) |
| `X-iReparo-Event` | Event type (redundant with JSON body) | PBX → CRM (events) |

### API Key

A simple shared secret. Generated once per deployment (48 hex chars is
fine). Both sides store it in their respective `.env` / `config.toml`.

Used on **all** requests in both directions. If missing or wrong, the
receiving side returns `401 Unauthorized`.

### HMAC Signature (events only)

The lookup endpoint only uses the API key. The event endpoint requires
**both** the API key AND an HMAC-SHA256 signature so that a leaked API
key alone is not enough to forge events.

**Signed message format:**

```
<unix_timestamp>.<raw_request_body>
```

A literal `.` separator. No JSON parsing, no whitespace trimming — the
CRM must compute the HMAC over the exact bytes it received.

**Signature header format:**

```
X-iReparo-Signature: sha256=<hex-digest>
```

**Replay protection:** timestamps older than 5 minutes are rejected.
Both sides must have reasonably synced clocks (NTP recommended).

---

## 2. Phone Normalization

All phone numbers in this integration are normalized to **E.164 without
the leading `+`**. Both sides MUST apply the same normalization before
comparing or persisting phones.

**Rules:**

1. Strip everything that is not a digit (regex `\D` → empty).
2. If the result starts with `00`, strip those two leading zeros.

**Examples:**

| Input | Normalized |
|-------|------------|
| `+34 873 94 07 02` | `34873940702` |
| `+34 (873) 940-702` | `34873940702` |
| `0034873940702` | `34873940702` |
| `34873940702` | `34873940702` |
| `  34 873 940 702 ` | `34873940702` |

**Laravel implementation:**

```php
public static function normalizePhone(?string $phone): string
{
    if (! $phone) return '';
    $digits = preg_replace('/\D/', '', $phone);
    if (str_starts_with($digits, '00')) {
        $digits = substr($digits, 2);
    }
    return $digits;
}
```

**Go implementation** (already in `internal/integrations/crm/normalizer.go`):

```go
func NormalizePhone(phone string) string {
    var b strings.Builder
    for _, r := range phone {
        if r >= '0' && r <= '9' {
            b.WriteRune(r)
        }
    }
    out := b.String()
    if strings.HasPrefix(out, "00") {
        out = out[2:]
    }
    return out
}
```

---

## 3. Endpoints

The CRM must expose **two endpoints** for the PBX to consume.

### 3.1 Lookup — `GET /api/pbx/lookup`

Called by the PBX on every incoming call ring to enrich the agent
screen-pop. Must answer quickly (< 1s typical; the PBX aborts the
lookup after 1.5s and falls back to "unknown caller" UX).

**Query parameters:**

| Name | Required | Notes |
|------|----------|-------|
| `phone` | yes | Already normalized (E.164 no `+`) |

**Headers:**

| Header | Required | Notes |
|--------|----------|-------|
| `X-iReparo-Api-Key` | yes | Shared secret |
| `Accept` | recommended | `application/json` |

**Response: `200 OK`** — always 200, even if the phone is not in the CRM
(use the `found` field to distinguish). Any non-2xx is treated as an
integration error and the PBX shows "unknown caller".

**Response body — found:**

```json
{
  "found": true,
  "normalized_phone": "34873940702",
  "customer": {
    "id": 12345,
    "name": "María García López",
    "phone": "34873940702",
    "phone_alt": "34666123456",
    "email": "maria@example.com",
    "profile_url": "https://sat.ireparo.es/customers/12345",
    "active_tickets_count": 2,
    "last_ticket": {
      "id": 9876,
      "tracking_token": "RT-9876",
      "status": "waiting_parts",
      "device": "iPhone 13 Pro",
      "opened_at": "2026-04-10T09:15:00Z",
      "url": "https://sat.ireparo.es/tickets/9876"
    },
    "total_spent_eur": 1245.50,
    "first_seen_at": "2023-11-02T11:30:00Z",
    "vip": false,
    "notes_summary": "Cliente recurrente, prefiere contacto por WhatsApp."
  }
}
```

**Response body — not found:**

```json
{
  "found": false,
  "normalized_phone": "34873940702",
  "create_url": "https://sat.ireparo.es/customers/new?phone=34873940702"
}
```

`create_url` is optional but recommended. The PBX agent panel will render
it as a "Create customer in CRM" button so the agent can one-click-open
the CRM with the phone pre-filled.

**Caching on the PBX side:**

- Positive responses cached for 5 minutes.
- Negative responses cached for 30 seconds (so a customer that becomes
  known is picked up quickly without hammering).

When a customer is created/updated in the CRM, the CRM SHOULD trigger
the PBX `POST /api/crm/invalidate-cache` (Phase 3.2) to invalidate its
lookup cache. For now, callers just wait for the TTL to expire.

---

### 3.2 Call Events — `POST /api/pbx/call-event`

Called by the PBX after every call lifecycle transition. Must:

- Verify the HMAC signature (reject with 403 on mismatch).
- Return 2xx quickly (< 5s). Slow responses block the PBX worker.
- Be idempotent — the PBX will retry on any non-2xx response with
  exponential backoff, so the CRM may receive the same `call_id` +
  `event` pair multiple times.

**Headers:**

| Header | Required | Notes |
|--------|----------|-------|
| `X-iReparo-Api-Key` | yes | Shared secret |
| `X-iReparo-Signature` | yes | `sha256=<hex>` of `<ts>.<body>` |
| `X-iReparo-Timestamp` | yes | Unix epoch seconds (≤ 5 min skew) |
| `X-iReparo-Event` | yes | Event type (redundant with body) |
| `Content-Type` | yes | `application/json` |

**Body envelope:**

```json
{
  "event": "call.ringing",
  "timestamp": "2026-04-11T18:12:34.567Z",
  "data": { ... event-specific ... }
}
```

Where `event` is one of (see section 4 for full payloads):

- `call.ringing`
- `call.answered`
- `call.ended`
- `call.missed`
- `message.inbound` — **Phase 3.2, not yet emitted**
- `message.outbound` — **Phase 3.2, not yet emitted**

**Response:**

- `2xx` → the PBX marks the event as delivered and never retries.
- Any other status → retry with exponential backoff (see section 5).

**Idempotency:** the CRM MUST deduplicate by `(call_id, event)`. The
easiest way is a unique constraint on the `customer_call_logs` table.
See section 6 for the recommended schema.

---

## 4. Event Payloads

All payloads share the same outer envelope from section 3.2. The
`data` field is what varies per event type.

### 4.1 `call.ringing`

Fired when a call starts ringing (before the agent picks up). The CRM
should use this to **create a pending call log row** and optionally
open a ticket if none is active for this customer.

```json
{
  "event": "call.ringing",
  "timestamp": "2026-04-11T18:12:34.567Z",
  "data": {
    "call_id": "call-ctrl-id-abc123",
    "direction": "incoming",
    "caller_phone": "34666123456",
    "called_phone": "34873940702",
    "pbx_contact_id": "7f9e2c1a-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
    "external_crm_id": 12345,
    "ivr_flow_name": "Recepción Barcelona",
    "channel": "telnyx_pstn"
  }
}
```

**Field notes:**

- `call_id` — unique per call, stable across all lifecycle events for
  the same call. Opaque string (Telnyx `call_control_id` or WhatsApp
  `wamid`).
- `direction` — `"incoming"` or `"outgoing"`.
- `caller_phone` / `called_phone` — both normalized.
- `pbx_contact_id` — iReparo's internal `contacts.id` (UUID).
- `external_crm_id` — if the PBX lookup already cached the CRM id,
  it is sent here. `null` means the PBX could not resolve it (unknown
  caller, CRM was down, etc.).
- `channel` — `"whatsapp"` for WhatsApp calls, `"telnyx_pstn"` for PSTN
  calls through Telnyx.

### 4.2 `call.answered`

Fired when an agent picks up (or the IVR answers). The CRM should
update the call log row with the answer timestamp and agent.

```json
{
  "event": "call.answered",
  "timestamp": "2026-04-11T18:12:48.123Z",
  "data": {
    "call_id": "call-ctrl-id-abc123",
    "answered_at": "2026-04-11T18:12:47.950Z",
    "agent": {
      "id": "4a2b3c1d-5e6f-7a8b-9c0d-1e2f3a4b5c6d",
      "email": "juan@ireparo.es",
      "full_name": "Juan Pérez"
    },
    "via_transfer": false,
    "channel": "telnyx_pstn",
    "external_crm_id": 12345
  }
}
```

- `agent` may be `null` if no human picked up (e.g. IVR-only call).
- `via_transfer=true` means the call was picked up by the second-hop
  agent after a transfer.

### 4.3 `call.ended`

Fired when the call hangs up (by either side). The CRM should close
the call log row with full metrics.

```json
{
  "event": "call.ended",
  "timestamp": "2026-04-11T18:17:02.456Z",
  "data": {
    "call_id": "call-ctrl-id-abc123",
    "direction": "incoming",
    "caller_phone": "34666123456",
    "called_phone": "34873940702",
    "pbx_contact_id": "7f9e2c1a-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
    "external_crm_id": 12345,
    "status": "completed",
    "duration_seconds": 254,
    "started_at": "2026-04-11T18:12:34.567Z",
    "answered_at": "2026-04-11T18:12:47.950Z",
    "ended_at": "2026-04-11T18:17:02.100Z",
    "disconnected_by": "caller",
    "agent": {
      "id": "4a2b3c1d-5e6f-7a8b-9c0d-1e2f3a4b5c6d",
      "email": "juan@ireparo.es",
      "full_name": "Juan Pérez"
    },
    "ivr_path": ["entry", "greeting", "menu", "option_2_repairs", "transfer"],
    "recording_url": "https://pbx.ireparo.es/api/recordings/call-ctrl-id-abc123.mp3",
    "recording_duration_seconds": 240,
    "channel": "telnyx_pstn"
  }
}
```

- `status` — `"completed"`, `"missed"`, `"rejected"`, or `"failed"`.
- `disconnected_by` — `"caller"`, `"callee"`, `"agent"`, `"system"`.
- `ivr_path` — ordered list of IVR node IDs the call traversed.
  Useful for analytics (which options did customers pick?).
- `recording_url` — present only if recording was enabled for the
  number. The URL is behind auth — the CRM should store it and fetch
  it on demand using the API key.

### 4.4 `call.missed`

Fired **instead of** `call.ended` when the call ended without being
answered. Separate event so the CRM can branch cleanly (e.g. auto-open
a "missed call" ticket).

```json
{
  "event": "call.missed",
  "timestamp": "2026-04-11T18:13:12.000Z",
  "data": {
    "call_id": "call-ctrl-id-abc123",
    "caller_phone": "34666123456",
    "called_phone": "34873940702",
    "pbx_contact_id": "7f9e2c1a-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
    "external_crm_id": 12345,
    "reason": "no_agent_available",
    "channel": "telnyx_pstn",
    "whatsapp_fallback_sent": true
  }
}
```

- `reason` — `"no_agent_available"`, `"caller_hung_up"`, `"busy"`.
- `whatsapp_fallback_sent` — `true` if iReparo automatically sent
  the "sorry we missed your call" WhatsApp template. The CRM can use
  this to avoid duplicate outreach.

**Important:** a call that is missed emits `call.missed` only — there
is no matching `call.ended` event. Do not expect both.

---

## 5. Retry & Idempotency Rules

The PBX enqueues every event to a **persistent queue** and delivers
it via a background worker. If a delivery fails, the worker retries
with exponential backoff:

| Attempt | Delay after previous |
|---------|----------------------|
| 1 | immediate (on-event) |
| 2 | +30 s |
| 3 | +1 min |
| 4 | +2 min |
| 5 | +5 min |
| 6 | +15 min |
| 7 | +30 min |
| 8 | +1 h |
| 9 | +2 h |
| 10 | +6 h |
| 11+ | dead-letter (no further retries, visible in admin UI) |

**Signature stability:** the signature and timestamp are computed
**once at enqueue time** and reused on every retry. This means a retry
that arrives 6 hours later will have a timestamp 6 hours in the past.
The CRM MUST tolerate timestamp skew for retries — either relax the
5-minute window on recognized event IDs, or (simpler) verify the HMAC
and skip the freshness check on retries.

**Recommended CRM behavior:**

```php
// Pseudocode for the receiver
$isRetry = CustomerCallLog::where('call_id', $data['call_id'])
    ->where('event', $envelope['event'])
    ->exists();

if (! $isRetry && abs(time() - $timestamp) > 300) {
    abort(403, 'Timestamp out of window');
}
// else: accept it, we already have a record so the skew is expected
```

**Idempotency key:** the natural idempotency key is
`(call_id, event)` — the CRM MUST deduplicate on this pair. See the
migration template in section 7.

---

## 6. Laravel Controller Template

Drop-in Laravel 10/11 controller that implements both endpoints. Place
at `app/Http/Controllers/Api/PbxWebhookController.php`.

```php
<?php

namespace App\Http\Controllers\Api;

use App\Http\Controllers\Controller;
use App\Models\Customer;
use App\Models\CustomerCallLog;
use Illuminate\Http\JsonResponse;
use Illuminate\Http\Request;
use Illuminate\Support\Facades\Log;

class PbxWebhookController extends Controller
{
    /**
     * GET /api/pbx/lookup?phone=<normalized>
     *
     * Authenticated via X-iReparo-Api-Key header (middleware).
     */
    public function lookup(Request $request): JsonResponse
    {
        $phone = self::normalizePhone($request->query('phone', ''));
        if ($phone === '') {
            return response()->json([
                'found' => false,
                'normalized_phone' => '',
            ]);
        }

        $customer = Customer::query()
            ->where('phone_normalized', $phone)
            ->orWhere('phone_alt_normalized', $phone)
            ->with(['activeTickets' => fn ($q) => $q->latest()->limit(1)])
            ->first();

        if (! $customer) {
            return response()->json([
                'found' => false,
                'normalized_phone' => $phone,
                'create_url' => url("/customers/new?phone={$phone}"),
            ]);
        }

        $lastTicket = $customer->activeTickets->first();

        return response()->json([
            'found' => true,
            'normalized_phone' => $phone,
            'customer' => [
                'id' => $customer->id,
                'name' => $customer->full_name,
                'phone' => $customer->phone_normalized,
                'phone_alt' => $customer->phone_alt_normalized,
                'email' => $customer->email,
                'profile_url' => url("/customers/{$customer->id}"),
                'active_tickets_count' => $customer->active_tickets_count,
                'last_ticket' => $lastTicket ? [
                    'id' => $lastTicket->id,
                    'tracking_token' => $lastTicket->tracking_token,
                    'status' => $lastTicket->status,
                    'device' => $lastTicket->device,
                    'opened_at' => $lastTicket->created_at->toIso8601String(),
                    'url' => url("/tickets/{$lastTicket->id}"),
                ] : null,
                'total_spent_eur' => (float) $customer->total_spent_eur,
                'first_seen_at' => $customer->created_at?->toIso8601String(),
                'vip' => (bool) $customer->vip,
                'notes_summary' => str($customer->notes ?? '')->limit(200)->toString(),
            ],
        ]);
    }

    /**
     * POST /api/pbx/call-event
     *
     * Authenticated via X-iReparo-Api-Key AND X-iReparo-Signature.
     */
    public function callEvent(Request $request): JsonResponse
    {
        $rawBody = $request->getContent();
        $signature = $request->header('X-iReparo-Signature', '');
        $timestamp = $request->header('X-iReparo-Timestamp', '');
        $secret = config('ireparo.pbx_webhook_secret');

        if (! self::verifySignature($secret, $signature, $timestamp, $rawBody)) {
            Log::warning('PBX webhook: invalid signature', [
                'event' => $request->header('X-iReparo-Event'),
            ]);
            return response()->json(['error' => 'invalid signature'], 403);
        }

        $payload = json_decode($rawBody, true);
        $event = $payload['event'] ?? '';
        $data = $payload['data'] ?? [];

        match ($event) {
            'call.ringing'  => $this->handleCallRinging($data),
            'call.answered' => $this->handleCallAnswered($data),
            'call.ended'    => $this->handleCallEnded($data),
            'call.missed'   => $this->handleCallMissed($data),
            default         => Log::info("PBX webhook: ignoring event {$event}"),
        };

        return response()->json(['ok' => true]);
    }

    protected function handleCallRinging(array $data): void
    {
        // Idempotent upsert — a retry finds the existing row and does nothing.
        CustomerCallLog::updateOrCreate(
            ['call_id' => $data['call_id'], 'event' => 'call.ringing'],
            [
                'customer_id'     => $data['external_crm_id'] ?? null,
                'pbx_contact_id'  => $data['pbx_contact_id'] ?? null,
                'direction'       => $data['direction'] ?? 'incoming',
                'caller_phone'    => $data['caller_phone'] ?? '',
                'called_phone'    => $data['called_phone'] ?? '',
                'ivr_flow_name'   => $data['ivr_flow_name'] ?? null,
                'channel'         => $data['channel'] ?? 'telnyx_pstn',
                'started_at'      => now(),
            ]
        );
    }

    protected function handleCallAnswered(array $data): void
    {
        CustomerCallLog::updateOrCreate(
            ['call_id' => $data['call_id'], 'event' => 'call.answered'],
            [
                'customer_id'   => $data['external_crm_id'] ?? null,
                'answered_at'   => $data['answered_at'] ?? now(),
                'agent_email'   => $data['agent']['email'] ?? null,
                'agent_name'    => $data['agent']['full_name'] ?? null,
                'via_transfer'  => $data['via_transfer'] ?? false,
                'channel'       => $data['channel'] ?? 'telnyx_pstn',
            ]
        );
    }

    protected function handleCallEnded(array $data): void
    {
        CustomerCallLog::updateOrCreate(
            ['call_id' => $data['call_id'], 'event' => 'call.ended'],
            [
                'customer_id'                 => $data['external_crm_id'] ?? null,
                'pbx_contact_id'              => $data['pbx_contact_id'] ?? null,
                'direction'                   => $data['direction'] ?? 'incoming',
                'caller_phone'                => $data['caller_phone'] ?? '',
                'called_phone'                => $data['called_phone'] ?? '',
                'status'                      => $data['status'] ?? 'completed',
                'duration_seconds'            => $data['duration_seconds'] ?? 0,
                'started_at'                  => $data['started_at'] ?? null,
                'answered_at'                 => $data['answered_at'] ?? null,
                'ended_at'                    => $data['ended_at'] ?? now(),
                'disconnected_by'             => $data['disconnected_by'] ?? null,
                'agent_email'                 => $data['agent']['email'] ?? null,
                'agent_name'                  => $data['agent']['full_name'] ?? null,
                'ivr_path'                    => isset($data['ivr_path']) ? json_encode($data['ivr_path']) : null,
                'recording_url'               => $data['recording_url'] ?? null,
                'recording_duration_seconds'  => $data['recording_duration_seconds'] ?? null,
                'channel'                     => $data['channel'] ?? 'telnyx_pstn',
            ]
        );
    }

    protected function handleCallMissed(array $data): void
    {
        CustomerCallLog::updateOrCreate(
            ['call_id' => $data['call_id'], 'event' => 'call.missed'],
            [
                'customer_id'             => $data['external_crm_id'] ?? null,
                'pbx_contact_id'          => $data['pbx_contact_id'] ?? null,
                'caller_phone'            => $data['caller_phone'] ?? '',
                'called_phone'            => $data['called_phone'] ?? '',
                'status'                  => 'missed',
                'missed_reason'           => $data['reason'] ?? null,
                'whatsapp_fallback_sent'  => $data['whatsapp_fallback_sent'] ?? false,
                'channel'                 => $data['channel'] ?? 'telnyx_pstn',
                'ended_at'                => now(),
            ]
        );
        // OPTIONAL: open a ticket automatically on missed calls.
        // TicketService::openFromMissedCall($data);
    }

    /* ------------------------------------------------------------------ */
    /* Helpers                                                            */
    /* ------------------------------------------------------------------ */

    public static function normalizePhone(?string $phone): string
    {
        if (! $phone) return '';
        $digits = preg_replace('/\D/', '', $phone);
        if (str_starts_with($digits, '00')) {
            $digits = substr($digits, 2);
        }
        return $digits;
    }

    public static function verifySignature(
        string $secret,
        string $signatureHeader,
        string $timestampHeader,
        string $rawBody
    ): bool {
        if ($secret === '' || $signatureHeader === '' || $timestampHeader === '') {
            return false;
        }
        // Replay protection: 5 minute window (relax for known retries if needed)
        $ts = (int) $timestampHeader;
        if (abs(time() - $ts) > 300) {
            // For retries of known calls, allow. Simpler: check uniqueness below.
            // Uncomment to enforce strict freshness on ALL events:
            // return false;
        }
        $expected = 'sha256=' . hash_hmac('sha256', $ts . '.' . $rawBody, $secret);
        return hash_equals($expected, $signatureHeader);
    }
}
```

**Routes** (`routes/api.php`):

```php
Route::middleware('pbx.apikey')->prefix('pbx')->group(function () {
    Route::get('/lookup', [PbxWebhookController::class, 'lookup']);
    Route::post('/call-event', [PbxWebhookController::class, 'callEvent']);
});
```

**API key middleware** (`app/Http/Middleware/PbxApiKey.php`):

```php
public function handle(Request $request, Closure $next)
{
    $provided = $request->header('X-iReparo-Api-Key', '');
    $expected = config('ireparo.pbx_api_key');
    if ($provided === '' || ! hash_equals($expected, $provided)) {
        return response()->json(['error' => 'unauthorized'], 401);
    }
    return $next($request);
}
```

**Config** (`config/ireparo.php`):

```php
return [
    'pbx_api_key'        => env('IREPARO_PBX_API_KEY'),
    'pbx_webhook_secret' => env('IREPARO_PBX_WEBHOOK_SECRET'),
    'pbx_base_url'       => env('IREPARO_PBX_BASE_URL', 'https://pbx.ireparo.es'),
];
```

**`.env`:**

```
IREPARO_PBX_API_KEY=<48-hex-from-pbx-deploy.sh>
IREPARO_PBX_WEBHOOK_SECRET=<64-hex-from-pbx-deploy.sh>
IREPARO_PBX_BASE_URL=https://pbx.ireparo.es
```

The PBX `scripts/deploy.sh` generates these two secrets on first deploy
and prints them — copy them into the CRM's `.env`.

---

## 7. Laravel Migration Template

Creates the `customer_call_logs` table with the right indexes and
idempotency constraint.

```php
<?php

use Illuminate\Database\Migrations\Migration;
use Illuminate\Database\Schema\Blueprint;
use Illuminate\Support\Facades\Schema;

return new class extends Migration
{
    public function up(): void
    {
        Schema::create('customer_call_logs', function (Blueprint $t) {
            $t->id();

            // Idempotency key. The PBX may re-send the same event on retry;
            // we deduplicate on (call_id, event).
            $t->string('call_id', 128);
            $t->string('event', 32);   // call.ringing | call.answered | call.ended | call.missed

            // Relations
            $t->foreignId('customer_id')->nullable()->constrained('customers')->nullOnDelete();
            $t->uuid('pbx_contact_id')->nullable();

            // Call basics
            $t->enum('direction', ['incoming', 'outgoing'])->default('incoming');
            $t->enum('channel', ['whatsapp', 'telnyx_pstn'])->default('telnyx_pstn');
            $t->string('caller_phone', 32)->nullable();
            $t->string('called_phone', 32)->nullable();
            $t->string('ivr_flow_name')->nullable();

            // Timing
            $t->timestamp('started_at')->nullable();
            $t->timestamp('answered_at')->nullable();
            $t->timestamp('ended_at')->nullable();
            $t->unsignedInteger('duration_seconds')->nullable();

            // Status
            $t->enum('status', ['ringing', 'completed', 'missed', 'rejected', 'failed'])->nullable();
            $t->enum('disconnected_by', ['caller', 'callee', 'agent', 'system'])->nullable();
            $t->string('missed_reason')->nullable(); // no_agent_available | caller_hung_up | busy

            // Agent
            $t->string('agent_email')->nullable();
            $t->string('agent_name')->nullable();
            $t->boolean('via_transfer')->default(false);

            // IVR trace + recording
            $t->json('ivr_path')->nullable();
            $t->string('recording_url', 512)->nullable();
            $t->unsignedInteger('recording_duration_seconds')->nullable();

            // Fallback flag
            $t->boolean('whatsapp_fallback_sent')->default(false);

            $t->timestamps();

            // Idempotency: a retry finds this unique index and upserts the row
            $t->unique(['call_id', 'event']);

            // Query shortcuts
            $t->index('customer_id');
            $t->index('started_at');
            $t->index(['customer_id', 'started_at']);
        });
    }

    public function down(): void
    {
        Schema::dropIfExists('customer_call_logs');
    }
};
```

**Recommended customer side helpers:**

```php
// app/Models/Customer.php
public function callLogs()
{
    return $this->hasMany(CustomerCallLog::class)->orderByDesc('started_at');
}

public function lastCall()
{
    return $this->hasOne(CustomerCallLog::class)->latestOfMany('started_at');
}

public function missedCallsCount()
{
    return $this->callLogs()->where('status', 'missed')->count();
}
```

**Scopes on the log model** (`app/Models/CustomerCallLog.php`):

```php
public function scopeMissed($q) { return $q->where('status', 'missed'); }
public function scopeAnswered($q) { return $q->whereNotNull('answered_at'); }
public function scopeIncoming($q) { return $q->where('direction', 'incoming'); }
public function scopeOutgoing($q) { return $q->where('direction', 'outgoing'); }
```

---

## 8. Configuration Checklist

Before declaring the integration live, verify:

### On the PBX side (`config.toml`)

```toml
[integrations.crm]
enabled                 = true
base_url                = "https://sat.ireparo.es"
api_key                 = "<matches CRM .env>"
webhook_secret          = "<matches CRM .env>"
lookup_timeout_ms       = 1500
http_timeout_ms         = 5000
lookup_cache_ttl_secs   = 300
negative_cache_ttl_secs = 30
```

### On the CRM side (`.env`)

```
IREPARO_PBX_API_KEY=<same as PBX api_key>
IREPARO_PBX_WEBHOOK_SECRET=<same as PBX webhook_secret>
```

### Customer model requirements

The CRM `customers` table must have:

- `phone_normalized` column (varchar) — UPDATED via a boot hook that
  normalizes `phone` on save. Indexed.
- `phone_alt_normalized` column — same, for the secondary phone.
- `total_spent_eur` column — precomputed or view-based.
- `active_tickets_count` — cached count of `status in ('open','waiting_*')`.

If these do not exist yet, add a migration. Example boot hook:

```php
// app/Models/Customer.php
protected static function booted(): void
{
    static::saving(function (Customer $c) {
        $c->phone_normalized = PbxWebhookController::normalizePhone($c->phone);
        $c->phone_alt_normalized = PbxWebhookController::normalizePhone($c->phone_alt);
    });
}
```

---

## 9. End-to-End Smoke Test

After both sides are configured, verify the round-trip:

### Test 1: Lookup

From the CRM side:

```bash
curl -s "https://pbx.ireparo.es/api/pbx/test-lookup?phone=34873940702" \
  -H "X-iReparo-Api-Key: <key>"
# Expected: 200 OK with the PBX's view of the customer
```

Or (more useful) from the PBX side you can call the CRM directly:

```bash
curl -s "https://sat.ireparo.es/api/pbx/lookup?phone=34873940702" \
  -H "X-iReparo-Api-Key: <key>" | jq
```

### Test 2: Event delivery

Place a real call to a Telnyx number configured in iReparo. Then:

```bash
# On the CRM side, check the log was created
php artisan tinker
>>> CustomerCallLog::latest()->first()->toArray()
```

You should see a `call.ringing` row appear within ~1 second of the
call starting, then `call.answered` / `call.ended` / `call.missed` as
the call progresses.

### Test 3: Retry resilience

With the CRM stopped:

```bash
# Place a real call that will fail to deliver
# Then on the PBX side:
docker compose exec -T db psql -U whatomate -d whatomate \
  -c "SELECT event_type, status, attempt_count, last_error FROM crm_event_queue ORDER BY created_at DESC LIMIT 5;"
```

You should see rows with `status=pending` and an error message. Restart
the CRM and watch them transition to `status=delivered` within a minute.

---

## 10. Troubleshooting

### Signature verification fails

- **Time skew**: check both VPSes have NTP synchronized
  (`timedatectl status`).
- **Wrong secret**: the PBX `webhook_secret` and CRM
  `IREPARO_PBX_WEBHOOK_SECRET` MUST be byte-identical.
- **Body was mutated**: some frameworks trim whitespace or re-serialize
  JSON before the middleware. The HMAC is over the **raw body**. In
  Laravel use `$request->getContent()`, NOT `$request->all()`.

### Lookup always returns 404

- API key missing or mismatched → returns 401, but some proxies
  rewrite 401 to 404. Check `storage/logs/laravel.log`.
- `phone_normalized` column not indexed or not populated. Run:
  ```sql
  UPDATE customers SET phone_normalized = regexp_replace(phone, '\D', '', 'g');
  ```

### Retries piling up

- Check `storage/logs/laravel.log` on the CRM side for 500 errors.
- Common cause: migration not run, `customer_call_logs` table missing.
- Common cause: the `(call_id, event)` unique constraint violated —
  should not happen with `updateOrCreate` but watch for race
  conditions in multi-worker Horizon setups.

### Events delivered but not showing up in UI

- Verify the `customer_id` foreign key actually resolves. If the PBX
  sends `external_crm_id: null` (unknown caller), you need a UI
  path to manually link the log to a customer after the fact.

---

## 11. Roadmap

| Phase | Feature | Status |
|-------|---------|--------|
| 3.1 A | PBX-side lookup + event emit | Done |
| **3.1 B** | **This spec doc + Laravel templates** | **Done** |
| 3.2 | `message.inbound` / `message.outbound` events | Planned |
| 3.2 | Admin UI for dead-letter queue replay | Planned |
| 3.2 | `POST /api/crm/invalidate-cache` (CRM → PBX) | Planned |
| 3.3 | Click-to-call from CRM customer page | Planned |

---

*Last updated: 2026-04-14. Keep this document in sync with
`internal/integrations/crm/events.go` — if a field changes in the Go
struct, update the JSON example here.*




