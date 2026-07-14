package mesh

import (
	"reflect"
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

func TestCecHelpHubTopology(t *testing.T) {
	cases := []struct {
		name string
		spec string
		want map[string]interface{}
	}{
		{"empty", "", nil},
		{"whitespace", "   ", nil},
		{"single", "hubA", map[string]interface{}{"kind": "hubs", "hubs": []string{"hubA"}}},
		{"multi", "hubA, hubB ,hubC", map[string]interface{}{"kind": "hubs", "hubs": []string{"hubA", "hubB", "hubC"}}},
		{"redundancy", "hubA,hubB:2", map[string]interface{}{"kind": "hubs", "hubs": []string{"hubA", "hubB"}, "spoke_redundancy": 2}},
		{"bad_redundancy_kept_as_ids", "hubA:xyz", map[string]interface{}{"kind": "hubs", "hubs": []string{"hubA"}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := cecHelpHubTopology(c.spec)
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("cecHelpHubTopology(%q) = %#v, want %#v", c.spec, got, c.want)
			}
		})
	}
}

func TestCecHelpNetworkConfigShape(t *testing.T) {
	// Without CEC_HELP_HUBS the config must be Open + nostr/mDNS and carry NO
	// topology (daemon default). Mirrors AllMyStuff help_network_config.
	cfg := cecHelpNetworkConfig()
	if cfg["id"] != CecHelpNetworkID || cfg["network_id"] != CecHelpNetworkID {
		t.Errorf("network id = %v/%v, want %s", cfg["id"], cfg["network_id"], CecHelpNetworkID)
	}
	if cfg["kind"] != "open" {
		t.Errorf("kind = %v, want open", cfg["kind"])
	}
	if cfg["auto_approve"] != true {
		t.Errorf("auto_approve = %v, want true", cfg["auto_approve"])
	}
	sig, ok := cfg["signaling"].(map[string]interface{})
	if !ok || sig["strategy"] != "nostr" || sig["mdns"] != true {
		t.Errorf("signaling = %v, want nostr+mdns", cfg["signaling"])
	}
	if _, present := cfg["topology"]; present {
		t.Errorf("topology present without CEC_HELP_HUBS: %v", cfg["topology"])
	}
}
