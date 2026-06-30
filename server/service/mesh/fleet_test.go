package mesh

import "testing"

// Golden values computed from the AUTHORITATIVE Rust implementation
// (AllMyStuff node/src/ownership.rs::derive_fleet_network_id) run over these
// exact keys. If the Go port diverges anywhere — byte reversal, the >>21 shift,
// base36 digit order, or a word-list edit — these stop matching. This is the
// cross-language guard that the KVM derives the SAME closed-network id its fleet
// uses (a mismatch would silently strand the KVM on a different network).
func TestDeriveFleetNetworkIDMatchesRust(t *testing.T) {
	cases := map[string]string{
		"fleet-secret-key": "lucky-gauss-e67fs",
		"casey-fleet-01":   "lively-gauss-f8oty",
		"test":             "tidal-dijkstra-l3ejy",
		"":                 "ancient-meitner-54xu4",
		"a1b2c3":           "bright-fermi-xx3lc",
	}
	for key, want := range cases {
		if got := DeriveFleetNetworkID(key); got != want {
			t.Errorf("DeriveFleetNetworkID(%q) = %q, want %q (Rust authoritative)", key, got, want)
		}
	}
}

// The word-lists must stay the exact length the Rust side froze (52 adjectives,
// 48 names) — a length change shifts every modulo and re-derives every fleet.
func TestFleetWordListLengths(t *testing.T) {
	if len(fleetAdjectives) != 52 {
		t.Errorf("fleetAdjectives = %d, want 52 (must match ownership.rs)", len(fleetAdjectives))
	}
	if len(fleetNames) != 48 {
		t.Errorf("fleetNames = %d, want 48 (must match ownership.rs)", len(fleetNames))
	}
}
