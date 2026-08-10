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

## Change 4 — turn off pre-publishing  *(latency, agent side, after 1–3)*

`config.yaml` on the agent:

```yaml
server:
  live_room_prepublish: false
```

Why it matters: while the agent publishes the room up front, `_bridge_daily_room`
takes its "already have a `daily_room_url`" fast path and **never calls
`/join-call` at all** — so the agent only learns about the operator from Daily's
`participant.joined` webhook, after the browser has finished joining.

Do this **last**. With `JOIN_CALL_URL` unset or wrong, `false` means Join finds
no room.

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
operator's join *is* what starts the session — so registration is serialised in
front of our INVITE. Not tunable on either side.

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
