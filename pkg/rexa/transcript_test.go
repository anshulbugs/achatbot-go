package rexa

import (
	"sync"
	"testing"
	"time"
)

func TestTranscriptRecordsTurnsInOrder(t *testing.T) {
	start := time.Now()
	tr := NewTranscript(start)
	tr.Add("assistant", "Hi, this is a quick call.")
	tr.Add("user", "Hello?")
	tr.Add("assistant", "Goodbye.")

	turns := tr.Turns()
	if len(turns) != 3 {
		t.Fatalf("len = %d, want 3", len(turns))
	}
	want := []struct{ role, content string }{
		{RoleAgent, "Hi, this is a quick call."},
		{RoleUser, "Hello?"},
		{RoleAgent, "Goodbye."},
	}
	for i, w := range want {
		if turns[i].Role != w.role || turns[i].Content != w.content {
			t.Errorf("turn %d = %+v, want %s/%q", i, turns[i], w.role, w.content)
		}
		if turns[i].T == nil {
			t.Errorf("turn %d has no timestamp", i)
		}
	}
}

// "assistant" is what the LLM emits; the platform's canonical name is "agent".
func TestRoleNormalisation(t *testing.T) {
	cases := map[string]string{
		"assistant": RoleAgent, "agent": RoleAgent, "bot": RoleAgent,
		"user": RoleUser, "human": RoleUser,
	}
	for in, want := range cases {
		tr := NewTranscript(time.Now())
		tr.Add(in, "x")
		turns := tr.Turns()
		if len(turns) != 1 || turns[0].Role != want {
			t.Errorf("Add(%q) produced %+v, want role %q", in, turns, want)
		}
	}
}

// System prompts and tool results are not part of the spoken conversation and
// must not appear in a transcript shown to a tenant.
func TestNonConversationalRolesDropped(t *testing.T) {
	tr := NewTranscript(time.Now())
	tr.Add("system", "You are a helpful assistant with these secret rules...")
	tr.Add("tool", "{\"status\":\"ok\"}")
	tr.Add("mystery", "???")
	if n := tr.Len(); n != 0 {
		t.Errorf("len = %d, want 0 — non-conversational turns leaked into the transcript", n)
	}
}

func TestEmptyContentDropped(t *testing.T) {
	tr := NewTranscript(time.Now())
	tr.Add("assistant", "")
	if n := tr.Len(); n != 0 {
		t.Errorf("len = %d, want 0", n)
	}
}

// The greeting is the first turn at t=0. That zero must survive to the wire,
// or the transcript loses its anchor.
func TestFirstTurnTimestampIsZeroAndSurvives(t *testing.T) {
	start := time.Now()
	tr := NewTranscript(start)
	tr.now = func() time.Time { return start }
	tr.Add("assistant", "hello")

	turns := tr.Turns()
	if turns[0].T == nil {
		t.Fatal("timestamp is nil")
	}
	if *turns[0].T != 0 {
		t.Errorf("t = %v, want 0", *turns[0].T)
	}
}

func TestTimestampsAreSecondsSinceStart(t *testing.T) {
	start := time.Now()
	tr := NewTranscript(start)
	tr.now = func() time.Time { return start.Add(2500 * time.Millisecond) }
	tr.Add("user", "hi")
	if got := *tr.Turns()[0].T; got < 2.4 || got > 2.6 {
		t.Errorf("t = %v, want ~2.5", got)
	}
}

// A clock that goes backwards (NTP correction mid-call) must not produce a
// negative timestamp, which would fail the platform's validation.
func TestNegativeElapsedClampedToZero(t *testing.T) {
	start := time.Now()
	tr := NewTranscript(start)
	tr.now = func() time.Time { return start.Add(-5 * time.Second) }
	tr.Add("user", "hi")
	if got := *tr.Turns()[0].T; got != 0 {
		t.Errorf("t = %v, want 0", got)
	}
}

