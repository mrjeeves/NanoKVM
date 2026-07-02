package mesh

import (
	"encoding/json"
	"testing"
	"time"

	"NanoKVM-Server/config"
)

func TestAppControlDecodes(t *testing.T) {
	// The exact frame AllMyStuff's "Restart device" menu item sends
	// (app.rs AppControl::RestartDevice, tag "kind", snake_case).
	msg, err := DecodeControlMessage(json.RawMessage(`{"t":"app","kind":"restart_device"}`))
	if err != nil {
		t.Fatalf("decode restart_device: %v", err)
	}
	if msg.Kind != ControlKindApp || msg.App == nil {
		t.Fatalf("restart_device should decode as app control, got %+v", msg)
	}
	if msg.App.Kind != AppControlKindRestartDevice {
		t.Fatalf("kind = %q, want restart_device", msg.App.Kind)
	}

	// A newer app command decodes as Unknown, never errors (serde(other)).
	msg, err = DecodeControlMessage(json.RawMessage(`{"t":"app","kind":"defragment_flux_capacitor"}`))
	if err != nil {
		t.Fatalf("decode future app kind: %v", err)
	}
	if msg.Kind != ControlKindApp || msg.App == nil || msg.App.Kind != AppControlKindUnknown {
		t.Fatalf("future app kind should decode as unknown, got %+v", msg)
	}
}

// testBridge builds a Bridge with persisted state in a temp dir — enough for
// control handling, no daemon connections.
func testBridge(t *testing.T) *Bridge {
	t.Helper()
	return &Bridge{
		conf:  &config.Config{},
		mesh:  config.Mesh{NetworkId: "n", Name: "CEC-KVM"},
		state: LoadState(t.TempDir()),
	}
}

func TestHandleAppGatesOnOwner(t *testing.T) {
	fired := make(chan string, 2)
	restartDeviceFn = func() error { fired <- "device"; return nil }
	restartServerFn = func() error { fired <- "server"; return nil }
	t.Cleanup(func() {
		restartDeviceFn = restartDevice
		restartServerFn = restartServer
	})

	b := testBridge(t)
	restart := &AppControl{Kind: AppControlKindRestartDevice}

	// Unclaimed device: nobody may reboot it.
	b.handleApp("n", "somebody", restart)
	select {
	case got := <-fired:
		t.Fatalf("unclaimed device rebooted (%s) — restart_device must be owner-gated", got)
	case <-time.After(50 * time.Millisecond):
	}

	// Claimed: only the owner may.
	if !b.state.TryClaim("owner-node-ABCDE") {
		t.Fatal("claim should succeed on fresh state")
	}
	b.handleApp("n", "intruder", restart)
	select {
	case got := <-fired:
		t.Fatalf("non-owner rebooted the device (%s)", got)
	case <-time.After(50 * time.Millisecond):
	}

	// The owner's request goes through — including with the display-suffix vs
	// bare-pubkey mismatch canonicalEqual tolerates.
	b.handleApp("n", "owner-node", restart)
	select {
	case got := <-fired:
		if got != "device" {
			t.Fatalf("wrong action fired: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner's restart_device never fired")
	}

	// And "restart" relaunches the server, not the OS.
	b.handleApp("n", "owner-node", &AppControl{Kind: AppControlKindRestart})
	select {
	case got := <-fired:
		if got != "server" {
			t.Fatalf("wrong action fired: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("owner's restart never fired")
	}

	// upgrade / unknown are ignored even from the owner.
	b.handleApp("n", "owner-node", &AppControl{Kind: AppControlKindUpgrade})
	b.handleApp("n", "owner-node", &AppControl{Kind: AppControlKindUnknown})
	select {
	case got := <-fired:
		t.Fatalf("upgrade/unknown should not act, fired %s", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestPresencePayloadStampsSentAtPerSend(t *testing.T) {
	b := testBridge(t)
	before := uint64(time.Now().UnixMilli())
	payload, err := b.presencePayload()
	if err != nil {
		t.Fatalf("presencePayload: %v", err)
	}
	var p NodeProfile
	if err := json.Unmarshal(payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	// The test host's clock is sane, so the stamp must be present and current.
	if p.SentAt < before || p.SentAt > uint64(time.Now().UnixMilli()) {
		t.Fatalf("sent_at = %d, want a fresh Unix-ms stamp", p.SentAt)
	}
	// And it must be a per-send stamp: the built profile itself stays clean so
	// no cached copy can carry a stale clock.
	if got := b.currentProfile().SentAt; got != 0 {
		t.Fatalf("currentProfile().SentAt = %d, want 0 (stamp only at send)", got)
	}
}

func TestNewRouteRejectShape(t *testing.T) {
	msg := NewRouteReject("r-1", "not this KVM's owner — claim it first")
	var m map[string]interface{}
	if err := json.Unmarshal(msg.Payload(), &m); err != nil {
		t.Fatalf("unmarshal reject: %v", err)
	}
	if m["t"] != "route" || m["kind"] != "reject" || m["route_id"] != "r-1" || m["reason"] == "" {
		t.Fatalf("reject wire shape wrong: %v", m)
	}
	// It must round-trip through our own decoder too.
	decoded, err := DecodeControlMessage(msg.Payload())
	if err != nil || decoded.Route == nil || decoded.Route.Kind != RouteControlKindReject {
		t.Fatalf("reject should decode as route reject: %+v (%v)", decoded, err)
	}
}
