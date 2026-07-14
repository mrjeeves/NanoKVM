package button

import (
	"testing"
	"time"
)

// press feeds a down+up pair separated by hold, at wall-clock base+offset for
// the down. Returns whether the up completed the gesture (toggle).
func press(d *detector, downAt time.Time, hold time.Duration) bool {
	d.feed(valueDown, downAt)
	return d.feed(valueUp, downAt.Add(hold))
}

func newDetector() *detector {
	return &detector{shortMax: shortPressMax, doubleWindow: doubleWindow}
}

func TestDoubleShortPressToggles(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)

	// First short tap: no toggle yet.
	if press(d, t0, 100*time.Millisecond) {
		t.Fatal("single tap should not toggle")
	}
	// Second short tap within the window: toggle.
	if !press(d, t0.Add(300*time.Millisecond), 100*time.Millisecond) {
		t.Fatal("double tap within window should toggle")
	}
	// A third tap must start a fresh gesture, not toggle off a stale one.
	if press(d, t0.Add(600*time.Millisecond), 100*time.Millisecond) {
		t.Fatal("third tap should be a fresh first-of-pair, not a toggle")
	}
}

func TestSingleTapNeverToggles(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)
	// Taps spaced beyond the double-window are all lone first-taps.
	for i := 0; i < 5; i++ {
		at := t0.Add(time.Duration(i) * 2 * time.Second)
		if press(d, at, 120*time.Millisecond) {
			t.Fatalf("isolated tap %d should not toggle", i)
		}
	}
}

func TestLongPressIgnoredAndBreaksPair(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)

	// A long press (>= shortMax) is the screen firmware's gesture, not ours.
	if press(d, t0, shortPressMax+200*time.Millisecond) {
		t.Fatal("long press should not toggle")
	}
	// One short tap...
	if press(d, t0.Add(2*time.Second), 100*time.Millisecond) {
		t.Fatal("first short after long should not toggle")
	}
	// ...then a long press must cancel the in-progress pair...
	if press(d, t0.Add(2*time.Second+300*time.Millisecond), shortPressMax+100*time.Millisecond) {
		t.Fatal("long press should not toggle")
	}
	// ...so the next short tap is again a lone first-tap.
	if press(d, t0.Add(2*time.Second+600*time.Millisecond), 100*time.Millisecond) {
		t.Fatal("short after long-cancelled pair should not toggle")
	}
}

func TestSecondTapTooLateStartsNewPair(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)

	if press(d, t0, 100*time.Millisecond) {
		t.Fatal("first tap should not toggle")
	}
	// Second tap after the double-window elapses: not a double, becomes the
	// new first tap.
	late := t0.Add(100*time.Millisecond + doubleWindow + 200*time.Millisecond)
	if press(d, late, 100*time.Millisecond) {
		t.Fatal("second tap past the window should not toggle")
	}
	// A prompt follow-up now completes a double.
	if !press(d, late.Add(200*time.Millisecond), 100*time.Millisecond) {
		t.Fatal("tap within window of the new first-tap should toggle")
	}
}

func TestAutorepeatIgnored(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)
	d.feed(valueDown, t0)
	// Autorepeat events while held must not be treated as taps.
	for i := 0; i < 3; i++ {
		if d.feed(valueRepeat, t0.Add(time.Duration(i+1)*100*time.Millisecond)) {
			t.Fatal("autorepeat should never toggle")
		}
	}
	// Release completes a single short press (no toggle on its own).
	if d.feed(valueUp, t0.Add(400*time.Millisecond)) {
		t.Fatal("release of a single held press should not toggle")
	}
}
