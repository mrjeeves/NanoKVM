package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"time"

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
	// ClaimCode is the device's current claim-code rendezvous secret (see
	// claimcode.go) — minted while the device sits claimable with public
	// claims enabled, shown on the web page, rotated after every successful
	// claim. Persisted so the code an operator wrote down survives a restart.
	ClaimCode string `json:"claim_code,omitempty"`
	// CecGrants records the time-boxed CEC support authorisations this device
	// has handed out: canonical technician pubkey → when it was made and when
	// it runs out. Persisted for two reasons, both of which matter: a repair
	// that spans a reboot isn't cut short halfway, and — more importantly — an
	// expiry that has already been handed out cannot be forgotten by a restart
	// into an unbounded grant. See cec.go.
	CecGrants map[string]cecGrant `json:"cec_grants,omitempty"`
	// JoiningPublic remembers which signaling policy the joining mesh was
	// LAST JOINED with (nil = never recorded): the daemon persists a
	// network's config across restarts, so when the operator flips
	// config.Mesh.PublicClaims the bridge must re-join the mesh to apply the
	// new signaling — this is how it notices.
	JoiningPublic *bool `json:"joining_public,omitempty"`
}

// cecGrant is one time-boxed CEC support authorisation, as persisted. Both
// stamps are unix seconds on the wall clock; Granted is kept because it's the
// only way to tell, after a restart, that a deadline was minted before this
// device's clock was ever set (see cecExpiryLocked).
type cecGrant struct {
	Granted int64 `json:"granted"`
	Expires int64 `json:"expires"`
}

// UnmarshalJSON accepts the bare expiry a previous build wrote as well as the
// object this one writes. That compatibility is not cosmetic: persistedState is
// decoded as a whole, so one grant this build couldn't parse would fail the
// entire record — and LoadState treats an unparseable record as corrupt,
// quarantining it and resetting the device to claimable. A firmware update must
// not be able to make a KVM forget its owner.
//
// A bare number carries no mint time, so it decodes with Granted zeroed, which
// reads as "minted before the clock was set" — the conservative arm, since a
// build that recorded no mint time is exactly the one that could have written
// a 1970 deadline.
func (g *cecGrant) UnmarshalJSON(raw []byte) error {
	var expires int64
	if err := json.Unmarshal(raw, &expires); err == nil {
		g.Granted, g.Expires = 0, expires
		return nil
	}
	type plain cecGrant // no recursion back into this method
	var p plain
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	*g = cecGrant(p)
	return nil
}

// State is the live, lock-guarded KVM ownership/attachment state. It persists to
// $Home/kvm-state.json on every change. A change notifier (set via OnChange) is
// fired after each persisted mutation so the bridge can re-advertise presence.
type State struct {
	path string

	mu   sync.Mutex
	data persistedState

	// cecMono is the monotonic deadline for each grant THIS process minted.
	// Deliberately not persisted and deliberately preferred over the on-disk
	// deadline while it exists: a monotonic reading can't be moved by the wall
	// clock stepping, which is what NTP does on this no-RTC box, so a grant made
	// before the clock landed still runs its full window instead of expiring the
	// instant the correction arrives. Guarded by mu with the rest of the state.
	cecMono map[string]time.Time

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
				// Keep the corrupt bytes for forensics before falling
				// through to the fresh-device default. Note what "fresh"
				// means here: the owner is forgotten and the device offers
				// itself for claiming again — writes are atomic (see
				// persistLocked) precisely so our own writer can never
				// produce this file.
				quarantined := s.path + ".corrupt"
				if err := os.Rename(s.path, quarantined); err != nil {
					quarantined = "(quarantine failed: " + err.Error() + ")"
				}
				log.Warnf("mesh: failed to parse %s (%s) — corrupt copy kept at %s, starting fresh (claimable)", s.path, err, quarantined)
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
//
// The write is atomic — temp file, fsync, rename — because this file holds the
// ownership record: a plain truncate-and-write interrupted by a power cut
// leaves a 0-byte file, which loads as a *fresh device* on the next boot —
// silently forgetting the owner and re-offering the KVM for claiming. The
// fsync before the rename matters on the FAT-backed /data this runs from.
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
	tmp := s.path + ".tmp"
	if err := writeFileSync(tmp, raw, 0o600); err != nil {
		log.Warnf("mesh: write state %s: %s", tmp, err)
		_ = os.Remove(tmp)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Warnf("mesh: publish state %s: %s", s.path, err)
		_ = os.Remove(tmp)
	}
}

