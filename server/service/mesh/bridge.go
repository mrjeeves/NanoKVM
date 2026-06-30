package mesh

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"NanoKVM-Server/config"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// meshLogFile mirrors the bridge's log lines to a persistent file. The KVM
// server logs to stdout and S95nanokvm launches it backgrounded with no
// redirect, so without this the bridge is invisible on the device.
const meshLogFile = "/var/log/nanokvm-mesh.log"

// meshLogHook writes mesh-tagged logrus entries to meshLogFile, in addition to
// the server's normal (discarded) stdout.
type meshLogHook struct {
	w  *os.File
	tf log.Formatter
}

func (h *meshLogHook) Levels() []log.Level { return log.AllLevels }

func (h *meshLogHook) Fire(e *log.Entry) error {
	if !strings.Contains(strings.ToLower(e.Message), "mesh") {
		return nil
	}
	b, err := h.tf.Format(e)
	if err != nil {
		return err
	}
	_, err = h.w.Write(b)
	return err
}

// appVersion is the NanoKVM application version advertised in presence. The KVM
// build doesn't expose a single canonical version constant to this package, so
// we read /kvmapp/version best-effort at start; this constant is the fallback.
const appVersion = "1.0.0"

// presenceInterval is how often we re-broadcast the NodeProfile. AllMyStuff's
// gossip is event-driven, but a slow heartbeat covers a peer that missed our
// boot-driven advert and keeps us visible.
const presenceInterval = 30 * time.Second

// reconnectDelay is how long the bridge waits before retrying a failed connect.
const reconnectDelay = 5 * time.Second

// Bridge orchestrates the AllMyStuff mesh integration: it owns the daemon
// connections, the persisted state, and the site-tunnel host, and drives the
// presence loop + control handling.
type Bridge struct {
	conf   *config.Config
	engine *gin.Engine
	mesh   config.Mesh

	state *State
	dev   deviceInfo
	boot  uint64

	// events is the events_subscribe connection (server-push stream).
	events *Socket
	// ctl is a separate single-shot request connection for outbound ops, so a
	// blocking request never races the event reader on the same socket.
	ctl *Socket

	sites *siteHost

	mu       sync.Mutex
	nodeID   string
	networks []string // network ids we're subscribed on (NetworkId + any fleet)
	running  bool
}

// NewBridge builds a Bridge from the gin engine and config. It does not connect;
// call Start.
func NewBridge(engine *gin.Engine, conf *config.Config) *Bridge {
	home := conf.Mesh.Home
	st := LoadState(home)
	dev := gatherDeviceInfo()
	port := webPort(conf)

	b := &Bridge{
		conf:   conf,
		engine: engine,
		mesh:   conf.Mesh,
		state:  st,
		dev:    dev,
		boot:   newBootID(),
	}
	// The site host serves only our advertised web port.
	b.sites = newSiteHost(engine, port, b.sendSiteFrame)
	// Re-advertise whenever persisted state changes (claim/attach/detach/fleet).
	st.OnChange(func() { b.reAdvertise() })
	// Mirror mesh logs to a file (the server's stdout is discarded at boot).
	if f, err := os.OpenFile(meshLogFile, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
		log.AddHook(&meshLogHook{w: f, tf: &log.TextFormatter{FullTimestamp: true}})
		log.Infof("mesh: logging to %s", meshLogFile)
	}
	return b
}

// Start runs the bridge: it connects to the daemon (retrying on failure), joins
// the network, subscribes to the planes, advertises capabilities, and runs the
// presence loop. It blocks until stop is closed, so callers run it in a
// goroutine. Connect failures are non-fatal — the daemon may not be up yet.
func (b *Bridge) Start(stop <-chan struct{}) {
	for {
		select {
		case <-stop:
			return
		default:
		}

		if err := b.connectAndRun(stop); err != nil {
			log.Warnf("mesh: bridge run ended: %s; retrying in %s", err, reconnectDelay)
		}

		select {
		case <-stop:
			return
		case <-time.After(reconnectDelay):
		}
	}
}

