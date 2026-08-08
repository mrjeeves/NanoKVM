package viewer

import (
	"sync"
	"time"
)

// A Lease keeps the KVM's HDMI receiver and capture path active while at least
// one live viewer (the web console or an AllMyStuff display route) can consume
// frames.
//
// Powering the receiver down is NOT a free "stop capturing": it de-asserts HPD,
// so the attached computer sees its monitor unplugged — windows rearrange, the
// resolution resets, and some machines blank or sleep. Bringing it back costs
// the source a full re-negotiation, which is seconds, not milliseconds. Two
// consequences shape this file:
//
//   - idleDelay is long. A page reload, a pop-out, or a hand-off between the
//     web console and a mesh display route must never reach the hardware. The
//     first version used 2s, which a plain browser refresh already exceeds, so
//     ordinary use hot-plugged the attached machine over and over.
//   - Acquire BLOCKS until the receiver is actually back. The encoder cannot
//     produce a frame until the source has re-negotiated, so returning early
//     just makes the first reads fail and paints a capture error ("No image
//     captured") over a link that is really only mid-handshake.
//
// The owner's own preference outranks the lease: a KVM whose HDMI the operator
// switched off in the web UI stays off no matter who connects. See SetAllowed —
// without it the lease silently overrode the persisted setting that
// /api/vm/hdmi still reported back to the UI.
// Both are var, not const, only so the tests can shrink them; nothing in the
// server reassigns them.
var (
	// idleDelay is how long the last viewer's departure is tolerated before
	// the receiver is powered down.
	idleDelay = 60 * time.Second
	// settleTimeout bounds how long Acquire waits for a re-activation to
	// finish. It only bounds the wait — a slower source still recovers, the
	// caller just starts reading (and retrying) before it does.
	settleTimeout = 5 * time.Second
)

var (
	mu sync.Mutex

	// viewers is the number of live screen viewers holding a lease.
	viewers int
	// allowed mirrors the operator's persisted HDMI preference. False pins the
	// receiver off regardless of who is watching.
	allowed = true
	// desired is the state the hardware should be in: allowed && viewers > 0.
	desired bool

	// apply is the device-specific hardware switch, installed by Configure.
	apply func(bool)
	// applied is the last state apply was called with; known is false until it
	// has been called at all, so the boot state is always asserted once.
	applied  bool
	known    bool
	applying bool

	// appliedCh is closed and replaced on every completed hardware transition,
	// so waiters can block on a state change without polling.
	appliedCh = make(chan struct{})

	// offTimer is the pending idle power-down; offGen invalidates a timer that
	// has already fired or been superseded.
	offTimer *time.Timer
	offGen   uint64
)

// Configure installs the device-specific HDMI switch and asserts the current
// state. It is called once during server startup, after SetAllowed has seeded
// the persisted preference.
func Configure(fn func(bool)) {
	mu.Lock()
	apply = fn
	known = false // force the first transition so boot state is asserted
	recomputeLocked()
	mu.Unlock()
	pump()
}

// SetAllowed records the operator's persisted HDMI preference (the web UI's
// enable/disable switch, backed by utils.IsHdmiDisabled). Disabling pins the
// receiver off immediately; enabling lets the lease bring it up for the next
// viewer — and for a viewer already connected, right away.
func SetAllowed(v bool) {
	mu.Lock()
	allowed = v
	if !v {
		cancelOffTimerLocked()
	}
	recomputeLocked()
	mu.Unlock()
	pump()
	if v {
		waitActive()
	}
}

// Note records a hardware change made outside the lease — the web UI's manual
// enable/disable/reset handlers drive the receiver directly. Without it the
// lease's idea of the hardware and the hardware itself drift apart, and the
// next transition is skipped as a no-op.
func Note(active bool) {
	mu.Lock()
	applied, known = active, true
	notifyAppliedLocked()
	mu.Unlock()
	pump()
}

// Acquire marks one live screen viewer and returns an idempotent release. It
// blocks until the receiver is up (bounded by settleTimeout) so the caller's
// first frame read lands on a negotiated link rather than a dead one.
func Acquire() func() {
	mu.Lock()
	viewers++
	cancelOffTimerLocked()
	recomputeLocked()
	mu.Unlock()
	pump()
	waitActive()

	var once sync.Once
	return func() { once.Do(release) }
}

func release() {
	mu.Lock()
	if viewers > 0 {
		viewers--
	}
	if viewers > 0 {
		mu.Unlock()
		return
	}
	armOffTimerLocked()
	mu.Unlock()
}

// recomputeLocked refreshes the desired hardware state from the inputs.
func recomputeLocked() {
	desired = allowed && viewers > 0
}

// cancelOffTimerLocked stops any pending power-down and invalidates a timer
// that has already fired but not yet taken the lock.
func cancelOffTimerLocked() {
	if offTimer != nil {
		offTimer.Stop()
		offTimer = nil
	}
	offGen++
}

// armOffTimerLocked schedules the idle power-down.
func armOffTimerLocked() {
	cancelOffTimerLocked()
	gen := offGen
	offTimer = time.AfterFunc(idleDelay, func() {
		mu.Lock()
		if gen != offGen || viewers > 0 {
			mu.Unlock()
			return
		}
		offTimer = nil
		recomputeLocked()
		mu.Unlock()
		pump()
	})
}

// pump drives the hardware toward `desired`, one caller at a time, with mu
// released across the switch itself — apply reaches cgo and procfs and can
// block for hundreds of milliseconds, which must never stall an Acquire on an
// unrelated goroutine. Whoever owns the switch re-reads `desired` after each
// transition, so a change made while it was running is never lost.
func pump() {
	mu.Lock()
	defer mu.Unlock()
	for {
		if applying || apply == nil || (known && applied == desired) {
			return
		}
		want, fn := desired, apply
		applying = true
		mu.Unlock()
		fn(want)
		mu.Lock()
		applying = false
		applied, known = want, true
		notifyAppliedLocked()
	}
}

// notifyAppliedLocked wakes every waitActive blocked on a state change.
func notifyAppliedLocked() {
	close(appliedCh)
	appliedCh = make(chan struct{})
}

// waitActive blocks until the receiver is up, the desired state stops being
// "up", or settleTimeout expires.
func waitActive() {
	t := time.NewTimer(settleTimeout)
	defer t.Stop()
	for {
		mu.Lock()
		if !desired || (known && applied) {
			mu.Unlock()
			return
		}
		ch := appliedCh
		mu.Unlock()
		select {
		case <-ch:
		case <-t.C:
			return
		}
	}
}
