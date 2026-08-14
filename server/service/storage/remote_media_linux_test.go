//go:build linux

package storage

import (
	"bytes"
	"context"
	"encoding/binary"
	"sync"
	"testing"

	"NanoKVM-Server/proto"
)

func TestRecoverUSBAtStartupReplacesDeadRemoteMedia(t *testing.T) {
	oldRead, oldMount, oldEnsure := startupReadFile, startupMountImage, startupEnsureBound
	defer func() { startupReadFile, startupMountImage, startupEnsureBound = oldRead, oldMount, oldEnsure }()

	startupReadFile = func(string) ([]byte, error) { return []byte(remoteMediaMount + "/windows.iso\n"), nil }
	mounted := false
	startupMountImage = func(req proto.MountImageReq) error {
		mounted = true
		if req.File != "" {
			t.Fatalf("startup replacement file = %q, want default media", req.File)
		}
		return nil
	}
	startupEnsureBound = func() error {
		t.Fatal("startup recovery only bound the gadget instead of replacing dead FUSE media")
		return nil
	}

	recoverUSBAtStartup()
	if !mounted {
		t.Fatal("startup did not replace dead remote media")
	}
}

func TestRecoverUSBAtStartupEnsuresGadgetIsBound(t *testing.T) {
	oldRead, oldMount, oldEnsure := startupReadFile, startupMountImage, startupEnsureBound
	defer func() { startupReadFile, startupMountImage, startupEnsureBound = oldRead, oldMount, oldEnsure }()

	startupReadFile = func(string) ([]byte, error) { return []byte(kvmDriveImage + "\n"), nil }
	startupMountImage = func(proto.MountImageReq) error {
		t.Fatal("healthy local media was replaced at startup")
		return nil
	}
	bound := false
	startupEnsureBound = func() error { bound = true; return nil }

	recoverUSBAtStartup()
	if !bound {
		t.Fatal("startup did not ensure that the USB gadget was bound")
	}
}

type memoryRangeProvider struct {
	data  []byte
	done  chan struct{}
	mu    sync.Mutex
	reads map[int64]int
}

func TestPrimeBootMediaFetchesElToritoBootImages(t *testing.T) {
	data := make([]byte, remoteChunkSize*24)
	descriptor := data[17*isoSectorSize : 18*isoSectorSize]
	descriptor[0] = 0
	copy(descriptor[1:6], "CD001")
	copy(descriptor[7:39], "EL TORITO SPECIFICATION")
	catalogOffset := int64(remoteChunkSize * 4)
	binary.LittleEndian.PutUint32(descriptor[71:75], uint32(catalogOffset/isoSectorSize))

	catalog := data[catalogOffset : catalogOffset+isoSectorSize]
	catalog[0], catalog[30], catalog[31] = 1, 0x55, 0xaa
	catalog[32] = 0x88
	binary.LittleEndian.PutUint16(catalog[38:40], 4)
	biosOffset := int64(remoteChunkSize * 6)
	binary.LittleEndian.PutUint32(catalog[40:44], uint32(biosOffset/isoSectorSize))
	catalog[64], catalog[65] = 0x91, 0xef
	binary.LittleEndian.PutUint16(catalog[66:68], 1)
	catalog[96] = 0x88
	binary.LittleEndian.PutUint16(catalog[102:104], uint16(remoteChunkSize/diskSectorSize))
	efiOffset := int64(remoteChunkSize * 16)
	binary.LittleEndian.PutUint32(catalog[104:108], uint32(efiOffset/isoSectorSize))

	provider := newMemoryRangeProvider(data)
	image := &pagedImage{
		size: int64(len(data)), chunkSize: remoteChunkSize, cacheDir: t.TempDir(),
		provider: provider, flights: make(map[int64]*chunkFlight), cached: make(map[int64]uint64),
	}
	if err := image.primeBootMedia(context.Background(), true); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	for _, offset := range []int64{catalogOffset, biosOffset, efiOffset} {
		if provider.reads[offset] == 0 {
			t.Fatalf("boot-critical chunk at %d was not fetched", offset)
		}
	}
}

