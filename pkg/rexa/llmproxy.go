package rexa

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
)

// An OpenAI-compatible chat-completions endpoint the platform can point any
// SDK at, so it can ask the model for whatever it likes without an agent
// deploy for each new use.
//
// WHY THE AGENT FRONTS IT. SGLang listens on 127.0.0.1 with no API key. That is
// safe only for as long as nothing outside the box can reach it, and publishing
// it directly would be publishing an unauthenticated GPU. Everything here
// exists to keep that true while still making the model usable: authentication
// the agent owns, a ceiling on what one request may cost, and the same gate
// that keeps evaluations off the calls' critical path.
//
// BEARER, NOT HMAC, and that is a deliberate exception. Every other endpoint in
// this package verifies an HMAC envelope, because the platform signs its
// dispatches and we must be sure they are the platform's. This one is meant for
// ad-hoc use from ordinary tooling — a bearer token is what an OpenAI client
// already sends, so pointing one at this URL works with no signing code at all.
// The key is therefore a SEPARATE credential from the HMAC secrets: it is
// handed to more places, so it must be rotatable without touching dispatch.
//
// NOT A GENERAL-PURPOSE PROXY. Streaming is not supported, the request is
// re-encoded rather than forwarded byte-for-byte, and max_tokens is capped
// regardless of what was asked for. Each of those is a limit on how much of the
// callers' GPU one request can take.

// ChatCompleter forwards an OpenAI chat-completions body to the model.
//
// Returns the upstream status and body so the caller sees the model's own error
// text — a context-length overflow says so, instead of arriving as a bare 500.
type ChatCompleter interface {
	Forward(ctx context.Context, body []byte) (status int, respBody []byte, err error)
}

// LLMProxy serves POST /v1/chat/completions.
type LLMProxy struct {
	upstream ChatCompleter
	gate     *Gate
	apiKey   string
	// maxTokensCap bounds generation length regardless of the request, because
	// generation time is the slot-holding cost and an uncapped request could
	// hold one of the few background slots for minutes.
	maxTokensCap int
	// defaultModel fills in when the caller omits one. Only one model is
	// served, so omitting it is the common case and failing on it would be
	// pedantry.
	defaultModel string
}

// LLMProxyDefaultMaxTokens is the ceiling applied when none is configured.
// Roughly a page of text: enough for summaries, extraction and drafting, short
// enough that one request cannot occupy a slot indefinitely.
const LLMProxyDefaultMaxTokens = 2048

// NewLLMProxy builds the endpoint. An empty apiKey disables it — see
// SetLLMProxy.
func NewLLMProxy(upstream ChatCompleter, gate *Gate, apiKey, defaultModel string, maxTokensCap int) *LLMProxy {
	if maxTokensCap <= 0 {
		maxTokensCap = LLMProxyDefaultMaxTokens
	}
	return &LLMProxy{
		upstream:     upstream,
		gate:         gate,
		apiKey:       apiKey,
		maxTokensCap: maxTokensCap,
		defaultModel: defaultModel,
	}
}

// MaxTokensCap reports the EFFECTIVE ceiling, after defaults.
//
// Exists so callers can log what is actually enforced. Logging the raw config
// value printed "capped at 0" whenever the setting was absent, which reads as
// "no generation allowed" and is the opposite of the truth.
func (p *LLMProxy) MaxTokensCap() int { return p.maxTokensCap }

// authorized checks the bearer token in constant time.
//
// Constant-time because a byte-by-byte comparison leaks the key one character
// at a time to anyone who can measure the difference, and this key guards a
// GPU.
func (p *LLMProxy) authorized(r *http.Request) bool {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return false
	}
	got := strings.TrimSpace(h[len(prefix):])
	return subtle.ConstantTimeCompare([]byte(got), []byte(p.apiKey)) == 1
}

// SetLLMProxy installs the endpoint. Without it the route is not registered, so
// a deployment that has not set a key answers 404 rather than serving an
// unauthenticated model.
func (s *Server) SetLLMProxy(p *LLMProxy) { s.llmProxy = p }

// handleChatCompletions serves POST /v1/chat/completions.
func (s *Server) handleChatCompletions(w http.ResponseWriter, r *http.Request) {
	p := s.llmProxy
	if !p.authorized(r) {
		// No detail: telling an unauthenticated caller whether the header was
		// missing or merely wrong is free reconnaissance.
		w.Header().Set("WWW-Authenticate", `Bearer realm="agent"`)
		writeErr(w, Errorf(ErrCodeUnauthenticated, "a valid bearer token is required"))
		return
	}

	body, err := readCappedBody(r)
	if err != nil {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "%s", err.Error()))
		return
	}

	// Decode into a map rather than a struct: the OpenAI body has a long tail
	// of options (stop, top_p, response_format, seed) that this endpoint has no
	// opinion about, and a struct would silently drop every one of them.
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "body is not valid JSON"))
		return
	}
	if _, ok := req["messages"]; !ok {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "messages is required"))
		return
	}
	// Streaming would hold a background slot for the whole generation and make
	// the gate's accounting a lie. Refused explicitly rather than ignored, so a
	// client asking for it finds out here instead of hanging.
	if b, ok := req["stream"].(bool); ok && b {
		writeErr(w, Errorf(ErrCodeInvalidRequest,
			"streaming is not supported on this endpoint; omit stream or set it to false"))
		return
	}
	if _, ok := req["model"]; !ok && p.defaultModel != "" {
		req["model"] = p.defaultModel
	}
	req["max_tokens"] = cappedMaxTokens(req["max_tokens"], p.maxTokensCap)

	forward, err := json.Marshal(req)
	if err != nil {
		writeErr(w, Errorf(ErrCodeInternal, "could not re-encode request"))
		return
	}

	var status int
	var out []byte
	waited, deferred, err := p.gate.Run(r.Context(), func(ctx context.Context) error {
		var e error
		status, out, e = p.upstream.Forward(ctx, forward)
		return e
	})
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return // the caller hung up; nothing to report to
		}
		log.Printf("rexa: chat completion failed: %v", err)
		writeErr(w, Errorf(ErrCodeInternal, "upstream model request failed"))
		return
	}

	// The gate's accounting rides in headers so the body stays exactly what an
	// OpenAI client expects. A client that ignores them still works; one that
	// reads them can see when it is competing with live calls.
	w.Header().Set("X-Agent-Waited-Ms", strconv.FormatInt(waited.Milliseconds(), 10))
	if deferred {
		w.Header().Set("X-Agent-Deferred", "true")
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(out)
}

// cappedMaxTokens clamps whatever the caller asked for, filling in the cap when
// they asked for nothing. JSON numbers arrive as float64.
func cappedMaxTokens(v any, cap int) int {
	n, ok := v.(float64)
	if !ok || n <= 0 {
		return cap
	}
	if int(n) > cap {
		return cap
	}
	return int(n)
}

// readCappedBody reads at most MaxBodyBytes, reporting a usable error rather
// than truncating silently.
func readCappedBody(r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		return nil, errors.New("could not read request body")
	}
	if len(body) > MaxBodyBytes {
		return nil, errors.New("body exceeds " + strconv.Itoa(MaxBodyBytes) + " bytes")
	}
	return body, nil
}
