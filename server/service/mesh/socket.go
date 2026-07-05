package mesh

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

// daemon control protocol — line-delimited JSON over $MYOWNMESH_HOME/daemon.sock.
//
// Most ops are single-shot request → response. The exception is
// events_subscribe, which turns the connection into a one-way server-push
// stream of ServerOut frames (tagged by "kind"); we run a reader goroutine that
// dispatches channel_inbound frames to registered handlers.

// Request is a daemon control request: {"op": <snake_case>, ...op fields}.
// We build these as raw maps so we only ever emit the ops we actually use,
// without mirroring the daemon's entire Request enum.
type request map[string]interface{}

// Response is the single-shot reply shape: {ok, error?, data?}.
type Response struct {
	OK    bool            `json:"ok"`
	Error string          `json:"error,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

// ChannelInbound is a typed-channel frame the daemon pushes after a
// channel_subscribe. Mirrors ServerOut::ChannelInbound.
type ChannelInbound struct {
	Network string          `json:"network"`
	From    string          `json:"from"`
	Channel string          `json:"channel"`
	Payload json.RawMessage `json:"payload"`
}

// ChannelHandler is invoked for every channel_inbound frame on the event stream.
type ChannelHandler func(ChannelInbound)

// MeshEvent mirrors the daemon's engine event (myownmesh events.rs MeshEvent),
// delivered inside a ServerOut::Event frame: {"kind":"event","event":{...}}.
// The outer discriminator is "event_kind" (peer|phase|diag); a Peer/Phase
// event additionally carries an inner "kind" variant tag (a Diag has none).
// We decode a flat superset of only the fields the bridge reacts to — a diag's
// category/message, a phase's kind/prev/next — and ignore everything else,
// so new event families or fields are forward-compatible.
type MeshEvent struct {
	EventKind string `json:"event_kind"` // "peer" | "phase" | "diag"
	NetworkID string `json:"network_id"`

	// diag (event_kind == "diag"): category is an exact-match discriminator
	// ("network", "ice", "signaling", …); message is human text, not parsed.
	Level    string `json:"level"`
	Category string `json:"category"`
	Message  string `json:"message"`

	// phase (event_kind == "phase"): Kind == "changed", prev/next are MeshPhase.
	Kind string `json:"kind"`
	Prev string `json:"prev"`
	Next string `json:"next"`
}

// MeshEventHandler is invoked for every engine event frame on the event stream.
type MeshEventHandler func(MeshEvent)

// Socket is a connection to the myownmesh daemon control socket. A Socket is
// either a single-shot request connection or (after Subscribe) an event-stream
// connection carrying server-push frames. The bridge uses one of each.
type Socket struct {
	path string

	// reqMu serializes single-shot request/response round-trips so concurrent
	// callers (e.g. the presence loop and a greetPeer from the readLoop) can't
	// interleave writes or steal each other's reply line.
	reqMu sync.Mutex

	mu     sync.Mutex
	conn   net.Conn
	reader *bufio.Reader
	writer *bufio.Writer

	// event-stream state
	clientID     string
	handler      ChannelHandler
	eventHandler MeshEventHandler

	// onFatal fires once the request/response stream is compromised (a write,
	// read, or decode failure) — set by the bridge to tear the whole session
	// down and re-establish, rather than limping on a desynced socket.
	onFatal func()
}

// Dial connects to the daemon control socket at sockPath.
func Dial(sockPath string) (*Socket, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial daemon socket %s: %w", sockPath, err)
	}
	return &Socket{
		path:   sockPath,
		conn:   conn,
		reader: bufio.NewReaderSize(conn, 64*1024),
		writer: bufio.NewWriterSize(conn, 64*1024),
	}, nil
}

// Close closes the underlying connection.
func (s *Socket) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return nil
	}
	err := s.conn.Close()
	s.conn = nil
	return err
}

// writeLine encodes v as one JSON line and flushes it.
func (s *Socket) writeLine(v interface{}) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.conn == nil {
		return fmt.Errorf("socket closed")
	}
	if _, err := s.writer.Write(b); err != nil {
		return err
	}
	if err := s.writer.WriteByte('\n'); err != nil {
		return err
	}
	return s.writer.Flush()
}

// request sends a single-shot request and blocks for its Response. It must NOT
// be called on an event-stream socket (that connection only emits push frames
// after the ack).
// daemonReadTimeout bounds how long a single-shot request/ack read waits for the
// daemon. Without it, a daemon that's busy (e.g. mid peer-connection at boot) and
// slow to answer would hang the bridge's handshake forever — no node id, no
// error, no retry. On timeout the socket is treated as fatal (see request) and
// the bridge re-establishes.
//
// It must be well ABOVE the daemon's transient engine stalls, not right at their
// edge: a network change (e.g. the Virtual Network toggle changing usb0) makes
// the daemon drop every peer and restart ICE, which stalls the single-core
// engine driver for ~10 s while channel_send_* ops queue behind it. At 10 s this
// timeout tripped exactly there, turning a stall into a fatal reconnect — and the
// reconnect's re-subscribe/re-advertise piled more work onto the stalled engine,
// so it looped for a minute+ instead of settling. 45 s lets those ops BLOCK
// through the stall and succeed. A genuinely dead daemon is still caught
// promptly — the socket closes and readLoop fires onClose — so this longer bound
// only affects the alive-but-briefly-busy case it's meant to ride out.
const daemonReadTimeout = 45 * time.Second

func (s *Socket) request(req request) (Response, error) {
	// The op name in every error is what separates "daemon down" from "one
	// specific handler stalled" when reading /var/log/nanokvm-mesh.log — the
	// dispatch-path ops (identity_show, networks_list, channel_subscribe) are
	// answered by the connection task, while capabilities_set / channel_send_*
	// round-trip through the engine driver and stall when it is busy.
	op, _ := req["op"].(string)
	if op == "" {
		op = "unknown-op"
	}
	s.reqMu.Lock()
	defer s.reqMu.Unlock()
	if err := s.writeLine(req); err != nil {
		s.fatal()
		return Response{}, fmt.Errorf("%s: write request: %w", op, err)
	}
	// Snapshot the conn under s.mu: Close (from connectAndRun's teardown, a
	// different goroutine) nils s.conn, and an unguarded s.conn.SetReadDeadline
	// here would be a nil-pointer crash on whichever goroutine was mid-request
	// when the bridge tore down. Deadline calls on the snapshot are safe after
	// Close — a closed net.Conn returns errors, it doesn't panic.
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return Response{}, fmt.Errorf("%s: socket closed", op)
	}
	_ = conn.SetReadDeadline(time.Now().Add(daemonReadTimeout))
	line, err := s.reader.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		// A read failure (timeout, EOF, reset) breaks request/response
		// correlation for good: the daemon may still emit THIS op's reply
		// later, and the next request would then read that stale line as its
		// own — a permanent desync only a reconnect clears. This is the
		// "offline until restarted" path — an engine stall past
		// daemonReadTimeout (e.g. the ICE-restart fan-out a network change
		// kicks on this single-core device) timed out one request and left
		// the ctl socket corrupt for the rest of the process's life. Tear the
		// socket down and fire onFatal so the bridge re-establishes cleanly.
		s.fatal()
		return Response{}, fmt.Errorf("%s: read response: %w", op, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		// Undecodable line = framing desync; same reasoning as a read error.
		s.fatal()
		return Response{}, fmt.Errorf("%s: decode response: %w", op, err)
	}
	if !resp.OK {
		// A clean protocol-level refusal: the reply was read and framed
		// correctly, so the stream is still in sync — NOT fatal.
		return resp, fmt.Errorf("%s: daemon error: %s", op, resp.Error)
	}
	return resp, nil
}

// SetOnFatal registers a callback fired when this socket's request/response
// stream is compromised (a write, read, or decode failure). The bridge uses it
// to drop and re-establish the whole session instead of continuing on a
// desynced connection.
func (s *Socket) SetOnFatal(f func()) {
	s.mu.Lock()
	s.onFatal = f
	s.mu.Unlock()
}

// fatal closes the connection and invokes onFatal (if set). Closing first makes
// any concurrent or subsequent read fail fast rather than block on — or
// misread — a stream we can no longer trust. Idempotent: Close guards a nil
// conn, and the bridge's onFatal is itself sync.Once-guarded.
func (s *Socket) fatal() {
	s.mu.Lock()
	f := s.onFatal
	s.mu.Unlock()
	_ = s.Close()
	if f != nil {
		f()
	}
}

// ClientID returns the client id captured from the events_subscribe ack ("c<n>").
func (s *Socket) ClientID() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clientID
}

// Subscribe sends events_subscribe, captures the client_id from the ack, and
// starts the reader goroutine that dispatches channel_inbound frames to handler.
// It returns once the ack is received; the reader runs until the connection
// drops or onClose fires. onClose (may be nil) is invoked when the stream ends,
// so the caller can reconnect.
func (s *Socket) Subscribe(handler ChannelHandler, events MeshEventHandler, onClose func()) error {
	s.handler = handler
	s.eventHandler = events
	if err := s.writeLine(request{"op": "events_subscribe"}); err != nil {
		return err
	}
	// First line is the ack carrying client_id. Bound the wait (the long-lived
	// readLoop started below intentionally runs with no deadline). Same conn
	// snapshot as request(): a concurrent Close must produce an error, not a
	// nil dereference.
	s.mu.Lock()
	conn := s.conn
	s.mu.Unlock()
	if conn == nil {
		return fmt.Errorf("events_subscribe: socket closed")
	}
	_ = conn.SetReadDeadline(time.Now().Add(daemonReadTimeout))
	line, err := s.reader.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		return fmt.Errorf("read events_subscribe ack: %w", err)
	}
	var ack Response
	if err := json.Unmarshal(line, &ack); err != nil {
		return fmt.Errorf("decode events_subscribe ack: %w", err)
	}
	if !ack.OK {
		return fmt.Errorf("events_subscribe refused: %s", ack.Error)
	}
	var ackData struct {
		ClientID string `json:"client_id"`
	}
	_ = json.Unmarshal(ack.Data, &ackData)
	s.mu.Lock()
	s.clientID = ackData.ClientID
	s.mu.Unlock()
	if ackData.ClientID == "" {
		return fmt.Errorf("events_subscribe ack carried no client_id")
	}

	go s.readLoop(onClose)
	return nil
}

// readLoop dispatches server-push frames until the connection drops.
func (s *Socket) readLoop(onClose func()) {
	defer func() {
		if onClose != nil {
			onClose()
		}
	}()
	for {
		line, err := s.reader.ReadBytes('\n')
		if err != nil {
			log.Debugf("mesh: event stream ended: %s", err)
			return
		}
		var probe struct {
			Kind string `json:"kind"`
		}
		if err := json.Unmarshal(line, &probe); err != nil {
			continue
		}
		switch probe.Kind {
		case "channel_inbound":
			var ci ChannelInbound
			if err := json.Unmarshal(line, &ci); err != nil {
				continue
			}
			if s.handler != nil {
				s.handler(ci)
			}
		case "event":
			// ServerOut::Event{event: MeshEvent}. The bridge reacts to
			// network-change diags here (re-establishing on an interface flip).
			var frame struct {
				Event MeshEvent `json:"event"`
			}
			if err := json.Unmarshal(line, &frame); err != nil {
				continue
			}
			if s.eventHandler != nil {
				s.eventHandler(frame.Event)
			}
		default:
			// lagged / rpc_* / video_* / audio_* — not used by the bridge.
			// Ignored, never an error (additive forward-compat).
		}
	}
}

// ---- typed helpers ----------------------------------------------------------

// NetworkSummary is one entry of networks_list's data.networks.
type NetworkSummary struct {
	ConfigID  string `json:"config_id"`
	NetworkID string `json:"network_id"`
	// other fields (label/phase/topology) are ignored.
}

// NetworksList returns the daemon's currently-joined networks.
func (s *Socket) NetworksList() ([]NetworkSummary, error) {
	resp, err := s.request(request{"op": "networks_list"})
	if err != nil {
		return nil, err
	}
	var data struct {
		Networks []NetworkSummary `json:"networks"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, err
	}
	return data.Networks, nil
}

