package rexa

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// recorder captures what actually went upstream.
type recorder struct {
	key  string
	body []byte
}

func (r *recorder) RoundTrip(req *http.Request) (*http.Response, error) {
	r.key = req.Header.Get(PrefixKeyHeader)
	if req.Body != nil {
		r.body, _ = io.ReadAll(req.Body)
	}
	return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("ok")),
		Header: http.Header{}, Request: req}, nil
}

func chatBody(t *testing.T, system string, user string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model": "m",
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func roundTrip(t *testing.T, body []byte) *recorder {
	t.Helper()
	rec := &recorder{}
	req, err := http.NewRequest(http.MethodPost, "http://lb/v1/chat/completions",
		bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := PrefixRouter(rec).RoundTrip(req); err != nil {
		t.Fatal(err)
	}
	return rec
}

func TestSameCampaignRoutesTogetherDespiteDifferentContacts(t *testing.T) {
	// This is the entire point: two calls in one campaign differ only in the
	// contact block at the end, and must land on the same replica so the second
	// finds the campaign prefix already cached.
	campaign := strings.Repeat("You are a recruiter for client 7. ", 200)
	a := roundTrip(t, chatBody(t, campaign+"Candidate Alice, id 1001.", "hi"))
	b := roundTrip(t, chatBody(t, campaign+"Candidate Bob, id 1002.", "hi"))

	if a.key == "" {
		t.Fatal("no routing key was set")
	}
	if a.key != b.key {
		t.Fatalf("same campaign routed two ways (%s vs %s) — each replica would "+
			"prefill the campaign separately, which is the bug this fixes",
			a.key, b.key)
	}
}

func TestDifferentCampaignsRouteApart(t *testing.T) {
	// Not a correctness requirement — a hash collision would only cost cache —
	// but if everything hashed the same the fleet would collapse onto one
	// replica, so it is worth knowing the key actually varies.
	a := roundTrip(t, chatBody(t, "You are a recruiter for client 7. "+strings.Repeat("x ", 500), "hi"))
	b := roundTrip(t, chatBody(t, "You are a recruiter for client 9. "+strings.Repeat("x ", 500), "hi"))
	if a.key == b.key {
		t.Fatalf("two different campaigns produced the same key %s", a.key)
	}
}

func TestKeyIgnoresTailBeyondTheSharedRegion(t *testing.T) {
	// A prompt longer than prefixKeyBytes must route on its head alone, or a
	// growing per-contact tail would move the call between replicas.
	head := strings.Repeat("a", prefixKeyBytes+10)
	a := roundTrip(t, chatBody(t, head+"tail one", "hi"))
	b := roundTrip(t, chatBody(t, head+"a completely different tail", "hi"))
	if a.key != b.key {
		t.Fatal("the tail past prefixKeyBytes changed the route")
	}
}

func TestBodyReachesUpstreamIntact(t *testing.T) {
	// The router reads the body to find the prompt. If it forgets to put it
	// back, every LLM request goes out empty — an outage caused by a routing
	// optimisation, which is exactly the wrong trade.
	body := chatBody(t, "system prompt", "hello there")
	rec := roundTrip(t, body)
	if !bytes.Equal(rec.body, body) {
		t.Fatalf("upstream got %d bytes, sent %d", len(rec.body), len(body))
	}
}

func TestUnparseableBodyStillSendsTheRequest(t *testing.T) {
	// Routing is an optimisation. Losing the key costs cache locality; failing
	// the request costs the call.
	rec := roundTrip(t, []byte("this is not json"))
	if rec.key != "" {
		t.Fatalf("keyed an unparseable body as %q", rec.key)
	}
	if string(rec.body) != "this is not json" {
		t.Fatal("body was not forwarded")
	}
}

func TestTurnsOfOneCallShareAKey(t *testing.T) {
	// Turn 8 must land where turn 1 built its history. Same system prompt,
	// longer conversation.
	system := strings.Repeat("campaign preamble ", 300)
	a := roundTrip(t, chatBody(t, system, "first thing I said"))
	b := roundTrip(t, chatBody(t, system, "something said much later in the call"))
	if a.key != b.key {
		t.Fatal("two turns of one call routed to different replicas")
	}
}
