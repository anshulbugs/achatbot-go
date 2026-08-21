package rexa

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type fakeUpstream struct {
	calls int
	got   map[string]any
	reply string
	code  int
}

func (f *fakeUpstream) Forward(_ context.Context, body []byte) (int, []byte, error) {
	f.calls++
	f.got = map[string]any{}
	_ = json.Unmarshal(body, &f.got)
	code := f.code
	if code == 0 {
		code = http.StatusOK
	}
	reply := f.reply
	if reply == "" {
		reply = `{"choices":[{"message":{"content":"hi"}}]}`
	}
	return code, []byte(reply), nil
}

func newTestProxy(up ChatCompleter, m *Metrics, key string, capTokens int) *Server {
	s := NewServer("secret", NewMemoryNonceStore(), nil, m)
	s.SetLLMProxy(NewLLMProxy(up, newTestGate(m, 2, 60*time.Second), key, "test-model", capTokens))
	return s
}

func postChat(s *Server, body, auth string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	w := httptest.NewRecorder()
	s.handleChatCompletions(w, r)
	return w
}

const okBody = `{"messages":[{"role":"user","content":"hello"}]}`

// The key guards a GPU that live calls depend on. Everything about this test is
// the point of the endpoint.
func TestLLMProxyRejectsBadOrMissingKeys(t *testing.T) {
	up := &fakeUpstream{}
	s := newTestProxy(up, NewMetrics(10), "right-key", 0)

	for _, tc := range []struct{ name, auth string }{
		{"no header", ""},
		{"wrong key", "Bearer wrong-key"},
		{"not bearer", "Basic right-key"},
		{"bare key", "right-key"},
		{"empty bearer", "Bearer "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := postChat(s, okBody, tc.auth)
			if w.Code == http.StatusOK {
				t.Errorf("accepted %q", tc.auth)
			}
			if up.calls != 0 {
				t.Error("an unauthenticated request reached the model")
			}
		})
	}
}

func TestLLMProxyAcceptsTheRightKey(t *testing.T) {
	up := &fakeUpstream{}
	s := newTestProxy(up, NewMetrics(10), "right-key", 0)
	w := postChat(s, okBody, "Bearer right-key")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", w.Code, w.Body.String())
	}
	if up.calls != 1 {
		t.Fatalf("upstream calls = %d, want 1", up.calls)
	}
	// The body must stay exactly what an OpenAI client expects.
	var out map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("response is not JSON: %v", err)
	}
	if _, ok := out["choices"]; !ok {
		t.Error("the upstream body must be passed through unmodified")
	}
}

// Generation length is the slot-holding cost, so the cap must hold whatever the
// caller asks for.
func TestLLMProxyCapsMaxTokens(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want float64
	}{
		{"over the cap is clamped", `{"messages":[],"max_tokens":999999}`, 100},
		{"under the cap is kept", `{"messages":[],"max_tokens":50}`, 50},
		{"absent gets the cap", `{"messages":[]}`, 100},
		{"zero gets the cap", `{"messages":[],"max_tokens":0}`, 100},
		{"negative gets the cap", `{"messages":[],"max_tokens":-5}`, 100},
	} {
		t.Run(tc.name, func(t *testing.T) {
			up := &fakeUpstream{}
			s := newTestProxy(up, NewMetrics(10), "k", 100)
			if w := postChat(s, tc.body, "Bearer k"); w.Code != http.StatusOK {
				t.Fatalf("status %d: %s", w.Code, w.Body.String())
			}
			if got := up.got["max_tokens"]; got != tc.want {
				t.Errorf("max_tokens forwarded = %v, want %v", got, tc.want)
			}
		})
	}
}

// Streaming would hold a background slot for the whole generation and make the
// gate's accounting a lie, so it must be refused rather than silently ignored.
func TestLLMProxyRefusesStreaming(t *testing.T) {
	up := &fakeUpstream{}
	s := newTestProxy(up, NewMetrics(10), "k", 0)
	w := postChat(s, `{"messages":[],"stream":true}`, "Bearer k")
	if w.Code == http.StatusOK {
		t.Error("streaming must be refused")
	}
	if up.calls != 0 {
		t.Error("a streaming request must not reach the model")
	}
	if !strings.Contains(w.Body.String(), "streaming") {
		t.Errorf("the error should say why: %s", w.Body.String())
	}
}

// The long tail of OpenAI options must survive: this endpoint has no opinion
// about them and dropping them silently would change what the caller asked for.
func TestLLMProxyPreservesUnknownFields(t *testing.T) {
	up := &fakeUpstream{}
	s := newTestProxy(up, NewMetrics(10), "k", 0)
	body := `{"messages":[],"temperature":0.2,"top_p":0.9,"seed":7,"response_format":{"type":"json_object"}}`
	if w := postChat(s, body, "Bearer k"); w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	for _, k := range []string{"temperature", "top_p", "seed", "response_format"} {
		if _, ok := up.got[k]; !ok {
			t.Errorf("%s was dropped on the way to the model", k)
		}
	}
}

func TestLLMProxyFillsInTheServedModel(t *testing.T) {
	up := &fakeUpstream{}
	s := newTestProxy(up, NewMetrics(10), "k", 0)
	postChat(s, `{"messages":[]}`, "Bearer k")
	if up.got["model"] != "test-model" {
		t.Errorf("model = %v, want the configured default", up.got["model"])
	}
}

func TestLLMProxyRequiresMessages(t *testing.T) {
	up := &fakeUpstream{}
	s := newTestProxy(up, NewMetrics(10), "k", 0)
	if w := postChat(s, `{"temperature":0.5}`, "Bearer k"); w.Code == http.StatusOK {
		t.Error("a body with no messages must be refused")
	}
	if up.calls != 0 {
		t.Error("an invalid request reached the model")
	}
}

// An upstream error carries the model's own explanation, which is the
// difference between "context length exceeded" and a bare 500.
func TestLLMProxyPassesUpstreamErrorsThrough(t *testing.T) {
	up := &fakeUpstream{code: http.StatusBadRequest, reply: `{"error":"context length exceeded"}`}
	s := newTestProxy(up, NewMetrics(10), "k", 0)
	w := postChat(s, okBody, "Bearer k")
	if w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want the upstream's 400", w.Code)
	}
	if !strings.Contains(w.Body.String(), "context length") {
		t.Errorf("the model's reason was lost: %s", w.Body.String())
	}
}

// The gate's accounting must not pollute the OpenAI body.
func TestLLMProxyReportsWaitingInHeaders(t *testing.T) {
	m := NewMetrics(10)
	for i := 0; i < 6; i++ { // past half the ceiling, so the gate waits
		m.TryReserve(sessionN(i))
	}
	up := &fakeUpstream{}
	s := NewServer("secret", NewMemoryNonceStore(), nil, m)
	s.SetLLMProxy(NewLLMProxy(up, newTestGate(m, 2, 60*time.Second), "k", "test-model", 0))

	w := postChat(s, okBody, "Bearer k")
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if w.Header().Get("X-Agent-Deferred") != "true" {
		t.Error("a run that waited out the deadline must say so in a header")
	}
	if w.Header().Get("X-Agent-Waited-Ms") == "" || w.Header().Get("X-Agent-Waited-Ms") == "0" {
		t.Errorf("X-Agent-Waited-Ms = %q, want the real wait", w.Header().Get("X-Agent-Waited-Ms"))
	}
	if strings.Contains(w.Body.String(), "deferred") {
		t.Error("the body must stay a plain OpenAI response")
	}
}
