package mesh

import (
	"bufio"
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
	clientID string
	handler  ChannelHandler
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
// error, no retry. On timeout connectAndRun returns and Start reconnects.
const daemonReadTimeout = 10 * time.Second

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
		return Response{}, fmt.Errorf("%s: write request: %w", op, err)
	}
	_ = s.conn.SetReadDeadline(time.Now().Add(daemonReadTimeout))
	line, err := s.reader.ReadBytes('\n')
	_ = s.conn.SetReadDeadline(time.Time{})
	if err != nil {
		return Response{}, fmt.Errorf("%s: read response: %w", op, err)
	}
	var resp Response
	if err := json.Unmarshal(line, &resp); err != nil {
		return Response{}, fmt.Errorf("%s: decode response: %w", op, err)
	}
	if !resp.OK {
		return resp, fmt.Errorf("%s: daemon error: %s", op, resp.Error)
	}
	return resp, nil
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
func (s *Socket) Subscribe(handler ChannelHandler, onClose func()) error {
	s.handler = handler
	if err := s.writeLine(request{"op": "events_subscribe"}); err != nil {
		return err
	}
	// First line is the ack carrying client_id. Bound the wait (the long-lived
	// readLoop started below intentionally runs with no deadline).
	_ = s.conn.SetReadDeadline(time.Now().Add(daemonReadTimeout))
	line, err := s.reader.ReadBytes('\n')
	_ = s.conn.SetReadDeadline(time.Time{})
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
		default:
			// event / lagged / rpc_* / video_* / audio_* — not used by the
			// bridge. Ignored, never an error (additive forward-compat).
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
