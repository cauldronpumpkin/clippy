package osclip

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"sync"
	"testing"
	"time"

	"clippy/internal/clipwire"

	"golang.design/x/clipboard"
)

type writeCall struct {
	format clipboard.Format
	data   []byte
}

type fakeBackend struct {
	textWatch  chan []byte
	imageWatch chan []byte

	mu        sync.Mutex
	writes    []writeCall
	writeErrs map[clipboard.Format]error
}

func newFakeBackend() *fakeBackend {
	return &fakeBackend{
		textWatch:  make(chan []byte, 16),
		imageWatch: make(chan []byte, 16),
		writeErrs:  make(map[clipboard.Format]error),
	}
}

func (f *fakeBackend) Read(clipboard.Format) []byte {
	return nil
}

func (f *fakeBackend) Write(format clipboard.Format, data []byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()

	if err := f.writeErrs[format]; err != nil {
		return err
	}

	f.writes = append(f.writes, writeCall{
		format: format,
		data:   append([]byte(nil), data...),
	})
	return nil
}

func (f *fakeBackend) Watch(_ context.Context, format clipboard.Format) <-chan []byte {
	switch format {
	case clipboard.FmtText:
		return f.textWatch
	case clipboard.FmtImage:
		return f.imageWatch
	default:
		ch := make(chan []byte)
		close(ch)
		return ch
	}
}

func (f *fakeBackend) writesSnapshot() []writeCall {
	f.mu.Lock()
	defer f.mu.Unlock()

	out := make([]writeCall, len(f.writes))
	copy(out, f.writes)
	return out
}

func newTestManager(tb testing.TB, backend *fakeBackend) *Manager {
	tb.Helper()

	manager := newManagerWithBackend(8, backend)
	manager.coalesceWindow = 10 * time.Millisecond
	manager.pending = newPendingLedger(25 * time.Millisecond)
	manager.Start(context.Background())
	return manager
}

func TestLocalTextWatchEmitsPayload(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	backend.textWatch <- []byte("hello world")

	payload := recvPayload(t, manager.Events())
	if payload.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_TEXT {
		t.Fatalf("unexpected payload type: got %v", payload.GetType())
	}
	if got := string(payload.GetData()); got != "hello world" {
		t.Fatalf("unexpected payload data: got %q", got)
	}
	if payload.GetSizeBytes() != uint64(len("hello world")) {
		t.Fatalf("unexpected size_bytes: got %d", payload.GetSizeBytes())
	}
}

func TestLocalURLWatchEmitsURLPayload(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	backend.textWatch <- []byte("https://example.com/path")

	payload := recvPayload(t, manager.Events())
	if payload.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_URL {
		t.Fatalf("unexpected payload type: got %v", payload.GetType())
	}
}

func TestDualFormatEmitsTextThenImage(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	imageData := mustEncodePNG(t)
	backend.imageWatch <- imageData
	backend.textWatch <- []byte("https://example.com")

	first := recvPayload(t, manager.Events())
	second := recvPayload(t, manager.Events())

	if first.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_URL {
		t.Fatalf("unexpected first payload type: got %v", first.GetType())
	}
	if second.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_IMAGE {
		t.Fatalf("unexpected second payload type: got %v", second.GetType())
	}
	if !bytes.Equal(second.GetData(), imageData) {
		t.Fatal("unexpected image payload bytes")
	}
}

func TestApplyRemoteTextSuppressesEcho(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_TEXT, []byte("local text"), "")
	if err != nil {
		t.Fatalf("NewPayload returned error: %v", err)
	}

	if err := manager.ApplyRemote(context.Background(), payload); err != nil {
		t.Fatalf("ApplyRemote returned error: %v", err)
	}

	backend.textWatch <- []byte("local text")
	assertNoPayload(t, manager.Events())
}

func TestApplyRemoteURLSuppressesEcho(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_URL, []byte("https://example.com"), "")
	if err != nil {
		t.Fatalf("NewPayload returned error: %v", err)
	}

	if err := manager.ApplyRemote(context.Background(), payload); err != nil {
		t.Fatalf("ApplyRemote returned error: %v", err)
	}

	backend.textWatch <- []byte("https://example.com")
	assertNoPayload(t, manager.Events())
}

func TestApplyRemotePNGSuppressesEcho(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	imageData := mustEncodePNG(t)
	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_IMAGE, imageData, "image/png")
	if err != nil {
		t.Fatalf("NewPayload returned error: %v", err)
	}

	if err := manager.ApplyRemote(context.Background(), payload); err != nil {
		t.Fatalf("ApplyRemote returned error: %v", err)
	}

	backend.imageWatch <- imageData
	assertNoPayload(t, manager.Events())
}

