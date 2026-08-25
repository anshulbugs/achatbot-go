package main

import (
	"strings"
	"testing"
	"time"

	"achatbot/pkg/common"
	"achatbot/pkg/config"
	"achatbot/pkg/telnyx"
)

// A browser session has no carrier leg, so it must not be offered a tool that
// hangs one up -- the same rule call_transfer follows.
func TestEndCallToolOnlyRegisteredForCarrierCalls(t *testing.T) {
	cfg = &config.Config{}
	size := 2

	phone := common.NewSession("s1", &size)
	registerEndCallTool(phone, testCall(""), "call-1", nil)
	if phone.Func("end_call") == nil {
		t.Error("tool not registered on a carrier call")
	}

	browser := common.NewSession("s2", &size)
	registerEndCallTool(browser, nil, "", nil)
	if browser.Func("end_call") != nil {
		t.Error("tool registered without a call to end")
	}
}

// Registering without advertising is a silent no-op: the model never learns the
// tool exists, and the symptom looks like a model that refuses to hang up.
func TestEndCallToolIsAdvertised(t *testing.T) {
	cfg = &config.Config{}
	size := 2
	s := common.NewSession("s", &size)
	registerEndCallTool(s, testCall(""), "c", nil)

	schemas := s.ToolCalls()
	if len(schemas) != 1 {
		t.Fatalf("ToolCalls() returned %d schemas, want 1", len(schemas))
	}
	fn, _ := schemas[0]["function"].(map[string]any)
	if fn == nil || fn["name"] != "end_call" {
		t.Fatalf("advertised schema is not end_call: %v", schemas[0])
	}
	// The description has to say the sentence is not the action. call_transfer
	// was missed twice on one real call because the model said "I'm just
	// getting you connected now" and treated that as the transfer; a goodbye
	// invites that mistake far more strongly.
	desc, _ := fn["description"].(string)
	if !strings.Contains(desc, "does NOT end the call") {
		t.Error("description does not tell the model that saying goodbye is not hanging up")
	}
}

// The second invocation must not schedule a second hangup.
func TestEndCallIsOnlyActedOnOnce(t *testing.T) {
	cfg = &config.Config{}
	tool := &endCallTool{callID: "c", client: nil, ser: nil}

	first, err := tool.Execute(map[string]any{"reason": "done"})
	if err != nil {
		t.Fatalf("first invocation errored: %v", err)
	}
	second, err := tool.Execute(map[string]any{"reason": "done again"})
	if err != nil {
		t.Fatalf("second invocation errored: %v", err)
	}
	if !strings.Contains(second, "already ending") {
		t.Errorf("second invocation was acted on: %q", second)
	}
	// Both results must tell the model to stop talking: anything it says now is
	// spoken over a line that is about to close.
	for _, r := range []string{first, second} {
		if !strings.Contains(r, "Say nothing further") {
			t.Errorf("tool result does not stop the model: %q", r)
		}
	}
}

// THE POINT OF THE WHOLE TOOL: the goodbye must finish before the line drops.
// Hanging up inline would cut it off mid-word, which is precisely what makes a
// call sound broken to the person on it.
func TestEndCallWaitsForTheClosingLineToPlay(t *testing.T) {
	ser := telnyx.NewSerializer(16000)
	// A closing line still playing out at the carrier.
	ser.NoteAnnouncement(600 * time.Millisecond)

	tool := &endCallTool{callID: "c", client: nil, ser: ser}
	started := time.Now()
	tool.hangUpWhenQuiet()
	waited := time.Since(started)

	// It must outlast the playback, plus the settle window and the grace.
	min := 600*time.Millisecond + endCallSettle
	if waited < min {
		t.Errorf("hung up after %s, before the closing line finished (need >= %s)", waited, min)
	}
	if waited > endCallMaxWait {
		t.Errorf("waited %s, past the cap %s", waited, endCallMaxWait)
	}
}

// A pipeline that never goes quiet must not hold the call open forever: a tool
// the model invoked that then does nothing is the defect this file removes.
func TestEndCallGivesUpWaitingEventually(t *testing.T) {
	ser := telnyx.NewSerializer(16000)
	ser.NoteAnnouncement(endCallMaxWait + 30*time.Second)

	tool := &endCallTool{callID: "c", client: nil, ser: ser}
	started := time.Now()
	tool.hangUpWhenQuiet()
	if waited := time.Since(started); waited > endCallMaxWait+2*time.Second {
		t.Errorf("waited %s; the cap %s was not enforced", waited, endCallMaxWait)
	}
}
