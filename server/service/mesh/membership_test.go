package mesh

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"NanoKVM-Server/config"
)

// connectedBridge wires a Bridge to a fake daemon (events + ctl) with a known
// joining mesh — the scaffolding every membership test shares. Unclaimed;
// tests claim it as needed.
func connectedBridge(t *testing.T, f *fakeDaemon) *Bridge {
	t.Helper()
	events, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	ctl, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial ctl: %v", err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	if err := events.Subscribe(nil, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	meshConf := config.Mesh{Name: "CEC-KVM", Label: "CEC KVM Joining Mesh"}
	b := &Bridge{
		conf:   &config.Config{Mesh: meshConf},
		mesh:   meshConf,
		state:  LoadState(t.TempDir()),
		events: events,
		ctl:    ctl,
	}
	b.joiningMesh = DeriveJoiningMeshID("test-device")
	b.running = true
	return b
}

// waitFor polls cond until it holds or the deadline passes — the async
// membership handlers run off the event goroutine, so tests observe, not call.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func networksListLine(ids ...string) string {
	type net struct {
		ConfigID  string `json:"config_id"`
		NetworkID string `json:"network_id"`
	}
	nets := make([]net, 0, len(ids))
	for _, id := range ids {
		nets = append(nets, net{ConfigID: id, NetworkID: id})
	}
	raw, _ := json.Marshal(map[string]interface{}{"networks": nets})
	return fmt.Sprintf(`{"ok":true,"data":%s}`, raw)
}

func lastRequest(f *fakeDaemon, op string) map[string]interface{} {
	reqs := f.requests(op)
	if len(reqs) == 0 {
		return nil
	}
	return reqs[len(reqs)-1]
}

// TestFleetKeyIsOwnerGated: the joining mesh is auto-approve open, so a
// stranger who reads the id off the screen can join and send control frames.
// An ungated FleetKey would let them capture an unclaimed device onto their
// own fleet — the handler must refuse a key from anyone but the owner.
func TestFleetKeyIsOwnerGated(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)

	// Unclaimed device (no owner): a stranger's fleet key is refused, so no
	// fleet credential is adopted and no fleet network is joined.
	b.handleOwnership("n", "stranger", &OwnershipControl{
		Kind: OwnershipKindFleetKey, Key: "attacker-key", Name: "Mallory",
	})
	time.Sleep(100 * time.Millisecond)
	if b.state.FleetKey() != "" {
		t.Fatalf("stranger's fleet key adopted: %q", b.state.FleetKey())
	}

	// The owner's key, after a legitimate claim, is accepted.
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed")
	}
	b.handleOwnership("n", "owner-node", &OwnershipControl{
		Kind: OwnershipKindFleetKey, Key: "real-key", Name: "Casey",
	})
	if b.state.FleetKey() != "real-key" {
		t.Fatalf("owner's fleet key not adopted: %q", b.state.FleetKey())
	}
}

// TestMeshAddIsOwnerGated: a stranger's mesh_add never reaches the daemon.
func TestMeshAddIsOwnerGated(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)

	b.handleKvm("n", "stranger", &KvmControl{Kind: KvmControlKindMeshAdd, NetworkID: "extra-mesh"})
	time.Sleep(100 * time.Millisecond)
	if got := f.requests("network_add"); len(got) != 0 {
		t.Fatalf("stranger walked the KVM onto a mesh: %v", got)
	}
}

// TestMeshAddJoinsAndSubscribes: the owner's mesh_add normalizes the id,
// joins an auto-approve open network, subscribes the planes, and lands the
// mesh in the advertised membership list.
func TestMeshAddJoinsAndSubscribes(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed on fresh state")
	}

	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindMeshAdd, NetworkID: "  Extra-Mesh "})

	waitFor(t, "network_add", func() bool { return len(f.requests("network_add")) == 1 })
	cfg, _ := lastRequest(f, "network_add")["config"].(map[string]interface{})
	if cfg == nil {
		t.Fatal("network_add carried no config")
	}
	if cfg["network_id"] != "extra-mesh" {
		t.Errorf("network_id = %v, want the normalized extra-mesh", cfg["network_id"])
	}
	if cfg["auto_approve"] != true {
		t.Errorf("auto_approve = %v, want true (that flag is what admits peers)", cfg["auto_approve"])
	}
	if cfg["kind"] != "open" {
		t.Errorf("kind = %v, want open", cfg["kind"])
	}
	waitFor(t, "planes on extra-mesh", func() bool {
		count := 0
		for _, req := range f.requests("channel_subscribe") {
			if req["network"] == "extra-mesh" {
				count++
			}
		}
		return count == 3
	})
	waitFor(t, "advertised membership", func() bool {
		for _, n := range b.networksSnapshot() {
			if n == "extra-mesh" {
				return true
			}
		}
		return false
	})
}

