package common

import (
	"regexp"
	"strings"
)

// placeholderRe matches an unsubstituted mustache token: {{contact_first_name}},
// {{company}}, and anything else the platform's templating left behind.
//
// Only the mustache form. Square-bracket placeholders like [Agent Name] appear
// in tenant scripts too, but "[" is also ordinary punctuation, and stripping a
// bracketed aside a tenant meant to be read is a worse failure than leaving one
// placeholder in. The system prompt already tells the model to avoid speaking
// those, and unlike the greeting the model's output is not pre-rendered.
var placeholderRe = regexp.MustCompile(`\{\{[^{}]*\}\}`)

// spaceBeforePunctRe finds the gap a removed placeholder leaves in front of
// punctuation: "Hi , this is Alex" -> "Hi, this is Alex".
var spaceBeforePunctRe = regexp.MustCompile(`\s+([,.!?;:])`)

// repeatedPunctRe collapses the doubled punctuation a removed placeholder can
// strand: "Hi ,, this is" -> "Hi, this is".
var repeatedPunctRe = regexp.MustCompile(`([,;:])\s*[,;:]+`)

// multiSpaceRe collapses runs of whitespace left behind by a removal.
var multiSpaceRe = regexp.MustCompile(`[ \t]{2,}`)

// danglingCommaRe drops a separator stranded against the end of a sentence:
// "Thanks for your time, {{name}}." -> "Thanks for your time.". The terminal
// punctuation wins, because it is the one that belongs to the sentence.
var danglingCommaRe = regexp.MustCompile(`[,;:]+([.!?])`)

// StripPlaceholders removes unsubstituted template tokens from text destined
// for a speech engine, and tidies the punctuation around the hole.
//
// WHY THIS EXISTS. The greeting and the voicemail message are rendered to audio
// before the call is placed, so nothing between the platform and the caller's
// ear ever reads them. A dispatch that omits contact_first_name therefore does
// not produce a slightly awkward greeting — it produces a speech engine reading
// "curly curly contact underscore first underscore name" to a real person, and
// the same text goes into the model's prompt as "what you have already said".
//
// Removed rather than replaced with a friendly noun. "Hi there" would be an
// invention: the platform asked for a name, and if it did not supply one, the
// honest rendering of "Hi {{name}}, this is Alex" is "Hi, this is Alex" — which
// is a sentence a person would actually say.
func StripPlaceholders(text string) string {
	if !strings.Contains(text, "{{") {
		return text
	}
	out := placeholderRe.ReplaceAllString(text, "")
	out = repeatedPunctRe.ReplaceAllString(out, "$1")
	out = spaceBeforePunctRe.ReplaceAllString(out, "$1")
	out = danglingCommaRe.ReplaceAllString(out, "$1")
	out = multiSpaceRe.ReplaceAllString(out, " ")
	// A placeholder at the very start can stand the sentence's opening
	// punctuation up front: ", this is Alex."
	out = strings.TrimLeft(out, " \t,;:")
	return strings.TrimSpace(out)
}
