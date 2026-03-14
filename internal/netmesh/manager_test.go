package netmesh

import (
	"context"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/grandcat/zeroconf"
)

type recordingHandler struct {
	mu     sync.Mutex
	frames [][]byte
	seen   chan []byte
}

func newRecordingHandler() *recordingHandler {
	return &recordingHandler{
		seen: make(chan []byte, 16),
	}
}

func (h *recordingHandler) HandleFrame(_ context.Context, _ Peer, frame []byte) error {
	h.mu.Lock()
	h.frames = append(h.frames, append([]byte(nil), frame...))
	h.mu.Unlock()

	h.seen <- append([]byte(nil), frame...)
	return nil
}

func TestParseTXT(t *testing.T) {
	meta, err := parseTXT([]string{
		"id=node-a",
		"name=alpha",
		"ws_path=/ws",
		"proto=v1",
	})
	if err != nil {
		t.Fatalf("parseTXT returned error: %v", err)
	}

	if meta.id != "node-a" || meta.name != "alpha" || meta.wsPath != "/ws" || meta.proto != protocolVersion {
		t.Fatalf("unexpected metadata: %#v", meta)
	}
}

func TestParseTXTRejectsInvalidEntries(t *testing.T) {
	cases := [][]string{
		{"id=node-a", "name=alpha", "proto=v1"},
		{"id=node-a", "name=alpha", "ws_path=ws", "proto=v1"},
		{"id=node-a", "name=alpha", "ws_path=/ws", "proto=v2"},
		{"id=node-a", "ws_path=/ws", "proto=v1"},
	}

	for _, tc := range cases {
		if _, err := parseTXT(tc); err == nil {
			t.Fatalf("expected parseTXT(%v) to fail", tc)
		}
	}
}

func TestShouldDial(t *testing.T) {
	if !shouldDial("a", "b") {
		t.Fatal("expected lexicographically smaller node to dial")
	}
	if shouldDial("b", "a") {
		t.Fatal("expected lexicographically larger node to wait for inbound")
	}
}

func TestPreferredDirection(t *testing.T) {
	if got := preferredDirection("a", "b"); got != connectionDirectionOutbound {
		t.Fatalf("unexpected preferred direction: got %v want outbound", got)
	}
	if got := preferredDirection("b", "a"); got != connectionDirectionInbound {
		t.Fatalf("unexpected preferred direction: got %v want inbound", got)
	}
}

