# Using the agent's LLM from Rexa

The voice agent exposes the model it runs calls on, so Rexa can use it for
anything — summaries, extraction, classification, drafting, scoring — without
us shipping a new endpoint each time.

It is **OpenAI-compatible**. Point any OpenAI SDK at the agent's `/v1` and it
works.

## Connect

| | |
|---|---|
| Base URL | `https://<agent-host>/v1` |
| Auth | `Authorization: Bearer <REXA_LLM_API_KEY>` |
| Model | `google/gemma-4-E4B-it` (or omit `model` — it fills in) |
| Context window | 8192 tokens, prompt + reply combined |
| Max reply | 2048 tokens, capped server-side whatever you ask for |

```python
from openai import OpenAI

client = OpenAI(base_url="https://<agent-host>/v1", api_key=REXA_LLM_API_KEY)

resp = client.chat.completions.create(
    model="google/gemma-4-E4B-it",
    messages=[
        {"role": "system", "content": "Reply with JSON only."},
        {"role": "user", "content": f"Summarise this call:\n{transcript}"},
    ],
    max_tokens=300,
    temperature=0,        # use 0 for anything you will compare or store
)
print(resp.choices[0].message.content)
```

`curl` equivalent:

```bash
curl https://<agent-host>/v1/chat/completions \
  -H "Authorization: Bearer $REXA_LLM_API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"hello"}],"max_tokens":100}'
```

## The one behaviour that will surprise you: it may wait

**This model is shared with live phone calls, and calls win.** Before running
your request the agent checks whether the box is busy; if it is, your request
waits for a lull — up to **60 seconds** — and only then runs regardless.

So a request that normally takes 200 ms can take a minute under call load. That
is by design, not a fault. Two response headers tell you what happened:

| header | meaning |
|---|---|
| `X-Agent-Waited-Ms` | how long it waited for a quiet box |
| `X-Agent-Deferred: true` | it waited the full 60 s, never got a lull, and ran anyway |

**Set your client timeout to at least 90 seconds**, and do not put this on a
path where a user is watching a spinner. It is built for background work.

If you start seeing `X-Agent-Deferred: true` regularly, tell us — that is the
signal the model needs its own capacity rather than sharing with calls.

## Limits, and why

- **No streaming.** `"stream": true` returns 400. A stream holds a slot for the
  whole generation, which would break the fairness described above.
- **`max_tokens` capped at 2048** regardless of the request. Generation time is
  what holds a slot.
- **Body capped**; keep a single request under a few hundred KB.
- **At most 2 requests run at once** across all of Rexa. More than that queue.
  Batch work is fine — just expect it to take as long as it takes rather than
  fanning out.
- **8192-token context.** Prompt *plus* reply. A long transcript plus a 2048
  reply will overflow; trim, or summarise in chunks. Overflow returns the
  model's own error, not a generic 500, so read the response body.

## The key

`REXA_LLM_API_KEY` is a **64-character hex string**, supplied separately —
see the note that came with this document, or ask Anshul.

It is a bearer token, not one of the HMAC secrets. Deliberately separate: it
goes to more places than dispatch signing does, so it can be rotated without
touching call dispatch. Treat it as a production credential — it grants use of
a GPU that live calls depend on. Server-side only; never ship it to a browser
or a mobile client.

To rotate: we regenerate it on the agent and hand you the new one. Old key stops
working at that moment, so coordinate.

## The agent host, and a caveat

Today the agent is reachable on a **Cloudflare quick tunnel**, and that hostname
**changes every time the agent restarts**. Fine for testing, wrong for a
configured integration.

Before you wire this into anything that matters, ask us for the stable hostname
— we will move it onto a named Cloudflare tunnel with a fixed name. Until then,
expect the URL to change and do not hard-code it.

Do **not** use `http://<box-ip>:4399`. That port is firewalled from the
internet, and it would be plain HTTP, which is wrong for anything carrying a
key.

## If you want call evaluation specifically

There is also `POST /evaluate` — HMAC-signed like `/connection`, takes a
transcript and a rubric, returns the model's answer. It handles the fiddly parts
for you: it trims over-long transcripts from the middle rather than the ends,
and tells the model the text came from speech recognition so it does not mark
the agent down for the recogniser's mistakes.

Use `/v1/chat/completions` if you would rather own the prompt. Both share the
same 60-second gate and the same 2-request budget.

## Quick checks

```bash
# is the agent up?
curl https://<agent-host>/health

# is the key good?  (200 = yes, 401 = no)
curl -o /dev/null -w '%{http_code}\n' -X POST https://<agent-host>/v1/chat/completions \
  -H "Authorization: Bearer $REXA_LLM_API_KEY" -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"hi"}],"max_tokens":5}'
```

`401` means the key is wrong or missing. `400` means read the body — it carries
the model's own reason. A long pause before a `200` is the gate doing its job.
