package rexa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
)

// MaxBodyBytes bounds an inbound dispatch body.
//
// The platform's own builder refuses to emit more than 128 KB, so anything
// larger is a bug or an attack rather than a legitimate dispatch. Bounding the
// read also keeps HMAC verification honest: the signature covers the bytes we
// actually read, and an unbounded read is a trivial memory-exhaustion vector
// on an endpoint that authenticates only after reading.
const MaxBodyBytes = 128 * 1024

// Dispatcher is the call-placing side of the contract, implemented by the
// caller. Keeping it an interface means the HTTP surface — verification,
// decoding, status-code mapping — is testable without touching telephony,
// GPUs, or a real Daily room.
//
// Implementations must return promptly. The platform abandons a dispatch after
// 30 s and retries, so these methods should acknowledge once the call is
// accepted and continue the work in the background rather than blocking until
// the call completes.
type Dispatcher interface {
	// DispatchPhone places an outbound call.
	DispatchPhone(ctx context.Context, req PhoneDispatchRequest) (DispatchResponse, error)
	// DispatchIncoming answers a carrier leg that is already ringing.
	DispatchIncoming(ctx context.Context, req IncomingDispatchRequest) (DispatchResponse, error)
	// DispatchWebrtc provisions a room and joins it.
	DispatchWebrtc(ctx context.Context, req WebrtcDispatchRequest) (WebrtcDispatchResponse, error)
}

// DispatchError lets a Dispatcher choose the contract error code the platform
// sees. The code is not cosmetic: the platform retries some and fails the
// session outright on others, so returning a plain error (which maps to
// internal_error, and is retried) for invalid credentials turns a permanent
// misconfiguration into three doomed attempts.
type DispatchError struct {
	Code    string
	Message string
}

func (e *DispatchError) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

// Errorf builds a DispatchError with one of the ErrCode* constants.
func Errorf(code, format string, args ...any) *DispatchError {
	return &DispatchError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// statusFor maps a contract error code to its HTTP status.
//
// The platform branches on the code in the body, but the status has to agree:
// it treats 5xx as retryable and 4xx as terminal before it ever looks at the
// code, so a 500 carrying provider_credentials_invalid would still be retried.
func statusFor(code string) int {
	switch code {
	case ErrCodeInvalidRequest:
		return http.StatusBadRequest
	case ErrCodeUnauthenticated:
		return http.StatusUnauthorized
	case ErrCodeNotFound:
		return http.StatusNotFound
	case ErrCodeProviderCredsInvalid:
		return http.StatusPreconditionFailed // 412, per the contract
	case ErrCodeAtCapacity, ErrCodeProviderUnavailable:
		return http.StatusServiceUnavailable
	default:
		return http.StatusInternalServerError
	}
}

// Server exposes the HTTP surface the platform dispatches to.
type Server struct {
	verifier   *Verifier
	dispatcher Dispatcher
	metrics    *Metrics
}

// NewServer builds the dispatch surface. secret is the platform's OUTBOUND
// secret — the one it signs dispatches with, which we verify. Signing our
// callbacks uses the other one; see NewPoster.
//
// A nil metrics registry is replaced with one that has no ceiling, so tests
// and demo-only deployments need not supply one.
func NewServer(secret string, nonces NonceStore, d Dispatcher, m *Metrics) *Server {
	if m == nil {
		m = NewMetrics(0)
	}
	return &Server{verifier: NewVerifier(secret, nonces), dispatcher: d, metrics: m}
}

// Metrics exposes the registry so callers can instrument their call paths.
func (s *Server) Metrics() *Metrics { return s.metrics }

// Drain takes the agent out of rotation. Calls already running are unaffected.
func (s *Server) Drain() { s.metrics.SetDraining(true) }

// Routes registers the contract endpoints on mux.
//
// Paths are the ones the platform actually calls, which are not all the ones
// its OpenAPI documents: the WebRTC path is /connection_webrtc, not the
// spec's /connect_webrtc.
func (s *Server) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /connection", s.handlePhone)
	mux.HandleFunc("POST /incoming", s.handleIncoming)
	mux.HandleFunc("POST /connection_webrtc", s.handleWebrtc)
}