func TestHandleServiceEntryRefreshesPeer(t *testing.T) {
	manager, err := NewManager(Options{NodeID: "node-b", Name: "beta"}, FrameHandlerFunc(func(context.Context, Peer, []byte) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	first := serviceEntry("node-a", "alpha", 9001, "127.0.0.1")
	manager.handleServiceEntry(first)

	peers := manager.Peers()
	if len(peers) != 1 {
		t.Fatalf("expected one peer, got %d", len(peers))
	}
	if peers[0].Port != 9001 {
		t.Fatalf("unexpected initial port: got %d want 9001", peers[0].Port)
	}

	time.Sleep(10 * time.Millisecond)

	second := serviceEntry("node-a", "alpha-renamed", 9010, "127.0.0.2")
	manager.handleServiceEntry(second)

	peers = manager.Peers()
	if peers[0].Name != "alpha-renamed" {
		t.Fatalf("unexpected updated name: got %q", peers[0].Name)
	}
	if peers[0].Port != 9010 {
		t.Fatalf("unexpected updated port: got %d want 9010", peers[0].Port)
	}
	if got := peers[0].Addresses[0].String(); got != "127.0.0.2" {
		t.Fatalf("unexpected updated address: got %s", got)
	}
}

func TestPruneStalePeersKeepsLiveConnection(t *testing.T) {
	manager, err := NewManager(Options{NodeID: "node-b", Name: "beta"}, FrameHandlerFunc(func(context.Context, Peer, []byte) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	ps := manager.getOrCreatePeer("node-a")
	ps.mu.Lock()
	ps.discovered = true
	ps.peer = Peer{
		ID:        "node-a",
		Name:      "alpha",
		Host:      "localhost",
		Port:      9001,
		Addresses: []net.IP{net.ParseIP("127.0.0.1")},
		Connected: true,
		LastSeen:  time.Now().Add(-2 * manager.opts.PeerStaleAfter),
	}
	ps.conn = &managedConn{}
	ps.mu.Unlock()

	manager.pruneStalePeers(time.Now())

	peer := manager.Peers()[0]
	if !peer.Connected {
		t.Fatal("expected live connection to remain connected after pruning")
	}
	if peer.Port != 0 {
		t.Fatalf("expected stale discovery port to be cleared, got %d", peer.Port)
	}
}

func TestReconnectTriggeredOnlyForDialOwner(t *testing.T) {
	manager, err := NewManager(Options{NodeID: "node-a", Name: "alpha"}, FrameHandlerFunc(func(context.Context, Peer, []byte) error {
		return nil
	}))
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	dialOwner := manager.getOrCreatePeer("node-b")
	dialOwner.mu.Lock()
	dialOwner.discovered = true
	dialOwner.conn = &managedConn{}
	dialOwner.peer.ID = "node-b"
	dialOwner.peer.Connected = true
	dialOwner.mu.Unlock()

	manager.handleConnectionClosed(dialOwner, dialOwner.conn)

	select {
	case <-dialOwner.reconnectCh:
	default:
		t.Fatal("expected dial owner to be scheduled for reconnect")
	}

	waiter := manager.getOrCreatePeer("0")
	waiter.mu.Lock()
	waiter.discovered = true
	waiter.conn = &managedConn{}
	waiter.peer.ID = "0"
	waiter.peer.Connected = true
	waiter.mu.Unlock()

	manager.handleConnectionClosed(waiter, waiter.conn)

	select {
	case <-waiter.reconnectCh:
		t.Fatal("did not expect inbound owner to schedule reconnect")
	default:
	}
}

func TestManagedConnQueueSaturation(t *testing.T) {
	conn := &managedConn{
		ctx:   context.Background(),
		sendQ: make(chan []byte, 1),
	}
	conn.sendQ <- []byte("full")

	if err := conn.enqueue(context.Background(), []byte("overflow")); !errors.Is(err, ErrSendQueueFull) {
		t.Fatalf("unexpected enqueue error: got %v want %v", err, ErrSendQueueFull)
	}
}

func TestManagersSendReceiveAndReconnect(t *testing.T) {
	handlerA := newRecordingHandler()
	handlerB := newRecordingHandler()

	ctxA, cancelA := context.WithCancel(context.Background())
	defer cancelA()

	managerA := startTestManager(t, ctxA, Options{
		NodeID:              "node-a",
		Name:                "alpha",
		HeartbeatInterval:   50 * time.Millisecond,
		PongTimeout:         200 * time.Millisecond,
		ReconnectMinBackoff: 25 * time.Millisecond,
		ReconnectMaxBackoff: 50 * time.Millisecond,
	}, handlerA)

	ctxB, cancelB := context.WithCancel(context.Background())
	managerB := startTestManager(t, ctxB, Options{
		NodeID:              "node-b",
		Name:                "beta",
		HeartbeatInterval:   50 * time.Millisecond,
		PongTimeout:         200 * time.Millisecond,
		ReconnectMinBackoff: 25 * time.Millisecond,
		ReconnectMaxBackoff: 50 * time.Millisecond,
	}, handlerB)

	defer func() {
		cancelB()
		<-managerB.done
	}()

	managerA.handleServiceEntry(serviceEntryForManager(managerB))
	waitForConnected(t, managerA, "node-b")
	waitForConnected(t, managerB, "node-a")

	payload := []byte("first frame")
	if err := managerA.Send(context.Background(), "node-b", payload); err != nil {
		t.Fatalf("Send returned error: %v", err)
	}
	waitForFrame(t, handlerB, payload)

	cancelB()
	<-managerB.done

	ctxB2, cancelB2 := context.WithCancel(context.Background())
	managerB2 := startTestManager(t, ctxB2, Options{
		NodeID:              "node-b",
		Name:                "beta",
		HeartbeatInterval:   50 * time.Millisecond,
		PongTimeout:         200 * time.Millisecond,
		ReconnectMinBackoff: 25 * time.Millisecond,
		ReconnectMaxBackoff: 50 * time.Millisecond,
	}, handlerB)
	defer func() {
		cancelB2()
		<-managerB2.done
	}()

	managerA.handleServiceEntry(serviceEntryForManager(managerB2))
	waitForConnected(t, managerA, "node-b")
	waitForConnected(t, managerB2, "node-a")

	second := []byte("second frame")
	if err := managerA.Send(context.Background(), "node-b", second); err != nil {
		t.Fatalf("Send after reconnect returned error: %v", err)
	}
	waitForFrame(t, handlerB, second)
}

func TestOversizedInboundFrameIsRejected(t *testing.T) {
	handler := newRecordingHandler()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	manager := startTestManager(t, ctx, Options{
		NodeID:            "node-b",
		Name:              "beta",
		MaxFrameBytes:     32,
		HeartbeatInterval: 50 * time.Millisecond,
		PongTimeout:       200 * time.Millisecond,
	}, handler)
	defer func() {
		cancel()
		<-manager.done
	}()

	url := "ws://" + net.JoinHostPort("127.0.0.1", portString(manager)) + manager.opts.WebSocketPath
	header := http.Header{
		headerNodeID:   []string{"node-a"},
		headerNodeName: []string{"alpha"},
		headerProto:    []string{protocolVersion},
	}

	ws, _, err := websocket.DefaultDialer.Dial(url, header)
	if err != nil {
		t.Fatalf("Dial returned error: %v", err)
	}
	defer ws.Close()

	if err := ws.WriteMessage(websocket.BinaryMessage, make([]byte, 128)); err != nil {
		t.Fatalf("WriteMessage returned error: %v", err)
	}

	ws.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if _, _, err := ws.ReadMessage(); err == nil {
		t.Fatal("expected oversized inbound frame to close the connection")
	}

	select {
	case frame := <-handler.seen:
		t.Fatalf("unexpected frame delivered to handler: %q", string(frame))
	default:
	}
}

func startTestManager(t *testing.T, ctx context.Context, opts Options, handler FrameHandler) *Manager {
	t.Helper()

	manager, err := NewManager(opts, handler)
	if err != nil {
		t.Fatalf("NewManager returned error: %v", err)
	}

	if err := manager.Start(ctx); err != nil {
		t.Fatalf("Start returned error: %v", err)
	}

	return manager
}

func serviceEntry(id, name string, port int, addr string) *zeroconf.ServiceEntry {
	entry := zeroconf.NewServiceEntry("clippy", defaultServiceName, defaultDomain)
	entry.HostName = "localhost.local."
	entry.Port = port
	entry.Text = discoveryText(id, name, defaultWebSocketPath)
	entry.AddrIPv4 = []net.IP{net.ParseIP(addr)}
	return entry
}

func serviceEntryForManager(manager *Manager) *zeroconf.ServiceEntry {
	return serviceEntry(manager.nodeID, manager.name, manager.listenerPort(), "127.0.0.1")
}

func waitForConnected(t *testing.T, manager *Manager, peerID string) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		peers := manager.Peers()
		for _, peer := range peers {
			if peer.ID == peerID && peer.Connected {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("peer %s did not become connected", peerID)
}

func waitForFrame(t *testing.T, handler *recordingHandler, want []byte) {
	t.Helper()

	select {
	case got := <-handler.seen:
		if string(got) != string(want) {
			t.Fatalf("unexpected frame: got %q want %q", string(got), string(want))
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("timed out waiting for frame %q", string(want))
	}
}

func portString(manager *Manager) string {
	return strconv.Itoa(manager.listenerPort())
}
