package hid

import (
	"fmt"
	"os"
	"syscall"
	"testing"
	"time"
)

func resetFaults() {
	faultMu.Lock()
	faultCount = 0
	faultFirst = time.Time{}
	faultLast = time.Time{}
	faultMu.Unlock()
}

// The classifier is the whole point of this file: it decides what counts as
// evidence that the host is not there. Getting it wrong in either direction is
// costly — miss ESHUTDOWN and the dead link is never repaired; count a timeout
// and a busy host has its gadget rebound underneath an interactive user.
func TestIsLinkFaultDistinguishesDeadLinkFromBackPressure(t *testing.T) {
	faults := []struct {
		name string
		err  error
		want bool
	}{
		{"ESHUTDOWN — the gadget is not enumerated", syscall.ESHUTDOWN, true},
		{"ENODEV — the function went away under us", syscall.ENODEV, true},
		{"wrapped ESHUTDOWN", fmt.Errorf("write: %w", syscall.ESHUTDOWN), true},

		{"nil", nil, false},
		{"deadline — a busy but live host", os.ErrDeadlineExceeded, false},
		{"ErrClosed — our own teardown", os.ErrClosed, false},
		{"EAGAIN — back-pressure", syscall.EAGAIN, false},
		{"EWOULDBLOCK — back-pressure", syscall.EWOULDBLOCK, false},
		{"unrelated error", fmt.Errorf("something else"), false},
	}
	for _, f := range faults {
		if got := isLinkFault(f.err); got != f.want {
			t.Errorf("%s: isLinkFault = %v, want %v", f.name, got, f.want)
		}
	}
}

// A run of link faults must accumulate with the time it started, since the
// supervisor uses both the count and the age.
func TestWriteFaultsAccumulateAndReportTheirStart(t *testing.T) {
	resetFaults()
	defer resetFaults()

	start := time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)
	now := start
	faultNow = func() time.Time { return now }
	defer func() { faultNow = time.Now }()

	for i := 0; i < 5; i++ {
		recordWriteFault(syscall.ESHUTDOWN)
		now = now.Add(time.Second)
	}

	count, since := WriteFaults()
	if count != 5 {
		t.Fatalf("count = %d, want 5", count)
	}
	if !since.Equal(start) {
		t.Fatalf("since = %v, want the first failure at %v", since, start)
	}
}

// Back-pressure must not accumulate, or an interactive session against a slow
// host would look identical to a dead link.
func TestWriteFaultsIgnoreBackPressure(t *testing.T) {
	resetFaults()
	defer resetFaults()

	for i := 0; i < 100; i++ {
		recordWriteFault(os.ErrDeadlineExceeded)
		recordWriteFault(syscall.EAGAIN)
		recordWriteFault(os.ErrClosed)
	}
	if count, _ := WriteFaults(); count != 0 {
		t.Fatalf("count = %d after only back-pressure, want 0", count)
	}
}

// One report landing proves the link carries traffic, whatever came before it.
func TestWriteFaultsClearOnSuccess(t *testing.T) {
	resetFaults()
	defer resetFaults()

	for i := 0; i < 20; i++ {
		recordWriteFault(syscall.ESHUTDOWN)
	}
	if count, _ := WriteFaults(); count == 0 {
		t.Fatal("faults did not accumulate")
	}

	clearWriteFaults()

	count, since := WriteFaults()
	if count != 0 {
		t.Fatalf("count = %d after a successful report, want 0", count)
	}
	if !since.IsZero() {
		t.Fatalf("since = %v after a successful report, want zero", since)
	}
}
