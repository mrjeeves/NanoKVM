// Package button watches the device's physical user button — the BOOT button
// on the PCIe NanoKVM, the USR button on the Pro — and toggles the CEC
// hand-raise on a deliberate gesture.
//
// The button is read as an evdev node (default /dev/input/event0) that emits
// EV_KEY events. On the PCIe NanoKVM the on-device screen firmware (the C++
// kvm_system app) ALSO reads this node without grabbing it, and reacts to
// presses: a short press cycles the OLED page, a long press (>=1.5s) opens WiFi
// config, and a very-long press (>=9s) resets the password. We cannot suppress
// those. So the hand-raise gesture is a DOUBLE SHORT-PRESS — two taps each well
// under the long-press threshold — which the firmware treats as two harmless
// page cycles while we treat it as a toggle. A single tap is deliberately NOT
// used, so ordinary button use never raises a hand by accident. (On the Pro the
// USR button is not surfaced to Linux by the repo, so the watcher ships
// disabled there; set mesh.handRaise on a device where the evdev node is
// known.)
package button

import (
	"encoding/binary"
	"io"
	"os"
	"time"

	log "github.com/sirupsen/logrus"
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
)

// Gesture thresholds. shortPressMax stays safely under the screen firmware's
// 1500ms long-press so our taps can never trip WiFi config or the 9s
// password-reset. doubleWindow is the max gap between the two taps.
const (
	shortPressMax = 800 * time.Millisecond
	doubleWindow  = 700 * time.Millisecond
	reopenBackoff = 5 * time.Second
)

// Toggler is the hand-raise action the button drives. *mesh.Bridge satisfies
// it (ToggleHand); kept as an interface so this package doesn't import mesh and
// stays trivially testable.
type Toggler interface {
	ToggleHand() (raised bool, err error)
}

// Config controls the watcher. Device is the evdev node to read; KeyCode, when
// non-zero, restricts the gesture to a specific evdev key code (0 = any key,
// which is right for a device with a single user button).
type Config struct {
	Enabled bool
	Device  string
	KeyCode int
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

// run reads the evdev node, reopening it with backoff if it disappears (e.g.
// the input driver comes up after us at boot), and toggles on the gesture.
func run(cfg Config, toggler Toggler) {
	log.Infof("button: watching %s for hand-raise (double short-press)", cfg.Device)
	for {
		if watchOnce(cfg, toggler) {
			return // clean EOF with nothing more to do is unexpected; back off and retry
		}
		time.Sleep(reopenBackoff)
	}
}

// watchOnce opens the device and reads events until an error. It returns false
// so the caller retries after a backoff.
func watchOnce(cfg Config, toggler Toggler) bool {
	f, err := os.Open(cfg.Device)
	if err != nil {
		log.Warnf("button: open %s: %s (will retry)", cfg.Device, err)
		return false
	}
	defer func() { _ = f.Close() }()

	det := &detector{shortMax: shortPressMax, doubleWindow: doubleWindow}
	buf := make([]byte, eventSize)
	for {
		if _, err := io.ReadFull(f, buf); err != nil {
			log.Warnf("button: read %s: %s (reopening)", cfg.Device, err)
			return false
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
		if det.feed(value, time.Now()) {
			raised, err := toggler.ToggleHand()
			if err != nil {
				log.Errorf("button: hand-raise toggle failed: %s", err)
			} else if raised {
				log.Info("button: hand raised")
			} else {
				log.Info("button: hand lowered")
			}
		}
	}
}

// detector is the double-short-press state machine, split out for testing. Feed
// it evdev key values (with the wall-clock time of the event); it returns true
// once per completed double short-press.
type detector struct {
	shortMax     time.Duration
	doubleWindow time.Duration

	downAt       time.Time
	haveOneShort bool
	lastShortUp  time.Time
}

func (d *detector) feed(value int32, at time.Time) bool {
	switch value {
	case valueDown:
		d.downAt = at
	case valueUp:
		if d.downAt.IsZero() {
			return false
		}
		hold := at.Sub(d.downAt)
		d.downAt = time.Time{}
		if hold > d.shortMax {
			// A long press belongs to the screen firmware, not us; it also
			// breaks any in-progress double-tap.
			d.haveOneShort = false
			return false
		}
		if d.haveOneShort && at.Sub(d.lastShortUp) <= d.doubleWindow {
			d.haveOneShort = false
			return true
		}
		d.haveOneShort = true
		d.lastShortUp = at
	case valueRepeat:
		// ignore autorepeat
	}
	return false
}