// TestMeshCommandsRefuseFleetMesh: neither mesh_add nor mesh_remove may touch
// the fleet mesh — that membership is governed by the fleet key.
func TestMeshCommandsRefuseFleetMesh(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed")
	}
	b.state.AdoptFleetKey("fleet-secret-key", "Casey", nil)
	fleetNet := DeriveFleetNetworkID("fleet-secret-key")

	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindMeshAdd, NetworkID: fleetNet})
	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindMeshRemove, NetworkID: fleetNet})
	time.Sleep(100 * time.Millisecond)
	if got := f.requests("network_add"); len(got) != 0 {
		t.Fatalf("mesh_add touched the fleet mesh: %v", got)
	}
	if got := f.requests("network_remove"); len(got) != 0 {
		t.Fatalf("mesh_remove touched the fleet mesh: %v", got)
	}
}

// TestMeshRemoveLeavesAndPurges: the owner's mesh_remove leaves the network
// with purge (a genuine forget) and prunes the advertised list.
func TestMeshRemoveLeavesAndPurges(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed")
	}
	b.mu.Lock()
	b.networks = []string{"extra-mesh"}
	b.mu.Unlock()
	f.respondWith("networks_list", networksListLine("extra-mesh"))

	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindMeshRemove, NetworkID: "extra-mesh"})

	waitFor(t, "network_remove", func() bool { return len(f.requests("network_remove")) == 1 })
	req := lastRequest(f, "network_remove")
	if req["network"] != "extra-mesh" || req["purge"] != true {
		t.Fatalf("network_remove = %v, want extra-mesh with purge", req)
	}
	waitFor(t, "membership pruned", func() bool { return len(b.networksSnapshot()) == 0 })
}

// TestReleaseUnclaimsAndReturnsToJoiningMesh: an owner's Release resets the
// device — owner/fleet/attachment forgotten, fleet mesh left (purged), the
// joining mesh rejoined, claim mode back on. A stranger's Release does nothing.
func TestReleaseUnclaimsAndReturnsToJoiningMesh(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "Casey's Mac") {
		t.Fatal("claim should succeed")
	}
	b.state.AdoptFleetKey("fleet-secret-key", "Casey", nil)
	fleetNet := DeriveFleetNetworkID("fleet-secret-key")
	b.mu.Lock()
	b.networks = []string{fleetNet}
	b.fleetRoster = map[string]struct{}{"owner-node": {}}
	b.mu.Unlock()
	f.respondWith("networks_list", networksListLine(fleetNet))

	// A stranger can't factory-reset the mesh identity.
	b.handleOwnership("n", "stranger", &OwnershipControl{Kind: OwnershipKindRelease})
	time.Sleep(100 * time.Millisecond)
	if b.state.Claimable() || b.state.Owner() == "" {
		t.Fatal("stranger's release took effect")
	}

	b.handleOwnership("n", "owner-node", &OwnershipControl{Kind: OwnershipKindRelease})

	waitFor(t, "state reset", func() bool {
		return b.state.Claimable() && b.state.Owner() == "" && b.state.FleetKey() == "" &&
			b.state.AttachedTo() == ""
	})
	waitFor(t, "joining mesh rejoined", func() bool {
		req := lastRequest(f, "network_add")
		if req == nil {
			return false
		}
		cfg, _ := req["config"].(map[string]interface{})
		return cfg != nil && cfg["network_id"] == b.joiningMeshID()
	})
	waitFor(t, "fleet mesh left", func() bool {
		req := lastRequest(f, "network_remove")
		return req != nil && req["network"] == fleetNet && req["purge"] == true
	})
	waitFor(t, "membership = joining mesh only", func() bool {
		nets := b.networksSnapshot()
		return len(nets) == 1 && nets[0] == b.joiningMeshID()
	})
	// The identity label reverts to the brand name with the attachment gone.
	waitFor(t, "identity label reset", func() bool {
		req := lastRequest(f, "identity_set_label")
		return req != nil && req["label"] == "CEC-KVM"
	})
}

