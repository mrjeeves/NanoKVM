package mesh

import (
	"strings"
	"testing"

	"NanoKVM-Server/config"
)

// TestClaimCodeEncodingMatchesRust: the base32 encoding and the network-id
// derivation are FROZEN mirrors of AllMyStuff's allmystuff-protocol — the
// same vectors its Rust tests pin ("hello" → nbswy3dp per RFC 4648).
func TestClaimCodeEncodingMatchesRust(t *testing.T) {
	if got := claimCodeFromBytes([]byte("hello")); got != "nbswy3dp" {
		t.Fatalf("claimCodeFromBytes(hello) = %q, want nbswy3dp", got)
	}
	code := claimCodeFromBytes([]byte{0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB,
		0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB, 0xAB})
	if len(code) != 26 {
		t.Fatalf("16 bytes must encode to 26 chars, got %d (%q)", len(code), code)
	}
	for _, c := range code {
		if !((c >= 'a' && c <= 'z') || (c >= '2' && c <= '7')) {
			t.Fatalf("non-base32 char %q in %q", c, code)
		}
	}
}

// TestClaimCodeNetworkIDNormalizesDisplayForms: the dashed, upper-case form a
// human transcribes derives the same network as the raw code — and the same
// id AllMyStuff derives.
func TestClaimCodeNetworkIDNormalizesDisplayForms(t *testing.T) {
	code := "prfuhl5zyyfiyjbje753bw5wp4"
	want := "amsclaim-" + code
	if got := claimCodeNetworkID(code); got != want {
		t.Fatalf("raw form: %q, want %q", got, want)
	}
	pretty := formatClaimCode(code)
	if !strings.Contains(pretty, "-") {
		t.Fatalf("display form should be dashed, got %q", pretty)
	}
	if got := claimCodeNetworkID(strings.ToUpper(pretty)); got != want {
		t.Fatalf("display form: %q, want %q", got, want)
	}
}

// TestNewClaimCodeIsRandomAndWellFormed: two mints never collide and always
// produce full-length codes.
func TestNewClaimCodeIsRandomAndWellFormed(t *testing.T) {
	a, b := newClaimCode(), newClaimCode()
	if a == b {
		t.Fatal("two freshly minted claim codes collided")
	}
	if len(a) != 26 || len(b) != 26 {
		t.Fatalf("mint lengths = %d/%d, want 26", len(a), len(b))
	}
}

// TestClaimNetworkAllowed: the defense-in-depth gate honors claims only via
// the claim rendezvous meshes unless public claims are enabled in the
// device's config.
func TestClaimNetworkAllowed(t *testing.T) {
	b := &Bridge{
		mesh:  config.Mesh{},
		state: LoadState(t.TempDir()),
	}
	b.joiningMesh = DeriveJoiningMeshID("test-device")

	if !b.claimNetworkAllowed(localClaimMesh) {
		t.Fatal("LAN claim mesh must always be allowed")
	}
	if !b.claimNetworkAllowed(b.joiningMeshID()) {
		t.Fatal("the joining mesh must always be allowed")
	}
	if b.claimNetworkAllowed("some-shared-mesh") {
		t.Fatal("an ordinary mesh must be refused while public claims are off")
	}

	// The device's own claim-code rendezvous is allowed once a code exists.
	code := b.state.EnsureClaimCode()
	if !b.claimNetworkAllowed(claimCodeNetworkID(code)) {
		t.Fatal("the device's own claim-code mesh must be allowed")
	}
	if b.claimNetworkAllowed(claimCodeNetworkID("someothercode")) {
		t.Fatal("someone else's claim-code mesh must be refused")
	}

	// Public claims on: everything is allowed (legacy-claimer compat).
	b.mesh.PublicClaims = true
	if !b.claimNetworkAllowed("some-shared-mesh") {
		t.Fatal("public claims on must allow ordinary meshes")
	}
}

