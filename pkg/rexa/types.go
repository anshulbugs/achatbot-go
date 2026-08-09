package rexa

import (
	"encoding/json"
	"fmt"
	"time"
)

// Wire types for the platform ↔ agent contract.
//
// These mirror packages/agent-contract/src/schemas.ts in the jtvapi repo,
// which is the runtime source of truth. Deliberately NOT modelled on the
// sibling openapi.yaml: that file is a stale v0 draft describing a nested
// voice{}/reporting{} shape and a `destination` field that no current code
// sends. Fields below are named exactly as they appear on the wire.
//
// Two shapes the platform sends are close but not identical, and the
// difference is load-bearing rather than accidental:
//
//   - /connection (outbound) carries `direction` + `voicemail_message` and no
//     carrier call id, because the call does not exist until we place it.
//   - /incoming (inbound) carries `CCID` — the carrier's call-control id for a
//     leg that is already ringing — and no voicemail_message, because there is
//     nobody to leave a message for. The capitalisation of CCID is literal;
//     the platform's verifier checks that exact key.

// ISOTime formats t for the wire.
//
// The platform validates timestamps with Zod's `z.string().datetime()`, which
// accepts only UTC with a literal `Z` — an RFC3339 string carrying a numeric
// offset like +05:30 is rejected and takes the whole report down with it. So
// convert to UTC and pin millisecond precision rather than using
// time.RFC3339Nano, whose trailing-zero trimming produces variable precision.
func ISOTime(t time.Time) string {
	return t.UTC().Format("2006-01-02T15:04:05.000Z")
}

// ─── Credentials ────────────────────────────────────────────────────

// TelecomCredentials is the platform's discriminated credential envelope.
//
// Credentials are per-call and tenant-owned (BYO): the platform decrypts them
// out of its vault for each dispatch. They must never be written to disk or
// logged, and should be dropped once the call terminates.
type TelecomCredentials struct {
	Provider    string          `json:"provider"`
	Credentials json.RawMessage `json:"credentials"`
}

// TelnyxCredentials is the `credentials` shape when Provider == "telnyx".
type TelnyxCredentials struct {
	APIKey       string `json:"api_key"`
	ConnectionID string `json:"connection_id"`
	ProfileID    string `json:"profile_id,omitempty"`
}

// Telnyx decodes the credential blob as Telnyx credentials.
//
// Returns an error for any other provider rather than a zero value, so a
// misrouted Twilio dispatch fails at the boundary with a clear message instead
// of attempting a Telnyx dial with empty credentials.
func (c TelecomCredentials) Telnyx() (TelnyxCredentials, error) {
	var out TelnyxCredentials
	if c.Provider != "telnyx" {
		return out, fmt.Errorf("expected provider telnyx, got %q", c.Provider)
	}
	if err := json.Unmarshal(c.Credentials, &out); err != nil {
		return out, fmt.Errorf("decode telnyx credentials: %w", err)
	}
	if out.APIKey == "" || out.ConnectionID == "" {
		return out, fmt.Errorf("telnyx credentials require api_key and connection_id")
	}
	return out, nil
}

// ─── Platform → agent ───────────────────────────────────────────────

// PhoneDispatchRequest is the body of POST /connection — an outbound call.
type PhoneDispatchRequest struct {
	SessionID          string             `json:"session_id"`
	TenantID           string             `json:"tenant_id"`
	ToNumber           string             `json:"to_number"`
	FromNumber         string             `json:"from_number"`
	Direction          string             `json:"direction,omitempty"`
	TelecomCredentials TelecomCredentials `json:"telecom_credentials"`
	// Voice is a bare id in the platform's vocabulary (e.g. "leah"), not a
	// kokoro speaker id. Resolve it through the voice map before use.
	Voice string `json:"voice"`
	// Language is an ISO 639-1 two-letter code, NOT a BCP-47 tag. The
	// contract's older BCP-47 fields belong to the deferred voice{} object.
	Language         string `json:"language,omitempty"`
	SystemPrompt     string `json:"system_prompt"`
	HelloMessage     string `json:"hello_message"`
	VoicemailMessage string `json:"voicemail_message"`
	WebhookURL       string `json:"webhook_url"`
	// TransferNumber is set only when the tenant enabled transfer AND
	// configured a number. Its presence is the signal that we may offer to
	// transfer on this call.
	TransferNumber string `json:"transfer_number,omitempty"`
	// SentimentAnalysis and SentimentWebhook always travel as a pair; the
	// platform's builder omits both unless the session opted in.
	SentimentAnalysis bool   `json:"sentiment_analysis,omitempty"`
	SentimentWebhook  string `json:"sentiment_webhook,omitempty"`
	DisplayName       string `json:"display_name,omitempty"`
	// Redis connection for live call state. Optional and per-tenant: when
	// absent nothing is published and the call is unaffected.
	RedisHost     string `json:"redis_host,omitempty"`
	RedisPort     int    `json:"redis_port,omitempty"`
	RedisDB       int    `json:"redis_db,omitempty"`
	RedisPassword string `json:"redis_password,omitempty"`
}

