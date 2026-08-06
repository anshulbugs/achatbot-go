package rexa

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sync/atomic"
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
	// draining flips to true on shutdown so /health reports false and the
	// platform's load balancer stops routing new calls here, while calls
	// already in flight run to completion.
	draining atomic.Bool
}

// NewServer builds the dispatch surface. secret is the platform's OUTBOUND
// secret — the one it signs dispatches with, which we verify. Signing our
// callbacks uses the other one; see NewPoster.
func NewServer(secret string, nonces NonceStore, d Dispatcher) *Server {
	return &Server{verifier: NewVerifier(secret, nonces), dispatcher: d}
}

// Drain makes /health report false. Calls already running are unaffected.
func (s *Server) Drain() { s.draining.Store(true) }

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
// deliberately free of dependency checks. A deep readiness probe would couple
// our health to downstream systems we do not own, so a slow LLM would take the
// whole agent out of rotation instead of merely slowing calls down.
func (s *Server) handleHealth(w http.ResponseWriter, _ *http.Request) {
	live := !s.draining.Load()
	status := http.StatusOK
	if !live {
		status = http.StatusServiceUnavailable
	}
	writeJSON(w, status, HealthResponse{Status: live})
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
