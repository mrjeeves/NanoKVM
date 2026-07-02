package mesh

import (
	"bufio"
	"encoding/json"
	"net"
	"path/filepath"
	"sync"
	"testing"

	"NanoKVM-Server/config"
)

// fakeDaemon is a scripted stand-in for the myownmesh control socket that
// mirrors the one connection semantic that matters for request routing: a
// connection that sends events_subscribe is acked and then becomes ONE-WAY —
// the fake never reads another byte from it, exactly like the daemon's
// run_events_stream (myownmesh control.rs), which only writes push frames
// after the subscribe ack. Every other connection answers request lines in
// order. A request written to the events connection therefore hangs until the
// client's read deadline — the same failure the real daemon produces.
type fakeDaemon struct {
	sock string
	done chan struct{}

	mu         sync.Mutex
	subscribes []map[string]interface{} // channel_subscribe requests, as received
}

func startFakeDaemon(t *testing.T) *fakeDaemon {
	t.Helper()
	f := &fakeDaemon{
		sock: filepath.Join(t.TempDir(), "daemon.sock"),
		done: make(chan struct{}),
	}
	ln, err := net.Listen("unix", f.sock)
	if err != nil {
		t.Fatalf("fake daemon listen: %v", err)
	}
	t.Cleanup(func() {
		close(f.done)
		_ = ln.Close()
	})
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(conn)
		}
	}()
	return f
}

func (f *fakeDaemon) serve(conn net.Conn) {
	defer conn.Close()
	r := bufio.NewReader(conn)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil {
			return
		}
		var req map[string]interface{}
		if json.Unmarshal(line, &req) != nil {
			continue
		}
		op, _ := req["op"].(string)
		switch op {
		case "events_subscribe":
			_, _ = conn.Write([]byte(`{"ok":true,"data":{"subscribed":true,"client_id":"c7"}}` + "\n"))
			// One-way from here on: hold the connection open, never read
			// again. A client that (incorrectly) sends a request here gets
			// silence, like against the real daemon.
			<-f.done
			return
		case "channel_subscribe":
			f.mu.Lock()
			f.subscribes = append(f.subscribes, req)
			f.mu.Unlock()
			_, _ = conn.Write([]byte(`{"ok":true,"data":{"subscribed":true}}` + "\n"))
		default:
			_, _ = conn.Write([]byte(`{"ok":true,"data":{}}` + "\n"))
		}
	}
}

func (f *fakeDaemon) channelSubscribes() []map[string]interface{} {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]map[string]interface{}, len(f.subscribes))
	copy(out, f.subscribes)
	return out
}

// TestJoinPlanesRoutesChannelSubscribeOverCtl pins the connection-routing
// contract that on-device bring-up tripped over: channel_subscribe names the
// events client via client_id but must ride the ctl connection, because the
// daemon never reads from a subscribed event stream. With the request on the
// wrong socket, joinPlanes dies as "channel_subscribe: read response: i/o
// timeout" on every connect cycle even though the daemon is healthy.
func TestJoinPlanesRoutesChannelSubscribeOverCtl(t *testing.T) {
	f := startFakeDaemon(t)

	events, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial events: %v", err)
	}
	defer events.Close()
	ctl, err := Dial(f.sock)
	if err != nil {
		t.Fatalf("dial ctl: %v", err)
	}
	defer ctl.Close()

	if err := events.Subscribe(nil, nil); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if got := events.ClientID(); got != "c7" {
		t.Fatalf("events client_id = %q, want c7", got)
	}

	b := &Bridge{
		conf:   &config.Config{},
		mesh:   config.Mesh{NetworkId: "cec-backend-client-mesh", Name: "CEC-KVM"},
		state:  LoadState(t.TempDir()),
		events: events,
		ctl:    ctl,
	}

	// Against the one-way events connection this only completes if every
	// channel_subscribe rode ctl; on the wrong socket it would block for the
	// full 10 s read deadline and fail.
	if err := b.joinPlanes("cec-backend-client-mesh"); err != nil {
		t.Fatalf("joinPlanes: %v", err)
	}

	subs := f.channelSubscribes()
	want := map[string]bool{ChannelPresence: true, ChannelControl: true, ChannelMedia: true}
	if len(subs) != len(want) {
		t.Fatalf("channel_subscribe count = %d, want %d (%v)", len(subs), len(want), subs)
	}
	for _, req := range subs {
		if id, _ := req["client_id"].(string); id != "c7" {
			t.Errorf("channel_subscribe client_id = %q, want the events client c7", id)
		}
		ch, _ := req["channel"].(string)
		if !want[ch] {
			t.Errorf("unexpected channel %q", ch)
		}
		delete(want, ch)
	}
}

// TestJoinPlanesBeforeConnectFailsFast guards the nil-socket path: joinPlanes
// before connectAndRun has dialed must return an error, not panic.
func TestJoinPlanesBeforeConnectFailsFast(t *testing.T) {
	b := &Bridge{
		mesh:  config.Mesh{NetworkId: "n"},
		state: LoadState(t.TempDir()),
	}
	if err := b.joinPlanes("n"); err == nil {
		t.Fatal("joinPlanes with no connections should fail")
	}
}
