//go:build linux

package storage

// usb_watchdog_linux.go supervises the USB gadget while the server runs.
//
// Everything else in this package repairs the gadget at exactly two moments:
// once at startup (recoverUSBAtStartup) and whenever a media change touches it.
// A gadget that dies at any other time — the host cuts VBUS on a reboot, the
// controller loses its session, an enumeration never completes — stayed dead
// until somebody restarted the server or unplugged the cable, because nothing
// was watching. That is the failure this file exists to end.
//
// The signal is /sys/class/udc/<udc>/state, which the gadget framework
// maintains and which nothing here read before. It is the only authoritative
// view of the LINK. configfs' g0/UDC is a binding record: it stays populated
// across a dead link, so "is UDC non-empty" — the health test the rest of this
// package uses — cannot see this failure at all. (is_a_peripheral was tried and
// correctly abandoned for the same reason; see ensureUSBGadgetBound.)
//
// The hard part is not detecting "not attached", it is that a healthy gadget on
// a powered-off host reports exactly the same thing. Re-arming a gadget nobody
// is looking at is harmless in itself, but doing it every few seconds forever
// is churn, log noise, and a way to interrupt a host that is halfway through
// enumerating. So escalation is deliberately reluctant: see shouldRecover.

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"

	"NanoKVM-Server/service/hid"
)

const (
	// How often the link state is sampled. Cheap: one small sysfs read.
	udcPollInterval = 2 * time.Second

	// How long the link must look dead before acting, once it has been seen
	// working. Long enough to ride out a host reboot's own disconnect, short
	// enough that a wedged gadget recovers without a human.
	udcDeadDebounceSeen = 15 * time.Second

	// The same, for a link that has NEVER been seen working since this server
	// started. That covers a bind that silently failed at boot, but it is also
	// the exact reading of a device sitting in a powered-off machine, so it
	// waits far longer before touching anything.
	udcDeadDebounceUnseen = 3 * time.Minute

	// Floor between recovery attempts, doubling up to udcRecoveryBackoffMax
	// while the link stays dead, and reset the moment it comes back.
	udcRecoveryBackoffMin = 30 * time.Second
	udcRecoveryBackoffMax = 15 * time.Minute

	// A media change legitimately unbinds and rebinds the gadget. Ignore the
	// link for this long after one so the watchdog never races the mount path.
	udcSettleAfterMediaOp = 10 * time.Second

	// How many consecutive failed HID reports, sustained for how long, count as
	// proof that a live user is driving a dead link. Both bounds matter: a
	// couple of failures happen around any legitimate gadget operation, and a
	// burst that is over in a millisecond says nothing about a persistent fault.
	udcHidFaultThreshold = 10
	udcHidFaultMinAge    = 5 * time.Second
)

// udcStateConfigured is the only state that means the host has accepted the
// gadget and reports are actually going somewhere. The others below it in the
// enumeration sequence are transient and are treated as "in progress", not as
// health, so a link stuck at "addressed" is still a link this recovers.
const udcStateConfigured = "configured"

// udcStateDetached is what a link with no session reads as: no host, or a host
// that stopped talking to us.
const udcStateDetached = "not attached"

var (
	// Overridable for tests; the real paths live under /sys.
	udcClassDir    = usbUDCClass
	udcReadFile    = os.ReadFile
	udcNow         = time.Now
	udcRecoverSoft = softRecoverUSBGadget
	udcRecoverHard = hardRecoverUSBGadget

	// mediaOpSettleUntil is set by the mount path so the watchdog holds off
	// while the gadget is legitimately in flux. Guarded by its own mutex rather
	// than imageMountMu: the watchdog must be able to READ it without waiting
	// on a media operation that takes seconds.
	mediaOpMu          sync.Mutex
	mediaOpSettleUntil time.Time
)

// noteUSBGadgetMutated tells the watchdog that something deliberately disturbed
// the gadget. Called by the media path, which unbinds and rebinds the UDC as
// part of normal operation — without this the transient "not attached" during a
// mount would look exactly like the failure this watches for.
func noteUSBGadgetMutated() {
	mediaOpMu.Lock()
	mediaOpSettleUntil = udcNow().Add(udcSettleAfterMediaOp)
	mediaOpMu.Unlock()
}

