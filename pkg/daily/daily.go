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
	"net/url"
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

// RoomOptions configures a room. A struct rather than a run of booleans
// because the call sites read as prose this way and "true, false" does not.
type RoomOptions struct {
	// TTL bounds the room's life.
	TTL time.Duration
	// Record turns on cloud recording. Only browser calls set it: a phone call
	// is already recorded by the carrier, so recording its listen-in room too
	// would bill twice for two copies of one conversation.
	Record bool
	// Public drops the meeting-token requirement.
	//
	// Set for the LISTEN-IN room, and the reason is a failure rather than a
	// preference. A private room needs the client to receive the token and hand
	// it to the Daily SDK, and when that did not happen the operator got
	// "You are not allowed to join this meeting" — twice, once with no token
	// published and once with the token published correctly. A listening
	// feature that cannot be joined is worth less than the secrecy of a room
	// name that is a random twenty-character string, published only to the
	// tenant's own Redis, deleted when the call ends and expired within hours.
	//
	// The browser-call room stays private: that path hands the token back in
	// the dispatch response and is known to work.
	Public bool
}

// CreateRoom makes a SIP-enabled room that expires after opt.TTL.
//
// Private, not public: the room carries a live recording of a real customer
// conversation, and a public room URL is a working link for anyone who ever
// sees it. The returned JoinURL embeds a meeting token so an operator still
// only needs the one link.
//
// eject_at_room_exp is set as well as exp — expiry alone stops new joins but
// leaves anyone already inside connected, and billing with them.
func (c *Client) CreateRoom(ctx context.Context, opt RoomOptions) (*Room, error) {
	if c == nil {
		return nil, nil
	}
	exp := time.Now().Add(opt.TTL).Unix()
	props := map[string]any{
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
	}
	if opt.Record {
		props["enable_recording"] = "cloud"
	}
	privacy := "private"
	if opt.Public {
		privacy = "public"
	}
	body := map[string]any{"privacy": privacy, "properties": props}
	var out roomResp
	if err := c.do(ctx, http.MethodPost, "/rooms", body, &out); err != nil {
		return nil, err
	}

	room := &Room{
		Name: out.Name, URL: out.URL, JoinURL: out.URL,
		SIPURI: out.Config.SIPURI.Endpoint, TokenTTL: opt.TTL,
	}

	// A token failure is not fatal for listening: an operator with the bare URL
	// can still be let in manually, which beats failing a call over it. It IS
	// fatal for a browser dispatch, so that caller checks Token itself.
	if tok, err := c.meetingToken(ctx, out.Name, exp, opt.Record); err == nil && tok != "" {
		room.Token = tok
		room.JoinURL = out.URL + "?t=" + tok
	}
	return room, nil
}

// meetingToken mints an owner token scoped to one room and expiring with it.
//
// record puts start_cloud_recording on the token rather than starting the
// recording over the API afterwards. Daily has no REST call that starts a cloud
// recording in an empty room — recording is a participant action — and the
// token is the one hook that fires the instant somebody joins. The sidecar
// holds this token and joins before the browser does, so recording begins
// ahead of the greeting rather than a second or two into it.
func (c *Client) meetingToken(ctx context.Context, room string, exp int64, record bool) (string, error) {
	return c.namedToken(ctx, room, exp, record, "operator")
}

// PrewarmUserName marks the room pre-joiner so it is not mistaken for a
// supervisor.
//
// EVERY listener detection we have is "a participant appeared" — the presence
// sweep counts them, the Daily webhook fires on them. A pre-joiner is a
// participant, so the first one ever to run was read as an operator barging in
// one second after the call was answered: the agent hushed mid-greeting, left,
// and the caller heard nothing again. Both detectors must skip this name.
const PrewarmUserName = "__prewarm__"

// PrewarmToken mints a token for the room pre-joiner. Not an owner — it never
// speaks, and it must never start a recording.
func (c *Client) PrewarmToken(ctx context.Context, room string, exp int64) (string, error) {
	return c.namedToken(ctx, room, exp, false, PrewarmUserName)
}

