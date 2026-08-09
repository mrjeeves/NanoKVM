package mesh

import (
	"testing"

	"NanoKVM-Server/config"
)

// The bug this pins: a technician answering a raised hand dials the customer's
// PRIVATE room (allmystuff-cec-protocol network_id_for_device, "cec-" + the
// support number) and sends the connect Request there. The KVM used to join
// only the discovery area and the asking room, so that Request was delivered to
// nobody, cecAdmit never ran, and the technician's session sat at "requested"
// forever — first attempt and every attempt.
func TestRaiseHandJoinsThePrivateSessionRoom(t *testing.T) {
	f := startFakeDaemon(t)

	events, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	defer events.Close()
	ctl, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial ctl: %v", err)
	}
	defer ctl.Close()
	if err := events.Subscribe(nil, nil, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	b := &Bridge{
		conf:   &config.Config{},
		mesh:   config.Mesh{NetworkId: "cec-backend-client-mesh"},
		state:  LoadState(t.TempDir()),
		events: events,
		ctl:    ctl,
		nodeID: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
	}

	want := b.cecSessionNetworkID()
	if want == "" || want == cecNetworkPrefix {
		t.Fatalf("session room id = %q, want cec-<9 digits>", want)
	}
	if len(want) != len(cecNetworkPrefix)+9 {
		t.Fatalf("session room id = %q, want %d chars", want, len(cecNetworkPrefix)+9)
	}

	if err := b.RaiseHand(); err != nil {
		t.Fatalf("RaiseHand: %v", err)
	}

	// The room must be joined...
	joined := map[string]bool{}
	for _, req := range f.requests("network_add") {
		if cfg, ok := req["config"].(map[string]interface{}); ok {
			if id, _ := cfg["network_id"].(string); id != "" {
				joined[id] = true
			}
		}
	}
	if !joined[want] {
		t.Fatalf("private session room %s was never joined; joined %v", want, joined)
	}
	if !joined[CecAskNetworkID] {
		t.Errorf("asking room %s was not joined", CecAskNetworkID)
	}

	// ...and cec.control subscribed ON it, or the Request still reaches nobody.
	subscribed := map[string]bool{}
	for _, req := range f.requests("channel_subscribe") {
		if ch, _ := req["channel"].(string); ch == CecChannelControl {
			id, _ := req["network"].(string)
			subscribed[id] = true
		}
	}
	if !subscribed[want] {
		t.Fatalf("cec.control not subscribed on %s; subscribed on %v", want, subscribed)
	}
}

// The room id must match allmystuff-cec-protocol exactly — a technician derives
// it independently from the number read over the phone, so a one-character
// difference is a room nobody shares.
func TestSessionRoomIDMatchesTheProtocolDerivation(t *testing.T) {
	b := &Bridge{nodeID: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"}
	number := b.SupportID()
	if got, want := b.cecSessionNetworkID(), cecNetworkPrefix+number; got != want {
		t.Fatalf("session room = %q, want %q", got, want)
	}
	// Display-suffixed device ids yield the same number, so the same room.
	suffixed := &Bridge{nodeID: b.nodeID + "-AB12C"}
	if got := suffixed.cecSessionNetworkID(); got != b.cecSessionNetworkID() {
		t.Fatalf("display suffix changed the room: %q vs %q", got, b.cecSessionNetworkID())
	}
}

// Before identity_show there is no number, and "cec-" is a room shared with
// every other un-identified device.
func TestSessionRoomIsEmptyBeforeIdentity(t *testing.T) {
	b := &Bridge{}
	if got := b.cecSessionNetworkID(); got != "" {
		t.Fatalf("session room = %q before identity, want empty", got)
	}
}
