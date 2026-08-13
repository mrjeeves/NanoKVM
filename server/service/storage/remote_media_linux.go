//go:build linux

package storage

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"NanoKVM-Server/proto"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/hanwen/go-fuse/v2/fs"
	"github.com/hanwen/go-fuse/v2/fuse"
	log "github.com/sirupsen/logrus"
)

const (
	remoteMediaMount = "/run/allmystuff-remote-media"
	remoteMediaCache = "/data/.allmystuff-remote-media"
	remoteChunkSize  = 256 * 1024
	remoteReadWait   = 30 * time.Second
	remoteReconnect  = 2 * time.Minute
	remoteCacheMax   = 512 * 1024 * 1024
	remoteCacheFloor = 8 * 1024 * 1024
	remoteCacheSpare = 128 * 1024 * 1024
	remotePrefetch   = 32 * 1024 * 1024
)

type remoteMediaManager struct {
	mu     sync.Mutex
	active *remoteSession
}

type remoteManifest struct {
	Session string `json:"session"`
	Name    string `json:"name"`
	Size    int64  `json:"size"`
	Cdrom   bool   `json:"cdrom"`
	Source  string `json:"source"`
	Label   string `json:"label"`
}

type remoteReadRequest struct {
	Kind   string `json:"kind"`
	ID     uint64 `json:"id"`
	Offset int64  `json:"offset"`
	Length int    `json:"length"`
}

type remoteStatus struct {
	Kind    string `json:"kind"`
	Message string `json:"message,omitempty"`
}

type remoteReply struct {
	data []byte
	err  error
}

type wsRangeProvider struct {
	conn    *websocket.Conn
	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  atomic.Uint64
	pending map[uint64]chan remoteReply
	done    chan struct{}
	err     error
}

type reconnectingRangeProvider struct {
	mu         sync.Mutex
	current    remoteRangeConnection
	changed    chan struct{}
	done       chan struct{}
	closed     bool
	generation uint64
	once       sync.Once
}

type chunkFlight struct {
	done chan struct{}
	data []byte
	err  error
}

type remoteRangeProvider interface {
	readAt(context.Context, int64, int) ([]byte, error)
	doneSignal() <-chan struct{}
}

// remoteRangeConnection is one live path back to the machine holding the
// media. WebSocket is the fast path; the HTTP long-poll implementation is a
// compatibility path for site relays that cannot preserve an Upgrade.
type remoteRangeConnection interface {
	readAt(context.Context, int64, int) ([]byte, error)
	closePermanently()
}

type pagedImage struct {
	size      int64
	chunkSize int64
	cacheDir  string
	provider  remoteRangeProvider
	mu        sync.Mutex
	flights   map[int64]*chunkFlight
	cacheMu   sync.Mutex
	maxChunks int
	cacheTick uint64
	cached    map[int64]uint64
}

type remoteRoot struct {
	fs.Inode
	image *pagedImage
	name  string
}

type remoteFile struct {
	fs.Inode
	image *pagedImage
}

type remoteSession struct {
	manifest remoteManifest
	provider *reconnectingRangeProvider
	image    *pagedImage
	server   *fuse.Server
	file     string
	poll     *pollRangeProvider
	once     sync.Once
}

var remoteUpgrader = websocket.Upgrader{
	ReadBufferSize:  remoteChunkSize + 8,
	WriteBufferSize: 1024,
	CheckOrigin:     func(*http.Request) bool { return true },
}

func newRemoteMediaManager() *remoteMediaManager { return &remoteMediaManager{} }

func remoteMediaAvailable() (bool, string) {
	if info, err := os.Stat("/dev/fuse"); err != nil {
		return false, "/dev/fuse is unavailable; install the matching KVM FUSE module"
	} else if info.Mode()&os.ModeDevice == 0 {
		return false, "/dev/fuse is not a device"
	}
	return true, ""
}

func (s *Service) RemoteMediaEnabled(c *gin.Context) {
	var rsp proto.Response
	enabled, reason := remoteMediaAvailable()
	rsp.OkRspWithData(c, gin.H{
		"enabled":   enabled,
		"reason":    reason,
		"chunkSize": remoteChunkSize,
	})
}

func sanitizeRemoteName(name string, cdrom bool) string {
	name = filepath.Base(name)
	name = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("._-", r) {
			return r
		}
		return '_'
	}, name)
	if name == "" || name == "." {
		if cdrom {
			return "remote.iso"
		}
		return "remote.img"
	}
	return name
}

