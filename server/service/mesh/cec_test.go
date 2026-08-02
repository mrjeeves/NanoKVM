package mesh

import (
	"testing"
)

func TestSupportIDFromDevice(t *testing.T) {
	// Golden vectors: SHA-256(pubkey), first 8 bytes big-endian, mod 1e9,
	// zero-padded to 9 digits. Cross-checked against the Rust reference
	// (allmystuff-cec-protocol ids.rs support_id_from_string).
	cases := []struct{ in, want string }{
		{"abcdefghij", "481813332"},
		{"k7q2mzt5", "637341148"},
		// The display-suffixed form (-XXXXX, dash + 5 alnum) must derive the
		// SAME number as the bare pubkey — else a raised hand and a dialed
		// number would never meet.
		{"abcdefghij-AB12C", "481813332"},
	}
	for _, c := range cases {
		if got := supportIDFromDevice(c.in); got != c.want {
			t.Errorf("supportIDFromDevice(%q) = %q, want %q", c.in, got, c.want)
		}
		if len(supportIDFromDevice(c.in)) != 9 {
			t.Errorf("supportIDFromDevice(%q) not 9 digits: %q", c.in, supportIDFromDevice(c.in))
		}
	}
}

func TestCecHelpNetworkConfigShape(t *testing.T) {
	// The standing support area is Silent — signaling-only presence, no
	// auto-dial, no topology shaping (there are no connections to shape).
	// Mirrors AllMyStuff help_network_config.
	cfg := cecHelpNetworkConfig()
	if cfg["id"] != CecHelpNetworkID || cfg["network_id"] != CecHelpNetworkID {
		t.Errorf("network id = %v/%v, want %s", cfg["id"], cfg["network_id"], CecHelpNetworkID)
	}
	if cfg["kind"] != "silent" {
		t.Errorf("kind = %v, want silent", cfg["kind"])
	}
	if cfg["auto_approve"] != true {
		t.Errorf("auto_approve = %v, want true", cfg["auto_approve"])
	}
	sig, ok := cfg["signaling"].(map[string]interface{})
	if !ok || sig["strategy"] != "nostr" || sig["mdns"] != true {
		t.Errorf("signaling = %v, want nostr+mdns", cfg["signaling"])
	}
	if _, present := cfg["topology"]; present {
		t.Errorf("signaling-only area must carry no topology: %v", cfg["topology"])
	}
}

func TestCecAskNetworkConfigShape(t *testing.T) {
	// The asking room: same Silent shape under its own well-known id —
	// membership is the whole raised-hand signal. Mirrors AllMyStuff
	// ask_network_config.
	cfg := cecAskNetworkConfig()
	if cfg["id"] != CecAskNetworkID || cfg["network_id"] != CecAskNetworkID {
		t.Errorf("network id = %v/%v, want %s", cfg["id"], cfg["network_id"], CecAskNetworkID)
	}
	if CecAskNetworkID == CecHelpNetworkID {
		t.Fatal("the queue must be its own room")
	}
	if cfg["kind"] != "silent" {
		t.Errorf("kind = %v, want silent", cfg["kind"])
	}
	if _, present := cfg["topology"]; present {
		t.Errorf("asking room must carry no topology: %v", cfg["topology"])
	}
}
