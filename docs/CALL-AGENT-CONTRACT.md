# Call-Agent Contract — achatbot-go ↔ Rexa platform

What this agent exposes, what it sends back, and how the platform should drive
it. Written from the implementation, not from a spec — every field and status
below is what the code actually does.

**Version:** v1.1 · **Status:** phone (outbound + inbound) and call transfer
implemented · WebRTC **not** implemented
· **Agent side:** `pkg/rexa/` in `achatbot-go`

---

## 0. Read this first — differences from `packages/agent-contract`

The platform repo carries an `openapi.yaml` that is **out of date**. Where it
disagrees with this document, this document is what the agent implements.
Known divergences:

| `openapi.yaml` says | Reality |
|---|---|
| `POST /connect_webrtc` | The path is **`/connection_webrtc`** |
| `destination` | Split into **`to_number`** + **`from_number`** |
| Nested `voice: {voice_id, hello_message, …}` | Flat: **`voice`** is a bare string id; `system_prompt`, `hello_message`, `voicemail_message` are top-level |
| Nested `reporting: {webhook_url, …}` | Flat: **`webhook_url`** is top-level |
| `GET /voices`, `/voices/clones/*` | **Not implemented.** Nothing in the platform calls them either |

`packages/agent-contract/src/schemas.ts` is accurate and matches this document.
`docs/integrations/call-agent-hmac-spec.md` is accurate for HMAC and payloads.

---

## 1. Authentication

Every request in both directions carries three lowercase headers.

| Header | Value |
|---|---|
| `x-signature` | `HMAC-SHA256(secret, canonical)` as 64 lowercase hex chars |
| `x-timestamp` | Unix epoch in **milliseconds** |
| `x-nonce` | Fresh UUIDv4 per logical request |

The canonical string is:

```
{timestamp}\n{nonce}\n{body}
```

Single `\n` after the first two fields, none at the end, and `body` is the
**raw bytes on the wire**. Sign once and send those exact bytes — re-serialising
JSON between signing and sending reorders keys and breaks the signature. This is
the single most common way to break this contract.

**The secret is used as raw UTF-8, not hex-decoded**, even though it is 64 hex
characters. Node's `createHmac(alg, secret)` does not decode a string key, and
neither do we. Decoding it produces signatures that look valid and never verify.

Two secrets, one per direction:

| Direction | Secret | Agent env var |
|---|---|---|
| Platform → agent (dispatches) | platform signs, agent verifies | `REXA_OUTBOUND_HMAC_SECRET` |
| Agent → platform (callbacks) | agent signs, platform verifies | `REXA_INBOUND_HMAC_SECRET` |

**Verifier rules (both sides):** reject if any header is missing, if
`|now − timestamp| > 5 minutes`, if the nonce was seen in the last 10 minutes,
or if the signature does not match.

**Nonce reuse across retries is expected.** The platform already reuses one
nonce across its three dispatch attempts so the receiver can collapse them into
one logical dispatch; the agent does the same on callback retries. Treat a
repeated nonce inside the window as a duplicate of the same event, which is why
it is rejected rather than processed twice.

### Reference (Python)

```python
import hashlib, hmac, time, uuid

def sign(secret: str, body: bytes) -> dict[str, str]:
    ts = str(int(time.time() * 1000))
    nonce = str(uuid.uuid4())
    canonical = f"{ts}\n{nonce}\n".encode() + body
    sig = hmac.new(secret.encode(), canonical, hashlib.sha256).hexdigest()
    return {"x-signature": sig, "x-timestamp": ts, "x-nonce": nonce}
```

### Test vector

Verified against the platform's own `createHmac` implementation:

```
secret    0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
timestamp 1700000000000
nonce     11111111-2222-3333-4444-555555555555
body      {"session_id":"a","tenant_id":"b"}
signature 03ca38f072971348d005a459b144e8d85343c6747688e4e7fe923a98676099f8
```

If your implementation reproduces this, the wire format agrees. If it does not,
nothing else in this contract will work.

---

## 2. Endpoints the agent exposes

Base URL for integration testing:

```
http://38.65.239.47:4399
```

> **Plain HTTP, and that is a known gap, not an oversight.** Every
> `/connection` body carries the tenant's live Telnyx API key in
> `telecom_credentials.credentials`. HMAC authenticates the request; it does not
> encrypt it, so on this URL that credential crosses the network in the clear.
>
> Acceptable for integration testing against test credentials. **Do not put a
> production tenant's carrier key through it.** A TLS hostname replaces this
> before real traffic; treat the base URL as a config value, never a constant in
> code, so the swap costs you nothing.
>
> Tell us your egress IPs and we will restrict the port to them, which removes
> most of the exposure and requires no change on your side.