func TestApplyRemoteJPEGNormalizesToPNGSuppressesEcho(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	jpegData := mustEncodeJPEG(t)
	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_IMAGE, jpegData, "image/jpeg")
	if err != nil {
		t.Fatalf("NewPayload returned error: %v", err)
	}

	if err := manager.ApplyRemote(context.Background(), payload); err != nil {
		t.Fatalf("ApplyRemote returned error: %v", err)
	}

	writes := backend.writesSnapshot()
	if len(writes) != 1 {
		t.Fatalf("unexpected writes: got %d want 1", len(writes))
	}
	if writes[0].format != clipboard.FmtImage {
		t.Fatalf("unexpected write format: got %v", writes[0].format)
	}
	if _, err := png.Decode(bytes.NewReader(writes[0].data)); err != nil {
		t.Fatalf("expected png write data: %v", err)
	}

	backend.imageWatch <- writes[0].data
	assertNoPayload(t, manager.Events())
}

func TestRepeatedLocalCopyStillEmitsTwice(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	backend.textWatch <- []byte("repeat")
	first := recvPayload(t, manager.Events())
	if first.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_TEXT {
		t.Fatalf("unexpected first payload type: got %v", first.GetType())
	}

	time.Sleep(20 * time.Millisecond)

	backend.textWatch <- []byte("repeat")
	second := recvPayload(t, manager.Events())
	if second.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_TEXT {
		t.Fatalf("unexpected second payload type: got %v", second.GetType())
	}
}

func TestPendingEntriesExpire(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_TEXT, []byte("stale"), "")
	if err != nil {
		t.Fatalf("NewPayload returned error: %v", err)
	}

	if err := manager.ApplyRemote(context.Background(), payload); err != nil {
		t.Fatalf("ApplyRemote returned error: %v", err)
	}

	time.Sleep(40 * time.Millisecond)

	backend.textWatch <- []byte("stale")
	got := recvPayload(t, manager.Events())
	if got.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_TEXT {
		t.Fatalf("unexpected payload type after expiry: got %v", got.GetType())
	}
}

func TestApplyRemoteRejectsInvalidPayloadAndWriteFailures(t *testing.T) {
	t.Parallel()

	backend := newFakeBackend()
	manager := newTestManager(t, backend)

	hash := clipwire.ComputeContentSHA256([]byte("bad"))
	invalid := &clipwire.ClipboardPayload{
		Type:      clipwire.PayloadType_PAYLOAD_TYPE_IMAGE,
		Data:      []byte("bad"),
		MimeType:  "image/gif",
		SizeBytes: 3,
		Sha256:    hash[:],
	}
	if err := manager.ApplyRemote(context.Background(), invalid); err == nil {
		t.Fatal("expected invalid payload to fail")
	}

	backend.writeErrs[clipboard.FmtText] = errors.New("boom")
	valid, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_TEXT, []byte("x"), "")
	if err != nil {
		t.Fatalf("NewPayload returned error: %v", err)
	}
	if err := manager.ApplyRemote(context.Background(), valid); err == nil {
		t.Fatal("expected write failure to fail")
	}

	backend.textWatch <- []byte("x")
	got := recvPayload(t, manager.Events())
	if got.GetType() != clipwire.PayloadType_PAYLOAD_TYPE_TEXT {
		t.Fatalf("unexpected payload type after failed write: got %v", got.GetType())
	}
}

func recvPayload(t *testing.T, ch <-chan *clipwire.ClipboardPayload) *clipwire.ClipboardPayload {
	t.Helper()

	select {
	case payload := <-ch:
		return payload
	case <-time.After(500 * time.Millisecond):
		t.Fatal("timed out waiting for payload")
		return nil
	}
}

func assertNoPayload(t *testing.T, ch <-chan *clipwire.ClipboardPayload) {
	t.Helper()

	select {
	case payload := <-ch:
		t.Fatalf("unexpected payload: %+v", payload)
	case <-time.After(100 * time.Millisecond):
	}
}

func mustEncodePNG(t *testing.T) []byte {
	t.Helper()

	img := image.NewNRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.NRGBA{R: 255, A: 255})
	img.Set(1, 1, color.NRGBA{G: 255, A: 255})

	var out bytes.Buffer
	if err := png.Encode(&out, img); err != nil {
		t.Fatalf("png.Encode returned error: %v", err)
	}
	return out.Bytes()
}

func mustEncodeJPEG(t *testing.T) []byte {
	t.Helper()

	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	img.Set(0, 0, color.RGBA{R: 255, G: 255, B: 0, A: 255})

	data, err := encodeJPEG(img)
	if err != nil {
		t.Fatalf("encodeJPEG returned error: %v", err)
	}
	return data
}
