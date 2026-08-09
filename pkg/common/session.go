package common

import (
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