| Method | Path | Auth | Purpose |
|---|---|---|---|
| `GET` | `/health` | none | Liveness **and capacity**. See §5 |
| `POST` | `/connection` | HMAC | Place an outbound call |
| `POST` | `/incoming` | HMAC | Answer a ringing inbound leg |
| `POST` | `/connection_webrtc` | HMAC | **Not implemented** — returns 503 |
| `GET` | `/dashboard` | none | Operator capacity dashboard (HTML) |

Request bodies are capped at **128 KB**; larger returns `400 invalid_request`.

### Verify your integration in two calls

Before wiring anything up, confirm the two things that break silently.

**1. The agent is reachable and reporting capacity:**

```bash
curl -s http://38.65.239.47:4399/health
```

Expect `200` with `"status": true` and `"accepting": true`.

**2. Your signing matches ours.** Send a `/connection` with deliberately fake
Telnyx credentials:

- `401 unauthenticated` → the HMAC does not match. Work through §1 before
  anything else; nothing will function until this passes.
- `503 provider_unavailable` with a Telnyx `10009` in the message → **correct**.
  The signature verified, the body parsed, and the agent got as far as trying to
  place the call. Real credentials are all that is missing.
- `400 invalid_request` → signing is fine; a required field is absent. The
  message names it.

---

## 3. `POST /connection` — outbound call

### Request

```json
{
  "session_id": "01920000-0000-7000-8000-000000000001",
  "tenant_id":  "01920000-0000-7000-8000-000000000002",
  "to_number":   "+15557654321",
  "from_number": "+15551234567",
  "direction":   "outbound",

  "telecom_credentials": {
    "provider": "telnyx",
    "credentials": { "api_key": "KEY01...", "connection_id": "1234567890" }
  },

  "voice":             "leah",
  "language":          "en",
  "system_prompt":     "You are a friendly survey caller...",
  "hello_message":     "Hi, this is a quick call.",
  "voicemail_message": "Sorry we missed you — we'll try again.",

  "transfer_number": "+15559998888",
  "webhook_url": "https://platform.example.com/v1/_internal/webhooks/call-agent"
}
```

**Required:** `session_id`, `tenant_id`, `to_number`, `from_number`, `voice`,
`system_prompt`, `webhook_url`. A missing one returns `400 invalid_request` with
the field named in the message.

| Field | Notes |
|---|---|
| `telecom_credentials` | **Per-call, per-tenant.** The agent builds a Telnyx client from these for this call only. Only `provider: "telnyx"` is supported; anything else returns `412`. |
| `voice` | Bare string in your vocabulary (`"leah"`). Resolved against the local kokoro catalogue, then `REXA_VOICE_MAP`, then a bare integer speaker id, then the configured default. **An unknown voice does not fail the call** — it falls back and logs once. |
| `language` | ISO 639-1 two-letter (`"en"`), not BCP-47. |
| `voicemail_message` | Spoken after the beep when answering-machine detection reports a machine. Pre-rendered at dispatch, so it costs no GPU at call time. |
| `transfer_number` | Optional. When set, the model is given a `call_transfer` tool and will transfer on an explicit request for a human. Omit it and the tool is not registered at all, so the model cannot promise what it cannot do. See §6. |
| `webhook_url` | Where the agent POSTs the end-of-call report. |

### Response — 200

```json
{ "status": "accepted", "agent_session_id": "v3:abc123..." }
```

`agent_session_id` is the carrier call-control id, useful for cross-system grep.

### Errors

| Status | `error.code` | Meaning | What the platform should do |
|---|---|---|---|
| 400 | `invalid_request` | Missing/malformed field, or body > 128 KB | Fix the payload. Do not retry |
| 401 | `unauthenticated` | HMAC failed — bad signature, drift, or replay | Do not retry. The body deliberately does not say which |
| 412 | `provider_credentials_invalid` | Not Telnyx, or missing `api_key`/`connection_id` | **Fail the session immediately.** Retrying cannot help |
| 503 | `at_capacity` | Agent is full — see §5 | **Hold and retry.** The call is fine, the agent is busy |
| 503 | `provider_unavailable` | Dial failed at the carrier | Retryable |
| 500 | `internal_error` | Unexpected failure | Retryable |