func (c *Client) namedToken(ctx context.Context, room string, exp int64, record bool, userName string) (string, error) {
	props := map[string]any{
		"room_name": room,
		"exp":       exp,
		// Owner so the operator can unmute and speak to the caller, which
		// is the point of joining rather than reading a transcript.
		"is_owner":  userName != PrewarmUserName,
		"user_name": userName,
	}
	if record {
		props["enable_recording"] = "cloud"
		props["start_cloud_recording"] = true
	}
	body := map[string]any{"properties": props}
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

// Recording is one cloud recording of a room.
//
// Only the fields the platform's recording event needs. StartTS is unix
// seconds and Duration is whole seconds — Daily reports both that way, and
// converting them here keeps the arithmetic in one place rather than at each
// call site.
type Recording struct {
	ID         string `json:"id"`
	RoomName   string `json:"room_name"`
	StartTS    int64  `json:"start_ts"`
	Status     string `json:"status"`
	Duration   int    `json:"duration"`
	S3Key      string `json:"s3key"`
	ShareToken string `json:"share_token"`
}

// Finished reports whether the recording has been written out and is safe to
// hand to the platform. Daily reports "in-progress" while the call is live and
// "finished" once the file is complete; anything else (notably "canceled") is
// not something to send a URL for.
func (r *Recording) Finished() bool { return r != nil && r.Status == "finished" }

// LatestRecording returns the most recent recording for a room, or nil when the
// room has none.
//
// Nil-and-no-error for "none yet" rather than an error: a room with no
// recording is the normal state for the first seconds after a call ends, and
// for every call where recording was never enabled. The caller polls.
func (c *Client) LatestRecording(ctx context.Context, room string) (*Recording, error) {
	if c == nil || room == "" {
		return nil, nil
	}
	var out struct {
		Data []Recording `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, "/recordings?limit=1&room_name="+url.QueryEscape(room), nil, &out); err != nil {
		return nil, err
	}
	if len(out.Data) == 0 {
		return nil, nil
	}
	rec := out.Data[0]
	return &rec, nil
}

// AccessLink returns a time-limited download URL for a recording.
//
// ttl is clamped by Daily itself; passing a long one is not a way to get a
// permanent link, and it should not be treated as one. The platform is expected
// to fetch and re-host if it wants the recording to outlive the link.
func (c *Client) AccessLink(ctx context.Context, id string, ttl time.Duration) (string, error) {
	if c == nil || id == "" {
		return "", nil
	}
	var out struct {
		DownloadLink string `json:"download_link"`
	}
	path := fmt.Sprintf("/recordings/%s/access-link?valid_for_secs=%d", url.PathEscape(id), int(ttl.Seconds()))
	if err := c.do(ctx, http.MethodGet, path, nil, &out); err != nil {
		return "", err
	}
	return out.DownloadLink, nil
}

// PresenceAll returns the participant count for every room that has anyone in
// it, in ONE request for the whole domain.
//
// The per-room Presence call is what live listening used, and it scales the
// wrong way: a check per watched call per interval, so sixty concurrent calls
// at one second apart would be sixty requests a second. That forced the
// interval up to five seconds, which an operator experiences as five seconds of
// the agent still talking after they press Join.
//
// This costs one request regardless of how many calls are in flight, so the
// interval can be short enough to feel immediate.
func (c *Client) PresenceAll(ctx context.Context) (map[string]int, error) {
	if c == nil {
		return nil, nil
	}
	var raw map[string]json.RawMessage
	if err := c.do(ctx, http.MethodGet, "/presence", nil, &raw); err != nil {
		return nil, err
	}
	out := make(map[string]int, len(raw))
	for room, v := range raw {
		var participants []struct {
			UserName string `json:"userName"`
		}
		if err := json.Unmarshal(v, &participants); err == nil {
			// The pre-joiner does not count as a listener. It is in every
			// prewarmed room by design, so counting it would report every
			// answered call as being watched and bridge them all.
			n := 0
			for _, p := range participants {
				if p.UserName != PrewarmUserName {
					n++
				}
			}
			out[room] = n
			continue
		}
		// A shape we do not recognise still means the room was listed, and
		// Daily only lists rooms with someone in them. Counting it as one
		// occupant is the reading that does not lose a listener to a schema
		// change.
		out[room] = 1
	}
	return out, nil
}

// Webhook is a domain-level subscription to Daily's own events.
type Webhook struct {
	UUID       string   `json:"uuid"`
	URL        string   `json:"url"`
	EventTypes []string `json:"eventTypes"`
	State      string   `json:"state"`
}

// EnsureWebhook makes Daily push events to url and nothing else.
//
// WHY A WEBHOOK AT ALL. The REST presence endpoint lags a real join by about
// 5.6 seconds — measured, not assumed — and that lag is most of the ten seconds
// an operator spends listening to the agent after taking over a call. Polling
// faster cannot help; the data is simply stale. This is the only signal Daily
// offers that arrives when the join does, and unlike every other option it
// needs no change from the platform or the other call agent.
//
// Re-registered on every start because the URL is a cloudflared tunnel whose
// hostname changes each time it comes up: a webhook registered yesterday points
// at a host that no longer resolves. Stale subscriptions are deleted rather
// than left, or Daily accumulates dead endpoints and keeps retrying them.
//
// Daily VALIDATES the URL by POSTing to it during registration and refuses
// anything that does not answer 200 — so the handler must exist and be
// reachable before this is called.
func (c *Client) EnsureWebhook(ctx context.Context, url string, events []string) error {
	if c == nil || url == "" {
		return nil
	}
	var existing []Webhook
	if err := c.do(ctx, http.MethodGet, "/webhooks", nil, &existing); err != nil {
		return err
	}
	for _, w := range existing {
		if w.URL == url && w.State == "ACTIVE" {
			return nil // already pointing at us
		}
		// Not fatal if this fails: a subscription we could not remove costs
		// Daily a retry, not this call anything.
		_ = c.do(ctx, http.MethodDelete, "/webhooks/"+w.UUID, nil, nil)
	}
	return c.do(ctx, http.MethodPost, "/webhooks", map[string]any{
		"url": url, "eventTypes": events,
	}, nil)
}
