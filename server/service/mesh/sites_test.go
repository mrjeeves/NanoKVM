package mesh

import (
	"bytes"
	"io"
	"sync"
	"testing"
	"time"
)

// captureSend records every outbound SiteFrame from a meshConn.
type captureSend struct {
	mu     sync.Mutex
	frames []SiteFrame
}

func (c *captureSend) fn(f SiteFrame) error {
	c.mu.Lock()
	c.frames = append(c.frames, f)
	c.mu.Unlock()
	return nil
}

func (c *captureSend) snapshot() []SiteFrame {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]SiteFrame(nil), c.frames...)
}

func TestMeshConnWriteChunksAndFrames(t *testing.T) {
	cap := &captureSend{}
	c := newMeshConn("route:r", 5, cap.fn)

	// A write larger than SiteChunkBytes must split into multiple Data frames,
	// each <= SiteChunkBytes, with incrementing seq and the right conn id.
	payload := bytes.Repeat([]byte("x"), SiteChunkBytes+1234)
	n, err := c.Write(payload)
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if n != len(payload) {
		t.Fatalf("short write: %d != %d", n, len(payload))
	}

	frames := cap.snapshot()
	if len(frames) != 2 {
		t.Fatalf("expected 2 chunks, got %d", len(frames))
	}
	var rebuilt []byte
	for i, f := range frames {
		if f.Kind != SiteEventKindData {
			t.Fatalf("frame %d not Data: %+v", i, f)
		}
		if f.Conn != 5 || f.Route != "route:r" {
			t.Fatalf("frame %d wrong conn/route: %+v", i, f)
		}
		if uint64(i) != f.Seq {
			t.Fatalf("frame %d seq should be %d, got %d", i, i, f.Seq)
		}
		if len(f.Data) > SiteChunkBytes {
			t.Fatalf("frame %d exceeds chunk size: %d", i, len(f.Data))
		}
		rebuilt = append(rebuilt, f.Data...)
	}
	if !bytes.Equal(rebuilt, payload) {
		t.Fatalf("reassembled payload mismatch")
	}
}

func TestMeshConnReadFromInboundData(t *testing.T) {
	cap := &captureSend{}
	c := newMeshConn("route:r", 1, cap.fn)

	// Feed two Data chunks; Read must return them in order, splitting across
	// small buffers (leftover handling).
	c.feed([]byte("hello "))
	c.feed([]byte("world"))

	got := make([]byte, 0, 11)
	buf := make([]byte, 4)
	deadline := time.Now().Add(time.Second)
	for len(got) < 11 {
		_ = c.SetReadDeadline(deadline)
		n, err := c.Read(buf)
		if err != nil {
			t.Fatalf("read: %v (got %q)", err, got)
		}
		got = append(got, buf[:n]...)
	}
	if string(got) != "hello world" {
		t.Fatalf("read mismatch: %q", got)
	}
}

func TestMeshConnRemoteCloseGivesEOF(t *testing.T) {
	cap := &captureSend{}
	c := newMeshConn("route:r", 1, cap.fn)

	c.feed([]byte("tail"))
	c.remoteClose()

	// Drain the buffered Data first, then EOF.
	buf := make([]byte, 16)
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	n, err := c.Read(buf)
	if err != nil {
		t.Fatalf("first read should return buffered data: %v", err)
	}
	if string(buf[:n]) != "tail" {
		t.Fatalf("buffered data wrong: %q", buf[:n])
	}
	_ = c.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("after remote close + drain, expected EOF, got %v", err)
	}
}

func TestMeshConnCloseEmitsCloseFrame(t *testing.T) {
	cap := &captureSend{}
	c := newMeshConn("route:r", 9, cap.fn)

	if err := c.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	frames := cap.snapshot()
	if len(frames) != 1 || frames[0].Kind != SiteEventKindClose {
		t.Fatalf("close should emit exactly one Close frame, got %+v", frames)
	}
	if frames[0].Conn != 9 || frames[0].Route != "route:r" {
		t.Fatalf("close frame wrong conn/route: %+v", frames[0])
	}
	// Closing again is a no-op (no extra frames), and Read returns EOF.
	if err := c.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
	if got := cap.snapshot(); len(got) != 1 {
		t.Fatalf("second close should not emit a frame, got %d", len(got))
	}
	buf := make([]byte, 4)
	if _, err := c.Read(buf); err != io.EOF {
		t.Fatalf("read after close should be EOF, got %v", err)
	}
}

