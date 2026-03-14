package netmesh

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"sync"
	"time"

	"clippy/internal/clipwire"

	"github.com/gorilla/websocket"
	"github.com/grandcat/zeroconf"
)

const (
	protocolVersion = "v1"

	headerNodeID   = "X-Clippy-Node-ID"
	headerNodeName = "X-Clippy-Node-Name"
	headerProto    = "X-Clippy-Proto"

	defaultServiceName         = "_clippy._tcp"
	defaultDomain              = "local."
	defaultWebSocketPath       = "/ws"
	defaultHeartbeatInterval   = 10 * time.Second
	defaultPongTimeout         = 30 * time.Second
	defaultPeerStaleAfter      = 90 * time.Second
	defaultBrowseCycle         = 30 * time.Second
	defaultReconnectMinBackoff = 1 * time.Second
	defaultReconnectMaxBackoff = 30 * time.Second
	defaultSendQueueSize       = 64
	defaultWriteTimeout        = 5 * time.Second
	defaultAdvertiseRefresh    = 60 * time.Second
	defaultDiscoveryRetryDelay = 3 * time.Second
	minimumAdvertiseRefresh    = 15 * time.Second
	defaultMaxFrameBytes       = clipwire.DefaultChunkSize + 64<<10
	defaultInstanceNamePrefix  = "clippy"
)

var (
	ErrNilHandler        = errors.New("netmesh: frame handler is nil")
	ErrManagerStarted    = errors.New("netmesh: manager already started")
	ErrManagerNotStarted = errors.New("netmesh: manager is not started")
	ErrPeerNotFound      = errors.New("netmesh: peer not found")
	ErrPeerNotConnected  = errors.New("netmesh: peer is not connected")
	ErrSendQueueFull     = errors.New("netmesh: send queue is full")
	ErrInvalidFrame      = errors.New("netmesh: frame exceeds configured maximum")
)

// FrameHandler receives raw binary frames from a remote peer.
//
// The transport treats every WebSocket binary message as one opaque payload.
// Higher layers own protobuf decoding and clipboard semantics.
type FrameHandler interface {
	HandleFrame(ctx context.Context, from Peer, frame []byte) error
}

// FrameHandlerFunc adapts a function into a FrameHandler.
type FrameHandlerFunc func(ctx context.Context, from Peer, frame []byte) error

// HandleFrame implements FrameHandler.
func (f FrameHandlerFunc) HandleFrame(ctx context.Context, from Peer, frame []byte) error {
	return f(ctx, from, frame)
}

// Options controls discovery, transport, and retry behavior.
type Options struct {
	NodeID string
	Name   string

	ServiceName   string
	Domain        string
	WebSocketPath string

	ListenAddr string
	Port       int

	HeartbeatInterval   time.Duration
	PongTimeout         time.Duration
	PeerStaleAfter      time.Duration
	BrowseCycle         time.Duration
	ReconnectMinBackoff time.Duration
	ReconnectMaxBackoff time.Duration
	MaxFrameBytes       int
	SendQueueSize       int
}

// Peer is the stable snapshot exposed to callers.
type Peer struct {
	ID        string
	Name      string
	Host      string
	Port      int
	Addresses []net.IP
	Connected bool
	LastSeen  time.Time
}

// Manager owns the local WebSocket server, mDNS discovery loops, and one
// connection state machine per discovered peer.
type Manager struct {
	opts    Options
	handler FrameHandler

	nodeID string
	name   string

	mu       sync.RWMutex
	peers    map[string]*peerState
	started  bool
	startErr error

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}

	listener   net.Listener
	httpServer *http.Server
	upgrader   websocket.Upgrader
	dialer     websocket.Dialer

	advertiseMu sync.Mutex
	advertiser  *zeroconf.Server

	shutdownOnce sync.Once
	wg           sync.WaitGroup
}

// NodeID returns the local node identifier chosen for this manager instance.
func (m *Manager) NodeID() string {
	return m.nodeID
}

// Name returns the local node display name.
func (m *Manager) Name() string {
	return m.name
}

