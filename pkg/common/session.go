package common

// Session represents a chat session with chat history
type Session struct {
	chatRound   int
	sessionID   string
	chatHistory *ChatHistory
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
