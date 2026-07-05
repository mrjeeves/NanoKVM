package mesh

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
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

	// membershipMu serializes every multi-step network-membership move (the
	// connect-time ensure, mesh add/remove commands, unclaim) so two of them
	// can't interleave their list/add/remove sequences.
	membershipMu sync.Mutex

	mu          sync.Mutex
	nodeID      string
	joiningMesh string   // this device's joining mesh (derived, or config-pinned)
	networks    []string // network ids we're subscribed on (joining/fleet/owner-added)
	running     bool
	greetedAt   map[string]time.Time // peer id → last time we sent it our presence
	// peerLabels caches peer display labels gleaned from presence adverts,
	// keyed by canonical pubkey part — the fallback that names this device
	// "KVM-<target>" when an attach (or the claim auto-attach) arrives without
	// a label.
	peerLabels map[string]string
	// identityLabel is the last label pushed to the daemon identity, so the
	// sync is change-driven rather than rewriting the anchor every presence.
	identityLabel string
	// fleetRoster caches the fleet network's approved-peer roster as canonical
	// pubkey parts — the co-fleet half of senderMayControl. Refreshed on fleet
	// join and each presence tick; a cache (never an inline roster_list) so
	// authorization checks on the event-stream goroutine stay non-blocking.
	fleetRoster map[string]struct{}
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
	// A frame on a dead route gets a Reject back: after a bridge restart only
	// we can tell a viewer its accepted tunnel no longer exists (AllMyStuff
	// deliberately never NACKs the site plane itself).
	b.sites.nack = b.rejectRouteTo
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
	b.greetedAt = nil // fresh session: re-greet every peer that announces
	b.mu.Unlock()

	defer func() {
		b.mu.Lock()
		b.running = false
		b.mu.Unlock()
		_ = events.Close()
		_ = ctl.Close()
		// The daemon connection carried every tunnel's frames; without it each
		// tunneled browser connection's serveHTTP goroutine would block in
		// meshConn.Read forever (no deadline, no Close frame ever arriving),
		// leaking a goroutine + conn per reconnect. Routes die with the peer
		// links anyway — the viewer re-offers on the fresh session, and a
		// stray frame on the old route id gets a Reject.
		b.sites.tearDownAll()
	}()

	// Connection-dropped signal: the event reader fires onClose when the stream
	// ends, which we use to break out of the serve loop and reconnect.
	dropped := make(chan struct{})
	var dropOnce sync.Once
	onClose := func() { dropOnce.Do(func() { close(dropped) }) }

	// A fatal error on the ctl request stream (a read timeout desyncs
	// response correlation — see Socket.request) must drop the whole session,
	// not just wedge ctl: route it into the same drop signal the events stream
	// uses so connectAndRun returns and Start re-establishes cleanly.
	ctl.SetOnFatal(onClose)

	// 1. events_subscribe → capture client_id and start the reader. onMeshEvent
	// consumes the engine event stream (observe-only — logs network changes).
	if err := events.Subscribe(b.onChannelInbound, b.onMeshEvent, onClose); err != nil {
		return err
	}

	// 2. identity_show → our node id + this device's joining mesh (derived
	// from the identity, unless the config pins a custom one).
	id, err := ctl.IdentityShow()
	if err != nil {
		return err
	}
	joining := b.mesh.NetworkId
	if joining == "" {
		joining = DeriveJoiningMeshID(id.DeviceID)
	}
	b.mu.Lock()
	b.nodeID = id.DeviceID
	b.joiningMesh = joining
	b.networks = nil // rebuilt below from the daemon's actual list
	b.identityLabel = id.Label
	b.mu.Unlock()
	log.Infof("mesh: node id %s, joining mesh %s", id.DeviceID, joining)
	// Surface the joining mesh where a human standing at the hardware can
	// read it: the OLED app polls this file into the screen's IP rotation.
	writeJoiningMeshFile(joining)

	// 3. Reconcile network membership with the claim state (unclaimed → the
	// joining mesh; claimed → the fleet mesh; owner-added meshes kept) and
	// subscribe the planes + advertise capabilities on every one of them.
	if err := b.ensureMemberships(); err != nil {
		return err
	}

	// 4. Keep the daemon identity label in step with the display label
	// (KVM-<target> when attached, the brand name otherwise).
	b.syncIdentityLabel()

	b.reAdvertise()
	// The handshake is done — everything below is steady-state. Without this
	// line a healthy bridge is silent at INFO until the first peer greeting,
	// which makes `just verify` right after boot unreadable: no news must be
	// distinguishable from a hang.
	log.Infof("mesh: up — advertising %q on %v (claimable=%v, owner=%q), presence every %s",
		b.currentProfile().Label, b.networksSnapshot(), b.state.Claimable(), b.state.Owner(), presenceInterval)

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
			// Keep the co-fleet authorization roster warm. Here (this
			// goroutine, off the event stream) so senderMayControl never
			// blocks on a daemon round-trip; a co-member added mid-session
			// gains control within one presence interval.
			b.refreshFleetRoster()
			// Keep the daemon identity label in step with the live advert
			// label — the attached node's presence (which feeds KVM-<label>)
			// may have landed after the attach, or the target may have
			// renamed. Change-guarded, so it's a no-op once converged.
			b.syncIdentityLabel()
		}
	}
}

