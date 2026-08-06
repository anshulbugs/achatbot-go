package rexa

import (
	"log"
	"strconv"
	"strings"
	"sync"
)

// VoiceResolver maps the platform's voice vocabulary onto local TTS speaker
// ids.
//
// The two systems name voices independently: the platform sends opaque ids
// like "leah" from its own `voices` table, while kokoro addresses speakers by
// integer with names like "Bella" and "Heart". Nothing guarantees the two
// catalogues ever agree, so resolution is deliberately forgiving — an
// unrecognised voice degrades to the configured default and logs, rather than
// failing a dispatch. A caller who has already been dialled would much rather
// hear the wrong voice than dead air.
//
// Resolution order, first match wins:
//
//  1. An explicit override from config (`rexa.voice_map`). This is the seam
//     for reconciling the two catalogues without a code change.
//  2. A case-insensitive match against the local catalogue's names, so a
//     platform voice literally called "bella" lands on kokoro's Bella.
//  3. A bare integer, so the platform can address a speaker id directly.
//  4. The fallback id.
type VoiceResolver struct {
	overrides map[string]int
	byName    map[string]int
	fallback  int

	// warned dedupes the unknown-voice log. Without it a campaign configured
	// with one bad voice emits a line per call, which buries everything else.
	mu     sync.Mutex
	warned map[string]bool
}

// NewVoiceResolver builds a resolver.
//
//   - catalog maps local voice names to speaker ids (e.g. "Heart" → 3). Names
//     are matched case-insensitively, and anything after the first space is
//     ignored so catalogue labels like "Bella (US female)" still match "bella".
//   - overrides maps platform voice ids to local speaker ids and wins over
//     everything else.
//   - fallback is used when nothing matches. It should be a voice that is
//     definitely present — normally the configured default speaker.
func NewVoiceResolver(catalog map[string]int, overrides map[string]int, fallback int) *VoiceResolver {
	r := &VoiceResolver{
		overrides: make(map[string]int, len(overrides)),
		byName:    make(map[string]int, len(catalog)),
		fallback:  fallback,
		warned:    make(map[string]bool),
	}
	for k, v := range overrides {
		r.overrides[normaliseVoice(k)] = v
	}
	for name, id := range catalog {
		r.byName[normaliseVoice(name)] = id
	}
	return r
}

// normaliseVoice lowercases and strips the descriptive suffix from a catalogue
// label, so "Bella (US female)" and "bella" are the same key.
func normaliseVoice(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if i := strings.IndexAny(s, " ("); i > 0 {
		s = s[:i]
	}
	return s
}

// Resolve returns the local speaker id for a platform voice, and whether it
// was matched rather than defaulted. The bool is for callers that want to
// surface the mismatch; the id is always usable.
func (r *VoiceResolver) Resolve(voice string) (int, bool) {
	key := normaliseVoice(voice)
	if key == "" {
		return r.fallback, false
	}
	if id, ok := r.overrides[key]; ok {
		return id, true
	}
	if id, ok := r.byName[key]; ok {
		return id, true
	}
	// A bare integer addresses a speaker directly. Parse the original string,
	// not the normalised key, so nothing has been trimmed away.
	if id, err := strconv.Atoi(strings.TrimSpace(voice)); err == nil {
		return id, true
	}
	r.warnOnce(voice)
	return r.fallback, false
}

func (r *VoiceResolver) warnOnce(voice string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.warned[voice] {
		return
	}
	r.warned[voice] = true
	log.Printf("rexa: unknown platform voice %q — using fallback speaker %d. "+
		"Add it to rexa.voice_map to silence this.", voice, r.fallback)
}
