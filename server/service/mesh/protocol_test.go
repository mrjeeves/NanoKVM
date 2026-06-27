package mesh

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// roundTripJSON marshals v, unmarshals into a fresh value of the same type, and
// returns the JSON string for assertions.
func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestNodeProfileRoundTripWithKvm(t *testing.T) {
	owner := "my-laptop"
	attached := "den-tower"
	p := NodeProfile{
		Protocol: ProtocolVersion,
		Node:     "kvm-1",
		Label:    "Den KVM",
		Hostname: "den-kvm.local",
		Summary: InventorySummary{
			OS:          "linux",
			CPU:         "T-Head C906",
			RAMBytes:    256 << 20,
			DeviceCount: 1,
		},
		Capabilities: []Capability{},
		Owner:        &owner,
		Claimable:    false,
		Boot:         42,
		Features:     []string{FeatureKVM, FeatureSites},
		Sites: []SiteAdvert{{
			ID:       "tcp:80",
			Label:    "KVM Web UI",
			Port:     80,
			Scheme:   "http",
			Loopback: false,
		}},
		Version:    "1.4.2",
		FleetName:  "Casey",
		FleetOwner: "Casey",
		Kvm: &KvmAdvert{
			AttachedTo: &attached,
			Web:        "tcp:80",
		},
	}
	s := mustMarshal(t, p)
	// Exact-shape assertions against the Rust contract.
	if !strings.Contains(s, `"kvm"`) || !strings.Contains(s, `"attached_to":"den-tower"`) {
		t.Fatalf("kvm advert not in wire shape: %s", s)
	}
	if !strings.Contains(s, `"features":["kvm","sites"]`) {
		t.Fatalf("features not in wire shape: %s", s)
	}
	if !strings.Contains(s, `"web":"tcp:80"`) {
		t.Fatalf("web not in wire shape: %s", s)
	}

	var back NodeProfile
	if err := json.Unmarshal([]byte(s), &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(p, back) {
		t.Fatalf("round trip mismatch:\n got %+v\nwant %+v", back, p)
	}
}

func TestNodeProfileOmitsEmptyOptionalFields(t *testing.T) {
	// A freshly-claimed-but-unattached KVM omits attached_to; an unnamed fleet
	// omits fleet_name/fleet_owner. NOTE: `owner` is NOT omitted when unclaimed —
	// the Rust source serializes it as null (#[serde(default)] without skip), so
	// we must too, or the wire shape drifts (verified by the contract fixtures).
	p := NodeProfile{
		Protocol:     ProtocolVersion,
		Node:         "kvm-1",
		Label:        "KVM",
		Hostname:     "kvm",
		Summary:      InventorySummary{OS: "linux"},
		Capabilities: []Capability{},
		Claimable:    true,
		Boot:         1,
		Features:     []string{FeatureKVM, FeatureSites},
		Sites:        []SiteAdvert{{ID: "tcp:80", Label: "Web", Port: 80, Scheme: "http"}},
		Version:      "1.0.0",
		Kvm:          &KvmAdvert{Web: "tcp:80"},
	}
	s := mustMarshal(t, p)
	if !strings.Contains(s, `"owner":null`) {
		t.Fatalf("unclaimed device must serialize owner as null (matches Rust): %s", s)
	}
	if strings.Contains(s, "attached_to") {
		t.Fatalf("unattached KVM must omit attached_to: %s", s)
	}
	if strings.Contains(s, "fleet_name") || strings.Contains(s, "fleet_owner") {
		t.Fatalf("unfleeted device must omit fleet fields: %s", s)
	}
	// An empty-web, unattached KvmAdvert serializes as {}.
	if !strings.Contains(s, `"kvm":{`) {
		t.Fatalf("kvm key must be present: %s", s)
	}
}

func TestOlderPeerProfileDecodes(t *testing.T) {
	// A minimal/older advert (no features/sites/kvm/version) must still decode.
	const j = `{
		"protocol":1,"node":"old","label":"Old","hostname":"old",
		"summary":{"os":"linux","cpu":"cpu","ram_bytes":1,"device_count":1}
	}`
	var p NodeProfile
	if err := json.Unmarshal([]byte(j), &p); err != nil {
		t.Fatalf("older peer profile failed decode: %v", err)
	}
	if p.Kvm != nil || len(p.Features) != 0 || p.Version != "" {
		t.Fatalf("absent fields should be zero/nil: %+v", p)
	}
}

func TestControlMessageOwnershipRoundTrip(t *testing.T) {
	// Claim from a peer: outer t:"ownership", kind:"claim", owner present.
	raw := json.RawMessage(`{"t":"ownership","kind":"claim","owner":"laptop-AB12C"}`)
	msg, err := DecodeControlMessage(raw)
	if err != nil {
		t.Fatalf("decode claim: %v", err)
	}
	if msg.Kind != ControlKindOwnership || msg.Ownership == nil {
		t.Fatalf("expected ownership, got %+v", msg)
	}
	if msg.Ownership.Kind != OwnershipKindClaim || msg.Ownership.Owner != "laptop-AB12C" {
		t.Fatalf("bad claim: %+v", msg.Ownership)
	}

	// Claimed reply built by NewClaimed must carry t:"ownership", kind:"claimed".
	reply := NewClaimed("laptop-AB12C")
	s := string(reply.Payload())
	if !strings.Contains(s, `"t":"ownership"`) || !strings.Contains(s, `"kind":"claimed"`) {
		t.Fatalf("Claimed reply wrong shape: %s", s)
	}
	if !strings.Contains(s, `"owner":"laptop-AB12C"`) {
		t.Fatalf("Claimed reply missing owner: %s", s)
	}
	// And it decodes back.
	back, err := DecodeControlMessage(reply.Payload())
	if err != nil || back.Ownership == nil || back.Ownership.Kind != OwnershipKindClaimed {
		t.Fatalf("Claimed reply doesn't round trip: %+v %v", back, err)
	}
}

func TestControlMessageFleetKey(t *testing.T) {
	venue := `{"signaling":{"servers":["wss://x"]}}`
	raw, _ := json.Marshal(map[string]interface{}{
		"t":     "ownership",
		"kind":  "fleet_key",
		"key":   "abcd",
		"name":  "Casey",
		"venue": venue,
	})
	msg, err := DecodeControlMessage(raw)
	if err != nil {
		t.Fatalf("decode fleet_key: %v", err)
	}
	if msg.Ownership == nil || msg.Ownership.Kind != OwnershipKindFleetKey {
		t.Fatalf("expected fleet_key, got %+v", msg)
	}
	if msg.Ownership.Key != "abcd" || msg.Ownership.Name != "Casey" {
		t.Fatalf("bad fleet_key fields: %+v", msg.Ownership)
	}
	if msg.Ownership.Venue == nil || *msg.Ownership.Venue != venue {
		t.Fatalf("venue not carried: %+v", msg.Ownership.Venue)
	}
}

func TestControlMessageKvmRoundTrip(t *testing.T) {
	raw := json.RawMessage(`{"t":"kvm","kind":"attach","node":"den-tower"}`)
	msg, err := DecodeControlMessage(raw)
	if err != nil {
		t.Fatalf("decode attach: %v", err)
	}
	if msg.Kind != ControlKindKvm || msg.Kvm == nil ||
		msg.Kvm.Kind != KvmControlKindAttach || msg.Kvm.Node != "den-tower" {
		t.Fatalf("bad kvm attach: %+v", msg)
	}

	detach := json.RawMessage(`{"t":"kvm","kind":"detach"}`)
	msg, err = DecodeControlMessage(detach)
	if err != nil || msg.Kvm == nil || msg.Kvm.Kind != KvmControlKindDetach {
		t.Fatalf("bad kvm detach: %+v %v", msg, err)
	}
}

func TestControlMessageRouteOfferAndAccept(t *testing.T) {
	raw := json.RawMessage(`{"t":"route","kind":"offer","route":{
		"id":"r1","from":"peer:site","to":"kvm:site-view:0","media":"generic"
	}}`)
	msg, err := DecodeControlMessage(raw)
	if err != nil {
		t.Fatalf("decode offer: %v", err)
	}
	if msg.Route == nil || msg.Route.Kind != RouteControlKindOffer {
		t.Fatalf("expected route offer: %+v", msg)
	}
	if msg.Route.Route == nil || !msg.Route.Route.IsSiteRoute() {
		t.Fatalf("offer route should be a site route: %+v", msg.Route.Route)
	}

	acc := NewRouteAccept("r1")
	s := string(acc.Payload())
	if !strings.Contains(s, `"t":"route"`) || !strings.Contains(s, `"kind":"accept"`) ||
		!strings.Contains(s, `"route_id":"r1"`) {
		t.Fatalf("route accept wrong shape: %s", s)
	}
}

func TestControlMessageForwardCompatUnknown(t *testing.T) {
	// An unknown outer tag and an unknown nested kind must both decode without
	// error (never failing the control channel).
	unknownOuter := json.RawMessage(`{"t":"teleport","foo":1}`)
	msg, err := DecodeControlMessage(unknownOuter)
	if err != nil {
		t.Fatalf("unknown outer tag errored: %v", err)
	}
	if msg.Kind != ControlKindUnknown {
		t.Fatalf("unknown outer tag should map to Unknown, got %q", msg.Kind)
	}

	unknownKvm := json.RawMessage(`{"t":"kvm","kind":"attach_two_at_once"}`)
	msg, err = DecodeControlMessage(unknownKvm)
	if err != nil {
		t.Fatalf("unknown nested kind errored: %v", err)
	}
	if msg.Kvm == nil || msg.Kvm.Kind != KvmControlKindUnknown {
		t.Fatalf("unknown nested kind should map to Unknown, got %+v", msg.Kvm)
	}

	unknownOwnership := json.RawMessage(`{"t":"ownership","kind":"banish"}`)
	msg, _ = DecodeControlMessage(unknownOwnership)
	if msg.Ownership == nil || msg.Ownership.Kind != OwnershipKindUnknown {
		t.Fatalf("unknown ownership kind should map to Unknown, got %+v", msg.Ownership)
	}
}

func TestSiteFrameRoundTripAndDemux(t *testing.T) {
	events := []SiteFrame{
		NewSiteOpen("route:peer:site->kvm:site-view:0", 1, 7, 80),
		NewSiteData("route:peer:site->kvm:site-view:0", 2, 7, []byte("GET / HTTP/1.1")),
		NewSiteClose("route:peer:site->kvm:site-view:0", 3, 7),
	}
	for _, f := range events {
		b, err := json.Marshal(f)
		if err != nil {
			t.Fatalf("marshal site frame: %v", err)
		}
		s := string(b)
		if !strings.Contains(s, `"t":"site"`) {
			t.Fatalf("site frame missing t tag: %s", s)
		}
		back, ok := DecodeSiteFrame(b)
		if !ok {
			t.Fatalf("site frame failed demux: %s", s)
		}
		if back.Kind != f.Kind || back.Conn != f.Conn || back.Route != f.Route || back.Seq != f.Seq {
			t.Fatalf("site frame mismatch: got %+v want %+v", back, f)
		}
		if f.Kind == SiteEventKindData && string(back.Data) != string(f.Data) {
			t.Fatalf("site data mismatch: %q vs %q", back.Data, f.Data)
		}
	}

	// Data bytes travel as base64.
	b, _ := json.Marshal(NewSiteData("r", 0, 9, []byte{0xFF}))
	if !strings.Contains(string(b), `"data":"/w=="`) {
		t.Fatalf("site data should be base64: %s", b)
	}

	// A non-site media frame is not a SiteFrame.
	if _, ok := DecodeSiteFrame(json.RawMessage(`{"t":"video","route":"r","seq":1}`)); ok {
		t.Fatalf("video frame should not decode as site")
	}
	// An unknown site event kind decodes to Unknown rather than failing.
	f, ok := DecodeSiteFrame(json.RawMessage(`{"t":"site","route":"r","seq":1,"kind":"warp","conn":3}`))
	if !ok || f.Kind != SiteEventKindUnknown {
		t.Fatalf("unknown site event should map to Unknown: %+v ok=%v", f, ok)
	}
}

func TestControlRequestResponseAndClientID(t *testing.T) {
	// A daemon Response decodes ok/error/data.
	resp := `{"ok":true,"data":{"client_id":"c3","subscribed":true}}`
	var r Response
	if err := json.Unmarshal([]byte(resp), &r); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if !r.OK {
		t.Fatalf("response should be ok")
	}
	var data struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(r.Data, &data); err != nil {
		t.Fatalf("decode data: %v", err)
	}
	if data.ClientID != "c3" {
		t.Fatalf("client_id should serialize as c<n>, got %q", data.ClientID)
	}

	// An error response.
	errResp := `{"ok":false,"error":"unknown network: x"}`
	if err := json.Unmarshal([]byte(errResp), &r); err != nil {
		t.Fatalf("decode err response: %v", err)
	}
	if r.OK || r.Error == "" {
		t.Fatalf("error response should carry ok=false + error")
	}

	// A request marshals to the {"op":...} line shape.
	req := request{"op": "channel_subscribe", "client_id": "c3", "network": "n", "channel": ChannelPresence}
	b, _ := json.Marshal(req)
	if !strings.Contains(string(b), `"op":"channel_subscribe"`) {
		t.Fatalf("request op shape wrong: %s", b)
	}

	// ChannelInbound (a ServerOut push frame) decodes.
	ci := `{"kind":"channel_inbound","network":"n","from":"peer-AB12C","channel":"allmystuff/control/v1","payload":{"t":"kvm","kind":"detach"}}`
	var inbound ChannelInbound
	if err := json.Unmarshal([]byte(ci), &inbound); err != nil {
		t.Fatalf("decode channel_inbound: %v", err)
	}
	if inbound.From != "peer-AB12C" || inbound.Channel != ChannelControl {
		t.Fatalf("channel_inbound fields wrong: %+v", inbound)
	}
}

func TestFleetNetworkIDIsDeterministicWordSalad(t *testing.T) {
	// Deterministic, and shape adjective-name-suffix from the frozen lists.
	key := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	a := DeriveFleetNetworkID(key)
	if a != DeriveFleetNetworkID(key) {
		t.Fatalf("derivation not deterministic")
	}
	parts := strings.Split(a, "-")
	if len(parts) != 3 {
		t.Fatalf("expected adjective-name-suffix, got %q", a)
	}
	if !contains(fleetAdjectives, parts[0]) {
		t.Fatalf("adjective %q not in frozen list (id %q)", parts[0], a)
	}
	if !contains(fleetNames, parts[1]) {
		t.Fatalf("name %q not in frozen list (id %q)", parts[1], a)
	}
	if len(parts[2]) != 5 {
		t.Fatalf("suffix should be 5 chars: %q", a)
	}
	for _, c := range a {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			t.Fatalf("id must be lowercase alnum + '-': %q", a)
		}
	}
	// Distinct keys differ.
	if a == DeriveFleetNetworkID(key+"x") {
		t.Fatalf("distinct keys derived the same id")
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}