// onMeshEvent consumes the daemon's engine event stream. It's observe-only: a
// network-change diag (category "network", emitted when this device's interface
// set moves — e.g. the Virtual Network toggle bringing usb0 up) is LOGGED for
// diagnostics but does NOT drive a bridge re-establish.
//
// Reacting to it used to force a full reconnect, which was actively harmful: the
// daemon already handles a network change itself (it restarts ICE on every peer
// and recovers in ~10 s), and the bridge's channel subscriptions ride the LOCAL
// daemon socket — unaffected by the mesh network moving — so they survive it.
// Re-subscribing/re-advertising on top of the daemon's in-flight ICE restart
// just piled load onto its stalled single-core engine and turned a brief blip
// into a minute-long reconnect loop. The bridge now simply blocks through the
// stall (see daemonReadTimeout) and resumes; site routes re-map on demand when a
// viewer's next frame hits the reset route (the KVM NACKs, the app re-offers).
func (b *Bridge) onMeshEvent(ev MeshEvent) {
	if ev.EventKind == "diag" && ev.Category == "network" {
		log.Infof("mesh: daemon reported a network change on %s (%s)", ev.NetworkID, ev.Message)
	}
}

// refreshFleetRoster snapshots the fleet network's approved-peer roster into
// the senderMayControl cache. No fleet key → empty cache (owner-only).
func (b *Bridge) refreshFleetRoster() {
	key := b.state.FleetKey()
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if key == "" || ctl == nil {
		b.mu.Lock()
		b.fleetRoster = nil
		b.mu.Unlock()
		return
	}
	entries, err := ctl.RosterList(DeriveFleetNetworkID(key))
	if err != nil {
		// Keep the previous snapshot: a transient daemon stall must not
		// revoke a co-member's working controls.
		log.Debugf("mesh: fleet roster refresh: %s", err)
		return
	}
	roster := make(map[string]struct{}, len(entries))
	for _, e := range entries {
		roster[pubkeyPart(e.DeviceID)] = struct{}{}
	}
	b.mu.Lock()
	b.fleetRoster = roster
	b.mu.Unlock()
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

// localClaimMesh is the well-known LAN claim rendezvous every AllMyStuff node
// always sits on: signaling is mDNS-only (strategy "none"), so it never
// touches relays, STUN, or TURN — and it needs no wall clock, which covers
// the KVM's pre-NTP boot window. A claimable KVM joins it so it simply
// APPEARS in the claim sheet of any AllMyStuff machine on the same LAN — no
// reading the id off the OLED, no adding a mesh. FROZEN: mirrors
// allmystuff-protocol's LOCAL_CLAIM_NETWORK_ID.
const localClaimMesh = "allmystuff-local-claim-v1"

// legacySharedMesh is the retired pre-release default every KVM used to share.
// A device that still holds it is migrated off on connect: each KVM now waits
// for adoption alone on its own joining mesh.
const legacySharedMesh = "cec-backend-client-mesh"

// joiningMeshID returns this device's joining mesh (set during connect).
func (b *Bridge) joiningMeshID() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.joiningMesh
}

// networksSnapshot returns a sorted copy of the network ids we're subscribed
// on — the membership list the KVM advertises.
func (b *Bridge) networksSnapshot() []string {
	b.mu.Lock()
	nets := append([]string(nil), b.networks...)
	b.mu.Unlock()
	sort.Strings(nets)
	return nets
}

// dropNetwork prunes a network id from the subscribed list.
func (b *Bridge) dropNetwork(networkID string) {
	b.mu.Lock()
	kept := b.networks[:0]
	for _, n := range b.networks {
		if n != networkID {
			kept = append(kept, n)
		}
	}
	b.networks = kept
	b.mu.Unlock()
}

// isFleetMesh reports whether networkID is this device's fleet mesh — the one
// membership no KvmControl may touch (it's governed by the fleet key).
func (b *Bridge) isFleetMesh(networkID string) bool {
	key := b.state.FleetKey()
	return key != "" && DeriveFleetNetworkID(key) == networkID
}

// normalizeNetworkID mirrors myownmesh's normalize_network_id: trimmed,
// lowercased, 3–64 chars of [a-z0-9-_]. The daemon would reject anything else
// anyway; checking locally turns garbage into one log line instead of a
// doomed round-trip.
func normalizeNetworkID(input string) (string, error) {
	id := strings.ToLower(strings.TrimSpace(input))
	if len(id) < 3 || len(id) > 64 {
		return "", fmt.Errorf("network id must be 3-64 chars, got %d", len(id))
	}
	for i := 0; i < len(id); i++ {
		c := id[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-' || c == '_') {
			return "", fmt.Errorf("network id may only contain a-z, 0-9, '-' and '_'")
		}
	}
	return id, nil
}

