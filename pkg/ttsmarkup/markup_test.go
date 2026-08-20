package ttsmarkup

import "testing"

func TestStrip(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain text untouched", "Hello there.", "Hello there."},
		{"empty", "", ""},
		{"stress up", "That part is [free](+1).", "That part is free."},
		{"stress two", "That is [exactly](+2) right!", "That is exactly right!"},
		{"stress down", "One [of the](-1) recruiters.", "One of the recruiters."},
		{"phonemes", "Welcome to [JobTalk](/dʒˈɑbtɔk/).", "Welcome to JobTalk."},
		{
			"several in one turn",
			"[Ooh](+1) — [JobTalk](/dʒˈɑbtɔk/) is [free](+1) for you.",
			"Ooh — JobTalk is free for you.",
		},
		{"multi-word span", "[your recruiters](+1) will see it.", "your recruiters will see it."},

		// Left alone on purpose: not our markup.
		{"markdown link kept whole", "See [docs](https://example.com).", "See [docs](https://example.com)."},
		{"bare brackets", "[whisper] hello", "[whisper] hello"},
		{"two-digit stress is not ours", "[free](+12)", "[free](+12)"},
		{"unclosed phonemes", "[free](/dʒ)", "[free](/dʒ)"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Strip(c.in); got != c.want {
				t.Errorf("Strip(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}

// A malformed opening bracket must not let one match swallow the rest of the
// reply — that would delete words the caller heard.
func TestStripDoesNotRunAway(t *testing.T) {
	in := "[ I said hello and then [free](+1) and then goodbye."
	want := "[ I said hello and then free and then goodbye."
	if got := Strip(in); got != want {
		t.Errorf("Strip(%q) = %q, want %q", in, got, want)
	}
}

func TestHas(t *testing.T) {
	if Has("nothing here") {
		t.Error("Has reported markup in plain text")
	}
	if !Has("a [word](+1) here") {
		t.Error("Has missed stress markup")
	}
	if !Has("a [word](/wˈɜɹd/) here") {
		t.Error("Has missed phoneme markup")
	}
}
