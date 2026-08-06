package rexa

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// newTestPoster returns a Poster whose retry ladder does not actually sleep,
// plus the delays it would have waited.
func newTestPoster(secret string) (*Poster, *[]time.Duration) {
	p := NewPoster(secret)
	var mu sync.Mutex
	delays := &[]time.Duration{}
	p.sleep = func(_ context.Context, d time.Duration) error {
		mu.Lock()
		defer mu.Unlock()
		*delays = append(*delays, d)
		return nil
	}
	return p, delays
}

// The receiving end must be able to verify what we send. This drives a real
// HTTP server through our own Verifier over the exact bytes received — the
// same check the platform performs.
func TestPostSignsVerifiably(t *testing.T) {
	const secret = "s3cr3t"
	var gotBody []byte
	var gotEnv Envelope

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotEnv = Envelope{
			Signature: r.Header.Get(HeaderSignature),
			Timestamp: r.Header.Get(HeaderTimestamp),
			Nonce:     r.Header.Get(HeaderNonce),
		}
		if ct := r.Header.Get("content-type"); ct != "application/json" {
			t.Errorf("content-type = %q", ct)
		}
		w.Write([]byte(`{"ok":true,"duplicate":false}`))
	}))
	defer srv.Close()

	p, _ := newTestPoster(secret)
	report := EndOfCallReport{
		SessionID:  "11111111-1111-7111-8111-111111111111",
		TenantID:   "22222222-2222-7222-8222-222222222222",
		CallStatus: CallStatusCompleted,
		EndReason:  EndReasonCompleted,
	}
	if err := p.PostEndOfCall(context.Background(), srv.URL, report); err != nil {
		t.Fatalf("post: %v", err)
	}
	if err := NewVerifier(secret, NewMemoryNonceStore()).Verify(gotBody, gotEnv); err != nil {
		t.Fatalf("platform could not verify what we sent: %v", err)
	}

	// The body must carry the required v1 fields.
	var decoded map[string]any
	if err := json.Unmarshal(gotBody, &decoded); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"session_id", "tenant_id", "call_status", "end_reason"} {
		if _, ok := decoded[k]; !ok {
			t.Errorf("required field %q missing from wire body", k)
		}
	}
}

// 5xx is transient: keep trying, and succeed when the platform recovers.
func TestPostRetriesServerErrorsThenSucceeds(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	p, delays := newTestPoster("s")
	if err := p.Post(context.Background(), srv.URL, EndOfCallReport{}); err != nil {
		t.Fatalf("post: %v", err)
	}
	if attempts != 3 {
		t.Errorf("attempts = %d, want 3", attempts)
	}
	// Two failures means two waits, and they must follow the documented ladder.
	want := RetrySchedule[:2]
	if len(*delays) != len(want) {
		t.Fatalf("delays = %v, want %v", *delays, want)
	}
	for i := range want {
		if (*delays)[i] != want[i] {
			t.Errorf("delay[%d] = %v, want %v", i, (*delays)[i], want[i])
		}
	}
}

// All attempts fail: the ladder runs to exhaustion and the error surfaces.
func TestPostGivesUpAfterFullLadder(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	p, delays := newTestPoster("s")
	err := p.Post(context.Background(), srv.URL, EndOfCallReport{})
	if err == nil {
		t.Fatal("expected an error after exhausting retries")
	}
	// One initial attempt plus one per rung.
	if want := len(RetrySchedule) + 1; attempts != want {
		t.Errorf("attempts = %d, want %d", attempts, want)
	}
	if len(*delays) != len(RetrySchedule) {
		t.Errorf("delays = %d, want %d", len(*delays), len(RetrySchedule))
	}
}