// NewManager validates the caller-provided settings and prepares an idle manager.
func NewManager(opts Options, handler FrameHandler) (*Manager, error) {
	if handler == nil {
		return nil, ErrNilHandler
	}

	resolved, err := withDefaults(opts)
	if err != nil {
		return nil, err
	}

	return &Manager{
		opts:    resolved,
		handler: handler,
		nodeID:  resolved.NodeID,
		name:    resolved.Name,
		peers:   make(map[string]*peerState),
		done:    make(chan struct{}),
		upgrader: websocket.Upgrader{
			CheckOrigin: func(*http.Request) bool {
				return true
			},
		},
		dialer: websocket.Dialer{
			HandshakeTimeout: resolved.PongTimeout,
		},
	}, nil
}

// Start launches the local server and the background discovery supervisors.
//
// It returns once the listener is ready and the long-lived goroutines have been
// started. Shutdown is driven by the passed context.
func (m *Manager) Start(ctx context.Context) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	if m.started {
		return ErrManagerStarted
	}

	listener, err := net.Listen("tcp", listenAddress(m.opts.ListenAddr, m.opts.Port))
	if err != nil {
		return fmt.Errorf("netmesh: listen: %w", err)
	}

	mctx, cancel := context.WithCancel(ctx)
	m.ctx = mctx
	m.cancel = cancel
	m.listener = listener

	mux := http.NewServeMux()
	mux.HandleFunc(m.opts.WebSocketPath, m.handleWebSocket)

	m.httpServer = &http.Server{
		Handler: mux,
	}

	m.started = true

	m.wg.Add(4)
	go m.serveHTTP()
	go m.advertiseLoop()
	go m.browseLoop()
	go m.pruneLoop()
	go func() {
		<-m.ctx.Done()
		m.shutdown()
	}()

	return nil
}

// Send delivers a frame to one connected peer.
func (m *Manager) Send(ctx context.Context, peerID string, frame []byte) error {
	if len(frame) > m.opts.MaxFrameBytes {
		return ErrInvalidFrame
	}

	ps, err := m.peerState(peerID)
	if err != nil {
		return err
	}

	conn := ps.activeConn()
	if conn == nil {
		return ErrPeerNotConnected
	}

	return conn.enqueue(ctx, frame)
}

// Broadcast delivers the same frame to every currently connected peer.
func (m *Manager) Broadcast(ctx context.Context, frame []byte) error {
	if len(frame) > m.opts.MaxFrameBytes {
		return ErrInvalidFrame
	}

	m.mu.RLock()
	peers := make([]*peerState, 0, len(m.peers))
	for _, ps := range m.peers {
		peers = append(peers, ps)
	}
	m.mu.RUnlock()

	var errs []error
	for _, ps := range peers {
		conn := ps.activeConn()
		if conn == nil {
			continue
		}
		if err := conn.enqueue(ctx, frame); err != nil {
			errs = append(errs, fmt.Errorf("peer %s: %w", ps.id, err))
		}
	}

	return errors.Join(errs...)
}

// Peers returns a copy of the current peer table.
func (m *Manager) Peers() []Peer {
	m.mu.RLock()
	peers := make([]*peerState, 0, len(m.peers))
	for _, ps := range m.peers {
		peers = append(peers, ps)
	}
	m.mu.RUnlock()

	snapshots := make([]Peer, 0, len(peers))
	for _, ps := range peers {
		snapshots = append(snapshots, ps.snapshot())
	}

	sort.Slice(snapshots, func(i, j int) bool {
		return snapshots[i].ID < snapshots[j].ID
	})

	return snapshots
}

func (m *Manager) serveHTTP() {
	defer m.wg.Done()

	err := m.httpServer.Serve(m.listener)
	if err != nil && !errors.Is(err, http.ErrServerClosed) {
		m.cancel()
	}
}

func (m *Manager) shutdown() {
	m.shutdownOnce.Do(func() {
		if m.cancel != nil {
			m.cancel()
		}

		m.advertiseMu.Lock()
		if m.advertiser != nil {
			m.advertiser.Shutdown()
			m.advertiser = nil
		}
		m.advertiseMu.Unlock()

		if m.httpServer != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			_ = m.httpServer.Shutdown(ctx)
			cancel()
		}
		if m.listener != nil {
			_ = m.listener.Close()
		}

		m.mu.RLock()
		peers := make([]*peerState, 0, len(m.peers))
		for _, ps := range m.peers {
			peers = append(peers, ps)
		}
		m.mu.RUnlock()

		for _, ps := range peers {
			if conn := ps.activeConn(); conn != nil {
				conn.close(websocket.CloseNormalClosure, "manager shutting down")
			}
		}

		m.wg.Wait()
		close(m.done)
	})
}