func TestMeshConnReadDeadline(t *testing.T) {
	cap := &captureSend{}
	c := newMeshConn("route:r", 1, cap.fn)

	_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	buf := make([]byte, 4)
	_, err := c.Read(buf)
	ne, ok := err.(interface{ Timeout() bool })
	if !ok || !ne.Timeout() {
		t.Fatalf("expected a timeout error, got %v", err)
	}
}

func TestSiteHostOpenAllowListAndDataRoundTrip(t *testing.T) {
	// Verify the host demux: an Open on the allowed port creates a meshConn that
	// receives subsequent Data and is closed on Close, while an Open on a
	// disallowed port is refused with a Close frame.
	var outMu sync.Mutex
	var out []SiteFrame
	send := func(peer string, f SiteFrame) error {
		outMu.Lock()
		out = append(out, f)
		outMu.Unlock()
		return nil
	}

	const allowed = uint16(80)
	h := newSiteHost(nil, allowed, send) // nil engine: we only test demux/conn wiring here
	// Don't drive HTTP — this test reads from the conn directly to verify the
	// demux feeds it, so override the serve hook to a no-op.
	h.serveConn = func(*meshConn) {}
	const route = "route:peer:site->kvm:site-view:0"
	const peer = "peer-AB12C"
	h.markRouteActive(route, peer)

	// Disallowed port → refused with a Close frame, no conn created.
	h.handleFrame(peer, NewSiteOpen(route, 0, 1, 8080))
	outMu.Lock()
	refused := len(out) == 1 && out[0].Kind == SiteEventKindClose && out[0].Conn == 1
	outMu.Unlock()
	if !refused {
		t.Fatalf("disallowed port should be refused with a Close frame, got %+v", out)
	}
	if h.lookup(route, 1) != nil {
		t.Fatalf("no conn should exist for a refused open")
	}

	// Allowed port → a conn is created (the gin engine is nil, so we don't drive
	// HTTP; we just confirm the conn exists and receives Data, then Close).
	h.handleFrame(peer, NewSiteOpen(route, 0, 2, allowed))
	conn := waitForConn(t, h, route, 2)

	// Data is fed to the conn's inbound; a Read returns it.
	h.handleFrame(peer, NewSiteData(route, 1, 2, []byte("PING")))
	buf := make([]byte, 8)
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	n, err := conn.Read(buf)
	if err != nil {
		t.Fatalf("conn read after Data: %v", err)
	}
	if string(buf[:n]) != "PING" {
		t.Fatalf("conn got %q, want PING", buf[:n])
	}

	// Close from the peer → the conn sees EOF on the next read.
	h.handleFrame(peer, NewSiteClose(route, 2, 2))
	_ = conn.SetReadDeadline(time.Now().Add(time.Second))
	if _, err := conn.Read(buf); err != io.EOF {
		t.Fatalf("after remote Close expected EOF, got %v", err)
	}

	// A frame on an inactive route is dropped (no conn created).
	h.handleFrame(peer, NewSiteOpen("route:other", 0, 3, allowed))
	if h.lookup("route:other", 3) != nil {
		t.Fatalf("frame on inactive route must be dropped")
	}

	// Teardown closes everything for the route.
	h.tearDownRoute(route)
	if _, active := h.routePeer(route); active {
		t.Fatalf("route should be inactive after teardown")
	}
}

// waitForConn polls for the host to register a conn (handleOpen creates it
// synchronously, but serve() runs in a goroutine — the conn map insert is
// synchronous, so a short poll is just defensive).
func waitForConn(t *testing.T, h *siteHost, route string, conn uint64) *meshConn {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if c := h.lookup(route, conn); c != nil {
			return c
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("conn %d on route %s never registered", conn, route)
	return nil
}
