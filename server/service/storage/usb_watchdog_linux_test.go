//go:build linux

package storage

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// base is a fixed instant so every test reasons in explicit offsets rather than
// wall-clock timing, which is what makes these deterministic.
var base = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func newTestWatchdog() *udcWatchdog { return &udcWatchdog{} }

// feed drives the watchdog through a sequence of samples at fixed offsets,
// returning the actions it decided on.
func feed(w *udcWatchdog, samples []struct {
	at    time.Duration
	state string
}) []udcAction {
	var out []udcAction
	for _, s := range samples {
		now := base.Add(s.at)
		udcNow = func() time.Time { return now }
		w.observe(s.state, nil)
		out = append(out, w.shouldRecover(now, 0, time.Time{}))
	}
	udcNow = time.Now
	return out
}

// A link that comes up and stays up must never be touched. This is the case
// that runs on every healthy device forever, so a false positive here would be
// the watchdog disconnecting working KVMs.
func TestWatchdogLeavesHealthyLinkAlone(t *testing.T) {
	w := newTestWatchdog()
	var samples []struct {
		at    time.Duration
		state string
	}
	for i := 0; i < 200; i++ {
		samples = append(samples, struct {
			at    time.Duration
			state string
		}{time.Duration(i) * udcPollInterval, udcStateConfigured})
	}
	for i, a := range feed(w, samples) {
		if a != udcActionNone {
			t.Fatalf("sample %d: acted on a healthy link (%v)", i, a)
		}
	}
}

// The failure this was built for: the link worked, then died and stayed dead.
// It must escalate — but only after the debounce, so a host reboot's own brief
// disconnect doesn't trigger a rebind.
func TestWatchdogRecoversLinkThatDiedAfterWorking(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()

	w.observe(udcStateConfigured, nil)
	if !w.sawConfigured {
		t.Fatal("a configured link was not recorded as healthy")
	}

	dead := base.Add(time.Second)
	udcNow = func() time.Time { return dead }
	w.observe(udcStateDetached, nil)

	// Inside the debounce: a host rebooting looks exactly like this.
	if a := w.shouldRecover(dead.Add(udcDeadDebounceSeen-time.Second), 0, time.Time{}); a != udcActionNone {
		t.Fatalf("acted inside the debounce window: %v", a)
	}
	// Past it: this link is not coming back on its own.
	if a := w.shouldRecover(dead.Add(udcDeadDebounceSeen+time.Second), 0, time.Time{}); a != udcActionSoft {
		t.Fatalf("want a rebind past the debounce, got %v", a)
	}
}

// A link never seen working is the ambiguous case: a bind that failed at boot
// looks identical to a KVM sitting in a switched-off computer. It must still be
// recovered eventually, but only after a much longer wait — a machine that is
// simply off must not have its gadget churned every 15 seconds.
func TestWatchdogWaitsMuchLongerForALinkNeverSeenWorking(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()
	w.observe(udcStateDetached, nil)

	if a := w.shouldRecover(base.Add(udcDeadDebounceSeen+time.Second), 0, time.Time{}); a != udcActionNone {
		t.Fatal("a link never seen working was recovered on the short debounce — an idle powered-off host would be churned")
	}
	if a := w.shouldRecover(base.Add(udcDeadDebounceUnseen+time.Second), 0, time.Time{}); a != udcActionSoft {
		t.Fatalf("want a rebind past the long debounce, got %v", a)
	}
}

// Failed HID reports are what separates the two cases above: an idle
// powered-off host produces none, because nobody is typing at it. Sustained
// failures mean a live user driving a dead link, which must not wait minutes.
func TestWatchdogHidFaultsShortenTheUnseenWait(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()
	w.observe(udcStateDetached, nil)

	now := base.Add(udcDeadDebounceSeen + time.Second)
	faultsSince := base

	// Too few failures to mean anything.
	if a := w.shouldRecover(now, udcHidFaultThreshold-1, faultsSince); a != udcActionNone {
		t.Fatalf("acted on an insignificant number of HID faults: %v", a)
	}
	// Enough failures, but a burst too brief to prove a persistent fault.
	if a := w.shouldRecover(base.Add(time.Second), udcHidFaultThreshold, base); a != udcActionNone {
		t.Fatalf("acted on a momentary burst of HID faults: %v", a)
	}
	// Sustained: somebody is using this KVM and the input is going nowhere.
	if a := w.shouldRecover(now, udcHidFaultThreshold, faultsSince); a != udcActionSoft {
		t.Fatalf("want a rebind once HID faults prove a live user, got %v", a)
	}
}