func sanitizeSession(session string) string {
	session = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, session)
	if session == "" {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return session
}

func (s *Service) RemoteMedia(c *gin.Context) {
	var rsp proto.Response
	if ok, reason := remoteMediaAvailable(); !ok {
		rsp.ErrRsp(c, -1, reason)
		return
	}
	conn, err := remoteUpgrader.Upgrade(c.Writer, c.Request, nil)
	if err != nil {
		return
	}

	_, raw, err := conn.ReadMessage()
	if err != nil {
		_ = conn.Close()
		return
	}
	var manifest remoteManifest
	if err := json.Unmarshal(raw, &manifest); err != nil || manifest.Size <= 0 {
		_ = conn.WriteJSON(remoteStatus{Kind: "error", Message: "invalid remote-media manifest"})
		_ = conn.Close()
		return
	}
	manifest.Session = sanitizeSession(manifest.Session)
	manifest.Name = sanitizeRemoteName(manifest.Name, manifest.Cdrom)

	connection := newWSRangeProvider(conn)

	s.remote.mu.Lock()
	active := s.remote.active
	if active != nil && active.matches(manifest) && active.provider.attach(connection) {
		s.remote.mu.Unlock()
		go connection.run()
		_ = connection.writeJSON(remoteStatus{Kind: "mounted"})
		<-connection.done
		s.remote.connectionLost(active, connection)
		return
	}
	s.remote.mu.Unlock()

	provider := newReconnectingRangeProvider(connection)
	image := &pagedImage{
		size: manifest.Size, chunkSize: remoteChunkSize,
		cacheDir: filepath.Join(remoteMediaCache, manifest.Session),
		provider: provider, flights: make(map[int64]*chunkFlight), cached: make(map[int64]uint64),
	}
	session := &remoteSession{manifest: manifest, provider: provider, image: image}

	s.remote.mu.Lock()
	old := s.remote.active
	s.remote.active = session
	s.remote.mu.Unlock()
	if old != nil {
		old.close()
	}

	go connection.run()
	if err := session.start(); err != nil {
		_ = connection.writeJSON(remoteStatus{Kind: "error", Message: err.Error()})
		session.close()
		s.remote.mu.Lock()
		if s.remote.active == session {
			s.remote.active = nil
		}
		s.remote.mu.Unlock()
		return
	}
	_ = connection.writeJSON(remoteStatus{Kind: "mounted"})
	go image.backgroundFill()
	<-connection.done
	s.remote.connectionLost(session, connection)
}

func (m *remoteMediaManager) connectionLost(session *remoteSession, connection remoteRangeConnection) {
	generation, detached := session.provider.detach(connection)
	if !detached {
		return
	}
	go func() {
		timer := time.NewTimer(remoteReconnect)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-session.provider.done:
			return
		}
		if !session.provider.stillDetached(generation) {
			return
		}
		m.mu.Lock()
		if m.active != session {
			m.mu.Unlock()
			return
		}
		m.active = nil
		m.mu.Unlock()
		session.close()
	}()
}

func (m *remoteMediaManager) replaceActive(req proto.MountImageReq) (bool, error) {
	m.mu.Lock()
	active := m.active
	m.active = nil
	m.mu.Unlock()
	if active == nil {
		return false, nil
	}
	return true, active.closeWith(req)
}

func (s *remoteSession) matches(manifest remoteManifest) bool {
	return s.manifest.Session == manifest.Session && s.manifest.Name == manifest.Name &&
		s.manifest.Size == manifest.Size && s.manifest.Cdrom == manifest.Cdrom
}

func newWSRangeProvider(conn *websocket.Conn) *wsRangeProvider {
	return &wsRangeProvider{conn: conn, pending: make(map[uint64]chan remoteReply), done: make(chan struct{})}
}

func newReconnectingRangeProvider(connection remoteRangeConnection) *reconnectingRangeProvider {
	return &reconnectingRangeProvider{
		current: connection,
		changed: make(chan struct{}),
		done:    make(chan struct{}),
	}
}

func (p *reconnectingRangeProvider) notifyLocked() {
	close(p.changed)
	p.changed = make(chan struct{})
}

func (p *reconnectingRangeProvider) attach(connection remoteRangeConnection) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.current != nil {
		return false
	}
	p.current = connection
	p.generation++
	p.notifyLocked()
	return true
}

func (p *reconnectingRangeProvider) detach(connection remoteRangeConnection) (uint64, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.current != connection {
		return p.generation, false
	}
	p.current = nil
	p.generation++
	p.notifyLocked()
	return p.generation, true
}