// 4xx is permanent — the spec is explicit that retrying a 401/400 just burns
// the ladder on a bug that needs a code change.
func TestPostDoesNotRetryClientErrors(t *testing.T) {
	for _, code := range []int{http.StatusBadRequest, http.StatusUnauthorized} {
		var mu sync.Mutex
		attempts := 0
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			mu.Lock()
			attempts++
			mu.Unlock()
			w.WriteHeader(code)
		}))

		p, delays := newTestPoster("s")
		if err := p.Post(context.Background(), srv.URL, EndOfCallReport{}); err == nil {
			t.Errorf("%d: expected an error", code)
		}
		if attempts != 1 {
			t.Errorf("%d: attempts = %d, want 1 (must not retry)", code, attempts)
		}
		if len(*delays) != 0 {
			t.Errorf("%d: slept %v before giving up", code, *delays)
		}
		srv.Close()
	}
}

// Every attempt in one ladder is the same logical event, so the nonce must be
// stable — otherwise the platform sees N distinct deliveries instead of one
// retried delivery.
func TestRetriesReuseOneNonce(t *testing.T) {
	var mu sync.Mutex
	var nonces []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		nonces = append(nonces, r.Header.Get(HeaderNonce))
		n := len(nonces)
		mu.Unlock()
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	p, _ := newTestPoster("s")
	if err := p.Post(context.Background(), srv.URL, EndOfCallReport{}); err != nil {
		t.Fatalf("post: %v", err)
	}
	for i, n := range nonces {
		if n != nonces[0] {
			t.Errorf("nonce[%d] = %q, want %q — retries must reuse one nonce", i, n, nonces[0])
		}
	}
}

// A cancelled context must abandon the ladder promptly rather than sitting in
// a 12-minute sleep during shutdown.
func TestPostAbandonsOnContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	p := NewPoster("s")
	p.sleep = func(c context.Context, _ time.Duration) error {
		cancel() // simulate shutdown arriving during the first backoff
		return c.Err()
	}
	err := p.Post(ctx, srv.URL, EndOfCallReport{})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "abandoned") {
		t.Errorf("error = %v, want it to mention abandonment", err)
	}
}

// The platform validates timestamps with Zod's .datetime(), which accepts only
// UTC with a literal Z. A numeric offset fails validation and takes the whole
// report down.
func TestISOTimeIsUTCWithZ(t *testing.T) {
	loc := time.FixedZone("IST", 5*3600+1800)
	got := ISOTime(time.Date(2026, 8, 6, 14, 30, 0, 123456789, loc))
	const want = "2026-08-06T09:00:00.123Z"
	if got != want {
		t.Errorf("ISOTime = %q, want %q", got, want)
	}
}

// A greeting is always the first turn at t=0.0. With a plain float64 and
// omitempty that zero vanishes, silently un-anchoring the transcript.
func TestZeroTimestampSurvivesSerialisation(t *testing.T) {
	zero := 0.0
	b, err := json.Marshal(MessageTurn{Role: RoleAgent, Content: "hi", T: &zero})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"t":0`) {
		t.Errorf("t=0 dropped from %s", b)
	}
}

func TestTelnyxCredentials(t *testing.T) {
	ok := TelecomCredentials{
		Provider:    "telnyx",
		Credentials: json.RawMessage(`{"api_key":"KEY01","connection_id":"123"}`),
	}
	c, err := ok.Telnyx()
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if c.APIKey != "KEY01" || c.ConnectionID != "123" {
		t.Errorf("decoded = %+v", c)
	}

	// A misrouted provider must fail loudly rather than dial with empty creds.
	wrong := TelecomCredentials{
		Provider:    "twilio",
		Credentials: json.RawMessage(`{"account_sid":"AC","auth_token":"t"}`),
	}
	if _, err := wrong.Telnyx(); err == nil {
		t.Error("expected an error for a non-telnyx provider")
	}

	// Present but incomplete is just as unusable as absent.
	partial := TelecomCredentials{
		Provider:    "telnyx",
		Credentials: json.RawMessage(`{"api_key":"KEY01"}`),
	}
	if _, err := partial.Telnyx(); err == nil {
		t.Error("expected an error when connection_id is missing")
	}
}
