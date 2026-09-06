package mesh

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"
)

func requestSupport(b *Bridge, peer, session string) {
	raw, _ := json.Marshal(cecConnect{T: "connect", Kind: "request", SessionID: session, AgentName: "Alice", WantControl: true})
	b.handleCecControl(CecHelpNetworkID, peer, raw)
}
func TestNumberRequestNeedsApproval(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	requestSupport(b, "tech-pub-AB12C", "s1")
	pending, _ := b.cecSupportSnapshot(time.Now())
	if len(pending) != 1 || pending[0].AgentName != "Alice" || pending[0].Technician != "tech-pub" {
		t.Fatalf("pending: %+v", pending)
	}
	if b.senderMayControl("tech-pub") {
		t.Fatal("pending request gave access")
	}
	if err := b.DecideCecRequest("tech-pub", "wrong", true); err == nil {
		t.Fatal("stale session approved")
	}
	if err := b.DecideCecRequest("someone-else", "s1", true); err == nil {
		t.Fatal("wrong technician approved")
	}
	if err := b.DecideCecRequest("tech-pub", "s1", true); err != nil {
		t.Fatal(err)
	}
	if !b.senderMayControl("tech-pub-AB12C") {
		t.Fatal("explicit approval did not grant control")
	}
	expiry, _ := b.state.CecTechExpiry("tech-pub")
	if left := time.Until(expiry); left > 3*time.Hour || left < 3*time.Hour-time.Second {
		t.Fatalf("grant length %s", left)
	}
	requestSupport(b, "tech-pub", "s1")
	again, _ := b.state.CecTechExpiry("tech-pub")
	if !again.Equal(expiry) {
		t.Fatal("retry extended access")
	}
}
func TestApprovalWindowRefreshAndSingleUse(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	if err := b.AuthorizeSupport(); err != nil {
		t.Fatal(err)
	}
	b.help.armedUntil = time.Now().Add(time.Minute)
	if err := b.AuthorizeSupport(); err != nil {
		t.Fatal(err)
	}
	_, seconds := b.cecSupportSnapshot(time.Now())
	if seconds < 299 || seconds > 300 {
		t.Fatalf("refresh: %ds", seconds)
	}
	requestSupport(b, "first", "s1")
	requestSupport(b, "second", "s2")
	if !b.cecApprovedTech("first") || b.cecApprovedTech("second") {
		t.Fatal("window must approve exactly one request")
	}
	pending, seconds := b.cecSupportSnapshot(time.Now())
	if seconds != 0 || len(pending) != 1 {
		t.Fatalf("consumed: %ds %+v", seconds, pending)
	}
}
func TestApprovalWindowExpiresAndButtonApprovesCurrent(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	b.help.armedUntil = time.Now().Add(-time.Second)
	requestSupport(b, "tech", "s1")
	if b.cecApprovedTech("tech") {
		t.Fatal("expired window approved request")
	}
	if err := b.AuthorizeSupport(); err != nil {
		t.Fatal(err)
	}
	if !b.cecApprovedTech("tech") {
		t.Fatal("button did not approve current request")
	}
	_, remaining := b.cecSupportSnapshot(time.Now())
	if remaining != 0 {
		t.Fatal("approving current request must not also arm next")
	}
}
func TestConcurrentRequestsConsumeWindowOnce(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	_ = b.AuthorizeSupport()
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(i int) { defer wg.Done(); requestSupport(b, fmt.Sprintf("tech%d", i), "s1") }(i)
	}
	wg.Wait()
	if n := len(b.state.snapshot().CecGrants); n != 1 {
		t.Fatalf("got %d grants, want one", n)
	}
}
func TestDeniedRequestRetryStaysDenied(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	requestSupport(b, "tech", "s1")
	if err := b.DecideCecRequest("tech", "s1", false); err != nil {
		t.Fatal(err)
	}
	requestSupport(b, "tech", "s1")
	pending, _ := b.cecSupportSnapshot(time.Now())
	if len(pending) != 0 || b.cecApprovedTech("tech") {
		t.Fatal("declined retry resurfaced or gained access")
	}
	requestSupport(b, "tech", "s2")
	pending, _ = b.cecSupportSnapshot(time.Now())
	if len(pending) != 1 {
		t.Fatal("new request should need a new decision")
	}
}
func TestRequestExpiryAndCapacity(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	for i := 0; i < cecMaxRequests+5; i++ {
		requestSupport(b, fmt.Sprintf("tech%d", i), "s1")
	}
	pending, _ := b.cecSupportSnapshot(time.Now())
	if len(pending) != cecMaxRequests {
		t.Fatal("request cap failed")
	}
	pending, _ = b.cecSupportSnapshot(time.Now().Add(cecRequestTTL))
	if len(pending) != 0 {
		t.Fatal("stale requests survived")
	}
	if err := b.DecideCecRequest("tech0", "s1", true); err == nil {
		t.Fatal("expired request approved")
	}
}
func TestResetClearsPendingAndWindow(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	requestSupport(b, "tech", "s1")
	b.help.armedUntil = time.Now().Add(time.Minute)
	b.clearCecRequests()
	pending, seconds := b.cecSupportSnapshot(time.Now())
	if len(pending) != 0 || seconds != 0 {
		t.Fatal("reset retained approval state")
	}
}
func TestRequestOnUnrelatedNetworkIsIgnored(t *testing.T) {
	b := &Bridge{state: LoadState(""), nodeID: "device"}
	b.handleCecControl("unrelated", "tech", []byte(`{"t":"connect","kind":"request","session_id":"s1"}`))
	pending, _ := b.cecSupportSnapshot(time.Now())
	if len(pending) != 0 {
		t.Fatal("unrelated network request accepted")
	}
}

func TestExistingGrantDoesNotConsumeNewApprovalWindow(t *testing.T) {
	b := &Bridge{state: LoadState("")}
	b.state.GrantCecTech("known", cecGrantWindow)
	_ = b.AuthorizeSupport()
	requestSupport(b, "known", "reconnect")
	if _, remaining := b.cecSupportSnapshot(time.Now()); remaining < 299 {
		t.Fatal("reconnect consumed the next-request permission")
	}
	requestSupport(b, "new", "s1")
	if !b.cecApprovedTech("new") {
		t.Fatal("next new request was not approved")
	}
	if _, remaining := b.cecSupportSnapshot(time.Now()); remaining != 0 {
		t.Fatal("new approval did not consume the window")
	}
}
