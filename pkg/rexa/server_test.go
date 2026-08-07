package rexa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeDispatcher records what it was handed and returns canned results.
type fakeDispatcher struct {
	phone    *PhoneDispatchRequest
	incoming *IncomingDispatchRequest
	webrtc   *WebrtcDispatchRequest
	err      error
}

func (f *fakeDispatcher) DispatchPhone(_ context.Context, r PhoneDispatchRequest) (DispatchResponse, error) {
	f.phone = &r
	if f.err != nil {
		return DispatchResponse{}, f.err
	}
	return DispatchResponse{Status: "accepted", AgentSessionID: "agent-1"}, nil
}

func (f *fakeDispatcher) DispatchIncoming(_ context.Context, r IncomingDispatchRequest) (DispatchResponse, error) {
	f.incoming = &r
	if f.err != nil {
		return DispatchResponse{}, f.err
	}
	return DispatchResponse{Status: "accepted"}, nil
}

func (f *fakeDispatcher) DispatchWebrtc(_ context.Context, r WebrtcDispatchRequest) (WebrtcDispatchResponse, error) {
	f.webrtc = &r
	if f.err != nil {
		return WebrtcDispatchResponse{}, f.err
	}
	return WebrtcDispatchResponse{RoomURL: "https://x.daily.co/r", Token: "tok"}, nil
}

const testSecret = "test-secret"

// newTestServer wires a Server behind httptest and returns it with its fake.
func newTestServer(t *testing.T) (*httptest.Server, *fakeDispatcher, *Server) {
	t.Helper()
	f := &fakeDispatcher{}
	s := NewServer(testSecret, NewMemoryNonceStore(), f, nil)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, f, s
}

// post signs body the way the platform would and sends it.
func post(t *testing.T, srv *httptest.Server, path, body string) *http.Response {
	t.Helper()
	env := Sign(testSecret, []byte(body))
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set(HeaderSignature, env.Signature)
	req.Header.Set(HeaderTimestamp, env.Timestamp)
	req.Header.Set(HeaderNonce, env.Nonce)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

const validPhone = `{
  "session_id":"11111111-1111-7111-8111-111111111111",
  "tenant_id":"22222222-2222-7222-8222-222222222222",
  "to_number":"+15557654321","from_number":"+15551234567","direction":"outbound",
  "telecom_credentials":{"provider":"telnyx","credentials":{"api_key":"KEY01","connection_id":"123"}},
  "voice":"leah","language":"en","system_prompt":"be helpful",
  "hello_message":"hi","voicemail_message":"sorry we missed you",
  "webhook_url":"https://platform.example.com/v1/_internal/webhooks/call-agent"
}`

func TestPhoneDispatchHappyPath(t *testing.T) {
	srv, f, _ := newTestServer(t)
	resp := post(t, srv, "/connection", validPhone)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out DispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Status != "accepted" {
		t.Errorf("status = %q", out.Status)
	}
	if f.phone == nil {
		t.Fatal("dispatcher not invoked")
	}
	// The per-call credentials must survive decoding — this is what replaces
	// the process-global API key.
	creds, err := f.phone.TelecomCredentials.Telnyx()
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if creds.APIKey != "KEY01" || creds.ConnectionID != "123" {
		t.Errorf("credentials = %+v", creds)
	}
	if f.phone.FromNumber != "+15551234567" {
		t.Errorf("from_number = %q — the platform's caller id must be honoured", f.phone.FromNumber)
	}
}

// Nothing gets through without a valid signature, and the dispatcher must not
// be reached.
func TestUnsignedRequestRejected(t *testing.T) {
	srv, f, _ := newTestServer(t)
	for _, path := range []string{"/connection", "/incoming", "/connection_webrtc"} {
		resp, err := srv.Client().Post(srv.URL+path, "application/json", strings.NewReader(validPhone))
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s: status = %d, want 401", path, resp.StatusCode)
		}
		resp.Body.Close()
	}
	if f.phone != nil || f.incoming != nil || f.webrtc != nil {
		t.Error("dispatcher was reached without authentication")
	}
}

func TestTamperedBodyRejected(t *testing.T) {
	srv, f, _ := newTestServer(t)
	env := Sign(testSecret, []byte(validPhone))
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/connection",
		strings.NewReader(strings.Replace(validPhone, "+15557654321", "+19995550000", 1)))
	req.Header.Set(HeaderSignature, env.Signature)
	req.Header.Set(HeaderTimestamp, env.Timestamp)
	req.Header.Set(HeaderNonce, env.Nonce)
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 for a rewritten destination", resp.StatusCode)
	}
	if f.phone != nil {
		t.Error("tampered dispatch reached the dispatcher")
	}
}

// A 401 must not explain which check failed — that is free reconnaissance for
// an unauthenticated caller.
func TestAuthFailureDoesNotLeakReason(t *testing.T) {
	srv, _, _ := newTestServer(t)
	resp, err := srv.Client().Post(srv.URL+"/connection", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var e AgentError
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"drift", "replay", "nonce", "mismatch", "missing_headers"} {
		if strings.Contains(strings.ToLower(e.Error.Message), leak) {
			t.Errorf("message %q leaks the failure reason", e.Error.Message)
		}
	}
	if e.Error.Code != ErrCodeUnauthenticated {
		t.Errorf("code = %q", e.Error.Code)
	}
}