// TestStateClaimCodeLifecycle: minted lazily, stable until rotated, dropped
// by an unclaim reset.
func TestStateClaimCodeLifecycle(t *testing.T) {
	s := LoadState(t.TempDir())
	if s.ClaimCode() != "" {
		t.Fatal("fresh state should hold no claim code")
	}
	first := s.EnsureClaimCode()
	if s.EnsureClaimCode() != first {
		t.Fatal("code must be stable until rotated")
	}
	s.RotateClaimCode()
	if s.ClaimCode() != "" {
		t.Fatal("rotate must clear the code")
	}
	if s.EnsureClaimCode() == first {
		t.Fatal("rotation must mint a fresh secret")
	}
}

// TestEnsureMembershipsPublicClaims: with publicClaims enabled, an unclaimed
// device joins the joining mesh WITH the relay venue (no LAN-only pin), plus
// the claim-code rendezvous.
func TestEnsureMembershipsPublicClaims(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	b.mesh.PublicClaims = true
	f.respondWith("networks_list", networksListLine())

	if err := b.ensureMemberships(); err != nil {
		t.Fatalf("ensureMemberships: %v", err)
	}
	added := map[string]map[string]interface{}{}
	for _, req := range f.requests("network_add") {
		if cfg, _ := req["config"].(map[string]interface{}); cfg != nil {
			if id, _ := cfg["network_id"].(string); id != "" {
				added[id] = cfg
			}
		}
	}
	joinCfg := added[b.joiningMeshID()]
	if joinCfg == nil {
		t.Fatalf("joining mesh not joined: %v", added)
	}
	if sig, _ := joinCfg["signaling"].(map[string]interface{}); sig != nil && sig["strategy"] == "none" {
		t.Fatal("public claims on: the joining mesh must keep its relay venue, not be pinned LAN-only")
	}
	code := b.state.ClaimCode()
	if code == "" {
		t.Fatal("public claims on: a claim code must be minted")
	}
	if added[claimCodeNetworkID(code)] == nil {
		t.Fatalf("claim-code rendezvous not joined: %v", added)
	}
	if added[localClaimMesh] == nil {
		t.Fatalf("LAN claim mesh not joined: %v", added)
	}
}

// TestEnsureMembershipsFleetRetiresClaimMeshes: once the fleet mesh carries
// the device, every claim rendezvous is left and the spent code rotates.
func TestEnsureMembershipsFleetRetiresClaimMeshes(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	b.mesh.PublicClaims = true
	code := b.state.EnsureClaimCode()
	codeNet := claimCodeNetworkID(code)
	if !b.state.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed")
	}
	b.state.AdoptFleetKey("fleet-secret-key", "Casey", nil)
	f.respondWith("networks_list", networksListLine(b.joiningMeshID(), localClaimMesh, codeNet))

	if err := b.ensureMemberships(); err != nil {
		t.Fatalf("ensureMemberships: %v", err)
	}
	removed := map[string]bool{}
	for _, req := range f.requests("network_remove") {
		id, _ := req["network"].(string)
		removed[id] = true
	}
	for _, id := range []string{b.joiningMeshID(), localClaimMesh, codeNet} {
		if !removed[id] {
			t.Fatalf("claim mesh %s not retired (removed: %v)", id, removed)
		}
	}
	if b.state.ClaimCode() == code {
		t.Fatal("the spent claim code must rotate once the fleet carries the device")
	}
}

// TestClaimGateDeclinesOverOrdinaryMesh: a Claim arriving over a non-claim
// mesh is refused with a Declined reply and the device stays unclaimed.
func TestClaimGateDeclinesOverOrdinaryMesh(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)

	b.handleOwnership("some-shared-mesh", "stranger-node", &OwnershipControl{
		Kind: OwnershipKindClaim, Owner: "stranger-node",
	})

	if b.state.Owner() != "" || !b.state.Claimable() {
		t.Fatal("gated claim must not take")
	}
	waitFor(t, "declined reply", func() bool {
		for _, req := range f.requests("channel_send_to") {
			payload, _ := req["payload"].(map[string]interface{})
			if payload == nil {
				continue
			}
			if payload["t"] == string(ControlKindOwnership) &&
				payload["kind"] == string(OwnershipKindDeclined) {
				return true
			}
		}
		return false
	})
}
