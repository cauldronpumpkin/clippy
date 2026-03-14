package osclip

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"net/url"
	"strings"
	"sync"
	"time"

	"clippy/internal/clipwire"

	"golang.design/x/clipboard"
)

const (
	defaultEventBuffer    = 16
	defaultCoalesceWindow = 50 * time.Millisecond
	defaultPendingTTL     = 5 * time.Second
)

var (
	// ErrClipboardWriteFailed indicates that the clipboard package did not
	// accept the requested native write.
	ErrClipboardWriteFailed = errors.New("osclip: clipboard write failed")
)

type formatBucket uint8

const (
	textBucket formatBucket = iota + 1
	imageBucket
)

type clipboardBackend interface {
	Read(clipboard.Format) []byte
	Write(clipboard.Format, []byte) error
	Watch(context.Context, clipboard.Format) <-chan []byte
}

type nativeBackend struct{}

func (nativeBackend) Read(format clipboard.Format) []byte {
	return clipboard.Read(format)
}

func (nativeBackend) Write(format clipboard.Format, data []byte) error {
	if changed := clipboard.Write(format, data); changed == nil {
		return ErrClipboardWriteFailed
	}
	return nil
}

func (nativeBackend) Watch(ctx context.Context, format clipboard.Format) <-chan []byte {
	return clipboard.Watch(ctx, format)
}

type observedEvent struct {
	bucket formatBucket
	data   []byte
}

// Manager owns native clipboard reads, writes, and anti-echo suppression.
//
// macOS builds require CGO because golang.design/x/clipboard uses Cocoa.
type Manager struct {
	backend clipboardBackend

	events   chan *clipwire.ClipboardPayload
	observed chan observedEvent

	pending *pendingLedger

	coalesceWindow time.Duration
	now            func() time.Time

	startOnce sync.Once
}

// NewManager initializes the native clipboard package and returns an idle
// manager. Start must be called to begin observing local clipboard changes.
func NewManager(eventBuffer int) (*Manager, error) {
	if err := clipboard.Init(); err != nil {
		return nil, err
	}

	return newManagerWithBackend(eventBuffer, nativeBackend{}), nil
}

func newManagerWithBackend(eventBuffer int, backend clipboardBackend) *Manager {
	if eventBuffer <= 0 {
		eventBuffer = defaultEventBuffer
	}

	return &Manager{
		backend:        backend,
		events:         make(chan *clipwire.ClipboardPayload, eventBuffer),
		observed:       make(chan observedEvent, eventBuffer*2),
		pending:        newPendingLedger(defaultPendingTTL),
		coalesceWindow: defaultCoalesceWindow,
		now:            time.Now,
	}
}

// Start launches background clipboard watchers. Repeated calls are ignored.
func (m *Manager) Start(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}

	m.startOnce.Do(func() {
		go m.watchLoop(ctx, clipboard.FmtText, textBucket)
		go m.watchLoop(ctx, clipboard.FmtImage, imageBucket)
		go m.processObserved(ctx)
	})
}

// Events exposes locally observed clipboard payloads for the network layer.
func (m *Manager) Events() <-chan *clipwire.ClipboardPayload {
	return m.events
}

// ApplyRemote writes a network payload into the local OS clipboard and records
// the exact bytes written so the next matching watcher event can be ignored.
func (m *Manager) ApplyRemote(ctx context.Context, payload *clipwire.ClipboardPayload) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := clipwire.ValidatePayload(payload); err != nil {
		return err
	}

	format, bucket, data, err := normalizeWritePayload(payload)
	if err != nil {
		return err
	}

	hash := clipwire.ComputeContentSHA256(data)
	now := m.now()
	m.pending.remember(bucket, hash, now)

	if err := m.backend.Write(format, data); err != nil {
		m.pending.forget(bucket, hash)
		return fmt.Errorf("osclip: write clipboard: %w", err)
	}

	return nil
}

