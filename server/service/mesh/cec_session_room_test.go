package mesh

import (
	"testing"

	"NanoKVM-Server/config"
)

// cecBridge is a bridge wired to the fake daemon with a FIXED identity, so the
// support number — and therefore the session room id a technician derives
// independently — is deterministic.
func cecBridge(t *testing.T, f *fakeDaemon) *Bridge {
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
	if err := events.Subscribe(nil, nil, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	return &Bridge{
		conf:   &config.Config{},
		mesh:   config.Mesh{NetworkId: "cec-backend-client-mesh"},
		state:  LoadState(t.TempDir()),
		events: events,
		ctl:    ctl,
		nodeID: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd",
	}
}

// joinedNetworks is every network the daemon was asked to join, by id.
func joinedNetworks(f *fakeDaemon) map[string]bool {
	joined := map[string]bool{}
	for _, req := range f.requests("network_add") {
		if cfg, ok := req["config"].(map[string]interface{}); ok {
			if id, _ := cfg["network_id"].(string); id != "" {
				joined[id] = true
			}
		}
	}
	return joined
}

// subscribedChannels maps each network to the channels subscribed on it.
func subscribedChannels(f *fakeDaemon) map[string]map[string]bool {
	subs := map[string]map[string]bool{}
	for _, req := range f.requests("channel_subscribe") {
		net, _ := req["network"].(string)
		ch, _ := req["channel"].(string)
		if subs[net] == nil {
			subs[net] = map[string]bool{}
		}
		subs[net][ch] = true
	}
	return subs
}

// advertisedTags returns the capability tags advertised on one network, and
// whether any advert was made there at all.
func advertisedTags(f *fakeDaemon, network string) ([]string, bool) {
	var tags []string
	found := false
	for _, req := range f.requests("capabilities_set") {
		if net, _ := req["network"].(string); net != network {
			continue
		}
		found = true
		caps, _ := req["capabilities"].(map[string]interface{})
		raw, _ := caps["tags"].([]interface{})
		tags = tags[:0]
		for _, tag := range raw {
			if s, _ := tag.(string); s != "" {
				tags = append(tags, s)
			}
		}
	}
	return tags, found
}

// The bug this pins: a technician answering a raised hand dials the customer's
// PRIVATE room (allmystuff-cec-protocol network_id_for_device, "cec-" + the
// support number) and sends the connect Request there. The KVM used to join
// only the discovery area and the asking room, so that Request was delivered to
// nobody, cecAdmit never ran, and the technician's session sat at "requested"
// forever — first attempt and every attempt.
func TestRaiseHandJoinsThePrivateSessionRoom(t *testing.T) {
	f := startFakeDaemon(t)
	b := cecBridge(t, f)

	want := b.cecSessionNetworkID()
	if want == "" || want == cecNetworkPrefix {
		t.Fatalf("session room id = %q, want cec-<9 digits>", want)
	}
	if len(want) != len(cecNetworkPrefix)+9 {
		t.Fatalf("session room id = %q, want %d chars", want, len(cecNetworkPrefix)+9)
	}

	if err := b.RaiseHand(); err != nil {
		t.Fatalf("RaiseHand: %v", err)
	}

	// The room must be joined...
	joined := joinedNetworks(f)
	if !joined[want] {
		t.Fatalf("private session room %s was never joined; joined %v", want, joined)
	}
	if !joined[CecAskNetworkID] {
		t.Errorf("asking room %s was not joined", CecAskNetworkID)
	}

	// ...and cec.control subscribed ON it, or the Request still reaches nobody.
	if !subscribedChannels(f)[want][CecChannelControl] {
		t.Fatalf("cec.control not subscribed on %s; subscribed %v", want, subscribedChannels(f))
	}
}

// The room id must match allmystuff-cec-protocol exactly — a technician derives
// it independently from the number read over the phone, so a one-character
// difference is a room nobody shares.
func TestSessionRoomIDMatchesTheProtocolDerivation(t *testing.T) {
	b := &Bridge{nodeID: "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"}
	number := b.SupportID()
	if got, want := b.cecSessionNetworkID(), cecNetworkPrefix+number; got != want {
		t.Fatalf("session room = %q, want %q", got, want)
	}
	// Display-suffixed device ids yield the same number, so the same room.
	suffixed := &Bridge{nodeID: b.nodeID + "-AB12C"}
	if got := suffixed.cecSessionNetworkID(); got != b.cecSessionNetworkID() {
		t.Fatalf("display suffix changed the room: %q vs %q", got, b.cecSessionNetworkID())
	}
}

// Before identity_show there is no number, and "cec-" is a room shared with
// every other un-identified device.
func TestSessionRoomIsEmptyBeforeIdentity(t *testing.T) {
	b := &Bridge{}
	if got := b.cecSessionNetworkID(); got != "" {
		t.Fatalf("session room = %q before identity, want empty", got)
	}
}

// The follow-on bug: the room was joined and cec.control subscribed, so the
// handshake completed — and then the technician's app refused to wire anything
// with "isn't running AllMyStuff". Capability adverts are PER NETWORK, and the
// first cut of the session-room join skipped joinPlanes, so the KVM sat in the
// room advertising nothing and read as a bare mesh endpoint.
//
// Being a full participant means all three: the AllMyStuff planes (CEC screen
// and input ride those, not cec.media), the capability advert, and cec.control.
func TestSessionRoomIsJoinedAsAFullParticipant(t *testing.T) {
	f := startFakeDaemon(t)
	b := cecBridge(t, f)
	room := b.cecSessionNetworkID()

	if err := b.RaiseHand(); err != nil {
		t.Fatalf("RaiseHand: %v", err)
	}

	// The capability advert — the thing the technician reads to decide we are
	// an AllMyStuff node at all.
	tags, advertised := advertisedTags(f, room)
	if !advertised {
		t.Fatalf("no capability advert on %s — the technician reads this KVM as a bare mesh endpoint", room)
	}
	found := false
	for _, tag := range tags {
		if tag == CapTagAllMyStuff {
			found = true
		}
	}
	if !found {
		t.Errorf("advert on %s carries tags %v, want one of them %q", room, tags, CapTagAllMyStuff)
	}

	// And the planes CEC screen/input actually ride.
	planes := subscribedChannels(f)[room]
	for _, ch := range []string{ChannelPresence, ChannelControl, ChannelMedia, CecChannelControl} {
		if !planes[ch] {
			t.Errorf("channel %q not subscribed on %s; got %v", ch, room, planes)
		}
	}
}

// Residency is not a raised hand. A technician's dial is pinned, so their
// daemon keeps redialing this KVM for the whole 3-hour grant — across the
// reboot an update causes. The grant survives that (it is persisted, and
// cecAdmit re-admits with no human); the transport did not, because the rooms
// were only ever joined by RaiseHand and a rebooted KVM comes back with its
// hand down. Dial-by-number had the same hole: it resolves digits against the
// support area's membership, so a rebooted KVM was un-diallable until someone
// physically pressed its button — the one thing a remote customer cannot do.
func TestCecOnlineTakesResidenceWithTheHandDown(t *testing.T) {
	f := startFakeDaemon(t)
	b := cecBridge(t, f)
	room := b.cecSessionNetworkID()

	if err := b.CecOnline(); err != nil {
		t.Fatalf("CecOnline: %v", err)
	}

	joined := joinedNetworks(f)
	if !joined[CecHelpNetworkID] {
		t.Errorf("support area %s not joined at bring-up; joined %v", CecHelpNetworkID, joined)
	}
	if !joined[room] {
		t.Errorf("session room %s not joined at bring-up; joined %v", room, joined)
	}
	// ...but the hand stays DOWN. Joining the asking room is the entire "I
	// need help" signal every watching technician reads; taking residence
	// must never put this KVM in the queue.
	if joined[CecAskNetworkID] {
		t.Error("bring-up joined the asking room — that raises the hand on every boot")
	}
	if b.HelpAsking() {
		t.Error("bring-up left the hand up")
	}

	// A technician who reconnects on a live grant needs the same full
	// participation a raised hand gets: the handshake channel, the planes,
	// and the advert that stops their app calling us a bare mesh endpoint.
	planes := subscribedChannels(f)[room]
	for _, ch := range []string{ChannelPresence, ChannelControl, ChannelMedia, CecChannelControl} {
		if !planes[ch] {
			t.Errorf("channel %q not subscribed on %s at bring-up; got %v", ch, room, planes)
		}
	}
	if _, advertised := advertisedTags(f, room); !advertised {
		t.Errorf("no capability advert on %s at bring-up", room)
	}
}

// `joined` means "on THIS daemon connection", not "ever": every subscribe above
// names the current event stream's client id, which a reconnect replaces. Left
// latched, a bridge that reconnected would hold residence the daemon has no
// subscription for — the rooms look joined and the connect Request goes
// nowhere, which is the original bug wearing a different hat.
func TestCecResidenceIsRedoneOnEachDaemonConnection(t *testing.T) {
	f := startFakeDaemon(t)
	b := cecBridge(t, f)
	room := b.cecSessionNetworkID()

	cecControlSubscribes := func() int {
		n := 0
		for _, req := range f.requests("channel_subscribe") {
			net, _ := req["network"].(string)
			ch, _ := req["channel"].(string)
			if net == room && ch == CecChannelControl {
				n++
			}
		}
		return n
	}

	if err := b.CecOnline(); err != nil {
		t.Fatalf("CecOnline: %v", err)
	}
	if got := cecControlSubscribes(); got != 1 {
		t.Fatalf("first CecOnline subscribed cec.control %d times, want 1", got)
	}

	// Same connection: cheap no-op, so a hand raised right after bring-up
	// doesn't re-attach everything.
	if err := b.CecOnline(); err != nil {
		t.Fatalf("second CecOnline: %v", err)
	}
	if got := cecControlSubscribes(); got != 1 {
		t.Fatalf("CecOnline repeated the work on the same connection (%d subscribes)", got)
	}

	// A reconnect, though, must redo all of it.
	b.resetHelpRun()
	if err := b.CecOnline(); err != nil {
		t.Fatalf("CecOnline after reconnect: %v", err)
	}
	if got := cecControlSubscribes(); got != 2 {
		t.Fatalf("cec.control subscribed %d times across a reconnect, want 2 — "+
			"the new event stream has no subscription", got)
	}
}

// An unclaimed device sheds every mesh it doesn't recognise, which is what
// makes an interrupted unclaim converge. The CEC rooms are not claim state:
// they are how a customer reaches a technician, which an unclaimed device needs
// at least as much as a claimed one. Shedding them would fight CecOnline in a
// remove/re-add loop on every reconnect — and networkRemove purges, so it would
// drop the session room's pins under a technician mid-session.
func TestUnclaimedShedKeepsTheCecRooms(t *testing.T) {
	f := startFakeDaemon(t)
	b := connectedBridge(t, f)
	b.nodeID = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	room := b.cecSessionNetworkID()
	b.help.asking = true // hand up, so the asking room is not stale
	f.respondWith("networks_list", networksListLine(
		b.joiningMeshID(), CecHelpNetworkID, CecAskNetworkID, room, "den-site-mesh"))

	if err := b.ensureMemberships(); err != nil {
		t.Fatalf("ensureMemberships: %v", err)
	}

	removed := map[string]bool{}
	for _, req := range f.requests("network_remove") {
		id, _ := req["network"].(string)
		removed[id] = true
	}
	if !removed["den-site-mesh"] {
		t.Errorf("a genuinely stale mesh was kept: %v", removed)
	}
	for _, id := range []string{CecHelpNetworkID, CecAskNetworkID, room} {
		if removed[id] {
			t.Errorf("CEC room %s was shed as unclaimed leftovers", id)
		}
	}
}

// The wiring itself: connectAndRun must take CEC residence on every daemon
// connection. The rooms being joinable is half the fix — the half that broke
// support for a rebooted KVM was that nothing joined them until a human pressed
// the button.
func TestBringUpTakesCecResidence(t *testing.T) {
	f := startFakeDaemon(t)
	const device = "abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"
	f.respondWith("identity_show",
		`{"ok":true,"data":{"device_id":"`+device+`","pubkey":"","label":"CEC KVM"}}`)
	f.respondWith("networks_list", networksListLine())

	meshConf := config.Mesh{Name: "CEC-KVM", Socket: f.sock, Home: t.TempDir()}
	b := &Bridge{
		conf:  &config.Config{Mesh: meshConf},
		mesh:  meshConf,
		state: LoadState(t.TempDir()),
	}
	b.sites = newSiteHost(nil, 80, b.sendSiteFrame)

	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = b.connectAndRun(stop)
	}()
	room := (&Bridge{nodeID: device}).cecSessionNetworkID()
	waitFor(t, "the CEC session room to be joined at bring-up", func() bool {
		return joinedNetworks(f)[room]
	})
	waitFor(t, "cec.control to be subscribed on the session room", func() bool {
		return subscribedChannels(f)[room][CecChannelControl]
	})
	close(stop)
	<-done

	if joinedNetworks(f)[CecAskNetworkID] {
		t.Error("bring-up joined the asking room — every boot would raise the hand")
	}
}
