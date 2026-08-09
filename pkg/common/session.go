package common

import (
	"strings"
	"sync"
	"time"
)

// LLMObserver receives the time a caller waited for the first token of one
// reply, with turn numbered from 1.
//
// The turn number is the point of it. Turn 1 is the only turn that pays a cold
// prefill, so it is where KV-cache pressure shows up first and by far the
// largest margin; pooled with warm turns it disappears into the average.
type LLMObserver func(ttft time.Duration, turn int)

// Session represents a chat session with chat history
type Session struct {
	chatRound   int
	sessionID   string
	chatHistory *ChatHistory

	// llmMu guards the observer and turn counter. The observer is installed on
	// the goroutine that builds the session and called on the pipeline
	// goroutine that runs the LLM.
	llmMu       sync.Mutex
	llmObserver LLMObserver
	llmTurns    int
	// agentTurn accumulates the current reply as it is handed to speech;
	// agentObserver receives each completed or interrupted turn.
	agentTurn     strings.Builder
	agentObserver AgentTurnObserver
	// spokenSecs is audio actually sent for the current turn; charsPerSec
	// converts it back to a position in the text.
	spokenSecs  float64
	charsPerSec float64
	// funcs are tools scoped to THIS session, checked before the global
	// registry.
	//
	// The global registry maps a name to one implementation for the whole
	// process, and a tool invocation carries only its arguments — no session,
	// no call id. That is fine for stateless tools like web search, but a tool
	// that acts on the call it was invoked from (transferring it, ending it)
	// has no way to know which of the calls in flight it belongs to. Binding
	// the tool per session lets the implementation be a closure over that
	// call's own state.
	funcs map[string]IFunction
}

// RegisterFunc binds a tool to this session only. It shadows a global
// registration of the same name, so a per-call implementation always wins over
// a process-wide one.
func (s *Session) RegisterFunc(name string, fn IFunction) {
	if s.funcs == nil {
		s.funcs = make(map[string]IFunction, 1)
	}
	s.funcs[name] = fn
}

// Func returns this session's implementation of name, or nil when the session
// has none and the caller should fall back to the global registry.
func (s *Session) Func(name string) IFunction {
	if s == nil || s.funcs == nil {
		return nil
	}
	return s.funcs[name]
}

// ToolCalls returns the OpenAI tool schemas for this session's own tools, so
// they can be advertised to the model alongside the global ones.
func (s *Session) ToolCalls() []map[string]any {
	if s == nil || len(s.funcs) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(s.funcs))
	for _, fn := range s.funcs {
		out = append(out, fn.GetToolCall())
	}
	return out
}

// AgentTurnObserver receives the agent's side of the conversation as it is
// handed to speech, one completed turn at a time.
type AgentTurnObserver func(text string)

// SetAgentTurnObserver installs the callback that records what the agent said.
//
// Separate from the chat-history observer for one reason: chat history holds
// what the MODEL PRODUCED, and after an interruption that is not what the
// caller HEARD. The model runs ahead of the voice, so a turn cut off after one
// sentence still leaves three in the history, and reporting those as spoken
// puts words in the agent's mouth.
func (s *Session) SetAgentTurnObserver(fn AgentTurnObserver) {
	if s == nil {
		return
	}
	s.llmMu.Lock()
	s.agentObserver = fn
	s.llmMu.Unlock()
}

// RecordAgentChunk accumulates one piece of the agent's current turn, at the
// point it is handed downstream to be spoken.
func (s *Session) RecordAgentChunk(text string) {
	if s == nil || text == "" {
		return
	}
	s.llmMu.Lock()
	s.agentTurn.WriteString(text)
	s.llmMu.Unlock()
}

// RecordSpokenAudio adds the duration of audio actually sent to the caller for
// the current turn.
//
// This is the only honest measure of what was said. Text is generated far
// faster than it is spoken — the model finishes a reply in a second or two
// while the voice takes ten — so "what the model produced" and "what the caller
// heard" diverge the moment anyone interrupts.
func (s *Session) RecordSpokenAudio(d time.Duration) {
	if s == nil || d <= 0 {
		return
	}
	s.llmMu.Lock()
	s.spokenSecs += d.Seconds()
	s.llmMu.Unlock()
}

// SetSpeakingRate calibrates characters per second for the voice in use, used
// to cut an interrupted turn at the point speech reached.
func (s *Session) SetSpeakingRate(charsPerSec float64) {
	if s == nil || charsPerSec <= 0 {
		return
	}
	s.llmMu.Lock()
	s.charsPerSec = charsPerSec
	s.llmMu.Unlock()
}