// writeFileSync is os.WriteFile plus an fsync before close, so the rename
// that follows can never land ahead of the data.
func writeFileSync(path string, raw []byte, mode os.FileMode) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := f.Write(raw); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
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

// ClaimCode returns the persisted claim code, or "" if none is minted.
func (s *State) ClaimCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.ClaimCode
}

// EnsureClaimCode returns the claim code, minting (and persisting) a fresh
// one if absent.
func (s *State) EnsureClaimCode() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.ClaimCode == "" {
		s.data.ClaimCode = newClaimCode()
		s.persistLocked()
	}
	return s.data.ClaimCode
}

// RotateClaimCode discards the claim code so the next EnsureClaimCode mints a
// fresh one — a code that admitted an owner is spent.
func (s *State) RotateClaimCode() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.ClaimCode != "" {
		s.data.ClaimCode = ""
		s.persistLocked()
	}
}

// JoiningPublic returns the signaling policy the joining mesh was last
// joined with (nil = never recorded).
func (s *State) JoiningPublic() *bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data.JoiningPublic
}

// SetJoiningPublic records the signaling policy the joining mesh was just
// joined with.
func (s *State) SetJoiningPublic(public bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.data.JoiningPublic != nil && *s.data.JoiningPublic == public {
		return
	}
	s.data.JoiningPublic = &public
	s.persistLocked()
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
	// DeepEqual, not ==: persistedState carries the CEC grant map, and a struct
	// with a map field isn't comparable.
	if reflect.DeepEqual(s.data, fresh) {
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

// ---- CEC support grants ------------------------------------------------------
//
// A technician who answers this device's raised hand is authorised for a bounded
// window, not indefinitely (see cec.go). The window lives here so it survives a
// restart in BOTH directions: a repair spanning a reboot keeps working, and a
// grant that has already expired can't be forgotten back into an open one.
//
// Measuring that window is harder than it looks on this hardware. The KVM has no
// RTC, so it boots at 1970 and stays there until NTP lands. An absolute deadline
// minted in that state is fiction, and the moment the clock corrects it lands
// decades in the past — expiring a repair that is actively under way. So there
// are two clocks here: a monotonic deadline, which is the authority for as long
// as this process lives and cannot be moved by the wall clock stepping, and the
// persisted wall-clock deadline, which exists only to carry a grant across a
// restart. See cecExpiryLocked for how a grant minted before the clock was set
// is re-anchored rather than retroactively killed.

// GrantCecTech authorises `key` for `window` from now. A non-positive window is
// refused, so a caller can never persist a grant that's already dead.
//
// The caller passes a duration rather than a deadline deliberately: computing
// "now + 3h" against an unset clock is how you get a deadline in 1970, and this
// is the one place that knows the clock might not be trustworthy.
func (s *State) GrantCecTech(key string, window time.Duration) {
	if key == "" || window <= 0 {
		return
	}
	now := time.Now()
	s.mu.Lock()
	if s.data.CecGrants == nil {
		s.data.CecGrants = map[string]cecGrant{}
	}
	if s.cecMono == nil {
		s.cecMono = map[string]time.Time{}
	}
	s.data.CecGrants[key] = cecGrant{Granted: now.Unix(), Expires: now.Add(window).Unix()}
	// Retains now's monotonic reading, so this deadline survives NTP landing.
	s.cecMono[key] = now.Add(window)
	s.persistLocked()
	s.mu.Unlock()
}

// CecTechExpiry returns when `key`'s authorisation runs out and whether one is
// currently held. An expired grant reports held=false — callers never have to
// compare the deadline themselves. A held grant whose deadline can't yet be
// stated (see cecExpiryLocked) reports the zero time with held=true.
func (s *State) CecTechExpiry(key string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cecExpiryLocked(key, time.Now())
}

// cecExpiryLocked resolves one grant against `now` (caller holds s.mu), healing
// a deadline the clock has invalidated.
//
// Three cases, in the order they're trusted:
//
//  1. This process minted the grant, so it holds a monotonic deadline for it.
//     That is immune to the wall clock stepping and always wins — it's what
//     keeps NTP landing mid-repair from ending the session.
//  2. The grant came off disk (we restarted) and was minted while the clock was
//     already set. Its absolute deadline means what it says.
//  3. The grant came off disk and was minted before the clock was ever set, so
//     its deadline is meaningless. If the clock is sane NOW, re-anchor to a full
//     window from this moment — the first instant the device could measure one.
//     If the clock is still unset there is nothing to measure with, and the
//     grant is held: refusing would end a live repair on the strength of a
//     deadline we know is fiction, and the window is still bounded by the next
//     restart.
func (s *State) cecExpiryLocked(key string, now time.Time) (time.Time, bool) {
	g, ok := s.data.CecGrants[key]
	if !ok {
		return time.Time{}, false
	}
	if until, ok := s.cecMono[key]; ok {
		return until, until.After(now)
	}
	if clockSane(time.Unix(g.Granted, 0)) {
		at := time.Unix(g.Expires, 0)
		return at, at.After(now)
	}
	if !clockSane(now) {
		return time.Time{}, true
	}
	until := now.Add(cecGrantWindow)
	s.data.CecGrants[key] = cecGrant{Granted: now.Unix(), Expires: until.Unix()}
	if s.cecMono == nil {
		s.cecMono = map[string]time.Time{}
	}
	s.cecMono[key] = until
	s.persistLocked()
	log.Infof("mesh: CEC grant for %s was minted before the clock was set — re-anchored to %s from now", key, cecGrantWindow)
	return until, true
}

// RevokeCecTech drops `key`'s authorisation immediately (the technician ended
// the session, or the device was unclaimed).
func (s *State) RevokeCecTech(key string) {
	s.mu.Lock()
	if _, ok := s.data.CecGrants[key]; ok {
		delete(s.data.CecGrants, key)
		delete(s.cecMono, key)
		s.persistLocked()
	}
	s.mu.Unlock()
}

// PruneCecGrants drops every authorisation that has run out by `now`, returning
// the keys it dropped so the caller can tear down whatever they still hold open.
// Resolution goes through cecExpiryLocked, so a grant the clock can't yet judge
// is carried rather than swept.
func (s *State) PruneCecGrants(now time.Time) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var dropped []string
	for key := range s.data.CecGrants {
		if _, held := s.cecExpiryLocked(key, now); held {
			continue
		}
		delete(s.data.CecGrants, key)
		delete(s.cecMono, key)
		dropped = append(dropped, key)
	}
	if len(dropped) > 0 {
		s.persistLocked()
	}
	return dropped
}

// LatestCecGrant reports whether any authorisation is outstanding at `now` and
// the furthest-out deadline among them. held can be true with a zero deadline —
// a grant the clock can't yet put a time on is still a grant, and a viewer
// showing "authorised, no deadline" is honest where "not authorised" would be a
// lie.
func (s *State) LatestCecGrant(now time.Time) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var latest time.Time
	held := false
	for key := range s.data.CecGrants {
		at, ok := s.cecExpiryLocked(key, now)
		if !ok {
			continue
		}
		held = true
		if at.After(latest) {
			latest = at
		}
	}
	return latest, held
}

// CecGrantKeys returns the technician keys currently holding a grant, expired
// or not — the eviction sweep a reset performs before clearing them.
func (s *State) CecGrantKeys() map[string]struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]struct{}, len(s.data.CecGrants))
	for key := range s.data.CecGrants {
		out[key] = struct{}{}
	}
	return out
}

// ClearCecGrants drops every authorisation — the unclaim path, where the device
// is handing itself back and must not stay reachable by a previous technician.
func (s *State) ClearCecGrants() {
	s.mu.Lock()
	if len(s.data.CecGrants) > 0 {
		s.data.CecGrants = nil
		s.cecMono = nil
		s.persistLocked()
	}
	s.mu.Unlock()
}
