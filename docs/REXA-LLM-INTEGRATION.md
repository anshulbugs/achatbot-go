# Using the agent's LLM from Rexa

The voice agent exposes the model it runs calls on, so Rexa can use it for
anything — summaries, extraction, classification, drafting, scoring — without
us shipping a new endpoint each time.

It is **OpenAI-compatible**. Point any OpenAI SDK at the agent's `/v1` and it
works.

## Connect

| | |
|---|---|
| Base URL | `https://agent.rexa.ai/v1` |
| Auth | `Authorization: Bearer <REXA_LLM_API_KEY>` |
| Model | `google/gemma-4-E4B-it` (or omit `model` — it fills in) |
| Context window | 8192 tokens, prompt + reply combined |
| Max reply | 2048 tokens, capped server-side whatever you ask for |

### Set a User-Agent — the Node SDK's default is blocked

The hostname sits behind Cloudflare, and the zone's bot rules **403 any
User-Agent starting `OpenAI/`** before the request ever reaches the agent.
That is the OpenAI **Node** SDK's default (`OpenAI/JS 6.37.0`), so a stock
`new OpenAI({...})` fails with `Your request was blocked.` — which looks like
an agent outage and is not one.

Measured against the live endpoint: `OpenAI/JS 6.37.0` → 403, `OpenAI/JS 4.0.0`
→ 403, `OpenAI/NodeJS` → 403, but `openai-node/6.37.0` → 200 and
`rexa-platform/1.0` → 200. It is the `OpenAI/` prefix, not a version, so
upgrading the SDK will not help.

Send your own instead. It is also the more truthful header: this is your worker
calling your own model, not an OpenAI crawler fetching a page.

```js
const client = new OpenAI({
  baseURL: "https://agent.rexa.ai/v1",
  apiKey: process.env.REXA_LLM_API_KEY,
  defaultHeaders: { "User-Agent": "rexa-platform/1.0" },   // REQUIRED on Node
});
```

The Python SDK's default (`openai-python/...`) is not affected, but setting one
explicitly is still worth doing so a future SDK change cannot reintroduce this.

```python
from openai import OpenAI

client = OpenAI(
    base_url="https://agent.rexa.ai/v1",
    api_key=REXA_LLM_API_KEY,
    default_headers={"User-Agent": "rexa-platform/1.0"},
)

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
curl https://agent.rexa.ai/v1/chat/completions \
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

## The agent host

```
https://agent.rexa.ai
```

Stable. It is a **named Cloudflare tunnel**, so it survives agent restarts and
reboots — unlike the temporary `*.trycloudflare.com` names used during
bring-up, which changed every time. Safe to put in config.

Do **not** use `http://<box-ip>:4399`. That port is firewalled from the
internet, and it would be plain HTTP, which is wrong for anything carrying a
key.

## Quick checks

```bash
# is the agent up?
curl https://agent.rexa.ai/health

# is the key good?  (200 = yes, 401 = no)
curl -o /dev/null -w '%{http_code}\n' -X POST https://agent.rexa.ai/v1/chat/completions \
  -H "Authorization: Bearer $REXA_LLM_API_KEY" -H "Content-Type: application/json" \
  -d '{"messages":[{"role":"user","content":"hi"}],"max_tokens":5}'
```

`401` means the key is wrong or missing. `400` means read the body — it carries
the model's own reason. A long pause before a `200` is the gate doing its job.

**`403` with the body `Your request was blocked.` is Cloudflare, not us.** The
response will carry `Server: cloudflare` and a `CF-RAY` header, and the agent
never saw the request. Almost always it is the User-Agent above.
