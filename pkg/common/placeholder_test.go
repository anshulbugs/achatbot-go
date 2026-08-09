package common

import "testing"

func TestStripPlaceholders(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			// The one seen in production, verbatim from the announcement cache.
			name: "name mid-sentence",
			in:   "Hi {{contact_first_name}}, this is Alex calling from ConnectMobile.",
			want: "Hi, this is Alex calling from ConnectMobile.",
		},
		{
			name: "no placeholder is returned untouched",
			in:   "Hi Sam, this is Alex.",
			want: "Hi Sam, this is Alex.",
		},
		{
			name: "placeholder opens the sentence",
			in:   "{{contact_first_name}}, are you there?",
			want: "are you there?",
		},
		{
			name: "placeholder ends the sentence",
			in:   "Thanks for your time, {{contact_first_name}}.",
			want: "Thanks for your time.",
		},
		{
			name: "several in one line",
			in:   "Hi {{first}}, this is {{agent}} from {{company}}.",
			want: "Hi, this is from.",
		},
		{
			name: "whitespace collapses without eating single spaces",
			in:   "Sorry {{name}} we missed you.",
			want: "Sorry we missed you.",
		},
		{
			name: "square brackets are deliberately left alone",
			in:   "Hi [Agent Name], this is Alex.",
			want: "Hi [Agent Name], this is Alex.",
		},
		{
			name: "empty stays empty",
			in:   "",
			want: "",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := StripPlaceholders(c.in); got != c.want {
				t.Errorf("StripPlaceholders(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}
