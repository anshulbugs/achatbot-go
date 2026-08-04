// Package telnyx is a minimal Telnyx Call Control v2 client plus the media
// bridge that connects a Telnyx call's audio to the achatbot pipeline.
//
// Secrets come from the environment, never config files: TELNYX_API_KEY,
// TELNYX_APP_ID (the Call Control Application / connection id), and
// TELNYX_FROM_NUMBER (caller id). TELNYX_PUBLIC_URL is the externally
// reachable base URL (e.g. an https tunnel) used to build per-call webhook
// and media-stream URLs.
package telnyx

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"
)

const apiBase = "https://api.telnyx.com/v2"

// Client talks to the Telnyx Call Control v2 API.
type Client struct {
	apiKey     string
	appID      string
	fromNumber string
	publicURL  string // e.g. https://xxxx.trycloudflare.com
	http       *http.Client
}

// NewClientFromEnv builds a Client from TELNYX_* environment variables.
// Returns nil when TELNYX_API_KEY is unset, so callers can treat Telnyx as
// an optional feature.
func NewClientFromEnv() *Client {
	key := os.Getenv("TELNYX_API_KEY")
	if key == "" {
		return nil
	}
	return &Client{
		apiKey:     key,
		appID:      os.Getenv("TELNYX_APP_ID"),
		fromNumber: os.Getenv("TELNYX_FROM_NUMBER"),
		publicURL:  os.Getenv("TELNYX_PUBLIC_URL"),
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

func (c *Client) FromNumber() string { return c.fromNumber }
func (c *Client) PublicURL() string  { return c.publicURL }

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return fmt.Errorf("telnyx %s %s: %d %s", method, path, resp.StatusCode, string(respBody))
	}
	if out != nil && len(respBody) > 0 {
		return json.Unmarshal(respBody, out)
	}
	return nil
}

// DialResult is the subset of the Telnyx dial response we use.
type DialResult struct {
	Data struct {
		CallControlID string `json:"call_control_id"`
		CallLegID     string `json:"call_leg_id"`
	} `json:"data"`
}

// Dial places an outbound call. webhookURL overrides the application's
// configured webhook for this call only (so test calls do not hit a shared
// production endpoint). clientState is echoed back on every webhook event.
func (c *Client) Dial(ctx context.Context, to, webhookURL, clientState, amd string) (string, error) {
	body := map[string]any{
		"connection_id": c.appID,
		"to":            to,
		"from":          c.fromNumber,
		"webhook_url":   webhookURL,
		"client_state":  encodeState(clientState),
		"timeout_secs":  30,
		"answering_machine_detection": func() string {
			if amd == "" {
				return "disabled"
			}
			return amd
		}(),
	}
	var res DialResult
	if err := c.do(ctx, http.MethodPost, "/calls", body, &res); err != nil {
		return "", err
	}
	return res.Data.CallControlID, nil
}

// Speak uses Telnyx's built-in TTS to play text into the call. Used only for
// the greeting-loop smoke test; real conversation audio goes over the media
// stream.
func (c *Client) Speak(ctx context.Context, callControlID, text, voice string) error {
	if voice == "" {
		voice = "Telnyx.KokoroTTS.af_heart"
	}
	body := map[string]any{"payload": text, "voice": voice, "language": "en-US"}
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/speak", body, nil)
}

// StreamingStart forks the call's audio to a bidirectional WebSocket at
// streamURL. Audio is PCMU (G.711 µ-law) at 8 kHz in both directions.
func (c *Client) StreamingStart(ctx context.Context, callControlID, streamURL string) error {
	body := map[string]any{
		"stream_url":                 streamURL,
		"stream_track":               "inbound_track",
		"stream_bidirectional_mode":  "rtp",
		"stream_bidirectional_codec": "PCMU",
	}
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/streaming_start", body, nil)
}

// Answer answers an inbound call (unused for outbound tests but handy).
func (c *Client) Answer(ctx context.Context, callControlID string) error {
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/answer", nil, nil)
}

// Hangup ends the call.
func (c *Client) Hangup(ctx context.Context, callControlID string) error {
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/hangup", nil, nil)
}

// RecordStart begins a dual-channel recording of the call. Telnyx stores the
// file and exposes it via GET /recordings; "dual" keeps each side on its own
// channel, which is what makes an agent-to-agent call reviewable.
func (c *Client) RecordStart(ctx context.Context, callControlID string) error {
	body := map[string]any{"format": "mp3", "channels": "dual"}
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/record_start", body, nil)
}
