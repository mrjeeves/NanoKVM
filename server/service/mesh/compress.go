package mesh

import (
	"bufio"
	"compress/gzip"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

// Compression on the site tunnel is not the marginal bandwidth tweak it is on a
// LAN — it removes ROUND-TRIPS, which is what the tunnel is actually short of.
//
// Every byte the KVM writes back becomes a base64 SiteFrame, and each frame is
// one synchronous request/response through Socket.request, serialised behind a
// single mutex on the one daemon control socket shared with presence, control
// and the native video lane. The web UI's bundle (antd + xterm + a variable
// font) is several uncompressed megabytes, so a page load was hundreds of those
// round-trips, and a single one exceeding daemonReadTimeout is fatal: the
// socket is declared desynced, the bridge tears down, tearDownAll() closes
// every tunneled connection, and the browser is left holding a truncated
// document with no scripts — the blank page the sites feature kept showing.
//
// gzip at BestSpeed takes the bundle down by roughly 3-4x for a fraction of the
// CPU of the default level, which matters on a single-core device that is also
// running the H.264 encoder. Bytes are cheap here; round-trips and CPU are not.

// gzipWriterPool bounds the deflate state allocated per concurrent response — a
// browser opens several asset connections at once, and each gzip.Writer holds
// several hundred KB of window and hash tables.
var gzipWriterPool = sync.Pool{
	New: func() any {
		w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
		return w
	},
}

// compressibleType reports whether a Content-Type is worth compressing.
// Deliberately an allow-list: the tunnel also carries already-compressed
// payloads (woff2, png, the MJPEG multipart stream), where gzip would burn CPU
// to add bytes — and buffering a multipart/x-mixed-replace stream would stall
// the video outright.
func compressibleType(contentType string) bool {
	ct := contentType
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	switch strings.ToLower(strings.TrimSpace(ct)) {
	case "text/html", "text/css", "text/plain", "text/xml", "text/javascript",
		"application/javascript", "application/x-javascript", "application/json",
		"application/manifest+json", "application/xml", "application/wasm",
		"image/svg+xml":
		return true
	}
	return false
}

// acceptsGzip reports whether the requester advertised gzip support.
func acceptsGzip(r *http.Request) bool {
	for _, enc := range strings.Split(r.Header.Get("Accept-Encoding"), ",") {
		if name, _, _ := strings.Cut(strings.TrimSpace(enc), ";"); strings.EqualFold(name, "gzip") {
			return true
		}
	}
	return false
}

// gzipWriter wraps a ResponseWriter and compresses the body when the response
// turns out to be worth compressing. The decision is deferred to the first
// WriteHeader/Write because the handler sets Content-Type on the way out.
type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
}

func (w *gzipWriter) WriteHeader(status int) {
	if !w.decided {
		w.decided = true
		h := w.Header()
		h.Add("Vary", "Accept-Encoding")
		if compressibleStatus(status) && h.Get("Content-Encoding") == "" &&
			compressibleType(h.Get("Content-Type")) {
			// Length describes the identity body; the framework recomputes
			// nothing, so it has to go or the client truncates the response.
			h.Del("Content-Length")
			h.Set("Content-Encoding", "gzip")
			gz := gzipWriterPool.Get().(*gzip.Writer)
			gz.Reset(w.ResponseWriter)
			w.gz = gz
		}
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *gzipWriter) Write(p []byte) (int, error) {
	if !w.decided {
		w.WriteHeader(http.StatusOK)
	}
	if w.gz != nil {
		return w.gz.Write(p)
	}
	return w.ResponseWriter.Write(p)
}

// Flush pushes the deflate buffer out before the underlying writer's, so a
// streaming response (SSE, chunked JSON) still arrives incrementally.
func (w *gzipWriter) Flush() {
	if w.gz != nil {
		_ = w.gz.Flush()
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// Hijack keeps WebSocket upgrades working: http.Serve hands back the meshConn
// itself, and an upgrade writes its 101 by hand without ever reaching
// WriteHeader — so no gzip writer is ever started on a hijacked connection.
func (w *gzipWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	if h, ok := w.ResponseWriter.(http.Hijacker); ok {
		return h.Hijack()
	}
	return nil, nil, http.ErrNotSupported
}

// close finishes the gzip stream and returns the writer to the pool. Safe to
// call on a response that was never compressed.
func (w *gzipWriter) close() {
	if w.gz == nil {
		return
	}
	_ = w.gz.Close()
	gzipWriterPool.Put(w.gz)
	w.gz = nil
}

// compressibleStatus excludes the responses that carry no body of their own —
// including 101, which is a WebSocket upgrade rather than a document.
func compressibleStatus(status int) bool {
	return status >= http.StatusOK && status < http.StatusMultipleChoices &&
		status != http.StatusNoContent && status != http.StatusNotModified
}
