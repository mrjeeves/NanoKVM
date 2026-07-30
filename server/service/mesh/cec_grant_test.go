package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A grant handed out by answering a raised hand runs for cecGrantWindow and is
// written down, so the deadline outlives the process that set it.
func TestCecGrantIsTimeBoxedAndPersisted(t *testing.T) {
	home := t.TempDir()
	b := &Bridge{state: LoadState(home)}
	b.help.asking = true

	if admit, lower := b.cecAdmit("tech-pub-AB12C"); !admit || !lower {
		t.Fatalf("admit=%v lower=%v, want true/true", admit, lower)
	}
	at, held := b.state.CecTechExpiry("tech-pub")
	if !held {
		t.Fatal("an admitted technician should hold a grant")
	}
	// Bounded, and bounded at three hours — not the open-ended grant an
	// unattended appliance would otherwise hand out.
	left := time.Until(at)
	if left > cecGrantWindow || left < cecGrantWindow-time.Minute {
		t.Fatalf("grant runs for %s, want ~%s", left, cecGrantWindow)
	}

	// A fresh State over the same home is what a reboot produces.
	if _, held := LoadState(home).CecTechExpiry("tech-pub"); !held {
		t.Fatal("a live grant must survive a restart — a repair can span a reboot")
	}
}

// Re-admitting a technician (their Request retransmits until the data channel
// is up) must NOT push the deadline out, or staying connected would renew the
// authorisation indefinitely — exactly the unbounded access the window exists
// to prevent.
func TestCecReadmitDoesNotExtendTheWindow(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	b.help.asking = true

	if admit, _ := b.cecAdmit("tech-pub"); !admit {
		t.Fatal("first admit should succeed while asking")
	}
	first, _ := b.state.CecTechExpiry("tech-pub")

	for i := 0; i < 3; i++ {
		if admit, lower := b.cecAdmit("tech-pub"); !admit || lower {
			t.Fatalf("retransmit %d: admit=%v lower=%v, want true/false", i, admit, lower)
		}
	}
	if again, _ := b.state.CecTechExpiry("tech-pub"); !again.Equal(first) {
		t.Fatalf("deadline moved from %s to %s — re-admission must not renew", first, again)
	}
}

// An expired grant is refused, and refusing it is what stops the technician
// controlling the device — no sweep required for the decision itself.
func TestCecExpiredGrantIsRefused(t *testing.T) {
	home := t.TempDir()
	// A grant that ran out while the device was off. Written by hand because
	// GrantCecTech deliberately refuses to record a dead deadline. Minted on a
	// clock that was already set, so its deadline means what it says — a grant
	// minted before the clock was set is a different case (see the re-anchor
	// tests below), and using a zero mint time here would exercise that instead.
	raw, err := json.Marshal(persistedState{
		Claimable: false,
		Owner:     "owner-node",
		CecGrants: map[string]cecGrant{"tech-pub": {
			Granted: time.Now().Add(-4 * time.Hour).Unix(),
			Expires: time.Now().Add(-time.Minute).Unix(),
		}},
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := os.WriteFile(filepath.Join(home, stateFile), raw, 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}

	b := &Bridge{state: LoadState(home)}
	if _, held := b.state.CecTechExpiry("tech-pub"); held {
		t.Fatal("a grant whose deadline has passed must not read as held")
	}
	if b.cecApprovedTech("tech-pub") {
		t.Fatal("an expired technician must not be approved")
	}
	if b.senderMayControl("tech-pub") {
		t.Fatal("an expired technician must not pass senderMayControl")
	}
	// And a restart doesn't quietly renew it either.
	if b.cecApprovedTech("tech-pub-AB12C") {
		t.Fatal("suffixed form of an expired grant must also be refused")
	}
}

// The sweep reports what it dropped, so the caller can evict whatever the
// lapsed technician still holds open (a running screen share won't stop itself).
func TestPruneCecGrantsReportsExpired(t *testing.T) {
	s := LoadState("")
	s.GrantCecTech("live", cecGrantWindow)
	s.GrantCecTech("short", 2*time.Second)

	dropped := s.PruneCecGrants(time.Now().Add(time.Minute))
	if len(dropped) != 1 || dropped[0] != "short" {
		t.Fatalf("dropped %v, want [short]", dropped)
	}
	if _, held := s.CecTechExpiry("live"); !held {
		t.Fatal("the unexpired grant should survive the sweep")
	}
	if _, held := s.CecTechExpiry("short"); held {
		t.Fatal("the swept grant should be gone")
	}
}

// A technician can be cut off before the deadline — the session-end path.
func TestUnapproveTechDropsGrantImmediately(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	b.help.asking = true
	if admit, _ := b.cecAdmit("tech-pub"); !admit {
		t.Fatal("admit should succeed while asking")
	}
	b.unapproveTech("tech-pub-AB12C") // suffixed form, same technician
	if b.cecApprovedTech("tech-pub") {
		t.Fatal("ending the session should drop the grant")
	}
}

// A grant is only handed out to a device that actually asked for help; an idle
// KVM can't be driven off the open support mesh.
func TestCecAdmitRefusesWhenNotAsking(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	if admit, _ := b.cecAdmit("stranger"); admit {
		t.Fatal("must not admit a technician when not asking for help")
	}
	if _, held := b.state.CecTechExpiry("stranger"); held {
		t.Fatal("a refused technician must not be left holding a grant")
	}
}

// An expiring grant must hand its video lane back. Every other teardown path
// (stopDisplayRoute, stopDisplaySession, tearDownNative) frees the lane under
// b.mu; if the eviction sweep doesn't, each expiry burns one of maxVideoLanes
// permanently and the device ends up refusing every display offer with nothing
// but a reboot to clear it.
func TestCecEvictionFreesTheVideoLane(t *testing.T) {
	b := &Bridge{state: LoadState(""), lanes: map[uint8]bool{}}
	lane, ok := b.allocLaneLocked()
	if !ok {
		t.Fatal("lane alloc failed")
	}
	b.display = &displaySession{routeID: "r1", peer: "tech-pub-AB12C", lane: lane, cancel: make(chan struct{})}

	b.evictTech("tech-pub")

	if b.display != nil {
		t.Fatal("evicted technician kept its display session")
	}
	if b.lanes[lane] {
		t.Fatalf("lane %d still held after eviction — every expiry would burn one of %d", lane, maxVideoLanes)
	}
}

// A peer with standing authority — the device's owner or a fleet co-member —
// keeps its session when a CEC grant lapses. Whatever put a grant in their name,
// their authority comes from senderMayControl and outlives it, so sweeping them
// would drop the screen share, the input route and the web-UI tunnel of someone
// entitled to all three.
func TestCecEvictionSparesAPeerWithStandingAuthority(t *testing.T) {
	b := &Bridge{state: LoadState(""), lanes: map[uint8]bool{}}
	if !b.state.TryClaim("owner-node-ABCDE", "") {
		t.Fatal("claim failed")
	}
	lane, _ := b.allocLaneLocked()
	b.display = &displaySession{routeID: "r1", peer: "owner-node", lane: lane, cancel: make(chan struct{})}
	b.inputRoute, b.inputPeer = "r1", "owner-node"

	b.evictTech("owner-node")

	if b.display == nil {
		t.Fatal("the owner's display session was torn down by a lapsed CEC grant")
	}
	if b.inputPeer != "owner-node" {
		t.Fatal("the owner's input route was cleared by a lapsed CEC grant")
	}
}