// ensureMemberships reconciles the daemon's network list with the claim
// state: the legacy shared mesh is retired, an unclaimed device sits on its
// joining mesh, a claimed one on its fleet mesh (never the joining mesh —
// unclaim is what returns it there), and the planes are subscribed on every
// remaining network, owner-added meshes included. A joining-mesh failure is
// returned (aborting the handshake so Start retries — it's the claim path);
// everything else is best-effort with a log line.
func (b *Bridge) ensureMemberships() error {
	b.membershipMu.Lock()
	defer b.membershipMu.Unlock()

	joining := b.joiningMeshID()
	nets, err := b.ctlNetworksList()
	if err != nil {
		return err
	}
	present := map[string]bool{}
	for _, n := range nets {
		present[n.NetworkID] = true
	}

	if present[legacySharedMesh] && legacySharedMesh != joining {
		if err := b.networkRemove(legacySharedMesh); err != nil {
			log.Warnf("mesh: retire legacy shared mesh: %s", err)
		} else {
			log.Infof("mesh: retired legacy shared mesh %s", legacySharedMesh)
			delete(present, legacySharedMesh)
		}
	}

	// The joining mesh's signaling follows the public-claims policy, and the
	// daemon persists a network's config across restarts — so when the
	// recorded policy differs from the configured one (or was never
	// recorded, i.e. the mesh predates the policy), re-join to apply it.
	public := b.publicClaimsAllowed()
	if present[joining] {
		if last := b.state.JoiningPublic(); last == nil || *last != public {
			if err := b.networkRemove(joining); err != nil {
				log.Warnf("mesh: re-join joining mesh for signaling policy: %s", err)
			} else {
				log.Infof("mesh: re-joining %s (public claims now %v)", joining, public)
				delete(present, joining)
			}
		}
	}

	// Only a device that actually holds a fleet key has somewhere else to be:
	// the fleet mesh covers it, so it may leave the auto-approve joining mesh.
	// A CLAIMED-BUT-KEYLESS device (owner recorded, but the fleet-key handoff
	// was lost or hasn't arrived) must NOT leave — it would strand itself on
	// zero meshes, permanently unreachable, with no key to derive a fleet mesh
	// from. It stays on the joining mesh (reachable) so the owner's presence
	// can re-hand the key; the deferred leave then runs once the fleet mesh
	// converges.
	hasFleet := b.state.FleetKey() != ""
	if hasFleet {
		// A fleet member doesn't linger on its claim rendezvous meshes.
		// This also covers a crash between fleet adoption and the deferred
		// joining-mesh leave.
		for _, id := range []string{joining, localClaimMesh} {
			if !present[id] {
				continue
			}
			if err := b.networkRemove(id); err != nil {
				log.Warnf("mesh: leave claim mesh %s: %s", id, err)
			} else {
				log.Infof("mesh: left claim mesh %s (fleet mesh carries us)", id)
				delete(present, id)
			}
		}
		if code := b.state.ClaimCode(); code != "" {
			delete(present, claimCodeNetworkID(code))
			b.retireClaimCodeMesh()
		}
	} else if b.state.Owner() != "" {
		// Claimed but keyless: keep every claim rendezvous (reachable) and
		// don't shed — the claim is mid-flight, not finished; the Claimed
		// reply and the fleet-key handoff ride whichever mesh carried the
		// claim.
		if !present[joining] {
			if err := b.networkAdd(b.joiningMeshConfig(joining)); err != nil {
				return err
			}
			present[joining] = true
			b.state.SetJoiningPublic(public)
			log.Infof("mesh: on joining mesh %s (claimed, awaiting fleet key)", joining)
		}
	} else {
		// Unclaimed: the claim rendezvous meshes are the ONLY legitimate
		// memberships — owner-added meshes require an owner and a fleet mesh
		// requires a key, so anything else here is leftovers from an unclaim
		// that died mid-teardown (or an old fleet). Shedding them on connect
		// makes the unclaim reset convergent no matter where it was
		// interrupted.
		keep := map[string]bool{joining: true, localClaimMesh: true}
		codeNet := ""
		if public {
			codeNet = claimCodeNetworkID(b.state.EnsureClaimCode())
			keep[codeNet] = true
		}
		for id := range present {
			if keep[id] {
				continue
			}
			if err := b.networkRemove(id); err != nil {
				log.Warnf("mesh: shed stale mesh %s: %s", id, err)
				continue
			}
			log.Infof("mesh: shed stale mesh %s (unclaimed)", id)
			delete(present, id)
		}
		if !present[joining] {
			// The adoption venue: kind "open" plus auto_approve true. Note the
			// mechanism — it is the auto_approve FLAG that admits
			// authenticating peers (the daemon's handshake consults only
			// cfg.auto_approve || rostered, and the flag defaults to FALSE);
			// the "open" kind gates roster-gossip trust and governance, never
			// admission. Drop the flag and every peer wedges at
			// PendingApproval with no one on this headless box to approve
			// them.
			if err := b.networkAdd(b.joiningMeshConfig(joining)); err != nil {
				return err
			}
			present[joining] = true
			log.Infof("mesh: joined joining mesh %s", joining)
		}
		b.state.SetJoiningPublic(public)
		// The well-known LAN claim rendezvous: every AllMyStuff node always
		// sits on it (mDNS-only), so a claimable KVM on the same LAN simply
		// appears in its claim sheet — no id transcription, no mesh joining.
		if !present[localClaimMesh] {
			if err := b.networkAdd(b.localClaimConfig()); err != nil {
				log.Warnf("mesh: join LAN claim mesh: %s", err)
			} else {
				present[localClaimMesh] = true
				log.Infof("mesh: joined LAN claim mesh %s", localClaimMesh)
			}
		}
		// The claim-code rendezvous (public claims on): the random,
		// rotating WAN meeting point behind AllMyStuff's "Claim a remote
		// device" flow. The code is surfaced on the device's web page.
		if codeNet != "" {
			if !present[codeNet] {
				cfg := b.networkConfig(codeNet, codeNet, "Remote claiming", b.mesh.Relays, nil, "open", true)
				if err := b.networkAdd(cfg); err != nil {
					log.Warnf("mesh: join claim rendezvous: %s", err)
				} else {
					present[codeNet] = true
				}
			}
			log.Infof("mesh: remote claiming enabled — claim code %s (AllMyStuff → Fleet → \"Claim a remote device\")",
				formatClaimCode(b.state.ClaimCode()))
		}
	}

	for id := range present {
		if err := b.joinPlanes(id); err != nil {
			if id == joining {
				return err
			}
			log.Warnf("mesh: join planes on %s: %s", id, err)
		}
	}

	// (Re)join + plane-subscribe the fleet network if we already hold a key.
	if key := b.state.FleetKey(); key != "" {
		fleetNet := DeriveFleetNetworkID(key)
		venue := b.fleetVenue()
		b.joinFleetNetwork(fleetNet, b.state.FleetName(), venue)
		b.refreshFleetRoster()
	}
	return nil
}

