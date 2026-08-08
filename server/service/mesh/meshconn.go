package mesh

import (
	"errors"
	"io"
	"net"
	"sync"
	"time"
)

// meshConn adapts one tunneled site-route connection to net.Conn so the gin
// engine can be served over it with http.Serve.
//
//   - Read pulls bytes from an inbound channel fed by SiteEvent Data frames.
//   - Write splits the payload into SITE_CHUNK_BYTES pieces and emits each as a
//     SiteEvent Data frame (with an incrementing seq) via the send func.
//   - Close emits a SiteEvent Close frame and unblocks any pending Read.
//
// Because Close/Read/Write satisfy net.Conn, http.Serve's connection hijack
// (used for WebSocket upgrade) hands the caller this very meshConn, so upgraded
// streams ride the tunnel transparently.
type meshConn struct {
	route string
	conn  uint64

	// send emits one outbound SiteFrame on the media channel. It returns an
	// error if the daemon send failed; meshConn treats that as a write error.
	send func(SiteFrame) error
	// seq is the outbound frame sequence (this conn's own counter). Guarded by
	// writeMu so concurrent writes stay ordered.
	writeMu sync.Mutex
	seq     uint64

	// inbound carries decoded Data payloads from the route demux.
	inbound chan []byte
	// leftover holds bytes from a Data chunk a Read didn't fully consume.
	leftover []byte

	closeOnce sync.Once
	closed    chan struct{}
	// closeErr, if set, makes the next Read return it instead of io.EOF — used
	// when the remote half closed.
	remoteClosed chan struct{}

	readDeadline  time.Time
	writeDeadline time.Time
	deadlineMu    sync.Mutex
	// deadlineCh is closed and replaced whenever a read deadline is set, so a
	// Read already parked in its select re-evaluates instead of sleeping
	// through it. net/http depends on exactly that: finishRequest calls
	// abortPendingRead, which sets a deadline in the past and then WAITS for
	// the in-flight read to return. A conn that only sampled the deadline on
	// entry never returned, so the request never finished, the connection was
	// never closed, and every tunneled browser connection leaked a goroutine
	// and a table entry — until the browser hit its per-host connection cap
	// and simply stopped fetching the rest of the page.
	deadlineCh chan struct{}
}

// newMeshConn builds a meshConn for one tunneled connection.
func newMeshConn(route string, conn uint64, send func(SiteFrame) error) *meshConn {
	return &meshConn{
		route:        route,
		conn:         conn,
		send:         send,
		inbound:      make(chan []byte, 64),
		closed:       make(chan struct{}),
		remoteClosed: make(chan struct{}),
		deadlineCh:   make(chan struct{}),
	}
}

// feedStallTimeout bounds how long feed waits for the HTTP consumer to drain a
// full inbound buffer. feed runs on the event-stream readLoop — the single
// goroutine that also carries claims, attach/detach, presence greets, and
// every OTHER tunnel's frames — so a consumer that stops reading must cost the
// stream one bounded stall, not freeze the bridge until the consumer recovers.
const feedStallTimeout = 5 * time.Second

// feed delivers one inbound Data payload. It never blocks the demux on a closed
// conn, and a conn whose consumer has stalled past feedStallTimeout with a full
// buffer (64 chunks ≈ 2.5 MB backlog) is closed as dead rather than allowed to
// head-of-line-block the whole event stream.
func (m *meshConn) feed(data []byte) {
	if len(data) == 0 {
		return
	}
	// Copy: the caller's buffer (from JSON decode) may be reused.
	cp := make([]byte, len(data))
	copy(cp, data)
	select {
	case m.inbound <- cp:
	case <-m.closed:
	default:
		// Buffer full — give the consumer a bounded grace, then declare it dead.
		t := time.NewTimer(feedStallTimeout)
		defer t.Stop()
		select {
		case m.inbound <- cp:
		case <-m.closed:
		case <-t.C:
			_ = m.Close()
		}
	}
}

// remoteClose signals that the far side closed its half — a subsequent Read,
// once inbound is drained, returns EOF.
func (m *meshConn) remoteClose() {
	select {
	case <-m.remoteClosed:
		// already
	default:
		close(m.remoteClosed)
	}
}

