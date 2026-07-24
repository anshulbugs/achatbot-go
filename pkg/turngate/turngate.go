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

const systemPrompt = `You are a turn-taking classifier for a phone voice assistant. Given the caller's speech so far (a live speech-to-text transcript that may cut off mid-thought when they pause), decide whether they have finished a complete thought and expect a reply now, or are still mid-sentence and about to continue.

Rules:
- A grammatically complete sentence or a clear question/command is COMPLETE.
- Text that trails off, ends on a preposition/conjunction/filler ("to", "and", "so", "um", "the"), or is a bare fragment is INCOMPLETE.
- Do not consider politeness or length; a two-word answer like "Yes please" is COMPLETE.

Examples:
"What time is it?" => complete
"I want to book a flight to" => incomplete
"Tell me a joke." => complete
"So the thing is" => incomplete
"Yes." => complete
"Can you tell me the weather in Delhi today?" => complete
"Um, I was thinking that maybe we could" => incomplete
"gene" => incomplete
"What does relativity actually mean?" => complete

Reply with exactly one word: complete or incomplete.`

// Decision is the gate's verdict for one accumulated utterance. Refined is the
// text to forward downstream (currently the input unchanged; the gate no
// longer rewrites transcripts, to avoid the model hallucinating completions).
type Decision struct {
	Complete bool
	Refined  string
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

// Decide classifies whether text is a complete turn. priorAssistant is what
// the bot last said, given as context so short replies to a question are read
// as complete. It fails open: on any error it returns Complete=true so the
// agent replies rather than hanging. Refined is the input text unchanged.
func (g *Gate) Decide(ctx context.Context, priorAssistant, text string) Decision {
	fallback := Decision{Complete: true, Refined: text}
	userContent := "Caller so far: \"" + text + "\""
	if priorAssistant != "" {
		userContent = "The assistant just said: \"" + priorAssistant + "\"\n" + userContent
	}
	body, err := json.Marshal(chatReq{
		Model: g.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: userContent},
		},
		Temperature: 0,
		MaxTokens:   4,
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
	out := strings.ToLower(cr.Choices[0].Message.Content)
	// "incomplete" contains "complete", so test for it first.
	if strings.Contains(out, "incomplete") {
		return Decision{Complete: false, Refined: text}
	}
	if strings.Contains(out, "complete") {
		return Decision{Complete: true, Refined: text}
	}
	return fallback // unrecognized: reply rather than hang
}

// String is handy for logs.
func (d Decision) String() string {
	return fmt.Sprintf("complete=%v refined=%q", d.Complete, d.Refined)
}