// applyMeshAdd joins a fleet-owner-requested mesh and subscribes its planes.
// Runs off the event goroutine (network_add attaches signaling — seconds, not
// millis). The re-advertised membership list is the confirmation; a refusal
// is a log line (the protocol has no KVM-control ack, exactly like attach).
func (b *Bridge) applyMeshAdd(networkID string) {
	id, err := normalizeNetworkID(networkID)
	if err != nil {
		log.Infof("mesh: mesh_add %q refused: %s", networkID, err)
		return
	}
	if b.isFleetMesh(id) {
		log.Infof("mesh: mesh_add %s refused: the fleet mesh is governed by the fleet key", id)
		return
	}
	b.membershipMu.Lock()
	defer b.membershipMu.Unlock()
	nets, err := b.ctlNetworksList()
	if err != nil {
		log.Warnf("mesh: mesh_add %s: %s", id, err)
		return
	}
	joined := false
	for _, n := range nets {
		if n.NetworkID == id {
			joined = true
			break
		}
	}
	if !joined {
		// Owner-added meshes are ordinary open venues (see ensureMemberships
		// on why auto_approve is what admits peers). Control stays gated by
		// senderMayControl regardless of who else is on the mesh.
		cfg := b.networkConfig(id, id, "", b.mesh.Relays, nil, "open", true)
		if err := b.networkAdd(cfg); err != nil {
			log.Warnf("mesh: mesh_add %s: %s", id, err)
			return
		}
		log.Infof("mesh: joined mesh %s (owner request)", id)
	}
	if err := b.joinPlanes(id); err != nil {
		log.Warnf("mesh: join planes on %s: %s", id, err)
		return
	}
	b.reAdvertise()
}

// applyMeshRemove leaves a fleet-owner-named mesh (never the fleet mesh) and
// purges its local state. Runs off the event goroutine; the re-advertised
// membership list is the confirmation.
func (b *Bridge) applyMeshRemove(networkID string) {
	id, err := normalizeNetworkID(networkID)
	if err != nil {
		log.Infof("mesh: mesh_remove %q refused: %s", networkID, err)
		return
	}
	if b.isFleetMesh(id) {
		log.Infof("mesh: mesh_remove %s refused: leaving the fleet is Release + governance, not a membership edit", id)
		return
	}
	b.membershipMu.Lock()
	defer b.membershipMu.Unlock()
	nets, err := b.ctlNetworksList()
	if err != nil {
		log.Warnf("mesh: mesh_remove %s: %s", id, err)
		return
	}
	joined := false
	for _, n := range nets {
		if n.NetworkID == id {
			joined = true
			break
		}
	}
	if !joined {
		log.Infof("mesh: mesh_remove %s: not joined (no-op)", id)
		return
	}
	if err := b.networkRemove(id); err != nil {
		log.Warnf("mesh: mesh_remove %s: %s", id, err)
		return
	}
	b.dropNetwork(id)
	log.Infof("mesh: left mesh %s (owner request)", id)
	b.reAdvertise()
}

