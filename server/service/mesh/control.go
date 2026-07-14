package mesh

import (
	log "github.com/sirupsen/logrus"
)

// handleControl dispatches one ControlMessage that arrived on CHANNEL_CONTROL.
// from is the authenticated sender (the daemon proved their identity), network
// is the network it arrived on. The bridge re-advertises presence after any
// state change so the change is confirmed authoritatively.
func (b *Bridge) handleControl(network, from string, msg ControlMessage) {
	switch msg.Kind {
	case ControlKindOwnership:
		b.handleOwnership(network, from, msg.Ownership)
	case ControlKindKvm:
		b.handleKvm(network, from, msg.Kvm)
	case ControlKindRoute:
		b.handleRoute(network, from, msg.Route)
	case ControlKindApp:
		b.handleApp(network, from, msg.App)
	default:
		// share / site / unknown — not acted on in v1.
	}
}

// handleApp processes app-level commands (app.rs AppControl), gated on the
// sender being the owner exactly like KVM curation. restart_device reboots the
// appliance and "restart" relaunches NanoKVM-Server; neither sends a reply —
// presence dropping and returning (or the fresh process re-advertising) is the
// confirmation, mirroring AllMyStuff's node. "upgrade" is meaningless here
// (the KVM's firmware is not AllMyStuff's self-updater) and is ignored.
func (b *Bridge) handleApp(network, from string, ac *AppControl) {
	if ac == nil {
		return
	}
	if !b.senderMayControl(from) {
		log.Infof("mesh: app control %q from non-owner %s ignored", ac.Kind, from)
		return
	}
	switch ac.Kind {
	case AppControlKindRestartDevice:
		log.Infof("mesh: device reboot requested by %s — handing to the OS", from)
		// Off the event-stream goroutine: restartDevice blocks on each OS
		// attempt, and a stalled reboot must not stall inbound frames.
		go func() {
			if err := restartDeviceFn(); err != nil {
				log.Warnf("mesh: device reboot refused: %s", err)
			}
		}()
	case AppControlKindRestart:
		log.Infof("mesh: app restart requested by %s — relaunching NanoKVM-Server", from)
		go func() {
			if err := restartServerFn(); err != nil {
				log.Warnf("mesh: app restart failed: %s", err)
			}
		}()
	default:
		// upgrade / unknown — nothing to act on.
	}
}

// The reboot/relaunch actions behind handleApp, swappable so tests can observe
// the dispatch without actually asking the OS for a reboot.
var (
	restartDeviceFn = restartDevice
	restartServerFn = restartServer
)