// Redis extracts the live-state target from a dispatch.
func (r PhoneDispatchRequest) Redis() RedisTarget {
	return RedisTarget{Host: r.RedisHost, Port: r.RedisPort, DB: r.RedisDB, Password: r.RedisPassword}
}

// IncomingDispatchRequest is the body of POST /incoming — a call already
// ringing on a carrier leg we are being asked to answer.
type IncomingDispatchRequest struct {
	// CCID is the carrier's call-control id for the live leg (Telnyx's
	// call_control_id). Capitalisation is literal and checked by the platform.
	CCID               string             `json:"CCID"`
	SessionID          string             `json:"session_id"`
	TenantID           string             `json:"tenant_id"`
	FromNumber         string             `json:"from_number"`
	ToNumber           string             `json:"to_number"`
	TelecomCredentials TelecomCredentials `json:"telecom_credentials"`
	Voice              string             `json:"voice"`
	Language           string             `json:"language,omitempty"`
	SystemPrompt       string             `json:"system_prompt"`
	HelloMessage       string             `json:"hello_message"`
	// TransferNumber is nullable on this path — the platform sends an explicit
	// null when the inbound number has no transfer target configured.
	TransferNumber    string `json:"transfer_number,omitempty"`
	WebhookURL        string `json:"webhook_url"`
	SentimentAnalysis bool   `json:"sentiment_analysis,omitempty"`
	SentimentWebhook  string `json:"sentiment_webhook,omitempty"`
	RedisHost         string `json:"redis_host,omitempty"`
	RedisPort         int    `json:"redis_port,omitempty"`
	RedisDB           int    `json:"redis_db,omitempty"`
	RedisPassword     string `json:"redis_password,omitempty"`
}

// Redis extracts the live-state target from an inbound dispatch.
func (r IncomingDispatchRequest) Redis() RedisTarget {
	return RedisTarget{Host: r.RedisHost, Port: r.RedisPort, DB: r.RedisDB, Password: r.RedisPassword}
}

// WebrtcDispatchRequest is the body of POST /connection_webrtc.
//
// Mirrors the phone payload minus everything telephony-specific: a browser
// room has no PSTN leg, so there are no numbers, no credentials, no voicemail
// message and no direction.
type WebrtcDispatchRequest struct {
	SessionID    string `json:"session_id"`
	TenantID     string `json:"tenant_id"`
	Voice        string `json:"voice"`
	Language     string `json:"language,omitempty"`
	SystemPrompt string `json:"system_prompt"`
	HelloMessage string `json:"hello_message"`
	WebhookURL   string `json:"webhook_url"`
}

// ─── Agent → platform (responses) ───────────────────────────────────

// HealthResponse is the body of GET /health. The platform's load balancer
// probes it every 5s and caches the result for 10s.
type HealthResponse struct {
	Status bool `json:"status"`
}

// DispatchResponse acknowledges a phone dispatch.
//
// The platform accepts any non-empty `status` string and reads either
// `agent_session_id` or `uuid` for correlation, so this shape is forgiving by
// design — new vocabulary will not bounce a dispatch into a retry loop.
type DispatchResponse struct {
	Status         string `json:"status"`
	AgentSessionID string `json:"agent_session_id,omitempty"`
	UUID           string `json:"uuid,omitempty"`
}

// WebrtcDispatchResponse returns the Daily room the browser should join.
//
// The platform requires room_url + token and stores them verbatim; the two
// optional fields are accepted when present but the v1.0 platform never
// assumes them.
type WebrtcDispatchResponse struct {
	RoomURL         string `json:"room_url"`
	Token           string `json:"token"`
	AgentSessionID  string `json:"agent_session_id,omitempty"`
	TokenTTLSeconds int    `json:"token_ttl_seconds,omitempty"`
}

