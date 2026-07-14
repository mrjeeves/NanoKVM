package mesh

import (
	"encoding/json"
	"testing"
)

// A technician's connect Request (internally-tagged Rust ControlMessage +
// ConnectControl) must decode into our flat cecConnect view.
func TestCecConnectRequestDecodes(t *testing.T) {
	raw := []byte(`{"t":"connect","kind":"request","session_id":"s-123","agent_name":"Alice","want_control":true}`)
	var m cecConnect
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if m.T != "connect" || m.Kind != "request" || m.SessionID != "s-123" {
		t.Fatalf("got %+v", m)
	}
}

// The Approve we send back must match the wire form the technician expects.
func TestCecApprovePayloadShape(t *testing.T) {
	b, err := json.Marshal(cecApprovePayload("s-123"))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["t"] != "connect" || got["kind"] != "approve" || got["session_id"] != "s-123" {
		t.Fatalf("top-level fields wrong: %v", got)
	}
	scope, ok := got["scope"].(map[string]any)
	if !ok || scope["kind"] != "forever" {
		t.Fatalf("scope wrong: %v", got["scope"])
	}
}

// approveTech authorizes a technician (by canonical pubkey), so senderMayControl
// lets an answered technician drive — tolerant of the display-suffix form.
func TestApprovedTechMayControl(t *testing.T) {
	b := &Bridge{}
	// A new technician is refused while we're not asking for help, and stays
	// unauthorized. (We don't call senderMayControl on an unapproved sender:
	// that path reads b.state, which a bare Bridge doesn't have — the approved
	// path we care about returns before touching it.)
	if admit, _ := b.cecAdmit("tech-pub-AB12C"); admit {
		t.Fatal("must not admit a technician when not asking for help")
	}
	if b.cecApprovedTech("tech-pub") {
		t.Fatal("refused technician must not be authorized")
	}
	// While asking, the technician is admitted and we're told to drop the hand.
	b.help.asking = true
	if admit, lower := b.cecAdmit("tech-pub-AB12C"); !admit || !lower { // display-suffixed form
		t.Fatalf("asking: admit=%v lower=%v, want true/true", admit, lower)
	}
	// A retransmit (already approved) re-acks without asking to lower again.
	if admit, lower := b.cecAdmit("tech-pub"); !admit || lower {
		t.Fatalf("retransmit: admit=%v lower=%v, want true/false", admit, lower)
	}
	if !b.cecApprovedTech("tech-pub") {
		t.Fatal("canonical lookup should match the suffixed grant")
	}
	if !b.senderMayControl("tech-pub") {
		t.Fatal("approved technician should pass senderMayControl")
	}
	if !b.senderMayControl("tech-pub-AB12C") {
		t.Fatal("approved technician (suffixed) should pass senderMayControl")
	}
	b.unapproveTech("tech-pub")
	if b.cecApprovedTech("tech-pub-AB12C") {
		t.Fatal("session end should drop the grant")
	}
}