// unclaim executes an owner-ordered Release: forget owner, attachment, and
// fleet credential; leave every mesh; return to the joining mesh in claim
// mode. Runs off the event goroutine; serialized with every other membership
// move. The device simply disappears from the old owner's graph (their
// governance eviction already cleaned their side) and reappears claimable on
// its joining mesh — exactly where its screen says it will be.
func (b *Bridge) unclaim(from string) {
	if !b.state.Unclaim() {
		return
	}
	// The state notifier just re-advertised the claimable profile on the
	// still-joined meshes (the old fleet included) — a cooperative goodbye —
	// and now the memberships are rebuilt around the joining mesh.
	log.Infof("mesh: released by %s — resetting to joining mesh + claim mode", from)

	b.membershipMu.Lock()
	defer b.membershipMu.Unlock()

	joining := b.joiningMeshID()
	nets, err := b.ctlNetworksList()
	if err != nil {
		log.Warnf("mesh: unclaim networks_list: %s", err)
		return
	}
	// Return to the joining mesh BEFORE dropping anything, so the device is
	// never on zero meshes if the teardown dies midway. If the rejoin fails we
	// keep the old meshes rather than shedding into nothing — a reachable
	// (if stale) device beats a dark one; the next connect's ensureMemberships
	// retries from the persisted claimable state.
	onJoining := false
	for _, n := range nets {
		if n.NetworkID == joining {
			onJoining = true
			break
		}
	}
	if !onJoining {
		if err := b.networkAdd(b.joiningMeshConfig(joining)); err != nil {
			log.Warnf("mesh: unclaim rejoin %s failed — keeping current meshes, will retry on reconnect: %s", joining, err)
			b.syncIdentityLabel()
			b.reAdvertise()
			return
		}
		b.state.SetJoiningPublic(b.publicClaimsAllowed())
	}
	if err := b.joinPlanes(joining); err != nil {
		log.Warnf("mesh: unclaim join planes on %s failed — keeping current meshes: %s", joining, err)
		b.syncIdentityLabel()
		b.reAdvertise()
		return
	}
	// Back on the LAN claim rendezvous too, so the reset device reappears in
	// same-LAN claim sheets right away (best-effort — the next connect's
	// ensureMemberships converges it regardless).
	onLocalClaim := false
	for _, n := range nets {
		if n.NetworkID == localClaimMesh {
			onLocalClaim = true
			break
		}
	}
	if !onLocalClaim {
		if err := b.networkAdd(b.localClaimConfig()); err != nil {
			log.Warnf("mesh: unclaim rejoin LAN claim mesh: %s", err)
		} else if err := b.joinPlanes(localClaimMesh); err != nil {
			log.Warnf("mesh: unclaim join planes on %s: %s", localClaimMesh, err)
		}
	} else if err := b.joinPlanes(localClaimMesh); err != nil {
		log.Warnf("mesh: unclaim join planes on %s: %s", localClaimMesh, err)
	}
	for _, n := range nets {
		if n.NetworkID == joining || n.NetworkID == localClaimMesh {
			continue
		}
		if err := b.networkRemove(n.NetworkID); err != nil {
			log.Warnf("mesh: unclaim leave %s: %s", n.NetworkID, err)
			continue
		}
		b.dropNetwork(n.NetworkID)
	}
	b.mu.Lock()
	b.fleetRoster = nil
	b.mu.Unlock()
	b.syncIdentityLabel()
	b.reAdvertise()
	log.Infof("mesh: reset complete — claimable on %s", joining)
}

// leaveJoiningMeshAfterAdoption leaves the joining mesh once the fleet mesh is
// really carrying us — the fleet roster has CONVERGED — so the device is never
// left dark. If the fleet mesh never converges within the bounded wait we
// KEEP the joining mesh: reachable on two meshes beats stranded on a fleet
// mesh that isn't working (the old behavior stayed on the shared mesh forever
// too, so this is no regression). The tail of the claim conversation, which
// rode the joining mesh, is thus never cut mid-sentence either.
func (b *Bridge) leaveJoiningMeshAfterAdoption() {
	joining := b.joiningMeshID()
	if joining == "" || b.isFleetMesh(joining) {
		return
	}
	converged := false
	for i := 0; i < 12; i++ {
		time.Sleep(5 * time.Second)
		if b.state.FleetKey() == "" {
			return // released mid-wait — the joining mesh is home again
		}
		b.refreshFleetRoster()
		b.mu.Lock()
		converged = len(b.fleetRoster) > 0
		b.mu.Unlock()
		if converged {
			break
		}
	}
	if !converged {
		// Never proven reachable on the fleet mesh — stay put. The next
		// adoption (or a manual reconnect) retries; a dark device is worse
		// than one lingering on its own auto-approve venue.
		log.Warnf("mesh: fleet roster never converged — keeping joining mesh %s for reachability", joining)
		return
	}
	b.membershipMu.Lock()
	defer b.membershipMu.Unlock()
	// Re-check under the lock: an unclaim (which clears the key before taking
	// this same lock, then re-adds the joining mesh) may have run while we
	// waited. Leaving now would strand the just-reset device on zero meshes.
	if b.state.FleetKey() == "" {
		return
	}
	nets, err := b.ctlNetworksList()
	if err != nil {
		return // the next connect's ensureMemberships finishes the job
	}
	// Every claim rendezvous has done its job: the joining mesh, the LAN
	// claim mesh, and (via retire below) the claim-code rendezvous.
	left := false
	for _, n := range nets {
		if n.NetworkID != joining && n.NetworkID != localClaimMesh {
			continue
		}
		if err := b.networkRemove(n.NetworkID); err != nil {
			log.Warnf("mesh: leave claim mesh %s: %s", n.NetworkID, err)
			continue
		}
		b.dropNetwork(n.NetworkID)
		log.Infof("mesh: left claim mesh %s (adopted into a fleet)", n.NetworkID)
		left = true
	}
	b.retireClaimCodeMesh()
	if left {
		b.reAdvertise()
	}
}

