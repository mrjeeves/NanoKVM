package viewer

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// recorder collects the hardware transitions the lease asks for.
type recorder struct {
	mu  sync.Mutex
	got []bool
}

func (r *recorder) apply(active bool) {
	r.mu.Lock()
	r.got = append(r.got, active)
	r.mu.Unlock()
}

func (r *recorder) snapshot() []bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]bool(nil), r.got...)
}

// newLease resets the package state and installs a recorder. The delays are
// shrunk so the tests exercise the transitions rather than the grace windows.
func newLease(t *testing.T) *recorder {
	t.Helper()
	mu.Lock()
	if offTimer != nil {
		offTimer.Stop()
	}
	viewers = 0
	apply = nil
	allowed = true
	desired = false
	applied = false
	known = false
	applying = false
	offTimer = nil
	offGen++
	idleDelay = 5 * time.Millisecond
	settleTimeout = time.Second
	mu.Unlock()

	r := &recorder{}
	Configure(r.apply)
	return r
}

// waitFor polls until the recorded transitions match want, so a test never
// depends on a fixed sleep outrunning a timer.
func waitFor(t *testing.T, r *recorder, want []bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		got := r.snapshot()
		if reflect.DeepEqual(got, want) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("transitions = %v, want %v", got, want)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFirstViewerActivatesAndLastViewerDeactivates(t *testing.T) {
	r := newLease(t)

	releaseA := Acquire()
	releaseB := Acquire()
	releaseA()
	// One viewer remains: no power-down, and no redundant re-assert.
	time.Sleep(20 * time.Millisecond)
	if got := r.snapshot(); !reflect.DeepEqual(got, []bool{false, true}) {
		t.Fatalf("one remaining viewer must keep HDMI active: %v", got)
	}

	releaseB()
	waitFor(t, r, []bool{false, true, false})
}

func TestHandoffCancelsPendingDeactivation(t *testing.T) {
	r := newLease(t)

	release := Acquire()
	release()
	release() // idempotent
	// Re-acquire inside the idle grace: the hardware must never be touched.
	next := Acquire()
	time.Sleep(20 * time.Millisecond)
	if got := r.snapshot(); !reflect.DeepEqual(got, []bool{false, true}) {
		t.Fatalf("a hand-off inside the grace must not hot-plug the source: %v", got)
	}

	next()
	waitFor(t, r, []bool{false, true, false})
}

// A KVM whose operator switched HDMI off in the web UI must stay off however
// many viewers connect — the lease previously overrode that setting silently,
// while /api/vm/hdmi went on reporting it as the live state.
func TestDisabledPreferenceOutranksViewers(t *testing.T) {
	r := newLease(t)

	SetAllowed(false)
	release := Acquire()
	time.Sleep(20 * time.Millisecond)
	if got := r.snapshot(); !reflect.DeepEqual(got, []bool{false}) {
		t.Fatalf("a disabled KVM must not be woken by a viewer: %v", got)
	}
	release()

	// Re-enabling with a viewer connected brings it up immediately.
	release = Acquire()
	SetAllowed(true)
	waitFor(t, r, []bool{false, true})
	release()
	waitFor(t, r, []bool{false, true, false})
}

// Acquire must not return before the receiver is actually up: the caller reads
// a frame straight afterwards, and on a dead link that surfaces as a capture
// error rather than as the hand-off it really is.
func TestAcquireWaitsForActivation(t *testing.T) {
	newLease(t)

	var active bool
	var activeMu sync.Mutex
	mu.Lock()
	apply = func(on bool) {
		if on {
			time.Sleep(30 * time.Millisecond)
		}
		activeMu.Lock()
		active = on
		activeMu.Unlock()
	}
	known = false
	mu.Unlock()

	release := Acquire()
	activeMu.Lock()
	up := active
	activeMu.Unlock()
	if !up {
		t.Fatal("Acquire returned before the receiver finished coming up")
	}
	release()
}

// A manual enable/disable/reset drives the hardware directly; Note keeps the
// lease's cached view in step so the next transition is not skipped as a no-op.
func TestNoteResyncsExternalHardwareChange(t *testing.T) {
	r := newLease(t)

	release := Acquire()
	waitFor(t, r, []bool{false, true})

	// Something outside the lease switched the receiver off (a manual reset's
	// off half). Without Note the lease still believes it is on and would never
	// re-assert it for the viewer that is still connected.
	Note(false)
	waitFor(t, r, []bool{false, true, true})
	release()
}