func usbGadgetSettling() bool {
	mediaOpMu.Lock()
	defer mediaOpMu.Unlock()
	return udcNow().Before(mediaOpSettleUntil)
}

// udcWatchdog is the supervisor's state across polls. Kept in a struct so the
// decision logic (shouldRecover) is a pure function of observed state and can
// be tested without sysfs, sleeps, or a gadget.
type udcWatchdog struct {
	// sawConfigured records that the link worked at least once since the last
	// recovery. It is what separates "this died" from "this was never alive",
	// and it selects which debounce applies.
	sawConfigured bool

	// deadSince is when the link was first seen detached in the current run of
	// detached samples; zero when the link is not currently detached.
	deadSince time.Time

	// nextAttemptAfter throttles escalation, and attempt drives the backoff.
	nextAttemptAfter time.Time
	attempt          int

	lastState string
}

// StartUSBWatchdog runs the supervisor until ctx-less process exit. Best-effort
// and self-silencing in the same spirit as the rest of this package: it must
// never take the server down, so it recovers from panics and only logs.
func StartUSBWatchdog() {
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Errorf("usb watchdog panicked: %v", r)
			}
		}()

		w := &udcWatchdog{}
		for {
			time.Sleep(udcPollInterval)
			w.poll()
		}
	}()
}

func (w *udcWatchdog) poll() {
	state, err := readUDCState()
	if err != nil {
		// No UDC at all is not a transient link problem — it is a device
		// without a gadget controller, or a kernel that never bound one.
		// Nothing here can fix that, and saying so every two seconds helps
		// nobody, so it is logged only when it CHANGES.
		w.observe("", err)
		return
	}
	w.observe(state, nil)

	if usbGadgetSettling() {
		return
	}

	now := udcNow()
	faults, faultsSince := hid.WriteFaults()
	action := w.shouldRecover(now, faults, faultsSince)
	if action == udcActionNone {
		return
	}

	w.attempt++
	backoff := udcRecoveryBackoffMin << (w.attempt - 1)
	if backoff > udcRecoveryBackoffMax || backoff <= 0 {
		backoff = udcRecoveryBackoffMax
	}
	w.nextAttemptAfter = now.Add(backoff)

	switch action {
	case udcActionSoft:
		log.Warnf("usb watchdog: link has read %q for %s (attempt %d) — rebinding the gadget",
			udcStateDetached, now.Sub(w.deadSince).Truncate(time.Second), w.attempt)
		if err := udcRecoverSoft(); err != nil {
			log.Errorf("usb watchdog: rebind failed: %s", err)
		}
	case udcActionHard:
		log.Warnf("usb watchdog: link still dead after %d rebind(s) — resetting the USB PHY", w.attempt-1)
		if err := udcRecoverHard(); err != nil {
			log.Errorf("usb watchdog: PHY reset failed: %s", err)
		}
	}
}

type udcAction int

const (
	udcActionNone udcAction = iota
	udcActionSoft
	udcActionHard
)

// observe folds one sample into the watchdog's state and logs transitions.
//
// Transitions are logged at Info deliberately: a field failure currently leaves
// almost no evidence behind, and the sequence of link states with timestamps is
// the difference between "the host cut VBUS and we never came back" and "we
// never enumerated in the first place" — which are different bugs with
// different fixes.
func (w *udcWatchdog) observe(state string, err error) {
	if err != nil {
		state = "unavailable"
	}
	if state != w.lastState {
		if w.lastState != "" {
			log.Infof("usb watchdog: link state %q -> %q", w.lastState, state)
		} else {
			log.Infof("usb watchdog: link state %q", state)
		}
		w.lastState = state
	}

	switch state {
	case udcStateConfigured:
		if !w.deadSince.IsZero() || w.attempt > 0 {
			log.Infof("usb watchdog: link healthy again")
		}
		w.sawConfigured = true
		w.deadSince = time.Time{}
		w.attempt = 0
		w.nextAttemptAfter = time.Time{}
	case udcStateDetached:
		if w.deadSince.IsZero() {
			w.deadSince = udcNow()
		}
	default:
		// Mid-enumeration (attached / powered / default / addressed) or
		// unavailable. Not healthy, but not evidence of a dead link either —
		// leave any running dead-timer alone rather than restarting it, so a
		// link flapping through these states still eventually escalates.
	}
}

