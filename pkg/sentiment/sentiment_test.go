package sentiment

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func serve(t *testing.T, reply string, status int) *Classifier {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": reply}}},
		})
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "test-model")
}

func TestVerdictsMapToThePlatformVocabulary(t *testing.T) {
	// The platform's Zod enum rejects anything outside these three, and a
	// rejected event is a silently missed alert — so the mapping is the part
	// worth pinning.
	cases := map[string]string{
		"wants_human":         WantsHuman,
		"  WANTS_HUMAN\n":     WantsHuman,
		"highly_interested.":  HighlyInterested,
		"user_annoyed":        UserAnnoyed,
		"none":                None,
		"":                    None,
		"the caller seems ok": None,
	}
	for reply, want := range cases {
		if got := parseVerdict(reply); got != want {
			t.Errorf("parseVerdict(%q) = %q, want %q", reply, got, want)
		}
	}
}

func TestClassifierFailsClosed(t *testing.T) {
	// An alert that fires because a 0.6B model returned a 500 is an alert
	// people learn to ignore.
	c := serve(t, "", http.StatusInternalServerError)
	if got := c.Classify(context.Background(), "", "I want a human"); got != None {
		t.Fatalf("a failing endpoint produced %q, want %q", got, None)
	}
}

func TestEmptyCallerTextIsNotClassified(t *testing.T) {
	// Costs a request per turn otherwise, for a turn with nothing in it.
	c := New("http://127.0.0.1:1", "unused") // would fail if it dialled
	if got := c.Classify(context.Background(), "hi", "   "); got != None {
		t.Fatalf("blank text produced %q", got)
	}
}

func TestClassifierSendsPriorAgentTurn(t *testing.T) {
	// Without it, "no, a person" reads as a fragment rather than an answer.
	var body []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buf := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(buf)
		body = buf
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": "none"}}},
		})
	}))
	defer srv.Close()

	New(srv.URL, "m").Classify(context.Background(), "Shall I go on?", "No, a person")
	if !strings.Contains(string(body), "Shall I go on?") {
		t.Fatal("the agent's prior turn was not sent as context")
	}
}

func TestTrackerReportsChangesOnly(t *testing.T) {
	var tr Tracker

	if !tr.Observe(UserAnnoyed) {
		t.Fatal("first real verdict was not reported")
	}
	// A caller who is annoyed stays annoyed. Re-alerting every turn is how an
	// alert becomes noise.
	if tr.Observe(UserAnnoyed) {
		t.Fatal("repeat of the same verdict was reported again")
	}
	if !tr.Observe(WantsHuman) {
		t.Fatal("a change to a different verdict was not reported")
	}
	// "Stopped being annoyed" is not something anyone acts on, and sending it
	// would clear an operator's alert while the call is still going badly.
	if tr.Observe(None) {
		t.Fatal("a return to none was reported")
	}
	if tr.Current() != WantsHuman {
		t.Fatalf("current = %q after a none, want it unchanged at %q", tr.Current(), WantsHuman)
	}
}

func TestTrackerStartsSilent(t *testing.T) {
	var tr Tracker
	if tr.Observe(None) {
		t.Fatal("none on a fresh call was reported")
	}
	if tr.Current() != "" {
		t.Fatalf("current = %q on a fresh tracker", tr.Current())
	}
}
