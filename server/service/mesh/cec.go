package mesh

// CEC "hand raise" (Ask-for-help) support.
//
// A KVM raises its hand exactly the way a CEC Support customer does: it lives
// on the one well-known **Silent** support area (`cecsupport-clients`) and
// raises its hand by **joining the asking room** (`cecsupport-asking`) — a
// second Silent mesh whose signaling membership IS the technicians' queue.
// MyOwnMesh is a mesh signaling system for direct WebRTC peer-to-peer
// connections, and a Silent room uses only that half: co-present devices see
// each other's announces, nothing ever connects on its own, and nothing is
// routed through anything (the only data-path fallback anywhere is WebRTC's
// own TURN relay when NAT rules out a direct pair). Lowering the hand is
// leaving the room. A technician answers by dialing this device directly on
// the standing area; the KVM's own consent/route gating (control.go,
// native.go) guards any actual session.
//
// This replaces the `cec.presence` channel beacons: those rode data channels,
// which a Silent area rightly never opens on its own — the exact deadlock
// that once forced the area Open (auto-connecting every customer to every
// stranger).
//
// This is a NEW, additive plane: the KVM's normal presence lives on the
// AllMyStuff graph (allmystuff-cloud-mesh-v1, see protocol.go), whereas a hand
// raise rides the CEC plane. The two never mix. Everything here mirrors
// AllMyStuff's node/src/mesh.rs (cec_ask_help / cec_help_watch),
// node/src/cec.rs (help_network_config / ask_network_config), and the wire
// contract in crates/allmystuff-cec-protocol (lib.rs, ids.rs support_id).

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// CecHelpNetworkID is the single well-known standing support area every
	// CEC node lives on (allmystuff-cec-protocol::HELP_NETWORK_ID). Silent:
	// residents are discoverable at the signaling layer (that's how a
	// technician's pinned redial finds a rebooted KVM, and how a phoned-in
	// number resolves to a device), and connected to nobody until a
	// technician deliberately dials.
	CecHelpNetworkID = "cecsupport-clients"
	// CecAskNetworkID is the asking room — the help queue itself
	// (allmystuff-cec-protocol::ASK_NETWORK_ID). Joined only while the hand
	// is up; membership is the entire signal.
	CecAskNetworkID = "cecsupport-asking"
	// CecChannelControl carries the point-to-point connect handshake
	// (allmystuff-cec-protocol::CHANNEL_CONTROL) — a technician's connect
	// Request and our Approve reply.
	CecChannelControl = "cec.control"
	// cecScopeThreeHours is the ApprovalScope this device grants
	// (allmystuff-cec-protocol::ApprovalScope::ThreeHours, serialised
	// internally-tagged as {"kind":"three_hours"}).
	cecScopeThreeHours = "three_hours"
	// cecGrantWindow is how long an answered hand-raise authorises a technician
	// for. An appliance has nobody standing at it to tap "that's enough", so the
	// authorisation has to end by itself — this is the whole reason the grant is
	// time-boxed rather than the Forever an unattended device would otherwise be
	// stuck with.
	cecGrantWindow = 3 * time.Hour
	// cecGrantSweep is how often expired grants are swept. Access is refused the
	// moment a grant lapses (every check is deadline-aware), so this only
	// governs how quickly an already-open stream is actually torn down.
	cecGrantSweep = 30 * time.Second
)

// helpState tracks whether this device currently has its hand up (asking-room
// membership) and the per-run area bookkeeping. Guarded by its own mutex so a
// raise/lower never contends with the bridge's membership or presence locks.
type helpState struct {
	mu     sync.Mutex
	asking bool
	joined bool // whether we've joined the support area this run (idempotent join)
	// migrated notes the once-per-run area re-create that heals a room a
	// beacon-era build persisted as `open` (the network kind is governed
	// state bootstrapped at first attach — a config update can't flip it, so
	// the room is re-created silent, which also purges its leaked roster).
	migrated bool
}

