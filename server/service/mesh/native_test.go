package mesh

import (
	"encoding/binary"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"NanoKVM-Server/config"

	"github.com/gin-gonic/gin"
)

// ---- encodeMediaFrame -------------------------------------------------------

func TestEncodeMediaFrameRoundTrip(t *testing.T) {
	network := "amber-turing-x3k9q"
	peer := "console-abcdef"
	au := []byte{0x00, 0x00, 0x00, 0x01, 0x67, 0x42, 0x00, 0x00, 0x00, 0x01, 0x65, 0x88}
	const durUs uint64 = 1_000_000 / 30
	const lane uint8 = 2

	frame := encodeMediaFrame(mediaKindVideo, lane, durUs, network, peer, au)

	// Outer length prefix covers the whole body.
	if len(frame) < 4 {
		t.Fatalf("frame too short: %d", len(frame))
	}
	bodyLen := binary.LittleEndian.Uint32(frame[0:4])
	if int(bodyLen) != len(frame)-4 {
		t.Fatalf("body_len = %d, want %d", bodyLen, len(frame)-4)
	}

	// Decode the body field by field.
	o := 4
	if frame[o] != mediaKindVideo {
		t.Fatalf("kind = %d, want %d (video)", frame[o], mediaKindVideo)
	}
	o++
	if frame[o] != lane {
		t.Fatalf("stream = %d, want %d", frame[o], lane)
	}
	o++
	if got := binary.LittleEndian.Uint64(frame[o : o+8]); got != durUs {
		t.Fatalf("duration_us = %d, want %d", got, durUs)
	}
	o += 8
	netLen := int(binary.LittleEndian.Uint16(frame[o : o+2]))
	o += 2
	if got := string(frame[o : o+netLen]); got != network {
		t.Fatalf("network = %q, want %q", got, network)
	}
	o += netLen
	peerLen := int(binary.LittleEndian.Uint16(frame[o : o+2]))
	o += 2
	if got := string(frame[o : o+peerLen]); got != peer {
		t.Fatalf("peer = %q, want %q", got, peer)
	}
	o += peerLen
	if got := frame[o:]; string(got) != string(au) {
		t.Fatalf("data = %x, want %x", got, au)
	}
}

// ---- DecodeInputEvent -------------------------------------------------------

func TestDecodeInputEvent(t *testing.T) {
	move, ok := DecodeInputEvent(json.RawMessage(
		`{"t":"input","route":"r1","seq":5,"kind":"mouse_move","x":0.25,"y":0.75,"screen":1}`))
	if !ok || move.Route != "r1" || move.Seq != 5 || move.Action.Kind != InputActionMouseMove {
		t.Fatalf("mouse_move decoded wrong: %+v ok=%v", move, ok)
	}
	if move.Action.X != 0.25 || move.Action.Y != 0.75 || move.Action.Screen == nil || *move.Action.Screen != 1 {
		t.Fatalf("mouse_move fields wrong: %+v", move.Action)
	}

	btn, ok := DecodeInputEvent(json.RawMessage(
		`{"t":"input","route":"r1","seq":6,"kind":"mouse_button","button":2,"down":true}`))
	if !ok || btn.Action.Kind != InputActionMouseButton || btn.Action.Button != 2 || !btn.Action.Down {
		t.Fatalf("mouse_button decoded wrong: %+v", btn.Action)
	}

	wheel, ok := DecodeInputEvent(json.RawMessage(
		`{"t":"input","route":"r1","seq":7,"kind":"wheel","dx":0,"dy":-3.5}`))
	if !ok || wheel.Action.Kind != InputActionWheel || wheel.Action.DY != -3.5 {
		t.Fatalf("wheel decoded wrong: %+v", wheel.Action)
	}

	key, ok := DecodeInputEvent(json.RawMessage(
		`{"t":"input","route":"r1","seq":8,"kind":"key","key":"a","code":"KeyA","down":true}`))
	if !ok || key.Action.Kind != InputActionKey || key.Action.Key != "a" || key.Action.Code != "KeyA" || !key.Action.Down {
		t.Fatalf("key decoded wrong: %+v", key.Action)
	}

	// An unknown action kind decodes to Unknown rather than failing.
	unknown, ok := DecodeInputEvent(json.RawMessage(
		`{"t":"input","route":"r1","seq":9,"kind":"gamepad","axis":3}`))
	if !ok || unknown.Action.Kind != InputActionUnknown {
		t.Fatalf("unknown action should map to Unknown: %+v ok=%v", unknown.Action, ok)
	}

	// A non-input media frame is not an InputEvent.
	if _, ok := DecodeInputEvent(json.RawMessage(`{"t":"site","route":"r","seq":1,"kind":"open"}`)); ok {
		t.Fatalf("site frame should not decode as input")
	}
}