// connectAndRun does one full connect → serve cycle, returning when the
// connection drops or stop fires.
func (b *Bridge) connectAndRun(stop <-chan struct{}) error {
	sockPath := b.socketPath()

	events, err := Dial(sockPath)
	if err != nil {
		return err
	}
	ctl, err := Dial(sockPath)
	if err != nil {
		_ = events.Close()
		return err
	}

	b.mu.Lock()
	b.events = events
	b.ctl = ctl
	b.running = true
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		_ = events.Close()
		_ = ctl.Close()
	}()

	// Connection-dropped signal: the event reader fires onClose when the stream
	// ends, which we use to break out of the serve loop and reconnect.
	dropped := make(chan struct{})
	var dropOnce sync.Once
	onClose := func() { dropOnce.Do(func() { close(dropped) }) }

	// 1. events_subscribe → capture client_id and start the reader.
	if err := events.Subscribe(b.onChannelInbound, onClose); err != nil {
		return err
	}

	// 2. identity_show → our node id.
	id, err := ctl.IdentityShow()
	if err != nil {
		return err
	}
	b.mu.Lock()
	b.nodeID = id.DeviceID
	b.mu.Unlock()
	log.Infof("mesh: node id %s", id.DeviceID)

	// 3. networks_list → join our network if absent.
	if err := b.ensureNetwork(); err != nil {
		return err
	}

	// 4. Subscribe to the planes + advertise capabilities + first presence.
	if err := b.joinPlanes(b.mesh.NetworkId); err != nil {
		return err
	}

	// Also (re)join + plane-subscribe the fleet network if we already hold a key.
	if key := b.state.FleetKey(); key != "" {
		fleetNet := DeriveFleetNetworkID(key)
		venue := b.fleetVenue()
		b.joinFleetNetwork(fleetNet, b.state.FleetName(), venue)
	}

	b.reAdvertise()

	// 5. Presence loop until the connection drops or we're told to stop.
	ticker := time.NewTicker(presenceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return nil
		case <-dropped:
			return nil
		case <-ticker.C:
			b.broadcastPresence()
		}
	}
}

// socketPath resolves the daemon control socket path. We use mesh.Socket (on
// tmpfs by default) because the daemon's natural default, $Home/daemon.sock, is
// on /data — typically exFAT/FAT, which can't hold a Unix socket (bind ->
// EPERM). The init script pins the daemon's control_socket to this same path.
// Empty mesh.Socket falls back to the daemon's $Home/daemon.sock default.
func (b *Bridge) socketPath() string {
	if b.mesh.Socket != "" {
		return b.mesh.Socket
	}
	return filepath.Join(b.mesh.Home, "daemon.sock")
}

// ensureNetwork joins our configured network if the daemon hasn't already.
func (b *Bridge) ensureNetwork() error {
	nets, err := b.ctl.NetworksList()
	if err != nil {
		return err
	}
	for _, n := range nets {
		if n.NetworkID == b.mesh.NetworkId {
			return nil // already joined
		}
	}
	// The public venue is an OPEN network that auto-approves members, so the KVM
	// and any AllMyStuff app on the same network actually roster each other.
	cfg := b.networkConfig(b.mesh.NetworkId, b.mesh.NetworkId, b.mesh.Label, b.mesh.Relays, nil, "open", true)
	if err := b.ctl.NetworkAdd(cfg); err != nil {
		return err
	}
	log.Infof("mesh: joined network %s", b.mesh.NetworkId)
	return nil
}

