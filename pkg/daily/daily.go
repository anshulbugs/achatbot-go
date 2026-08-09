// Package daily creates short-lived Daily rooms so an operator can listen to a
// live call.
//
// COST IS THE DESIGN CONSTRAINT. Daily bills per participant-minute, and a room
// per call across a whole campaign would be a large bill for a feature almost
// nobody opens. So a room is created only when the dispatch carries Redis
// details — the platform's own signal that someone is watching this call — and
// every room is created with an expiry so an abandoned one cannot bill
// indefinitely.
//
// The API key is agent-side configuration (DAILY_API_KEY), not a per-dispatch
// field: it is our account being billed, not the tenant's.
package daily

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const apiBase = "https://api.daily.co/v1"

// Client talks to the Daily REST API.
type Client struct {
	apiKey string
	http   *http.Client
}

// New returns a client, or nil when no key is configured — which is the normal
// state for a deployment that does not use live listening. Every method is
// nil-safe.
func New(apiKey string) *Client {
	if strings.TrimSpace(apiKey) == "" {
		return nil
	}
	return &Client{
		apiKey: apiKey,
		// Short: this runs while a call is being placed, and a slow room
		// creation must not delay the dial.
		http: &http.Client{Timeout: 5 * time.Second},
	}
}

// Room is a created room plus the ways in.
type Room struct {
	Name string
	// URL is the bare room address. Not usable on its own for a private room —
	// use JoinURL.
	URL string
	// JoinURL carries a meeting token, so it grants access by itself.
	JoinURL string
	// Token is the meeting token on its own. The platform's WebRTC response
	// takes room_url and token as separate fields and hands them to a browser
	// SDK, which wants them apart rather than glued into a URL.
	Token string
	// TokenTTL is how long both remain valid.
	TokenTTL time.Duration
	// SIPURI is the address a carrier dials to put a phone leg into the room.
	SIPURI string
}

type roomResp struct {
	Name   string `json:"name"`
	URL    string `json:"url"`
	Config struct {
		SIPURI struct {
			Endpoint string `json:"endpoint"`
		} `json:"sip_uri"`
	} `json:"config"`
}

// CreateRoom makes a private, SIP-enabled room that expires after ttl.
//
// Private, not public: the room carries a live recording of a real customer
// conversation, and a public room URL is a working link for anyone who ever
// sees it. The returned JoinURL embeds a meeting token so an operator still
// only needs the one link.
//
// eject_at_room_exp is set as well as exp — expiry alone stops new joins but
// leaves anyone already inside connected, and billing with them.
func (c *Client) CreateRoom(ctx context.Context, ttl time.Duration) (*Room, error) {
	if c == nil {
		return nil, nil
	}
	exp := time.Now().Add(ttl).Unix()
	body := map[string]any{
		"privacy": "private",
		"properties": map[string]any{
			"exp":               exp,
			"eject_at_room_exp": true,
			// An operator dropping in on a live call wants to be listening
			// immediately, not reading a device-setup screen while the moment
			// they joined for passes.
			"enable_prejoin_ui": false,
			"start_video_off":   true,
			"enable_chat":       false,
			"sip": map[string]any{
				"display_name":  "caller",
				"sip_mode":      "dial-in",
				"num_endpoints": 1,
				"video":         false,
			},
		},
	}
	var out roomResp
	if err := c.do(ctx, http.MethodPost, "/rooms", body, &out); err != nil {
		return nil, err
	}

	room := &Room{
		Name: out.Name, URL: out.URL, JoinURL: out.URL,
		SIPURI: out.Config.SIPURI.Endpoint, TokenTTL: ttl,
	}

	// A token failure is not fatal for listening: an operator with the bare URL
	// can still be let in manually, which beats failing a call over it. It IS
	// fatal for a browser dispatch, so that caller checks Token itself.
	if tok, err := c.meetingToken(ctx, out.Name, exp); err == nil && tok != "" {
		room.Token = tok
		room.JoinURL = out.URL + "?t=" + tok
	}
	return room, nil
}

// meetingToken mints an owner token scoped to one room and expiring with it.
func (c *Client) meetingToken(ctx context.Context, room string, exp int64) (string, error) {
	body := map[string]any{
		"properties": map[string]any{
			"room_name": room,
			"exp":       exp,
			// Owner so the operator can unmute and speak to the caller, which
			// is the point of joining rather than reading a transcript.
			"is_owner":  true,
			"user_name": "operator",
		},
	}
	var out struct {
		Token string `json:"token"`
	}
	if err := c.do(ctx, http.MethodPost, "/meeting-tokens", body, &out); err != nil {
		return "", err
	}
	return out.Token, nil
}

// Presence reports how many participants are currently in a room.
//
// Needed because Daily refuses a SIP participant that would be alone in the
// room — `allow_sip_only_in_room` is false on this domain, and the call is
// rejected with SIP 480 Temporarily Unavailable, which reads like a network
// fault rather than a policy. So the agent waits for the browser before dialling
// in, which is the natural order anyway: the browser is the caller.
func (c *Client) Presence(ctx context.Context, room string) (int, error) {
	if c == nil || room == "" {
		return 0, nil
	}
	var out struct {
		TotalCount int `json:"total_count"`
	}
	if err := c.do(ctx, http.MethodGet, "/rooms/"+room+"/presence", nil, &out); err != nil {
		return 0, err
	}
	return out.TotalCount, nil
}

// WaitForParticipant polls until someone joins the room, or gives up.
//
// Returns false on timeout, which means the browser never arrived — a dispatch
// nobody answered. Polling rather than a webhook because a webhook would need a
// publicly reachable callback registered with Daily per deployment, and this
// runs for at most a couple of minutes per call.
func (c *Client) WaitForParticipant(ctx context.Context, room string, timeout, interval time.Duration) bool {
	if c == nil {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		n, err := c.Presence(ctx, room)
		if err == nil && n > 0 {
			return true
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(interval):
		}
	}
	return false
}

// DeleteRoom removes a room when the call ends.
//
// Rooms expire on their own, so this is tidiness rather than correctness — but
// a room that outlives its call is a link that still opens, and an operator who
// clicks it hears silence and reports the feature as broken.
func (c *Client) DeleteRoom(ctx context.Context, name string) error {
	if c == nil || name == "" {
		return nil
	}
	return c.do(ctx, http.MethodDelete, "/rooms/"+name, nil, nil)
}

func (c *Client) do(ctx context.Context, method, path string, body any, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, apiBase+path, rdr)
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
	if resp.StatusCode >= 300 {
		var buf bytes.Buffer
		_, _ = buf.ReadFrom(resp.Body)
		return fmt.Errorf("daily %s %s: %d %s", method, path, resp.StatusCode,
			strings.TrimSpace(buf.String()))
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