func TestMissingRequiredFieldIsInvalidRequest(t *testing.T) {
	srv, f, _ := newTestServer(t)
	// Valid signature, but no webhook_url — without it we could never report.
	body := `{"session_id":"a","tenant_id":"b","to_number":"+1555","from_number":"+1556",
	          "voice":"leah","system_prompt":"p"}`
	resp := post(t, srv, "/connection", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
	var e AgentError
	json.NewDecoder(resp.Body).Decode(&e)
	if !strings.Contains(e.Error.Message, "webhook_url") {
		t.Errorf("message = %q, want it to name webhook_url", e.Error.Message)
	}
	if f.phone != nil {
		t.Error("incomplete dispatch reached the dispatcher")
	}
}

// The status code has to agree with the error code: the platform decides
// retryability from the status before it reads the body, so a credentials
// failure returned as 500 would be retried three times instead of failing the
// session immediately.
func TestErrorCodeDrivesStatus(t *testing.T) {
	cases := map[string]int{
		ErrCodeProviderCredsInvalid: http.StatusPreconditionFailed,
		ErrCodeAtCapacity:           http.StatusServiceUnavailable,
		ErrCodeProviderUnavailable:  http.StatusServiceUnavailable,
		ErrCodeInvalidRequest:       http.StatusBadRequest,
		ErrCodeInternal:             http.StatusInternalServerError,
	}
	for code, wantStatus := range cases {
		f := &fakeDispatcher{err: Errorf(code, "nope")}
		s := NewServer(testSecret, NewMemoryNonceStore(), f, nil)
		mux := http.NewServeMux()
		s.Routes(mux)
		srv := httptest.NewServer(mux)

		resp := post(t, srv, "/connection", validPhone)
		if resp.StatusCode != wantStatus {
			t.Errorf("%s: status = %d, want %d", code, resp.StatusCode, wantStatus)
		}
		var e AgentError
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error.Code != code {
			t.Errorf("%s: body code = %q", code, e.Error.Code)
		}
		resp.Body.Close()
		srv.Close()
	}
}

// An unexpected error must not leak its detail, and must be retryable.
func TestUnexpectedErrorBecomesInternal(t *testing.T) {
	f := &fakeDispatcher{err: errString("gpu pool exhausted at /dev/nvidia3")}
	s := NewServer(testSecret, NewMemoryNonceStore(), f, nil)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	resp := post(t, srv, "/connection", validPhone)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", resp.StatusCode)
	}
	var e AgentError
	json.NewDecoder(resp.Body).Decode(&e)
	if strings.Contains(e.Error.Message, "nvidia") {
		t.Errorf("internal detail leaked: %q", e.Error.Message)
	}
}

type errString string

func (e errString) Error() string { return string(e) }

