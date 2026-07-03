package mesh

// The bridge's one HTTP surface: GET /api/mesh/status, the read-only snapshot
// behind the web UI's "Mesh" settings tab. Its headline field is JoiningMesh —
// the per-device network a human joins from AllMyStuff to adopt the device —
// which is exactly why it lives in the web UI (and on the OLED): nothing is
// printed on a box.

import (
	"NanoKVM-Server/middleware"
	"NanoKVM-Server/proto"

	"github.com/gin-gonic/gin"
)

// MeshMembership is one mesh the device is currently joined to.
type MeshMembership struct {
	NetworkID string `json:"networkId"`
	// Fleet marks the owner's fleet mesh (governed by the fleet key).
	Fleet bool `json:"fleet"`
	// Joining marks the device's own joining mesh.
	Joining bool `json:"joining"`
}

// MeshStatus is the /api/mesh/status payload.
type MeshStatus struct {
	Enabled bool `json:"enabled"`
	// Connected reports whether the bridge currently holds a live daemon
	// session (JoiningMesh/NodeID are empty until the first connect).
	Connected     bool             `json:"connected"`
	NodeID        string           `json:"nodeId"`
	Label         string           `json:"label"`
	JoiningMesh   string           `json:"joiningMesh"`
	Claimable     bool             `json:"claimable"`
	Owner         string           `json:"owner"`
	FleetName     string           `json:"fleetName"`
	AttachedTo    string           `json:"attachedTo"`
	AttachedLabel string           `json:"attachedLabel"`
	Meshes        []MeshMembership `json:"meshes"`
}

// RegisterRoutes mounts the mesh API. bridge may be nil (mesh disabled in
// config) — the endpoint then reports enabled:false so the web UI can say so
// instead of showing a broken tab.
func RegisterRoutes(r *gin.Engine, bridge *Bridge) {
	api := r.Group("/api/mesh").Use(middleware.CheckToken())
	api.GET("/status", func(c *gin.Context) {
		var rsp proto.Response
		if bridge == nil {
			rsp.OkRspWithData(c, MeshStatus{Enabled: false, Meshes: []MeshMembership{}})
			return
		}
		rsp.OkRspWithData(c, bridge.StatusSnapshot())
	})
}

// StatusSnapshot assembles the current MeshStatus.
func (b *Bridge) StatusSnapshot() MeshStatus {
	snap := b.state.snapshot()
	b.mu.Lock()
	nodeID := b.nodeID
	joining := b.joiningMesh
	running := b.running
	b.mu.Unlock()

	fleetNet := ""
	if snap.FleetKey != "" {
		fleetNet = DeriveFleetNetworkID(snap.FleetKey)
	}
	nets := b.networksSnapshot()
	meshes := make([]MeshMembership, 0, len(nets))
	for _, n := range nets {
		meshes = append(meshes, MeshMembership{
			NetworkID: n,
			Fleet:     fleetNet != "" && n == fleetNet,
			Joining:   n == joining,
		})
	}

	return MeshStatus{
		Enabled:       true,
		Connected:     running,
		NodeID:        nodeID,
		Label:         b.currentProfile().Label,
		JoiningMesh:   joining,
		Claimable:     snap.Claimable,
		Owner:         snap.Owner,
		FleetName:     snap.FleetName,
		AttachedTo:    snap.AttachedTo,
		AttachedLabel: snap.AttachedLabel,
		Meshes:        meshes,
	}
}
