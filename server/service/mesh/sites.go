package mesh

import (
	"net"
	"net/http"
	"sync"

	"NanoKVM-Server/middleware"

	"github.com/gin-gonic/gin"
	log "github.com/sirupsen/logrus"
)

// siteHost is the host side of the AllMyStuff "sites" plane. It accepts a single
// advertised web port, demultiplexes inbound SiteFrames by (route, conn), and
// serves each tunneled browser connection — mapped to a meshConn — as in-process
// HTTP through the gin engine with the login bypassed (mesh roster membership is
// the auth).
type siteHost struct {
	engine      *gin.Engine
	allowedPort uint16

	// send emits one outbound SiteFrame on CHANNEL_MEDIA to a specific peer.
	send func(peer string, frame SiteFrame) error

	// serveConn drives one tunneled connection. Defaults to serveHTTP (the gin
	// engine over the meshConn); overridable in tests that exercise only the
	// demux without driving HTTP.
	serveConn func(*meshConn)

	mu sync.Mutex
	// conns is keyed by route then conn id.
	conns map[string]map[uint64]*meshConn
	// activeRoutes is the set of route ids we accepted an Offer for; only frames
	// on an active route are served.
	activeRoutes map[string]string // route id -> peer (the offerer)
}

func newSiteHost(engine *gin.Engine, allowedPort uint16, send func(peer string, frame SiteFrame) error) *siteHost {
	h := &siteHost{
		engine:       engine,
		allowedPort:  allowedPort,
		send:         send,
		conns:        make(map[string]map[uint64]*meshConn),
		activeRoutes: make(map[string]string),
	}
	h.serveConn = h.serveHTTP
	return h
}

// markRouteActive records that we accepted a site route Offer from peer, so its
// media SiteFrames are served.
func (h *siteHost) markRouteActive(route, peer string) {
	h.mu.Lock()
	h.activeRoutes[route] = peer
	h.mu.Unlock()
}

// tearDownRoute closes every connection of a route and drops it from the active
// set (honoring a RouteControl Teardown).
func (h *siteHost) tearDownRoute(route string) {
	h.mu.Lock()
	conns := h.conns[route]
	delete(h.conns, route)
	delete(h.activeRoutes, route)
	h.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

// routePeer returns the peer a route was offered by, if active.
func (h *siteHost) routePeer(route string) (string, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	peer, ok := h.activeRoutes[route]
	return peer, ok
}

// handleFrame processes one inbound SiteFrame for a route. peer is the sender.
func (h *siteHost) handleFrame(peer string, f SiteFrame) {
	// Only serve frames on a route whose Offer we accepted, and only from the
	// peer that made that offer (the mesh authenticates the sender).
	offerer, active := h.routePeer(f.Route)
	if !active || offerer != peer {
		log.Debugf("mesh: dropping site frame on inactive/foreign route %s", f.Route)
		return
	}

	switch f.Kind {
	case SiteEventKindOpen:
		h.handleOpen(peer, f)
	case SiteEventKindData:
		if c := h.lookup(f.Route, f.Conn); c != nil {
			c.feed(f.Data)
		}
	case SiteEventKindClose:
		if c := h.lookup(f.Route, f.Conn); c != nil {
			c.remoteClose()
		}
	default:
		// Unknown event kind — ignore (forward-compat).
	}
}

// handleOpen validates the requested port against our allow-list and, if it
// matches, spins up a meshConn served by the gin engine.
func (h *siteHost) handleOpen(peer string, f SiteFrame) {
	if f.Port != h.allowedPort {
		// The advert is the allow-list: refuse anything else by immediately
		// closing the connection.
		log.Warnf("mesh: site open for unadvertised port %d (allow %d); refusing", f.Port, h.allowedPort)
		_ = h.send(peer, NewSiteClose(f.Route, 0, f.Conn))
		return
	}

	send := func(frame SiteFrame) error { return h.send(peer, frame) }
	c := newMeshConn(f.Route, f.Conn, send)

	h.mu.Lock()
	if h.conns[f.Route] == nil {
		h.conns[f.Route] = make(map[uint64]*meshConn)
	}
	// If a conn id is reused, close the stale one first.
	if old := h.conns[f.Route][f.Conn]; old != nil {
		_ = old.Close()
	}
	h.conns[f.Route][f.Conn] = c
	h.mu.Unlock()

	go h.serveConn(c)
}

// lookup returns the meshConn for (route, conn), or nil.
func (h *siteHost) lookup(route string, conn uint64) *meshConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	if byRoute := h.conns[route]; byRoute != nil {
		return byRoute[conn]
	}
	return nil
}

// drop removes a finished conn from the table.
func (h *siteHost) drop(route string, conn uint64) {
	h.mu.Lock()
	if byRoute := h.conns[route]; byRoute != nil {
		delete(byRoute, conn)
		if len(byRoute) == 0 {
			delete(h.conns, route)
		}
	}
	h.mu.Unlock()
}

// serveHTTP runs http.Serve over a one-shot listener that yields c exactly once,
// using a handler that wraps the gin engine and marks each request mesh-
// authenticated. This serves one browser TCP connection (mapped to one mesh
// conn) as in-process HTTP with auth bypassed; a WebSocket upgrade works because
// http's hijack returns our meshConn.
func (h *siteHost) serveHTTP(c *meshConn) {
	defer h.drop(c.route, c.conn)
	defer c.Close()

	handler := meshAuthHandler{engine: h.engine}
	srv := &http.Server{Handler: handler}
	// http.Serve consumes the listener; oneShotListener returns c once then
	// blocks until c closes, at which point Accept returns an error and Serve
	// exits cleanly.
	ln := newOneShotListener(c)
	_ = srv.Serve(ln)
}

// meshAuthHandler wraps the gin engine, marking every request mesh-authenticated
// so the token middleware passes without a login cookie.
type meshAuthHandler struct {
	engine *gin.Engine
}

func (m meshAuthHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.engine.ServeHTTP(w, middleware.WithMeshAuth(r))
}

// oneShotListener is a net.Listener that yields a single pre-built conn once,
// then blocks Accept until that conn closes (and reports an error so http.Serve
// stops). This lets us drive http.Serve per tunneled connection.
type oneShotListener struct {
	conn     *meshConn
	once     sync.Once
	yielded  chan net.Conn
	closedCh chan struct{}
}

func newOneShotListener(c *meshConn) *oneShotListener {
	l := &oneShotListener{
		conn:     c,
		yielded:  make(chan net.Conn, 1),
		closedCh: make(chan struct{}),
	}
	l.yielded <- c
	return l
}

// Accept yields the conn once, then blocks until the conn (or listener) closes.
func (l *oneShotListener) Accept() (net.Conn, error) {
	select {
	case c := <-l.yielded:
		return c, nil
	case <-l.closedCh:
		return nil, net.ErrClosed
	case <-l.conn.closed:
		return nil, net.ErrClosed
	}
}

// Close stops the listener. The served conn is closed by serve()'s defer.
func (l *oneShotListener) Close() error {
	l.once.Do(func() { close(l.closedCh) })
	return nil
}

// Addr implements net.Listener.
func (l *oneShotListener) Addr() net.Addr { return meshAddr(l.conn.route) }