// Agent error codes. The platform branches on these: it retries the
// transient ones and fails the session immediately on
// ErrCodeProviderCredentialsInvalid, so returning the wrong code turns a
// permanent misconfiguration into an infinite retry or vice versa.
const (
	ErrCodeInvalidRequest       = "invalid_request"
	ErrCodeUnauthenticated      = "unauthenticated"
	ErrCodeNotFound             = "not_found"
	ErrCodeAtCapacity           = "at_capacity"
	ErrCodeProviderCredsInvalid = "provider_credentials_invalid"
	ErrCodeProviderUnavailable  = "provider_unavailable"
	ErrCodeInternal             = "internal_error"
)

// AgentError is the error envelope for any non-2xx response.
type AgentError struct {
	Error AgentErrorBody `json:"error"`
}

// AgentErrorBody carries the machine-readable code and human message.
type AgentErrorBody struct {
	Code      string         `json:"code"`
	Message   string         `json:"message"`
	RequestID string         `json:"request_id,omitempty"`
	Details   map[string]any `json:"details,omitempty"`
}

// NewAgentError builds an error envelope.
func NewAgentError(code, message string) AgentError {
	return AgentError{Error: AgentErrorBody{Code: code, Message: message}}
}

// ─── Agent → platform (callbacks) ───────────────────────────────────

// Call statuses. This is a closed enum on the platform side: an unrecognised
// value fails validation and the entire report is dropped.
const (
	CallStatusCompleted = "completed"
	CallStatusFailed    = "failed"
	CallStatusNoAnswer  = "no_answer"
	CallStatusVoicemail = "voicemail"
	CallStatusBusy      = "busy"
)

// Canonical end reasons. Unlike CallStatus this is a free string on the wire —
// the platform normalises unknown values — but sending a canonical one skips
// its guesswork entirely.
//
// The human-hangup pair is direction-aware: an outbound call reports
// EndReasonCalleeHungUp (we dialled them), an inbound call reports
// EndReasonCallerHungUp (they dialled us). Sending the wrong one is not fatal —
// the platform re-derives it from the session direction — but it is free to be
// correct.
const (
	EndReasonCompleted     = "completed"
	EndReasonCallerHungUp  = "caller_hung_up"
	EndReasonCalleeHungUp  = "callee_hung_up"
	EndReasonAgentHungUp   = "agent_hung_up"
	EndReasonVoicemail     = "voicemail"
	EndReasonNoAnswer      = "no_answer"
	EndReasonBusy          = "busy"
	EndReasonError         = "error"
	EndReasonProviderFail  = "provider_failure"
	EndReasonPlatformEnded = "platform_end_call"
)

// MessageTurn is one turn of the inline transcript.
//
// Content is omitempty because a turn can legitimately carry no spoken text.
// T is seconds since call start; it is a pointer so that a genuine 0.0 (the
// greeting, always the first turn) survives serialisation instead of being
// dropped by omitempty and losing the transcript's anchor.
type MessageTurn struct {
	Role    string   `json:"role"`
	Content string   `json:"content,omitempty"`
	T       *float64 `json:"t,omitempty"`
}

// Transcript roles the platform understands. It maps "assistant" onto "agent"
// and drops system/tool turns before relaying to tenants, but emitting the
// canonical names avoids relying on that.
const (
	RoleAgent = "agent"
	RoleUser  = "user"
)

// EndOfCallReport is POSTed to the dispatch's webhook_url when a call ends.
//
// This is the v1 minimum the platform requires, plus the optional content
// fields we can actually produce. Deliberately excludes disposition_code,
// question_answers, function_invocations, opt_out_events and cost_signals:
// those correspond to agent features that do not exist here, and the platform
// treats all of them as optional.
//
// The report must be emitted for EVERY call that was dispatched, including
// ones where no pipeline ever ran — voicemail, no-answer and busy all still
// need a report, so the emitter has to hang off the carrier call lifecycle
// rather than off pipeline teardown.
type EndOfCallReport struct {
	SessionID  string `json:"session_id"`
	TenantID   string `json:"tenant_id"`
	CallStatus string `json:"call_status"`
	EndReason  string `json:"end_reason"`

	StartedAt string `json:"started_at,omitempty"`
	EndedAt   string `json:"ended_at,omitempty"`
	// DurationSeconds is sent whenever we know it. When absent the platform
	// derives it from the timestamps, so an inconsistent trio is worse than an
	// incomplete one.
	DurationSeconds int `json:"duration_seconds,omitempty"`

	RecordingURL string `json:"recording_url,omitempty"`
	// CCID is the carrier call id. The platform uses it to fetch a
	// provider-side recording after the call, so send it whenever we have it.
	CCID     string        `json:"CCID,omitempty"`
	Messages []MessageTurn `json:"messages,omitempty"`
}

