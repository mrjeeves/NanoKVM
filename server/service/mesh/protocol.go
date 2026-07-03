// Package mesh makes the NanoKVM a first-class node on the AllMyStuff mesh.
//
// It connects to the local myownmesh daemon's control socket, advertises this
// device as a KVM appliance on the AllMyStuff presence plane, tunnels its own
// web UI over the mesh "sites" plane with the login bypassed, and supports
// claim + attach/detach over the control plane.
//
// v1 scope: NO native screen/HID streaming — the tunneled web UI delivers the
// full KVM experience. This package therefore imports none of the CGO/libkvm
// packages (server/service/hid, server/common), so it builds and `go test`s on
// host amd64.
//
// This file mirrors the AllMyStuff wire contract in Go with exact json tags.
// Decoding is forward-compatible: we never use DisallowUnknownFields, and every
// tagged enum decodes an unrecognised tag to an Unknown/zero value rather than
// failing — so a newer peer's message never breaks an older NanoKVM.
package mesh

import "encoding/json"

// ---- constants (mirror allmystuff-protocol/src/app.rs) ----------------------

const (
	AppID           = "allmystuff-cloud-mesh-v1"
	ProtocolVersion = 1

	ChannelPresence = "allmystuff/presence/v1"
	ChannelControl  = "allmystuff/control/v1"
	ChannelMedia    = "allmystuff/media/v1"

	CapTagAllMyStuff = "allmystuff"

	FeatureKVM   = "kvm"
	FeatureSites = "sites"
)

// SiteChunkBytes is the max raw bytes per SiteEvent Data frame before base64 —
// kept well under the daemon channel's ~64 KiB message ceiling once base64
// (×4/3) and the JSON envelope are added. Mirrors SITE_CHUNK_BYTES.
const SiteChunkBytes = 40 * 1024

// ---- InventorySummary -------------------------------------------------------

// InventorySummary is a thumbnail of a node's hardware for the graph node card.
type InventorySummary struct {
	OS          string `json:"os"`
	CPU         string `json:"cpu"`
	RAMBytes    uint64 `json:"ram_bytes"`
	DeviceCount uint32 `json:"device_count"`
}

// ---- SiteAdvert -------------------------------------------------------------

// SiteAdvert is one TCP service a node exposes for reverse-proxy over the mesh.
// Scheme and Loopback mirror Rust's #[serde(default)] WITHOUT skip_serializing_if
// — they are always present on the wire (scheme "" and loopback false included),
// so no omitempty here or the shape drifts from allmystuff-protocol.
type SiteAdvert struct {
	ID       string `json:"id"`
	Label    string `json:"label"`
	Port     uint16 `json:"port"`
	Scheme   string `json:"scheme"`
	Loopback bool   `json:"loopback"`
}

// ---- KvmAdvert --------------------------------------------------------------

// KvmAdvert is a KVM appliance's binding, carried in NodeProfile.Kvm. AttachedTo
// is the graph node this KVM physically controls; Web is the SiteAdvert.ID that
// serves the KVM's own web UI. JoiningMesh is the per-device cec-kvm-xxxxx-xxxxx
// network the device returns to when unclaimed/reset (the same name it shows on
// its screen); Meshes is every network id it's currently joined to, fleet
// included — the membership list a fleet owner curates with KvmControl
// MeshAdd/MeshRemove.
//
// Note the omitempty contract: AttachedTo serialises *without* the key when nil
// (mirrors Rust's Option None / skip_serializing_if), Web/JoiningMesh likewise
// when "", and Meshes when empty.
type KvmAdvert struct {
	AttachedTo  *string  `json:"attached_to,omitempty"`
	Web         string   `json:"web,omitempty"`
	JoiningMesh string   `json:"joining_mesh,omitempty"`
	Meshes      []string `json:"meshes,omitempty"`
}

// ---- NodeProfile ------------------------------------------------------------

