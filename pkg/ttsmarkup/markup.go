// Package ttsmarkup handles the inline markup our speech engine understands,
// and — more importantly — keeps it away from everything that is not the
// speech engine.
//
// Kokoro's G2P front end (misaki, for lang_code "a") accepts two markdown-link
// forms inside otherwise plain text:
//
//	[JobTalk](/dʒˈɑbtɔk/)   force an exact pronunciation
//	[only](+1)              raise the stress on a word, [the](-1) lowers it
//
// Both are instructions to the synthesiser, not words. They must reach the TTS
// request intact and must reach nothing else — a transcript, a live feed or an
// end-of-call report that shows "[only](+1)" is showing the tenant our
// plumbing. Strip is the one function that removes them, leaving the word the
// caller actually heard.
//
// This is the same class of defect that has bitten this pipeline twice: markup
// the engine does not understand gets SPOKEN ALOUD (raw gemma-4 tool markup
// once reached a caller; IPA stress marks are read out as "stress-free"). So
// Strip is also the safety interlock on the TTS side — when markup support is
// off, we strip before synthesis rather than gamble that the engine ignores
// what it cannot parse.
package ttsmarkup

import "regexp"

// markupRe matches the two supported forms and nothing else.
//
// Deliberately narrow. The target inside the parentheses must be either a
// slash-delimited phoneme string or a signed one-digit stress level; an
// ordinary markdown link like [docs](https://…) is left alone so that a
// tenant prompt leaking a URL is not silently rewritten into bare text. The
// bracketed part cannot itself contain brackets, so nesting cannot make the
// match run away across a whole reply.
var markupRe = regexp.MustCompile(`\[([^\[\]]*)\]\((?:/[^()/]*/|[+-][0-9])\)`)

// Strip removes the markup and keeps the words.
//
// "[Hello](+1) from [JobTalk](/dʒˈɑbtɔk/)." becomes "Hello from JobTalk."
func Strip(s string) string {
	if s == "" {
		return s
	}
	return markupRe.ReplaceAllString(s, "$1")
}

// Has reports whether s contains any markup, so a caller can log or count it
// without paying for the rewrite.
func Has(s string) bool {
	return s != "" && markupRe.MatchString(s)
}