// NetworkAdd joins a network described by config (a NetworkConfig JSON object,
// see config.rs). config is passed as a generic map so we can build exactly the
// fields we want and let the daemon fill the rest with defaults.
func (s *Socket) NetworkAdd(config map[string]interface{}) error {
	_, err := s.request(request{"op": "network_add", "config": config})
	return err
}

// NetworkRemove leaves a network. purge additionally deletes the network's
// persisted governance state + roster — a genuine forget (leaving a fleet or
// walking off a mesh for good), not just unloading it for this run. Idempotent
// on the daemon side (removing an unknown id is success-with-warning).
func (s *Socket) NetworkRemove(network string, purge bool) error {
	_, err := s.request(request{
		"op":      "network_remove",
		"network": network,
		"purge":   purge,
	})
	return err
}

// IdentitySetLabel updates the daemon's device label — persisted to the
// identity anchor and advertised on the next handshake, no restart needed.
// Empty clears it (peers then fall back to the truncated device id). This is
// how an attached KVM names itself "KVM-<target>" at the myownmesh layer, not
// just on the AllMyStuff graph.
func (s *Socket) IdentitySetLabel(label string) error {
	_, err := s.request(request{"op": "identity_set_label", "label": label})
	return err
}

// ChannelSubscribe subscribes an event-stream client — named by the client_id
// from ITS events_subscribe ack — to a typed channel. Call this on the ctl
// socket, never on the events socket: after events_subscribe the daemon treats
// that connection as a one-way push stream and never reads from it again
// (myownmesh control.rs run_events_stream), so a request written there is never
// answered and dies as "read response: i/o timeout". That is the whole reason
// the op carries client_id — it names the event stream the frames should route
// to, while the request itself rides a command connection. Mirrors AllMyStuff's
// control client (node/src/control_client.rs + mesh.rs subscribe_channels).
func (s *Socket) ChannelSubscribe(clientID, network, channel string) error {
	if clientID == "" {
		return fmt.Errorf("channel_subscribe needs an events_subscribe client_id")
	}
	_, err := s.request(request{
		"op":        "channel_subscribe",
		"client_id": clientID,
		"network":   network,
		"channel":   channel,
	})
	return err
}

