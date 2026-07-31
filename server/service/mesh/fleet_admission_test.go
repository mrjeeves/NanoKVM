package mesh

import (
	"fmt"
	"testing"
)

// A fleet mesh is CLOSED with auto_approve false, so the daemon admits an
// authenticating peer only when it is already in the local roster. Nothing in
// the KVM ever put its owner there, and roster gossip can't do it either (that
// is itself an application frame, dropped by the very admission gate it would
// open). The link therefore authenticated, parked at PendingApproval, and every
// frame after it — presence, control, the site tunnel behind Open / Wi-Fi /
// Update / Unclaim — was dropped pre-admission while the owner's app went on
// showing a healthy peer.
//
// These pin the seeding that makes a claimed device reachable by its owner.

// TestFleetJoinPreRostersOwner: adopting a fleet key must put the owner in the
// fleet network's roster, on the way in.
func TestFleetJoinPreRostersOwner(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed on fresh state")
	}

	b.handleOwnership("n", "owner-node", &OwnershipControl{
		Kind: OwnershipKindFleetKey, Key: "real-key", Name: "Casey",
	})

	fleetNet := DeriveFleetNetworkID("real-key")
	waitFor(t, "roster_approve for the owner", func() bool {
		for _, req := range f.requests("roster_approve") {
			if req["network"] == fleetNet && req["device_id"] == "owner-node" {
				return true
			}
		}
		return false
	})
}

// TestFleetRosterRefreshRepairsMissingOwner: a device whose roster has lost its
// owner heals on the next refresh rather than going quietly deaf to the only
// node allowed to drive it.
func TestFleetRosterRefreshRepairsMissingOwner(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node-A1B2C", "") {
		t.Fatal("claim should succeed on fresh state")
	}
	if !b.state.AdoptFleetKey("real-key", "Casey", nil) {
		t.Fatal("fleet key should be adopted")
	}

	// roster_list answers with a co-member but not the owner.
	f.respondWith("roster_list",
		`{"ok":true,"data":{"roster":[{"device_id":"co-member","label":"Laptop"}]}}`)
	b.refreshFleetRoster()

	reqs := f.requests("roster_approve")
	if len(reqs) != 1 {
		t.Fatalf("roster_approve count = %d, want 1 (the missing owner)", len(reqs))
	}
	// The daemon's roster compares peers by the bare pubkey; the display
	// suffix the owner is recorded with must not be sent through.
	if got := reqs[0]["device_id"]; got != "owner-node" {
		t.Errorf("approved device_id = %v, want the pubkey part owner-node", got)
	}
	if got := reqs[0]["network"]; got != DeriveFleetNetworkID("real-key") {
		t.Errorf("approved on network %v, want the fleet mesh", got)
	}

	// Already rostered: no repeat write. This runs every presence tick, so a
	// converged device must not chatter at the daemon forever.
	f.respondWith("roster_list",
		`{"ok":true,"data":{"roster":[{"device_id":"owner-node","label":"Casey"}]}}`)
	b.refreshFleetRoster()
	if got := len(f.requests("roster_approve")); got != 1 {
		t.Errorf("roster_approve count = %d after the owner was present, want 1", got)
	}
}

// TestFleetMeshCarryingUsNeedsAnActivePeer: the joining-mesh handover waits for
// a link that actually passes frames. A peer stuck at pending_approval is the
// exact state this guard exists to keep the device out of — it must not read as
// "the fleet mesh has us".
func TestFleetMeshCarryingUsNeedsAnActivePeer(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.AdoptFleetKey("real-key", "Casey", nil) {
		t.Fatal("fleet key should be adopted")
	}

	for _, tc := range []struct {
		name  string
		peers string
		want  bool
	}{
		{"no peers", `[]`, false},
		{"authenticated but unapproved", `[{"device_id":"o","status":"pending_approval"}]`, false},
		{"reconnecting", `[{"device_id":"o","status":"reconnecting"}]`, false},
		{"active", `[{"device_id":"o","status":"active"}]`, true},
		{"one of several active",
			`[{"device_id":"a","status":"pending_approval"},{"device_id":"b","status":"active"}]`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f.respondWith("peers_list", fmt.Sprintf(`{"ok":true,"data":{"peers":%s}}`, tc.peers))
			if got := b.fleetMeshCarryingUs(); got != tc.want {
				t.Errorf("fleetMeshCarryingUs() = %v, want %v", got, tc.want)
			}
		})
	}
}
