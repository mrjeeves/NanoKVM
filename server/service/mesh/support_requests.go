package mesh

import (
	"fmt"
	"sort"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	cecApprovalWindow = 5 * time.Minute
	cecRequestTTL     = 2 * time.Minute
	cecMaxRequests    = 32
)

// CecSupportRequest is safe to display only as an incoming request, never as
// evidence of access. Technician comes from the authenticated transport sender.
type CecSupportRequest struct {
	Technician       string `json:"technician"`
	SessionID        string `json:"sessionId"`
	AgentName        string `json:"agentName"`
	VerificationCode string `json:"verificationCode"`
	WantControl      bool   `json:"wantControl"`
	RequestedAt      int64  `json:"requestedAt"`
	network          string
	peer             string
	seenAt           time.Time
}
type cecDeniedRequest struct {
	sessionID string
	until     time.Time
}

func (b *Bridge) pruneCecRequestsLocked(now time.Time) {
	for key, request := range b.help.pending {
		if now.Sub(request.seenAt) >= cecRequestTTL {
			delete(b.help.pending, key)
		}
	}
	for key, denied := range b.help.denied {
		if !now.Before(denied.until) {
			delete(b.help.denied, key)
		}
	}
	if !now.Before(b.help.armedUntil) {
		b.help.armedUntil = time.Time{}
	}
}

func (b *Bridge) cecSupportSnapshot(now time.Time) ([]CecSupportRequest, int64) {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	b.pruneCecRequestsLocked(now)
	rows := make([]CecSupportRequest, 0, len(b.help.pending))
	for _, request := range b.help.pending {
		rows = append(rows, request)
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].RequestedAt != rows[j].RequestedAt {
			return rows[i].RequestedAt < rows[j].RequestedAt
		}
		return rows[i].Technician < rows[j].Technician
	})
	remaining := int64(0)
	if now.Before(b.help.armedUntil) {
		remaining = int64((b.help.armedUntil.Sub(now) + time.Second - 1) / time.Second)
	}
	return rows, remaining
}

func (b *Bridge) receiveCecRequest(network, peer string, message cecConnect) {
	key := pubkeyPart(peer)
	if key == "" || len(message.AgentName) > 256 {
		return
	}
	now := time.Now()
	request := CecSupportRequest{Technician: key, SessionID: message.SessionID,
		AgentName: message.AgentName, WantControl: message.WantControl,
		VerificationCode: supportIDFromString(peer + ":" + message.SessionID)[:6],
		RequestedAt:      now.Unix(), network: network, peer: peer, seenAt: now}
	b.help.mu.Lock()
	b.pruneCecRequestsLocked(now)
	if denied, found := b.help.denied[key]; found && denied.sessionID == message.SessionID {
		b.help.mu.Unlock()
		b.sendCecDecision(request, false)
		return
	}
	if b.cecApprovedTech(peer) {
		b.help.mu.Unlock()
		b.sendCecDecision(request, true)
		return
	}
	if now.Before(b.help.armedUntil) {
		b.grantCecRequestLocked(request)
		b.help.mu.Unlock()
		b.sendCecDecision(request, true)
		return
	}
	if b.help.pending == nil {
		b.help.pending = make(map[string]CecSupportRequest)
	}
	if old, found := b.help.pending[key]; found && old.SessionID == request.SessionID {
		// Retransmits refresh liveness, not the identity/name the user reviewed.
		old.seenAt = now
		old.network, old.peer = network, peer
		b.help.pending[key] = old
	} else if found || len(b.help.pending) < cecMaxRequests {
		b.help.pending[key] = request
	}
	b.help.mu.Unlock()
}

// AuthorizeSupport approves the oldest pending request, or arms one request
// for five minutes. Pressing it again refreshes, never toggles off, the window.
func (b *Bridge) AuthorizeSupport() error {
	now := time.Now()
	b.help.mu.Lock()
	b.pruneCecRequestsLocked(now)
	var current *CecSupportRequest
	for _, request := range b.help.pending {
		if current == nil || request.RequestedAt < current.RequestedAt ||
			(request.RequestedAt == current.RequestedAt && request.Technician < current.Technician) {
			copy := request
			current = &copy
		}
	}
	if current == nil {
		b.help.armedUntil = now.Add(cecApprovalWindow)
		b.help.mu.Unlock()
		return nil
	}
	b.grantCecRequestLocked(*current)
	b.help.mu.Unlock()
	b.sendCecDecision(*current, true)
	return nil
}

func (b *Bridge) grantCecRequestLocked(request CecSupportRequest) {
	b.state.GrantCecTech(request.Technician, cecGrantWindow)
	delete(b.help.pending, request.Technician)
	delete(b.help.denied, request.Technician)
	// Every explicit approval consumes any outstanding one-request permission.
	b.help.armedUntil = time.Time{}
}

func (b *Bridge) DecideCecRequest(technician, sessionID string, approve bool) error {
	key := pubkeyPart(technician)
	b.help.mu.Lock()
	b.pruneCecRequestsLocked(time.Now())
	request, found := b.help.pending[key]
	if !found || request.SessionID != sessionID {
		b.help.mu.Unlock()
		return fmt.Errorf("this support request is no longer pending; refresh and try again")
	}
	if approve {
		b.grantCecRequestLocked(request)
	} else {
		delete(b.help.pending, key)
		if b.help.denied == nil {
			b.help.denied = make(map[string]cecDeniedRequest)
		}
		if len(b.help.denied) >= cecMaxRequests {
			var oldest string
			for k, denied := range b.help.denied {
				if oldest == "" || denied.until.Before(b.help.denied[oldest].until) {
					oldest = k
				}
			}
			delete(b.help.denied, oldest)
		}
		b.help.denied[key] = cecDeniedRequest{sessionID: sessionID, until: time.Now().Add(cecApprovalWindow)}
	}
	b.help.mu.Unlock()
	b.sendCecDecision(request, approve)
	return nil
}

func (b *Bridge) sendCecDecision(request CecSupportRequest, approve bool) {
	payload := cecApprovePayload(request.SessionID)
	if !approve {
		payload = map[string]interface{}{"t": "connect", "kind": "deny", "session_id": request.SessionID, "reason": "Support request declined"}
	}
	// Consent is committed even if this reply is lost. A retransmitted request
	// is re-acked under the same grant without extending its deadline.
	if err := b.channelSendTo(request.network, CecChannelControl, request.peer, payload); err != nil {
		log.Warnf("mesh: support decision delivery failed: %s", err)
	}
}

func (b *Bridge) cancelCecRequest(peer, sessionID string) {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	key := pubkeyPart(peer)
	if request, found := b.help.pending[key]; found && request.SessionID == sessionID {
		delete(b.help.pending, key)
	}
}

func (b *Bridge) clearCecRequests() {
	b.help.mu.Lock()
	defer b.help.mu.Unlock()
	b.help.pending = nil
	b.help.denied = nil
	b.help.armedUntil = time.Time{}
}