func TestIncomingDispatch(t *testing.T) {
	srv, f, _ := newTestServer(t)
	body := `{"CCID":"v3:abc","session_id":"s","tenant_id":"t",
	          "from_number":"+15551112222","to_number":"+15553334444",
	          "telecom_credentials":{"provider":"telnyx","credentials":{"api_key":"K","connection_id":"C"}},
	          "voice":"leah","language":"en","system_prompt":"p","hello_message":"hi",
	          "webhook_url":"https://p.example.com/cb"}`
	resp := post(t, srv, "/incoming", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if f.incoming == nil {
		t.Fatal("dispatcher not invoked")
	}
	// CCID's capitalisation is literal on this contract; a lowercase tag here
	// would silently decode to "" and we would answer the wrong leg.
	if f.incoming.CCID != "v3:abc" {
		t.Errorf("CCID = %q, want v3:abc", f.incoming.CCID)
	}
}

func TestWebrtcDispatchReturnsRoom(t *testing.T) {
	srv, f, _ := newTestServer(t)
	body := `{"session_id":"s","tenant_id":"t","voice":"leah","language":"en",
	          "system_prompt":"p","hello_message":"hi","webhook_url":"https://p.example.com/cb"}`
	resp := post(t, srv, "/connection_webrtc", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var out WebrtcDispatchResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.RoomURL == "" || out.Token == "" {
		t.Errorf("room_url + token are both required by the platform: %+v", out)
	}
	if f.webrtc == nil {
		t.Fatal("dispatcher not invoked")
	}
}

// /health is unauthenticated by design — the load balancer probes it without
// signing — and must not require or consult the HMAC headers.
func TestHealthIsUnauthenticated(t *testing.T) {
	srv, _, s := newTestServer(t)

	resp, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var h HealthResponse
	json.NewDecoder(resp.Body).Decode(&h)
	if !h.Status {
		t.Error("status = false while healthy")
	}

	// Draining must take us out of rotation without killing live calls.
	s.Drain()
	resp2, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("draining status = %d, want 503", resp2.StatusCode)
	}
	var h2 HealthResponse
	json.NewDecoder(resp2.Body).Decode(&h2)
	if h2.Status {
		t.Error("status = true while draining")
	}
}

// An oversized body must be refused rather than read into memory, on an
// endpoint that authenticates only after reading.
func TestOversizedBodyRejected(t *testing.T) {
	srv, f, _ := newTestServer(t)
	huge := `{"pad":"` + strings.Repeat("x", MaxBodyBytes+100) + `"}`
	resp := post(t, srv, "/connection", huge)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
	if f.phone != nil {
		t.Error("oversized dispatch reached the dispatcher")
	}
}

// The platform reuses one nonce across its three dispatch retries. Those are
// the same logical call, so the second attempt must be rejected as a replay
// rather than placing a second call to the same person.
func TestReplayedDispatchRejected(t *testing.T) {
	srv, _, _ := newTestServer(t)
	env := Sign(testSecret, []byte(validPhone))
	send := func() int {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/connection", strings.NewReader(validPhone))
		req.Header.Set(HeaderSignature, env.Signature)
		req.Header.Set(HeaderTimestamp, env.Timestamp)
		req.Header.Set(HeaderNonce, env.Nonce)
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}
	if got := send(); got != http.StatusOK {
		t.Fatalf("first = %d, want 200", got)
	}
	if got := send(); got != http.StatusUnauthorized {
		t.Errorf("replay = %d, want 401", got)
	}
}

// Backpressure: past the ceiling the platform must get 503 at_capacity, which
// it holds and retries rather than failing the session. Nothing else in the
// stack caps concurrency — the provider pools create instances on demand
// rather than blocking — so without this the 62nd call is accepted and every
// caller's p95 degrades together.
func TestAtCapacityRejectsDispatch(t *testing.T) {
	f := &fakeDispatcher{}
	m := NewMetrics(2)
	s := NewServer(testSecret, NewMemoryNonceStore(), f, m)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m.CallStarted()
	m.CallStarted() // at the ceiling

	resp := post(t, srv, "/connection", validPhone)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", resp.StatusCode)
	}
	var e AgentError
	json.NewDecoder(resp.Body).Decode(&e)
	if e.Error.Code != ErrCodeAtCapacity {
		t.Errorf("code = %q, want at_capacity", e.Error.Code)
	}
	if f.phone != nil {
		t.Error("dispatch reached the dispatcher while at capacity")
	}
	if got := m.Snapshot().Totals.Rejected; got != 1 {
		t.Errorf("rejected total = %d, want 1", got)
	}

	// Freeing a slot must resume acceptance.
	m.CallEnded()
	resp2 := post(t, srv, "/connection", validPhone)
	defer resp2.Body.Close()
	if resp2.StatusCode != http.StatusOK {
		t.Errorf("status = %d after a slot freed, want 200", resp2.StatusCode)
	}
}

// Inbound is deliberately exempt from backpressure: the carrier leg is already
// ringing with a human on it, so refusing costs a real answered call, whereas
// an outbound dispatch can simply wait.
func TestIncomingNotRejectedAtCapacity(t *testing.T) {
	f := &fakeDispatcher{}
	m := NewMetrics(1)
	s := NewServer(testSecret, NewMemoryNonceStore(), f, m)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()
	m.CallStarted() // at the ceiling

	body := `{"CCID":"v3:abc","session_id":"s","tenant_id":"t",
	          "from_number":"+15551112222","to_number":"+15553334444",
	          "telecom_credentials":{"provider":"telnyx","credentials":{"api_key":"K","connection_id":"C"}},
	          "voice":"leah","language":"en","system_prompt":"p","hello_message":"hi",
	          "webhook_url":"https://p.example.com/cb"}`
	resp := post(t, srv, "/incoming", body)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — inbound must not be refused at capacity", resp.StatusCode)
	}
}

// /health must carry capacity without lying about liveness: a full agent is
// healthy, just busy. A 503 here would make the platform's load balancer mark
// the URL unhealthy when all we wanted was backpressure.
func TestHealthCarriesCapacityButStays200WhenFull(t *testing.T) {
	f := &fakeDispatcher{}
	m := NewMetrics(2)
	s := NewServer(testSecret, NewMemoryNonceStore(), f, m)
	mux := http.NewServeMux()
	s.Routes(mux)
	srv := httptest.NewServer(mux)
	defer srv.Close()

	m.CallStarted()
	m.CallStarted()
	m.VoicemailStarted()

	resp, err := srv.Client().Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status = %d, want 200 — full is not unhealthy", resp.StatusCode)
	}
	var snap HealthSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if !snap.Status {
		t.Error("status = false while alive")
	}
	if snap.Accepting {
		t.Error("accepting = true at the ceiling")
	}
	if snap.Calls.OnGPU != 2 || snap.Calls.Voicemail != 1 || snap.Calls.Total != 3 {
		t.Errorf("calls = %+v", snap.Calls)
	}
	if snap.Capacity.Headroom != 0 {
		t.Errorf("headroom = %v, want 0", snap.Capacity.Headroom)
	}
}
