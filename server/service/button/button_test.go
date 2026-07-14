package button

import (
	"testing"
	"time"
)

func newDetector() *detector {
	return &detector{tapMax: tapMax, resetHold: resetHold}
}

// press feeds a down then an up separated by hold, and returns the gesture the
// release produced.
func press(d *detector, downAt time.Time, hold time.Duration) gesture {
	d.feed(valueDown, downAt)
	return d.feed(valueUp, downAt.Add(hold))
}

func TestTapRaisesHand(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)
	for i, hold := range []time.Duration{10 * time.Millisecond, 200 * time.Millisecond, tapMax} {
		if g := press(d, t0.Add(time.Duration(i)*time.Second), hold); g != gestureTap {
			t.Errorf("hold %s: got %v, want gestureTap", hold, g)
		}
	}
}

func TestLongHoldResets(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)
	if g := press(d, t0, resetHold); g != gestureReset {
		t.Fatalf("hold == resetHold: got %v, want gestureReset", g)
	}
	if g := press(d, t0.Add(30*time.Second), resetHold+3*time.Second); g != gestureReset {
		t.Fatalf("hold > resetHold: got %v, want gestureReset", g)
	}
}

func TestMediumHoldIgnored(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)
	// Between tapMax and resetHold — formerly the firmware's WiFi gesture, now
	// deliberately nothing.
	for i, hold := range []time.Duration{tapMax + time.Millisecond, 3 * time.Second, resetHold - time.Millisecond} {
		if g := press(d, t0.Add(time.Duration(i)*time.Minute), hold); g != gestureNone {
			t.Errorf("hold %s: got %v, want gestureNone", hold, g)
		}
	}
}

func TestUpWithoutDownIgnored(t *testing.T) {
	d := newDetector()
	if g := d.feed(valueUp, time.Unix(1_000_000, 0)); g != gestureNone {
		t.Fatalf("stray release: got %v, want gestureNone", g)
	}
}

func TestAutorepeatIgnored(t *testing.T) {
	d := newDetector()
	t0 := time.Unix(1_000_000, 0)
	d.feed(valueDown, t0)
	for i := 0; i < 3; i++ {
		if g := d.feed(valueRepeat, t0.Add(time.Duration(i+1)*time.Second)); g != gestureNone {
			t.Fatal("autorepeat should never produce a gesture")
		}
	}
	// A tap-length total hold still resolves to a tap on release.
	if g := d.feed(valueUp, t0.Add(300*time.Millisecond)); g != gestureTap {
		t.Fatalf("release after autorepeat: got %v, want gestureTap", g)
	}
}
