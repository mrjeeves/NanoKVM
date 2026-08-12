//go:build linux

package storage

import (
	"bytes"
	"context"
	"sync"
	"testing"
)

type memoryRangeProvider struct {
	data  []byte
	done  chan struct{}
	mu    sync.Mutex
	reads map[int64]int
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