// networkConfig builds a NetworkConfig JSON object (config.rs schema). Omitted
// fields (stun_servers/turn_servers) pick up the daemon's public-venue defaults.
// relays empty leaves signaling at its built-in defaults too. venue, when given,
// is a NetworkConfig-shaped JSON object string that overrides the transport.
//
//   - autoApprove is what ADMITS peers: the daemon's handshake approves an
//     authenticating peer iff cfg.auto_approve || already-rostered — kind is
//     never consulted for admission, and the flag defaults to false. True on
//     the public venue (that's what makes the KVM and the app see each other),
//     false on fleets (the owner's signed roster controls membership).
//   - kind: "open" vs "closed" gates roster-GOSSIP trust and governance
//     transitions (unsigned roster entries merge only on open networks) —
//     it mirrors AllMyStuff's open-venue vs closed-fleet split, but it is
//     not the admission mechanism.
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

// publicClaimsAllowed reports the device-local public-claims policy —
// config-file only (see config.Mesh.PublicClaims); nothing remote can flip it.
func (b *Bridge) publicClaimsAllowed() bool {
	return b.mesh.PublicClaims
}

// claimNetworkAllowed reports whether an inbound Claim arriving on `network`
// may be honored. The claim rendezvous meshes always may — the LAN claim
// mesh and (LAN-only by default) the joining mesh, plus the device's own
// claim-code rendezvous; anything else only when public claims are
// deliberately enabled on this device.
func (b *Bridge) claimNetworkAllowed(network string) bool {
	if b.publicClaimsAllowed() {
		return true
	}
	if network == localClaimMesh || network == b.joiningMeshID() {
		return true
	}
	if code := b.state.ClaimCode(); code != "" && network == claimCodeNetworkID(code) {
		return true
	}
	return false
}

// lanOnlySignaling pins a network config to LAN-local signaling: no remote
// strategy, mDNS only, and explicit empty STUN/TURN lists so the daemon's
// public-venue defaults never apply — the network touches no remote
// infrastructure at all (and needs no wall clock, covering the KVM's
// pre-NTP boot window).
func lanOnlySignaling(cfg map[string]interface{}) map[string]interface{} {
	cfg["signaling"] = map[string]interface{}{"strategy": "none", "mdns": true}
	cfg["stun_servers"] = []interface{}{}
	cfg["turn_servers"] = []interface{}{}
	return cfg
}

// localClaimConfig builds the well-known LAN claim rendezvous config.
func (b *Bridge) localClaimConfig() map[string]interface{} {
	cfg := b.networkConfig(localClaimMesh, localClaimMesh, "Local claiming (this LAN)", nil, nil, "open", true)
	return lanOnlySignaling(cfg)
}

// joiningMeshConfig builds the joining mesh's config under the current
// public-claims policy: LAN-only signaling by default; the relay venue
// (WAN claiming via the on-screen id) when public claims are enabled.
func (b *Bridge) joiningMeshConfig(joining string) map[string]interface{} {
	cfg := b.networkConfig(joining, joining, b.mesh.Label, b.mesh.Relays, nil, "open", true)
	if !b.publicClaimsAllowed() {
		return lanOnlySignaling(cfg)
	}
	return cfg
}

// retireClaimCodeMesh leaves the claim-code rendezvous (if joined) and
// rotates the spent code — called once the fleet mesh carries the device.
func (b *Bridge) retireClaimCodeMesh() {
	code := b.state.ClaimCode()
	if code == "" {
		return
	}
	codeNet := claimCodeNetworkID(code)
	if nets, err := b.ctlNetworksList(); err == nil {
		for _, n := range nets {
			if n.NetworkID != codeNet {
				continue
			}
			if err := b.networkRemove(codeNet); err != nil {
				log.Warnf("mesh: leave claim rendezvous %s: %s", codeNet, err)
			} else {
				log.Infof("mesh: left claim rendezvous %s (claimed — code rotated)", codeNet)
			}
			break
		}
	}
	b.state.RotateClaimCode()
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
		// Via the nil-ctl-guarded helper — a concurrent teardown may have
		// nilled b.ctl, and a bare b.ctl read here would race it.
		if err := b.networkAdd(cfg); err != nil {
			log.Warnf("mesh: join fleet network %s: %s", networkID, err)
			return
		}
		log.Infof("mesh: joined fleet network %s", networkID)
	}
	// Retry the plane subscribes a few times before giving up: unlike the main
	// network (where a joinPlanes failure aborts connectAndRun and the whole
	// handshake retries), a fleet failure here would leave the fleet planes
	// dark for the rest of the session with one warn line. The daemon's engine
	// can stall ~10 s mid peer-connect on this device, so a beat between tries
	// is usually all it takes. Mirrors AllMyStuff's bring-up retry cadence.
	for attempt := 1; attempt <= 3; attempt++ {
		if err = b.joinPlanes(networkID); err == nil {
			return
		}
		time.Sleep(time.Duration(attempt) * 500 * time.Millisecond)
	}
	log.Errorf("mesh: fleet planes DARK on %s until reconnect — owner's fleet won't see this KVM: %s",
		networkID, err)
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
	if ctl == nil {
		return nil, fmt.Errorf("networks_list: bridge not connected")
	}
	return ctl.NetworksList()
}