// FlushAgentTurn reports the turn accumulated so far and starts a new one.
//
// interrupted decides whether the text is trimmed to what was actually spoken.
// A turn that ends normally is reported in full — the audio is still playing
// but all of it will be heard, so trimming it would under-report. A turn cut
// short is trimmed, because the rest was generated and never reached anyone.
func (s *Session) FlushAgentTurn(interrupted bool) {
	if s == nil {
		return
	}
	s.llmMu.Lock()
	text := strings.TrimSpace(s.agentTurn.String())
	spoken, rate := s.spokenSecs, s.charsPerSec
	s.agentTurn.Reset()
	s.spokenSecs = 0
	fn := s.agentObserver
	s.llmMu.Unlock()

	if interrupted {
		text = trimToSpoken(text, spoken, rate)
	}
	if fn != nil && text != "" {
		fn(text)
	}
}

// trimToSpoken cuts text at the word boundary nearest to what the caller heard.
//
// Deliberately generous by one word: over-trimming drops something that WAS
// said, which is worse than including one word that was not — a transcript
// missing the caller's answer reads as a different conversation.
func trimToSpoken(text string, spokenSecs, charsPerSec float64) string {
	if text == "" || spokenSecs <= 0 || charsPerSec <= 0 {
		return ""
	}
	limit := int(spokenSecs * charsPerSec)
	if limit >= len(text) {
		return text
	}
	cut := strings.LastIndexByte(text[:limit], ' ')
	if cut <= 0 {
		// Interrupted inside the first word: nothing meaningful was heard.
		return ""
	}
	// Include the word the cut landed in — the caller almost certainly heard
	// its beginning, and half a word is not something to report either way.
	if next := strings.IndexByte(text[cut+1:], ' '); next > 0 {
		cut = cut + 1 + next
	} else {
		cut = len(text)
	}
	return strings.TrimSpace(text[:cut])
}

// SetLLMObserver installs the callback for per-turn LLM timing. nil disables
// it, which is the default: a session that nobody is measuring costs nothing.
func (s *Session) SetLLMObserver(fn LLMObserver) {
	if s == nil {
		return
	}
	s.llmMu.Lock()
	s.llmObserver = fn
	s.llmMu.Unlock()
}

// ObserveLLMTTFT reports one turn's time to first token, counting the turn.
//
// Called once per conversational turn — not once per HTTP request. A turn that
// runs a tool makes several requests before any token reaches the caller, and
// the caller waits for all of them, so timing the requests individually would
// report a turn as fast when the person on the phone heard a long silence.
func (s *Session) ObserveLLMTTFT(d time.Duration) {
	if s == nil {
		return
	}
	s.llmMu.Lock()
	s.llmTurns++
	turn := s.llmTurns
	fn := s.llmObserver
	s.llmMu.Unlock()
	if fn != nil {
		fn(d, turn)
	}
}

// NewSession creates a new Session instance
func NewSession(sessionID string, chatHistorySize *int) *Session {
	// Create a new ChatHistory with the given size and no initial messages
	chatHistory := NewChatHistory(chatHistorySize, nil, nil)

	return &Session{
		chatRound:   0,
		sessionID:   sessionID,
		chatHistory: chatHistory,
		// A reasonable default for kokoro at normal speed; callers that know
		// the voice and speed override it with SetSpeakingRate.
		charsPerSec: 14,
	}
}

// InitChatMessage initializes the chat with a message
func (s *Session) InitChatMessage(initChatMessage map[string]any) {
	s.chatHistory.Init(initChatMessage)
}

// Reset resets the session chat round and clears chat history
func (s *Session) Reset() {
	s.chatRound = 0
	s.chatHistory.Clear()
	// Also reset the initial chat message to fully clear the history
	s.chatHistory.Init(nil)
}

// SetChatHistorySize sets the size limit of the chat history
func (s *Session) SetChatHistorySize(chatHistorySize *int) {
	s.chatHistory.SetSize(chatHistorySize)
}

// SetSessionID sets the session ID
func (s *Session) SetSessionID(sessionID string) {
	s.sessionID = sessionID
}

// IncrementChatRound increments the chat round counter
func (s *Session) IncrementChatRound() {
	s.chatRound++
}

// GetChatRound returns the current chat round
func (s *Session) GetChatRound() int {
	return s.chatRound
}

// GetSessionID returns the session ID
func (s *Session) GetSessionID() string {
	return s.sessionID
}

// GetChatHistory returns the chat history
func (s *Session) GetChatHistory() *ChatHistory {
	return s.chatHistory
}

func (s *Session) Copy() *Session {
	cpSsession := NewSession(s.sessionID, nil)
	cpSsession.chatRound = s.chatRound
	cpSsession.sessionID = s.sessionID
	cpSsession.chatHistory = s.chatHistory.Copy()

	return cpSsession
}