// networkConfig builds a NetworkConfig JSON object (config.rs schema). Omitted
// fields (stun_servers/turn_servers) pick up the daemon's public-venue defaults.
// relays empty leaves signaling at its built-in defaults too. venue, when given,
// is a NetworkConfig-shaped JSON object string that overrides the transport.
// networkConfig builds a NetworkConfig JSON object (config.rs schema).
//
//   - kind: "open" (the public venue) auto-accepts members; "closed" (fleets)
//     gates membership behind the signed authority chain. This mirrors how
//     AllMyStuff uses open networks for discovery and closed networks as fleets.
//   - autoApprove: on an open network we add every authenticating peer to the
//     roster automatically (that's what makes the KVM and the app actually see
//     each other). On a closed fleet it's false — the owner controls membership.
func (b *Bridge) networkConfig(id, networkID, label string, relays []string, venue *string, kind string, autoApprove bool) map[string]interface{} {
	if venue != nil && *venue != "" {
		// The owner handed down a full transport config; use it verbatim but
		// pin our local id/network_id/label and governance so it lands correctly.
		var v map[string]interface{}
		if err := json.Unmarshal([]byte(*venue), &v); err == nil && v != nil {
			v["id"] = id
			v["network_id"] = networkID
			if label != "" {
				v["label"] = label
			}
			v["auto_approve"] = autoApprove
			if kind != "" {
				v["kind"] = kind
			}
			return v
		}
		log.Warnf("mesh: fleet venue not valid JSON; falling back to defaults")
	}

	cfg := map[string]interface{}{
		"id":           id,
		"network_id":   networkID,
		"label":        label,
		"auto_approve": autoApprove,
	}
	if kind != "" {
		cfg["kind"] = kind
	}
	if len(relays) > 0 {
		cfg["signaling"] = map[string]interface{}{
			"servers": relays,
		}
	}
	return cfg
}

// joinFleetNetwork joins the fleet's closed network (derived from the fleet key)
// if not already joined, then subscribes the planes on it. Best-effort.
func (b *Bridge) joinFleetNetwork(networkID, name string, venue *string) {
	nets, err := b.ctlNetworksList()
	if err != nil {
		log.Warnf("mesh: networks_list before fleet join: %s", err)
		return
	}
	joined := false
	for _, n := range nets {
		if n.NetworkID == networkID {
			joined = true
			break
		}
	}
	if !joined {
		// A fleet is a CLOSED network: membership is gated by the owner's signed
		// authority chain, not auto-approved.
		cfg := b.networkConfig(networkID, networkID, name, nil, venue, "closed", false)
		if err := b.ctl.NetworkAdd(cfg); err != nil {
			log.Warnf("mesh: join fleet network %s: %s", networkID, err)
			return
		}
		log.Infof("mesh: joined fleet network %s", networkID)
	}
	if err := b.joinPlanes(networkID); err != nil {
		log.Warnf("mesh: subscribe planes on fleet network %s: %s", networkID, err)
	}
}

// fleetVenue returns the persisted fleet venue (transport config string) if any.
func (b *Bridge) fleetVenue() *string {
	snap := b.state.snapshot()
	if snap.FleetVenue == "" {
		return nil
	}
	v := snap.FleetVenue
	return &v
}

// ctlNetworksList is a thin guard so a nil ctl (defensive) doesn't panic.
func (b *Bridge) ctlNetworksList() ([]NetworkSummary, error) {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	return ctl.NetworksList()
}

// joinPlanes subscribes the event-stream client to presence/control/media on a
// network, advertises capabilities, and records the network for presence
// broadcasts. Idempotent: re-subscribing is cheap.
func (b *Bridge) joinPlanes(networkID string) error {
	for _, ch := range []string{ChannelPresence, ChannelControl, ChannelMedia} {
		if err := b.events.ChannelSubscribe(networkID, ch); err != nil {
			return err
		}
	}
	if err := b.advertiseCapabilities(networkID); err != nil {
		return err
	}
	b.mu.Lock()
	found := false
	for _, n := range b.networks {
		if n == networkID {
			found = true
			break
		}
	}
	if !found {
		b.networks = append(b.networks, networkID)
	}
	b.mu.Unlock()
	return nil
}