// TestAttachRenamesIdentity: an attach names the device KVM-<label> on both
// the presence advert and the daemon identity; detach reverts both; a
// label-less attach falls back to the label from the target's own presence.
func TestAttachRenamesIdentity(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed")
	}

	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindAttach, Node: "den-tower", Label: "Den Tower"})
	if b.state.AttachedTo() != "den-tower" || b.state.AttachedLabel() != "Den Tower" {
		t.Fatalf("attach state = %q/%q", b.state.AttachedTo(), b.state.AttachedLabel())
	}
	if req := lastRequest(f, "identity_set_label"); req == nil || req["label"] != "KVM-Den Tower" {
		t.Fatalf("identity_set_label = %v, want KVM-Den Tower", req)
	}
	if got := b.currentProfile().Label; got != "KVM-Den Tower" {
		t.Fatalf("advertised label = %q, want KVM-Den Tower", got)
	}

	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindDetach})
	if req := lastRequest(f, "identity_set_label"); req == nil || req["label"] != "CEC-KVM" {
		t.Fatalf("identity_set_label after detach = %v, want CEC-KVM", req)
	}
	if got := b.currentProfile().Label; got != "CEC-KVM" {
		t.Fatalf("advertised label after detach = %q, want CEC-KVM", got)
	}

	// Label-less attach (an older sender): the presence-cache fallback names it.
	b.notePeerLabel("lab-rig-AB3CD", json.RawMessage(`{"protocol":1,"node":"lab-rig-AB3CD","label":"Lab Rig"}`))
	b.handleKvm("n", "owner-node", &KvmControl{Kind: KvmControlKindAttach, Node: "lab-rig-AB3CD"})
	if b.state.AttachedLabel() != "Lab Rig" {
		t.Fatalf("presence-cache fallback label = %q, want Lab Rig", b.state.AttachedLabel())
	}
}

// TestClaimAutoAttachUsesClaimerLabel: the claim auto-attach picks up the
// claimer's label from its presence advert, so a fresh KVM is named
// KVM-<claimer> from the first second it's owned.
func TestClaimAutoAttachUsesClaimerLabel(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	b.notePeerLabel("owner-node-XY3ZW", json.RawMessage(`{"protocol":1,"node":"owner-node-XY3ZW","label":"Casey's Mac"}`))

	b.handleOwnership("n", "owner-node-XY3ZW", &OwnershipControl{Kind: OwnershipKindClaim, Owner: "owner-node-XY3ZW"})

	if b.state.Owner() != "owner-node-XY3ZW" {
		t.Fatalf("owner = %q", b.state.Owner())
	}
	if b.state.AttachedLabel() != "Casey's Mac" {
		t.Fatalf("auto-attach label = %q, want Casey's Mac", b.state.AttachedLabel())
	}
	if got := b.currentProfile().Label; got != "KVM-Casey's Mac" {
		t.Fatalf("advertised label = %q, want KVM-Casey's Mac", got)
	}
}

// TestAttachLabelSelfHealsFromLivePresence: when a claim/attach lands before
// the target's label is known (its presence hadn't arrived yet), the KVM
// first advertises KVM-<id> — but as soon as the target's presence is cached,
// the advert RESOLVES LIVE to KVM-<label> without a re-attach. This is the
// fix for a KVM stuck showing the attached node's id instead of its label.
func TestAttachLabelSelfHealsFromLivePresence(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)

	// Claim with NO cached label (the racy order): the auto-attach bakes an
	// empty label, so the advert falls back to the id.
	b.handleOwnership("n", "den-tower-AB3CD", &OwnershipControl{Kind: OwnershipKindClaim, Owner: "den-tower-AB3CD"})
	if b.state.AttachedLabel() != "" {
		t.Fatalf("expected an empty baked label, got %q", b.state.AttachedLabel())
	}
	got := b.currentProfile().Label
	if got == "" || got[:4] != "KVM-" || got == "KVM-" {
		t.Fatalf("pre-heal label = %q, want a KVM-<id> fallback", got)
	}
	if got == "KVM-Den Tower" {
		t.Fatal("label resolved before the target's presence arrived")
	}

	// The target's presence now lands — the advert self-heals to the label,
	// no re-attach needed.
	b.notePeerLabel("den-tower-AB3CD", json.RawMessage(`{"protocol":1,"node":"den-tower-AB3CD","label":"Den Tower"}`))
	if got := b.currentProfile().Label; got != "KVM-Den Tower" {
		t.Fatalf("post-heal label = %q, want KVM-Den Tower", got)
	}
}

