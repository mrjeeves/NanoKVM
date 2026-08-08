package mesh

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

// bundle is a stand-in for the web UI's JS chunk: big and highly compressible,
// which is exactly the payload that made the tunnel fall over.
func bundle(n int) []byte {
	body := make([]byte, n)
	line := []byte("export const value = {a: 1, b: 2, c: 3}; // repeated filler\n")
	for i := range body {
		body[i] = line[i%len(line)]
	}
	return body
}

// TestTunnelCompressesLargeAssetAndRoundTrips drives a real gin engine over the
// site tunnel and reassembles the outbound frames the way the AllMyStuff side
// does. It pins the two properties that matter: the body survives byte-for-byte
// after decompression, and it costs far fewer SiteFrames than the raw asset —
// each frame being one synchronous daemon round-trip is the whole problem.
func TestTunnelCompressesLargeAssetAndRoundTrips(t *testing.T) {
	gin.SetMode(gin.TestMode)

	body := bundle(2 * 1024 * 1024)
	engine := gin.New()
	engine.GET("/app.js", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/javascript", body)
	})

	const (
		allowed = uint16(80)
		route   = "route:peer:site->kvm:0"
		peer    = "peer-AB12C"
		conn    = uint64(1)
	)

	var mu sync.Mutex
	var frames []SiteFrame
	send := func(_, _ string, f SiteFrame) error {
		// Production json.Marshals the frame synchronously, copying Data before
		// meshConn's caller buffer is reused; a capture holding the slice would
		// alias that buffer and see corruption.
		f.Data = append([]byte(nil), f.Data...)
		mu.Lock()
		frames = append(frames, f)
		mu.Unlock()
		return nil
	}

	h := newSiteHost(engine, allowed, send)
	h.markRouteActive(route, "turn-net", peer)
	h.handleFrame(peer, NewSiteOpen(route, 0, conn, allowed))

	req := "GET /app.js HTTP/1.1\r\nHost: localhost\r\n" +
		"Accept-Encoding: gzip, deflate\r\nConnection: close\r\n\r\n"
	h.handleFrame(peer, NewSiteData(route, 1, conn, []byte(req)))

	raw, count := drainResponse(t, h, route, conn, &mu, &frames)

	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), nil)
	if err != nil {
		t.Fatalf("parse tunneled response: %v", err)
	}
	defer resp.Body.Close()

	if got := resp.Header.Get("Content-Encoding"); got != "gzip" {
		t.Fatalf("Content-Encoding = %q, want gzip", got)
	}
	if got := resp.Header.Get("Vary"); got != "Accept-Encoding" {
		t.Errorf("Vary = %q, want Accept-Encoding", got)
	}
	// A stale identity Content-Length would truncate the body at the client.
	if got := resp.Header.Get("Content-Length"); got != "" {
		t.Errorf("Content-Length = %q, want it dropped for a compressed body", got)
	}

	zr, err := gzip.NewReader(resp.Body)
	if err != nil {
		t.Fatalf("gzip reader: %v", err)
	}
	got, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("read compressed body: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Fatalf("body corrupted: got %d bytes, want %d", len(got), len(body))
	}

	// The point of compressing here is round-trips, not bytes: each frame is one
	// serialised request/response to the daemon.
	rawFrames := (len(body) + SiteChunkBytes - 1) / SiteChunkBytes
	if count*2 > rawFrames {
		t.Fatalf("compression saved too little: %d frames vs %d uncompressed", count, rawFrames)
	}
	t.Logf("%d frames compressed vs ~%d uncompressed", count, rawFrames)
}