// advertiseCapabilities sets the network's capability matrix. The daemon's
// CapabilityAdvert is a typed struct — only tags/app_version/max_connections and
// a freeform `extra` survive (de)serialization — so app-specific data (the
// inventory summary + endpoints) MUST be nested under `extra`, mirroring
// AllMyStuff (node/src/mesh.rs advertise_capabilities).
func (b *Bridge) advertiseCapabilities(networkID string) error {
	profile := b.currentProfile()
	tags := []string{CapTagAllMyStuff, FeatureKVM, FeatureSites}
	capabilities := map[string]interface{}{
		"tags":        tags,
		"app_version": b.versionString(),
		"extra": map[string]interface{}{
			"summary":   profile.Summary,
			"endpoints": profile.Capabilities,
		},
	}
	return b.ctl.CapabilitiesSet(networkID, capabilities)
}

// onChannelInbound is the dispatcher for every channel_inbound frame. It routes
// by channel: presence is ignored host-side (we broadcast, we don't consume our
// own roster), control is handled, media carries site frames.
func (b *Bridge) onChannelInbound(ci ChannelInbound) {
	switch ci.Channel {
	case ChannelControl:
		msg, err := DecodeControlMessage(ci.Payload)
		if err != nil {
			log.Debugf("mesh: bad control payload: %s", err)
			return
		}
		b.handleControl(ci.Network, ci.From, msg)
	case ChannelMedia:
		if f, ok := DecodeSiteFrame(ci.Payload); ok {
			b.sites.handleFrame(ci.From, f)
		}
	case ChannelPresence:
		// Other nodes' presence — not needed by a KVM appliance in v1.
	}
}

// sendControlTo sends a ControlMessage point-to-point on CHANNEL_CONTROL.
func (b *Bridge) sendControlTo(network, peer string, msg ControlMessage) error {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	return ctl.ChannelSendTo(network, ChannelControl, peer, msg.Payload())
}

// sendSiteFrame sends one outbound SiteFrame on CHANNEL_MEDIA to a peer. It
// targets the network the peer's route is on; we broadcast across our networks
// to the peer (channel_send_to is addressed by peer, so the correct network's
// send reaches them and others are harmless no-ops).
func (b *Bridge) sendSiteFrame(peer string, frame SiteFrame) error {
	b.mu.Lock()
	ctl := b.ctl
	nets := append([]string(nil), b.networks...)
	b.mu.Unlock()
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	var lastErr error
	for _, n := range nets {
		if err := ctl.ChannelSendTo(n, ChannelMedia, peer, json.RawMessage(payload)); err != nil {
			lastErr = err
		} else {
			return nil // delivered on this network
		}
	}
	return lastErr
}

// broadcastPresence pushes the current NodeProfile on CHANNEL_PRESENCE to every
// network we're on.
func (b *Bridge) broadcastPresence() {
	profile := b.currentProfile()
	payload, err := json.Marshal(profile)
	if err != nil {
		log.Warnf("mesh: marshal presence: %s", err)
		return
	}
	b.mu.Lock()
	ctl := b.ctl
	nets := append([]string(nil), b.networks...)
	running := b.running
	b.mu.Unlock()
	if !running || ctl == nil {
		return
	}
	for _, n := range nets {
		if err := ctl.ChannelSendAll(n, ChannelPresence, json.RawMessage(payload)); err != nil {
			log.Debugf("mesh: broadcast presence on %s: %s", n, err)
		}
	}
}

// reAdvertise re-broadcasts presence after a state change. Safe to call when not
// connected (it no-ops).
func (b *Bridge) reAdvertise() {
	b.mu.Lock()
	running := b.running
	b.mu.Unlock()
	if running {
		b.broadcastPresence()
	}
}

// currentProfile builds the NodeProfile from the latest device info + state.
func (b *Bridge) currentProfile() NodeProfile {
	b.mu.Lock()
	nodeID := b.nodeID
	b.mu.Unlock()
	return buildProfile(nodeID, b.conf, b.dev, b.state, b.versionString(), b.boot)
}

// versionString returns the NanoKVM app version (best-effort file read).
func (b *Bridge) versionString() string {
	if raw, err := readFileTrim("/kvmapp/version"); err == nil && raw != "" {
		return raw
	}
	return appVersion
}