// Read implements net.Conn.
func (m *meshConn) Read(p []byte) (int, error) {
	// Serve any leftover from a previous Data chunk first.
	if len(m.leftover) > 0 {
		n := copy(p, m.leftover)
		m.leftover = m.leftover[n:]
		return n, nil
	}

	// Re-entered whenever the deadline moves under us, so a deadline set after
	// this Read parked still takes effect (see deadlineCh).
	for {
		m.deadlineMu.Lock()
		dl := m.readDeadline
		changed := m.deadlineCh
		m.deadlineMu.Unlock()

		var timeout <-chan time.Time
		var timer *time.Timer
		if !dl.IsZero() {
			d := time.Until(dl)
			if d <= 0 {
				return 0, timeoutErr{}
			}
			timer = time.NewTimer(d)
			timeout = timer.C
		}

		n, err, done := m.readOnce(p, timeout, changed)
		if timer != nil {
			timer.Stop()
		}
		if done {
			return n, err
		}
	}
}

// readOnce is one pass of Read's select. done is false only when the deadline
// changed and the caller must re-evaluate it.
func (m *meshConn) readOnce(p []byte, timeout <-chan time.Time, changed <-chan struct{}) (int, error, bool) {
	select {
	case <-m.closed:
		return 0, io.EOF, true
	case data := <-m.inbound:
		n := copy(p, data)
		if n < len(data) {
			m.leftover = data[n:]
		}
		return n, nil, true
	case <-timeout:
		return 0, timeoutErr{}, true
	case <-changed:
		return 0, nil, false
	case <-m.remoteClosed:
		// Drain any racing buffered Data before reporting EOF.
		select {
		case data := <-m.inbound:
			n := copy(p, data)
			if n < len(data) {
				m.leftover = data[n:]
			}
			return n, nil, true
		default:
			return 0, io.EOF, true
		}
	}
}

// Write implements net.Conn — chunks p into SITE_CHUNK_BYTES Data frames.
func (m *meshConn) Write(p []byte) (int, error) {
	select {
	case <-m.closed:
		return 0, errors.New("mesh conn closed")
	default:
	}

	m.writeMu.Lock()
	defer m.writeMu.Unlock()

	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > SiteChunkBytes {
			n = SiteChunkBytes
		}
		chunk := p[:n]
		frame := NewSiteData(m.route, m.seq, m.conn, chunk)
		m.seq++
		if err := m.send(frame); err != nil {
			return total, err
		}
		total += n
		p = p[n:]
	}
	return total, nil
}

// Close implements net.Conn — emits a Close frame and unblocks Read once.
func (m *meshConn) Close() error {
	m.closeOnce.Do(func() {
		m.writeMu.Lock()
		seq := m.seq
		m.seq++
		m.writeMu.Unlock()
		_ = m.send(NewSiteClose(m.route, seq, m.conn))
		close(m.closed)
	})
	return nil
}

// isClosed reports whether Close has been called locally.
func (m *meshConn) isClosed() bool {
	select {
	case <-m.closed:
		return true
	default:
		return false
	}
}

// LocalAddr implements net.Conn.
func (m *meshConn) LocalAddr() net.Addr { return meshAddr(m.route) }

// RemoteAddr implements net.Conn.
func (m *meshConn) RemoteAddr() net.Addr { return meshAddr(m.route) }

// SetDeadline implements net.Conn.
func (m *meshConn) SetDeadline(t time.Time) error {
	m.deadlineMu.Lock()
	m.readDeadline = t
	m.writeDeadline = t
	m.wakeReadersLocked()
	m.deadlineMu.Unlock()
	return nil
}

// SetReadDeadline implements net.Conn.
func (m *meshConn) SetReadDeadline(t time.Time) error {
	m.deadlineMu.Lock()
	m.readDeadline = t
	m.wakeReadersLocked()
	m.deadlineMu.Unlock()
	return nil
}

// wakeReadersLocked republishes deadlineCh so a parked Read re-reads the
// deadline it was given. Callers hold deadlineMu.
func (m *meshConn) wakeReadersLocked() {
	close(m.deadlineCh)
	m.deadlineCh = make(chan struct{})
}

// SetWriteDeadline implements net.Conn.
func (m *meshConn) SetWriteDeadline(t time.Time) error {
	m.deadlineMu.Lock()
	m.writeDeadline = t
	m.deadlineMu.Unlock()
	return nil
}

// meshAddr is a net.Addr for a tunneled connection.
type meshAddr string

func (a meshAddr) Network() string { return "allmystuff-site" }
func (a meshAddr) String() string  { return string(a) }

// timeoutErr is a net.Error reporting a deadline timeout.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "mesh conn: i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }
