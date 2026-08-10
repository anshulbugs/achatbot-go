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
	"strings"
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

// NewClient builds a Client from explicit credentials.
//
// The platform contract supplies telecom credentials per dispatch, scoped to
// the tenant that owns the call (BYO), so a process-wide client built from the
// environment cannot serve it — one tenant's calls would be placed with
// another's API key. Contract dispatches construct a Client per call from
// their own credentials and discard it when the call ends.
//
// NewClientFromEnv is untouched and remains the path for the local demo
// server, which has a single operator and one set of credentials.
//
//   - apiKey, appID: the dispatch's telnyx api_key + connection_id.
//   - fromNumber: the caller id the platform chose for this call. It is
//     per-call rather than per-process; a tenant may own several DIDs and the
//     platform picks one per campaign.
//   - publicURL: our own externally-reachable base URL, used to build the
//     per-call webhook and media-stream URLs. Ours, not the platform's.
func NewClient(apiKey, appID, fromNumber, publicURL string) *Client {
	return &Client{
		apiKey:     apiKey,
		appID:      appID,
		fromNumber: fromNumber,
		publicURL:  publicURL,
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
func (c *Client) Dial(ctx context.Context, to, webhookURL, clientState, amd string, ringSecs int) (string, error) {
	if ringSecs <= 0 {
		ringSecs = 30
	}
	body := map[string]any{
		"connection_id": c.appID,
		"to":            to,
		"from":          c.fromNumber,
		"webhook_url":   webhookURL,
		"client_state":  encodeState(clientState),
		"timeout_secs":  ringSecs,
		// Keep RTP flowing while we have nothing to say.
		//
		// THE VOICEMAIL MESSAGE MAY DEPEND ON THIS. Telnyx documents it as
		// "generate silence RTP packets when no transmission available", and it
		// defaults to false — so when we stop sending, the outbound path
		// carries nothing at all. That is exactly the shape of the failure: the
		// greeting plays because RTP flows from the instant the stream opens,
		// then we send nothing for the six to twelve seconds it takes detection
		// to answer and the mailbox to beep, and the message that follows is
		// accepted by the socket and never heard. A live conversation never
		// leaves a gap that long, which is why only this path breaks.
		"send_silence_when_idle": true,
		"answering_machine_detection": func() string {
			if amd == "" {
				return "disabled"
			}
			return amd
		}(),
	}
	if cfg := amdConfigFor(amd); cfg != nil {
		body["answering_machine_detection_config"] = cfg
	}
	var res DialResult
	if err := c.do(ctx, http.MethodPost, "/calls", body, &res); err != nil {
		return "", err
	}
	return res.Data.CallControlID, nil
}

// amdConfigFor returns the detection tuning to send with a dial, or nil to
// accept Telnyx's defaults.
//
// Set explicitly for `premium` because Telnyx's own documentation contradicts
// itself on what the default is: the OpenAPI spec gives
// total_analysis_time_millis a default of 3500, while the prose on the premium
// page says "by default, the timeout is set to 30 seconds" and their sample
// request passes 30000. Those are not close enough to guess between — one
// judges on three and a half seconds of audio, the other can take half a minute
// to answer.
//
// 10s is chosen against OUR call, not against either default. The agent plays a
// pre-rendered greeting of roughly fifteen seconds and waits out that audio for
// a verdict before committing a pipeline, so a verdict inside ten seconds
// arrives with margin to spare and can still route the call to voicemail and
// leave the message. A verdict at thirty seconds would land after the pipeline
// had started, where the best available outcome is hanging up on the machine
// without leaving anything. Ten seconds also gives premium detection nearly
// three times the audio that standard AMD's 3.5s default allows it.
//
// Only for premium: the standard modes have consistent documented defaults and
// their own well-tested tuning, and overriding those would be changing
// something that is not in question.
func amdConfigFor(amd string) map[string]any {
	if amd != "premium" {
		return nil
	}
	return map[string]any{
		// How long detection gets before it gives up.
		//
		// 10000 was manufacturing "not_sure". Every verdict in the log tells
		// the same story: human_residence lands at 3-4s, machine at 4-5s,
		// silence at 5s — and every single not_sure at 9, 10 or 11 seconds,
		// twelve of them, all pressed against a ten second deadline. That is
		// not detection being uncertain, it is detection running out of time,
		// and because not_sure is treated as human those calls put the agent
		// into a conversation with a voicemail. It is the long-ringing numbers
		// that land here: the ones whose mailbox picks up after a lengthy ring
		// and opens with a slow greeting are exactly the ones that need more
		// than ten seconds to characterise.
		//
		// Raising it does NOT slow down the calls that already work — a real
		// verdict still arrives in 3-5s and the pipeline starts the moment it
		// does. This only extends the deadline for the cases currently being
		// timed out into a wrong answer.
		"total_analysis_time_millis": 15000,
		// How long to keep listening for the beep after concluding "machine".
		//
		// THIS IS WHY A VOICEMAIL RECORDED SILENCE. The default is a few
		// seconds: on a real call the verdict landed at 4s and the greeting
		// event followed at 10s reporting no_beep_detected — Telnyx had given
		// up while the machine was still reading its own greeting. The agent
		// took that as its cue, played the message over the greeting, and hung
		// up; the machine then beeped and recorded the silence that followed.
		//
		// 10000 is the CEILING Telnyx allows: the documented range is
		// (100, 10000) and anything above it fails the dial outright with
		// "parameter is outside the valid range" — which is not a degraded
		// voicemail, it is no call at all. 30000 was tried and did exactly
		// that.
		//
		// So this is as much beep-detection as the carrier will sell us, and it
		// is still short of a long outgoing greeting. A greeting that outlasts
		// it reports no_beep_detected rather than beep_detected, and the
		// voicemail path has to handle that case on its own rather than trust
		// the cue. See runVoicemailCall.
		"greeting_duration_millis": 10000,
	}
}

// DialSIP places a call to a SIP URI and returns its call-control id.
//
// Used to put a Daily room's SIP endpoint into a conference with a live call,
// so an operator in that room hears the conversation. Unlike Dial this sends no
// answering-machine detection and a short timeout: the far end is a media
// server that answers immediately, and waiting 30 s for it means an operator
// clicks a join link and hears nothing for half a minute.
// conferenceName, when set, has the carrier put this leg straight into that
// conference — no separate join call, and joined at RINGBACK rather than at
// answer.
//
// THIS IS THE WHOLE BARGE LATENCY. Measured on a live call: our own API calls
// take 268ms (conference 208ms, dial 268ms) and Daily then takes 4.54 SECONDS to
// answer the SIP invite, because it does not register a room's SIP endpoint
// with the SIP network until a session exists — and the operator's join is what
// starts the session. So registration is serialised in front of our INVITE and
// there is nothing to tune on either side.
//
// `early_media` sidesteps the wait instead of shortening it. Telnyx: "Controls
// the moment when dialled call is joined into conference. If set to `true` user
// will be joined as soon as media is available (ringback). If `false` user will
// be joined when call is answered." So the leg enters the conference while
// Daily is still setting up, and the operator hears the call from that point
// rather than four and a half seconds later.
//
// It also removes the separate join round trip and the 422 "Call not answered
// yet" retry loop that existed only because joining required an answered leg.
func (c *Client) DialSIP(ctx context.Context, sipURI, webhookURL, clientState, conferenceName, supervisorRole string) (string, error) {
	if !strings.HasPrefix(sipURI, "sip:") {
		sipURI = "sip:" + sipURI
	}
	body := map[string]any{
		"connection_id": c.appID,
		"to":            sipURI,
		"from":          c.fromNumber,
		"webhook_url":   webhookURL,
		"client_state":  encodeState(clientState),
		"timeout_secs":  15,
	}
	if conferenceName != "" {
		cfg := map[string]any{
			"conference_name": conferenceName,
			"early_media":     true,
			// The conference already exists with the caller in it; this leg
			// must not restart or end it.
			"start_conference_on_enter": false,
			"end_conference_on_exit":    false,
		}
		// supervisorRole is Telnyx's own listen/whisper/barge vocabulary.
		// "monitor" is what makes silent listening possible at all: per their
		// docs, "nobody can hear supervisor call, but supervisor can hear
		// everything on the call". Empty leaves the default, which is an
		// ordinary participant everyone can hear.
		if supervisorRole != "" {
			cfg["supervisor_role"] = supervisorRole
		}
		body["conference_config"] = cfg
	}
	var res DialResult
	if err := c.do(ctx, http.MethodPost, "/calls", body, &res); err != nil {
		return "", err
	}
	return res.Data.CallControlID, nil
}

// ConferenceCreate starts a conference with callControlID as its first
// participant and returns the conference id.
//
// Conferencing rather than transferring, because the caller must stay where
// they are: a transfer hands the leg away and ends the agent's conversation,
// which is the opposite of letting someone listen in on it.
func (c *Client) ConferenceCreate(ctx context.Context, name, callControlID string) (string, error) {
	body := map[string]any{
		"name":            name,
		"call_control_id": callControlID,
		// The agent keeps talking to the caller through the media stream; a
		// hold tune underneath it would be heard by both.
		"hold_audio_url":             "",
		"start_conference_on_create": true,
	}
	var res struct {
		Data struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/conferences", body, &res); err != nil {
		return "", err
	}
	return res.Data.ID, nil
}

// ConferenceJoin adds another leg to an existing conference.
//
// The listener joins muted and deaf=false: they hear everything, and nothing
// they do is heard until they choose to unmute, so an operator dropping in
// cannot accidentally speak over a live sales call.
func (c *Client) ConferenceJoin(ctx context.Context, conferenceID, callControlID string, muted bool) error {
	body := map[string]any{
		"call_control_id": callControlID,
		"mute":            muted,
		// Silence the join/leave beeps: the caller would hear them and has no
		// idea anyone else is on the line.
		"beep_enabled": "never",
		"hold":         false,
	}
	return c.do(ctx, http.MethodPost, "/conferences/"+conferenceID+"/actions/join", body, nil)
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
		// Which leg our audio is played onto. Documented default is
		// "opposite", which we were relying on without ever saying so — and it
		// is the one parameter in this feature whose meaning depends on the
		// call's leg topology, so it is the wrong thing to leave implicit on a
		// call that gets conferenced, transferred, or handled by answering
		// machine detection. "both" is unambiguous.
		"stream_bidirectional_target_legs": "both",
	}
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/streaming_start", body, nil)
}

// StreamingStop ends the media fork started by StreamingStart.
//
// REQUIRED BEFORE WALKING AWAY FROM A CALL. Closing our end of the websocket
// does not end the fork: streaming_start stays active on the call, so the
// carrier simply reconnects, the media handler treats the new socket as a fresh
// call, and it plays the greeting and starts another pipeline. That is exactly
// what a transferred caller heard — the agent said goodbye, left, and then
// introduced itself again to a call it had already handed over.
func (c *Client) StreamingStop(ctx context.Context, callControlID string) error {
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/streaming_stop", nil, nil)
}

// Answer answers an inbound call (unused for outbound tests but handy).
func (c *Client) Answer(ctx context.Context, callControlID string) error {
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/answer", nil, nil)
}

// Transfer hands the call to another destination.
//
// The existing leg is connected to `to` — a DID in E.164, or a SIP URI. Our
// media fork is not part of that bridge, so the agent should stop speaking and
// release its pipeline once this returns; the carrier owns the conversation
// from that point.
//
// `from` is the caller ID presented to the transfer destination. Pass the
// tenant's own number (the one the call was placed from) rather than the
// contact's, so the person receiving the transfer sees who is transferring
// rather than who is being transferred.
//
// A transfer that cannot be completed hangs up the NEW leg and leaves the
// original call active, so failure here is recoverable — the agent still has
// the caller and can tell them it did not work instead of dropping them.
func (c *Client) Transfer(ctx context.Context, callControlID, to, from string, timeoutSecs int) error {
	body := map[string]any{"to": to}
	if from != "" {
		body["from"] = from
	}
	if timeoutSecs > 0 {
		body["timeout_secs"] = timeoutSecs
	}
	return c.do(ctx, http.MethodPost, "/calls/"+callControlID+"/actions/transfer", body, nil)
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