// shouldRecover decides whether to act, and how far to escalate. Pure: every
// input is already on the struct or passed in.
//
// faults/faultsSince are the current run of failed HID reports (see
// hid.WriteFaults). They are what lets this tell a wedged gadget from a healthy
// one in a switched-off computer, which read identically at the link. Somebody
// pressing keys that go nowhere is not an idle host, so that evidence collapses
// the long "never seen working" wait down to the short one.
func (w *udcWatchdog) shouldRecover(now time.Time, faults int, faultsSince time.Time) udcAction {
	if w.deadSince.IsZero() {
		return udcActionNone
	}
	debounce := udcDeadDebounceUnseen
	if w.sawConfigured {
		debounce = udcDeadDebounceSeen
	}
	if faults >= udcHidFaultThreshold && !faultsSince.IsZero() &&
		now.Sub(faultsSince) >= udcHidFaultMinAge {
		debounce = udcDeadDebounceSeen
	}
	if now.Sub(w.deadSince) < debounce {
		return udcActionNone
	}
	if !w.nextAttemptAfter.IsZero() && now.Before(w.nextAttemptAfter) {
		return udcActionNone
	}
	// Rebinding is the cheap, targeted fix and it resolves the common case
	// (a stale session after the host went away). The PHY reset tears the
	// controller off its driver and is reserved for a link that did not come
	// back from one, because it is far more disruptive.
	if w.attempt >= 2 {
		return udcActionHard
	}
	return udcActionSoft
}

// readUDCState returns the link state of the first gadget controller. The
// gadget framework exposes one `state` file per UDC; its values are the USB
// device states plus "not attached".
func readUDCState() (string, error) {
	entries, err := os.ReadDir(udcClassDir)
	if err != nil {
		return "", err
	}
	if len(entries) == 0 {
		return "", os.ErrNotExist
	}
	data, err := udcReadFile(filepath.Join(udcClassDir, entries[0].Name(), "state"))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// softRecoverUSBGadget re-arms the gadget without rebuilding it: unbind
// configfs from the controller, then bind it back. This is the cheap fix, and
// it is the one that resolves a stale session left behind when a host went
// away — the functions, descriptors and LUN are all still correct, only the
// attachment is dead.
//
// HID descriptors are closed across the operation and reopened after, exactly
// as the media path does: a descriptor held open across a rebind refers to a
// function the host can no longer see.
func softRecoverUSBGadget() error {
	h := hid.GetHid()
	h.Lock()
	h.CloseNoLock()
	defer func() {
		// The gadget has just been rebound; /dev/hidg* reappear a moment later.
		// Retry rather than taking one shot at reopening them, or input stays
		// dead until the next write happens to reopen by chance.
		if err := h.OpenNoLockWithRetry(2*time.Second, 100*time.Millisecond); err != nil {
			log.Errorf("usb watchdog: reopen HID after rebind: %s", err)
		}
		h.Unlock()
	}()

	// resetUSBGadget already does exactly this ladder — unbind, force the
	// controller back to peripheral role, rebind with retries — and is the same
	// code the media path leans on. Reusing it keeps one implementation of
	// "re-arm the gadget" rather than a second that drifts.
	return resetUSBGadget()
}

// hardRecoverUSBGadget is the escalation: S03usbdev's restart_phy, which
// unbinds the DWC2 controller from its driver and binds it back. The same path
// the user-facing "recover USB" button takes. Far more disruptive than a
// rebind, so it is reserved for a link that did not come back from one. It
// handles the HID descriptors itself.
func hardRecoverUSBGadget() error {
	return hid.ResetUSBPHY()
}
