package telnyx

import (
	"encoding/base64"
	"encoding/json"
)

// WebhookEnvelope is the Telnyx Call Control webhook event shape.
type WebhookEnvelope struct {
	Data struct {
		EventType string `json:"event_type"`
		Payload   struct {
			CallControlID string `json:"call_control_id"`
			ClientState   string `json:"client_state"`
			From          string `json:"from"`
			To            string `json:"to"`
			HangupCause   string `json:"hangup_cause"`
			// Direction is "incoming" for calls dialed *to* one of our numbers.
			Direction string `json:"direction"`
			// Result carries the answering-machine-detection outcome on
			// call.machine.*.ended events: human, machine, not_sure, silence,
			// fax_detected, beep_detected, ended.
			Result string `json:"result"`

			// Recording fields, present on call.recording.saved.
			//
			// RecordingURLs are pre-signed and EXPIRE — Telnyx documents ten
			// minutes. Anything that wants the audio later has to fetch and
			// re-host it; passing the link on unchanged produces a 403 for
			// whoever opens it an hour after the call.
			RecordingID      string         `json:"recording_id"`
			Channels         string         `json:"channels"`
			RecordingURLs    *RecordingURLs `json:"recording_urls"`
			PublicURLs       *RecordingURLs `json:"public_recording_urls"`
			RecordingStarted string         `json:"recording_started_at"`
			RecordingEnded   string         `json:"recording_ended_at"`
		} `json:"payload"`
	} `json:"data"`
}

// RecordingURLs is Telnyx's per-format link set for one recording.
type RecordingURLs struct {
	MP3 string `json:"mp3"`
	WAV string `json:"wav"`
}

// ParseWebhook decodes a Telnyx webhook body.
func ParseWebhook(body []byte) (*WebhookEnvelope, error) {
	var e WebhookEnvelope
	if err := json.Unmarshal(body, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// encodeState base64-encodes a client_state string as Telnyx requires.
func encodeState(s string) string {
	if s == "" {
		return ""
	}
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// DecodeState reverses encodeState; returns the input unchanged if it is not
// valid base64 (Telnyx echoes exactly what was sent).
func DecodeState(s string) string {
	if s == "" {
		return ""
	}
	if b, err := base64.StdEncoding.DecodeString(s); err == nil {
		return string(b)
	}
	return s
}
