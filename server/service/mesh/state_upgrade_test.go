package mesh

import (
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
	s.GrantCecTech("tech-pub", time.Now().Add(cecGrantWindow))

	reloaded := LoadState(home)

	if got := reloaded.Owner(); got != "owner-node-ABCDE" {
		t.Fatalf("owner = %q after reload, want it preserved", got)
	}
	if _, held := reloaded.CecTechExpiry("tech-pub"); !held {
		t.Fatal("grant lost across reload")
	}
}