// NodeProfile is what a node tells its peers about itself on the presence
// channel. The omitempty tags match AllMyStuff's skip_serializing_if so an
// older receiver sees exactly the presence shape it always did.
type NodeProfile struct {
	Protocol     uint32           `json:"protocol"`
	Node         string           `json:"node"`
	Label        string           `json:"label"`
	Hostname     string           `json:"hostname"`
	Summary      InventorySummary `json:"summary"`
	Capabilities []Capability     `json:"capabilities"`
	// Owner mirrors Rust's #[serde(default)] Option WITHOUT skip — it serializes
	// as null (not omitted) when unclaimed, so no omitempty here.
	Owner      *string      `json:"owner"`
	Claimable  bool         `json:"claimable"`
	Boot       uint64       `json:"boot"`
	Features   []string     `json:"features,omitempty"`
	Sites      []SiteAdvert `json:"sites,omitempty"`
	Version    string       `json:"version,omitempty"`
	FleetName  string       `json:"fleet_name,omitempty"`
	FleetOwner string       `json:"fleet_owner,omitempty"`
	Kvm        *KvmAdvert   `json:"kvm,omitempty"`
	// SentAt is the sender's wall clock (Unix ms) stamped at each send — the
	// sample behind AllMyStuff's passive clock-skew estimate (app.rs sent_at,
	// skip_serializing_if u64_is_zero ↔ omitempty here). Stamped per send, not
	// at profile build, and only when this device's clock is sane: the KVM has
	// no RTC, and a 1970 sample would read as ~56 years of skew on every peer.
	// Absent (0) simply means "no sample" — old receivers ignore it.
	SentAt uint64 `json:"sent_at,omitempty"`
}

// ---- graph model (mirror allmystuff-graph/src/model.rs) ---------------------

// Capability is one routable thing on one node. v1 advertises none of these
// (the tunneled web UI carries everything), but the type is mirrored so the
// presence advert round-trips identically.
// Origin and Default mirror Rust's #[serde(default)] WITHOUT skip_serializing_if
// — always present on the wire (origin "" and default false included), so no
// omitempty here.
type Capability struct {
	ID      string `json:"id"`
	Node    string `json:"node"`
	Label   string `json:"label"`
	Media   string `json:"media"`
	Flow    string `json:"flow"`
	Origin  string `json:"origin"`
	Default bool   `json:"default"`
}

// Route is a live connection between two capabilities. A site route is
// identified by Media == "generic" and From ending in ":site".
type Route struct {
	ID    string `json:"id"`
	From  string `json:"from"`
	To    string `json:"to"`
	Media string `json:"media"`
}

// IsSiteRoute reports whether r is an AllMyStuff "sites" plane route — generic
// media whose source capability id ends in ":site".
func (r Route) IsSiteRoute() bool {
	return r.Media == "generic" && endsWith(r.From, ":site")
}

func endsWith(s, suffix string) bool {
	return len(s) >= len(suffix) && s[len(s)-len(suffix):] == suffix
}

// ---- ControlMessage (mirror app.rs ControlMessage, tagged "t") --------------

// ControlKind is the discriminant of the outer ControlMessage "t" tag.
type ControlKind string

const (
	ControlKindRoute     ControlKind = "route"
	ControlKindShare     ControlKind = "share"
	ControlKindOwnership ControlKind = "ownership"
	ControlKindSite      ControlKind = "site"
	ControlKindApp       ControlKind = "app"
	ControlKindKvm       ControlKind = "kvm"
	// ControlKindUnknown is the forward-compatible fallback for a "t" a newer
	// build introduced. The whole message decodes (never errors) and is
	// ignored, so the traffic this build understands keeps flowing.
	ControlKindUnknown ControlKind = "unknown"
)

// ControlMessage is point-to-point control traffic on CHANNEL_CONTROL. Only the
// payload matching Kind is populated after DecodeControlMessage.
type ControlMessage struct {
	Kind      ControlKind
	Route     *RouteControl
	Ownership *OwnershipControl
	Kvm       *KvmControl
	App       *AppControl
	// Raw is the original JSON, retained for variants this bridge doesn't act
	// on (share/site) so nothing is silently lost.
	Raw json.RawMessage
}

