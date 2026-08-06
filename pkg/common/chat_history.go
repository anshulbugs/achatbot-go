package common

import (
	"maps"
	"encoding/json"
)

// ChatHistory buffers the local chat history with limit size to avoid LLM context too long
// - if size is nil, no limit
// - if size < 0, no history
//
// TODO: use kv store history like mem0
type ChatHistory struct {
	size            *int
	initChatMessage map[string]any
	initChatTools   map[string]any
	buffer          []map[string]any
	// observer, when set, is called with every appended item BEFORE the
	// buffer is trimmed. This buffer is a rolling LLM context window, not a
	// log: Append discards the oldest pair once it reaches 2*(size+1), so
	// reading `buffer` at the end of a call yields only the last few turns.
	// Anything that needs the whole conversation — a call transcript, an
	// audit trail — must observe appends as they happen rather than reading
	// the buffer afterwards, or it will silently report a truncated
	// conversation as a complete one.
	observer func(map[string]any)
}

// SetObserver registers a callback invoked on every Append, before trimming.
//
// Called on the pipeline goroutine, so the callback must not block; it should
// hand off to its own buffer and return. Passing nil clears it.
func (ch *ChatHistory) SetObserver(fn func(map[string]any)) {
	ch.observer = fn
}

// NewChatHistory creates a new ChatHistory instance
func NewChatHistory(size *int, initChatMessage, initChatTools map[string]any) *ChatHistory {
	return &ChatHistory{
		size:            size,
		initChatMessage: initChatMessage,
		initChatTools:   initChatTools,
		buffer:          make([]map[string]any, 0),
	}
}

// SetSize sets the size limit of the chat history
func (ch *ChatHistory) SetSize(size *int) {
	ch.size = size
}

// Clear clears the chat history buffer
func (ch *ChatHistory) Clear() {
	ch.buffer = ch.buffer[:0]
}

// Append adds a new item to the chat history
func (ch *ChatHistory) Append(item map[string]any) {
	if ch.size != nil && *ch.size < 0 {
		return
	}

	// Notify before trimming, so an observer sees every turn even though the
	// buffer only retains the most recent ones.
	if ch.observer != nil {
		ch.observer(item)
	}

	ch.buffer = append(ch.buffer, item)
	if ch.size == nil {
		return
	}

	// Each new step adds a prompt and assistant answer, so we need to keep 2*size items
	if len(ch.buffer) == 2*(*ch.size+1) {
		ch.buffer = ch.buffer[2:]
	}
}

// Pop removes an item from the chat history
func (ch *ChatHistory) Pop(index int) {
	if ch.size != nil && *ch.size < 0 {
		return
	}

	if len(ch.buffer) > 0 {
		if index < 0 {
			index = len(ch.buffer) + index
		}

		if index >= 0 && index < len(ch.buffer) {
			ch.buffer = append(ch.buffer[:index], ch.buffer[index+1:]...)
		}
	}
}

// Init sets the initial chat message
func (ch *ChatHistory) Init(initChatMessage map[string]any) {
	ch.initChatMessage = initChatMessage
}

// GetTools gets the initial chat tools
func (ch *ChatHistory) GetTools() map[string]any {
	return ch.initChatTools
}

// InitTools sets the initial chat tools
func (ch *ChatHistory) InitTools(tools map[string]any) {
	ch.initChatTools = tools
}

// ToListWithoutTools converts the chat history to a list without tools
func (ch *ChatHistory) ToListWithoutTools() []map[string]any {
	result := make([]map[string]any, 0)

	if ch.initChatMessage != nil {
		result = append(result, ch.initChatMessage)
	}

	result = append(result, ch.buffer...)
	return result
}

// ToList converts the chat history to a list
func (ch *ChatHistory) ToList() []map[string]any {
	result := make([]map[string]any, 0)

	if ch.initChatMessage != nil {
		result = append(result, ch.initChatMessage)

		if ch.initChatTools != nil {
			result = append(result, ch.initChatTools)
		}
	}

	result = append(result, ch.buffer...)
	return result
}

// MarshalJSON implements json.Marshaler interface
func (ch ChatHistory) MarshalJSON() ([]byte, error) {
	state := map[string]any{
		"size":              ch.size,
		"init_chat_message": ch.initChatMessage,
		"init_chat_tools":   ch.initChatTools,
		"buffer":            ch.buffer,
	}

	return json.Marshal(state)
}

// UnmarshalJSON implements json.Unmarshaler interface
func (ch *ChatHistory) UnmarshalJSON(data []byte) error {
	var state map[string]any
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}

	// Handle size
	if sizeVal, ok := state["size"]; ok && sizeVal != nil {
		switch s := sizeVal.(type) {
		case float64:
			sizeInt := int(s)
			ch.size = &sizeInt
		case int:
			ch.size = &s
		}
	}

	// Handle initChatMessage
	if msg, ok := state["init_chat_message"].(map[string]any); ok {
		ch.initChatMessage = msg
	}

	// Handle initChatTools
	if tools, ok := state["init_chat_tools"].(map[string]any); ok {
		ch.initChatTools = tools
	}

	// Handle buffer
	if buf, ok := state["buffer"].([]any); ok {
		ch.buffer = make([]map[string]any, len(buf))
		for i, item := range buf {
			if itemMap, ok := item.(map[string]any); ok {
				ch.buffer[i] = itemMap
			}
		}
	}

	return nil
}

func (ch *ChatHistory) Copy() *ChatHistory {
	// Copy size
	var sizeCopy *int
	if ch.size != nil {
		sizeCopy = new(int)
		*sizeCopy = *ch.size
	}

	// Copy initChatMessage
	var initChatMessageCopy map[string]any
	if ch.initChatMessage != nil {
		initChatMessageCopy = make(map[string]any)
		maps.Copy(initChatMessageCopy, ch.initChatMessage)
	}

	// Copy initChatTools
	var initChatToolsCopy map[string]any
	if ch.initChatTools != nil {
		initChatToolsCopy = make(map[string]any)
		maps.Copy(initChatToolsCopy, ch.initChatTools)
	}

	// Copy buffer
	bufferCopy := make([]map[string]any, len(ch.buffer))
	for i, item := range ch.buffer {
		bufferCopy[i] = make(map[string]any)
		maps.Copy(bufferCopy[i], item)
	}

	return &ChatHistory{
		size:            sizeCopy,
		initChatMessage: initChatMessageCopy,
		initChatTools:   initChatToolsCopy,
		buffer:          bufferCopy,
	}
}
