package rexa

import (
	"encoding/json"
	"testing"
)

// A real Telnyx call.recording.saved payload, taken off the wire.
const telnyxRecordingPayload = `{
  "call_control_id": "v3:fRzVYv7lYUo3SmaONWAl1iufgzA0Hy9lZFlM7woxzLrfOoyunO9Jjg",
  "channels": "dual",
  "public_recording_urls": {},
  "recording_ended_at": "2026-08-09T08:04:45.130756Z",
  "recording_id": "e565673e-216d-45a7-a345-97a54308b5bd",
  "recording_started_at": "2026-08-09T08:03:22.793076Z",
  "recording_urls": {"wav": "s3://rexa-recordings/fdba0877/2026-08-09/c896dd46.wav"},
  "status": ""
}`

func decodePayload(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(telnyxRecordingPayload), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestRecordingIsForwardedUnchanged(t *testing.T) {
	// A pass-through, not a re-mapping. Typing these fields would silently drop
	// every field Telnyx adds and quietly break on every field it renames.
	src := decodePayload(t)
	evt := NewRecordingSaved(src, "sess-1", "tenant-1")

	blob, err := json.Marshal(evt)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(blob, &got); err != nil {
		t.Fatal(err)
	}

	for k, want := range src {
		gotJSON, _ := json.Marshal(got[k])
		wantJSON, _ := json.Marshal(want)
		if string(gotJSON) != string(wantJSON) {
			t.Errorf("field %q = %s, want %s", k, gotJSON, wantJSON)
		}
	}
	if got["type"] != "recording_saved" {
		t.Errorf("type = %v", got["type"])
	}
	if got["session_id"] != "sess-1" || got["tenant_id"] != "tenant-1" {
		t.Errorf("ids not added: %v / %v", got["session_id"], got["tenant_id"])
	}
}

func TestRecordingURLsAreNotRewritten(t *testing.T) {
	// The s3:// form is what Telnyx sends and what the platform expects to
	// receive; "helpfully" converting it would break their handler.
	evt := NewRecordingSaved(decodePayload(t), "s", "t")
	urls, ok := evt["recording_urls"].(map[string]any)
	if !ok {
		t.Fatalf("recording_urls became %T", evt["recording_urls"])
	}
	if urls["wav"] != "s3://rexa-recordings/fdba0877/2026-08-09/c896dd46.wav" {
		t.Fatalf("wav url was rewritten to %v", urls["wav"])
	}
}

func TestEmptyStatusIsPreserved(t *testing.T) {
	// Telnyx sometimes reports "". The platform's handler normalises it by
	// inferring from URL presence; guessing on its behalf would mean two
	// different systems deciding the same thing differently.
	evt := NewRecordingSaved(decodePayload(t), "s", "t")
	if evt["status"] != "" {
		t.Fatalf("status = %q, want it left empty as received", evt["status"])
	}
}

func TestAddedIdsWinOverCarrierFields(t *testing.T) {
	// A carrier payload that happened to carry these keys must not overwrite
	// the ids the platform routes on.
	evt := NewRecordingSaved(map[string]any{
		"session_id": "carrier-value",
		"type":       "something_else",
	}, "sess-1", "tenant-1")
	if evt["session_id"] != "sess-1" || evt["type"] != "recording_saved" {
		t.Fatalf("carrier fields overwrote ours: %v / %v", evt["session_id"], evt["type"])
	}
}

func TestSourcePayloadIsNotMutated(t *testing.T) {
	src := decodePayload(t)
	NewRecordingSaved(src, "s", "t")
	if _, exists := src["type"]; exists {
		t.Fatal("the carrier payload was mutated in place")
	}
}

// The exact payload Telnyx sent on a real call. Note what is NOT in it: there
// is no `status` field, and the platform's schema requires one. A pure
// pass-through was rejected with
//
//	path ["status"] expected "string" received "undefined" — event dropped
//
// and both recordings for that call were lost after being delivered.
const telnyxPayloadNoStatus = `{
  "type": "recording_saved",
  "format": "wav",
  "channels": "single",
  "end_time": "2026-08-09T14:00:06.288441Z",
  "start_time": "2026-08-09T13:58:14.568482Z",
  "call_leg_id": "5b1a1c6a-93fa-11f1-9349-02420a0df31f",
  "client_state": null,
  "recording_id": "bd345af4-6c1b-42a6-b78c-8b350efff00a",
  "connection_id": "2580092767426316174",
  "recording_urls": {"wav": "s3://rexa-recordings/2026-08-09/x.wav"},
  "call_control_id": "v3:i7GbeXWAYMKTLreblOfJD25xhp4ZfbKJ",
  "flow_destination": "non_telnyx_pstn_number",
  "recording_ended_at": "2026-08-09T14:00:06.288441Z",
  "recording_started_at": "2026-08-09T13:58:14.568482Z",
  "public_recording_urls": {}
}`

func TestStatusIsAlwaysPresent(t *testing.T) {
	var src map[string]any
	if err := json.Unmarshal([]byte(telnyxPayloadNoStatus), &src); err != nil {
		t.Fatal(err)
	}
	if _, present := src["status"]; present {
		t.Fatal("the fixture is meant to lack status — that is the whole point of it")
	}

	evt := NewRecordingSaved(src, "sess", "tenant")
	status, ok := evt["status"]
	if !ok {
		t.Fatal("status missing — the platform rejects the event and drops the recording")
	}
	if _, isString := status.(string); !isString {
		t.Fatalf("status is %T, and the schema requires a string", status)
	}
}

func TestCarrierStatusIsNotOverwritten(t *testing.T) {
	// When Telnyx does report one, it wins: inventing a value over a real one
	// would make us disagree with the carrier about what happened.
	evt := NewRecordingSaved(map[string]any{"status": "completed"}, "s", "t")
	if evt["status"] != "completed" {
		t.Fatalf("status = %v, want the carrier's own value", evt["status"])
	}
}