// ChannelSendAll broadcasts a frame on a typed channel to every active peer.
func (s *Socket) ChannelSendAll(network, channel string, payload interface{}) error {
	_, err := s.request(request{
		"op":      "channel_send_all",
		"network": network,
		"channel": channel,
		"payload": payload,
	})
	return err
}

// ChannelSendTo sends one frame on a typed channel to a specific peer.
func (s *Socket) ChannelSendTo(network, channel, peer string, payload interface{}) error {
	_, err := s.request(request{
		"op":      "channel_send_to",
		"network": network,
		"channel": channel,
		"peer":    peer,
		"payload": payload,
	})
	return err
}

// CapabilitiesSet replaces the network's advertised capability matrix. The
// capabilities value must be a CapabilityAdvert-shaped object (tags,
// app_version, max_connections, extra) — see bridge.go for how it's built.
func (s *Socket) CapabilitiesSet(network string, capabilities interface{}) error {
	_, err := s.request(request{
		"op":           "capabilities_set",
		"network":      network,
		"capabilities": capabilities,
	})
	return err
}

// RosterEntry is the subset of a roster_list entry we use. DeviceID is the
// canonical pubkey portion (base32, no display suffix) — the daemon's roster
// compares peers by exactly this value.
type RosterEntry struct {
	DeviceID string `json:"device_id"`
	Label    string `json:"label"`
}

