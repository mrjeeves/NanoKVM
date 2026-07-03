package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"

	log "github.com/sirupsen/logrus"
)

// stateFile is the persisted KVM state under $Home.
const stateFile = "kvm-state.json"

// persistedState is the on-disk durable record. Claimable defaults to true for a
// fresh device (it's offering itself for adoption); everything else is empty.
type persistedState struct {
	Owner      string `json:"owner"`
	Claimable  bool   `json:"claimable"`
	AttachedTo string `json:"attached_to"`
	// AttachedLabel is the attach target's display label at attach time —
	// what names this device "KVM-<label>" on the graph and on the daemon
	// identity. Cosmetic, best-effort (may be empty), refreshed per attach.
	AttachedLabel string `json:"attached_label,omitempty"`
	FleetKey      string `json:"fleet_key"`
	FleetName     string `json:"fleet_name"`
	// FleetVenue is the owner's fleet-network transport config (a JSON object
	// string), handed down with the fleet key. Persisted so a restart can
	// rejoin the fleet network at the same venue.
	FleetVenue string `json:"fleet_venue,omitempty"`
}

// State is the live, lock-guarded KVM ownership/attachment state. It persists to
// $Home/kvm-state.json on every change. A change notifier (set via OnChange) is
// fired after each persisted mutation so the bridge can re-advertise presence.
type State struct {
	path string

	mu   sync.Mutex
	data persistedState

	onChange func()
}

// LoadState reads the persisted state from home, or starts fresh (claimable) if
// no file exists. A home of "" disables persistence (used in tests).
func LoadState(home string) *State {
	s := &State{}
	if home != "" {
		s.path = filepath.Join(home, stateFile)
	}
	// Fresh-device default: claimable so the device offers itself for adoption.
	s.data = persistedState{Claimable: true}

	if s.path != "" {
		if raw, err := os.ReadFile(s.path); err == nil {
			var loaded persistedState
			if err := json.Unmarshal(raw, &loaded); err == nil {
				s.data = loaded
			} else {
				log.Warnf("mesh: failed to parse %s, starting fresh: %s", s.path, err)
			}
		}
	}
	return s
}

// OnChange registers a callback fired after every persisted mutation.
func (s *State) OnChange(fn func()) {
	s.mu.Lock()
	s.onChange = fn
	s.mu.Unlock()
}

// persistLocked writes the current state to disk (caller holds s.mu). A missing
// home directory is created; a write failure is logged but not fatal.
func (s *State) persistLocked() {
	if s.path == "" {
		return
	}
	if dir := filepath.Dir(s.path); dir != "" {
		_ = os.MkdirAll(dir, 0o755)
	}
	raw, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		log.Warnf("mesh: marshal state: %s", err)
		return
	}
	if err := os.WriteFile(s.path, raw, 0o600); err != nil {
		log.Warnf("mesh: write state %s: %s", s.path, err)
	}
}

// snapshot returns a copy of the current state under the lock.
func (s *State) snapshot() persistedState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

// notify fires onChange outside the lock.
func (s *State) notify() {
	s.mu.Lock()
	fn := s.onChange
	s.mu.Unlock()
	if fn != nil {
		fn()
	}
}

// ---- accessors --------------------------------------------------------------

// Owner returns the recorded owner node id, or "" if unowned.
func (s *State) Owner() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Owner
}

// Claimable reports whether the device is currently offering itself for adoption.
func (s *State) Claimable() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.Claimable
}

// AttachedTo returns the graph node this KVM is bound to, or "" if detached.
func (s *State) AttachedTo() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AttachedTo
}

// AttachedLabel returns the attach target's display label, or "" if unknown.
func (s *State) AttachedLabel() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.AttachedLabel
}

// FleetName returns the device's fleet display name (cosmetic), or "".
func (s *State) FleetName() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.FleetName
}

// FleetKey returns the shared fleet key, or "".
func (s *State) FleetKey() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.FleetKey
}

// ---- mutations --------------------------------------------------------------

// TryClaim records owner and ends claim mode, but only if the device is still
// claimable. AUTO-ATTACH: the KVM is wired to the machine that claims it, so the
// claim also binds attached_to to the owner. ownerLabel is the claimer's display
// label when known (from its presence advert) — it names this device
// "KVM-<label>". Returns whether the claim took.
func (s *State) TryClaim(owner, ownerLabel string) bool {
	s.mu.Lock()
	if !s.data.Claimable || s.data.Owner != "" {
		s.mu.Unlock()
		return false
	}
	s.data.Owner = owner
	s.data.Claimable = false
	// Auto-attach: the KVM is physically wired to the claimer's machine.
	s.data.AttachedTo = owner
	s.data.AttachedLabel = ownerLabel
	s.persistLocked()
	s.mu.Unlock()
	s.notify()
	return true
}

// SetAttachedTo binds the KVM to node (or clears it when node == ""; the label
// clears with it). Returns whether anything changed.
func (s *State) SetAttachedTo(node, label string) bool {
	s.mu.Lock()
	if node == "" {
		label = ""
	}
	if s.data.AttachedTo == node && s.data.AttachedLabel == label {
		s.mu.Unlock()
		return false
	}
	s.data.AttachedTo = node
	s.data.AttachedLabel = label
	s.persistLocked()
	s.mu.Unlock()
	s.notify()
	return true
}

// Unclaim is the owner-ordered factory reset of the mesh identity: the device
// forgets its owner, its attachment, and its fleet credential, and offers
// itself for adoption again. The caller (the bridge) is responsible for the
// matching network moves — leaving the fleet mesh and returning to the joining
// mesh. Returns whether anything changed (a second Release is a no-op).
func (s *State) Unclaim() bool {
	s.mu.Lock()
	fresh := persistedState{Claimable: true}
	if s.data == fresh {
		s.mu.Unlock()
		return false
	}
	s.data = fresh
	s.persistLocked()
	s.mu.Unlock()
	s.notify()
	return true
}

// AdoptFleetKey records the fleet credential handed down by this device's owner.
// Returns whether anything changed.
func (s *State) AdoptFleetKey(key, name string, venue *string) bool {
	if key == "" {
		return false
	}
	s.mu.Lock()
	changed := false
	if s.data.FleetKey != key {
		s.data.FleetKey = key
		changed = true
	}
	if name != "" && s.data.FleetName != name {
		s.data.FleetName = name
		changed = true
	}
	if venue != nil && s.data.FleetVenue != *venue {
		s.data.FleetVenue = *venue
		changed = true
	}
	if changed {
		s.persistLocked()
	}
	s.mu.Unlock()
	if changed {
		s.notify()
	}
	return changed
}
