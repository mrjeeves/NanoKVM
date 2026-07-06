package mesh

import (
	"encoding/json"
	"sort"
	"strings"

	"NanoKVM-Server/service/iceservers"

	log "github.com/sirupsen/logrus"
)

// venue.go publishes the "venue" ICE-server union: the deduplicated set of
// STUN/TURN servers of EVERY mesh this KVM is on, fleet-venue first. The KVM's
// own web UI (reached via AllMyStuff's sites proxy) reads this union when it
// builds a browser's WebRTC ICE configuration, so a remote viewer that shares a
// mesh with the KVM (e.g. the fleet) has a reachable relay.
//
// Only the bridge can produce it — the STUN/TURN lists live in the myownmesh
// daemon's on-disk config, reachable via the config_show control op — so the
// bridge pushes the result into the standalone iceservers package that the
// webrtc stream package reads.

// venueMeshConfig is a DEFENSIVE subset of myownmesh's MeshConfig: only the
// fields the venue union needs. Everything else in the daemon's config is
// ignored so a schema addition on the daemon side never breaks parsing here.
type venueMeshConfig struct {
	Networks []venueNetwork `json:"networks"`
}

type venueNetwork struct {
	NetworkID   string      `json:"network_id"`
	StunServers []venueStun `json:"stun_servers"`
	TurnServers []venueTurn `json:"turn_servers"`
}

type venueStun struct {
	URLs []string `json:"urls"`
}

type venueTurn struct {
	URLs []string `json:"urls"`
	// username / credential are Option<String> on the daemon side: a JSON
	// null or an absent field decodes to "" here, which is exactly the
	// "no credential" case.
	Username   string `json:"username"`
	Credential string `json:"credential"`
}

// publishVenueICEServers fetches the daemon's config, builds the fleet-first,
// deduplicated venue ICE-server union, and stores it for the webrtc package.
// Best-effort: a missing ctl or a failed config_show logs at debug and returns
// without disturbing the bridge — the venue union simply stays as it was.
func (b *Bridge) publishVenueICEServers() {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl == nil {
		log.Debugf("mesh: venue ICE publish skipped — bridge not connected")
		return
	}
	raw, err := ctl.ConfigShow()
	if err != nil {
		log.Debugf("mesh: venue ICE publish — config_show failed: %s", err)
		return
	}

	fleetNet := ""
	if key := b.state.FleetKey(); key != "" {
		fleetNet = DeriveFleetNetworkID(key)
	}

	list := parseVenueICEServers(raw, fleetNet)
	iceservers.Set(list)
	log.Debugf("mesh: published %d venue ICE server(s) (fleet=%q)", len(list), fleetNet)
}

// parseVenueICEServers turns a raw MeshConfig JSON into the venue ICE-server
// union: the fleet network's servers first (when fleetNet is set and present),
// then every other network in config order, with duplicates collapsed by a
// stable key (sorted URLs + username + credential). Pure and daemon-free so it
// is unit-testable.
func parseVenueICEServers(raw json.RawMessage, fleetNet string) []iceservers.Server {
	var cfg venueMeshConfig
	if err := json.Unmarshal(raw, &cfg); err != nil {
		log.Debugf("mesh: venue ICE parse failed: %s", err)
		return nil
	}

	// Order the networks fleet-venue-first, then the rest in config order.
	ordered := make([]venueNetwork, 0, len(cfg.Networks))
	if fleetNet != "" {
		for _, n := range cfg.Networks {
			if n.NetworkID == fleetNet {
				ordered = append(ordered, n)
			}
		}
	}
	for _, n := range cfg.Networks {
		if fleetNet != "" && n.NetworkID == fleetNet {
			continue
		}
		ordered = append(ordered, n)
	}

	var out []iceservers.Server
	seen := map[string]bool{}
	add := func(s iceservers.Server) {
		if len(s.URLs) == 0 {
			return
		}
		key := venueServerKey(s)
		if seen[key] {
			return
		}
		seen[key] = true
		out = append(out, s)
	}
	for _, n := range ordered {
		for _, st := range n.StunServers {
			add(iceservers.Server{URLs: st.URLs})
		}
		for _, tn := range n.TurnServers {
			add(iceservers.Server{URLs: tn.URLs, Username: tn.Username, Credential: tn.Credential})
		}
	}
	return out
}

// venueServerKey is the stable dedup key for one ICE server: its URLs sorted
// (order-insensitive) joined with its credentials. Two networks that reference
// the SAME server (identical URLs + creds) collapse to one entry; the same URL
// with DIFFERENT credentials stays distinct.
func venueServerKey(s iceservers.Server) string {
	urls := make([]string, len(s.URLs))
	copy(urls, s.URLs)
	sort.Strings(urls)
	return strings.Join(urls, ",") + "|" + s.Username + "|" + s.Credential
}