Error envelope:

```json
{ "error": { "code": "at_capacity", "message": "agent at capacity: gpu cost 61.0 of 61, 61 of 200 calls in flight" } }
```

**The HTTP status always agrees with the code**, because the platform decides
retryability from the status before reading the body.

---

## 4. `POST /incoming` — inbound call

The platform has already accepted the leg at its edge and hands over the
carrier's call-control id. The agent answers it; there is no dial.

```json
{
  "CCID": "v3:carrier-call-control-id",
  "session_id": "…", "tenant_id": "…",
  "from_number": "+15551112222", "to_number": "+15553334444",
  "telecom_credentials": { "provider": "telnyx", "credentials": { … } },
  "voice": "leah", "language": "en",
  "system_prompt": "…", "hello_message": "Thanks for calling.",
  "transfer_number": null,
  "webhook_url": "https://platform.example.com/v1/_internal/webhooks/call-agent"
}
```

**Required:** `CCID`, `session_id`, `tenant_id`, `system_prompt`, `webhook_url`.

`CCID` capitalisation is **literal** — lowercase decodes to empty and returns
`400`.

**Inbound is never refused for capacity.** The leg is already ringing with a
human on it, so refusing costs a real answered call. It is still *counted*, so
inbound load reduces what outbound is allowed. Same response and error shapes as
`/connection`.

---

## 5. `GET /health` — liveness and capacity

Unauthenticated, and deliberately free of dependency checks — every number is an
in-process counter. A probe that fanned out to the LLM/ASR/TTS services would
drop the agent from rotation whenever one was merely slow.

```json
{
  "status": true,
  "accepting": true,
  "calls":    { "total": 47, "reserved": 12, "on_gpu": 29, "voicemail": 6 },
  "capacity": { "max_gpu_calls": 61, "max_total_calls": 200,
                "gpu_cost": 41.0, "human_weight": 1.0, "headroom": 0.33 },
  "measured": { "answer_rate": 0.28, "ring_ms_p95": 11200, "samples": 143 },
  "tiers":    { "llm": {"p95_ms": 1890, "state": "ok", "samples": 256},
                "asr": {"p95_ms": 310, "state": "ok", "samples": 256},
                "tts": {"p95_ms": 1180, "state": "degraded", "samples": 256} },
  "first_turn": { "p95_ms": 2100, "state": "ok", "samples": 32,
                  "blocked": false, "blocked_for_secs": 0, "trips": 0 },
  "sglang":   { "ok": true, "replicas": 2, "running_reqs": 14, "queued_reqs": 0,
                "cache_hit_rate": 0.83, "token_usage": 0.41, "age_secs": 3 },
  "totals":   { "calls": 1204, "voicemail": 380, "rejected": 4, "reaped": 0 }
}
```

### The two fields the platform must act on

**`status`** — liveness. `true` while the process is alive; `false` only while
draining. HTTP is 200 when true, 503 when false.

**`accepting`** — *stop sending work*. **This is the field to gate dispatch on.**

**The HTTP status stays 200 when the agent is merely full.** A full agent is
healthy, just busy. If it answered 503 at capacity, your load balancer would
mark the URL unhealthy and stop routing to it entirely, when all that was wanted
was backpressure.

`accepting` goes false when any of: draining · `gpu_cost >= max_gpu_calls` ·
`total >= max_total_calls` · any tier `saturated` · **`first_turn.blocked`**.

### `first_turn` — why `accepting` can go false with the ceiling nowhere near full

Expect to see `accepting: false` while `calls.on_gpu` reads 10 against a ceiling
of 61. That is not a bug, and it is the single most important thing to understand
about this endpoint.

`first_turn` measures one thing: the wait between the caller finishing their
first sentence and the first word of our reply. It is tracked apart from every
other turn because it is the only turn that pays a cold prefill, so it is where
KV-cache pressure appears first and by the largest margin. Measured at 60
concurrent calls with 3k-token prompts:

| | prompts share a campaign prefix | a different prompt per call |
|---|---|---|
| p95 across all turns | 1725 ms | 6252 ms |
| **p95 of turn 1** | **1853 ms** | **9903 ms** |

The pooled number is what a naive threshold would watch — and at 6252 ms it
would have let those calls through. So turn 1 gets its own gate, and that gate
can refuse traffic by itself.

