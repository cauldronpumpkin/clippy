package app

import (
	"bytes"
	"context"
	"io"
	"log"
	"sync"
	"testing"
	"time"

	"clippy/internal/clipwire"
	"clippy/internal/netmesh"

	"google.golang.org/protobuf/proto"
)

type fakeClipboard struct {
	events  chan *clipwire.ClipboardPayload
	applied []*clipwire.ClipboardPayload
	mu      sync.Mutex
}

func newFakeClipboard() *fakeClipboard {
	return &fakeClipboard{events: make(chan *clipwire.ClipboardPayload, 8)}
}

func (f *fakeClipboard) Start(context.Context) {}

func (f *fakeClipboard) Events() <-chan *clipwire.ClipboardPayload {
	return f.events
}

func (f *fakeClipboard) ApplyRemote(_ context.Context, payload *clipwire.ClipboardPayload) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.applied = append(f.applied, payload)
	return nil
}

type fakeMesh struct {
	handler    netmesh.FrameHandler
	broadcasts [][]byte
	peers      []netmesh.Peer
	started    bool
	startMu    sync.Mutex
}

func (f *fakeMesh) Start(context.Context) error {
	f.startMu.Lock()
	defer f.startMu.Unlock()
	f.started = true
	return nil
}

func (f *fakeMesh) Broadcast(_ context.Context, frame []byte) error {
	f.broadcasts = append(f.broadcasts, append([]byte(nil), frame...))
	return nil
}

func (f *fakeMesh) Peers() []netmesh.Peer {
	return append([]netmesh.Peer(nil), f.peers...)
}

func (f *fakeMesh) NodeID() string {
	return "node-1"
}

func (f *fakeMesh) Name() string {
	return "test-node"
}

func (f *fakeMesh) Wait() {}

func TestRuntimeBroadcastsClipboardPayload(t *testing.T) {
	t.Parallel()

	clipboard := newFakeClipboard()
	mesh := &fakeMesh{}
	rt := newRuntimeForTest(log.New(io.Discard, "", 0), runtimeDeps{
		clipboard: clipboard,
		mesh:      mesh,
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := rt.Start(ctx); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_TEXT, []byte("hello"), "")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	clipboard.events <- payload

	deadline := time.Now().Add(2 * time.Second)
	for len(mesh.broadcasts) == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if len(mesh.broadcasts) == 0 {
		t.Fatal("expected at least one broadcast frame")
	}

	var chunk clipwire.PayloadChunk
	if err := proto.Unmarshal(mesh.broadcasts[0], &chunk); err != nil {
		t.Fatalf("proto.Unmarshal() error = %v", err)
	}
	if !bytes.Equal(chunk.GetTransferId(), payload.GetSha256()) {
		t.Fatal("unexpected transfer id in outbound chunk")
	}
}

func TestRuntimeAppliesInboundPayload(t *testing.T) {
	t.Parallel()

	clipboard := newFakeClipboard()
	mesh := &fakeMesh{}
	rt := newRuntimeForTest(log.New(io.Discard, "", 0), runtimeDeps{
		clipboard: clipboard,
		mesh:      mesh,
	})

	payload, err := clipwire.NewPayload(time.Now(), clipwire.PayloadType_PAYLOAD_TYPE_TEXT, []byte("hello"), "")
	if err != nil {
		t.Fatalf("NewPayload() error = %v", err)
	}
	chunks, err := clipwire.ChunkPayload(payload, clipwire.DefaultChunkSize)
	if err != nil {
		t.Fatalf("ChunkPayload() error = %v", err)
	}
	frame, err := proto.Marshal(chunks[0])
	if err != nil {
		t.Fatalf("proto.Marshal() error = %v", err)
	}

	if err := rt.handleFrame(context.Background(), netmesh.Peer{ID: "peer-1"}, frame); err != nil {
		t.Fatalf("handleFrame() error = %v", err)
	}

	clipboard.mu.Lock()
	defer clipboard.mu.Unlock()
	if len(clipboard.applied) != 1 {
		t.Fatalf("expected one applied payload, got %d", len(clipboard.applied))
	}
	if string(clipboard.applied[0].GetData()) != "hello" {
		t.Fatalf("unexpected applied payload %q", string(clipboard.applied[0].GetData()))
	}
}

func TestRuntimeDropsInvalidFrame(t *testing.T) {
	t.Parallel()

	rt := newRuntimeForTest(log.New(io.Discard, "", 0), runtimeDeps{
		clipboard: newFakeClipboard(),
		mesh:      &fakeMesh{},
	})

	if err := rt.handleFrame(context.Background(), netmesh.Peer{ID: "peer-1"}, []byte("not-protobuf")); err != nil {
		t.Fatalf("handleFrame() error = %v", err)
	}
}