func (p *reconnectingRangeProvider) stillDetached(generation uint64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return !p.closed && p.current == nil && p.generation == generation
}

func (p *reconnectingRangeProvider) readAt(ctx context.Context, offset int64, length int) ([]byte, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("remote-media session closed")
		}
		connection := p.current
		changed := p.changed
		p.mu.Unlock()

		if connection == nil {
			select {
			case <-changed:
				continue
			case <-p.done:
				return nil, errors.New("remote-media session closed")
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		data, err := connection.readAt(ctx, offset, length)
		if err == nil {
			return data, nil
		}
		p.detach(connection)
	}
}

func (p *reconnectingRangeProvider) doneSignal() <-chan struct{} { return p.done }

func (p *reconnectingRangeProvider) close() {
	p.once.Do(func() {
		p.mu.Lock()
		p.closed = true
		connection := p.current
		p.current = nil
		p.notifyLocked()
		p.mu.Unlock()
		if connection != nil {
			connection.closePermanently()
		}
		close(p.done)
	})
}

func (p *wsRangeProvider) writeJSON(value any) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	return p.conn.WriteJSON(value)
}

func (p *wsRangeProvider) closePermanently() {
	p.writeMu.Lock()
	_ = p.conn.WriteControl(
		websocket.CloseMessage,
		websocket.FormatCloseMessage(websocket.CloseNormalClosure, "unmounted"),
		time.Now().Add(time.Second),
	)
	p.writeMu.Unlock()
	_ = p.conn.Close()
}

func (p *wsRangeProvider) doneSignal() <-chan struct{} { return p.done }

func (p *wsRangeProvider) run() {
	defer close(p.done)
	for {
		kind, data, err := p.conn.ReadMessage()
		if err != nil {
			p.fail(err)
			return
		}
		if kind != websocket.BinaryMessage || len(data) < 8 {
			continue
		}
		id := binary.BigEndian.Uint64(data[:8])
		p.mu.Lock()
		ch := p.pending[id]
		delete(p.pending, id)
		p.mu.Unlock()
		if ch != nil {
			ch <- remoteReply{data: append([]byte(nil), data[8:]...)}
		}
	}
}

func (p *wsRangeProvider) fail(err error) {
	p.mu.Lock()
	p.err = err
	pending := p.pending
	p.pending = make(map[uint64]chan remoteReply)
	p.mu.Unlock()
	for _, ch := range pending {
		ch <- remoteReply{err: err}
	}
}

func (p *wsRangeProvider) readAt(ctx context.Context, offset int64, length int) ([]byte, error) {
	id := p.nextID.Add(1)
	ch := make(chan remoteReply, 1)
	p.mu.Lock()
	if p.err != nil {
		err := p.err
		p.mu.Unlock()
		return nil, err
	}
	p.pending[id] = ch
	p.mu.Unlock()
	if err := p.writeJSON(remoteReadRequest{Kind: "read", ID: id, Offset: offset, Length: length}); err != nil {
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, err
	}
	timer := time.NewTimer(remoteReadWait)
	defer timer.Stop()
	select {
	case reply := <-ch:
		if reply.err != nil {
			return nil, reply.err
		}
		if len(reply.data) != length {
			return nil, fmt.Errorf("short remote read: got %d, want %d", len(reply.data), length)
		}
		return reply.data, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-timer.C:
		p.mu.Lock()
		delete(p.pending, id)
		p.mu.Unlock()
		return nil, errors.New("remote-media read timed out")
	}
}

func (p *pagedImage) chunkLength(index int64) int {
	offset := index * p.chunkSize
	remain := p.size - offset
	if remain <= 0 {
		return 0
	}
	if remain < p.chunkSize {
		return int(remain)
	}
	return int(p.chunkSize)
}

func (p *pagedImage) cachedChunk(index int64) ([]byte, error) {
	path := filepath.Join(p.cacheDir, fmt.Sprintf("%016x.chunk", index))
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(data) != p.chunkLength(index) {
		return nil, io.ErrUnexpectedEOF
	}
	p.cacheMu.Lock()
	p.ensureCacheStateLocked()
	p.cacheTick++
	p.cached[index] = p.cacheTick
	p.cacheMu.Unlock()
	return data, nil
}