// ---- Route media detection --------------------------------------------------

func TestRouteMediaDetection(t *testing.T) {
	display := Route{ID: "d", From: "console:in", To: "kvm-x:screen", Media: RouteMediaDisplay}
	if !display.IsDisplayRoute() || display.IsInputRoute() || display.IsSiteRoute() {
		t.Fatalf("display route misdetected: %+v", display)
	}

	input := Route{ID: "i", From: "console:out", To: "kvm-x:control", Media: RouteMediaInput}
	if !input.IsInputRoute() || input.IsDisplayRoute() || input.IsSiteRoute() {
		t.Fatalf("input route misdetected: %+v", input)
	}

	// media empty but the KVM's cap-id suffix is authoritative fallback.
	bySuffixDisplay := Route{From: "kvm-x:screen"}
	if !bySuffixDisplay.IsDisplayRoute() {
		t.Fatalf(":screen suffix should be a display route: %+v", bySuffixDisplay)
	}
	bySuffixInput := Route{To: "kvm-x:control"}
	if !bySuffixInput.IsInputRoute() {
		t.Fatalf(":control suffix should be an input route: %+v", bySuffixInput)
	}

	// A site route is neither display nor input.
	site := Route{From: "peer:site", To: "kvm:site-view:0", Media: "generic"}
	if !site.IsSiteRoute() || site.IsDisplayRoute() || site.IsInputRoute() {
		t.Fatalf("site route misdetected: %+v", site)
	}
}

// ---- NewRouteVideoLane ------------------------------------------------------

func TestNewRouteVideoLaneJSON(t *testing.T) {
	// lane 0 must still appear on the wire (a valid lane index).
	s := string(NewRouteVideoLane("route-1", 0).Payload())
	for _, want := range []string{`"t":"route"`, `"kind":"video_lane"`, `"route_id":"route-1"`, `"lane":0`} {
		if !strings.Contains(s, want) {
			t.Fatalf("video_lane missing %s: %s", want, s)
		}
	}
	// And it decodes back as a video_lane route control.
	msg, err := DecodeControlMessage(NewRouteVideoLane("route-1", 3).Payload())
	if err != nil || msg.Route == nil || msg.Route.Kind != RouteControlKindVideoLane {
		t.Fatalf("video_lane doesn't round trip: %+v %v", msg, err)
	}
}

// ---- RouteControl offer/tune decoding ---------------------------------------

func TestRouteControlOfferCarriesVideoTransports(t *testing.T) {
	raw := json.RawMessage(`{"t":"route","kind":"offer","session":"s1","video":["h264"],"audio":["opus"],
		"route":{"id":"d1","from":"console:out","to":"kvm-x:screen","media":"display"}}`)
	msg, err := DecodeControlMessage(raw)
	if err != nil || msg.Route == nil || msg.Route.Kind != RouteControlKindOffer {
		t.Fatalf("display offer decoded wrong: %+v %v", msg, err)
	}
	if !msg.Route.Route.IsDisplayRoute() {
		t.Fatalf("offer route should be a display route: %+v", msg.Route.Route)
	}
	if len(msg.Route.Video) != 1 || msg.Route.Video[0] != "h264" || msg.Route.Session != "s1" {
		t.Fatalf("offer transports/session wrong: %+v", msg.Route)
	}
}

func TestRouteControlTuneDecoding(t *testing.T) {
	msg, err := DecodeControlMessage(json.RawMessage(
		`{"t":"route","kind":"tune","route_id":"d1","max_edge":1280,"fps":24}`))
	if err != nil || msg.Route == nil || msg.Route.Kind != RouteControlKindTune {
		t.Fatalf("tune decoded wrong: %+v %v", msg, err)
	}
	if msg.Route.MaxEdge == nil || *msg.Route.MaxEdge != 1280 {
		t.Fatalf("tune max_edge wrong: %+v", msg.Route.MaxEdge)
	}
	if msg.Route.FPS == nil || *msg.Route.FPS != 24 {
		t.Fatalf("tune fps wrong: %+v", msg.Route.FPS)
	}
	// bitrate omitted → nil ("leave unchanged").
	if msg.Route.Bitrate != nil {
		t.Fatalf("omitted bitrate should be nil, got %v", *msg.Route.Bitrate)
	}
}