// RaiseHand puts this device's hand up: it takes residence on the (Silent)
// support area (once), then **joins the asking room** — whose signaling
// membership is the entire "I need help" signal every watching technician
// reads. No beacons, no wires: the engine's own room announce carries the
// hand. Idempotent — a raise while already asking is a no-op.
func (b *Bridge) RaiseHand() error {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()

	if b.help.asking {
		return nil
	}
	if !b.help.joined {
		if err := b.ensureSilentHelpAreaLocked(); err != nil {
			return fmt.Errorf("join help mesh: %w", err)
		}
		// Become a full participant so a technician who answers can actually
		// reach us. joinPlanes subscribes the AllMyStuff control/media planes —
		// CEC screen+input ride those, not cec.media — and advertises our
		// capabilities; then we also subscribe the CEC control channel so the
		// connect-request itself arrives.
		if err := b.joinPlanes(CecHelpNetworkID); err != nil {
			return fmt.Errorf("join help planes: %w", err)
		}
		if err := b.subscribeCecControl(); err != nil {
			return fmt.Errorf("subscribe cec control: %w", err)
		}
		b.help.joined = true
	}
	// Join the asking room before committing to "asking", so a dead daemon
	// surfaces as an error to the caller instead of a silently-raised hand.
	if err := b.networkAdd(cecAskNetworkConfig()); err != nil {
		return fmt.Errorf("join asking room: %w", err)
	}

	b.help.asking = true
	log.Infof("mesh: CEC hand raised (support id %s)", b.SupportID())
	return nil
}

// ensureSilentHelpAreaLocked joins the standing support area, healing a room
// persisted by a beacon-era build on the way: those rooms were created `open`
// (the daemon auto-dialed every co-present peer to carry beacons), and the
// network kind is governed state a config update can't flip — so once per
// run, the room is re-created, which bootstraps it Silent and purges the
// roster of strangers the open era gossiped in. Callers hold b.help.mu.
func (b *Bridge) ensureSilentHelpAreaLocked() error {
	if !b.help.migrated {
		// Best-effort: removing a room the daemon doesn't carry is success
		// on the daemon side, and a remove failure only means the add below
		// joins the room as-is (the old behavior, healed next run).
		if err := b.networkRemove(CecHelpNetworkID); err != nil {
			log.Debugf("mesh: CEC area pre-remove (migration): %s", err)
		}
		b.help.migrated = true
	}
	return b.networkAdd(cecHelpNetworkConfig())
}

// LowerHand takes this device's hand down: it **leaves the asking room**,
// which removes it from every watching technician's queue at once (the
// daemon broadcasts a signaling Leave; a crash instead ages out with the
// room's presence). Idempotent — lowering an already-down hand is a no-op.
// We do NOT leave the support area; matching AllMyStuff, the node stays a
// resident so reconnects and the next raise are instant.
func (b *Bridge) LowerHand() error {
	b.help.mu.Lock()
	if !b.help.asking {
		b.help.mu.Unlock()
		return nil
	}
	b.help.asking = false
	b.help.mu.Unlock()

	if err := b.networkRemove(CecAskNetworkID); err != nil {
		return fmt.Errorf("leave asking room: %w", err)
	}
	log.Infof("mesh: CEC hand lowered")
	return nil
}

// ToggleHand raises the hand if it's down and lowers it if it's up, returning
// the new raised state. This is the one-shot the physical user button and the
// web UI both drive.
func (b *Bridge) ToggleHand() (raised bool, err error) {
	if b.HelpAsking() {
		return false, b.LowerHand()
	}
	return true, b.RaiseHand()
}

// HelpAsking reports whether this device currently has its hand up.
func (b *Bridge) HelpAsking() bool {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	return b.help.asking
}

// SupportID is this device's 9-digit CEC support number (derived from the
// daemon device id) — the phone-readable fallback a customer reads out when the
// queue is crowded. Empty until the bridge has a node id.
func (b *Bridge) SupportID() string {
	b.mu.Lock()
	nodeID := b.nodeID
	b.mu.Unlock()
	if nodeID == "" {
		return ""
	}
	return supportIDFromDevice(nodeID)
}

// channelSendTo sends a typed-channel frame point-to-point to one peer.
func (b *Bridge) channelSendTo(network, channel, peer string, payload interface{}) error {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl == nil {
		return fmt.Errorf("channel_send_to: bridge not connected")
	}
	return ctl.ChannelSendTo(network, channel, peer, payload)
}

// subscribeCecControl subscribes our event stream to the CEC control channel on
// the help mesh, so a technician's connect Request is delivered to us.
func (b *Bridge) subscribeCecControl() error {
	b.mu.Lock()
	ctl, events := b.ctl, b.events
	b.mu.Unlock()
	if ctl == nil || events == nil {
		return fmt.Errorf("cec: subscribe before connect")
	}
	return ctl.ChannelSubscribe(events.ClientID(), CecHelpNetworkID, CecChannelControl)
}