// TestTunnelSkipsIncompressibleAndUnwillingClients pins the two cases that must
// pass through untouched: a client that never advertised gzip, and a payload
// that is already compressed (fonts, images, the MJPEG multipart stream, where
// buffering would stall the video outright).
func TestTunnelSkipsIncompressibleAndUnwillingClients(t *testing.T) {
	gin.SetMode(gin.TestMode)

	engine := gin.New()
	engine.GET("/font.woff2", func(c *gin.Context) {
		c.Data(http.StatusOK, "font/woff2", bundle(64*1024))
	})
	engine.GET("/app.js", func(c *gin.Context) {
		c.Data(http.StatusOK, "text/javascript", bundle(64*1024))
	})

	cases := []struct {
		name, path, accept string
	}{
		{"already compressed type", "/font.woff2", "gzip"},
		{"client does not accept gzip", "/app.js", "identity"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			r := httptest.NewRequest(http.MethodGet, tc.path, nil)
			r.Header.Set("Accept-Encoding", tc.accept)
			meshAuthHandler{engine: engine}.ServeHTTP(rec, r)

			if got := rec.Header().Get("Content-Encoding"); got != "" {
				t.Fatalf("Content-Encoding = %q, want the body left alone", got)
			}
			if rec.Body.Len() != 64*1024 {
				t.Fatalf("body length = %d, want it passed through intact", rec.Body.Len())
			}
		})
	}
}

// drainResponse collects the outbound Data frames for one conn, in seq order,
// and returns the reassembled bytes plus the frame count.
func drainResponse(t *testing.T, h *siteHost, route string, conn uint64, mu *sync.Mutex, frames *[]SiteFrame) ([]byte, int) {
	t.Helper()
	// The response is written by serveHTTP on its own goroutine; wait for the
	// Close frame that ends it.
	waitFor(t, "the response's closing frame", func() bool {
		mu.Lock()
		defer mu.Unlock()
		for _, f := range *frames {
			if f.Conn == conn && f.Kind == SiteEventKindClose {
				return true
			}
		}
		return false
	})

	mu.Lock()
	defer mu.Unlock()
	var out []byte
	count := 0
	for _, f := range *frames {
		if f.Conn != conn || f.Kind != SiteEventKindData {
			continue
		}
		out = append(out, f.Data...)
		count++
	}
	if _, ok := h.routeTarget(route); !ok {
		t.Fatal("route went inactive mid-response")
	}
	return out, count
}

// TestReadHonoursDeadlineSetWhileBlocked pins the net.Conn contract that
// net/http's finishRequest depends on. abortPendingRead sets a deadline in the
// past and then blocks until the in-flight Read returns; a Read that only
// sampled the deadline on entry never came back, so the request never
// finished, the connection was never closed, and every tunneled browser
// connection leaked — until the browser hit its per-host connection cap and
// stopped fetching the rest of the page.
func TestReadHonoursDeadlineSetWhileBlocked(t *testing.T) {
	c := newMeshConn("route:r", 1, func(SiteFrame) error { return nil })

	done := make(chan error, 1)
	go func() {
		buf := make([]byte, 16)
		_, err := c.Read(buf)
		done <- err
	}()

	// Let the Read park with no deadline at all, then move the deadline into
	// the past exactly as abortPendingRead does.
	time.Sleep(20 * time.Millisecond)
	_ = c.SetReadDeadline(time.Now().Add(-time.Second))

	select {
	case err := <-done:
		var ne net.Error
		if !errors.As(err, &ne) || !ne.Timeout() {
			t.Fatalf("Read err = %v, want a timeout", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read ignored a deadline set while it was blocked")
	}
}

// A deadline pushed further out must not cut a Read short.
func TestReadKeepsWaitingWhenDeadlineExtends(t *testing.T) {
	c := newMeshConn("route:r", 1, func(SiteFrame) error { return nil })

	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 16)
		n, err := c.Read(buf)
		if err != nil {
			done <- nil
			return
		}
		done <- buf[:n]
	}()

	time.Sleep(20 * time.Millisecond)
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	time.Sleep(20 * time.Millisecond)
	c.feed([]byte("hello"))

	select {
	case got := <-done:
		if string(got) != "hello" {
			t.Fatalf("Read = %q, want hello", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Read did not deliver the fed bytes")
	}
}