// ---- native route arms (integration against the fake daemon) ----------------

type fakeVideoSource struct {
	read chan struct{}
}

func (f *fakeVideoSource) Params() VideoParams {
	return VideoParams{Width: 0, Height: 0, FPS: 50, BitRate: 3000}
}
func (f *fakeVideoSource) ReadH264(_, _, _ int) ([]byte, int) {
	select {
	case f.read <- struct{}{}:
	default:
	}
	return []byte{0x00, 0x00, 0x00, 0x01, 0x65, 0x88}, 0
}
func (f *fakeVideoSource) SetFps(int)                     {}
func (f *fakeVideoSource) Tune(_, _, _ *uint32)           {}
func (f *fakeVideoSource) ForceIDR()                      {}
func (f *fakeVideoSource) Prepare()                       {}
func (f *fakeVideoSource) CaptureInterval() time.Duration { return 2 * time.Millisecond }

type fakeInputSink struct {
	mu      sync.Mutex
	actions []InputAction
}

func (f *fakeInputSink) Apply(a InputAction) {
	f.mu.Lock()
	f.actions = append(f.actions, a)
	f.mu.Unlock()
}
func (f *fakeInputSink) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.actions)
}

// newNativeTestBridge wires a Bridge to a fake daemon with an owner recorded, so
// senderMayControl(owner) passes and the media pipe can be dialed.
func newNativeTestBridge(t *testing.T, f *fakeDaemon, owner string) *Bridge {
	t.Helper()
	events, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	t.Cleanup(func() { _ = events.Close() })
	ctl, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial ctl: %v", err)
	}
	t.Cleanup(func() { _ = ctl.Close() })
	if err := events.Subscribe(nil, nil, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	st := LoadState(t.TempDir())
	if !st.TryClaim(owner, "") {
		t.Fatalf("seed owner failed")
	}

	b := &Bridge{
		conf:    &config.Config{},
		mesh:    config.Mesh{Socket: f.sock, Name: "CEC-KVM"},
		state:   st,
		events:  events,
		ctl:     ctl,
		running: true,
		lanes:   make(map[uint8]bool),
	}
	b.sites = newSiteHost(gin.New(), 80, func(string, SiteFrame) error { return nil })
	return b
}

