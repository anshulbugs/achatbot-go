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

// openAILLMClient forwards the platform's chat-completion requests to the same
// model the calls use.
//
// A separate, tiny client rather than the pipeline's LLM processor: that one is
// built per call session, carries chat history and streaming, and is shaped
// around a live conversation. This is one stateless passthrough, and borrowing
// the session machinery would couple the two so that a change for calls could
// quietly alter what the platform gets.
//
// It points at the same endpoint the calls use, which is the point: the model
// stays on 127.0.0.1 and only the agent can reach it.
type openAILLMClient struct {
	baseURL string
	model   string
	http    *http.Client
}

// llmHTTPTimeout is generous because latency is explicitly not a goal here —
// but it is finite, because a request that hangs holds one of the gate's few
// concurrency slots and starves everything behind it.
const llmHTTPTimeout = 120 * time.Second

func newLLMClient(baseURL, model string) *openAILLMClient {
	return &openAILLMClient{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		http:    &http.Client{Timeout: llmHTTPTimeout},
	}
}

// Forward implements rexa.ChatCompleter for the bearer-authed passthrough.
//
// Returns the upstream status and body untouched. That is the whole point: the
// caller gets the model's own error text — "context length exceeded" rather
// than a bare 500 — and a plain OpenAI response when it succeeds, so any SDK
// works against it.
func (c *openAILLMClient) Forward(ctx context.Context, body []byte) (int, []byte, error) {
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
