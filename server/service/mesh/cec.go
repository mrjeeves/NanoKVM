package mesh

// CEC support-number connections. The appliance stays discoverable and hosts
// its private session room. A new technician needs an explicit web/app approval
// or the one-request window armed by the physical button. Grants last 3 hours.

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
	// CecAskNetworkID is retained only to remove legacy queue membership.
	CecAskNetworkID = "cecsupport-asking"
	// cecNetworkPrefix prefixes a device's private support room
	// (allmystuff-cec-protocol CEC_NETWORK_PREFIX).
	cecNetworkPrefix = "cec-"
	// CecChannelControl carries the point-to-point connect handshake
	// (allmystuff-cec-protocol::CHANNEL_CONTROL) — a technician's connect
	// Request and our Approve reply.
	CecChannelControl = "cec.control"
	// cecScopeThreeHours is the ApprovalScope this device grants
	// (allmystuff-cec-protocol::ApprovalScope::ThreeHours, serialised
	// internally-tagged as {"kind":"three_hours"}).
	cecScopeThreeHours = "three_hours"
	// cecGrantWindow is how long explicit approval authorises a technician
	// for. An appliance has nobody standing at it to tap "that's enough", so the
	// authorisation has to end by itself — this is the whole reason the grant is
	// time-boxed rather than the Forever an unattended device would otherwise be
	// stuck with.
	cecGrantWindow = 3 * time.Hour
	// cecGrantSweep is how often expired grants are swept. Access is refused the
	// moment a grant lapses (every check is deadline-aware), so this only
	// governs how quickly an already-open stream is actually torn down.
	cecGrantSweep = 2 * time.Second
)

// helpState holds pending consent and per-connection support bookkeeping.
type helpState struct {
	mu         sync.Mutex
	pending    map[string]CecSupportRequest
	denied     map[string]cecDeniedRequest
	armedUntil time.Time
	// joined notes that CecOnline has taken residence on THIS daemon
	// connection (area + session room + their subscribes). Cleared by
	// resetHelpRun on every reconnect, because the subscribes name an event
	// stream client id the reconnect throws away.
	joined bool
	// migrated notes the once-per-run area re-create that heals a room a
	// beacon-era build persisted as `open` (the network kind is governed
	// state bootstrapped at first attach — a config update can't flip it, so
	// the room is re-created silent, which also purges its leaked roster).
	migrated bool
}

// CecOnline publishes this number and joins its private session room at
// bring-up and reconnect. This never authorizes a technician.
func (b *Bridge) CecOnline() error {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	return b.cecOnlineLocked()
}

// cecOnlineLocked is CecOnline's body. Callers hold b.help.mu.
//
// `joined` is per daemon connection, not per process: every subscribe here
// names the CURRENT event stream's client id, so a reconnect has to redo all
// of it — see resetHelpRun.
func (b *Bridge) cecOnlineLocked() error {
	if b.help.joined {
		return nil
	}
	if err := b.ensureSilentHelpAreaLocked(); err != nil {
		return fmt.Errorf("join help mesh: %w", err)
	}
	// Become a full participant so a technician who answers can actually reach
	// us. joinPlanes subscribes the AllMyStuff control/media planes — CEC
	// screen+input ride those, not cec.media — and advertises our capabilities;
	// then we also subscribe the CEC control channel so the connect-request
	// itself arrives.
	if err := b.joinPlanes(CecHelpNetworkID); err != nil {
		return fmt.Errorf("join help planes: %w", err)
	}
	if err := b.subscribeCecControl(); err != nil {
		return fmt.Errorf("subscribe cec control: %w", err)
	}
	// The directory is discovery only; number dials use this private room.
	if err := b.joinCecSessionRoomLocked(); err != nil {
		return err
	}
	b.help.joined = true
	log.Infof("mesh: CEC online — discoverable on %s, hosting %s (support id %s)",
		CecHelpNetworkID, b.cecSessionNetworkID(), b.SupportID())
	return nil
}

// resetHelpRun redoes subscriptions after a daemon connection changes.
func (b *Bridge) resetHelpRun() {
	b.help.mu.Lock()
	b.help.joined = false
	b.help.mu.Unlock()
}

// joinCecSessionRoomLocked joins this device's private support room and
// subscribes the CEC control channel on it. Callers hold b.help.mu.
//
// Both halves matter: being IN the room is what lets the technician's dial
// reach us at all, and the subscribe is what delivers the connect Request to
// this process once it does.
func (b *Bridge) joinCecSessionRoomLocked() error {
	id := b.cecSessionNetworkID()
	if id == "" {
		return fmt.Errorf("cec session room: no device identity yet")
	}
	if err := b.networkAdd(b.cecSessionNetworkConfig(id)); err != nil {
		return fmt.Errorf("join cec session room: %w", err)
	}
	// Become a FULL participant here, exactly as we do on the support area.
	// joinPlanes subscribes the AllMyStuff presence/control/media planes — CEC
	// screen and input ride those, not cec.media — and advertises our
	// capability tags on this network.
	//
	// Both halves are load-bearing and the advert is the one that bites first:
	// capabilities are per-network, so a KVM that joined this room without
	// advertising reads to the technician as a bare mesh endpoint, and their
	// app refuses to wire anything to it with "isn't running AllMyStuff" —
	// after the handshake has otherwise succeeded.
	if err := b.joinPlanes(id); err != nil {
		return fmt.Errorf("join cec session room planes: %w", err)
	}
	if err := b.subscribeCecControlOn(id); err != nil {
		return fmt.Errorf("subscribe cec control on session room: %w", err)
	}
	log.Infof("mesh: CEC session room %s joined (support id %s)", id, b.SupportID())
	return nil
}

