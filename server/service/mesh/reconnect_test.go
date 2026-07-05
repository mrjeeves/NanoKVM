package mesh

import (
	"bufio"
	"net"
	"sync/atomic"
	"testing"
	"time"
)

// newSocketPipe builds a Socket wired to an in-memory pipe, returning the
// Socket and the peer end the test scripts. Enough to exercise request() and
// readLoop() without a daemon.
func newSocketPipe() (*Socket, net.Conn) {
	c1, c2 := net.Pipe()
	s := &Socket{
		conn:   c1,
		reader: bufio.NewReaderSize(c1, 64*1024),
		writer: bufio.NewWriterSize(c1, 64*1024),
	}
	return s, c2
}

// TestRequestReadErrorIsFatal pins the desync fix: a read failure mid-request
// (here an EOF from a peer that never replies — a stand-in for the >10 s engine
// stall a network change kicks) must close the socket and fire onFatal, so the
// bridge re-establishes instead of limping on a stream whose next reply would
// be misread as the following request's. This is the "offline until restarted"
// root cause.
func TestRequestReadErrorIsFatal(t *testing.T) {
	s, peer := newSocketPipe()
	var fired atomic.Bool
	s.SetOnFatal(func() { fired.Store(true) })

	go func() {
		r := bufio.NewReader(peer)
		_, _ = r.ReadBytes('\n') // consume the request…
		_ = peer.Close()         // …then vanish without replying → EOF on read
	}()

	if _, err := s.request(request{"op": "identity_show"}); err == nil {
		t.Fatal("expected an error when the peer closes without replying")
	}
	if !fired.Load() {
		t.Fatal("onFatal must fire on a fatal read error")
	}
	s.mu.Lock()
	closed := s.conn == nil
	s.mu.Unlock()
	if !closed {
		t.Fatal("socket must be closed after a fatal error (fail fast, no desync)")
	}
}

// TestRequestProtocolErrorNotFatal guards the other side of the line: a clean
// {"ok":false} reply was read and framed correctly, so the stream is still in
// sync. It surfaces as an error but must NOT tear the session down — otherwise
// every idempotent-refusal (e.g. removing an unknown network) would flap the
// bridge.
func TestRequestProtocolErrorNotFatal(t *testing.T) {
	s, peer := newSocketPipe()
	var fired atomic.Bool
	s.SetOnFatal(func() { fired.Store(true) })

	go func() {
		r := bufio.NewReader(peer)
		_, _ = r.ReadBytes('\n')
		_, _ = peer.Write([]byte(`{"ok":false,"error":"nope"}` + "\n"))
	}()

	if _, err := s.request(request{"op": "identity_show"}); err == nil {
		t.Fatal("expected a protocol-level error")
	}
	if fired.Load() {
		t.Fatal("onFatal must NOT fire on a clean ok:false reply — the stream is synced")
	}
}

// TestReadLoopDispatchesEventFrame pins that the event stream now decodes the
// daemon's ServerOut::Event frames (previously discarded) and hands the inner
// MeshEvent to the handler — the wire path the network-change trigger rides.
func TestReadLoopDispatchesEventFrame(t *testing.T) {
	s, peer := newSocketPipe()
	got := make(chan MeshEvent, 1)
	s.eventHandler = func(ev MeshEvent) { got <- ev }
	closed := make(chan struct{})
	go s.readLoop(func() { close(closed) })

	// The exact frame myownmesh emits on an interface flip (network_watch.rs
	// on_network_change): a diag with category "network".
	frame := `{"kind":"event","event":{"event_kind":"diag","network_id":"cec-joining",` +
		`"level":"info","category":"network",` +
		`"message":"Primary network interface changed; renegotiating ICE with every active peer."}}` + "\n"
	if _, err := peer.Write([]byte(frame)); err != nil {
		t.Fatalf("write event frame: %v", err)
	}

	select {
	case ev := <-got:
		if ev.EventKind != "diag" || ev.Category != "network" || ev.NetworkID != "cec-joining" {
			t.Fatalf("decoded event = %+v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("event handler was not called for an event frame")
	}

	_ = peer.Close()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("onClose not fired after the stream ended")
	}
}

// TestOnMeshEventIsObserveOnly pins that consuming engine events never drives a
// reconnect and never panics — it's diagnostics only. Reacting to a
// network-change diag with a full re-establish used to pile load onto the
// daemon's in-flight ICE restart and turn a brief blip into a reconnect loop;
// the bridge now blocks through the stall instead (see daemonReadTimeout).
func TestOnMeshEventIsObserveOnly(t *testing.T) {
	b := &Bridge{}
	// A range of events, including the network-change diag that used to trigger
	// a re-establish — all must be safe no-ops (no channel, no reconnect).
	b.onMeshEvent(MeshEvent{EventKind: "diag", Category: "ice", NetworkID: "n"})
	b.onMeshEvent(MeshEvent{EventKind: "phase", Kind: "changed", NetworkID: "n"})
	b.onMeshEvent(MeshEvent{EventKind: "diag", Category: "network", NetworkID: "n", Message: "Primary network interface changed"})
	b.onMeshEvent(MeshEvent{EventKind: "peer", Kind: "dropped", NetworkID: "n"})
}
