// Package button watches the device's physical user button — the BOOT button on
// the PCIe NanoKVM, the USR button on the Pro — and drives the CEC hand raise
// from it.
//
// The button is an evdev node (default /dev/input/event0) that emits EV_KEY
// events. On the PCIe NanoKVM the on-device screen firmware (the C++ kvm_system
// app) also reads this node and, out of the box, reacts to presses: a short
// press cycles the OLED, a long press (>=1.5s) toggles a WiFi hotspot, and a
// very-long press (>=9s) resets the account. To make the button do ONE obvious
// thing — raise a hand — we take exclusive ownership of the node with EVIOCGRAB
// (Config.Grab), so the firmware stops seeing presses, and this watcher becomes
// the sole handler:
//
//	tap  (< tapMax)     → toggle the CEC hand raise
//	hold (>= resetHold) → OnFactoryReset (we re-implement the firmware's own
//	                      hold-to-reset, since the grab took it away)
//	anything in between → ignored
//
// The OLED *display* keeps working (that's a separate firmware thread); only the
// button's firmware gestures are suppressed. If the grab fails we fall back to
// co-reading: hand raise still works, but the firmware gestures fire too.
package button

import (
	"encoding/binary"
	"io"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
	"golang.org/x/sys/unix"
)

// evdev input_event constants. On 64-bit Linux (riscv64 and aarch64 are both
// 64-bit, little-endian) struct input_event is: struct timeval{sec i64, usec
// i64} (16) + type u16 (2) + code u16 (2) + value i32 (4) = 24 bytes. We only
// read type/code/value; timing comes from the wall clock at read.
const (
	eventSize   = 24
	evKey       = 0x01 // EV_KEY
	valueUp     = 0    // key released
	valueDown   = 1    // key pressed
	valueRepeat = 2    // autorepeat (ignored)
	// eviocgrab is EVIOCGRAB = _IOW('E', 0x90, int): take exclusive ownership of
	// an evdev node so no other reader (the screen firmware) sees its events.
	// Released automatically when the fd is closed.
	eviocgrab = 0x40044590
)

// Gesture thresholds — mirror the on-device firmware's own key timings
// (kvm_system include/config.h: KEY_LONG_PRESS=1500, KEY_LONGLONG_PRESS=9000)
// so the button keeps the same feel: a quick tap is a tap, and the familiar
// long hold still resets.
const (
	tapMax        = 1500 * time.Millisecond // <= this on release → hand raise
	resetHold     = 9 * time.Second         // >= this on release → factory reset
	reopenBackoff = 5 * time.Second
)

// Toggler is the hand-raise action a tap drives. *mesh.Bridge satisfies it
// (ToggleHand); kept as an interface so this package doesn't import mesh and
// stays trivially testable.
type Toggler interface {
	ToggleHand() (raised bool, err error)
}

// Config controls the watcher.
type Config struct {
	// Enabled turns the watcher on. Device is the evdev node to read; KeyCode,
	// when non-zero, restricts to a specific evdev key code (0 = any key, right
	// for a single-button device).
	Enabled bool
	Device  string
	KeyCode int
	// Grab takes exclusive ownership of the node (EVIOCGRAB) so the on-device
	// firmware stops reacting to the button and this watcher becomes its sole
	// handler. Best-effort: on failure we co-read instead (hand raise still
	// works; the firmware gestures also still fire).
	Grab bool
	// OnFactoryReset, if set, runs when the button is held >= resetHold. Wire it
	// alongside Grab so the grabbed device keeps a working hold-to-reset.
	OnFactoryReset func()
}

// Watch starts the button watcher in a background goroutine and returns
// immediately. It is a no-op when disabled or misconfigured, so callers can
// invoke it unconditionally.
func Watch(cfg Config, toggler Toggler) {
	if !cfg.Enabled {
		log.Info("button: hand-raise button disabled")
		return
	}
	if cfg.Device == "" {
		log.Warn("button: hand-raise enabled but no input device configured; not watching")
		return
	}
	if toggler == nil {
		log.Warn("button: no hand-raise handler; not watching")
		return
	}
	go run(cfg, toggler)
}

// run reads the evdev node, reopening it with backoff if it disappears (e.g. the
// input driver comes up after us at boot).
func run(cfg Config, toggler Toggler) {
	log.Infof("button: watching %s (tap = hand raise, hold %s = factory reset)", cfg.Device, resetHold)
	for {
		watchOnce(cfg, toggler)
		time.Sleep(reopenBackoff)
	}
}

// watchOnce opens (and optionally grabs) the device and reads events until an
// error, then returns so the caller retries after a backoff.
func watchOnce(cfg Config, toggler Toggler) {
	f, err := os.Open(cfg.Device)
	if err != nil {
		log.Warnf("button: open %s: %s (will retry)", cfg.Device, err)
		return
	}
	defer func() { _ = f.Close() }()

	if cfg.Grab {
		if err := unix.IoctlSetInt(int(f.Fd()), eviocgrab, 1); err != nil {
			log.Warnf("button: could not grab %s (%s); co-reading — firmware gestures will still fire", cfg.Device, err)
		} else {
			log.Infof("button: grabbed %s — this watcher now owns the button", cfg.Device)
		}
	}

	det := &detector{tapMax: tapMax, resetHold: resetHold}
	buf := make([]byte, eventSize)
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			log.Warnf("button: read %s: %s (reopening)", cfg.Device, err)
			return
		}
		etype := binary.LittleEndian.Uint16(buf[16:18])
		code := binary.LittleEndian.Uint16(buf[18:20])
		value := int32(binary.LittleEndian.Uint32(buf[20:24]))
		if etype != evKey {
			continue
		}
		if cfg.KeyCode != 0 && int(code) != cfg.KeyCode {
			continue
		}
		switch det.feed(value, time.Now()) {
		case gestureTap:
			raised, err := toggler.ToggleHand()
			if err != nil {
				log.Errorf("button: hand-raise toggle failed: %s", err)
			} else if raised {
				log.Info("button: hand raised")
			} else {
				log.Info("button: hand lowered")
			}
		case gestureReset:
			if cfg.OnFactoryReset != nil {
				log.Warn("button: held past the reset threshold — running factory reset")
				cfg.OnFactoryReset()
			}
		}
	}
}

type gesture int

const (
	gestureNone gesture = iota
	gestureTap
	gestureReset
)

// detector maps a press (down→up) to a gesture by its hold duration, split out
// for testing. Feed it evdev key values with the wall-clock time of the event.
type detector struct {
	tapMax    time.Duration
	resetHold time.Duration

	downAt time.Time
}

func (d *detector) feed(value int32, at time.Time) gesture {
	switch value {
	case valueDown:
		d.downAt = at
	case valueUp:
		if d.downAt.IsZero() {
			return gestureNone
		}
		hold := at.Sub(d.downAt)
		d.downAt = time.Time{}
		switch {
		case hold >= d.resetHold:
			return gestureReset
		case hold <= d.tapMax:
			return gestureTap
		default:
			return gestureNone
		}
	case valueRepeat:
		// ignore autorepeat while a key is held
	}
	return gestureNone
}
