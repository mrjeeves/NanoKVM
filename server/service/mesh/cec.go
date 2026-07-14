package mesh

// CEC "hand raise" (Ask-for-help) support.
//
// A KVM raises its hand exactly the way a CEC Support customer does: it joins
// the one well-known help area (`cecsupport-clients`) and broadcasts a
// SupportPresence beacon on the `cec.presence` channel with available:true,
// re-beaconing until the hand is lowered. Technicians watching the area collect
// the beacons into a longest-waiting-first queue. The beacon carries *want*,
// never access — a technician still connects deliberately, and the KVM's own
// consent/route gating (control.go, native.go) guards any actual session.
//
// This is a NEW, additive plane: the KVM's normal presence lives on the
// AllMyStuff graph (allmystuff-cloud-mesh-v1, see protocol.go), whereas a hand
// raise rides the CEC plane. The two never mix. Everything here mirrors
// AllMyStuff's node/src/mesh.rs (cec_ask_help / cec_broadcast_presence),
// node/src/cec.rs (help_network_config), and the wire contract in
// crates/allmystuff-cec-protocol (wire.rs SupportPresence, ids.rs support_id).

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	// CecHelpNetworkID is the single well-known help mesh every CEC client
	// shares (allmystuff-cec-protocol::HELP_NETWORK_ID). A raiser joins it and
	// beacons there; the beacon is the whole signal.
	CecHelpNetworkID = "cecsupport-clients"
	// CecChannelPresence carries SupportPresence beacons
	// (allmystuff-cec-protocol::CHANNEL_PRESENCE).
	CecChannelPresence = "cec.presence"
	// CecChannelControl carries the point-to-point connect handshake
	// (allmystuff-cec-protocol::CHANNEL_CONTROL) — a technician's connect
	// Request and our Approve reply.
	CecChannelControl = "cec.control"
	// CecRoleClient marks this node as a help-seeker (customer), not a
	// technician (Role::Client in the wire enum).
	CecRoleClient = "client"
	// cecProtocolVersion is the CEC wire-protocol version
	// (allmystuff-cec-protocol::PROTOCOL_VERSION). Distinct from the AllMyStuff
	// graph's ProtocolVersion even though both are currently 1.
	cecProtocolVersion = 1
	// cecHelpBeaconInterval is the steady re-beacon cadence while a hand is up
	// (allmystuff-cec-protocol::HELP_BEACON_SECS). Technicians age a silent
	// beacon out after HELP_TTL_SECS (90s), so this tolerates a missed beat.
	cecHelpBeaconInterval = 20 * time.Second
)

// cecHelpWarmup is the front-loaded beacon schedule after a raise: absolute
// offsets t=2,3,5,10s (then cecHelpBeaconInterval thereafter), so a technician
// sees a freshly raised hand quickly instead of waiting a full 20s. Mirrors the
// warm-up loop in AllMyStuff node/src/mesh.rs cec_ask_help.
var cecHelpWarmup = []time.Duration{
	2 * time.Second,
	3 * time.Second,
	5 * time.Second,
	10 * time.Second,
}

// SupportPresence is the CEC help beacon. Field names/json tags mirror
// allmystuff-cec-protocol/src/wire.rs exactly — the daemon relays this as an
// opaque payload and a technician's app decodes it. `protocol` and `available`
// are always sent (they have serde defaults on the Rust side, but being
// explicit costs nothing and keeps intent clear).
type SupportPresence struct {
	Protocol   uint32 `json:"protocol"`
	DeviceID   string `json:"device_id"`
	SupportID  string `json:"support_id"`
	Label      string `json:"label"`
	Role       string `json:"role"`
	Available  bool   `json:"available"`
	AppVersion string `json:"app_version"`
	OS         string `json:"os"`
	Hostname   string `json:"hostname"`
	Boot       uint64 `json:"boot"`
	SentAt     uint64 `json:"sent_at"`
}

// helpState tracks whether this device currently has its hand up and owns the
// re-beacon goroutine. Guarded by its own mutex so a raise/lower never contends
// with the bridge's membership or presence locks.
type helpState struct {
	mu     sync.Mutex
	asking bool
	joined bool          // whether we've joined the help network this run (idempotent join)
	cancel chan struct{} // closed to stop the beacon loop
	done   chan struct{} // closed when the beacon loop has exited
	// techs is the set of canonical technician pubkeys we've auto-approved on
	// the CEC control channel. A member passes senderMayControl, so an approved
	// technician's screen/input routes are accepted — the whole point of
	// answering a raised hand. Populated when we approve a connect-request.
	techs map[string]struct{}
}

