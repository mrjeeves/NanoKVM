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
	if p.Kvm.JoiningMesh != "cec-kvm-ab3de-fg7hj" {
		t.Errorf("kvm profile joining_mesh = %q, want cec-kvm-ab3de-fg7hj", p.Kvm.JoiningMesh)
	}
	if len(p.Kvm.Meshes) != 2 {
		t.Errorf("kvm profile meshes = %v, want the fleet + owner-added pair", p.Kvm.Meshes)
	}

	// A freshly-claimed-but-unattached KVM omits attached_to and sits alone
	// on its joining mesh.
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
	if c.Kvm == nil || len(c.Kvm.Meshes) != 1 || c.Kvm.Meshes[0] != c.Kvm.JoiningMesh {
		t.Errorf("claimable kvm should be alone on its joining mesh, got %+v", c.Kvm)
	}
}

// The control-plane fixtures must decode to the right variant + fields via the
// forward-compatible DecodeControlMessage.
func TestContractControlMessages(t *testing.T) {
	attach, err := DecodeControlMessage(readFixture(t, "control_kvm_attach"))
	if err != nil || attach.Kind != ControlKindKvm || attach.Kvm == nil ||
		attach.Kvm.Kind != KvmControlKindAttach || attach.Kvm.Node != "den-tower" ||
		attach.Kvm.Label != "Den Tower" {
		t.Fatalf("control_kvm_attach decoded wrong: %+v (err %v)", attach, err)
	}

	detach, err := DecodeControlMessage(readFixture(t, "control_kvm_detach"))
	if err != nil || detach.Kvm == nil || detach.Kvm.Kind != KvmControlKindDetach {
		t.Fatalf("control_kvm_detach decoded wrong: %+v (err %v)", detach, err)
	}

	meshAdd, err := DecodeControlMessage(readFixture(t, "control_kvm_mesh_add"))
	if err != nil || meshAdd.Kvm == nil || meshAdd.Kvm.Kind != KvmControlKindMeshAdd ||
		meshAdd.Kvm.NetworkID != "den-site-mesh" {
		t.Fatalf("control_kvm_mesh_add decoded wrong: %+v (err %v)", meshAdd, err)
	}

	meshRemove, err := DecodeControlMessage(readFixture(t, "control_kvm_mesh_remove"))
	if err != nil || meshRemove.Kvm == nil || meshRemove.Kvm.Kind != KvmControlKindMeshRemove ||
		meshRemove.Kvm.NetworkID != "den-site-mesh" {
		t.Fatalf("control_kvm_mesh_remove decoded wrong: %+v (err %v)", meshRemove, err)
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

// The daemon-socket fixtures pin what the bridge actually WRITES to the
// control socket, exercised through the real Socket methods against a fake
// daemon — previously these six fixtures sat in testdata unloaded by any test,
// so the file paths existed but pinned nothing. (The mirror↔daemon link is
// still only pinned on the AllMyStuff side; these at least pin Go↔mirror.)
func TestContractDaemonSocketRequests(t *testing.T) {
	f := startFakeDaemon(t)
	ctl, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer ctl.Close()

	// channel_subscribe — the exact line, including the client_id field.
	subFix := readFixture(t, "req_channel_subscribe")
	var sub struct {
		ClientID string `json:"client_id"`
		Network  string `json:"network"`
		Channel  string `json:"channel"`
	}
	if err := json.Unmarshal(subFix, &sub); err != nil {
		t.Fatal(err)
	}
	if err := ctl.ChannelSubscribe(sub.ClientID, sub.Network, sub.Channel); err != nil {
		t.Fatalf("channel_subscribe: %v", err)
	}

	// channel_send_all — with the fixture's own NodeProfile payload verbatim.
	sendFix := readFixture(t, "req_channel_send_all")
	var send struct {
		Network string          `json:"network"`
		Channel string          `json:"channel"`
		Payload json.RawMessage `json:"payload"`
	}
	if err := json.Unmarshal(sendFix, &send); err != nil {
		t.Fatal(err)
	}
	if err := ctl.ChannelSendAll(send.Network, send.Channel, send.Payload); err != nil {
		t.Fatalf("channel_send_all: %v", err)
	}

	// capabilities_set — the fixture's capability matrix verbatim.
	capFix := readFixture(t, "req_capabilities_set")
	var caps struct {
		Network      string          `json:"network"`
		Capabilities json.RawMessage `json:"capabilities"`
	}
	if err := json.Unmarshal(capFix, &caps); err != nil {
		t.Fatal(err)
	}
	if err := ctl.CapabilitiesSet(caps.Network, caps.Capabilities); err != nil {
		t.Fatalf("capabilities_set: %v", err)
	}

	for fixName, op := range map[string]string{
		"req_channel_subscribe": "channel_subscribe",
		"req_channel_send_all":  "channel_send_all",
		"req_capabilities_set":  "capabilities_set",
	} {
		reqs := f.requests(op)
		if len(reqs) != 1 {
			t.Fatalf("%s: %d requests recorded", op, len(reqs))
		}
		got, _ := json.Marshal(reqs[0])
		if !jsonEqual(readFixture(t, fixName), got) {
			t.Errorf("%s drifted from fixture\n  fixture: %s\n  wire:    %s", op, readFixture(t, fixName), got)
		}
	}
}

// The daemon-socket response fixtures decode through the bridge's real types.
func TestContractDaemonSocketResponses(t *testing.T) {
	// response_ok — the generic ack shape, carrying a client_id here.
	var resp Response
	if err := json.Unmarshal(readFixture(t, "response_ok"), &resp); err != nil || !resp.OK {
		t.Fatalf("response_ok decoded wrong: %+v (err %v)", resp, err)
	}
	var data struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil || data.ClientID == "" {
		t.Fatalf("response_ok data decoded wrong: %+v (err %v)", data, err)
	}

	// client_id — the bare "c<n>" string form the ack carries.
	var id string
	if err := json.Unmarshal(readFixture(t, "client_id"), &id); err != nil || id != data.ClientID {
		t.Fatalf("client_id fixture = %q (err %v), want %q", id, err, data.ClientID)
	}

	// server_out_channel_inbound — a push frame whose payload must decode as a
	// control message through the real dispatcher.
	var ci ChannelInbound
	if err := json.Unmarshal(readFixture(t, "server_out_channel_inbound"), &ci); err != nil {
		t.Fatal(err)
	}
	if ci.Channel != ChannelControl || ci.From == "" {
		t.Fatalf("server_out_channel_inbound fields wrong: %+v", ci)
	}
	msg, err := DecodeControlMessage(ci.Payload)
	if err != nil || msg.Kind != ControlKindKvm || msg.Kvm == nil || msg.Kvm.Kind != KvmControlKindAttach {
		t.Fatalf("server_out payload decoded wrong: %+v (err %v)", msg, err)
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