// RosterList returns the approved-peers roster of a network.
func (s *Socket) RosterList(network string) ([]RosterEntry, error) {
	resp, err := s.request(request{"op": "roster_list", "network": network})
	if err != nil {
		return nil, err
	}
	var data struct {
		Roster []RosterEntry `json:"roster"`
	}
	if err := json.Unmarshal(resp.Data, &data); err != nil {
		return nil, err
	}
	return data.Roster, nil
}

// Identity is the subset of identity_show we use.
type Identity struct {
	DeviceID string `json:"device_id"`
	Pubkey   string `json:"pubkey"`
	Label    string `json:"label"`
}

// IdentityShow returns this daemon's device identity.
func (s *Socket) IdentityShow() (Identity, error) {
	resp, err := s.request(request{"op": "identity_show"})
	if err != nil {
		return Identity{}, err
	}
	var id Identity
	if err := json.Unmarshal(resp.Data, &id); err != nil {
		return Identity{}, err
	}
	return id, nil
}

// ConfigShow returns the daemon's full on-disk MeshConfig (used rarely).
func (s *Socket) ConfigShow() (json.RawMessage, error) {
	resp, err := s.request(request{"op": "config_show"})
	if err != nil {
		return nil, err
	}
	return resp.Data, nil
}

// ---- MediaTrackPipe (native video lane) -------------------------------------

// mediaKindVideo / mediaKindAudio are the frame `kind` byte of a native media
// frame (see encodeMediaFrame). Slice 1 only pushes video.
const (
	mediaKindVideo uint8 = 0
	mediaKindAudio uint8 = 1
)

