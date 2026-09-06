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
	// ThreeHours, not Forever: the device really does end the grant on that
	// deadline (cecGrantWindow), so the scope on the wire has to say so.
	scope, ok := got["scope"].(map[string]any)
	if !ok || scope["kind"] != cecScopeThreeHours {
		t.Fatalf("scope wrong: %v", got["scope"])
	}
}