// DecodeControlMessage parses a CHANNEL_CONTROL payload into a ControlMessage.
// Unknown outer tags and unknown nested kinds decode to Unknown rather than
// erroring, mirroring the Rust #[serde(other)] contract.
func DecodeControlMessage(raw json.RawMessage) (ControlMessage, error) {
	var probe struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return ControlMessage{}, err
	}
	msg := ControlMessage{Kind: ControlKind(probe.T), Raw: raw}
	switch ControlKind(probe.T) {
	case ControlKindRoute:
		var rc RouteControl
		if err := json.Unmarshal(raw, &rc); err != nil {
			return ControlMessage{}, err
		}
		msg.Route = &rc
	case ControlKindOwnership:
		var oc OwnershipControl
		if err := json.Unmarshal(raw, &oc); err != nil {
			return ControlMessage{}, err
		}
		msg.Ownership = &oc
	case ControlKindKvm:
		var kc KvmControl
		if err := json.Unmarshal(raw, &kc); err != nil {
			return ControlMessage{}, err
		}
		msg.Kvm = &kc
	case ControlKindApp:
		var ac AppControl
		if err := json.Unmarshal(raw, &ac); err != nil {
			return ControlMessage{}, err
		}
		msg.App = &ac
	default:
		// share / site / anything newer: keep Raw, mark Unknown so
		// callers don't misroute it. We deliberately don't fail.
		if probe.T != string(ControlKindShare) &&
			probe.T != string(ControlKindSite) {
			msg.Kind = ControlKindUnknown
		}
	}
	return msg, nil
}

// ---- OwnershipControl (tagged "kind") ---------------------------------------

// OwnershipKind discriminates an OwnershipControl message.
type OwnershipKind string

const (
	OwnershipKindClaim         OwnershipKind = "claim"
	OwnershipKindClaimed       OwnershipKind = "claimed"
	OwnershipKindDeclined      OwnershipKind = "declined"
	OwnershipKindRelease       OwnershipKind = "release"
	OwnershipKindFleetKey      OwnershipKind = "fleet_key"
	OwnershipKindFleetDeparted OwnershipKind = "fleet_departed"
	OwnershipKindUnknown       OwnershipKind = "unknown"
)

// OwnershipControl adopts/releases a device and hands down a fleet credential.
// It carries every field of every variant; only those for Kind are meaningful.
type OwnershipControl struct {
	Kind  OwnershipKind `json:"kind"`
	Owner string        `json:"owner,omitempty"`
	// FleetKey fields:
	Key    string  `json:"key,omitempty"`
	Name   string  `json:"name,omitempty"`
	Venue  *string `json:"venue,omitempty"`
	Reason string  `json:"reason,omitempty"`
}

// UnmarshalJSON decodes OwnershipControl, mapping an unrecognised "kind" to
// OwnershipKindUnknown rather than failing.
func (o *OwnershipControl) UnmarshalJSON(b []byte) error {
	type raw OwnershipControl
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*o = OwnershipControl(r)
	switch o.Kind {
	case OwnershipKindClaim, OwnershipKindClaimed, OwnershipKindDeclined,
		OwnershipKindRelease, OwnershipKindFleetKey, OwnershipKindFleetDeparted:
	default:
		o.Kind = OwnershipKindUnknown
	}
	return nil
}

// NewClaimed builds the Ownership Claimed confirmation a KVM replies with.
func NewClaimed(owner string) ControlMessage {
	return wrapControl(ControlKindOwnership, OwnershipControl{
		Kind:  OwnershipKindClaimed,
		Owner: owner,
	})
}

// ---- KvmControl (tagged "kind") ---------------------------------------------

// KvmControlKind discriminates a KvmControl message.
type KvmControlKind string

const (
	KvmControlKindAttach     KvmControlKind = "attach"
	KvmControlKindDetach     KvmControlKind = "detach"
	KvmControlKindMeshAdd    KvmControlKind = "mesh_add"
	KvmControlKindMeshRemove KvmControlKind = "mesh_remove"
	KvmControlKindUnknown    KvmControlKind = "unknown"
)

// KvmControl curates a KVM appliance: its attachment (Attach/Detach, with the
// target's display label riding along so the KVM can rename itself
// KVM-<label>) and its mesh membership (MeshAdd/MeshRemove, carrying the
// network id). It carries every field of every variant; only those for Kind
// are meaningful.
type KvmControl struct {
	Kind KvmControlKind `json:"kind"`
	Node string         `json:"node,omitempty"`
	// Label is the attach target's display label at attach time (attach only;
	// cosmetic, best-effort — empty from older senders).
	Label string `json:"label,omitempty"`
	// NetworkID is the mesh to join/leave (mesh_add / mesh_remove only).
	NetworkID string `json:"network_id,omitempty"`
}

