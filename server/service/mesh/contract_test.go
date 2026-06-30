package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// These tests round-trip the GOLDEN wire fixtures generated from the
// authoritative Rust source (AllMyStuff allmystuff-protocol / allmystuff-session,
// via `cargo run -p allmystuff-session --example dump_kvm_fixtures`) through the
// Go structs in protocol.go. If a json tag, a field, or an omitempty contract
// drifts from the Rust source, these fail — instead of the drift silently making
// real peers drop the KVM (a JSON parse error they never surface).
//
// Keep testdata/contract/ in sync with AllMyStuff/contract-fixtures/ whenever the
// protocol changes.

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "contract", name+".json"))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return b
}

// jsonEqual compares two JSON documents structurally (key order / whitespace
// independent). Numbers both go through float64, so int vs float framing matches.
func jsonEqual(a, b []byte) bool {
	var av, bv interface{}
	if json.Unmarshal(a, &av) != nil || json.Unmarshal(b, &bv) != nil {
		return false
	}
	return reflect.DeepEqual(av, bv)
}

// roundTrip unmarshals a fixture into T, re-marshals it, and asserts the result
// is structurally identical to the original — proving the Go struct captures
// every field with the right tag and omitempty behavior.
func roundTrip[T any](t *testing.T, name string) {
	t.Helper()
	orig := readFixture(t, name)
	var v T
	if err := json.Unmarshal(orig, &v); err != nil {
		t.Fatalf("%s: unmarshal into %T: %v", name, v, err)
	}
	got, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("%s: marshal %T: %v", name, v, err)
	}
	if !jsonEqual(orig, got) {
		t.Errorf("%s: round-trip drift\n  fixture: %s\n  go-out:  %s", name, orig, got)
	}
}

func TestContractRoundTripPresence(t *testing.T) {
	roundTrip[NodeProfile](t, "node_profile_kvm")
	roundTrip[NodeProfile](t, "node_profile_kvm_claimable")
	roundTrip[Capability](t, "capability_screen")
	roundTrip[Capability](t, "capability_control")
	roundTrip[SiteAdvert](t, "site_advert")
	roundTrip[InventorySummary](t, "inventory_summary")
}

func TestContractRoundTripSiteFrames(t *testing.T) {
	roundTrip[SiteFrame](t, "site_frame_open")
	roundTrip[SiteFrame](t, "site_frame_data")
	roundTrip[SiteFrame](t, "site_frame_close")
}

// The KVM presence fixture must decode to a node the GUI will render as a KVM:
// the FEATURE_KVM tag present, the web-UI site advertised, and (when claimed) the
// attach binding carried.
func TestContractKvmProfileSemantics(t *testing.T) {
	var p NodeProfile
	if err := json.Unmarshal(readFixture(t, "node_profile_kvm"), &p); err != nil {
		t.Fatal(err)
	}
	if !containsStr(p.Features, FeatureKVM) {
		t.Errorf("kvm profile missing %q feature tag: %v", FeatureKVM, p.Features)
	}
	if p.Kvm == nil || p.Kvm.Web != "tcp:80" {
		t.Errorf("kvm profile web site = %+v, want tcp:80", p.Kvm)
	}
	if p.Kvm.AttachedTo == nil || *p.Kvm.AttachedTo != "den-tower" {
		t.Errorf("kvm profile attached_to = %v, want den-tower", p.Kvm.AttachedTo)
	}
	if !hasSiteAdvert(p.Sites, "tcp:80") {
		t.Errorf("kvm profile missing the web-UI site advert: %+v", p.Sites)
	}

	// A freshly-claimed-but-unattached KVM omits attached_to.
	var c NodeProfile
	if err := json.Unmarshal(readFixture(t, "node_profile_kvm_claimable"), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Claimable {
		t.Error("claimable fixture should be claimable")
	}
	if c.Kvm == nil || c.Kvm.AttachedTo != nil {
		t.Errorf("claimable kvm should have no attachment, got %+v", c.Kvm)
	}
}

// The control-plane fixtures must decode to the right variant + fields via the
// forward-compatible DecodeControlMessage.
func TestContractControlMessages(t *testing.T) {
	attach, err := DecodeControlMessage(readFixture(t, "control_kvm_attach"))
	if err != nil || attach.Kind != ControlKindKvm || attach.Kvm == nil ||
		attach.Kvm.Kind != KvmControlKindAttach || attach.Kvm.Node != "den-tower" {
		t.Fatalf("control_kvm_attach decoded wrong: %+v (err %v)", attach, err)
	}

	detach, err := DecodeControlMessage(readFixture(t, "control_kvm_detach"))
	if err != nil || detach.Kvm == nil || detach.Kvm.Kind != KvmControlKindDetach {
		t.Fatalf("control_kvm_detach decoded wrong: %+v (err %v)", detach, err)
	}

	claim, err := DecodeControlMessage(readFixture(t, "control_ownership_claim"))
	if err != nil || claim.Ownership == nil || claim.Ownership.Kind != OwnershipKindClaim ||
		claim.Ownership.Owner != "den-tower" {
		t.Fatalf("control_ownership_claim decoded wrong: %+v (err %v)", claim, err)
	}

	fleet, err := DecodeControlMessage(readFixture(t, "control_ownership_fleetkey"))
	if err != nil || fleet.Ownership == nil || fleet.Ownership.Kind != OwnershipKindFleetKey ||
		fleet.Ownership.Key == "" || fleet.Ownership.Name != "Casey" {
		t.Fatalf("control_ownership_fleetkey decoded wrong: %+v (err %v)", fleet, err)
	}

	offer, err := DecodeControlMessage(readFixture(t, "control_route_offer_site"))
	if err != nil || offer.Route == nil || offer.Route.Kind != RouteControlKindOffer ||
		offer.Route.Route == nil || !offer.Route.Route.IsSiteRoute() {
		t.Fatalf("control_route_offer_site decoded wrong: %+v (err %v)", offer, err)
	}

	accept, err := DecodeControlMessage(readFixture(t, "control_route_accept"))
	if err != nil || accept.Route == nil || accept.Route.Kind != RouteControlKindAccept ||
		accept.Route.RouteID == "" {
		t.Fatalf("control_route_accept decoded wrong: %+v (err %v)", accept, err)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func hasSiteAdvert(xs []SiteAdvert, id string) bool {
	for _, x := range xs {
		if x.ID == id {
			return true
		}
	}
	return false
}
