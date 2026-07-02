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

// handleOwnership processes Claim and FleetKey.
func (b *Bridge) handleOwnership(network, from string, oc *OwnershipControl) {
	if oc == nil {
		return
	}
	switch oc.Kind {
	case OwnershipKindClaim:
		// The claimer is the message's `owner` field (the claimer's node id).
		claimer := oc.Owner
		if claimer == "" {
			claimer = from
		}
		if !b.state.TryClaim(claimer) {
			log.Infof("mesh: claim from %s refused (not claimable)", from)
			return
		}
		log.Infof("mesh: claimed by %s; auto-attached to it", claimer)
		// Confirm the adoption point-to-point, then re-advertise (the presence
		// advert is the authoritative confirmation).
		if err := b.sendControlTo(network, from, NewClaimed(claimer)); err != nil {
			log.Warnf("mesh: send Claimed to %s: %s", from, err)
		}
		b.reAdvertise()

	case OwnershipKindFleetKey:
		// Record the fleet credential and, since we can derive the closed-
		// network id from the key (matching AllMyStuff's derivation), actually
		// join the fleet's base network.
		changed := b.state.AdoptFleetKey(oc.Key, oc.Name, oc.Venue)
		if oc.Key != "" {
			fleetNet := DeriveFleetNetworkID(oc.Key)
			b.joinFleetNetwork(fleetNet, oc.Name, oc.Venue)
		}
		if changed {
			b.reAdvertise()
		}

	default:
		// claimed / declined / release / fleet_departed / unknown — no action
		// for a KVM appliance in v1.
	}
}

// handleKvm processes Attach/Detach, gated on the sender being the owner.
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
		if b.state.SetAttachedTo(kc.Node) {
			log.Infof("mesh: attached to %s", kc.Node)
			b.reAdvertise()
		}
	case KvmControlKindDetach:
		if b.state.SetAttachedTo("") {
			log.Infof("mesh: detached")
			b.reAdvertise()
		}
	default:
		// unknown — ignore.
	}
}

// handleRoute processes a site-route Offer/Teardown. A site route is generic
// media whose `from` ends ":site"; we accept it (so MEDIA SiteFrames are served)
// and reply Accept. Other route kinds/media are ignored in v1.
func (b *Bridge) handleRoute(network, from string, rc *RouteControl) {
	if rc == nil {
		return
	}
	switch rc.Kind {
	case RouteControlKindOffer:
		if rc.Route == nil || !rc.Route.IsSiteRoute() {
			return // v1 only tunnels site routes (the web UI)
		}
		routeID := rc.Route.ID
		if !b.senderMayControl(from) {
			// Reject with a reason instead of silence: without it the offerer
			// waits out its 15 s offer expiry and blames the network.
			log.Infof("mesh: site route offer from non-owner %s rejected", from)
			if err := b.sendControlTo(network, from,
				NewRouteReject(routeID, "not this KVM's owner — claim it first")); err != nil {
				log.Warnf("mesh: send route Reject to %s: %s", from, err)
			}
			return
		}
		b.sites.markRouteActive(routeID, from)
		if err := b.sendControlTo(network, from, NewRouteAccept(routeID)); err != nil {
			log.Warnf("mesh: send route Accept to %s: %s", from, err)
		}
		log.Infof("mesh: accepted site route %s from %s", routeID, from)

	case RouteControlKindTeardown:
		b.sites.tearDownRoute(rc.RouteID)
		log.Debugf("mesh: tore down route %s", rc.RouteID)

	case RouteControlKindReject:
		// The offerer refused/abandoned a route we track (e.g. it expired the
		// offer and re-offered under a fresh id) — treat like a teardown, but
		// only from the peer the route actually belongs to.
		if peer, ok := b.sites.routePeer(rc.RouteID); ok && peer == from {
			b.sites.tearDownRoute(rc.RouteID)
			log.Debugf("mesh: peer rejected route %s — torn down", rc.RouteID)
		}

	default:
		// accept / unknown — nothing to do host-side.
	}
}

// senderMayControl reports whether `from` is allowed to curate this device — its
// owner (or a member of the same fleet). The mesh authenticates the sender, so
// this is a real check. With no recorded owner the device is unclaimed and
// curation is refused (claim first).
func (b *Bridge) senderMayControl(from string) bool {
	owner := b.state.Owner()
	if owner == "" {
		return false
	}
	return canonicalEqual(owner, from)
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