// UnmarshalJSON decodes KvmControl, mapping an unrecognised "kind" to
// KvmControlKindUnknown rather than failing.
func (k *KvmControl) UnmarshalJSON(b []byte) error {
	type raw KvmControl
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*k = KvmControl(r)
	switch k.Kind {
	case KvmControlKindAttach, KvmControlKindDetach,
		KvmControlKindMeshAdd, KvmControlKindMeshRemove:
	default:
		k.Kind = KvmControlKindUnknown
	}
	return nil
}

// ---- AppControl (tagged "kind") ----------------------------------------------

// AppControlKind discriminates an AppControl message (app.rs AppControl).
type AppControlKind string

const (
	// AppControlKindUpgrade is "update yourself and restart" — meaningless on
	// the KVM (its firmware isn't AllMyStuff's self-updater), so it decodes
	// and is ignored.
	AppControlKindUpgrade AppControlKind = "upgrade"
	// AppControlKindRestart is "relaunch your app onto the same build" — for
	// the KVM that's restarting NanoKVM-Server via its init script.
	AppControlKindRestart AppControlKind = "restart"
	// AppControlKindRestartDevice is "reboot the machine you run on" — the
	// recovery step heavier than an app restart. The receiver hands it to the
	// OS; its presence dropping and returning is the confirmation (no reply).
	AppControlKindRestartDevice AppControlKind = "restart_device"
	AppControlKindUnknown       AppControlKind = "unknown"
)

// AppControl is an app-level command (upgrade / restart / restart_device),
// gated owner/fleet by the receiver exactly like KVM curation.
type AppControl struct {
	Kind AppControlKind `json:"kind"`
}

// UnmarshalJSON decodes AppControl, mapping an unrecognised "kind" to
// AppControlKindUnknown rather than failing (mirrors Rust's #[serde(other)]).
func (a *AppControl) UnmarshalJSON(b []byte) error {
	type raw AppControl
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*a = AppControl(r)
	switch a.Kind {
	case AppControlKindUpgrade, AppControlKindRestart, AppControlKindRestartDevice:
	default:
		a.Kind = AppControlKindUnknown
	}
	return nil
}

// ---- RouteControl (tagged "kind") -------------------------------------------

// RouteControlKind discriminates a RouteControl message. The bridge acts on
// Offer/Teardown for site routes; other kinds decode but are ignored.
type RouteControlKind string

const (
	RouteControlKindOffer    RouteControlKind = "offer"
	RouteControlKindAccept   RouteControlKind = "accept"
	RouteControlKindReject   RouteControlKind = "reject"
	RouteControlKindTeardown RouteControlKind = "teardown"
	RouteControlKindUnknown  RouteControlKind = "unknown"
)

// RouteControl is the lifecycle of a single cross-node route. Only fields
// relevant to a "sites" route are mirrored (plus the route id for Accept/
// Reject/Teardown); the many display/terminal-specific fields are forward-
// compatibly ignored.
type RouteControl struct {
	Kind    RouteControlKind `json:"kind"`
	Route   *Route           `json:"route,omitempty"`
	RouteID string           `json:"route_id,omitempty"`
	Reason  string           `json:"reason,omitempty"`
}

// UnmarshalJSON decodes RouteControl, mapping an unrecognised "kind" to
// RouteControlKindUnknown rather than failing.
func (rc *RouteControl) UnmarshalJSON(b []byte) error {
	type raw RouteControl
	var r raw
	if err := json.Unmarshal(b, &r); err != nil {
		return err
	}
	*rc = RouteControl(r)
	switch rc.Kind {
	case RouteControlKindOffer, RouteControlKindAccept,
		RouteControlKindReject, RouteControlKindTeardown:
	default:
		rc.Kind = RouteControlKindUnknown
	}
	return nil
}

// NewRouteAccept builds the RouteControl Accept reply for a route id.
func NewRouteAccept(routeID string) ControlMessage {
	return wrapControl(ControlKindRoute, struct {
		Kind    string `json:"kind"`
		RouteID string `json:"route_id"`
	}{Kind: string(RouteControlKindAccept), RouteID: routeID})
}