// This is the bug the collector exists to prevent: ChatHistory trims to a
// rolling window, so reading it at end-of-call yields a truncated conversation
// that looks complete. An observer must see every turn.
func TestRetainsTurnsBeyondChatHistoryWindow(t *testing.T) {
	tr := NewTranscript(time.Now())
	for i := 0; i < 100; i++ {
		tr.Add("user", "question")
		tr.Add("assistant", "answer")
	}
	if n := tr.Len(); n != 200 {
		t.Errorf("len = %d, want 200 — turns were lost", n)
	}
	// The opening must still be there; it is where the caller states intent.
	if first := tr.Turns()[0]; first.Role != RoleUser || first.Content != "question" {
		t.Errorf("first turn = %+v — the opening was dropped", first)
	}
}

// A pathological call must not grow the report past the platform's 128 KB
// limit, because a rejected report loses the entire conversation.
func TestRetentionCapDropsTheMiddleNotTheEnds(t *testing.T) {
	tr := NewTranscript(time.Now())
	tr.Add("user", "FIRST")
	for i := 0; i < MaxTranscriptTurns+500; i++ {
		tr.Add("assistant", "filler")
	}
	tr.Add("user", "LAST")

	turns := tr.Turns()
	if len(turns) > MaxTranscriptTurns {
		t.Errorf("len = %d, exceeds cap %d", len(turns), MaxTranscriptTurns)
	}
	if turns[0].Content != "FIRST" {
		t.Errorf("first turn = %q, want FIRST — the opening should be kept", turns[0].Content)
	}
	if last := turns[len(turns)-1]; last.Content != "LAST" {
		t.Errorf("last turn = %q, want LAST — the outcome should be kept", last.Content)
	}
}

// Turns arrive on the pipeline goroutine while the report may be built from
// the carrier's hangup goroutine.
func TestConcurrentAddAndRead(t *testing.T) {
	tr := NewTranscript(time.Now())
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				tr.Add("user", "x")
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		for j := 0; j < 100; j++ {
			_ = tr.Turns()
		}
	}()
	wg.Wait()
	if n := tr.Len(); n != 400 {
		t.Errorf("len = %d, want 400", n)
	}
}

// The chat-history observer records the CALLER only, and survives a malformed
// message rather than panicking mid-call.
//
// Agent turns deliberately do not come from here: chat history holds what the
// model produced, and after an interruption that is more than the caller heard.
func TestObserveChatHistoryRecordsCallerOnly(t *testing.T) {
	tr := NewTranscript(time.Now())
	obs := tr.ObserveChatHistory()
	obs(map[string]any{"role": "user", "content": "hello"})
	obs(map[string]any{"role": "assistant", "content": "generated, maybe not spoken"})
	obs(map[string]any{"role": "assistant"})             // tool-call turn, no text
	obs(map[string]any{"content": "orphaned"})           // no role
	obs(map[string]any{"role": 42, "content": []int{1}}) // wrong types

	turns := tr.Turns()
	if len(turns) != 1 {
		t.Fatalf("len = %d, want 1 (caller only): %+v", len(turns), turns)
	}
	if turns[0].Role != RoleUser || turns[0].Content != "hello" {
		t.Errorf("turns = %+v", turns)
	}
}

// The agent side arrives through its own observer, fed as text is handed to
// speech and closed off at an interruption — so the report says what the caller
// heard rather than what the model wrote.
func TestObserveAgentTurns(t *testing.T) {
	tr := NewTranscript(time.Now())
	chat, agent := tr.ObserveChatHistory(), tr.ObserveAgentTurns()

	agent("Hello, how can I help?")
	chat(map[string]any{"role": "user", "content": "I have a question"})
	agent("Of course") // cut short by the caller

	turns := tr.Turns()
	if len(turns) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(turns), turns)
	}
	if turns[0].Role != RoleAgent || turns[1].Role != RoleUser || turns[2].Role != RoleAgent {
		t.Fatalf("roles = %v/%v/%v", turns[0].Role, turns[1].Role, turns[2].Role)
	}
	if turns[2].Content != "Of course" {
		t.Errorf("interrupted turn = %q, want only what was spoken", turns[2].Content)
	}
}