// cecConnect is the flat view of a ControlMessage::Connect(ConnectControl) frame
// on cec.control. The Rust enums are internally tagged (outer "t", inner
// "kind"), so both tags plus the union of fields land in one flat struct.
type cecConnect struct {
	T         string `json:"t"`
	Kind      string `json:"kind"`
	SessionID string `json:"session_id"`
}

// handleCecControl processes an inbound cec.control frame. A KVM is an
// unattended help-seeker: when a technician answers our raised hand with a
// connect Request, we auto-approve (there's no human here to tap "approve") and
// remember the technician so their screen/input routes are accepted.
func (b *Bridge) handleCecControl(network, from string, payload []byte) {
	var m cecConnect
	if err := json.Unmarshal(payload, &m); err != nil {
		log.Debugf("mesh: bad CEC control from %s: %s", pubkeyPart(from), err)
		return
	}
	if m.T != "connect" {
		return
	}
	switch m.Kind {
	case "request":
		// Auto-approve — there's no human here to tap "approve". A NEW technician
		// is admitted only while we're actually asking for help, so a KVM that
		// isn't requesting help can't be driven off the open support mesh. An
		// already-approved technician is always re-acked: its Request retransmits
		// until the data channel is up, and each beat is our cue to re-send the
		// (possibly dropped) Approve. The technician ignores the scope.
		admit, lower := b.cecAdmit(from)
		if !admit {
			log.Infof("mesh: CEC connect-request from %s ignored (not asking for help)", pubkeyPart(from))
			return
		}
		if err := b.channelSendTo(network, CecChannelControl, from, cecApprovePayload(m.SessionID)); err != nil {
			log.Warnf("mesh: CEC auto-approve to %s failed: %s", pubkeyPart(from), err)
			return
		}
		log.Infof("mesh: CEC auto-approved technician %s (session %s)", pubkeyPart(from), m.SessionID)
		if lower {
			// Help has arrived — drop out of the queue (available:false),
			// matching the CEC customer flow.
			go func() { _ = b.LowerHand() }()
		}
	case "end":
		b.unapproveTech(from)
		log.Infof("mesh: CEC session ended by technician %s", pubkeyPart(from))
	}
}

// cecApprovePayload builds a ConnectControl::Approve frame for session_id. The
// shape mirrors the internally-tagged Rust wire form exactly:
// {"t":"connect","kind":"approve","session_id":…,"scope":{"kind":"three_hours"}}.
//
// The scope is what the device actually enforces (see cecAdmit): the technician
// side treats an Approve as "session active" whatever the scope says, so the
// deadline is ours to keep, not theirs to honour. Sending ThreeHours rather than
// Forever keeps the wire honest about the grant we've really made.
func cecApprovePayload(sessionID string) map[string]interface{} {
	return map[string]interface{}{
		"t":          "connect",
		"kind":       "approve",
		"session_id": sessionID,
		"scope":      map[string]interface{}{"kind": cecScopeThreeHours},
	}
}

// cecAdmit decides whether to auto-approve a connect Request from `from`, and
// records a cecGrantWindow authorisation when it does.
//
// A technician holding a live grant is always re-admitted (their Request
// retransmits until the data channel is up, and each beat needs an ack) — but
// re-admission does NOT extend the deadline, or a technician who stayed
// connected would hold the device forever by simply not disconnecting. A new
// technician is admitted only while we're actually asking for help, so an idle
// KVM can't be driven off the open support mesh; `lower` is true exactly for
// that first admission — the cue to drop our raised hand.
func (b *Bridge) cecAdmit(from string) (admit, lower bool) {
	key := pubkeyPart(from)
	if _, held := b.state.CecTechExpiry(key); held {
		return true, false
	}
	b.help.mu.Lock()
	asking := b.help.asking
	b.help.mu.Unlock()
	if !asking {
		return false, false
	}
	b.state.GrantCecTech(key, cecGrantWindow)
	log.Infof("mesh: CEC authorised technician %s for %s", key, cecGrantWindow)
	return true, true
}

// unapproveTech forgets a technician when their session ends.
func (b *Bridge) unapproveTech(from string) {
	b.state.RevokeCecTech(pubkeyPart(from))
}

// cecApprovedTech reports whether `from` holds a live CEC authorisation. Every
// caller reaches senderMayControl through this, so a lapsed grant stops being
// obeyed the instant it lapses — no sweep needed for the refusal, only for
// tearing down what's already open.
func (b *Bridge) cecApprovedTech(from string) bool {
	_, held := b.state.CecTechExpiry(pubkeyPart(from))
	return held
}

