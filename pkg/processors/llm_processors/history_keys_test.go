package llm_processors

import (
	"testing"

	"github.com/go-viper/mapstructure/v2"
	"github.com/openai/openai-go/v3"

	"achatbot/pkg/types"
)

// A real call's end-of-call report came back with five caller turns and not one
// from the agent. Two silent mismatches, either of which alone was enough:
// mapstructure names keys after struct fields ("Role"), and openai-go types
// Role as constant.Assistant rather than string, so `item["role"].(string)`
// fails even when the key is right.
func TestAssistantTurnsUseTheSameKeysAsUserTurns(t *testing.T) {
	msg := types.Message{ChatCompletionMessage: openai.ChatCompletionMessage{
		Role: "assistant", Content: "Hello, how can I help?",
	}}
	decoded := map[string]any{}
	if err := mapstructure.Decode(msg, &decoded); err != nil {
		t.Fatal(err)
	}
	got := normaliseHistoryItem(decoded)

	if role, _ := got["role"].(string); role != "assistant" {
		t.Fatalf("role = %q, want %q — the transcript observer reads this key", role, "assistant")
	}
	if content, _ := got["content"].(string); content != "Hello, how can I help?" {
		t.Fatalf("content = %q, want the reply text", content)
	}
}

func TestLowerKeysKeepsEveryField(t *testing.T) {
	// Tool calls and ids ride in the same map and are fed back to the model on
	// the next turn, so dropping one would break tool use rather than just the
	// transcript.
	in := map[string]any{"Role": "assistant", "ToolCallID": "call_1", "ToolCalls": []any{1}}
	out := normaliseHistoryItem(in)
	if len(out) != len(in) {
		t.Fatalf("got %d keys, want %d", len(out), len(in))
	}
	if out["toolcallid"] != "call_1" {
		t.Fatalf("tool call id lost: %v", out)
	}
}
