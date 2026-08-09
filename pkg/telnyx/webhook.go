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
		} `json:"payload"`
	} `json:"data"`
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