// TestEnsureMembershipsMigratesLegacy: the retired shared mesh is left
// (purged) and the unclaimed device lands on its own joining mesh instead.
func TestEnsureMembershipsMigratesLegacy(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	f.respondWith("networks_list", networksListLine(legacySharedMesh))

	if err := b.ensureMemberships(); err != nil {
		t.Fatalf("ensureMemberships: %v", err)
	}
	if req := lastRequest(f, "network_remove"); req == nil || req["network"] != legacySharedMesh || req["purge"] != true {
		t.Fatalf("legacy mesh not retired: %v", req)
	}
	req := lastRequest(f, "network_add")
	cfg, _ := req["config"].(map[string]interface{})
	if cfg == nil || cfg["network_id"] != b.joiningMeshID() {
		t.Fatalf("joining mesh not joined: %v", req)
	}
	nets := b.networksSnapshot()
	if len(nets) != 1 || nets[0] != b.joiningMeshID() {
		t.Fatalf("membership = %v, want the joining mesh alone", nets)
	}
}

// TestEnsureMembershipsUnclaimedShedsStaleMeshes: an unclaimed device's only
// legitimate membership is its joining mesh — an old fleet mesh or owner-added
// mesh left by an unclaim that died mid-teardown is shed on connect, making
// the reset convergent no matter where it was interrupted.
func TestEnsureMembershipsUnclaimedShedsStaleMeshes(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	f.respondWith("networks_list", networksListLine("lucky-gauss-e67fs", "den-site-mesh", b.joiningMeshID()))

	if err := b.ensureMemberships(); err != nil {
		t.Fatalf("ensureMemberships: %v", err)
	}
	removed := map[string]bool{}
	for _, req := range f.requests("network_remove") {
		id, _ := req["network"].(string)
		removed[id] = true
		if req["purge"] != true {
			t.Errorf("stale mesh %s removed without purge", id)
		}
	}
	if !removed["lucky-gauss-e67fs"] || !removed["den-site-mesh"] {
		t.Fatalf("stale meshes not shed: %v", removed)
	}
	if removed[b.joiningMeshID()] {
		t.Fatal("the joining mesh itself was shed")
	}
	nets := b.networksSnapshot()
	if len(nets) != 1 || nets[0] != b.joiningMeshID() {
		t.Fatalf("membership = %v, want the joining mesh alone", nets)
	}
}

// TestEnsureMembershipsClaimedLeavesJoiningMesh: a claimed device that (after
// a crash) still holds its joining mesh drops it and joins its fleet mesh —
// closed, not auto-approved.
func TestEnsureMembershipsClaimedLeavesJoiningMesh(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed")
	}
	b.state.AdoptFleetKey("fleet-secret-key", "Casey", nil)
	fleetNet := DeriveFleetNetworkID("fleet-secret-key")
	f.respondWith("networks_list", networksListLine(b.joiningMeshID()))

	if err := b.ensureMemberships(); err != nil {
		t.Fatalf("ensureMemberships: %v", err)
	}
	if req := lastRequest(f, "network_remove"); req == nil || req["network"] != b.joiningMeshID() {
		t.Fatalf("joining mesh not left on a claimed device: %v", req)
	}
	req := lastRequest(f, "network_add")
	cfg, _ := req["config"].(map[string]interface{})
	if cfg == nil || cfg["network_id"] != fleetNet {
		t.Fatalf("fleet mesh not joined: %v", req)
	}
	if cfg["kind"] != "closed" || cfg["auto_approve"] != false {
		t.Fatalf("fleet mesh config = %v, want closed and not auto-approved", cfg)
	}
}
