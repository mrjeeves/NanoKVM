package viewer

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

func resetTestState() {
	mu.Lock()
	if offTimer != nil {
		offTimer.Stop()
	}
	viewers = 0
	apply = nil
	enabled = false
	offTimer = nil
	idleDelay = 5 * time.Millisecond
	mu.Unlock()
}

func TestFirstViewerActivatesAndLastViewerDeactivates(t *testing.T) {
	resetTestState()
	var got []bool
	var gotMu sync.Mutex
	Configure(func(active bool) {
		gotMu.Lock()
		got = append(got, active)
		gotMu.Unlock()
	})

	releaseA := Acquire()
	releaseB := Acquire()
	releaseA()
	time.Sleep(10 * time.Millisecond)
	gotMu.Lock()
	mid := append([]bool(nil), got...)
	gotMu.Unlock()
	if !reflect.DeepEqual(mid, []bool{false, true}) {
		t.Fatalf("one remaining viewer must keep HDMI active: %v", mid)
	}

	releaseB()
	time.Sleep(15 * time.Millisecond)
	gotMu.Lock()
	final := append([]bool(nil), got...)
	gotMu.Unlock()
	if !reflect.DeepEqual(final, []bool{false, true, false}) {
		t.Fatalf("last viewer should deactivate HDMI: %v", final)
	}
}

func TestHandoffCancelsPendingDeactivationAndReleaseIsIdempotent(t *testing.T) {
	resetTestState()
	var got []bool
	var gotMu sync.Mutex
	Configure(func(active bool) {
		gotMu.Lock()
		got = append(got, active)
		gotMu.Unlock()
	})

	releaseA := Acquire()
	releaseA()
	releaseA()
	releaseB := Acquire()
	time.Sleep(10 * time.Millisecond)
	gotMu.Lock()
	mid := append([]bool(nil), got...)
	gotMu.Unlock()
	if !reflect.DeepEqual(mid, []bool{false, true}) {
		t.Fatalf("handoff should stay active and reassert the hardware: %v", mid)
	}
	releaseB()
	time.Sleep(10 * time.Millisecond)
	gotMu.Lock()
	final := append([]bool(nil), got...)
	gotMu.Unlock()
	if !reflect.DeepEqual(final, []bool{false, true, false}) {
		t.Fatalf("handoff should deactivate after its final viewer leaves: %v", final)
	}
}
