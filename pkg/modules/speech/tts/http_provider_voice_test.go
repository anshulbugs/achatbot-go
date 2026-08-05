package tts

import "testing"

// A wrong voice is the quietest possible failure here: the service accepts any
// name it knows, falls back to a default for one it does not, and returns
// perfectly good audio either way. Nothing errors, nothing logs, and the caller
// simply hears a different person. These tests pin the mapping so that can only
// happen deliberately.

func TestSupertonicIDsAreDisjointFromKokoro(t *testing.T) {
	for id := range supertonicSidToName {
		if _, clash := kokoroSidToName[id]; clash {
			t.Fatalf("speaker id %d exists in both engines; a stale id from one "+
				"would silently select a voice in the other", id)
		}
	}
}

func TestResolveVoiceUsesEngineTable(t *testing.T) {
	tests := []struct {
		name string
		p    *HTTPTTSProvider
		want string
	}{
		{
			name: "supertonic maps its own id",
			p:    &HTTPTTSProvider{sid: 101, sidToName: supertonicSidToName, defaultVoice: "F2"},
			want: "F2",
		},
		{
			name: "supertonic maps a male id",
			p:    &HTTPTTSProvider{sid: 107, sidToName: supertonicSidToName, defaultVoice: "F2"},
			want: "M3",
		},
		{
			// A kokoro id reaching a supertonic provider must not resolve to a
			// kokoro name — the service would reject it, which is the loud
			// failure we want, rather than synthesizing the wrong voice.
			name: "supertonic falls back for a kokoro id",
			p:    &HTTPTTSProvider{sid: 3, sidToName: supertonicSidToName, defaultVoice: "F2"},
			want: "F2",
		},
		{
			name: "kokoro still maps its own id",
			p:    &HTTPTTSProvider{sid: 3},
			want: "af_heart",
		},
		{
			name: "explicit voiceName wins over any table",
			p:    &HTTPTTSProvider{sid: 101, voiceName: "M5", sidToName: supertonicSidToName},
			want: "M5",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.resolveVoice(); got != tc.want {
				t.Errorf("resolveVoice() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSupertonicVoiceNamesMatchServiceStyles(t *testing.T) {
	// deploy/tts/supertonic/server.py exposes exactly these ten style files;
	// asking for anything else is a 400. Keep the two lists in step.
	want := map[string]bool{
		"F1": true, "F2": true, "F3": true, "F4": true, "F5": true,
		"M1": true, "M2": true, "M3": true, "M4": true, "M5": true,
	}
	if len(supertonicSidToName) != len(want) {
		t.Fatalf("have %d supertonic voices, service exposes %d", len(supertonicSidToName), len(want))
	}
	for id, name := range supertonicSidToName {
		if !want[name] {
			t.Errorf("id %d maps to %q, which the service does not expose", id, name)
		}
	}
}