The reasoning is worth stating plainly, because it is a deliberate trade: our
61-call ceiling was measured under one prompt size and one degree of prefix
sharing. A workload heavier than that invalidates the number, and continuing to
advertise it is not optimism, it is a false claim. **Serving 6 calls well beats
accepting 61 and serving all of them badly.**

What to do about it:

- **Treat it exactly like any other `accepting: false`** — hold dispatch, retry
  the health check. Nothing special is required on your side.
- **It is a duty cycle, not a latch.** The gate shuts for ~30 s, reopens, and
  re-measures on the next calls admitted. Under sustained overload that settles
  into admitting a trickle rather than stopping dead.
- **`trips` climbing while calls flow is a diagnosis, not noise.** It means the
  prompts being dispatched are not sharing prefixes. The two asks in §9 —
  per-contact block last, and batching per campaign — are what fix it at source;
  this gate is the safety net for when they are not honoured.

`sglang` is the LLM server's own cache and queue telemetry, polled in the
background. Informational only — nothing in it refuses traffic. A falling
`cache_hit_rate` points at prompt layout; a growing `queued_reqs` points at
volume. Ignore any reading with a large `age_secs`; it means polling has stopped
and the numbers are fossils.

### Call states

| State | Meaning | Costs GPU? |
|---|---|---|
| `reserved` | Dispatched, ringing, not yet answered | **Yes** — it will need a pipeline unless it turns out to be a machine |
| `on_gpu` | Live pipeline (VAD + ASR + TTS slots held) | Yes |
| `voicemail` | Machine answered; announcement plays from cache | **No** |

Reserving at dispatch rather than at pipeline start is essential: a call is
accepted while the phone is still ringing, and the pipeline does not exist until
someone answers — up to 30 seconds later. Counting only live pipelines reports
zero load throughout the ring period, so a caller dispatching against it can push
hundreds of calls before the first registers.

### Backpressure

`/connection` re-checks capacity on every dispatch, so a stale health cache
cannot overshoot: fire 200 dispatches off one cached reading and the ones past
the ceiling still return `503 at_capacity`. Treat `/health` as advisory and
`at_capacity` as authoritative.

---

## 6. Callbacks the agent sends

POSTed to the dispatch's `webhook_url`, signed with `REXA_INBOUND_HMAC_SECRET`.
Retried on 5xx and network errors at **1s, 5s, 30s, 2m, 12m**; 4xx is never
retried. All attempts in one ladder share a nonce, so they are one retried
delivery rather than five events.

### End-of-call report

No `type` field — that absence is the discriminator.

```json
{
  "session_id": "…", "tenant_id": "…",
  "call_status": "completed",
  "end_reason": "callee_hung_up",
  "started_at": "2026-08-09T08:39:50.123Z",
  "ended_at":   "2026-08-09T08:40:02.456Z",
  "duration_seconds": 12,
  "CCID": "v3:abc123",
  "messages": [
    { "role": "agent", "content": "Hi, this is a quick call.", "t": 0.0 },
    { "role": "user",  "content": "Hello?",                    "t": 1.4 }
  ]
}
```

**Emitted for every dispatched call**, including ones where no pipeline ever
ran — voicemail, no-answer and busy all report. The emitter hangs off the
carrier lifecycle, not pipeline teardown, precisely so those are not lost.

`call_status` is one of `completed` · `failed` · `no_answer` · `voicemail` ·
`busy`. `end_reason` is finer-grained and direction-aware: outbound human
hangups report `callee_hung_up`, inbound report `caller_hung_up`.

Outcome mapping:

| Situation | `call_status` | `end_reason` |
|---|---|---|
| Machine / silence / fax detected | `voicemail` | `voicemail` |
| Rang out, no answer | `no_answer` | `no_answer` |
| Busy or rejected | `busy` | `busy` |
| Human hung up (outbound) | `completed` | `callee_hung_up` |
| Agent hung up (max duration, voicemail done) | `completed` | `agent_hung_up` |
| Dial never succeeded | `failed` | `provider_failure` |

`messages` is the **full** transcript, not a window — captured turn by turn as
the conversation happens.

Timestamps are ISO 8601 UTC with a literal `Z` and millisecond precision.

Expected response: `200 {"ok": true, ...}`. The platform dedupes on
`session_id`, so a retry is safe.

### Transfer initiated

```json
{ "type": "transfer_initiated", "session_id": "…", "tenant_id": "…",
  "transfer_number": "+15559998888", "transferred_at": "2026-08-09T08:39:55.000Z" }
```

