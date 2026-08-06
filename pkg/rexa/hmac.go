// Package rexa implements the platform ↔ call-agent contract: the HMAC
// envelope both directions share, the wire types, the HTTP surface the
// platform dispatches to, and the callbacks we post back.
//
// The platform is the Voice API Platform ("Rexa.ai") in the jtvapi repo. Its
// runtime source of truth is packages/agent-contract/src/schemas.ts, NOT the
// OpenAPI YAML alongside it — the YAML is a stale v0 draft that still
// documents /connect_webrtc, a nested voice{} object and a voice-cloning
// lifecycle no code calls. Where the two disagree, the Zod schemas win.
package rexa

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Wire header names. Lowercase on the wire; Go's http.Header canonicalises on
// read, so always go through http.Header.Get rather than map indexing.
const (
	HeaderSignature = "x-signature"
	HeaderTimestamp = "x-timestamp"
	HeaderNonce     = "x-nonce"
)

const (
	// MaxDrift is how far a timestamp may sit from our clock before we reject
	// it. Matches the platform's verifier exactly — widening it on one side
	// only converts a clean 401 into a confusing one-way failure.
	MaxDrift = 5 * time.Minute
	// NonceTTL is how long a nonce is remembered for replay rejection.
	NonceTTL = 10 * time.Minute
)

// Verification failure reasons, returned rather than logged so the caller
// decides what is safe to expose. The platform's own error table maps these
// one-for-one, which makes cross-system debugging a string comparison.
var (
	ErrMissingHeaders = errors.New("missing_headers")
	ErrBadTimestamp   = errors.New("bad_timestamp")
	ErrDriftExceeded  = errors.New("drift_exceeded")
	ErrNonceReplay    = errors.New("nonce_replay")
	ErrSigMismatch    = errors.New("sig_mismatch")
)

// Envelope is the header triple accompanying a signed body.
type Envelope struct {
	Signature string
	Timestamp string
	Nonce     string
}

// canonical builds the signed string: "{timestamp}\n{nonce}\n{body}".
//
// A single \n after each of the first two fields and none at the end. The body
// is appended as raw bytes and never re-encoded — a JSON round-trip through a
// map reorders keys and changes whitespace, and the signature is over bytes,
// not over meaning.
func canonical(timestamp, nonce string, body []byte) []byte {
	out := make([]byte, 0, len(timestamp)+len(nonce)+len(body)+2)
	out = append(out, timestamp...)
	out = append(out, '\n')
	out = append(out, nonce...)
	out = append(out, '\n')
	return append(out, body...)
}

// Sign produces the envelope for an outgoing request body.
//
// secret is the shared secret as the platform stores it. It is used as raw
// UTF-8 key material, matching the platform's Python reference
// (`secret.encode()`) — NOT hex-decoded, even though the secret is
// conventionally distributed as 32 bytes of hex. Decoding it here would
// produce a valid-looking signature that never verifies.
func Sign(secret string, body []byte) Envelope {
	return signAt(secret, body, time.Now(), uuid.NewString())
}

func signAt(secret string, body []byte, now time.Time, nonce string) Envelope {
	ts := strconv.FormatInt(now.UnixMilli(), 10)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(canonical(ts, nonce, body))
	return Envelope{
		Signature: hex.EncodeToString(mac.Sum(nil)),
		Timestamp: ts,
		Nonce:     nonce,
	}
}

// SignWithNonce signs using a caller-supplied nonce.
//
// Needed for retries: the platform deliberately reuses one nonce across all
// three dispatch attempts so the receiver can collapse them into a single
// logical dispatch. We do the same when re-posting a callback, so the
// platform's dedup sees a retry rather than a new event.
func SignWithNonce(secret string, body []byte, nonce string) Envelope {
	return signAt(secret, body, time.Now(), nonce)
}

// NonceStore remembers recently-seen nonces so a captured request cannot be
// replayed inside the drift window.
type NonceStore interface {
	// SeenBefore records the nonce and reports whether it was already present.
	SeenBefore(nonce string) bool
}

// MemoryNonceStore is an in-process NonceStore.
//
// Sufficient while dispatch terminates on a single agent process. It is NOT
// sufficient across a multi-replica fleet behind the platform's round-robin
// load balancer: each replica would keep its own view, and a replayed request
// aimed at a different replica would be accepted. Swap in a Redis-backed
// store (SET key NX EX 600) before running more than one replica.
type MemoryNonceStore struct {
	mu   sync.Mutex
	seen map[string]time.Time
	// now is injectable so tests can advance the clock without sleeping.
	now func() time.Time
}

// NewMemoryNonceStore returns an empty store. Expired entries are evicted
// opportunistically during SeenBefore, so an idle process does not retain
// nonces forever and no background goroutine is needed.
func NewMemoryNonceStore() *MemoryNonceStore {
	return &MemoryNonceStore{seen: make(map[string]time.Time), now: time.Now}
}

// SeenBefore records nonce and reports whether it had already been recorded
// within NonceTTL.
func (s *MemoryNonceStore) SeenBefore(nonce string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	// Evict on write. The map only grows as fast as real traffic, and at any
	// plausible call rate the sweep is far cheaper than the lock we already hold.
	for k, t := range s.seen {
		if now.Sub(t) > NonceTTL {
			delete(s.seen, k)
		}
	}
	if t, ok := s.seen[nonce]; ok && now.Sub(t) <= NonceTTL {
		return true
	}
	s.seen[nonce] = now
	return false
}

// Verifier checks inbound envelopes against a shared secret.
type Verifier struct {
	secret string
	nonces NonceStore
	now    func() time.Time
}

// NewVerifier builds a Verifier. Passing a nil store disables replay
// protection, which is acceptable only in tests — a captured request stays
// replayable for the whole drift window without it.
func NewVerifier(secret string, nonces NonceStore) *Verifier {
	return &Verifier{secret: secret, nonces: nonces, now: time.Now}
}

// Verify checks an inbound request's envelope against the raw body.
//
// body MUST be the exact bytes read off the wire, before any JSON decoding.
// Verifying a re-encoded body is the single most common integration failure on
// both sides of this contract.
func (v *Verifier) Verify(body []byte, env Envelope) error {
	if env.Signature == "" || env.Timestamp == "" || env.Nonce == "" {
		return ErrMissingHeaders
	}
	ms, err := strconv.ParseInt(env.Timestamp, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	// Absolute difference: a timestamp from the future is as suspect as a stale
	// one, and clock skew runs in both directions.
	drift := v.now().Sub(time.UnixMilli(ms))
	if drift < 0 {
		drift = -drift
	}
	if drift > MaxDrift {
		return fmt.Errorf("%w: %s", ErrDriftExceeded, drift.Round(time.Second))
	}
	mac := hmac.New(sha256.New, []byte(v.secret))
	mac.Write(canonical(env.Timestamp, env.Nonce, body))
	expected := hex.EncodeToString(mac.Sum(nil))
	// Constant-time compare. hmac.Equal also tolerates length mismatch, so a
	// truncated signature fails here rather than panicking.
	if !hmac.Equal([]byte(expected), []byte(env.Signature)) {
		return ErrSigMismatch
	}
	// Replay check runs LAST, after the signature proves the request is
	// authentic. Recording nonces from unauthenticated requests would let an
	// attacker poison the store and lock out real traffic.
	if v.nonces != nil && v.nonces.SeenBefore(env.Nonce) {
		return ErrNonceReplay
	}
	return nil
}
