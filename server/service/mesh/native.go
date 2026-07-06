package mesh

import (
	"strconv"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// This file holds the native (Slice 1) screen/HID path: the display route
// (H.264 pushed onto a media-track lane) and the input route (remote input
// injected via HID). The mesh package stays CGO-FREE, so it reaches the KVM's
// encoder and HID gadget only through these injected interfaces — the on-device
// glue package (server/service/mesh/glue) supplies the real implementations at
// construction; on host test builds they're nil and the route arms reject.

// VideoParams is a snapshot of the capture geometry the pump encodes at. It's
// re-read each frame so a viewer Tune takes effect live — mirroring the WebRTC
// manager re-reading common.GetScreen()'s live fields between frames.
type VideoParams struct {
	Width   int
	Height  int
	FPS     int
	BitRate int
}

// VideoSource is the injected H.264 encoder the display pump drives. It models
// how webrtc/manager.go reads common.GetScreen() geometry and calls
// common.GetKvmVision().ReadH264 — adapted to plain ints so this package needs
// neither CGO nor server/common.
type VideoSource interface {
	// Params returns the current capture geometry to encode at.
	Params() VideoParams
	// ReadH264 returns one Annex-B H.264 access unit for the given geometry +
	// bitrate (an IDR carries SPS+PPS inline), plus a result code (<0 = error /
	// no frame ready).
	ReadH264(width, height, bitrate int) (au []byte, result int)
	// SetFps requests a capture frame rate (best-effort; a no-op is fine).
	SetFps(fps int)
	// Tune applies a viewer's requested constraints; a nil field means "leave
	// unchanged". Translating max_edge → a resolution is the source's job (it
	// owns the common.GetScreen / ResolutionMap semantics kept out of here).
	Tune(maxEdge, bitrate, fps *uint32)
	// ForceIDR best-effort asks the encoder for a keyframe now; a source that
	// can't relies on its periodic GOP to recover a refreshing viewer.
	ForceIDR()
	// Prepare puts the shared encoder into native H.264 capture mode before the
	// pump drains it — defensive against a prior web session having left it in
	// another stream type. A no-op where there's nothing to set.
	Prepare()
	// CaptureInterval is how often the pump polls ReadH264. It encodes the
	// encoder's model: a PUSH-driven HDMI encoder (the Pro) hands back the next
	// access unit (or nothing) and must be drained FASTER than the source rate
	// so every unit is taken in order — poll too slowly and the viewer decodes
	// P-frames whose reference frames it never received (one frame, then a
	// frozen picture). A PULL-driven encoder (NanoKVM) yields exactly one frame
	// per call, so the interval is simply the target fps.
	CaptureInterval() time.Duration
}

// InputSink is the injected HID gadget the input route feeds. Apply translates
// one normalized action to a HID report and writes it.
type InputSink interface {
	Apply(action InputAction)
}

// SetVideoSource injects the native display encoder (nil disables the display
// route). Called once by the glue at construction.
func (b *Bridge) SetVideoSource(vs VideoSource) {
	b.mu.Lock()
	b.videoSource = vs
	b.mu.Unlock()
}

// SetInputSink injects the native HID sink (nil disables the input route).
// Called once by the glue at construction.
func (b *Bridge) SetInputSink(is InputSink) {
	b.mu.Lock()
	b.inputSink = is
	b.mu.Unlock()
}

// displaySession is one active mesh display route and the goroutine streaming
// H.264 onto its lane. The pump goroutine owns pipe (closes it on exit); cancel
// is closed to stop it.
type displaySession struct {
	routeID string          // the accepted route id
	peer    string          // the console's canonical pubkey (media destination)
	network string          // the display offer's network
	lane    uint8           // the announced video lane
	pipe    *MediaTrackPipe // the dedicated media-track connection
	cancel  chan struct{}   // closed to stop runVideoPump
}

// maxVideoLanes bounds the lane allocator. v1 streams a single screen, so lane
// 0 is always the one picked; the small allocator keeps the shape honest.
const maxVideoLanes = 8

// allocLaneLocked reserves the lowest free video lane (caller holds b.mu).
func (b *Bridge) allocLaneLocked() (uint8, bool) {
	if b.lanes == nil {
		b.lanes = make(map[uint8]bool)
	}
	for lane := uint8(0); lane < maxVideoLanes; lane++ {
		if !b.lanes[lane] {
			b.lanes[lane] = true
			return lane, true
		}
	}
	return 0, false
}

// freeLaneLocked releases a lane (caller holds b.mu).
func (b *Bridge) freeLaneLocked(lane uint8) {
	delete(b.lanes, lane)
}

// handleDisplayOffer arms a display route (the KVM is the video source): it
// gates the offerer, opens a media pipe, allocates a lane, replies Accept +
// VideoLane, and starts the pump. Reasons for refusal are sent as a Reject so
// the console doesn't wait out its offer expiry blaming the network.
func (b *Bridge) handleDisplayOffer(network, from string, rc *RouteControl) {
	routeID := rc.Route.ID

	if !b.senderMayControl(from) {
		log.Infof("mesh: display route offer from non-owner %s rejected", from)
		b.sendRouteReject(network, from, routeID, "not this KVM's owner — claim it first")
		return
	}

	b.mu.Lock()
	vs := b.videoSource
	active := b.display
	b.mu.Unlock()

	if vs == nil {
		log.Warnf("mesh: display route %s offered but native video is unavailable on this build", routeID)
		b.sendRouteReject(network, from, routeID, "native screen streaming is unavailable on this device")
		return
	}
	// The console advertises the transports it can consume on the Offer; refuse
	// early if it can't take our h264 (we never re-encode).
	if len(rc.Video) > 0 && !stringsContain(rc.Video, "h264") {
		log.Infof("mesh: display route %s rejected — console can't consume h264 (offered %v)", routeID, rc.Video)
		b.sendRouteReject(network, from, routeID, "this KVM only streams h264")
		return
	}
	if active != nil {
		log.Infof("mesh: display route %s offered while %s is active — rejecting (v1 streams one screen)", routeID, active.routeID)
		b.sendRouteReject(network, from, routeID, "this KVM already has an active screen viewer")
		return
	}

	// Dial the media pipe BEFORE committing state so a slow handshake never
	// holds b.mu, and a failure leaves no half-armed route behind.
	pipe, err := DialMediaTrackPipe(b.socketPath())
	if err != nil {
		log.Warnf("mesh: open media pipe for display route %s: %s", routeID, err)
		b.sendRouteReject(network, from, routeID, "could not open the media lane")
		return
	}

	b.mu.Lock()
	if b.display != nil {
		b.mu.Unlock()
		_ = pipe.Close()
		b.sendRouteReject(network, from, routeID, "this KVM already has an active screen viewer")
		return
	}
	lane, ok := b.allocLaneLocked()
	if !ok {
		b.mu.Unlock()
		_ = pipe.Close()
		b.sendRouteReject(network, from, routeID, "no free video lane")
		return
	}
	sess := &displaySession{
		routeID: routeID,
		peer:    pubkeyPart(from),
		network: network,
		lane:    lane,
		pipe:    pipe,
		cancel:  make(chan struct{}),
	}
	b.display = sess
	b.mu.Unlock()

	if err := b.sendControlTo(network, from, NewRouteAccept(routeID)); err != nil {
		log.Warnf("mesh: send route Accept to %s: %s", from, err)
	}
	if err := b.sendControlTo(network, from, NewRouteVideoLane(routeID, lane)); err != nil {
		log.Warnf("mesh: send video_lane to %s: %s", from, err)
	}
	log.Infof("mesh: accepted display route %s from %s on lane %d", routeID, from, lane)
	go b.runVideoPump(sess)
}

// handleInputOffer arms an input route (the KVM is the keyboard/mouse sink): it
// gates the offerer, replies Accept, and records the route as the active input
// sink so its InputEvents are injected.
func (b *Bridge) handleInputOffer(network, from string, rc *RouteControl) {
	routeID := rc.Route.ID

	if !b.senderMayControl(from) {
		log.Infof("mesh: input route offer from non-owner %s rejected", from)
		b.sendRouteReject(network, from, routeID, "not this KVM's owner — claim it first")
		return
	}

	b.mu.Lock()
	is := b.inputSink
	b.mu.Unlock()
	if is == nil {
		log.Warnf("mesh: input route %s offered but HID injection is unavailable on this build", routeID)
		b.sendRouteReject(network, from, routeID, "keyboard/mouse injection is unavailable on this device")
		return
	}

	b.mu.Lock()
	b.inputRoute = routeID
	b.inputPeer = from
	b.mu.Unlock()

	if err := b.sendControlTo(network, from, NewRouteAccept(routeID)); err != nil {
		log.Warnf("mesh: send route Accept to %s: %s", from, err)
	}
	log.Infof("mesh: accepted input route %s from %s", routeID, from)
}

// handleRouteRefresh forces an IDR on the active display route (best-effort; a
// source that can't relies on its periodic GOP).
func (b *Bridge) handleRouteRefresh(from string, rc *RouteControl) {
	b.mu.Lock()
	sess := b.display
	vs := b.videoSource
	b.mu.Unlock()
	if sess == nil || sess.routeID != rc.RouteID || vs == nil {
		return
	}
	if !b.senderMayControl(from) {
		return
	}
	vs.ForceIDR()
	log.Debugf("mesh: refresh (force IDR) on display route %s", rc.RouteID)
}

// handleRouteTune applies a viewer's resolution/bitrate/fps request to the
// active display route (best-effort).
func (b *Bridge) handleRouteTune(from string, rc *RouteControl) {
	b.mu.Lock()
	sess := b.display
	vs := b.videoSource
	b.mu.Unlock()
	if sess == nil || sess.routeID != rc.RouteID || vs == nil {
		return
	}
	if !b.senderMayControl(from) {
		return
	}
	vs.Tune(rc.MaxEdge, rc.Bitrate, rc.FPS)
	log.Infof("mesh: tuned display route %s (max_edge=%s bitrate=%s fps=%s)",
		rc.RouteID, fmtU32(rc.MaxEdge), fmtU32(rc.Bitrate), fmtU32(rc.FPS))
}

// stopDisplayRoute stops the active display pump if it matches routeID (empty
// matches any) and frees its lane. Safe to call repeatedly and from any
// goroutine: the state clear is guarded by b.mu, so exactly one caller closes
// the pump's cancel channel.
func (b *Bridge) stopDisplayRoute(routeID string) {
	b.mu.Lock()
	sess := b.display
	if sess == nil || (routeID != "" && sess.routeID != routeID) {
		b.mu.Unlock()
		return
	}
	b.display = nil
	b.freeLaneLocked(sess.lane)
	b.mu.Unlock()
	close(sess.cancel)
	log.Infof("mesh: display route %s stopped", sess.routeID)
}

// clearInputRoute drops the active input route if it matches routeID (empty
// matches any), so its InputEvents stop being injected.
func (b *Bridge) clearInputRoute(routeID string) {
	b.mu.Lock()
	if b.inputRoute != "" && (routeID == "" || b.inputRoute == routeID) {
		b.inputRoute = ""
		b.inputPeer = ""
	}
	b.mu.Unlock()
}

// tearDownNative stops the display pump and clears the input route on a session
// drop (the daemon connection died, so the media pipe is useless too). Mirrors
// siteHost.tearDownAll for the native path.
func (b *Bridge) tearDownNative() {
	b.mu.Lock()
	sess := b.display
	b.display = nil
	b.inputRoute = ""
	b.inputPeer = ""
	b.lanes = make(map[uint8]bool)
	b.mu.Unlock()
	if sess != nil {
		close(sess.cancel)
	}
}

// One-shot diagnostics so the input path's fate is visible in the (INFO-level)
// bridge log without per-event spam — each fires at most once per process.
var (
	inputArrivedOnce   sync.Once
	inputDropRouteOnce sync.Once
	inputDropPeerOnce  sync.Once
	inputDropAuthOnce  sync.Once
	inputInjectedOnce  sync.Once
)

// handleInputEvent injects one InputEvent that arrived on CHANNEL_MEDIA, if it
// matches the active input route AND comes from that route's authorized offerer.
func (b *Bridge) handleInputEvent(from string, ev InputEvent) {
	b.mu.Lock()
	route := b.inputRoute
	peer := b.inputPeer
	is := b.inputSink
	b.mu.Unlock()

	inputArrivedOnce.Do(func() {
		log.Infof("mesh: first native input event received (from %s, event-route %s, kind %s; active input-route=%q, sink=%t)",
			from, ev.Route, ev.Action.Kind, route, is != nil)
	})

	if is == nil || route == "" || ev.Route != route {
		inputDropRouteOnce.Do(func() {
			log.Infof("mesh: input dropped — no matching input route (sink=%t, active=%q, event=%q)", is != nil, route, ev.Route)
		})
		return
	}
	// The mesh authenticates the sender; require it to be the peer that offered
	// this route AND still authorized to curate the device.
	if !canonicalEqual(peer, from) {
		inputDropPeerOnce.Do(func() {
			log.Infof("mesh: input dropped — sender %s is not the route's offerer %s", from, peer)
		})
		return
	}
	if !b.senderMayControl(from) {
		inputDropAuthOnce.Do(func() {
			log.Infof("mesh: input dropped — sender %s is not this KVM's owner/fleet", from)
		})
		return
	}
	if ev.Action.Kind == InputActionUnknown {
		return
	}
	inputInjectedOnce.Do(func() {
		log.Infof("mesh: injecting native input to HID (first event kind %s)", ev.Action.Kind)
	})
	is.Apply(ev.Action)
}

// runVideoPump drains H.264 access units off the encoder and onto the session's
// lane until the route is torn down or a pipe write fails. Modeled on
// webrtc/manager.go sendVideoStream: put the encoder in native mode, poll at the
// source's own CaptureInterval (NOT the requested fps — see VideoSource), and
// pace the daemon's RTP clock with the MEASURED gap between units, so the
// viewer's jitter buffer tracks the encoder's real output rate.
func (b *Bridge) runVideoPump(sess *displaySession) {
	defer sess.pipe.Close()

	b.mu.Lock()
	vs := b.videoSource
	b.mu.Unlock()
	if vs == nil {
		return
	}

	vs.Prepare()

	interval := vs.CaptureInterval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var last time.Time
	for {
		select {
		case <-sess.cancel:
			return
		case <-ticker.C:
		}

		p := vs.Params()
		au, result := vs.ReadH264(p.Width, p.Height, p.BitRate)
		if result < 0 || len(au) == 0 {
			continue
		}

		// Measured inter-unit gap drives duration_us — a fixed 1/fps would lie
		// whenever the encoder's real rate differs (it usually does), skewing the
		// RTP timestamps and the viewer's recv_fps. Seed the first unit at ~30fps.
		now := time.Now()
		durationUs := uint64(33_333)
		if !last.IsZero() {
			if d := now.Sub(last).Microseconds(); d > 0 {
				durationUs = uint64(d)
			}
		}
		last = now

		if err := sess.pipe.WriteVideo(sess.network, sess.peer, sess.lane, durationUs, au); err != nil {
			log.Warnf("mesh: display pump write failed on route %s: %s — tearing down", sess.routeID, err)
			b.stopDisplayRoute(sess.routeID)
			return
		}

		// A Tune may have changed the capture cadence (a pull encoder's fps);
		// pick it up without restarting the pump.
		if ni := vs.CaptureInterval(); ni != interval {
			interval = ni
			ticker.Reset(interval)
		}
	}
}

// sendRouteReject sends a RouteControl Reject with a human-readable reason.
func (b *Bridge) sendRouteReject(network, from, routeID, reason string) {
	if err := b.sendControlTo(network, from, NewRouteReject(routeID, reason)); err != nil {
		log.Warnf("mesh: send route Reject to %s: %s", from, err)
	}
}

// stringsContain reports whether want is in xs.
func stringsContain(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// fmtU32 renders an optional u32 for a log line ("-" when unset).
func fmtU32(v *uint32) string {
	if v == nil {
		return "-"
	}
	return strconv.FormatUint(uint64(*v), 10)
}
