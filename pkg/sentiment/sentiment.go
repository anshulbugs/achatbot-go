// Package sentiment classifies a caller's state mid-call so the platform can
// alert a human while the call is still live.
//
// It reports one of three things and nothing else: the caller wants a human,
// the caller is strongly interested, or the caller is annoyed. That closed set
// is the platform's, and it exists because each one is worth interrupting a
// person for. General sentiment ("slightly positive") is not.
//
// WHY A SEPARATE, SMALLER MODEL. The conversation model is the binding
// constraint on this stack: first-turn latency is what decides how many calls
// the fleet can carry, and it is dominated by prefill of the campaign prompt.
// Classifying on that model — whether as a second request or as extra tokens on
// the reply — spends the exact resource capacity is measured in. A 0.6B model
// on its own endpoint costs nothing that the caller waits for and nothing the
// capacity gate counts.
//
// It also runs OFF the reply path entirely: the classification happens on its
// own goroutine after the turn has been handed to the conversation model, so a
// slow or dead classifier delays no one.
package sentiment

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"time"
)

// Values the classifier may report, matching the platform's enum exactly. An
// unrecognised value is dropped rather than sent: the platform's schema
// rejects it, and a rejected event is a silently missed alert.
const (
	WantsHuman       = "wants_human"
	HighlyInterested = "highly_interested"
	UserAnnoyed      = "user_annoyed"
	// None is the classifier's "nothing worth reporting" answer, and it is by
	// far the most common one.
	None = "none"
)

const systemPrompt = `You classify a caller's state during a live phone call with an AI agent. Read the recent exchange and answer with exactly one word.

Answer wants_human if the caller asks to speak to a person, a human, a real agent, a manager, or says they do not want to talk to a bot.
Answer highly_interested if the caller is clearly keen to proceed: asking to book, apply, sign up, get details, or expressing strong enthusiasm.
Answer user_annoyed if the caller is frustrated, angry, insulting, repeatedly interrupted, asking to be removed, or telling the agent to stop calling.
Answer none for anything else, including ordinary polite conversation, mild hesitation, and simple questions.

Only one of these four words. No punctuation, no explanation.

Examples:
Caller: "Can I speak to a real person please?" => wants_human
Caller: "Is there a human I can talk to" => wants_human
Caller: "Yes I would love to apply, how do I start?" => highly_interested
Caller: "That sounds great, can you book me in?" => highly_interested
Caller: "Stop calling me, take me off your list" => user_annoyed
Caller: "This is useless, you keep repeating yourself" => user_annoyed
Caller: "I live in Brooklyn" => none
Caller: "What's the salary range?" => none
Caller: "Hmm, let me think about it" => none`

// Classifier calls an OpenAI-compatible chat endpoint for one-word verdicts.
type Classifier struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
}

// New builds a Classifier against an OpenAI-compatible base URL (Ollama's
// http://host:11434/v1, a second SGLang, anything that speaks the API).
//
// The timeout is short on purpose. A verdict that arrives after the caller has
// hung up is worthless, so giving up is strictly better than waiting.
func New(baseURL, model string) *Classifier {
	return &Classifier{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  os.Getenv("OPENAI_API_KEY"),
		client:  &http.Client{Timeout: 3 * time.Second},
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

// Classify returns one of the exported values, or None.
//
// priorAssistant is what the agent last said; without it "no, a person" reads
// as a bare fragment rather than an answer to "shall I go on?".
//
// Fails CLOSED: any error returns None. The opposite would page a human because
// a 0.6B model timed out, and an alert that fires on noise is one people learn
// to ignore.
func (c *Classifier) Classify(ctx context.Context, priorAssistant, callerText string) string {
	if strings.TrimSpace(callerText) == "" {
		return None
	}
	user := "Caller: \"" + callerText + "\""
	if priorAssistant != "" {
		user = "Agent: \"" + priorAssistant + "\"\n" + user
	}
	body, err := json.Marshal(chatReq{
		Model: c.model,
		Messages: []message{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: user},
		},
		Temperature: 0,
		MaxTokens:   6,
		Stream:      false,
	})
	if err != nil {
		return None
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return None
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return None
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return None
	}
	var cr chatResp
	if err := json.NewDecoder(resp.Body).Decode(&cr); err != nil || len(cr.Choices) == 0 {
		return None
	}
	return parseVerdict(cr.Choices[0].Message.Content)
}

// parseVerdict maps a model reply to a value. Small models add punctuation,
// quotes and the occasional preamble, so this looks for the token rather than
// demanding an exact match.
func parseVerdict(out string) string {
	s := strings.ToLower(out)
	switch {
	case strings.Contains(s, "wants_human"), strings.Contains(s, "wants human"):
		return WantsHuman
	case strings.Contains(s, "highly_interested"), strings.Contains(s, "highly interested"):
		return HighlyInterested
	case strings.Contains(s, "user_annoyed"), strings.Contains(s, "user annoyed"):
		return UserAnnoyed
	default:
		return None
	}
}

// Tracker holds one call's sentiment state and decides when an event is worth
// sending.
//
// The platform wants CHANGES, not a reading per turn: a caller who is annoyed
// stays annoyed for the rest of the call, and re-alerting on every turn would
// make the alert useless. It also never reverts to None — "the caller stopped
// being annoyed" is not something anyone acts on, and sending it would clear an
// operator's alert while the call is still going badly.
type Tracker struct {
	current string
}

// Observe records a verdict and reports whether it should be sent.
func (t *Tracker) Observe(v string) bool {
	if v == None || v == t.current {
		return false
	}
	t.current = v
	return true
}

// Current is the last reported sentiment, or empty if none has been.
func (t *Tracker) Current() string { return t.current }
