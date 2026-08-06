package rexa

import "testing"

// catalog mirrors the shape of the real kokoro list: descriptive labels with
// the speaker name first.
var catalog = map[string]int{
	"Bella (US female)": 2,
	"Heart (US female)": 3,
	"Emma (UK female)":  21,
	"Michael (US male)": 16,
}

func TestResolveByCatalogName(t *testing.T) {
	r := NewVoiceResolver(catalog, nil, 3)
	for _, in := range []string{"bella", "Bella", "BELLA", "  bella  ", "Bella (US female)"} {
		got, ok := r.Resolve(in)
		if !ok || got != 2 {
			t.Errorf("Resolve(%q) = (%d, %v), want (2, true)", in, got, ok)
		}
	}
}

// The override map is the seam for reconciling the two catalogues without a
// code change, so it must beat everything else.
func TestOverrideWins(t *testing.T) {
	r := NewVoiceResolver(catalog, map[string]int{"leah": 21, "bella": 16}, 3)
	if got, ok := r.Resolve("leah"); !ok || got != 21 {
		t.Errorf("Resolve(leah) = (%d, %v), want (21, true)", got, ok)
	}
	// Override must take precedence over the catalogue's own Bella.
	if got, _ := r.Resolve("bella"); got != 16 {
		t.Errorf("Resolve(bella) = %d, want 16 — override should win", got)
	}
}

func TestResolveBareInteger(t *testing.T) {
	r := NewVoiceResolver(catalog, nil, 3)
	if got, ok := r.Resolve("21"); !ok || got != 21 {
		t.Errorf("Resolve(21) = (%d, %v), want (21, true)", got, ok)
	}
}

// An unknown voice must never fail the call — a dialled caller would rather
// hear the wrong voice than dead air.
func TestUnknownVoiceFallsBack(t *testing.T) {
	r := NewVoiceResolver(catalog, nil, 3)
	got, ok := r.Resolve("some-voice-we-have-never-heard-of")
	if got != 3 {
		t.Errorf("id = %d, want the fallback 3", got)
	}
	if ok {
		t.Error("ok = true for an unknown voice; caller cannot detect the mismatch")
	}
}

func TestEmptyVoiceFallsBack(t *testing.T) {
	r := NewVoiceResolver(catalog, nil, 7)
	if got, ok := r.Resolve(""); got != 7 || ok {
		t.Errorf("Resolve(\"\") = (%d, %v), want (7, false)", got, ok)
	}
}

// A campaign misconfigured with one bad voice would otherwise log once per
// call and bury everything else in the log.
func TestUnknownVoiceWarnsOnce(t *testing.T) {
	r := NewVoiceResolver(catalog, nil, 3)
	for i := 0; i < 5; i++ {
		r.Resolve("mystery")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.warned) != 1 {
		t.Errorf("warned = %v, want exactly one entry", r.warned)
	}
}

// Distinct unknown voices are distinct problems and each deserves a line.
func TestDistinctUnknownVoicesEachWarn(t *testing.T) {
	r := NewVoiceResolver(catalog, nil, 3)
	r.Resolve("one")
	r.Resolve("two")
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.warned) != 2 {
		t.Errorf("warned = %v, want two entries", r.warned)
	}
}
