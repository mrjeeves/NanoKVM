package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// A state file written by the PREVIOUS firmware — no cec_grants key — must load
// with its owner intact. If it didn't, LoadState would quarantine it as corrupt
// and fall back to the fresh-device default: the KVM would forget its owner, go
// claimable, and vanish from its owner's mesh while still serving its LAN web
// UI. Adding a field to persistedState is exactly the change that could do it,
// so the upgrade path is pinned here.
func TestUpgradeFromPreGrantStateKeepsItsOwner(t *testing.T) {
	home := t.TempDir()
	old := `{"owner":"owner-node-ABCDE","claimable":false,"attached_to":"n1","fleet_key":"fk","fleet_name":"fn"}`
	if err := os.WriteFile(filepath.Join(home, stateFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	s := LoadState(home)

	if got := s.Owner(); got != "owner-node-ABCDE" {
		t.Fatalf("owner = %q after upgrade, want it preserved", got)
	}
	if s.Claimable() {
		t.Fatal("device went claimable on upgrade — it would leave its owner's mesh")
	}
	if s.AttachedTo() != "n1" || s.FleetKey() != "fk" {
		t.Fatal("attachment or fleet key lost on upgrade")
	}
	if _, err := os.Stat(filepath.Join(home, stateFile+".corrupt")); err == nil {
		t.Fatal("the old state file was quarantined as corrupt")
	}
}

// And a record this firmware wrote, grants included, reloads whole.
func TestStateWithGrantsRoundTrips(t *testing.T) {
	home := t.TempDir()
	s := LoadState(home)
	if !s.TryClaim("owner-node-ABCDE", "") {
		t.Fatal("claim failed")
	}
	s.GrantCecTech("tech-pub", cecGrantWindow)

	reloaded := LoadState(home)

	if got := reloaded.Owner(); got != "owner-node-ABCDE" {
		t.Fatalf("owner = %q after reload, want it preserved", got)
	}
	if _, held := reloaded.CecTechExpiry("tech-pub"); !held {
		t.Fatal("grant lost across reload")
	}
}

// The KVM has no RTC: it boots at 1970 and stays there until NTP lands. A grant
// minted in that window carries a deadline decades in the past, and once the
// clock corrects a naive comparison expires it instantly — ending a repair that
// is actively under way, at the exact moment the device finally learned the
// time. Such a grant must be re-anchored to a full window from the correction
// instead.
func TestGrantMintedBeforeTheClockWasSetIsReanchored(t *testing.T) {
	home := t.TempDir()
	dead := time.Date(1970, 1, 1, 3, 0, 0, 0, time.UTC) // 1970 + the 3h window
	raw, err := json.Marshal(persistedState{
		Owner: "owner-node",
		CecGrants: map[string]cecGrant{"tech-pub": {
			Granted: time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC).Unix(),
			Expires: dead.Unix(),
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, stateFile), raw, 0o600); err != nil {
		t.Fatal(err)
	}

	// A fresh State is what a restart produces: no monotonic anchor survives,
	// so only the persisted stamps are left to judge by.
	s := LoadState(home)
	at, held := s.CecTechExpiry("tech-pub")
	if !held {
		t.Fatal("NTP landing must not retroactively expire a live grant")
	}
	if left := time.Until(at); left > cecGrantWindow || left < cecGrantWindow-time.Minute {
		t.Fatalf("re-anchored grant runs for %s, want ~%s", left, cecGrantWindow)
	}

	// And the re-anchor is written down, so the next restart doesn't redo it
	// and hand out yet another full window.
	// Compared in whole seconds: that's the resolution the record stores, so
	// the reloaded deadline drops the sub-second part the live one carries.
	again, _ := LoadState(home).CecTechExpiry("tech-pub")
	if again.Unix() != at.Unix() {
		t.Fatalf("re-anchor not persisted: %s then %s", at, again)
	}
}

// A grant an EARLIER build wrote is a bare expiry, not an object. It has to
// keep parsing: persistedState is decoded whole, so one unreadable grant fails
// the entire record — and LoadState quarantines an unparseable record and
// resets the device to claimable. A firmware update must never be able to make
// a KVM forget its owner.
func TestBareExpiryGrantFromAnEarlierBuildStillLoads(t *testing.T) {
	home := t.TempDir()
	old := `{"owner":"owner-node","claimable":false,"cec_grants":{"tech-pub":2000000000}}`
	if err := os.WriteFile(filepath.Join(home, stateFile), []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}

	s := LoadState(home)

	if s.Owner() != "owner-node" {
		t.Fatal("owner lost decoding an older grant shape")
	}
	if _, err := os.Stat(filepath.Join(home, stateFile+".corrupt")); err == nil {
		t.Fatal("state file was quarantined as corrupt")
	}
	// It carries no mint time, so it's treated as minted before the clock was
	// set — the conservative arm, since that build is exactly the one that
	// could have written a 1970 deadline.
	if _, held := s.CecTechExpiry("tech-pub"); !held {
		t.Fatal("a grant from an older build should be honoured, not dropped")
	}
}

// While the clock is still unset there is nothing to measure a window with. A
// grant is held rather than refused — refusing would end a live repair on the
// strength of a deadline known to be fiction — and the sweep must not drop it.
func TestGrantIsHeldWhileTheClockIsStillUnset(t *testing.T) {
	s := LoadState("")
	s.data.CecGrants = map[string]cecGrant{"tech-pub": {Granted: 0, Expires: 3 * 3600}}

	unset := time.Date(1970, 1, 1, 1, 0, 0, 0, time.UTC)
	if _, held := s.cecExpiryLocked("tech-pub", unset); !held {
		t.Fatal("a grant must not be refused merely because the clock is unset")
	}
	if dropped := s.PruneCecGrants(unset); len(dropped) != 0 {
		t.Fatalf("sweep dropped %v on an unset clock — nothing is measurable yet", dropped)
	}
}