func (p *pagedImage) writeChunk(index int64, data []byte) error {
	if err := os.MkdirAll(p.cacheDir, 0o700); err != nil {
		return err
	}
	p.cacheMu.Lock()
	defer p.cacheMu.Unlock()
	p.ensureCacheStateLocked()
	if _, exists := p.cached[index]; !exists && len(p.cached) >= p.maxChunks {
		var oldestIndex int64
		var oldestTick uint64
		found := false
		for cachedIndex, tick := range p.cached {
			if !found || tick < oldestTick {
				oldestIndex, oldestTick, found = cachedIndex, tick, true
			}
		}
		if found {
			oldestPath := filepath.Join(p.cacheDir, fmt.Sprintf("%016x.chunk", oldestIndex))
			if err := os.Remove(oldestPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			delete(p.cached, oldestIndex)
		}
	}
	path := filepath.Join(p.cacheDir, fmt.Sprintf("%016x.chunk", index))
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	p.cacheTick++
	p.cached[index] = p.cacheTick
	return nil
}

func (p *pagedImage) ensureCacheStateLocked() {
	if p.cached == nil {
		p.cached = make(map[int64]uint64)
	}
	if p.maxChunks < 1 {
		p.maxChunks = int((p.size + p.chunkSize - 1) / p.chunkSize)
		if p.maxChunks < 1 {
			p.maxChunks = 1
		}
	}
}

func (p *pagedImage) getChunk(ctx context.Context, index int64) ([]byte, error) {
	if data, err := p.cachedChunk(index); err == nil {
		return data, nil
	}
	p.mu.Lock()
	if flight := p.flights[index]; flight != nil {
		p.mu.Unlock()
		select {
		case <-flight.done:
			return flight.data, flight.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	flight := &chunkFlight{done: make(chan struct{})}
	p.flights[index] = flight
	p.mu.Unlock()
	length := p.chunkLength(index)
	data, err := p.provider.readAt(ctx, index*p.chunkSize, length)
	if err == nil {
		err = p.writeChunk(index, data)
	}
	flight.data, flight.err = data, err
	close(flight.done)
	p.mu.Lock()
	delete(p.flights, index)
	p.mu.Unlock()
	return data, err
}

func (p *pagedImage) readAt(ctx context.Context, dest []byte, offset int64) (int, error) {
	if offset < 0 || offset >= p.size {
		return 0, io.EOF
	}
	if int64(len(dest)) > p.size-offset {
		dest = dest[:p.size-offset]
	}
	written := 0
	for written < len(dest) {
		position := offset + int64(written)
		index := position / p.chunkSize
		within := int(position % p.chunkSize)
		chunk, err := p.getChunk(ctx, index)
		if err != nil {
			return written, err
		}
		n := copy(dest[written:], chunk[within:])
		written += n
	}
	return written, nil
}

func (p *pagedImage) backgroundFill() {
	ctx := context.Background()
	chunks := (p.size + p.chunkSize - 1) / p.chunkSize
	prefetchChunks := int64(remotePrefetch / remoteChunkSize)
	if chunks > prefetchChunks {
		chunks = prefetchChunks
	}
	for index := int64(0); index < chunks; index++ {
		select {
		case <-p.provider.doneSignal():
			return
		default:
		}
		if _, err := p.getChunk(ctx, index); err != nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (r *remoteRoot) OnAdd(ctx context.Context) {
	child := r.NewPersistentInode(ctx, &remoteFile{image: r.image}, fs.StableAttr{Mode: syscall.S_IFREG, Ino: 2})
	r.AddChild(r.name, child, false)
}

func (r *remoteRoot) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o555
	return 0
}

func (f *remoteFile) Getattr(_ context.Context, _ fs.FileHandle, out *fuse.AttrOut) syscall.Errno {
	out.Mode = 0o444
	out.Size = uint64(f.image.size)
	return 0
}

func (f *remoteFile) Open(_ context.Context, flags uint32) (fs.FileHandle, uint32, syscall.Errno) {
	if flags&(syscall.O_WRONLY|syscall.O_RDWR) != 0 {
		return nil, 0, syscall.EROFS
	}
	return nil, fuse.FOPEN_DIRECT_IO, 0
}

func (f *remoteFile) Read(ctx context.Context, _ fs.FileHandle, dest []byte, off int64) (fuse.ReadResult, syscall.Errno) {
	buffer := make([]byte, len(dest))
	n, err := f.image.readAt(ctx, buffer, off)
	if err != nil && !errors.Is(err, io.EOF) {
		log.Warnf("remote media read at %d failed: %s", off, err)
		return nil, syscall.EIO
	}
	return fuse.ReadResultData(buffer[:n]), 0
}

func (s *remoteSession) start() error {
	if err := os.RemoveAll(remoteMediaMount); err != nil {
		return err
	}
	if err := os.MkdirAll(remoteMediaMount, 0o700); err != nil {
		return err
	}
	// A single KVM can expose only one mass-storage image at a time. Clear stale
	// session chunks before sizing the new bounded cache so interrupted sessions
	// cannot slowly fill the exFAT data partition.
	if err := os.RemoveAll(remoteMediaCache); err != nil {
		return err
	}
	if err := os.MkdirAll(s.image.cacheDir, 0o700); err != nil {
		return err
	}
	maxChunks, err := remoteCacheChunks(s.image.cacheDir, s.image.size)
	if err != nil {
		return err
	}
	s.image.maxChunks = maxChunks
	root := &remoteRoot{image: s.image, name: s.manifest.Name}
	server, err := fs.Mount(remoteMediaMount, root, &fs.Options{MountOptions: fuse.MountOptions{
		FsName: "allmystuff-remote-media", Name: "allmystuff", DirectMount: true,
	}})
	if err != nil {
		return fmt.Errorf("mount remote-media FUSE file: %w", err)
	}
	s.server = server
	s.file = filepath.Join(remoteMediaMount, s.manifest.Name)

	// Prime the firmware-visible boot layout before USB enumeration. Reporting a
	// mounted LUN while El Torito/GPT reads still depend on a live mesh round trip
	// makes valid media disappear from firmware boot menus.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := s.image.primeBootMedia(ctx, s.manifest.Cdrom); err != nil {
		_ = server.Unmount()
		return fmt.Errorf("prime remote boot media: %w", err)
	}
	if err := mountImage(proto.MountImageReq{
		File: s.file, Cdrom: s.manifest.Cdrom, ReadOnly: true,
		Source: s.manifest.Source, Label: s.manifest.Label,
	}); err != nil {
		_ = server.Unmount()
		return err
	}
	persistVirtualMedia(proto.MountImageReq{File: s.file, Cdrom: s.manifest.Cdrom, ReadOnly: true, Source: s.manifest.Source, Label: s.manifest.Label})
	return nil
}

func (s *remoteSession) close() {
	_ = s.closeWith(proto.MountImageReq{})
}

func (s *remoteSession) closeWith(replacement proto.MountImageReq) error {
	var closeErr error
	s.once.Do(func() {
		// Stop outstanding range reads first, then replace the gadget backing file
		// exactly once. This lets an explicit local mount/unmount take over without
		// a redundant second USB reset while the FUSE server is shutting down.
		s.provider.close()
		closeErr = mountImage(replacement)
		if closeErr == nil {
			persistVirtualMedia(replacement)
		}
		if s.server != nil {
			if err := s.server.Unmount(); closeErr == nil && err != nil {
				closeErr = err
			}
		}
		_ = os.RemoveAll(remoteMediaMount)
		_ = os.RemoveAll(s.image.cacheDir)
	})
	return closeErr
}

func remoteCacheChunks(path string, imageSize int64) (int, error) {
	var stat syscall.Statfs_t
	if err := syscall.Statfs(path, &stat); err != nil {
		return 0, fmt.Errorf("size remote-media cache: %w", err)
	}
	free := uint64(stat.Bavail) * uint64(stat.Bsize)
	usable := free / 4
	if free > remoteCacheSpare {
		usable = (free - remoteCacheSpare) / 4
	}
	if usable > remoteCacheMax {
		usable = remoteCacheMax
	}
	if usable < remoteCacheFloor {
		usable = free / 2
		if usable > remoteCacheFloor {
			usable = remoteCacheFloor
		}
	}
	chunks := int(usable / remoteChunkSize)
	if chunks < 3 {
		return 0, fmt.Errorf("remote-media cache needs at least %d bytes free", 6*remoteChunkSize)
	}
	imageChunks := int((imageSize + remoteChunkSize - 1) / remoteChunkSize)
	if chunks > imageChunks {
		chunks = imageChunks
	}
	return chunks, nil
}

var _ fs.NodeOnAdder = (*remoteRoot)(nil)
var _ fs.NodeGetattrer = (*remoteRoot)(nil)
var _ fs.NodeGetattrer = (*remoteFile)(nil)
var _ fs.NodeOpener = (*remoteFile)(nil)
var _ fs.NodeReader = (*remoteFile)(nil)
