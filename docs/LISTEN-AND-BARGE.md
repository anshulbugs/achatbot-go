# Listen and Barge — dialer integration

Two operator actions, one endpoint. **Join = listen silently. Barge = take the
call off the agent.**

Everything on the agent side is built and deployed. This is the dialer's half.

---

## What the agent does today

`GET /join-call?uuid=<call_uuid>&mode=listen|barge`

Returns the Daily room to open, and starts bridging the phone leg into it.

```json
{ "room_url": "https://jobtalk.daily.co/abc123",
  "daily_room_url": "https://jobtalk.daily.co/abc123",
  "token": "…", "daily_token": "…" }
```

| `mode` | SIP leg joins as | Agent | Caller hears the operator |
|---|---|---|---|
| `listen` | Telnyx `supervisor_role: monitor` | **stays**, keeps serving the caller | no — *"nobody can hear supervisor call, but supervisor can hear everything"* |
| `barge` (default) | ordinary participant | **leaves** the call | yes |

A `uuid` this agent does not own is proxied to `server.join_call_fallback_url`
and its answer returned unchanged — so one URL fronts both call agents and the
primary fleet's Join keeps working.

---

## Change 1 — send the mode  *(required for listen)*

`apps/api/app/routers/calls.py`, in `_bridge_daily_room`:

```python
url = f"{settings.join_call_url}?uuid={c.telnyx_call_control_id}&mode={mode}"
```

Pass `mode="listen"` from `join_call`, `mode="barge"` from `barge_call`. Without
it every action is a takeover, which is today's behaviour.

> These two endpoints are currently byte-identical in effect — both set
> `AGENT_JOINED`, both call `_bridge_daily_room`, both put the operator in with
> their mic live. Nothing else distinguishes them, which is why the agent cannot
> infer the difference.

For listen, the operator's mic should also start muted in the browser
(`DailyAudioCall`, `audioSource: false`), and Barge is the button that unmutes.

---

## Change 2 — point `JOIN_CALL_URL` at this agent  *(required)*

```
JOIN_CALL_URL=http://38.65.239.47:4399/join-call
```

Safe for the primary fleet: calls this agent doesn't recognise are proxied to
`join_call_fallback_url`, already configured as the legacy sidecar.

---

## Change 3 — stop discarding the join response  *(latency)*

`apps/web/src/hooks/useActiveCalls.ts:184`

```ts
onSuccess: (_data, callId) => {          // ← response thrown away
  qc.invalidateQueries({ queryKey: ACTIVE_CALLS_QUERY_KEY });
}
```

The `POST /join` response already contains `daily_room_url`. Discarding it and
invalidating forces a second round trip to learn a value you were just handed,
before `<DailyAudioCall>` can even mount. Use `_data.daily_room_url` directly.

---

## Change 4 — turn off pre-publishing  *(NOT for latency — measured)*

```yaml
server:
  live_room_prepublish: false
```

**This does not make barge faster.** It was listed here as a latency change; it
isn't. Room creation plus the meeting token was measured at **0.80s** (five runs,
0.48–0.63s and 0.28–0.32s), and that 0.80s is the same whether it runs before the
dial or at the barge. Turning pre-publishing off only *moves* it:

| | time-to-ring | barge |
|---|---|---|
| pre-publishing on (today) | +0.8s | 6.4s |
| pre-publishing off | +0s | **7.2s** |

The real reasons to do it are room hygiene and API volume: 175 rooms were created
in one day and 24 were ever opened, so 86% were created and deleted unused. Those
cost no money — Daily bills participant-minutes and an empty room has none — but
they are 151 pointless API calls.

Do this **last**. With `JOIN_CALL_URL` unset or wrong, `false` means Join finds
no room at all, from either source.

---

## Where the time goes

Measured on a live barge:

```
listener joined (Daily webhook)        instant
conference create                       208ms
SIP dial                                268ms
Daily answers the SIP invite           4 540ms   ← 94%
```

Daily doesn't register a room's SIP endpoint until a session exists, and the
operator's join *is* what starts the session — so registration was serialised in
front of our INVITE.

**Fixed by `live_room_prewarm`.** A silent WebRTC participant joins the room when
the call is *answered*, which starts the session then. Registration finishes
1.9–2.2s later — during the greeting — so by the time anyone barges the endpoint
has been ready for the whole conversation, and that 4 540ms is gone.

A SIP leg cannot do this itself: the endpoint it would dial isn't listening until
a session exists, so the first participant must be WebRTC.

Two things this cost, both worth knowing:

- **One Daily participant for the length of every answered call with a room**,
  barged or not. A billing question, not a capacity one — see
  [RESOURCES.md](RESOURCES.md). The process is 34 MB and uses no GPU, so the
  61-call ceiling is unaffected.
- **It looked exactly like a barge at first.** Every listener check is "a
  participant appeared", so the pre-joiner was read as an operator one second
  after answer: the agent hushed mid-greeting and left the call. It now joins as
  `__prewarm__` and both the webhook and the presence sweep skip that name. Any
  future participant we add to these rooms needs the same treatment.

**Already fixed:** the leg now joins the conference at **ringback** rather than
at answer (`conference_config.early_media`), so the operator hears the call while
Daily is still finishing setup.

**Left, and on the dialer's side:** click → `POST /join` → *(extra round trip,
Change 3)* → React mounts → WebRTC join → `participant.joined`. Changes 2 and 3
remove the avoidable parts; the WebRTC join itself, 1–3s, is irreducible.

Change 2 also makes this measurable for the first time — `join-call` in the
agent log is the click, `listener joined room` is the join, and the gap between
them is the dialer's share.

---

## Verify

Agent log, per barge:

```
rexa: session=… join-call — operator is opening room …
rexa: session=… live room bridged in 0.9s as listen (monitor, agent stays) …
rexa: session=… live room bridged in 0.9s as barge (agent leaves) …
rexa: call=… — agent has left the call (operator barged in)
```

- **Listen** — operator hears both sides, caller hears only the agent, agent
  keeps talking.
- **Barge** — operator is audible, and `agent has left the call` follows within
  ~1.5s.