// Escalation order: rebind first (cheap, keeps the composed gadget), rebuild
// only when rebinding did not bring the link back.
func TestWatchdogEscalatesToRebuildOnlyAfterRebindsFail(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()
	w.sawConfigured = true
	w.observe(udcStateDetached, nil)

	now := base.Add(udcDeadDebounceSeen + time.Second)
	if a := w.shouldRecover(now, 0, time.Time{}); a != udcActionSoft {
		t.Fatalf("first action should be a rebind, got %v", a)
	}
	w.attempt = 1
	w.nextAttemptAfter = time.Time{}
	if a := w.shouldRecover(now, 0, time.Time{}); a != udcActionSoft {
		t.Fatalf("second action should still be a rebind, got %v", a)
	}
	w.attempt = 2
	if a := w.shouldRecover(now, 0, time.Time{}); a != udcActionHard {
		t.Fatalf("third action should escalate to a rebuild, got %v", a)
	}
}

// Backoff must hold between attempts, or a link that stays down turns into a
// rebind every two seconds — which is worse than the bug.
func TestWatchdogBacksOffBetweenAttempts(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()
	w.sawConfigured = true
	w.observe(udcStateDetached, nil)

	now := base.Add(udcDeadDebounceSeen + time.Second)
	w.attempt = 1
	w.nextAttemptAfter = now.Add(udcRecoveryBackoffMin)

	if a := w.shouldRecover(now.Add(time.Second), 0, time.Time{}); a != udcActionNone {
		t.Fatalf("acted inside the backoff window: %v", a)
	}
	if a := w.shouldRecover(now.Add(udcRecoveryBackoffMin+time.Second), 0, time.Time{}); a == udcActionNone {
		t.Fatal("never acted again after the backoff expired")
	}
}

// A link that recovers must reset everything, so the next failure gets a full
// debounce and a fresh backoff rather than an escalated one.
func TestWatchdogResetsAfterRecovery(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()

	w.observe(udcStateDetached, nil)
	w.attempt = 3
	w.nextAttemptAfter = base.Add(time.Hour)

	w.observe(udcStateConfigured, nil)
	if !w.deadSince.IsZero() || w.attempt != 0 || !w.nextAttemptAfter.IsZero() {
		t.Fatalf("a recovered link left stale state: deadSince=%v attempt=%d next=%v",
			w.deadSince, w.attempt, w.nextAttemptAfter)
	}
	if a := w.shouldRecover(base.Add(time.Hour), 0, time.Time{}); a != udcActionNone {
		t.Fatalf("acted on a recovered link: %v", a)
	}
}

// Mid-enumeration states are not health, but they are not evidence of a dead
// link either. A link flapping through them must not have its dead-timer
// restarted on every sample, or it would never reach the debounce and never be
// recovered.
func TestWatchdogKeepsDeadTimerAcrossTransientStates(t *testing.T) {
	w := newTestWatchdog()
	udcNow = func() time.Time { return base }
	defer func() { udcNow = time.Now }()
	w.sawConfigured = true

	w.observe(udcStateDetached, nil)
	deadAt := w.deadSince

	later := base.Add(5 * time.Second)
	udcNow = func() time.Time { return later }
	w.observe("addressed", nil)
	w.observe(udcStateDetached, nil)

	if !w.deadSince.Equal(deadAt) {
		t.Fatalf("a transient state restarted the dead timer: %v -> %v", deadAt, w.deadSince)
	}
}

// A media change deliberately unbinds the gadget. The watchdog must hold off
// while that happens, or it would fight the mount path.
func TestWatchdogHoldsOffDuringMediaOperations(t *testing.T) {
	now := base
	udcNow = func() time.Time { return now }
	defer func() { udcNow = time.Now }()

	mediaOpMu.Lock()
	mediaOpSettleUntil = time.Time{}
	mediaOpMu.Unlock()

	if usbGadgetSettling() {
		t.Fatal("settling with no media operation in flight")
	}
	noteUSBGadgetMutated()
	if !usbGadgetSettling() {
		t.Fatal("not settling immediately after a media operation")
	}
	now = base.Add(udcSettleAfterMediaOp + time.Second)
	if usbGadgetSettling() {
		t.Fatal("still settling long after the media operation")
	}
}

// readUDCState must read the gadget framework's state file, which is the whole
// signal this feature rests on.
func TestReadUDCStateReadsTheStateFile(t *testing.T) {
	root := t.TempDir()
	udc := filepath.Join(root, "4340000.usb")
	if err := os.MkdirAll(udc, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(udc, "state"), []byte("configured\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	orig := udcClassDir
	udcClassDir = root
	defer func() { udcClassDir = orig }()

	got, err := readUDCState()
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	if got != udcStateConfigured {
		t.Fatalf("state = %q, want %q", got, udcStateConfigured)
	}
}

// A device with no gadget controller at all must be reported as an error, not
// silently treated as a healthy or a dead link.
func TestReadUDCStateWithNoController(t *testing.T) {
	orig := udcClassDir
	udcClassDir = t.TempDir()
	defer func() { udcClassDir = orig }()

	if _, err := readUDCState(); err == nil {
		t.Fatal("no error for a device with no UDC")
	}
}