Sent when the model invokes `call_transfer` — **before** the carrier is asked,
deliberately, so the platform can warn the receiving human that a call is about
to arrive. That only helps ahead of time.

**The consequence: this event means "a transfer was attempted", not "a transfer
succeeded."** A transfer the carrier cannot complete still produces it. When
that happens the agent keeps the caller, tells them it could not connect, and
the call continues — so the eventual end-of-call report is an ordinary one. If
you need to distinguish, treat `transfer_initiated` followed by a normal
end-of-call report as a failed attempt.

Caller ID presented to the destination is the **contact's** number by default,
so the receiving human sees whose call it is. See §8 for the trade-off.

---

## 6a. Call transfer

Set `transfer_number` on the dispatch and the agent registers a `call_transfer`
tool for that call. The model calls it when the person explicitly asks for a
human; the tool is described so it will not fire for questions the model could
answer itself.

Sequence:

1. Model tells the caller it is connecting them, then invokes `call_transfer`.
2. Agent POSTs `transfer_initiated` (see §6).
3. Agent asks the carrier to transfer the leg.
4. **On success** the carrier owns the conversation, the agent stops speaking
   and releases its pipeline — the call stops counting against the GPU ceiling
   while still holding a channel.
5. **On failure** the caller is still on the line. The tool tells the model the
   transfer failed and the model apologises and carries on. `transfer_initiated`
   has already been sent by then.

Omitting `transfer_number` leaves the tool unregistered, and the model cannot
transfer. Keep this consistent with `appendTransferNote` on your side: the
prompt says whether transfer is available and the tool decides whether it
actually is, and a caller is misled whenever the two disagree.

---

## 7. Not implemented

### WebRTC rooms

**`POST /connection_webrtc`** returns `503 provider_unavailable`:

```json
{ "error": { "code": "provider_unavailable",
             "message": "WebRTC rooms are not yet available on this agent" } }
```

The planned design is a Daily room joined over SIP, with the carrier bridging
the leg into the existing media path. It fails loudly rather than returning a
room nobody has joined, because a browser sitting in an empty room hears silence
and is indistinguishable from a broken agent.

### Everything else

Also absent, all optional on the platform side: `functions[]` (tool calling),
`recording` config, `opt_out_detection`, `metadata` passthrough, sentiment
webhooks, `disposition_code`, `question_answers[]`, `recording_saved`. Unlike
transfer, none of these are advertised to the model, so omitting them is
invisible to callers.

---

## 8. Configuring the agent side

```bash
REXA_OUTBOUND_HMAC_SECRET=...   # required — platform signs dispatches with this
REXA_INBOUND_HMAC_SECRET=...    # required — agent signs callbacks with this
TELNYX_PUBLIC_URL=https://...   # required — where the CARRIER reaches us
REXA_VOICE_MAP=leah=3,marcus=16 # optional — platform voice id → kokoro speaker
```

Both secrets are required together. With only one, the agent would either
verify dispatches but never report, or report but accept unauthenticated
calls — so it refuses to enable the contract at all and logs why.

Transfer caller ID, in `config.yaml`:

```yaml
server:
  transfer_caller_id: contact   # or "tenant"
```

`contact` (default) presents the number of the person being transferred, so the
receiving human sees whose call it is. The trade-off is that a `from` which is
not a number on the carrier account gets low or no STIR/SHAKEN attestation, and
US carriers increasingly mark those "Spam Likely" or drop them — **suspect this
first if transfers connect unreliably**. `tenant` presents our own number
instead. The contact's name rides along as `from_display_name` either way.

Capacity, in `config.yaml`:

```yaml
server:
  max_gpu_calls: 61          # measured ceiling for live pipelines
  max_total_calls: 200       # absolute in-flight cap incl. zero-GPU calls
  human_answer_weight: 1.0   # or "auto"; see below
```

`human_answer_weight` is the expected GPU cost of one dispatched call. The rule
is **weight ≥ the real answer rate**: admission allows `max_gpu_calls / weight`
calls to ring at once, and whatever fraction answers becomes pipelines.

- **`1.0`** — the only value that *guarantees* `on_gpu` never exceeds
  `max_gpu_calls`. Use this until a real campaign has been measured.
- **`auto`** — tracks the measured answer rate × a 2× safety margin, floored at
  0.15. Starts at 1.0 until enough calls resolve.
- **a number** — fixed over-subscription once the rate is known and steady.

---

