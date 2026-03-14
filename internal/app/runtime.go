package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	"clippy/internal/clipwire"
	"clippy/internal/netmesh"
	"clippy/internal/osclip"

	"google.golang.org/protobuf/proto"
)

type clipboardManager interface {
	Start(context.Context)
	Events() <-chan *clipwire.ClipboardPayload
	ApplyRemote(context.Context, *clipwire.ClipboardPayload) error
}

type meshManager interface {
	Start(context.Context) error
	Broadcast(context.Context, []byte) error
	Peers() []netmesh.Peer
	NodeID() string
	Name() string
	Wait()
}

type runtimeDeps struct {
	clipboard clipboardManager
	mesh      meshManager
}

// PeerStatus is the control-plane view of a discovered peer.
type PeerStatus struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	Host      string    `json:"host"`
	Port      int       `json:"port"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"last_seen"`
}

// Status is returned over the local control protocol.
type Status struct {
	Running   bool         `json:"running"`
	Name      string       `json:"name"`
	NodeID    string       `json:"node_id"`
	StartedAt time.Time    `json:"started_at"`
	Peers     []PeerStatus `json:"peers"`
}

// Runtime wires clipboard observation, network transport, and inbound apply.
type Runtime struct {
	logger *log.Logger

	clipboard   clipboardManager
	mesh        meshManager
	reassembler *clipwire.Reassembler

	startedAt time.Time

	runOnce  sync.Once
	waitCh   chan struct{}
	waitErr  error
	stateMu  sync.Mutex
	peerSeen map[string]bool
}

// New constructs a production runtime using the native clipboard and mesh managers.
func New(cfg Config, logger *log.Logger) (*Runtime, error) {
	cfg = cfg.WithDefaults()

	clipboard, err := osclip.NewManager(0)
	if err != nil {
		return nil, err
	}

	rt := &Runtime{
		logger:      logger,
		clipboard:   clipboard,
		reassembler: clipwire.NewReassembler(),
		waitCh:      make(chan struct{}),
		peerSeen:    make(map[string]bool),
	}

	mesh, err := netmesh.NewManager(netmesh.Options{
		Name:          cfg.Name,
		ListenAddr:    cfg.ListenAddr,
		Port:          cfg.Port,
		ServiceName:   cfg.ServiceName,
		Domain:        cfg.Domain,
		WebSocketPath: cfg.WebSocketPath,
	}, netmesh.FrameHandlerFunc(rt.handleFrame))
	if err != nil {
		return nil, err
	}

	rt.mesh = mesh
	return rt, nil
}

func newRuntimeForTest(logger *log.Logger, deps runtimeDeps) *Runtime {
	return &Runtime{
		logger:      logger,
		clipboard:   deps.clipboard,
		mesh:        deps.mesh,
		reassembler: clipwire.NewReassembler(),
		waitCh:      make(chan struct{}),
		peerSeen:    make(map[string]bool),
	}
}

// Start launches the runtime and returns once the clipboard and mesh are active.
func (r *Runtime) Start(ctx context.Context) error {
	var startErr error
	r.runOnce.Do(func() {
		r.startedAt = time.Now().UTC()

		r.clipboard.Start(ctx)
		if err := r.mesh.Start(ctx); err != nil {
			startErr = err
			r.waitErr = err
			close(r.waitCh)
			return
		}

		r.logger.Printf("daemon started: name=%s node_id=%s", r.mesh.Name(), r.mesh.NodeID())

		go r.runClipboardLoop(ctx)
		go r.runPeerLogLoop(ctx)
		go func() {
			<-ctx.Done()
			r.mesh.Wait()
			r.logger.Printf("daemon stopped")
			close(r.waitCh)
		}()
	})
	return startErr
}

// Wait blocks until the runtime has stopped.
func (r *Runtime) Wait() error {
	<-r.waitCh
	return r.waitErr
}

// Status returns a snapshot for the local control API.
func (r *Runtime) Status() Status {
	peers := r.mesh.Peers()
	snapshot := Status{
		Running:   true,
		Name:      r.mesh.Name(),
		NodeID:    r.mesh.NodeID(),
		StartedAt: r.startedAt,
		Peers:     make([]PeerStatus, 0, len(peers)),
	}

	for _, peer := range peers {
		snapshot.Peers = append(snapshot.Peers, PeerStatus{
			ID:        peer.ID,
			Name:      peer.Name,
			Host:      peer.Host,
			Port:      peer.Port,
			Connected: peer.Connected,
			LastSeen:  peer.LastSeen,
		})
	}

	return snapshot
}

func (r *Runtime) runClipboardLoop(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case payload, ok := <-r.clipboard.Events():
			if !ok {
				return
			}
			if payload == nil {
				continue
			}

			r.logger.Printf("clipboard event: type=%s size=%d", payload.GetType().String(), payload.GetSizeBytes())
			if err := r.broadcastPayload(ctx, payload); err != nil && !errors.Is(err, context.Canceled) {
				r.logger.Printf("broadcast failed: %v", err)
			}
		}
	}
}

func (r *Runtime) runPeerLogLoop(ctx context.Context) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.logPeerChanges()
		}
	}
}

func (r *Runtime) logPeerChanges() {
	peers := r.mesh.Peers()
	current := make(map[string]bool, len(peers))

	r.stateMu.Lock()
	defer r.stateMu.Unlock()

	for _, peer := range peers {
		current[peer.ID] = peer.Connected

		prev, known := r.peerSeen[peer.ID]
		switch {
		case !known:
			r.logger.Printf("peer discovered: id=%s name=%s host=%s port=%d connected=%t", peer.ID, peer.Name, peer.Host, peer.Port, peer.Connected)
		case prev != peer.Connected:
			r.logger.Printf("peer state changed: id=%s connected=%t", peer.ID, peer.Connected)
		}
	}

	for id := range r.peerSeen {
		if _, ok := current[id]; !ok {
			r.logger.Printf("peer expired: id=%s", id)
		}
	}

	r.peerSeen = current
}

func (r *Runtime) broadcastPayload(ctx context.Context, payload *clipwire.ClipboardPayload) error {
	chunks, err := clipwire.ChunkPayload(payload, clipwire.DefaultChunkSize)
	if err != nil {
		return err
	}

	for _, chunk := range chunks {
		frame, err := proto.Marshal(chunk)
		if err != nil {
			return fmt.Errorf("marshal chunk: %w", err)
		}
		if err := r.mesh.Broadcast(ctx, frame); err != nil {
			return err
		}
	}

	return nil
}

func (r *Runtime) handleFrame(ctx context.Context, from netmesh.Peer, frame []byte) error {
	var chunk clipwire.PayloadChunk
	if err := proto.Unmarshal(frame, &chunk); err != nil {
		r.logger.Printf("dropped invalid frame from peer=%s: %v", from.ID, err)
		return nil
	}

	payload, complete, err := r.reassembler.Add(&chunk)
	if err != nil {
		r.logger.Printf("dropped invalid chunk from peer=%s: %v", from.ID, err)
		return nil
	}
	if !complete {
		return nil
	}

	if err := r.clipboard.ApplyRemote(ctx, payload); err != nil {
		r.logger.Printf("apply remote payload failed: peer=%s err=%v", from.ID, err)
		return nil
	}

	r.logger.Printf("applied remote payload: peer=%s type=%s size=%d", from.ID, payload.GetType().String(), payload.GetSizeBytes())
	return nil
}