// networkAdd is the same nil-ctl guard for network_add.
func (b *Bridge) networkAdd(cfg map[string]interface{}) error {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl == nil {
		return fmt.Errorf("network_add: bridge not connected")
	}
	return ctl.NetworkAdd(cfg)
}

// networkRemove leaves a network, purging its persisted state — every leave
// the bridge performs is a deliberate forget (retiring the legacy mesh,
// owner-ordered removes, unclaim), never a temporary unload.
func (b *Bridge) networkRemove(networkID string) error {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	if ctl == nil {
		return fmt.Errorf("network_remove: bridge not connected")
	}
	return ctl.NetworkRemove(networkID, true)
}

// syncIdentityLabel keeps the daemon's identity label in step with the
// display label (KVM-<target> when attached, the brand name otherwise), so
// the device is named the same at the myownmesh layer — rosters, approvals,
// and the AllMyStuff graph all read one name. Change-driven: the identity
// anchor is only rewritten when the label actually moved.
func (b *Bridge) syncIdentityLabel() {
	want := b.attachmentLabel()
	if want == "" {
		want = b.mesh.Name
	}
	b.mu.Lock()
	ctl := b.ctl
	have := b.identityLabel
	b.mu.Unlock()
	if ctl == nil || want == have {
		return
	}
	// Don't clobber a label the operator set out-of-band (via the daemon's own
	// CLI): only overwrite when the current label is empty or one WE manage
	// (the brand name or a KVM-<…> attachment name). A genuinely custom label
	// is left alone — the bridge owns its naming, not the operator's.
	if have != "" && have != b.mesh.Name && !strings.HasPrefix(have, "KVM-") {
		return
	}
	if err := ctl.IdentitySetLabel(want); err != nil {
		log.Warnf("mesh: set identity label %q: %s", want, err)
		return
	}
	b.mu.Lock()
	b.identityLabel = want
	b.mu.Unlock()
	log.Infof("mesh: identity label → %q", want)
}

// notePeerLabel caches a peer's display label from its presence advert — the
// fallback that names this device KVM-<target> when an attach arrives without
// a label (an older sender) or at claim time (the auto-attach to the claimer).
// from is the daemon-AUTHENTICATED sender; the payload's self-declared `node`
// is not, so we cache only when they match. Without this a stranger on the
// open joining mesh could advertise a presence claiming node=<the claimer>
// with attacker text, poisoning the label the device renames itself to at
// claim time.
func (b *Bridge) notePeerLabel(from string, raw json.RawMessage) {
	if from == "" {
		return
	}
	var p struct {
		Node  string `json:"node"`
		Label string `json:"label"`
	}
	if err := json.Unmarshal(raw, &p); err != nil || p.Node == "" || p.Label == "" {
		return
	}
	// The advert must be about its own sender — a peer may only set its own
	// label, never someone else's cache slot.
	if !canonicalEqual(p.Node, from) {
		return
	}
	b.mu.Lock()
	if b.peerLabels == nil {
		b.peerLabels = map[string]string{}
	}
	// A hostile joining-mesh peer could churn identities to bloat the map;
	// resetting a full cache is cheaper and safer than an eviction policy —
	// the label of anyone who matters re-arrives with their next presence.
	if len(b.peerLabels) > 512 {
		b.peerLabels = map[string]string{}
	}
	b.peerLabels[pubkeyPart(from)] = p.Label
	b.mu.Unlock()
}

// peerLabel returns the cached display label for a peer, or "".
func (b *Bridge) peerLabel(peer string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.peerLabels[pubkeyPart(peer)]
}

// joiningMeshFile is where the bridge publishes the joining mesh id for the
// OLED app — kvm_system polls small files under /kvmapp/kvm, the established
// Go→C IPC on this device — so the screen can show the name a claimer needs.
const joiningMeshFile = "/kvmapp/kvm/mesh_name"

// writeJoiningMeshFile publishes the joining mesh id for the OLED. Best-effort:
// on a dev host the directory usually doesn't exist, and the screen is the
// only consumer.
func writeJoiningMeshFile(id string) {
	if err := os.MkdirAll(filepath.Dir(joiningMeshFile), 0o755); err != nil {
		return
	}
	if err := os.WriteFile(joiningMeshFile, []byte(id+"\n"), 0o644); err != nil {
		log.Debugf("mesh: write %s: %s", joiningMeshFile, err)
	}
}