// MediaTrackPipe is a SEPARATE daemon connection dedicated to pushing this KVM's
// already-encoded H.264 onto myownmesh's native RTP video-track lane, with zero
// base64. It is opened when a display route goes active and closed on teardown.
//
// Handshake: write one JSON line {"op":"media_track_pipe"}, read one JSON ack
// line {"ok":true,"data":{"media_track_pipe":true}}; thereafter the connection
// carries ONLY length-prefixed binary frames (see encodeMediaFrame).
type MediaTrackPipe struct {
	mu   sync.Mutex
	conn net.Conn
	w    *bufio.Writer
}

// DialMediaTrackPipe opens a fresh daemon connection and performs the
// media_track_pipe op handshake, returning a pipe ready for WriteVideo.
func DialMediaTrackPipe(sockPath string) (*MediaTrackPipe, error) {
	conn, err := net.DialTimeout("unix", sockPath, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("dial media pipe %s: %w", sockPath, err)
	}
	p := &MediaTrackPipe{conn: conn, w: bufio.NewWriterSize(conn, 64*1024)}

	if _, err := conn.Write([]byte(`{"op":"media_track_pipe"}` + "\n")); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("media_track_pipe handshake write: %w", err)
	}
	// One ack line. Bound the wait like every other daemon read; the binary
	// frames written afterwards use their own write deadline.
	r := bufio.NewReader(conn)
	_ = conn.SetReadDeadline(time.Now().Add(daemonReadTimeout))
	line, err := r.ReadBytes('\n')
	_ = conn.SetReadDeadline(time.Time{})
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("media_track_pipe ack read: %w", err)
	}
	var ack Response
	if err := json.Unmarshal(line, &ack); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("media_track_pipe ack decode: %w", err)
	}
	if !ack.OK {
		_ = conn.Close()
		return nil, fmt.Errorf("media_track_pipe refused: %s", ack.Error)
	}
	return p, nil
}

// WriteVideo frames one Annex-B H.264 access unit onto the native video lane.
// network is the display offer's network, peer is the console's canonical
// pubkey, lane is the announced lane index, durationUs is 1e6/fps. One access
// unit per call (no chunking); the daemon re-derives the IDR flag from the NAL
// stream, so no key/IDR flag is set here.
func (p *MediaTrackPipe) WriteVideo(network, peer string, lane uint8, durationUs uint64, au []byte) error {
	frame := encodeMediaFrame(mediaKindVideo, lane, durationUs, network, peer, au)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return fmt.Errorf("media pipe closed")
	}
	_ = p.conn.SetWriteDeadline(time.Now().Add(daemonReadTimeout))
	if _, err := p.w.Write(frame); err != nil {
		return err
	}
	return p.w.Flush()
}

// Close closes the pipe's daemon connection. Idempotent.
func (p *MediaTrackPipe) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.conn == nil {
		return nil
	}
	err := p.conn.Close()
	p.conn = nil
	return err
}

// encodeMediaFrame builds one length-prefixed native media frame per the
// AllMyStuff media_track_pipe wire contract (all integers little-endian, NO
// base64, NO JSON):
//
//	[u32 body_len][ kind:u8 · stream:u8 · duration_us:u64 ·
//	                net_len:u16 · network:UTF8 · peer_len:u16 · peer:UTF8 ·
//	                data:[remaining bytes = the Annex-B H.264 access unit] ]
func encodeMediaFrame(kind, stream uint8, durationUs uint64, network, peer string, data []byte) []byte {
	nb := []byte(network)
	pb := []byte(peer)
	bodyLen := 1 + 1 + 8 + 2 + len(nb) + 2 + len(pb) + len(data)
	buf := make([]byte, 4+bodyLen)
	binary.LittleEndian.PutUint32(buf[0:4], uint32(bodyLen))
	o := 4
	buf[o] = kind
	o++
	buf[o] = stream
	o++
	binary.LittleEndian.PutUint64(buf[o:o+8], durationUs)
	o += 8
	binary.LittleEndian.PutUint16(buf[o:o+2], uint16(len(nb)))
	o += 2
	o += copy(buf[o:], nb)
	binary.LittleEndian.PutUint16(buf[o:o+2], uint16(len(pb)))
	o += 2
	o += copy(buf[o:], pb)
	copy(buf[o:], data)
	return buf
}