// RaiseHand puts this device's hand up on the CEC help mesh: it joins the help
// area (once), sends an immediate available:true beacon, and starts the
// re-beacon loop. Idempotent — a raise while already asking is a no-op.
func (b *Bridge) RaiseHand() error {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()

	if b.help.asking {
		return nil
	}
	if !b.help.joined {
		if err := b.networkAdd(cecHelpNetworkConfig()); err != nil {
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
	// Send the first beacon before we commit to "asking" so a dead daemon
	// surfaces as an error to the caller instead of a silently-raised hand.
	if err := b.sendHelpBeacon(true); err != nil {
		return fmt.Errorf("beacon: %w", err)
	}

	b.help.asking = true
	b.help.cancel = make(chan struct{})
	b.help.done = make(chan struct{})
	go b.helpBeaconLoop(b.help.cancel, b.help.done)

	log.Infof("mesh: CEC hand raised (support id %s)", b.SupportID())
	return nil
}

// LowerHand takes this device's hand down: it stops the re-beacon loop and
// sends one available:false beacon so the technician queue drops it promptly
// (rather than waiting out the 90s TTL). Idempotent — lowering an already-down
// hand is a no-op. We do NOT leave the help mesh; matching AllMyStuff, the node
// stays a member so the next raise is instant.
func (b *Bridge) LowerHand() error {
	b.help.mu.Lock()
	if !b.help.asking {
		b.help.mu.Unlock()
		return nil
	}
	cancel, done := b.help.cancel, b.help.done
	b.help.asking = false
	b.help.cancel = nil
	b.help.done = nil
	b.help.mu.Unlock()

	// Stop the loop first so it can't race a stray available:true beacon in
	// after our hand-down.
	if cancel != nil {
		close(cancel)
	}
	if done != nil {
		<-done
	}
	if err := b.sendHelpBeacon(false); err != nil {
		return fmt.Errorf("beacon down: %w", err)
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

// helpBeaconLoop re-broadcasts the raised beacon on the warm-up schedule and
// then every cecHelpBeaconInterval until cancel is closed.
func (b *Bridge) helpBeaconLoop(cancel, done chan struct{}) {
	defer close(done)

	var elapsed time.Duration
	for _, at := range cecHelpWarmup {
		select {
		case <-cancel:
			return
		case <-time.After(at - elapsed):
		}
		elapsed = at
		if err := b.sendHelpBeacon(true); err != nil {
			log.Debugf("mesh: CEC re-beacon failed: %s", err)
		}
	}

	ticker := time.NewTicker(cecHelpBeaconInterval)
	defer ticker.Stop()
	for {
		select {
		case <-cancel:
			return
		case <-ticker.C:
			if err := b.sendHelpBeacon(true); err != nil {
				log.Debugf("mesh: CEC re-beacon failed: %s", err)
			}
		}
	}
}

// sendHelpBeacon broadcasts one SupportPresence on the help mesh.
func (b *Bridge) sendHelpBeacon(available bool) error {
	return b.channelSendAll(CecHelpNetworkID, CecChannelPresence, b.buildSupportPresence(available))
}

// buildSupportPresence assembles the beacon from the live device identity.
func (b *Bridge) buildSupportPresence(available bool) SupportPresence {
	b.mu.Lock()
	nodeID := b.nodeID
	b.mu.Unlock()

	profile := b.currentProfile()
	return SupportPresence{
		Protocol:   cecProtocolVersion,
		DeviceID:   nodeID,
		SupportID:  supportIDFromDevice(nodeID),
		Label:      profile.Label,
		Role:       CecRoleClient,
		Available:  available,
		AppVersion: profile.Version,
		OS:         "linux",
		Hostname:   b.dev.hostname,
		Boot:       b.boot,
		SentAt:     uint64(time.Now().Unix()),
	}
}

// channelSendAll broadcasts a typed-channel frame via the daemon control
// socket, guarding on a live connection the same way networkAdd does.
func (b *Bridge) channelSendAll(network, channel string, payload interface{}) error {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl == nil {
		return fmt.Errorf("channel_send_all: bridge not connected")
	}
	return ctl.ChannelSendAll(network, channel, payload)
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
		// Authorize the technician and reply Approve. The technician retransmits
		// its Request until it sees an answer and ignores the scope (it only
		// moves the session to active), so re-approving a repeat Request is a
		// harmless idempotent ack.
		b.approveTech(from)
		if err := b.channelSendTo(network, CecChannelControl, from, cecApprovePayload(m.SessionID)); err != nil {
			log.Warnf("mesh: CEC auto-approve to %s failed: %s", pubkeyPart(from), err)
			return
		}
		log.Infof("mesh: CEC auto-approved technician %s (session %s)", pubkeyPart(from), m.SessionID)
		// Help has arrived — drop out of the queue (available:false), matching
		// the CEC customer flow. Only when we're actually still asking, so a
		// Request retransmit doesn't spawn no-op lowers.
		if b.HelpAsking() {
			go func() { _ = b.LowerHand() }()
		}
	case "end":
		b.unapproveTech(from)
		log.Infof("mesh: CEC session ended by technician %s", pubkeyPart(from))
	}
}

// cecApprovePayload builds a ConnectControl::Approve frame for session_id. The
// shape mirrors the internally-tagged Rust wire form exactly:
// {"t":"connect","kind":"approve","session_id":...,"scope":{"kind":"forever"}}.
// The technician ignores the scope (it only flips the session to active), so
// Forever is a safe constant for an unattended appliance.
func cecApprovePayload(sessionID string) map[string]interface{} {
	return map[string]interface{}{
		"t":          "connect",
		"kind":       "approve",
		"session_id": sessionID,
		"scope":      map[string]interface{}{"kind": "forever"},
	}
}

// approveTech records a technician's canonical pubkey as authorized.
func (b *Bridge) approveTech(from string) {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	if b.help.techs == nil {
		b.help.techs = map[string]struct{}{}
	}
	b.help.techs[pubkeyPart(from)] = struct{}{}
}

// unapproveTech forgets a technician when their session ends.
func (b *Bridge) unapproveTech(from string) {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	delete(b.help.techs, pubkeyPart(from))
}

// cecApprovedTech reports whether `from` is a technician we auto-approved.
func (b *Bridge) cecApprovedTech(from string) bool {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	_, ok := b.help.techs[pubkeyPart(from)]
	return ok
}

// cecHelpNetworkConfig builds the daemon network config for the CEC help area.
// It mirrors AllMyStuff node/src/cec.rs help_network_config: an Open network
// (zero-config membership — the raiser's button is the whole join) with nostr +
// mDNS signaling, and, when CEC_HELP_HUBS is set, a hub topology so members
// connect only to CEC-operated hubs and beacons flood hub-ward to technicians
// (never a customer↔customer N² mesh). Unset leaves the daemon default.
func cecHelpNetworkConfig() map[string]interface{} {
	cfg := map[string]interface{}{
		"id":           CecHelpNetworkID,
		"network_id":   CecHelpNetworkID,
		"label":        "CEC Support",
		"kind":         "open",
		"auto_approve": true,
		"signaling":    map[string]interface{}{"strategy": "nostr", "mdns": true},
	}
	if topo := cecHelpHubTopology(os.Getenv("CEC_HELP_HUBS")); topo != nil {
		cfg["topology"] = topo
	}
	return cfg
}

// cecHelpHubTopology parses CEC_HELP_HUBS ("hub1,hub2[:redundancy]") into the
// daemon's hub topology JSON, or nil when unset/empty (caller then leaves the
// daemon default, a full mesh). Ports AllMyStuff cec.rs help_hub_topology: the
// last ':' separates an optional numeric spoke-redundancy; device pubkeys are
// base32 so they never contain a ':'.
func cecHelpHubTopology(spec string) map[string]interface{} {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	idsPart := spec
	redundancy := -1
	if i := strings.LastIndex(spec, ":"); i >= 0 {
		idsPart = spec[:i]
		if r, err := strconv.Atoi(strings.TrimSpace(spec[i+1:])); err == nil {
			redundancy = r
		}
	}
	var hubs []string
	for _, h := range strings.Split(idsPart, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hubs = append(hubs, h)
		}
	}
	if len(hubs) == 0 {
		return nil
	}
	topo := map[string]interface{}{"kind": "hubs", "hubs": hubs}
	if redundancy >= 0 {
		topo["spoke_redundancy"] = redundancy
	}
	return topo
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
