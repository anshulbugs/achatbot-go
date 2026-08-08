package main

import (
	"strings"
	"testing"

	"achatbot/pkg/common"
	"achatbot/pkg/config"
)

// The tool must be registered only when a destination exists. A model that can
// see the tool will offer a transfer, so offering it without a destination
// promises the caller something that cannot happen.
func TestTransferToolOnlyRegisteredWithDestination(t *testing.T) {
	cfg = &config.Config{}
	size := 2

	withDest := common.NewSession("s1", &size)
	registerTransferTool(withDest, &callParams{
		TransferNumber: "+15559998888",
		To:             "+15551112222",
	}, "call-1")
	if withDest.Func("call_transfer") == nil {
		t.Error("tool not registered despite a transfer number")
	}

	noDest := common.NewSession("s2", &size)
	registerTransferTool(noDest, &callParams{To: "+15551112222"}, "call-2")
	if noDest.Func("call_transfer") != nil {
		t.Error("tool registered with no transfer number — the model would promise a transfer it cannot make")
	}
}

// Registering an implementation without advertising it is a silent no-op: the
// model never learns the tool exists.
func TestTransferToolIsAdvertised(t *testing.T) {
	cfg = &config.Config{}
	size := 2
	s := common.NewSession("s", &size)
	registerTransferTool(s, &callParams{TransferNumber: "+15559998888", To: "+1555"}, "c")

	schemas := s.ToolCalls()
	if len(schemas) != 1 {
		t.Fatalf("ToolCalls() returned %d schemas, want 1", len(schemas))
	}
	fn, ok := schemas[0]["function"].(map[string]any)
	if !ok {
		t.Fatalf("schema has no function block: %+v", schemas[0])
	}
	if fn["name"] != "call_transfer" {
		t.Errorf("tool name = %v, want call_transfer", fn["name"])
	}
	desc, _ := fn["description"].(string)
	// The description is the only thing stopping the model transferring for
	// trivial reasons, so it must actually constrain it.
	if !strings.Contains(strings.ToLower(desc), "only") {
		t.Errorf("description does not constrain when to transfer: %q", desc)
	}
}

// Default shows the contact's number so the receiving human knows whose call
// it is; "tenant" is the escape hatch when carriers filter an unowned caller ID.
func TestFromNumberFor(t *testing.T) {
	const contact, tenant = "+15551112222", "+15557654321"

	cfg = &config.Config{}
	cfg.Server.TransferCallerID = ""
	if got := fromNumberFor(contact, tenant); got != contact {
		t.Errorf("default = %q, want the contact's number %q", got, contact)
	}
	cfg.Server.TransferCallerID = "contact"
	if got := fromNumberFor(contact, tenant); got != contact {
		t.Errorf("contact = %q, want %q", got, contact)
	}
	cfg.Server.TransferCallerID = "tenant"
	if got := fromNumberFor(contact, tenant); got != tenant {
		t.Errorf("tenant = %q, want %q", got, tenant)
	}
}

// A model that calls the tool twice must not fire a second transfer at a leg
// the carrier has already handed off.
func TestTransferIsIdempotent(t *testing.T) {
	tool := &transferTool{callID: "c", to: "+15559998888", done: true}
	out, err := tool.Execute(map[string]any{"reason": "wants a human"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "already") {
		t.Errorf("second call returned %q, want it to report the transfer already happened", out)
	}
}

// With no destination the tool must answer in terms the model can act on
// rather than erroring, so the caller is told something.
func TestTransferWithoutDestinationTellsTheModel(t *testing.T) {
	tool := &transferTool{callID: "c"}
	out, err := tool.Execute(map[string]any{"reason": "x"})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if !strings.Contains(strings.ToLower(out), "not available") {
		t.Errorf("out = %q, want it to say transfer is unavailable", out)
	}
}

func TestDescribeCallerID(t *testing.T) {
	if got := describeCallerID("+1555", "+1555"); !strings.Contains(got, "contact") {
		t.Errorf("got %q, want it to name the contact", got)
	}
	if got := describeCallerID("+1999", "+1555"); !strings.Contains(got, "tenant") {
		t.Errorf("got %q, want it to name the tenant", got)
	}
}
