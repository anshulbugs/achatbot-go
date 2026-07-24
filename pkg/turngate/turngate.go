// Package turngate implements semantic endpointing for the voice pipeline: a
// small, fast LLM judges whether the caller has finished their thought (so the
// agent should reply) or is only pausing mid-sentence (so it should wait), and
// cleans up speech-to-text errors along the way. This lets the VAD use a very
// short end-of-turn silence for responsiveness without the agent answering
// half-finished sentences.
package turngate

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

const systemPrompt = `You decide when a phone caller has finished speaking. The input is a speech-to-text transcript of what the caller has said so far. It may contain recognition errors, and it may be an incomplete sentence because the caller paused while thinking.

Do two things:
1. Produce a corrected, cleaned version of the text (fix obvious transcription errors, punctuation, casing).
2. Decide whether the caller has clearly finished their thought and is waiting for a reply. If the text trails off, ends mid-clause, ends with a filler like "and", "so", "um", or is only a partial phrase, they are NOT finished.

Reply with ONLY a compact JSON object and nothing else:
{"complete": true or false, "refined": "cleaned text"}`

// Decision is the gate LLM's verdict for one accumulated utterance.
type Decision struct {
	Complete bool   `json:"complete"`
	Refined  string `json:"refined"`
}

// Gate calls an OpenAI-compatible chat endpoint to make endpointing decisions.
type Gate struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
}

// New builds a Gate against the given OpenAI-compatible base URL (e.g. Ollama's
// http://host:11434/v1) and model. Uses OPENAI_API_KEY if set (empty for Ollama).
func New(baseURL, model string) *Gate {
	return &Gate{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  os.Getenv("OPENAI_API_KEY"),
		client:  &http.Client{Timeout: 4 * time.Second},
	}
}

type chatReq struct {
	Model       string    `json:"model"`
	Messages    []message `json:"messages"`
	Temperature float64   `json:"temperature"`
	MaxTokens   int       `json:"max_tokens"`
	Stream      bool      `json:"stream"`
}
type message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}
type chatResp struct {
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	} `json:"choices"`
}

// Decide returns whether the accumulated text is a complete turn and a cleaned
// version of it. It fails open: on any error or unparseable output it returns
// {Complete: true, Refined: text} so the agent replies rather than hanging.
func (g *Gate) Decide(ctx context.Context, text string) Decision {
	fallback := Decision{Complete: true, Refined: text}
	body, err := json.Marshal(chatReq{
		Model: g.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			// /no_think keeps reasoning models (e.g. qwen3) fast and terse.
			{Role: "user", Content: text + " /no_think"},
		},
		Temperature: 0,
		MaxTokens:   200,
		Stream:      false,
	})
	if err != nil {
		return fallback
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, g.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return fallback
	}
	req.Header.Set("Content-Type", "application/json")
	if g.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+g.apiKey)
	}
	resp, err := g.client.Do(req)
	if err != nil {
		return fallback
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fallback
	}
	var cr chatResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil || len(cr.Choices) == 0 {
		return fallback
	}
	d, ok := parseDecision(cr.Choices[0].Message.Content)
	if !ok {
		return fallback
	}
	if strings.TrimSpace(d.Refined) == "" {
		d.Refined = text
	}
	return d
}

// parseDecision extracts the JSON object from the model's reply, tolerating
// surrounding prose or code fences.
func parseDecision(content string) (Decision, bool) {
	start := strings.Index(content, "{")
	end := strings.LastIndex(content, "}")
	if start < 0 || end <= start {
		return Decision{}, false
	}
	var d Decision
	if err := json.Unmarshal([]byte(content[start:end+1]), &d); err != nil {
		return Decision{}, false
	}
	return d, true
}

// String is handy for logs.
func (d Decision) String() string {
	return fmt.Sprintf("complete=%v refined=%q", d.Complete, d.Refined)
}
