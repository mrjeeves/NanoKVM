package hid

// faults.go records HID write failures that are not the host merely being
// busy, so something else can act on them.
//
// A write to /dev/hidgN fails with ESHUTDOWN when the gadget is not enumerated:
// the function exists, the descriptor opens perfectly well, and every report
// goes nowhere. Before this, that produced one log line per keystroke and
// nothing else — the KVM's whole purpose was broken and the server's only
// response was to describe it.
//
// The count is deliberately not acted on here. The hid package cannot repair a
// gadget (the USB plumbing lives in service/storage, which imports this one, so
// calling into it would be a cycle), and a write path is the wrong place to
// start unbinding controllers besides. What this does is make the failure
// legible to the supervisor that CAN act, which polls it.
//
// Its second use is subtler and more valuable. A watchdog looking only at the
// link state cannot distinguish a wedged gadget from a perfectly healthy one in
// a switched-off computer — both read "not attached". A stream of failed HID
// writes resolves that: somebody is driving this KVM right now and the input is
// going nowhere, which is not what an idle, powered-off host looks like.

import (
	"errors"
	"os"
	"sync"
	"syscall"
	"time"
)

// Fault counting is a hot-path concern (one mouse move per event), so it stays
// a mutex around two words rather than anything cleverer.
var (
	faultMu    sync.Mutex
	faultCount int
	faultFirst time.Time
	faultLast  time.Time

	faultNow = time.Now // overridable in tests
)

// recordWriteFault notes one failed report whose error means "the host is not
// accepting input", as opposed to a timeout or a descriptor we just closed
// ourselves. Timeouts are excluded on purpose: a busy host that misses an 8 ms
// deadline is not a broken link, and counting those would have the supervisor
// rebinding a working gadget under an interactive user.
func recordWriteFault(err error) {
	if !isLinkFault(err) {
		return
	}
	now := faultNow()
	faultMu.Lock()
	if faultCount == 0 {
		faultFirst = now
	}
	faultCount++
	faultLast = now
	faultMu.Unlock()
}

// clearWriteFaults is called after a report lands. One success means the link
// is carrying traffic, whatever happened before it.
func clearWriteFaults() {
	faultMu.Lock()
	faultCount = 0
	faultFirst = time.Time{}
	faultLast = time.Time{}
	faultMu.Unlock()
}

// isLinkFault reports whether err means the gadget is not enumerated, rather
// than a transient condition or our own teardown.
//
// ESHUTDOWN is what f_hid returns when the function is disabled — the canonical
// "no host" error. ENODEV covers the function disappearing under us (a rebind
// racing a write). Deadlines, EAGAIN, and ErrClosed are explicitly NOT faults:
// the first two are back-pressure from a live host, and the last is the result
// of us closing the descriptor during a deliberate gadget operation.
func isLinkFault(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) ||
		errors.Is(err, os.ErrClosed) ||
		errors.Is(err, syscall.EAGAIN) ||
		errors.Is(err, syscall.EWOULDBLOCK) {
		return false
	}
	return errors.Is(err, syscall.ESHUTDOWN) || errors.Is(err, syscall.ENODEV)
}

// WriteFaults reports the current run of consecutive link faults: how many, and
// when the run started. A zero count means the last report went out fine (or
// none has been attempted).
//
// Exported for the USB supervisor in service/storage. Polled, not pushed, so
// the two packages stay one-directional.
func WriteFaults() (count int, since time.Time) {
	faultMu.Lock()
	defer faultMu.Unlock()
	return faultCount, faultFirst
}
