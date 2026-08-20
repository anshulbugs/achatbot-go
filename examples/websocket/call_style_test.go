package main

import (
	"strings"
	"testing"
)

// The stress-markup rule must never reach a model whose speech engine cannot
// parse it. That is the whole failure mode this feature was designed around,
// and it is guarded by one process-wide flag, so it is worth pinning.
func TestSpeechMarkupRulesFollowTheFlag(t *testing.T) {
	defer func(prev bool) { speechMarkupEnabled = prev }(speechMarkupEnabled)

	speechMarkupEnabled = false
	if got := withCallStyle("You are an agent.", ""); strings.Contains(got, "[word](+1)") {
		t.Error("markup rule offered to the model while markup is off")
	}

	speechMarkupEnabled = true
	if got := withCallStyle("You are an agent.", ""); !strings.Contains(got, "[word](+1)") {
		t.Error("markup rule missing while markup is on")
	}
}

// The rules are appended after the tenant's prompt so they carry more weight,
// and that ordering has been broken by accident before.
func TestCallStyleRulesComeLast(t *testing.T) {
	defer func(prev bool) { speechMarkupEnabled = prev }(speechMarkupEnabled)
	speechMarkupEnabled = true

	const tenant = "You are Sarah from JobTalk."
	got := withCallStyle(tenant, "")
	if !strings.HasPrefix(got, tenant) {
		t.Error("tenant prompt is no longer first, which breaks prefix caching")
	}
	if strings.Index(got, "Delivery rules") < strings.Index(got, tenant) {
		t.Error("delivery rules must follow the tenant prompt, not precede it")
	}
}