// handleHealth is the load-balancer liveness probe: unauthenticated, and
// deliberately free of dependency checks. Every number comes from in-process
// counters — a probe that fanned out to SGLang, Parakeet and Kokoro would take
// the agent out of rotation whenever one was merely slow, turning a latency
// blip into an outage. The platform hits this every 5 s across the fleet.
//
// The response is a superset of the v1 contract's `{status}`. The platform's
// Zod schema is a plain object, which strips unknown keys rather than
// rejecting them, so the extra fields are safe to serve before it reads them.
//
// The HTTP status still reflects liveness only, NOT capacity: a full agent is
// healthy, just busy. Reporting 503 when merely at capacity would make the
// platform's load balancer mark the URL unhealthy and stop probing it
// normally, when all we wanted was backpressure — which `accepting` carries.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	snap := s.metrics.Snapshot()
	status := http.StatusOK
	if !snap.Status {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, snap)
}

func (s *Server) handlePhone(w http.ResponseWriter, r *http.Request) {
	var req PhoneDispatchRequest
	if !s.decode(w, r, &req) {
		return
	}
	if missing := requireFields(map[string]string{
		"session_id":    req.SessionID,
		"tenant_id":     req.TenantID,
		"to_number":     req.ToNumber,
		"from_number":   req.FromNumber,
		"voice":         req.Voice,
		"system_prompt": req.SystemPrompt,
		"webhook_url":   req.WebhookURL,
	}); missing != "" {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "missing required field: %s", missing))
		return
	}
	if !s.admit(w, req.SessionID) {
		return
	}
	resp, err := s.dispatcher.DispatchPhone(r.Context(), req)
	if err != nil {
		log.Printf("rexa: /connection session=%s failed: %v", req.SessionID, err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIncoming(w http.ResponseWriter, r *http.Request) {
	var req IncomingDispatchRequest
	if !s.decode(w, r, &req) {
		return
	}
	if missing := requireFields(map[string]string{
		"CCID":          req.CCID,
		"session_id":    req.SessionID,
		"tenant_id":     req.TenantID,
		"system_prompt": req.SystemPrompt,
		"webhook_url":   req.WebhookURL,
	}); missing != "" {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "missing required field: %s", missing))
		return
	}
	resp, err := s.dispatcher.DispatchIncoming(r.Context(), req)
	if err != nil {
		log.Printf("rexa: /incoming session=%s failed: %v", req.SessionID, err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleWebrtc(w http.ResponseWriter, r *http.Request) {
	var req WebrtcDispatchRequest
	if !s.decode(w, r, &req) {
		return
	}
	if missing := requireFields(map[string]string{
		"session_id":    req.SessionID,
		"tenant_id":     req.TenantID,
		"voice":         req.Voice,
		"system_prompt": req.SystemPrompt,
		"webhook_url":   req.WebhookURL,
	}); missing != "" {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "missing required field: %s", missing))
		return
	}
	resp, err := s.dispatcher.DispatchWebrtc(r.Context(), req)
	if err != nil {
		log.Printf("rexa: /connection_webrtc session=%s failed: %v", req.SessionID, err)
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// admit applies backpressure, writing the error response itself and reporting
// whether the caller may proceed.
//
// Nothing else caps concurrency: the provider pools pre-warm instances but
// create more on demand rather than blocking, so without this the 62nd call is
// accepted and every caller's p95 degrades together. The measured cliff is
// sharp — 60 concurrent sessions held p95 at 1628 ms with zero dropped audio
// writes, while 100 produced 6244 ms and 234 drops.
//
// at_capacity is the right code: the platform holds the session and retries
// rather than failing it, so a caller is delayed rather than lost. Inbound
// calls are deliberately exempt — the carrier leg is already ringing and a
// human is on it, so refusing costs a real answered call, whereas an outbound
// dispatch can simply wait.
func (s *Server) admit(w http.ResponseWriter, sessionID string) bool {
	if s.metrics.Accepting() {
		return true
	}
	s.metrics.RejectedAtCapacity()
	snap := s.metrics.Snapshot()
	log.Printf("rexa: at capacity, refusing session=%s (on_gpu=%d/%d tiers=%v)",
		sessionID, snap.Calls.OnGPU, snap.Capacity.MaxGPUCalls, tierStates(snap))
	writeErr(w, Errorf(ErrCodeAtCapacity,
		"agent at capacity: %d of %d GPU calls in flight",
		snap.Calls.OnGPU, snap.Capacity.MaxGPUCalls))
	return false
}

// tierStates renders tier states compactly for one log line.
func tierStates(s HealthSnapshot) map[string]string {
	out := make(map[string]string, len(s.Tiers))
	for name, t := range s.Tiers {
		out[name] = t.State
	}
	return out
}

// decode reads the raw body, verifies the HMAC envelope over those exact
// bytes, then unmarshals. It writes the error response itself and reports
// whether the caller should continue.
//
// Order matters: the signature covers the bytes on the wire, so verification
// must happen before unmarshalling and must never run against a re-encoded
// body. This is the single most common way to break this contract on either
// side.
func (s *Server) decode(w http.ResponseWriter, r *http.Request, out any) bool {
	body, err := io.ReadAll(io.LimitReader(r.Body, MaxBodyBytes+1))
	if err != nil {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "could not read request body"))
		return false
	}
	if len(body) > MaxBodyBytes {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "body exceeds %d bytes", MaxBodyBytes))
		return false
	}
	env := Envelope{
		Signature: r.Header.Get(HeaderSignature),
		Timestamp: r.Header.Get(HeaderTimestamp),
		Nonce:     r.Header.Get(HeaderNonce),
	}
	if err := s.verifier.Verify(body, env); err != nil {
		// The reason goes to our log, not to the response: telling an
		// unauthenticated caller whether it failed on drift, replay or
		// signature is free reconnaissance. The platform's own error table
		// documents these strings for when we are asked to correlate.
		log.Printf("rexa: rejected %s %s: %v", r.Method, r.URL.Path, err)
		writeErr(w, Errorf(ErrCodeUnauthenticated, "HMAC verification failed"))
		return false
	}
	if err := json.Unmarshal(body, out); err != nil {
		writeErr(w, Errorf(ErrCodeInvalidRequest, "body is not valid JSON for this endpoint"))
		return false
	}
	return true
}

// requireFields returns the name of the first empty field, or "".
//
// Presence-only: E.164 and UUID shapes are the platform's to enforce, and
// duplicating its validation here would mean two places to update and a new
// way for the two sides to disagree.
func requireFields(fields map[string]string) string {
	// Iterate a fixed order so the reported field is deterministic; Go's map
	// order is randomised and a flapping error message is miserable to debug.
	for _, name := range []string{
		"CCID", "session_id", "tenant_id", "to_number", "from_number",
		"voice", "system_prompt", "webhook_url",
	} {
		if v, ok := fields[name]; ok && v == "" {
			return name
		}
	}
	return ""
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		log.Printf("rexa: write response: %v", err)
	}
}

// writeErr renders err as the contract's error envelope. A DispatchError
// carries its own code; anything else is an unexpected failure and becomes a
// retryable internal_error with its detail kept out of the response.
func writeErr(w http.ResponseWriter, err error) {
	var de *DispatchError
	if !errors.As(err, &de) {
		de = &DispatchError{Code: ErrCodeInternal, Message: "internal error"}
	}
	writeJSON(w, statusFor(de.Code), NewAgentError(de.Code, de.Message))
}
