package ttsmarkup

import "testing"

var table = map[string]string{
	"JobTalk": "ʤˈɑbtˌɔk",
	"Anshul":  "ˈʌnʃʊl",
}

func TestLexiconApply(t *testing.T) {
	l := NewLexicon(table)
	cases := []struct{ name, in, want string }{
		{
			"wraps the brand",
			"Welcome to JobTalk.",
			"Welcome to [JobTalk](/ʤˈɑbtˌɔk/).",
		},
		{
			"case-insensitive, casing preserved",
			"jobtalk and JOBTALK",
			"[jobtalk](/ʤˈɑbtˌɔk/) and [JOBTALK](/ʤˈɑbtˌɔk/)",
		},
		{
			"whole words only",
			"JobTalking is not JobTalk",
			"JobTalking is not [JobTalk](/ʤˈɑbtˌɔk/)",
		},
		{"no configured word present", "Hello there.", "Hello there."},
		{"empty", "", ""},
		{
			"two entries in one turn",
			"Anshul works at JobTalk.",
			"[Anshul](/ˈʌnʃʊl/) works at [JobTalk](/ʤˈɑbtˌɔk/).",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := l.Apply(c.in); got != c.want {
				t.Errorf("Apply(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A word the model already spelled out must not be wrapped a second time —
// "[[JobTalk](/x/)](/y/)" is not valid markup and would reach the caller as
// stray brackets.
func TestLexiconDoesNotDoubleWrap(t *testing.T) {
	l := NewLexicon(table)
	in := "Welcome to [JobTalk](/ʤˈɑbtˌɔk/), says Anshul."
	want := "Welcome to [JobTalk](/ʤˈɑbtˌɔk/), says [Anshul](/ˈʌnʃʊl/)."
	if got := l.Apply(in); got != want {
		t.Errorf("Apply(%q) = %q, want %q", in, got, want)
	}
	if got := l.Apply(l.Apply(in)); got != want {
		t.Errorf("Apply is not idempotent: %q", got)
	}
}

// Stripping what the lexicon added must give back exactly what came in — that
// round trip is what keeps the end-of-call report honest.
func TestLexiconRoundTripsThroughStrip(t *testing.T) {
	l := NewLexicon(table)
	in := "Anshul, welcome to JobTalk."
	if got := Strip(l.Apply(in)); got != in {
		t.Errorf("Strip(Apply(%q)) = %q, want the original", in, got)
	}
}

func TestNewLexiconEmptyIsNil(t *testing.T) {
	if NewLexicon(nil) != nil {
		t.Error("nil table should give a nil Lexicon")
	}
	if NewLexicon(map[string]string{"  ": "x", "y": ""}) != nil {
		t.Error("a table with no usable entries should give a nil Lexicon")
	}
	var l *Lexicon
	if got := l.Apply("untouched"); got != "untouched" {
		t.Errorf("nil Lexicon changed text: %q", got)
	}
}

// Slashes are optional in config, since the markup form has them and it is an
// easy thing to copy across.
func TestNewLexiconTolerartesSlashes(t *testing.T) {
	l := NewLexicon(map[string]string{"JobTalk": "/ʤˈɑbtˌɔk/"})
	want := "[JobTalk](/ʤˈɑbtˌɔk/)"
	if got := l.Apply("JobTalk"); got != want {
		t.Errorf("Apply = %q, want %q", got, want)
	}
}
