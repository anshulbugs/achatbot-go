package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// openAIEvalClient is the rexa.LLMClient the evaluation endpoint runs on.
//
// A separate, tiny client rather than the pipeline's LLM processor: that one is
// built per call session, carries chat history and streaming, and is shaped
// around a live conversation. An evaluation is one non-streaming request with
// no history, and borrowing the session machinery for it would couple the two
// so that a change for calls could quietly alter evaluations.
//
// It points at the same endpoint the calls use, which is the point of the whole
// feature: the model stays on 127.0.0.1 and only the agent can reach it.
type openAIEvalClient struct {
	baseURL string
	model   string
	http    *http.Client
}

// evalHTTPTimeout is generous because latency is explicitly not a goal here —
// but it is finite, because a request that hangs holds one of the evaluator's
// few concurrency slots and starves the ones behind it.
const evalHTTPTimeout = 120 * time.Second

func newEvalLLMClient(baseURL, model string) *openAIEvalClient {
	return &openAIEvalClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: evalHTTPTimeout},
	}
}

// Complete runs one non-streaming chat completion.
//
// temperature 0: an evaluation that returns a different score for the same
// transcript on two runs is not an evaluation. This is the one place in the
// stack where determinism matters more than sounding natural.
func (c *openAIEvalClient) Complete(ctx context.Context, system, user string, maxTokens int) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model": c.model,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"max_tokens":  maxTokens,
		"temperature": 0,
		"stream":      false,
	})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		// The body carries the reason — a context-length overflow, say — and
		// losing it would leave only "500" to debug from.
		return "", fmt.Errorf("llm returned %d: %s", resp.StatusCode, firstChars(string(raw), 200))
	}
	var out struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("could not parse llm response: %w", err)
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("llm returned no choices")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// Forward implements rexa.ChatCompleter for the bearer-authed passthrough.
//
// Returns the upstream status and body untouched. That is the whole point: the
// caller gets the model's own error text — "context length exceeded" rather
// than a bare 500 — and a plain OpenAI response when it succeeds, so any SDK
// works against it.
func (c *openAIEvalClient) Forward(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.baseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	// 8MB: generous for a completion, finite so a misbehaving upstream cannot
	// exhaust memory.
	out, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, out, nil
}