// cecGrantJanitor sweeps expired authorisations and evicts whatever the lapsed
// technician still holds open.
//
// The refusal itself is already automatic — cecApprovedTech is deadline-aware,
// so control messages, new route offers and (checked per event) input injection
// all stop the moment the window closes. What needs doing here is the session
// already in flight: a display route accepted while the grant was live keeps its
// pump running, and a site tunnel keeps its connections, until something tears
// them down. That something is this.
func (b *Bridge) cecGrantJanitor(stop <-chan struct{}) {
	ticker := time.NewTicker(cecGrantSweep)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			for _, key := range b.state.PruneCecGrants(now) {
				log.Infof("mesh: CEC authorisation for %s expired — ending its access", key)
				b.evictTech(key)
			}
		}
	}
}

// evictTech tears down everything a no-longer-authorised technician holds: the
// display pump streaming this KVM's screen, the input route injecting its
// keyboard and mouse, and any tunneled web-UI connections.
//
// A peer that still passes senderMayControl on its own account — the device's
// owner, or a fleet co-member — is left alone. Their authority never came from
// the CEC grant, so its lapsing says nothing about it, and tearing down a
// session they hold independently would be a straightforward bug. Checked
// before b.mu is taken, because senderMayControl takes it too. The unclaim path
// is unaffected: it evicts after Unclaim() has cleared the owner, so nobody
// passes the check and everybody goes.
func (b *Bridge) evictTech(key string) {
	if b.senderMayControl(key) {
		return
	}
	b.mu.Lock()
	var stopDisplay *displaySession
	if b.display != nil && pubkeyPart(b.display.peer) == key {
		stopDisplay = b.display
		b.display = nil
		// Hand the lane back, as every other teardown path does. Leaking one
		// per eviction would exhaust maxVideoLanes after a few expiries and
		// leave the device rejecting display offers until it was rebooted.
		b.freeLaneLocked(stopDisplay.lane)
	}
	if b.inputPeer != "" && pubkeyPart(b.inputPeer) == key {
		b.inputRoute = ""
		b.inputPeer = ""
	}
	b.mu.Unlock()
	if stopDisplay != nil {
		close(stopDisplay.cancel)
	}
	if b.sites != nil {
		b.sites.tearDownPeer(key)
	}
}

// cecHelpNetworkConfig builds the daemon network config for the standing CEC
// support area. Mirrors AllMyStuff node/src/cec.rs help_network_config: a
// **Silent** network — signaling-only presence, no auto-dial, no roster
// gossip, no topology (there are no connections to shape). `auto_approve`
// keeps the mesh-level handshake unattended when a technician deliberately
// dials this device; access stays gated by the KVM's own time-boxed CEC
// grants (cecAdmit).
func cecHelpNetworkConfig() map[string]interface{} {
	return map[string]interface{}{
		"id":           CecHelpNetworkID,
		"network_id":   CecHelpNetworkID,
		"label":        "CEC Support",
		"kind":         "silent",
		"auto_approve": true,
		"signaling":    map[string]interface{}{"strategy": "nostr", "mdns": true},
	}
}

// cecAskNetworkConfig builds the daemon network config for the asking room —
// the help queue itself. Mirrors AllMyStuff node/src/cec.rs
// ask_network_config: Silent like the area, joined only while the hand is up;
// membership is the entire signal.
func cecAskNetworkConfig() map[string]interface{} {
	return map[string]interface{}{
		"id":           CecAskNetworkID,
		"network_id":   CecAskNetworkID,
		"label":        "CEC Support — asking",
		"kind":         "silent",
		"auto_approve": true,
		"signaling":    map[string]interface{}{"strategy": "nostr", "mdns": true},
	}
}

// supportIDFromDevice derives the 9-digit CEC support number for a device id,
// tolerating either the bare or the display-suffixed form (both yield the same
// number). Ports allmystuff-cec-protocol ids.rs support_id_from_device: strip
// any trailing -XXXXX display suffix (via pubkeyPart, the KVM's device_pubkey),
// SHA-256, take the first 8 bytes big-endian, reduce mod 1e9, zero-pad to 9.
func supportIDFromDevice(deviceID string) string {
	return supportIDFromString(pubkeyPart(deviceID))
}

func supportIDFromString(input string) string {
	digest := sha256.Sum256([]byte(input))
	var acc uint64
	for _, b := range digest[:8] {
		acc = (acc << 8) | uint64(b)
	}
	return fmt.Sprintf("%09d", acc%1_000_000_000)
}
