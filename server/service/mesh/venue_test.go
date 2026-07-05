package mesh

import (
	"encoding/json"
	"testing"
)

// sampleMeshConfig is a trimmed MeshConfig as config_show would return it: two
// networks that SHARE the reference STUN server (so dedup must collapse it) plus
// distinct TURN servers, with the fleet network listed SECOND (so fleet-first
// ordering must reorder it).
const sampleMeshConfig = `{
  "version": 1,
  "networks": [
    {
      "id": "join",
      "network_id": "joining-net",
      "stun_servers": [{ "urls": ["stun:stun.myownmesh.com:3478"] }],
      "turn_servers": [
        {
          "urls": ["turn:turn.myownmesh.com:3478"],
          "username": "guest",
          "credential": "theguestpassword"
        }
      ]
    },
    {
      "id": "fleet",
      "network_id": "swift-tesla-abcde",
      "stun_servers": [{ "urls": ["stun:stun.myownmesh.com:3478"] }],
      "turn_servers": [
        {
          "urls": ["turn:fleet.example.com:3478"],
          "username": "fleetuser",
          "credential": "fleetpass"
        }
      ]
    },
    {
      "id": "empty",
      "network_id": "no-ice-net",
      "stun_servers": [],
      "turn_servers": []
    }
  ]
}`

func TestParseVenueICEServers_FleetFirstDedup(t *testing.T) {
	got := parseVenueICEServers(json.RawMessage(sampleMeshConfig), "swift-tesla-abcde")

	// Expected, in order:
	//   1. fleet STUN  (stun.myownmesh — attributed to the fleet, appears first)
	//   2. fleet TURN  (fleet.example)
	//   3. joining TURN (turn.myownmesh, guest) — its STUN is a dup of #1, dropped
	if len(got) != 3 {
		t.Fatalf("want 3 servers, got %d: %+v", len(got), got)
	}

	if len(got[0].URLs) != 1 || got[0].URLs[0] != "stun:stun.myownmesh.com:3478" {
		t.Errorf("entry 0 should be the shared STUN first: %+v", got[0])
	}
	if got[0].Username != "" || got[0].Credential != "" {
		t.Errorf("STUN entry should carry no credentials: %+v", got[0])
	}

	if got[1].URLs[0] != "turn:fleet.example.com:3478" || got[1].Username != "fleetuser" || got[1].Credential != "fleetpass" {
		t.Errorf("entry 1 should be the fleet TURN with creds: %+v", got[1])
	}

	if got[2].URLs[0] != "turn:turn.myownmesh.com:3478" || got[2].Username != "guest" {
		t.Errorf("entry 2 should be the joining TURN: %+v", got[2])
	}
}

func TestParseVenueICEServers_NoFleetKeepsConfigOrder(t *testing.T) {
	// With no fleet id, ordering follows config order: joining network first.
	got := parseVenueICEServers(json.RawMessage(sampleMeshConfig), "")
	if len(got) != 3 {
		t.Fatalf("want 3 servers, got %d: %+v", len(got), got)
	}
	if got[0].URLs[0] != "stun:stun.myownmesh.com:3478" {
		t.Errorf("entry 0 should be joining STUN: %+v", got[0])
	}
	// The joining TURN precedes the fleet TURN now.
	if got[1].URLs[0] != "turn:turn.myownmesh.com:3478" {
		t.Errorf("entry 1 should be joining TURN: %+v", got[1])
	}
	if got[2].URLs[0] != "turn:fleet.example.com:3478" {
		t.Errorf("entry 2 should be fleet TURN: %+v", got[2])
	}
}

func TestParseVenueICEServers_SameURLDifferentCredsKept(t *testing.T) {
	// The SAME TURN URL with DIFFERENT credentials must stay distinct.
	const cfg = `{
      "networks": [
        {
          "network_id": "a",
          "turn_servers": [{ "urls": ["turn:t.example:3478"], "username": "u1", "credential": "c1" }]
        },
        {
          "network_id": "b",
          "turn_servers": [{ "urls": ["turn:t.example:3478"], "username": "u2", "credential": "c2" }]
        }
      ]
    }`
	got := parseVenueICEServers(json.RawMessage(cfg), "")
	if len(got) != 2 {
		t.Fatalf("same URL with different creds must not dedup, got %d: %+v", len(got), got)
	}
}

func TestParseVenueICEServers_Garbage(t *testing.T) {
	// Defensive: malformed JSON yields an empty union, never a panic.
	if got := parseVenueICEServers(json.RawMessage("not json"), ""); got != nil {
		t.Errorf("garbage config should parse to nil, got %+v", got)
	}
}