## 9. Dispatch guidance — two asks that are worth more than anything else

Both of these are cheap on the platform side and were measured on this stack.
Together they are worth more than every other optimisation available to either
team, and neither changes a word of any tenant's prompt.

The mechanism behind both: SGLang caches KV blocks by PREFIX (RadixAttention).
Two calls share work only up to the first byte where their prompts differ.
Everything after that point is re-computed per call, however identical it is.

### 9.1 Put the per-contact part LAST

A campaign's prompt is mostly identical across its calls — position details,
guardrails, the conversation steps. Only the contact's own details differ. But
if the contact block sits partway through, everything after it stops being
shareable, including the conversation steps, which measured **53% of the whole
prompt**.

Measured, 60 concurrent calls in one campaign, 3k-token prompt, 8 turns each,
paced at 12.4s — the ONLY difference is where the contact block sits:

| contact block | TTFT p95 | **first turn p95** | turn 8 p95 |
|---|---|---|---|
| partway through (current) | 5356ms | **7368ms** | 1542ms |
| at the very end | 2416ms | **2471ms** | 585ms |

**Three times faster to first word**, same content, same token count.

First-turn latency is the one that matters: it is the pause after the callee
says "hello", and 7 seconds of silence is where people conclude the line is
dead and hang up.

**The ask:** when rendering a dispatch, order the prompt as
`[campaign-invariant content][per-contact content]`. The platform already
knows which fields it substituted per contact, so this split is exact rather
than inferred.

Doing it at dispatch — rather than in the tenant-facing template — means API
users can keep sending prompts in whatever shape they like, and existing call
agents are unaffected.

### 9.2 Dispatch several calls per campaign, not one per campaign

Round-robin across campaigns for fair allocation is the worst case for prefix
sharing: every concurrent call carries a different prompt, so nothing is
shared and `--schedule-policy lpm` has nothing to match on.

Measured, 60 concurrent calls, 3k prompts, 8 turns, 12.4s pacing:

| | TTFT p95 | first turn p95 |
|---|---|---|
| all from one campaign | 1725ms | 1853ms |
| every call a different campaign | **6252ms** | **9903ms** |

Nearly **10 seconds to first word** in the interleaved case.

**The ask:** batch by campaign. If 50 campaigns are running and there is room
for 50 calls, send **5 calls each from 10 campaigns** rather than 1 call from
each of 50. Rotate which campaigns get a block on the next pass.

Every campaign still progresses continuously — the allocation guarantee is
preserved — but the fleet holds a handful of distinct prefixes instead of
fifty. Larger blocks are better; even 5 recovers most of it.

### 9.3 Keep prompts near 3k tokens

Also measured, and the reason the two asks above are framed at 3k:

| prompt size | first-token latency, 30 concurrent |
|---|---|
| 3k tokens | ~250ms |
| 5k tokens | ~250ms |
| 20k tokens | **8762ms** even with a perfectly shared prefix |

Under about 5k is comfortable. Past that it degrades sharply, and at 20k it is
unusable regardless of how well the prefix is shared. If a tenant prompt
arrives much larger than this, it is worth truncating or rejecting at the
platform edge rather than letting it reach a live call.

Note the agent's ceiling (`max_gpu_calls`) is measured at ~3k. A materially
larger prompt invalidates it, and the agent would keep accepting calls it can
no longer serve well.

---

## 10. Integration checklist

1. Reproduce the §1 test vector. Nothing else works until it passes.
2. Point dispatch at the agent's base URL. Verify `GET /health` returns 200 with
   `accepting: true`.
3. Gate dispatch on `accepting`, and handle `503 at_capacity` by holding the
   session rather than failing it.
4. Send one `/connection` with real per-tenant Telnyx credentials.
5. Confirm the end-of-call report arrives, verifies against
   `REXA_INBOUND_HMAC_SECRET`, and carries a full `messages` transcript.
6. Confirm a voicemail-answered call still produces a report with
   `call_status: "voicemail"`.
7. If using transfer: dispatch with `transfer_number` set, ask the agent for a
   human, and confirm `transfer_initiated` arrives and the leg connects. Note
   the event fires on *attempt*, not success.
8. Apply §9.1 and §9.2 before any volume test — first-turn latency measured
   3x worse without them, and they cost the agent side nothing.

Watch `/dashboard` during the first campaign and record `measured.answer_rate`
and `measured.ring_ms_p95` — those are the numbers that decide whether
`human_answer_weight` can safely move off 1.0.