// TransferInitiatedEvent is POSTed to the same webhook_url mid-call when we
// hand the call to the configured transfer number. The Type discriminator is
// how the platform routes it away from the end-of-call handler.
type TransferInitiatedEvent struct {
	Type           string `json:"type"`
	SessionID      string `json:"session_id"`
	TenantID       string `json:"tenant_id"`
	TransferNumber string `json:"transfer_number"`
	TransferredAt  string `json:"transferred_at,omitempty"`
}

// NewTransferInitiated builds a transfer event with the discriminator set.
func NewTransferInitiated(sessionID, tenantID, number string, at time.Time) TransferInitiatedEvent {
	return TransferInitiatedEvent{
		Type:           "transfer_initiated",
		SessionID:      sessionID,
		TenantID:       tenantID,
		TransferNumber: number,
		TransferredAt:  ISOTime(at),
	}
}

// RecordingURLs is the per-format set of links to one recording.
type RecordingURLs struct {
	MP3 string `json:"mp3,omitempty"`
	WAV string `json:"wav,omitempty"`
}

// RecordingSavedEvent is POSTed to webhook_url once the carrier has finalised a
// recording.
//
// Separate from the end-of-call report because finalisation lags the call's end
// by tens of seconds — transcoding and upload — and holding the report back for
// it would delay every disposition the platform acts on. The platform re-emits
// this to tenants as `session.recording_completed`.
type RecordingSavedEvent struct {
	Type      string `json:"type"`
	SessionID string `json:"session_id"`
	TenantID  string `json:"tenant_id"`
	// CallControlID is provider-internal; the platform strips it before
	// relaying to a tenant.
	CallControlID string `json:"call_control_id,omitempty"`
	RecordingID   string `json:"recording_id"`
	// Status is "completed" or "failed". The platform tolerates an empty
	// string and infers from URL presence, but we always set it.
	Status string `json:"status"`
	// Channels is "single" or "dual" as the carrier reports it.
	Channels           string `json:"channels,omitempty"`
	RecordingStartedAt string `json:"recording_started_at,omitempty"`
	RecordingEndedAt   string `json:"recording_ended_at,omitempty"`
	// RecordingURLs are the carrier's own links. Telnyx's are pre-signed and
	// expire, which is why the platform prefers PublicRecordingURLs when we
	// have somewhere to host them.
	RecordingURLs *RecordingURLs `json:"recording_urls,omitempty"`
	// PublicRecordingURLs are tenant-fetchable links. Empty until the agent
	// mirrors recordings to storage of its own — see §6 of the contract.
	PublicRecordingURLs *RecordingURLs `json:"public_recording_urls,omitempty"`
}

// NewRecordingSaved builds a recording event with the discriminator set.
func NewRecordingSaved(sessionID, tenantID, ccid, recordingID string) RecordingSavedEvent {
	return RecordingSavedEvent{
		Type:          "recording_saved",
		SessionID:     sessionID,
		TenantID:      tenantID,
		CallControlID: ccid,
		RecordingID:   recordingID,
		Status:        "completed",
	}
}

// Sentiment values the mid-call classifier may report. Closed set — the
// platform's Zod enum rejects anything else, and a rejected event is a silently
// missed alert.
const (
	SentimentWantsHuman       = "wants_human"
	SentimentHighlyInterested = "highly_interested"
	SentimentUserAnnoyed      = "user_annoyed"
)

// SentimentEvent is POSTed MID-CALL to the dispatch's sentiment_webhook — a
// different URL from the end-of-call report, and the only agent callback that
// is worth nothing after the call ends.
//
// The platform jumps it to the front of its delivery queue and fires operator
// alerts from it, so an alert saying "the caller wants a human" reaches someone
// while the caller is still on the line.
type SentimentEvent struct {
	SessionID string `json:"session_id"`
	TenantID  string `json:"tenant_id"`
	// CallStatus is the call's state at the moment of detection ("in_progress").
	CallStatus string `json:"call_status,omitempty"`
	CCID       string `json:"CCID,omitempty"`
	Sentiment  string `json:"sentiment_value"`
	// DailyRoomURL lets an operator join the live call. Empty until the WebRTC
	// path exists; the platform's alert template degrades to no join link.
	DailyRoomURL string `json:"daily_room_url,omitempty"`
}
