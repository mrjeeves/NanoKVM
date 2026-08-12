//go:build linux

package storage

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
)

const remotePollWait = 25 * time.Second

// pollRangeProvider carries the same range protocol as the WebSocket path,
// but each direction is an ordinary HTTP request. It is deliberately kept as
// a first-class transport: reverse site relays and some TURN paths can proxy
// HTTP correctly while dropping Connection: Upgrade.
type pollRangeProvider struct {
	events  chan any
	done    chan struct{}
	once    sync.Once
	nextID  atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan remoteReply
}

func newPollRangeProvider() *pollRangeProvider {
	return &pollRangeProvider{
		events:  make(chan any, 256),
		done:    make(chan struct{}),
		pending: make(map[uint64]chan remoteReply),
	}
}

func (p *pollRangeProvider) sendStatus(kind, message string) {
	select {
	case p.events <- remoteStatus{Kind: kind, Message: message}:
	case <-p.done:
	}
}

func (p *pollRangeProvider) next(ctx context.Context) any {
	// Prefer a final queued status over observing done, so the source learns
	// that a deliberate unmount is terminal instead of reconnecting forever.
	select {
	case event := <-p.events:
		return event
	default:
	}
	timer := time.NewTimer(remotePollWait)
	defer timer.Stop()
	select {
	case event := <-p.events:
		return event
	case <-p.done:
		select {
		case event := <-p.events:
			return event
		default:
			return remoteStatus{Kind: "unmounted"}
		}
	case <-timer.C:
		return remoteStatus{Kind: "idle"}
	case <-ctx.Done():
		return nil
	}
}

func (p *pollRangeProvider) readAt(ctx context.Context, offset int64, length int) ([]byte, error) {
	id := p.nextID.Add(1)
	reply := make(chan remoteReply, 1)
	p.mu.Lock()
	p.pending[id] = reply
	p.mu.Unlock()

	request := remoteReadRequest{Kind: "read", ID: id, Offset: offset, Length: length}
	select {
	case p.events <- request:
	case <-p.done:
		p.removePending(id)
		return nil, errors.New("remote-media session closed")
	case <-ctx.Done():
		p.removePending(id)
		return nil, ctx.Err()
	}

	timer := time.NewTimer(remoteReadWait)
	defer timer.Stop()
	select {
	case result := <-reply:
		if result.err != nil {
			return nil, result.err
		}
		if len(result.data) != length {
			return nil, fmt.Errorf("short remote read: got %d, want %d", len(result.data), length)
		}
		return result.data, nil
	case <-p.done:
		p.removePending(id)
		return nil, errors.New("remote-media session closed")
	case <-ctx.Done():
		p.removePending(id)
		return nil, ctx.Err()
	case <-timer.C:
		p.removePending(id)
		return nil, errors.New("remote-media HTTP read timed out")
	}
}

func (p *pollRangeProvider) removePending(id uint64) {
	p.mu.Lock()
	delete(p.pending, id)
	p.mu.Unlock()
}

func (p *pollRangeProvider) answer(id uint64, data []byte) bool {
	p.mu.Lock()
	reply := p.pending[id]
	delete(p.pending, id)
	p.mu.Unlock()
	if reply == nil {
		return false
	}
	reply <- remoteReply{data: data}
	return true
}

func (p *pollRangeProvider) closePermanently() {
	p.once.Do(func() {
		// Queue the terminal event before closing done; next() intentionally
		// drains it first.
		select {
		case p.events <- remoteStatus{Kind: "unmounted"}:
		default:
		}
		close(p.done)
		p.mu.Lock()
		pending := p.pending
		p.pending = make(map[uint64]chan remoteReply)
		p.mu.Unlock()
		for _, reply := range pending {
			reply <- remoteReply{err: errors.New("remote-media session closed")}
		}
	})
}

func (s *Service) RemoteMediaPollOpen(c *gin.Context) {
	if ok, reason := remoteMediaAvailable(); !ok {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": reason})
		return
	}
	var manifest remoteManifest
	if err := c.ShouldBindJSON(&manifest); err != nil || manifest.Size <= 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote-media manifest"})
		return
	}
	manifest.Session = sanitizeSession(manifest.Session)
	manifest.Name = sanitizeRemoteName(manifest.Name, manifest.Cdrom)

	s.remote.mu.Lock()
	if active := s.remote.active; active != nil && active.matches(manifest) && active.poll != nil {
		poll := active.poll
		s.remote.mu.Unlock()
		poll.sendStatus("mounted", "")
		c.JSON(http.StatusOK, gin.H{"accepted": true, "resumed": true})
		return
	}
	poll := newPollRangeProvider()
	provider := newReconnectingRangeProvider(poll)
	image := &pagedImage{
		size: manifest.Size, chunkSize: remoteChunkSize,
		cacheDir: filepath.Join(remoteMediaCache, manifest.Session),
		provider: provider, flights: make(map[int64]*chunkFlight), cached: make(map[int64]uint64),
	}
	session := &remoteSession{manifest: manifest, provider: provider, image: image, poll: poll}
	old := s.remote.active
	s.remote.active = session
	s.remote.mu.Unlock()
	if old != nil {
		old.close()
	}

	// Return before priming. Priming itself asks the source for ranges through
	// /next, so holding this request open would deadlock the polling client.
	c.JSON(http.StatusAccepted, gin.H{"accepted": true})
	go func() {
		if err := session.start(); err != nil {
			poll.sendStatus("error", err.Error())
			session.close()
			s.remote.mu.Lock()
			if s.remote.active == session {
				s.remote.active = nil
			}
			s.remote.mu.Unlock()
			return
		}
		poll.sendStatus("mounted", "")
		go image.backgroundFill()
	}()
}

func (s *Service) activePoll(c *gin.Context) (*remoteSession, *pollRangeProvider, bool) {
	sessionID := sanitizeSession(c.Query("session"))
	s.remote.mu.Lock()
	defer s.remote.mu.Unlock()
	active := s.remote.active
	if active == nil || active.manifest.Session != sessionID || active.poll == nil {
		c.JSON(http.StatusGone, gin.H{"kind": "unmounted"})
		return nil, nil, false
	}
	return active, active.poll, true
}

func (s *Service) RemoteMediaPollNext(c *gin.Context) {
	_, poll, ok := s.activePoll(c)
	if !ok {
		return
	}
	event := poll.next(c.Request.Context())
	if event == nil {
		return
	}
	c.JSON(http.StatusOK, event)
}

func (s *Service) RemoteMediaPollReply(c *gin.Context) {
	_, poll, ok := s.activePoll(c)
	if !ok {
		return
	}
	id, err := strconv.ParseUint(c.Query("id"), 10, 64)
	if err != nil || id == 0 {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid remote-media request id"})
		return
	}
	data, err := io.ReadAll(io.LimitReader(c.Request.Body, remoteChunkSize+1))
	if err != nil || len(data) > remoteChunkSize {
		c.JSON(http.StatusRequestEntityTooLarge, gin.H{"error": "invalid remote-media reply"})
		return
	}
	if !poll.answer(id, data) {
		c.JSON(http.StatusGone, gin.H{"error": "remote-media request expired"})
		return
	}
	c.Status(http.StatusNoContent)
}

func (s *Service) RemoteMediaPollClose(c *gin.Context) {
	active, _, ok := s.activePoll(c)
	if !ok {
		return
	}
	s.remote.mu.Lock()
	if s.remote.active == active {
		s.remote.active = nil
	}
	s.remote.mu.Unlock()
	go active.close()
	c.Status(http.StatusNoContent)
}
