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
	// GrantCecTech deliberately refuses to record a dead deadline.
	raw, err := json.Marshal(persistedState{
		Claimable: false,
		Owner:     "owner-node",
		CecGrants: map[string]int64{"tech-pub": time.Now().Add(-time.Minute).Unix()},
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
	s.GrantCecTech("live", time.Now().Add(cecGrantWindow))
	s.GrantCecTech("short", time.Now().Add(2*time.Second))

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