// NewRouteReject builds the RouteControl Reject reply for a route id. The
// reason travels to the offerer's UI — a refusal must be visible, not a silent
// nothing-happened (the offerer would otherwise wait out its 15 s offer expiry
// and blame the network).
func NewRouteReject(routeID, reason string) ControlMessage {
	return wrapControl(ControlKindRoute, struct {
		Kind    string `json:"kind"`
		RouteID string `json:"route_id"`
		Reason  string `json:"reason,omitempty"`
	}{Kind: string(RouteControlKindReject), RouteID: routeID, Reason: reason})
}

// ---- control message encoding -----------------------------------------------

// wrapControl serialises an inner control payload tagged with the outer "t".
// The inner value's fields are flattened next to "t" so the wire shape matches
// serde's `#[serde(tag = "t")]` on ControlMessage.
func wrapControl(t ControlKind, inner interface{}) ControlMessage {
	// Marshal the inner payload, then splice in the "t" tag.
	innerBytes, _ := json.Marshal(inner)
	var m map[string]json.RawMessage
	_ = json.Unmarshal(innerBytes, &m)
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	tb, _ := json.Marshal(string(t))
	m["t"] = tb
	raw, _ := json.Marshal(m)
	return ControlMessage{Kind: t, Raw: raw}
}

// Payload returns the JSON the control message should be sent as.
func (m ControlMessage) Payload() json.RawMessage {
	return m.Raw
}

// ---- SiteFrame / SiteEvent (mirror allmystuff-session/src/media.rs) ---------

// SiteEventKind discriminates a SiteEvent on a tunneled connection.
type SiteEventKind string

const (
	SiteEventKindOpen    SiteEventKind = "open"
	SiteEventKindData    SiteEventKind = "data"
	SiteEventKindClose   SiteEventKind = "close"
	SiteEventKindUnknown SiteEventKind = "unknown"
)

// SiteFrame is one frame of a site route, demuxed off CHANNEL_MEDIA by t:"site".
// Event is flattened onto the frame (serde #[serde(flatten)]), so the fields of
// SiteEvent sit beside t/route/seq on the wire.
type SiteFrame struct {
	T     string `json:"t"`
	Route string `json:"route"`
	Seq   uint64 `json:"seq"`
	// flattened SiteEvent:
	Kind SiteEventKind `json:"kind"`
	Conn uint64        `json:"conn"`
	Port uint16        `json:"port,omitempty"`
	Data []byte        `json:"data,omitempty"` // base64 on the wire (Go's []byte default)
}

// NewSiteOpen builds an Open frame (client → host). Rarely used host-side, kept
// for symmetry and tests.
func NewSiteOpen(route string, seq, conn uint64, port uint16) SiteFrame {
	return SiteFrame{T: "site", Route: route, Seq: seq, Kind: SiteEventKindOpen, Conn: conn, Port: port}
}

// NewSiteData builds a Data frame carrying one chunk of a connection's bytes.
func NewSiteData(route string, seq, conn uint64, data []byte) SiteFrame {
	return SiteFrame{T: "site", Route: route, Seq: seq, Kind: SiteEventKindData, Conn: conn, Data: data}
}

// NewSiteClose builds a Close frame ending a connection's stream.
func NewSiteClose(route string, seq, conn uint64) SiteFrame {
	return SiteFrame{T: "site", Route: route, Seq: seq, Kind: SiteEventKindClose, Conn: conn}
}

// DecodeSiteFrame parses a CHANNEL_MEDIA payload as a SiteFrame. Returns ok=false
// for any payload whose t tag isn't "site" (another media plane), and maps an
// unknown event kind to SiteEventKindUnknown rather than failing.
func DecodeSiteFrame(raw json.RawMessage) (SiteFrame, bool) {
	var probe struct {
		T string `json:"t"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil || probe.T != "site" {
		return SiteFrame{}, false
	}
	var f SiteFrame
	if err := json.Unmarshal(raw, &f); err != nil {
		return SiteFrame{}, false
	}
	switch f.Kind {
	case SiteEventKindOpen, SiteEventKindData, SiteEventKindClose:
	default:
		f.Kind = SiteEventKindUnknown
	}
	return f, true
}
