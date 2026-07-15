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
	log "github.com/sirupsen/logrus"
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
	// PublicClaims mirrors config.Mesh.PublicClaims — READ-ONLY here by
	// design: the policy is settable only in the device's config file, so
	// no remote system (including a mesh-tunneled browser session) can
	// open the device to public claiming.
	PublicClaims bool `json:"publicClaims"`
	// ClaimCode is the device's current claim code in display form
	// (xxxx-xxxx-…) — the WAN rendezvous secret for AllMyStuff's "Claim a
	// remote device" flow. Populated only while the device is claimable
	// with public claims enabled; empty otherwise.
	ClaimCode string `json:"claimCode,omitempty"`
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
	// Rotate the claim code. Deliberately the ONLY claim mutation exposed
	// over HTTP: rotation can only invalidate an in-flight code (minting a
	// fresh one), never enable claiming — enabling lives in server.yaml
	// alone.
	api.POST("/claim/code/rotate", func(c *gin.Context) {
		var rsp proto.Response
		if bridge == nil {
			rsp.ErrRsp(c, -1, "mesh disabled")
			return
		}
		bridge.RotateClaimCode()
		rsp.OkRspWithData(c, bridge.StatusSnapshot())
	})

	// Reset the device's mesh ownership back to claim mode — the recovery path
	// for a device still showing claimed when the owner-side unclaim in
	// AllMyStuff never reached it. LOCAL ONLY: rejected over the mesh "sites"
	// tunnel, mirroring the device-local claim policy (a mesh viewer must not be
	// able to reset the box out from under its owner). Runs the same teardown as
	// an owner Release, then reports the now-claimable snapshot.
	api.POST("/unclaim", func(c *gin.Context) {
		var rsp proto.Response
		if bridge == nil {
			rsp.ErrRsp(c, -1, "mesh disabled")
			return
		}
		if middleware.IsMeshAuthed(c.Request) {
			rsp.ErrRsp(c, -1, "reset is only allowed from the local network, not over the mesh")
			return
		}
		bridge.Unclaim()
		rsp.OkRspWithData(c, bridge.StatusSnapshot())
	})

	// CEC "hand raise" (Ask for help). GET reports current state; the three
	// POSTs raise / lower / toggle the hand on the cecsupport-clients mesh
	// (see cec.go). The physical user button drives the same bridge.ToggleHand
	// in-process; these give the web UI (and scripts) the same control.
	api.GET("/help", func(c *gin.Context) {
		var rsp proto.Response
		if bridge == nil {
			rsp.OkRspWithData(c, HelpStatus{Enabled: false})
			return
		}
		rsp.OkRspWithData(c, bridge.HelpStatus())
	})
	api.POST("/help/raise", func(c *gin.Context) { handleHelp(c, bridge, helpRaise) })
	api.POST("/help/lower", func(c *gin.Context) { handleHelp(c, bridge, helpLower) })
	api.POST("/help/toggle", func(c *gin.Context) { handleHelp(c, bridge, helpToggle) })
}

// HelpStatus is the /api/mesh/help payload: whether a hand is up and this
// device's dialable support number.
type HelpStatus struct {
	Enabled   bool   `json:"enabled"`
	Asking    bool   `json:"asking"`
	SupportID string `json:"supportId"`
}

// HelpStatus assembles the current hand-raise snapshot.
func (b *Bridge) HelpStatus() HelpStatus {
	return HelpStatus{
		Enabled:   true,
		Asking:    b.HelpAsking(),
		SupportID: b.SupportID(),
	}
}

type helpAction int

const (
	helpRaise helpAction = iota
	helpLower
	helpToggle
)

// handleHelp runs a hand-raise action and returns the resulting HelpStatus, so
// a caller (web UI or button) always learns the new state in one round-trip.
func handleHelp(c *gin.Context, bridge *Bridge, action helpAction) {
	var rsp proto.Response
	if bridge == nil {
		rsp.ErrRsp(c, -1, "mesh disabled")
		return
	}
	var err error
	switch action {
	case helpRaise:
		err = bridge.RaiseHand()
	case helpLower:
		err = bridge.LowerHand()
	case helpToggle:
		_, err = bridge.ToggleHand()
	}
	if err != nil {
		log.Errorf("mesh: CEC hand-raise action failed: %s", err)
		rsp.ErrRsp(c, -2, err.Error())
		return
	}
	rsp.OkRspWithData(c, bridge.HelpStatus())
}

// RotateClaimCode discards the current claim code and, when the device is
// claimable with public claims on, re-establishes the rendezvous under a
// fresh one.
func (b *Bridge) RotateClaimCode() {
	old := b.state.ClaimCode()
	b.state.RotateClaimCode()
	if old == "" {
		return
	}
	b.membershipMu.Lock()
	defer b.membershipMu.Unlock()
	oldNet := claimCodeNetworkID(old)
	if err := b.networkRemove(oldNet); err != nil {
		log.Warnf("mesh: leave rotated claim rendezvous %s: %s", oldNet, err)
	}
	if b.state.Claimable() && b.publicClaimsAllowed() {
		code := b.state.EnsureClaimCode()
		codeNet := claimCodeNetworkID(code)
		cfg := b.networkConfig(codeNet, codeNet, "Remote claiming", b.mesh.Relays, nil, "open", true)
		if err := b.networkAdd(cfg); err != nil {
			log.Warnf("mesh: rejoin claim rendezvous: %s", err)
			return
		}
		if err := b.joinPlanes(codeNet); err != nil {
			log.Warnf("mesh: join planes on %s: %s", codeNet, err)
		}
		log.Infof("mesh: claim code rotated — now %s", formatClaimCode(code))
	}
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

	claimCode := ""
	if snap.Claimable && b.publicClaimsAllowed() && snap.ClaimCode != "" {
		claimCode = formatClaimCode(snap.ClaimCode)
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
		PublicClaims:  b.publicClaimsAllowed(),
		ClaimCode:     claimCode,
	}
}
