package rexa

import (
	"bytes"
	"encoding/json"
	"hash/fnv"
	"io"
	"net/http"
	"strconv"
)

// Prefix-aware routing across LLM replicas.
//
// SGLang caches KV by PREFIX, per replica. Two calls carrying the same campaign
// prompt only share that work if they are served by the SAME replica — and a
// connection-counting load balancer has no idea they are related, so it splits
// them. With two replicas each one caches the campaign separately and sees half
// the sharing; the same call's own accumulated history can also land on the
// replica that has never seen it.
//
// The fix is to route on what the cache actually keys on. This tags every LLM
// request with a hash of the leading bytes of the system prompt, and nginx
// hashes on that tag (deploy/llm/nginx-llm-lb.conf), so identical prefixes are
// pinned to one replica and stay warm.
//
// The trade is cache locality against load balance. `least_conn` spreads work
// evenly and wastes cache; hashing keeps cache and can pile a large campaign
// onto one replica. That is the right trade only while campaigns comfortably
// outnumber replicas — with 12 campaigns over 2 replicas it is fine, with 1
// campaign over 2 replicas it halves the fleet. Watch for it if the trip
// counter climbs while one GPU sits idle.

// PrefixKeyHeader is the header nginx hashes on.
const PrefixKeyHeader = "X-Prefix-Key"

// prefixKeyBytes is how much of the system prompt decides the key.
//
// Long enough to separate genuinely different campaigns, short enough that the
// per-contact tail — which differs on every call and must NOT change the route
// — never reaches it. Prompts here run 12-40 KB with the contact block last, so
// 4 KB is comfortably inside the shared region.
const prefixKeyBytes = 4096

// PrefixRouter wraps base so every request carries a routing tag derived from
// its system prompt. base may be nil for http.DefaultTransport.
func PrefixRouter(base http.RoundTripper) http.RoundTripper {
	if base == nil {
		base = http.DefaultTransport
	}
	return &prefixRouter{base: base}
}

type prefixRouter struct{ base http.RoundTripper }

func (p *prefixRouter) RoundTrip(r *http.Request) (*http.Response, error) {
	// Never fail a request over routing. A missing tag costs cache locality;
	// an error costs the call.
	if key, ok := routingKey(r); ok {
		r = r.Clone(r.Context())
		r.Header.Set(PrefixKeyHeader, key)
	}
	return p.base.RoundTrip(r)
}

// routingKey reads the request body and returns a hash of the system prompt's
// leading bytes.
//
// The body is consumed and replaced, which is safe here because these requests
// are built in-process from a buffer.
func routingKey(r *http.Request) (string, bool) {
	if r.Body == nil {
		return "", false
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil {
		return "", false
	}
	// Put the body back whatever happens next, or the request goes out empty.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	r.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(body)), nil
	}

	prompt, ok := systemPrompt(body)
	if !ok {
		return "", false
	}
	if len(prompt) > prefixKeyBytes {
		prompt = prompt[:prefixKeyBytes]
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(prompt))
	return strconv.FormatUint(h.Sum64(), 36), true
}

// chatRequest is the minimum of the chat-completions body needed to find the
// system prompt. Everything else is ignored, so a schema change elsewhere in
// the request cannot break routing.
type chatRequest struct {
	Messages []struct {
		Role    string `json:"role"`
		Content string `json:"content"`
	} `json:"messages"`
}

func systemPrompt(body []byte) (string, bool) {
	var req chatRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", false
	}
	for _, m := range req.Messages {
		if m.Role == "system" {
			return m.Content, m.Content != ""
		}
	}
	// No system message: fall back to the first message, which still groups a
	// conversation with itself even when there is no shared preamble to group
	// campaigns by.
	if len(req.Messages) > 0 && req.Messages[0].Content != "" {
		return req.Messages[0].Content, true
	}
	return "", false
}
