// Package iceservers holds the current "venue" ICE-server union — the
// deduplicated set of STUN/TURN servers drawn from every mesh this KVM is on.
//
// It is a deliberately tiny leaf package with NO dependencies on the mesh or
// webrtc packages: the mesh bridge PUBLISHES the union here (it's the only one
// that can talk to the myownmesh daemon), and the webrtc stream package READS
// it when building a browser's ICE configuration. Keeping it standalone avoids
// an import cycle (mesh imports webrtc-adjacent code and vice versa would not
// compile) and keeps it CGO-free so both sides can link it.
package iceservers

import "sync"

// Server is one ICE server entry: a set of URLs plus optional TURN credentials.
// It mirrors the fields both pion (webrtc.ICEServer) and the browser
// (RTCIceServer) need, without importing either.
type Server struct {
	URLs       []string
	Username   string
	Credential string
}

var (
	mu      sync.RWMutex
	current []Server
)

// Set replaces the current venue ICE-server union. The caller's slice is
// copied defensively so later mutation on their side can't race a reader.
func Set(s []Server) {
	cp := make([]Server, len(s))
	for i, srv := range s {
		urls := make([]string, len(srv.URLs))
		copy(urls, srv.URLs)
		cp[i] = Server{URLs: urls, Username: srv.Username, Credential: srv.Credential}
	}
	mu.Lock()
	current = cp
	mu.Unlock()
}

// Get returns a copy of the current venue ICE-server union. Safe to mutate.
func Get() []Server {
	mu.RLock()
	defer mu.RUnlock()
	out := make([]Server, len(current))
	for i, srv := range current {
		urls := make([]string, len(srv.URLs))
		copy(urls, srv.URLs)
		out[i] = Server{URLs: urls, Username: srv.Username, Credential: srv.Credential}
	}
	return out
}