func (m *Manager) watchLoop(ctx context.Context, format clipboard.Format, bucket formatBucket) {
	watchCh := m.backend.Watch(ctx, format)
	for {
		select {
		case <-ctx.Done():
			return
		case data, ok := <-watchCh:
			if !ok {
				return
			}
			if data == nil {
				continue
			}

			hash := clipwire.ComputeContentSHA256(data)
			if m.pending.consume(bucket, hash, m.now()) {
				continue
			}

			evt := observedEvent{
				bucket: bucket,
				data:   append([]byte(nil), data...),
			}

			select {
			case <-ctx.Done():
				return
			case m.observed <- evt:
			}
		}
	}
}

func (m *Manager) processObserved(ctx context.Context) {
	defer close(m.events)

	for {
		var first observedEvent
		select {
		case <-ctx.Done():
			return
		case first = <-m.observed:
		}

		batch := map[formatBucket]observedEvent{
			first.bucket: first,
		}

		timer := time.NewTimer(m.coalesceWindow)
	collect:
		for {
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case next := <-m.observed:
				// Keep only the latest value per native clipboard format inside
				// the coalescing window so one OS copy can emit both text and
				// image in a deterministic order.
				batch[next.bucket] = next
			case <-timer.C:
				break collect
			}
		}

		if event, ok := batch[textBucket]; ok {
			payload, err := buildTextPayload(m.now(), event.data)
			if err == nil && !m.emit(ctx, payload) {
				return
			}
		}

		if event, ok := batch[imageBucket]; ok {
			payload, err := clipwire.NewPayload(m.now(), clipwire.PayloadType_PAYLOAD_TYPE_IMAGE, event.data, "image/png")
			if err == nil && !m.emit(ctx, payload) {
				return
			}
		}
	}
}

func (m *Manager) emit(ctx context.Context, payload *clipwire.ClipboardPayload) bool {
	select {
	case <-ctx.Done():
		return false
	case m.events <- payload:
		return true
	}
}

func buildTextPayload(ts time.Time, data []byte) (*clipwire.ClipboardPayload, error) {
	return clipwire.NewPayload(ts, classifyTextPayload(data), data, "")
}

func classifyTextPayload(data []byte) clipwire.PayloadType {
	raw := string(data)
	if raw == "" || strings.TrimSpace(raw) != raw {
		return clipwire.PayloadType_PAYLOAD_TYPE_TEXT
	}

	parsed, err := url.Parse(raw)
	if err != nil || !parsed.IsAbs() || parsed.Host == "" {
		return clipwire.PayloadType_PAYLOAD_TYPE_TEXT
	}

	switch strings.ToLower(parsed.Scheme) {
	case "http", "https":
		return clipwire.PayloadType_PAYLOAD_TYPE_URL
	default:
		return clipwire.PayloadType_PAYLOAD_TYPE_TEXT
	}
}

func normalizeWritePayload(payload *clipwire.ClipboardPayload) (clipboard.Format, formatBucket, []byte, error) {
	switch payload.GetType() {
	case clipwire.PayloadType_PAYLOAD_TYPE_TEXT, clipwire.PayloadType_PAYLOAD_TYPE_URL:
		return clipboard.FmtText, textBucket, append([]byte(nil), payload.GetData()...), nil
	case clipwire.PayloadType_PAYLOAD_TYPE_IMAGE:
		data, err := normalizeImageBytes(payload.GetData(), payload.GetMimeType())
		if err != nil {
			return 0, 0, nil, err
		}
		return clipboard.FmtImage, imageBucket, data, nil
	default:
		return 0, 0, nil, clipwire.ErrInvalidPayloadType
	}
}

func normalizeImageBytes(data []byte, mimeType string) ([]byte, error) {
	switch mimeType {
	case "image/png":
		return append([]byte(nil), data...), nil
	case "image/jpeg":
		img, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, fmt.Errorf("osclip: decode jpeg: %w", err)
		}

		var out bytes.Buffer
		if err := png.Encode(&out, img); err != nil {
			return nil, fmt.Errorf("osclip: encode png: %w", err)
		}
		return out.Bytes(), nil
	default:
		return nil, clipwire.ErrInvalidImageMIME
	}
}

func encodeJPEG(img image.Image) ([]byte, error) {
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, nil); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}