// joinPlanes subscribes the event-stream client to presence/control/media on a
// network, advertises capabilities, and records the network for presence
// broadcasts. Idempotent: re-subscribing is cheap.
func (b *Bridge) joinPlanes(networkID string) error {
	b.mu.Lock()
	ctl, events := b.ctl, b.events
	b.mu.Unlock()
	if ctl == nil || events == nil {
		return fmt.Errorf("mesh: joinPlanes before connect")
	}
	// channel_subscribe names the EVENTS client (by its client_id) but must
	// ride the ctl connection — the daemon never reads from a subscribed event
	// stream, so a request written there sits unanswered until the read
	// deadline. That was the "read response: i/o timeout" bring-up loop: the
	// daemon was healthy, the request was just on the wrong socket.
	for _, ch := range []string{ChannelPresence, ChannelControl, ChannelMedia} {
		if err := ctl.ChannelSubscribe(events.ClientID(), networkID, ch); err != nil {
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
		// A peer announced itself. Remember its display label (the KVM-<label>
		// fallback), then greet a newly-seen peer by sending our profile
		// straight to it, so the app learns about us immediately (event-driven
		// gossip) instead of waiting for the slow heartbeat — without this we
		// only appear on a 30s beat and flap in and out of the graph.
		b.notePeerLabel(ci.From, ci.Payload)
		b.greetPeer(ci.Network, ci.From)
	}
}

// greetCooldown bounds how often we re-greet the same peer, so a peer's own
// presence (or a quick reconnect) can't trigger a tight greet loop.
const greetCooldown = 10 * time.Second

// greetPeer sends our current NodeProfile directly to a peer that just announced
// itself, debounced per peer, so a freshly-connected app sees us right away.
func (b *Bridge) greetPeer(network, peer string) {
	if peer == "" || network == "" {
		return
	}
	b.mu.Lock()
	if b.greetedAt == nil {
		b.greetedAt = map[string]time.Time{}
	}
	if t, ok := b.greetedAt[peer]; ok && time.Since(t) < greetCooldown {
		b.mu.Unlock()
		return
	}
	b.greetedAt[peer] = time.Now()
	ctl := b.ctl
	running := b.running
	b.mu.Unlock()
	if !running || ctl == nil {
		return
	}
	payload, err := b.presencePayload()
	if err != nil {
		return
	}
	if err := ctl.ChannelSendTo(network, ChannelPresence, peer, json.RawMessage(payload)); err != nil {
		log.Debugf("mesh: greet %s on %s: %s", peer, network, err)
		return
	}
	log.Infof("mesh: greeted peer %s on %s", peer, network)
}

// presencePayload marshals the current NodeProfile, stamped with this send's
// wall clock (the sample behind AllMyStuff's passive clock-skew estimate) —
// but only when the clock is sane. The KVM has no RTC and boots at 1970 until
// NTP lands; an insane clock stays unstamped (sent_at omitted = "no sample")
// rather than reading as decades of skew on every peer. Stamped here, per
// send, never in buildProfile — a profile built once and sent twice must not
// carry the first send's clock.
func (b *Bridge) presencePayload() ([]byte, error) {
	profile := b.currentProfile()
	if now := time.Now(); now.Year() >= 2020 {
		profile.SentAt = uint64(now.UnixMilli())
	}
	return json.Marshal(profile)
}

// sendControlTo sends a ControlMessage point-to-point on CHANNEL_CONTROL.
func (b *Bridge) sendControlTo(network, peer string, msg ControlMessage) error {
	b.mu.Lock()
	ctl := b.ctl
	b.mu.Unlock()
	return ctl.ChannelSendTo(network, ChannelControl, peer, msg.Payload())
}

// rejectRouteTo sends a RouteControl Reject to a peer across our networks —
// peer-addressed like sendSiteFrame, so the network the peer is actually on
// delivers it and the rest are harmless no-ops. Used for frames on dead
// routes, where we don't know which network the stale route rode.
func (b *Bridge) rejectRouteTo(peer, route, reason string) {
	b.mu.Lock()
	ctl := b.ctl
	nets := append([]string(nil), b.networks...)
	running := b.running
	b.mu.Unlock()
	if !running || ctl == nil {
		return
	}
	msg := NewRouteReject(route, reason)
	for _, n := range nets {
		if err := ctl.ChannelSendTo(n, ChannelControl, peer, msg.Payload()); err == nil {
			log.Infof("mesh: rejected dead route %s to %s (%s)", route, peer, reason)
			return
		}
	}
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
	payload, err := b.presencePayload()
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
			// Warn, not debug: a swallowed presence failure is the difference
			// between "the KVM is invisible and nothing says why" and a log
			// line naming it. channel_send_all rides the daemon's engine
			// driver, which on this single-core device can stall ~10 s while
			// a peer connect is in flight — the next 30 s tick retries.
			log.Warnf("mesh: broadcast presence on %s: %s", n, err)
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
	joining := b.joiningMesh
	b.mu.Unlock()
	return buildProfile(nodeID, b.conf, b.dev, b.state, b.versionString(), b.boot,
		joining, b.networksSnapshot(), b.attachmentLabel())
}

// versionString returns the NanoKVM app version (best-effort file read).
func (b *Bridge) versionString() string {
	if raw, err := readFileTrim("/kvmapp/version"); err == nil && raw != "" {
		return raw
	}
	return appVersion
}
