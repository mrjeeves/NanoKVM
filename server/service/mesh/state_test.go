package mesh

import (
	"os"
	"path/filepath"
	"testing"
)

// TestStatePersistIsAtomic: a persisted mutation reloads exactly and leaves no
// temp litter — the write goes temp+fsync+rename, never truncate-in-place.
func TestStatePersistIsAtomic(t *testing.T) {
	dir := t.TempDir()
	s := LoadState(dir)
	if !s.TryClaim("owner-node", "") {
		t.Fatal("claim should succeed on a fresh state")
	}

	if _, err := os.Stat(filepath.Join(dir, stateFile+".tmp")); !os.IsNotExist(err) {
		t.Fatalf("temp file must not survive a persist (stat err: %v)", err)
	}
	back := LoadState(dir)
	if back.Owner() != "owner-node" {
		t.Fatalf("reload lost the owner: %q", back.Owner())
	}
}

// TestStateCorruptFileQuarantinesAndStartsFresh: the power-cut case. A
// truncated kvm-state.json must not be silently deleted — it's moved aside as
// .corrupt — and the device starts as a fresh claimable box (the documented
// fresh-device default).
func TestStateCorruptFileQuarantinesAndStartsFresh(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, stateFile)
	if err := os.WriteFile(path, []byte{}, 0o600); err != nil {
		t.Fatalf("plant truncated state: %v", err)
	}

	s := LoadState(dir)
	if s.Owner() != "" || !s.Claimable() {
		t.Fatalf("corrupt state must load as fresh (owner=%q claimable=%v)", s.Owner(), s.Claimable())
	}
	if _, err := os.Stat(path + ".corrupt"); err != nil {
		t.Fatalf("corrupt bytes must be quarantined: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("original corrupt file must be moved aside (stat err: %v)", err)
	}
}