// Wait blocks until the manager has fully shut down.
func (m *Manager) Wait() {
	<-m.done
}

func (m *Manager) peerState(peerID string) (*peerState, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()

	if !m.started {
		return nil, ErrManagerNotStarted
	}

	ps, ok := m.peers[peerID]
	if !ok {
		return nil, ErrPeerNotFound
	}

	return ps, nil
}

func (m *Manager) getOrCreatePeer(peerID string) *peerState {
	m.mu.Lock()
	defer m.mu.Unlock()

	ps, ok := m.peers[peerID]
	if ok {
		return ps
	}

	ps = &peerState{
		id:          peerID,
		reconnectCh: make(chan struct{}, 1),
	}
	m.peers[peerID] = ps
	return ps
}

func (m *Manager) ensureDialLoop(ps *peerState) {
	ps.mu.Lock()
	if ps.dialLoopStarted {
		ps.mu.Unlock()
		return
	}
	ps.dialLoopStarted = true
	ps.mu.Unlock()

	m.wg.Add(1)
	go m.dialLoop(ps)
}

func (m *Manager) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != m.opts.WebSocketPath {
		http.NotFound(w, r)
		return
	}

	remoteID := r.Header.Get(headerNodeID)
	remoteName := r.Header.Get(headerNodeName)
	remoteProto := r.Header.Get(headerProto)
	if remoteID == "" {
		http.Error(w, "missing node id", http.StatusBadRequest)
		return
	}
	if remoteProto != protocolVersion {
		http.Error(w, "unsupported protocol version", http.StatusPreconditionFailed)
		return
	}
	if remoteID == m.nodeID {
		http.Error(w, "self connection rejected", http.StatusConflict)
		return
	}

	respHeader := http.Header{
		headerNodeID:   []string{m.nodeID},
		headerNodeName: []string{m.name},
		headerProto:    []string{protocolVersion},
	}

	ws, err := m.upgrader.Upgrade(w, r, respHeader)
	if err != nil {
		return
	}

	ps := m.getOrCreatePeer(remoteID)
	ps.mu.Lock()
	if remoteName != "" {
		ps.peer.Name = remoteName
	}
	ps.mu.Unlock()

	conn := newManagedConn(m.ctx, ws, connectionDirectionInbound, remoteID, remoteName, m.opts.SendQueueSize)
	if !m.attachConnection(ps, conn) {
		conn.close(websocket.CloseNormalClosure, "duplicate connection")
	}
}

func (m *Manager) attachConnection(ps *peerState, conn *managedConn) bool {
	var old *managedConn
	var accepted bool

	ps.mu.Lock()
	defer ps.mu.Unlock()

	ps.peer.ID = ps.id
	if conn.peerName != "" {
		ps.peer.Name = conn.peerName
	}

	preferred := preferredDirection(m.nodeID, ps.id)
	switch {
	case ps.conn == nil:
		ps.conn = conn
		ps.peer.Connected = true
		ps.backoff = 0
		accepted = true
	case ps.conn.direction == conn.direction:
		accepted = false
	case conn.direction == preferred:
		old = ps.conn
		ps.conn = conn
		ps.peer.Connected = true
		ps.backoff = 0
		accepted = true
	default:
		accepted = false
	}

	if accepted {
		conn.start(m, ps)
	}
	if old != nil {
		go old.close(websocket.CloseNormalClosure, "superseded by preferred connection")
	}

	return accepted
}

func (m *Manager) handleConnectionClosed(ps *peerState, conn *managedConn) {
	ps.mu.Lock()
	if ps.conn != conn {
		ps.mu.Unlock()
		return
	}

	ps.conn = nil
	ps.peer.Connected = false
	shouldReconnect := ps.discovered && shouldDial(m.nodeID, ps.id)
	ps.mu.Unlock()

	if shouldReconnect {
		ps.triggerReconnect()
	}
}

