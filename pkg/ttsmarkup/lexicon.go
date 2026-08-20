package ttsmarkup

import (
	"regexp"
	"sort"
	"strings"
)

// Lexicon forces the pronunciation of specific words just before synthesis.
//
// This is the deterministic half of phoneme control. The model cannot be
// trusted to spell a brand name in phonemes on every turn — and should not
// have to, since the correct phonemes never change — so the words that matter
// are rewritten here instead, on the way into the TTS request and nowhere
// else.
//
// The defect it fixes is live and audible: transcripts of our own output come
// back with "JobTalk" read as two words, "Job Talk". Contact and place names
// are the obvious next casualties.
//
// A zero Lexicon (or a nil one) applies nothing, so the feature costs nothing
// when no words are configured.
type Lexicon struct {
	re    *regexp.Regexp
	byKey map[string]string // lowercased word -> phonemes, without slashes
}

// NewLexicon compiles a word-to-phoneme table. Keys are matched whole-word and
// case-insensitively; values are misaki phoneme strings written WITHOUT the
// surrounding slashes (e.g. "ʤˈɑbtˌɔk").
//
// Returns nil for an empty table so callers can keep the field nil and skip
// the work entirely.
//
// Note the alphabet: kokoro's American English uses misaki's vocabulary, not
// plain IPA. Affricates are single characters there — "ʤ" and "ʧ", never "dʒ"
// or "tʃ" — and the diphthongs are written A I O W Y. A character outside that
// vocabulary is dropped by the model's tokeniser, which quietly changes the
// word rather than failing, so entries are worth hearing once before trusting.
func NewLexicon(table map[string]string) *Lexicon {
	if len(table) == 0 {
		return nil
	}
	l := &Lexicon{byKey: make(map[string]string, len(table))}
	keys := make([]string, 0, len(table))
	for word, phonemes := range table {
		word = strings.TrimSpace(word)
		phonemes = strings.Trim(strings.TrimSpace(phonemes), "/")
		if word == "" || phonemes == "" {
			continue
		}
		l.byKey[strings.ToLower(word)] = phonemes
		keys = append(keys, regexp.QuoteMeta(word))
	}
	if len(keys) == 0 {
		return nil
	}
	// Longest first, so a multi-word entry wins over a single-word one that is
	// a prefix of it.
	sort.Slice(keys, func(i, j int) bool { return len(keys[i]) > len(keys[j]) })
	l.re = regexp.MustCompile(`(?i)\b(?:` + strings.Join(keys, "|") + `)\b`)
	return l
}

// Apply wraps every configured word in phoneme markup, leaving the visible
// text — and its original capitalisation — unchanged.
//
// "Welcome to JobTalk." becomes "Welcome to [JobTalk](/ʤˈɑbtˌɔk/)."
//
// Text that is already inside markup is left alone, so a model that spelled
// out its own pronunciation for one turn does not end up with two.
func (l *Lexicon) Apply(s string) string {
	if l == nil || l.re == nil || s == "" {
		return s
	}
	var b strings.Builder
	last := 0
	for _, span := range protectedSpans(s) {
		b.WriteString(l.replace(s[last:span[0]]))
		b.WriteString(s[span[0]:span[1]])
		last = span[1]
	}
	b.WriteString(l.replace(s[last:]))
	return b.String()
}

func (l *Lexicon) replace(s string) string {
	if s == "" {
		return s
	}
	return l.re.ReplaceAllStringFunc(s, func(word string) string {
		phonemes, ok := l.byKey[strings.ToLower(word)]
		if !ok {
			return word
		}
		return "[" + word + "](/" + phonemes + "/)"
	})
}

// protectedSpans returns the byte ranges of existing markup, which Apply must
// not rewrite.
func protectedSpans(s string) [][]int {
	if !strings.ContainsRune(s, '[') {
		return nil
	}
	return markupRe.FindAllStringIndex(s, -1)
}