func TestDisplayRouteOfferAcceptsPumpsAndTearsDown(t *testing.T) {
	f := startFakeDaemon(t)
	b := newNativeTestBridge(t, f, "owner")
	vs := &fakeVideoSource{read: make(chan struct{}, 4)}
	b.SetVideoSource(vs)

	offer := &RouteControl{
		Kind:  RouteControlKindOffer,
		Video: []string{"h264"},
		Route: &Route{ID: "disp-1", From: "console:out", To: "kvm:screen", Media: RouteMediaDisplay},
	}
	b.handleRoute("netA", "owner", offer)

	b.mu.Lock()
	sess := b.display
	b.mu.Unlock()
	if sess == nil || sess.routeID != "disp-1" || sess.lane != 0 {
		t.Fatalf("display route not armed: %+v", sess)
	}

	// The pump should read at least one access unit within a reasonable window.
	select {
	case <-vs.read:
	case <-time.After(2 * time.Second):
		t.Fatal("video pump never called ReadH264")
	}

	// Accept + VideoLane were sent to the offerer.
	sends := f.requests("channel_send_to")
	var sawAccept, sawLane bool
	for _, r := range sends {
		p, _ := json.Marshal(r["payload"])
		s := string(p)
		if strings.Contains(s, `"kind":"accept"`) && strings.Contains(s, `"route_id":"disp-1"`) {
			sawAccept = true
		}
		if strings.Contains(s, `"kind":"video_lane"`) && strings.Contains(s, `"lane":0`) {
			sawLane = true
		}
	}
	if !sawAccept || !sawLane {
		t.Fatalf("expected Accept+VideoLane sends, got accept=%v lane=%v (%d sends)", sawAccept, sawLane, len(sends))
	}

	// Teardown stops the pump and frees the lane.
	b.handleRoute("netA", "owner", &RouteControl{Kind: RouteControlKindTeardown, RouteID: "disp-1"})
	deadline := time.Now().Add(2 * time.Second)
	for {
		b.mu.Lock()
		gone := b.display == nil
		freed := !b.lanes[0]
		b.mu.Unlock()
		if gone && freed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("display route not torn down")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestDisplayRouteRejectsUnauthorizedAndUnavailable(t *testing.T) {
	f := startFakeDaemon(t)
	b := newNativeTestBridge(t, f, "owner")
	// No video source injected → offered display route is rejected.
	offer := &RouteControl{
		Kind:  RouteControlKindOffer,
		Video: []string{"h264"},
		Route: &Route{ID: "disp-x", Media: RouteMediaDisplay, To: "kvm:screen"},
	}
	b.handleRoute("netA", "owner", offer)
	b.mu.Lock()
	armed := b.display != nil
	b.mu.Unlock()
	if armed {
		t.Fatal("display route should not arm without a video source")
	}

	// A stranger (not the owner) is rejected even with a source present.
	b.SetVideoSource(&fakeVideoSource{read: make(chan struct{}, 1)})
	b.handleRoute("netA", "stranger", &RouteControl{
		Kind:  RouteControlKindOffer,
		Route: &Route{ID: "disp-y", Media: RouteMediaDisplay, To: "kvm:screen"},
	})
	b.mu.Lock()
	armed = b.display != nil
	b.mu.Unlock()
	if armed {
		t.Fatal("display route from non-owner should be rejected")
	}

	// Both refusals should have produced a Reject to the offerer.
	if rejects := countRejects(f); rejects < 2 {
		t.Fatalf("expected >=2 route rejects, got %d", rejects)
	}
}

func TestInputRouteOfferRegistersAndInjects(t *testing.T) {
	f := startFakeDaemon(t)
	b := newNativeTestBridge(t, f, "owner")
	sink := &fakeInputSink{}
	b.SetInputSink(sink)

	offer := &RouteControl{
		Kind:  RouteControlKindOffer,
		Route: &Route{ID: "in-1", From: "console:out", To: "kvm:control", Media: RouteMediaInput},
	}
	b.handleRoute("netA", "owner", offer)

	b.mu.Lock()
	route := b.inputRoute
	b.mu.Unlock()
	if route != "in-1" {
		t.Fatalf("input route not registered: %q", route)
	}

	// An InputEvent on that route from the offerer is injected.
	ci := ChannelInbound{
		Network: "netA",
		From:    "owner",
		Channel: ChannelMedia,
		Payload: json.RawMessage(`{"t":"input","route":"in-1","seq":1,"kind":"mouse_move","x":0.5,"y":0.5}`),
	}
	b.onChannelInbound(ci)
	if sink.count() != 1 {
		t.Fatalf("input event not injected: %d actions", sink.count())
	}

	// An event from a stranger (or on a foreign route) is dropped.
	b.onChannelInbound(ChannelInbound{
		Network: "netA", From: "stranger", Channel: ChannelMedia,
		Payload: json.RawMessage(`{"t":"input","route":"in-1","seq":2,"kind":"key","code":"KeyA","down":true}`),
	})
	b.onChannelInbound(ChannelInbound{
		Network: "netA", From: "owner", Channel: ChannelMedia,
		Payload: json.RawMessage(`{"t":"input","route":"other","seq":3,"kind":"key","code":"KeyA","down":true}`),
	})
	if sink.count() != 1 {
		t.Fatalf("unauthorized/foreign-route input should be dropped, got %d actions", sink.count())
	}

	// Teardown clears the route.
	b.handleRoute("netA", "owner", &RouteControl{Kind: RouteControlKindTeardown, RouteID: "in-1"})
	b.mu.Lock()
	cleared := b.inputRoute == ""
	b.mu.Unlock()
	if !cleared {
		t.Fatal("input route not cleared on teardown")
	}
}

// countRejects returns how many channel_send_to requests carried a route Reject.
func countRejects(f *fakeDaemon) int {
	n := 0
	for _, r := range f.requests("channel_send_to") {
		p, _ := json.Marshal(r["payload"])
		if strings.Contains(string(p), `"kind":"reject"`) {
			n++
		}
	}
	return n
}
