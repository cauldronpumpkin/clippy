package netmesh

import (
	"context"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

type connectionDirection uint8

const (
	connectionDirectionInbound connectionDirection = iota + 1
	connectionDirectionOutbound
)

type peerState struct {
	mu sync.Mutex

	id   string
	peer Peer

	discovered      bool
	wsPath          string
	proto           string
	conn            *managedConn
	backoff         time.Duration
	reconnectCh     chan struct{}
	dialLoopStarted bool
}

func (ps *peerState) snapshot() Peer {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.peerCopyLocked()
}

func (ps *peerState) peerCopyLocked() Peer {
	snapshot := ps.peer
	snapshot.ID = ps.id
	snapshot.Addresses = copyIPs(ps.peer.Addresses)
	return snapshot
}

func (ps *peerState) activeConn() *managedConn {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	return ps.conn
}

func (ps *peerState) triggerReconnect() {
	select {
	case ps.reconnectCh <- struct{}{}:
	default:
	}
}

func (ps *peerState) resetBackoff() {
	ps.mu.Lock()
	defer ps.mu.Unlock()
	ps.backoff = 0
}

func (ps *peerState) bumpBackoff(minimum, maximum time.Duration) time.Duration {
	if ps.backoff <= 0 {
		ps.backoff = minimum
		return ps.backoff
	}

	ps.backoff *= 2
	if ps.backoff > maximum {
		ps.backoff = maximum
	}
	return ps.backoff
}

// managedConn wraps a WebSocket with the goroutines and channels required to
// enforce serialized writes and deterministic shutdown.
type managedConn struct {
	ws        *websocket.Conn
	direction connectionDirection
	peerID    string
	peerName  string

	ctx    context.Context
	cancel context.CancelFunc

	sendQ     chan []byte
	closeOnce sync.Once
}

func newManagedConn(parent context.Context, ws *websocket.Conn, direction connectionDirection, peerID, peerName string, sendQueueSize int) *managedConn {
	ctx, cancel := context.WithCancel(parent)
	return &managedConn{
		ws:        ws,
		direction: direction,
		peerID:    peerID,
		peerName:  peerName,
		ctx:       ctx,
		cancel:    cancel,
		sendQ:     make(chan []byte, sendQueueSize),
	}
}

func (c *managedConn) start(m *Manager, ps *peerState) {
	m.wg.Add(2)
	go c.readPump(m, ps)
	go c.writePump(m, ps)
}

func (c *managedConn) enqueue(ctx context.Context, frame []byte) error {
	if len(frame) == 0 {
		frame = nil
	}

	payload := append([]byte(nil), frame...)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.ctx.Done():
		return ErrPeerNotConnected
	case c.sendQ <- payload:
		return nil
	default:
		return ErrSendQueueFull
	}
}

func (c *managedConn) readPump(m *Manager, ps *peerState) {
	defer m.wg.Done()
	defer c.close(websocket.CloseNormalClosure, "reader stopped")
	defer m.handleConnectionClosed(ps, c)

	c.ws.SetReadLimit(int64(m.opts.MaxFrameBytes))
	_ = c.ws.SetReadDeadline(time.Now().Add(m.opts.PongTimeout))
	c.ws.SetPongHandler(func(string) error {
		return c.ws.SetReadDeadline(time.Now().Add(m.opts.PongTimeout))
	})

	for {
		messageType, payload, err := c.ws.ReadMessage()
		if err != nil {
			return
		}
		if messageType != websocket.BinaryMessage {
			continue
		}

		frame := append([]byte(nil), payload...)
		if err := m.handler.HandleFrame(c.ctx, ps.snapshot(), frame); err != nil {
			return
		}
	}
}

func (c *managedConn) writePump(m *Manager, ps *peerState) {
	defer m.wg.Done()
	defer c.close(websocket.CloseNormalClosure, "writer stopped")
	defer m.handleConnectionClosed(ps, c)

	ticker := time.NewTicker(m.opts.HeartbeatInterval)
	defer ticker.Stop()

	for {
		select {
		case <-c.ctx.Done():
			return
		case frame := <-c.sendQ:
			if err := c.write(websocket.BinaryMessage, frame); err != nil {
				return
			}
		case <-ticker.C:
			if err := c.write(websocket.PingMessage, nil); err != nil {
				return
			}
		}
	}
}

func (c *managedConn) write(messageType int, payload []byte) error {
	if err := c.ws.SetWriteDeadline(time.Now().Add(defaultWriteTimeout)); err != nil {
		return err
	}
	return c.ws.WriteMessage(messageType, payload)
}

func (c *managedConn) close(code int, text string) {
	c.closeOnce.Do(func() {
		c.cancel()
		if c.ws == nil {
			return
		}
		_ = c.write(websocket.CloseMessage, websocket.FormatCloseMessage(code, text))
		_ = c.ws.Close()
	})
}
