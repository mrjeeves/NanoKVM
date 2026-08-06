package viewer

import (
	"sync"
	"time"
)

// A Lease keeps the KVM's HDMI/capture path active while at least one live
// viewer (the web console or an AllMyStuff display route) can consume frames.
// The final release waits briefly before deactivating so a route re-offer,
// pop-out, or reconnect can hand the encoder over without hot-plugging the
// attached computer between the old and new viewer.
var (
	mu        sync.Mutex
	viewers   int
	apply     func(bool)
	enabled   bool
	offTimer  *time.Timer
	idleDelay = 2 * time.Second
)

// Configure installs the device-specific HDMI switch and immediately applies
// the current state. It is called once during server startup.
func Configure(fn func(bool)) {
	mu.Lock()
	apply = fn
	enabled = viewers > 0
	active := enabled
	mu.Unlock()
	if fn != nil {
		fn(active)
	}
}

// Acquire marks one live screen viewer and returns an idempotent release.
func Acquire() func() {
	mu.Lock()
	if offTimer != nil {
		offTimer.Stop()
		offTimer = nil
	}
	wasDisabled := !enabled
	enabled = true
	viewers++
	fn := apply
	mu.Unlock()
	if wasDisabled && fn != nil {
		fn(true)
	}

	var once sync.Once
	return func() {
		once.Do(release)
	}
}

func release() {
	mu.Lock()
	if viewers > 0 {
		viewers--
	}
	if viewers != 0 {
		mu.Unlock()
		return
	}
	if offTimer != nil {
		offTimer.Stop()
	}
	offTimer = time.AfterFunc(idleDelay, func() {
		mu.Lock()
		if viewers != 0 {
			mu.Unlock()
			return
		}
		offTimer = nil
		enabled = false
		fn := apply
		if fn != nil {
			fn(false)
		}
		mu.Unlock()
	})
	mu.Unlock()
}