// handleOwnership processes Claim, FleetKey, and Release.
func (b *Bridge) handleOwnership(network, from string, oc *OwnershipControl) {
	if oc == nil {
		return
	}
	switch oc.Kind {
	case OwnershipKindClaim:
		// Claims are honored only via the claim rendezvous meshes (the LAN
		// claim mesh, the joining mesh, the device's own claim code) unless
		// public claims are deliberately enabled in this device's config —
		// a defense-in-depth gate on top of the membership policy (an
		// unclaimed KVM shouldn't even BE anywhere else).
		if !b.claimNetworkAllowed(network) {
			log.Infof("mesh: claim from %s over %s refused (public claims disabled)", from, network)
			if err := b.sendControlTo(network, from, NewDeclined(
				"claims over the public mesh are disabled on this KVM — claim it from the "+
					"same local network, or set mesh.publicClaims in its server.yaml")); err != nil {
				log.Warnf("mesh: send Declined to %s: %s", from, err)
			}
			return
		}
		// The claimer is the message's `owner` field (the claimer's node id).
		claimer := oc.Owner
		if claimer == "" {
			claimer = from
		}
		if !b.state.TryClaim(claimer, b.peerLabel(claimer)) {
			log.Infof("mesh: claim from %s refused (not claimable)", from)
			return
		}
		log.Infof("mesh: claimed by %s; auto-attached to it", claimer)
		// The auto-attach renamed us KVM-<claimer>; mirror it on the daemon
		// identity too.
		b.syncIdentityLabel()
		// Confirm the adoption point-to-point, then re-advertise (the presence
		// advert is the authoritative confirmation).
		if err := b.sendControlTo(network, from, NewClaimed(claimer)); err != nil {
			log.Warnf("mesh: send Claimed to %s: %s", from, err)
		}
		b.reAdvertise()

	case OwnershipKindFleetKey:
		// The fleet key is a credential handed down by the device's OWNER
		// right after it claims — never volunteered by a stranger. Gate it:
		// the joining mesh is auto-approve open (any peer who reads the id
		// off the screen can join), so an ungated FleetKey would let anyone
		// there capture an unclaimed device onto their own fleet. An
		// unclaimed device (no owner) fails this closed; the owner passes via
		// the owner==from arm of senderMayControl. Mirrors AllMyStuff's node,
		// which rejects a fleet key from anyone but the recorded owner.
		if !b.senderMayControl(from) {
			log.Infof("mesh: fleet key from non-owner %s ignored", from)
			return
		}
		// Record the fleet credential and, since we can derive the closed-
		// network id from the key (matching AllMyStuff's derivation), actually
		// join the fleet's base network.
		changed := b.state.AdoptFleetKey(oc.Key, oc.Name, oc.Venue)
		if oc.Key != "" {
			// Off the event-stream goroutine: joinFleetNetwork does a
			// network_add (attaches signaling — seconds), and a stalled join
			// must not block inbound frames. refreshFleetRoster + the
			// deferred joining-mesh leave chain off the same goroutine.
			go func() {
				fleetNet := DeriveFleetNetworkID(oc.Key)
				b.joinFleetNetwork(fleetNet, oc.Name, oc.Venue)
				b.refreshFleetRoster()
				// Adopted into a fleet: the auto-approve joining mesh has done
				// its job. Leave it once the fleet mesh is really carrying us
				// (roster converged) — an unclaim re-derives and rejoins it.
				b.leaveJoiningMeshAfterAdoption()
			}()
		}
		if changed {
			b.reAdvertise()
		}

	case OwnershipKindRelease:
		// "You're no longer mine" — the unclaim. Owner/fleet gated like every
		// curation; a stranger can't factory-reset your KVM's mesh identity.
		if !b.senderMayControl(from) {
			log.Infof("mesh: release from non-owner %s ignored", from)
			return
		}
		// Off the event-stream goroutine: the reset is a series of daemon
		// round-trips (leave fleet, rejoin joining mesh) that must not stall
		// inbound frames.
		go b.unclaim(from)

	default:
		// claimed / declined / fleet_departed / unknown — no action for a KVM
		// appliance in v1.
	}
}

// handleKvm processes Attach/Detach and the mesh-membership commands
// MeshAdd/MeshRemove, gated on the sender being the owner or a fleet
// co-member.
func (b *Bridge) handleKvm(network, from string, kc *KvmControl) {
	if kc == nil {
		return
	}
	if !b.senderMayControl(from) {
		log.Infof("mesh: kvm control from non-owner %s ignored", from)
		return
	}
	switch kc.Kind {
	case KvmControlKindAttach:
		// The target's label rides the command; an older sender omits it and
		// we fall back to the label from the target's own presence advert.
		label := kc.Label
		if label == "" {
			label = b.peerLabel(kc.Node)
		}
		if b.state.SetAttachedTo(kc.Node, label) {
			log.Infof("mesh: attached to %s (%q)", kc.Node, label)
			// The attachment renames us KVM-<target>; mirror it on the
			// daemon identity.
			b.syncIdentityLabel()
			b.reAdvertise()
		}
	case KvmControlKindDetach:
		if b.state.SetAttachedTo("", "") {
			log.Infof("mesh: detached")
			b.syncIdentityLabel()
			b.reAdvertise()
		}
	case KvmControlKindMeshAdd:
		// Off the event-stream goroutine: joining a network attaches
		// signaling — seconds, not millis.
		go b.applyMeshAdd(kc.NetworkID)
	case KvmControlKindMeshRemove:
		go b.applyMeshRemove(kc.NetworkID)
	default:
		// unknown — ignore.
	}
}