func TestPrimeBootMediaFetchesGPTESP(t *testing.T) {
	data := make([]byte, remoteChunkSize*10)
	header := data[diskSectorSize : 2*diskSectorSize]
	copy(header[:8], "EFI PART")
	binary.LittleEndian.PutUint64(header[72:80], 2)
	binary.LittleEndian.PutUint32(header[80:84], 1)
	binary.LittleEndian.PutUint32(header[84:88], 128)
	entry := data[2*diskSectorSize : 2*diskSectorSize+128]
	copy(entry[:16], efiSystemPartitionGUID)
	espOffset := int64(remoteChunkSize * 5)
	binary.LittleEndian.PutUint64(entry[32:40], uint64(espOffset/diskSectorSize))
	binary.LittleEndian.PutUint64(entry[40:48], uint64(espOffset/diskSectorSize+31))

	provider := newMemoryRangeProvider(data)
	image := &pagedImage{
		size: int64(len(data)), chunkSize: remoteChunkSize, cacheDir: t.TempDir(),
		provider: provider, flights: make(map[int64]*chunkFlight), cached: make(map[int64]uint64),
	}
	if err := image.primeBootMedia(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	provider.mu.Lock()
	defer provider.mu.Unlock()
	if provider.reads[espOffset] == 0 {
		t.Fatalf("EFI system partition chunk at %d was not fetched", espOffset)
	}
}

func newMemoryRangeProvider(data []byte) *memoryRangeProvider {
	return &memoryRangeProvider{data: data, done: make(chan struct{}), reads: make(map[int64]int)}
}

func (p *memoryRangeProvider) readAt(_ context.Context, offset int64, length int) ([]byte, error) {
	p.mu.Lock()
	p.reads[offset]++
	p.mu.Unlock()
	return append([]byte(nil), p.data[offset:offset+int64(length)]...), nil
}

func (p *memoryRangeProvider) doneSignal() <-chan struct{} { return p.done }

func TestPagedImageFetchesExactDistantChunks(t *testing.T) {
	data := make([]byte, remoteChunkSize*3+37)
	for i := range data {
		data[i] = byte((i*31 + 7) % 251)
	}
	provider := newMemoryRangeProvider(data)
	image := &pagedImage{
		size: int64(len(data)), chunkSize: remoteChunkSize,
		cacheDir: t.TempDir(), provider: provider, flights: make(map[int64]*chunkFlight),
	}

	// Read the final bytes first. A sparse-file implementation would return
	// zeros here; the pager must fetch the final chunk from the source.
	got := make([]byte, 29)
	offset := int64(len(data) - len(got))
	n, err := image.readAt(context.Background(), got, offset)
	if err != nil || n != len(got) {
		t.Fatalf("tail read = %d, %v", n, err)
	}
	if !bytes.Equal(got, data[offset:]) {
		t.Fatal("tail read did not match the source")
	}
	provider.mu.Lock()
	tailOffset := int64(remoteChunkSize * 3)
	reads := provider.reads[tailOffset]
	provider.mu.Unlock()
	if reads != 1 {
		t.Fatalf("tail chunk fetched %d times, want 1", reads)
	}
}

func TestPagedImageCachesAndCoalescesReads(t *testing.T) {
	data := bytes.Repeat([]byte{0x5a}, remoteChunkSize*2)
	provider := newMemoryRangeProvider(data)
	image := &pagedImage{
		size: int64(len(data)), chunkSize: remoteChunkSize,
		cacheDir: t.TempDir(), provider: provider, flights: make(map[int64]*chunkFlight),
	}

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			buf := make([]byte, 4096)
			if _, err := image.readAt(context.Background(), buf, 8192); err != nil {
				t.Errorf("read: %v", err)
			}
		}()
	}
	wg.Wait()
	provider.mu.Lock()
	reads := provider.reads[0]
	provider.mu.Unlock()
	if reads != 1 {
		t.Fatalf("same chunk fetched %d times, want 1", reads)
	}
}

func TestPollRangeProviderRoundTripsExactRange(t *testing.T) {
	provider := newPollRangeProvider()
	want := []byte{7, 8, 9, 10}
	result := make(chan remoteReply, 1)
	go func() {
		data, err := provider.readAt(context.Background(), 1234, len(want))
		result <- remoteReply{data: data, err: err}
	}()

	event := provider.next(context.Background())
	request, ok := event.(remoteReadRequest)
	if !ok {
		t.Fatalf("poll event = %T, want remoteReadRequest", event)
	}
	if request.Offset != 1234 || request.Length != len(want) {
		t.Fatalf("poll request = %+v", request)
	}
	if !provider.answer(request.ID, want) {
		t.Fatal("range answer was not accepted")
	}
	got := <-result
	if got.err != nil {
		t.Fatalf("range read: %v", got.err)
	}
	if !bytes.Equal(got.data, want) {
		t.Fatalf("range data = %v, want %v", got.data, want)
	}
}

func TestPollRangeProviderReportsExplicitUnmount(t *testing.T) {
	provider := newPollRangeProvider()
	provider.closePermanently()
	event := provider.next(context.Background())
	status, ok := event.(remoteStatus)
	if !ok || status.Kind != "unmounted" {
		t.Fatalf("close event = %#v, want unmounted status", event)
	}
}