// ensureSilentHelpAreaLocked joins the standing support area, healing a room
// persisted by a beacon-era build on the way: those rooms were created `open`
// (the daemon auto-dialed every co-present peer to carry beacons), and the
// network kind is governed state a config update can't flip — so a room whose
// governed kind still reads `open` is re-created, which bootstraps it Silent
// and purges the roster of strangers the open era gossiped in. Checked at
// most once per run; a room already silent (or an unreadable kind — never
// churn the room on a guess) is joined as-is. Callers hold b.help.mu.
//
// The kind check is what makes this safe to run at bring-up: an unconditional
// re-create would purge the area's roster on every boot, not just the one that
// heals a beacon-era room.
func (b *Bridge) ensureSilentHelpAreaLocked() error {
	if !b.help.migrated {
		b.help.migrated = true
		if kind := b.governanceKind(CecHelpNetworkID); kind == "open" {
			log.Infof("mesh: CEC area is governed open (the beacon era) — re-creating it silent")
			if err := b.networkRemove(CecHelpNetworkID); err != nil {
				log.Debugf("mesh: CEC area pre-remove (migration): %s", err)
			}
		}
	}
	return b.networkAdd(cecHelpNetworkConfig())
}

// governanceKind is the nil-ctl guard for governance_state; empty on any
// failure (an older daemon, or a network the daemon doesn't carry).
func (b *Bridge) governanceKind(networkID string) string {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl == nil {
		return ""
	}
	kind, err := ctl.GovernanceKind(networkID)
	if err != nil {
		return ""
	}
	return kind
}

// SupportID is this device's 9-digit CEC support number (derived from the
// daemon device id) — the number the customer shares with their technician. Empty until the bridge has a node id.
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
	return b.subscribeCecControlOn(CecHelpNetworkID)
}

// subscribeCecControlOn subscribes our event stream to cec.control on one
// network. The support area and the private session room both carry it: the
// area so a directory-era technician is not silently ignored, the room because
// that is where the handshake really happens.
func (b *Bridge) subscribeCecControlOn(networkID string) error {
	b.mu.Lock()
	ctl, events := b.ctl, b.events
	b.mu.Unlock()
	if ctl == nil || events == nil {
		return fmt.Errorf("cec: subscribe before connect")
	}
	return ctl.ChannelSubscribe(events.ClientID(), networkID, CecChannelControl)
}

// cecConnect is the flat view of a ControlMessage::Connect(ConnectControl) frame
// on cec.control. The Rust enums are internally tagged (outer "t", inner
// "kind"), so both tags plus the union of fields land in one flat struct.
type cecConnect struct {
	T           string `json:"t"`
	Kind        string `json:"kind"`
	SessionID   string `json:"session_id"`
	AgentName   string `json:"agent_name"`
	WantControl bool   `json:"want_control"`
}

// handleCecControl accepts requests only on this KVM's support transports.
func (b *Bridge) handleCecControl(network, from string, payload []byte) {
	room := b.cecSessionNetworkID()
	if network != CecHelpNetworkID && (room == "" || network != room) {
		return
	}
	var m cecConnect
	if json.Unmarshal(payload, &m) != nil || m.T != "connect" || m.SessionID == "" || len(m.SessionID) > 256 || from == "" {
		return
	}
	switch m.Kind {
	case "request":
		b.receiveCecRequest(network, from, m)
	case "end":
		b.cancelCecRequest(from, m.SessionID)
		b.unapproveTech(from)
		b.evictTech(pubkeyPart(from))
	}
}

// cecApprovePayload builds a ConnectControl::Approve frame for session_id. The
// shape mirrors the internally-tagged Rust wire form exactly:
// {"t":"connect","kind":"approve","session_id":…,"scope":{"kind":"three_hours"}}.
//
// The scope is what the device actually enforces (see support_requests.go): the technician
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
			for _, key := range b.pruneDelegatedTechs(now) {
				log.Infof("mesh: attached-computer delegation for %s expired — ending its KVM access", key)
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
// gossip or data links. Actual transports use the private session room;
// access requires the KVM's own time-limited consent grant.
func cecHelpNetworkConfig() map[string]interface{} {
	return map[string]interface{}{
		"id":           CecHelpNetworkID,
		"network_id":   CecHelpNetworkID,
		"label":        "CEC Support",
		"kind":         "silent",
		"auto_approve": false,
		"signaling":    map[string]interface{}{"strategy": "nostr", "mdns": true},
	}
}

// cecSessionNetworkID is this device's PRIVATE support room —
// allmystuff-cec-protocol's network_id_for_device: "cec-" + the 9-digit
// support number. The support area is discovery only; this is where the
// connect handshake and every session actually happens, which is why the
// technician's dial joins exactly this room.
//
// Empty before identity_show has run, since the number derives from the
// device id. Callers must treat that as "not ready yet" rather than joining
// a room called "cec-".
func (b *Bridge) cecSessionNetworkID() string {
	number := b.SupportID()
	if number == "" {
		return ""
	}
	// The Rust side runs the number through normalize_input first; a
	// SupportID is already nine ASCII digits, for which that is the identity.
	return cecNetworkPrefix + number
}

// cecSessionNetworkConfig builds the daemon config for that room. Same shape as
// node/src/cec.rs session_network_config: silent, so it
// is never gossiped, and auto_approve, because the technician's deliberate dial
// is what opens the transport — human access is still gated by explicit approval.
func (b *Bridge) cecSessionNetworkConfig(id string) map[string]interface{} {
	return map[string]interface{}{
		"id":           id,
		"network_id":   id,
		"label":        fmt.Sprintf("CEC Support %s", b.SupportID()),
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