// handleRoute processes a route Offer/Teardown/Refresh/Tune across the site AND
// native planes. A site route (generic media, `from` ends ":site") is tunneled
// as before; a display route (KVM = video source) starts the H.264 pump; an
// input route (KVM = keyboard/mouse sink) is registered for HID injection.
func (b *Bridge) handleRoute(network, from string, rc *RouteControl) {
	if rc == nil {
		return
	}
	switch rc.Kind {
	case RouteControlKindOffer:
		if rc.Route == nil {
			return
		}
		switch {
		case rc.Route.IsSiteRoute():
			b.handleSiteOffer(network, from, rc)
		case rc.Route.IsDisplayRoute():
			b.handleDisplayOffer(network, from, rc)
		case rc.Route.IsInputRoute():
			b.handleInputOffer(network, from, rc)
		default:
			// A media kind this build doesn't stream — ignore.
		}

	case RouteControlKindTeardown:
		b.sites.tearDownRoute(rc.RouteID)
		b.stopDisplayRoute(rc.RouteID)
		b.clearInputRoute(rc.RouteID)
		log.Debugf("mesh: tore down route %s", rc.RouteID)

	case RouteControlKindRefresh:
		b.handleRouteRefresh(from, rc)

	case RouteControlKindTune:
		b.handleRouteTune(from, rc)

	case RouteControlKindReject:
		// The offerer refused/abandoned a route we track (e.g. it expired the
		// offer and re-offered under a fresh id) — treat like a teardown, but
		// only from the peer the route actually belongs to.
		if peer, ok := b.sites.routePeer(rc.RouteID); ok && peer == from {
			b.sites.tearDownRoute(rc.RouteID)
			log.Debugf("mesh: peer rejected route %s — torn down", rc.RouteID)
		}
		// A native route the offerer abandons is torn down the same way (the
		// stop/clear helpers no-op unless the id matches the active route).
		b.stopDisplayRoute(rc.RouteID)
		b.clearInputRoute(rc.RouteID)

	default:
		// accept / video_lane / unknown — nothing to do host-side.
	}
}

// handleSiteOffer accepts a "sites" plane route (the tunneled web UI) and replies
// Accept, so MEDIA SiteFrames on it are served.
func (b *Bridge) handleSiteOffer(network, from string, rc *RouteControl) {
	routeID := rc.Route.ID
	if !b.senderMayControl(from) {
		// Reject with a reason instead of silence: without it the offerer
		// waits out its 15 s offer expiry and blames the network.
		log.Infof("mesh: site route offer from non-owner %s rejected", from)
		b.sendRouteReject(network, from, routeID, "not this KVM's owner — claim it first")
		return
	}
	b.sites.markRouteActive(routeID, from)
	if err := b.sendControlTo(network, from, NewRouteAccept(routeID)); err != nil {
		log.Warnf("mesh: send route Accept to %s: %s", from, err)
	}
	log.Infof("mesh: accepted site route %s from %s", routeID, from)
}

// senderMayControl reports whether `from` is allowed to curate this device —
// its owner or a member of the same fleet, exactly the authority the protocol
// documents (app.rs KvmControl: "only the device's owner or a fleet co-member
// is obeyed") and the GUI offers controls for. The mesh authenticates the
// sender, so this is a real check; the fleet half reads the cached roster of
// the fleet's closed network (signed membership, refreshed off this
// goroutine). With no recorded owner the device is unclaimed and curation is
// refused (claim first).
func (b *Bridge) senderMayControl(from string) bool {
	// A technician we auto-approved on the CEC help mesh drives the KVM like an
	// owner — answering a raised hand is a full support session, by design.
	if b.cecApprovedTech(from) {
		return true
	}
	owner := b.state.Owner()
	if owner == "" {
		return false
	}
	if canonicalEqual(owner, from) {
		return true
	}
	b.mu.Lock()
	_, coFleet := b.fleetRoster[pubkeyPart(from)]
	b.mu.Unlock()
	return coFleet
}

// canonicalEqual compares two mesh device ids, tolerating MyOwnMesh's optional
// 5-char display suffix (e.g. "abcd-AB12C" vs "abcd"). Mirrors pubkey_part in
// ownership.rs so a display-id and bare-pubkey view of one machine match.
func canonicalEqual(a, b string) bool {
	return pubkeyPart(a) == pubkeyPart(b)
}

func pubkeyPart(id string) string {
	if i := lastIndexByte(id, '-'); i >= 0 {
		suffix := id[i+1:]
		if len(suffix) == 5 && allAlnum(suffix) {
			return id[:i]
		}
	}
	return id
}

func lastIndexByte(s string, c byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == c {
			return i
		}
	}
	return -1
}

func allAlnum(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z') {
			return false
		}
	}
	return true
}