func (m *Manager) dialPeer(ps *peerState, peer Peer, wsPath string) error {
	requestHeader := http.Header{
		headerNodeID:   []string{m.nodeID},
		headerNodeName: []string{m.name},
		headerProto:    []string{protocolVersion},
	}

	var lastErr error
	for _, addr := range peer.Addresses {
		url := fmt.Sprintf("ws://%s%s", net.JoinHostPort(addr.String(), strconv.Itoa(peer.Port)), wsPath)
		ws, resp, err := m.dialer.DialContext(m.ctx, url, requestHeader)
		if err != nil {
			lastErr = err
			continue
		}

		remoteID := responseHeader(resp, headerNodeID)
		if remoteID == "" {
			_ = ws.Close()
			lastErr = errors.New("missing peer id in websocket response")
			continue
		}
		if remoteID != peer.ID {
			_ = ws.Close()
			lastErr = fmt.Errorf("discovered peer %s responded as %s", peer.ID, remoteID)
			continue
		}
		if responseHeader(resp, headerProto) != protocolVersion {
			_ = ws.Close()
			lastErr = errors.New("unsupported peer protocol version")
			continue
		}

		remoteName := responseHeader(resp, headerNodeName)
		if remoteName == "" {
			remoteName = peer.Name
		}

		conn := newManagedConn(m.ctx, ws, connectionDirectionOutbound, peer.ID, remoteName, m.opts.SendQueueSize)
		if !m.attachConnection(ps, conn) {
			conn.close(websocket.CloseNormalClosure, "duplicate connection")
		}
		return nil
	}

	if lastErr == nil {
		lastErr = errors.New("no dialable addresses discovered")
	}

	return lastErr
}

func withDefaults(opts Options) (Options, error) {
	resolved := opts

	if resolved.ServiceName == "" {
		resolved.ServiceName = defaultServiceName
	}
	if resolved.Domain == "" {
		resolved.Domain = defaultDomain
	}
	if resolved.WebSocketPath == "" {
		resolved.WebSocketPath = defaultWebSocketPath
	}
	if resolved.HeartbeatInterval <= 0 {
		resolved.HeartbeatInterval = defaultHeartbeatInterval
	}
	if resolved.PongTimeout <= 0 {
		resolved.PongTimeout = defaultPongTimeout
	}
	if resolved.PeerStaleAfter <= 0 {
		resolved.PeerStaleAfter = defaultPeerStaleAfter
	}
	if resolved.BrowseCycle <= 0 {
		resolved.BrowseCycle = defaultBrowseCycle
	}
	if resolved.ReconnectMinBackoff <= 0 {
		resolved.ReconnectMinBackoff = defaultReconnectMinBackoff
	}
	if resolved.ReconnectMaxBackoff <= 0 {
		resolved.ReconnectMaxBackoff = defaultReconnectMaxBackoff
	}
	if resolved.ReconnectMaxBackoff < resolved.ReconnectMinBackoff {
		return Options{}, errors.New("netmesh: reconnect max backoff must be >= min backoff")
	}
	if resolved.MaxFrameBytes <= 0 {
		resolved.MaxFrameBytes = defaultMaxFrameBytes
	}
	if resolved.SendQueueSize <= 0 {
		resolved.SendQueueSize = defaultSendQueueSize
	}
	if resolved.Port < 0 {
		return Options{}, errors.New("netmesh: port must be >= 0")
	}

	if resolved.NodeID == "" {
		nodeID, err := newNodeID()
		if err != nil {
			return Options{}, fmt.Errorf("netmesh: generate node id: %w", err)
		}
		resolved.NodeID = nodeID
	}
	if resolved.Name == "" {
		host, err := os.Hostname()
		if err != nil {
			return Options{}, fmt.Errorf("netmesh: hostname: %w", err)
		}
		resolved.Name = host
	}

	return resolved, nil
}

func listenAddress(host string, port int) string {
	if host == "" {
		return fmt.Sprintf(":%d", port)
	}
	return net.JoinHostPort(host, strconv.Itoa(port))
}

func newNodeID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(raw[:]), nil
}

func shouldDial(selfID, peerID string) bool {
	return selfID < peerID
}

func preferredDirection(selfID, peerID string) connectionDirection {
	if shouldDial(selfID, peerID) {
		return connectionDirectionOutbound
	}
	return connectionDirectionInbound
}

func responseHeader(resp *http.Response, name string) string {
	if resp == nil {
		return ""
	}
	return resp.Header.Get(name)
}
