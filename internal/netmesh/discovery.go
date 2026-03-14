package netmesh

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"sort"
	"strings"
	"time"

	"github.com/grandcat/zeroconf"
)

type serviceMetadata struct {
	id     string
	name   string
	wsPath string
	proto  string
}

func (m *Manager) advertiseLoop() {
	defer m.wg.Done()

	refresh := defaultAdvertiseRefresh
	if refresh < minimumAdvertiseRefresh {
		refresh = minimumAdvertiseRefresh
	}

	for {
		if err := m.ctx.Err(); err != nil {
			return
		}

		server, err := zeroconf.Register(
			m.instanceName(),
			m.opts.ServiceName,
			m.opts.Domain,
			m.listenerPort(),
			discoveryText(m.nodeID, m.name, m.opts.WebSocketPath),
			nil,
		)
		if err != nil {
			if !sleepContext(m.ctx, defaultDiscoveryRetryDelay) {
				return
			}
			continue
		}

		m.advertiseMu.Lock()
		m.advertiser = server
		m.advertiseMu.Unlock()

		timer := time.NewTimer(refresh)
		select {
		case <-m.ctx.Done():
		case <-timer.C:
		}
		timer.Stop()

		m.advertiseMu.Lock()
		if m.advertiser == server {
			m.advertiser = nil
		}
		m.advertiseMu.Unlock()

		server.Shutdown()
	}
}

func (m *Manager) browseLoop() {
	defer m.wg.Done()

	for {
		if err := m.ctx.Err(); err != nil {
			return
		}

		resolver, err := zeroconf.NewResolver()
		if err != nil {
			if !sleepContext(m.ctx, defaultDiscoveryRetryDelay) {
				return
			}
			continue
		}

		sessionCtx, cancel := context.WithTimeout(m.ctx, m.opts.BrowseCycle)
		entries := make(chan *zeroconf.ServiceEntry)
		if err := resolver.Browse(sessionCtx, m.opts.ServiceName, m.opts.Domain, entries); err != nil {
			cancel()
			if !sleepContext(m.ctx, defaultDiscoveryRetryDelay) {
				return
			}
			continue
		}

		for {
			select {
			case <-m.ctx.Done():
				cancel()
				return
			case entry, ok := <-entries:
				if !ok {
					cancel()
					goto nextSession
				}
				m.handleServiceEntry(entry)
			}
		}

	nextSession:
		if !sleepContext(m.ctx, 250*time.Millisecond) {
			return
		}
	}
}

func (m *Manager) pruneLoop() {
	defer m.wg.Done()

	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-m.ctx.Done():
			return
		case now := <-ticker.C:
			m.pruneStalePeers(now)
		}
	}
}

func (m *Manager) pruneStalePeers(now time.Time) {
	m.mu.RLock()
	peers := make([]*peerState, 0, len(m.peers))
	for _, ps := range m.peers {
		peers = append(peers, ps)
	}
	m.mu.RUnlock()

	for _, ps := range peers {
		ps.mu.Lock()
		if ps.discovered && !ps.peer.LastSeen.IsZero() && now.Sub(ps.peer.LastSeen) > m.opts.PeerStaleAfter {
			ps.discovered = false
			ps.peer.Host = ""
			ps.peer.Port = 0
			ps.peer.Addresses = nil
			ps.wsPath = ""
			ps.proto = ""
		}
		ps.mu.Unlock()
	}
}

func (m *Manager) handleServiceEntry(entry *zeroconf.ServiceEntry) {
	peer, meta, err := peerFromEntry(entry)
	if err != nil || peer.ID == m.nodeID {
		return
	}

	ps := m.getOrCreatePeer(peer.ID)
	ps.mu.Lock()
	ps.peer.ID = peer.ID
	ps.peer.Name = peer.Name
	ps.peer.Host = peer.Host
	ps.peer.Port = peer.Port
	ps.peer.Addresses = copyIPs(peer.Addresses)
	ps.peer.LastSeen = peer.LastSeen
	ps.discovered = true
	ps.wsPath = meta.wsPath
	ps.proto = meta.proto
	ps.mu.Unlock()

	if shouldDial(m.nodeID, peer.ID) {
		m.ensureDialLoop(ps)
		ps.triggerReconnect()
	}
}

func (m *Manager) dialLoop(ps *peerState) {
	defer m.wg.Done()

	for {
		select {
		case <-m.ctx.Done():
			return
		case <-ps.reconnectCh:
		}

		for {
			peer, wsPath, shouldConnect, delay := m.prepareDial(ps)
			if !shouldConnect {
				break
			}

			if err := m.dialPeer(ps, peer, wsPath); err == nil {
				ps.resetBackoff()
				break
			}

			timer := time.NewTimer(withJitter(delay))
			select {
			case <-m.ctx.Done():
				timer.Stop()
				return
			case <-ps.reconnectCh:
				timer.Stop()
				continue
			case <-timer.C:
			}
		}
	}
}

func (m *Manager) prepareDial(ps *peerState) (Peer, string, bool, time.Duration) {
	ps.mu.Lock()
	defer ps.mu.Unlock()

	if ps.conn != nil || !ps.discovered || !shouldDial(m.nodeID, ps.id) {
		return Peer{}, "", false, 0
	}
	if ps.peer.Port == 0 || len(ps.peer.Addresses) == 0 || ps.wsPath == "" || ps.proto != protocolVersion {
		return Peer{}, "", false, 0
	}

	return ps.peerCopyLocked(), ps.wsPath, true, ps.bumpBackoff(m.opts.ReconnectMinBackoff, m.opts.ReconnectMaxBackoff)
}

func (m *Manager) instanceName() string {
	shortID := m.nodeID
	if len(shortID) > 12 {
		shortID = shortID[:12]
	}
	return fmt.Sprintf("%s-%s", defaultInstanceNamePrefix, shortID)
}

func (m *Manager) listenerPort() int {
	addr, ok := m.listener.Addr().(*net.TCPAddr)
	if !ok {
		return m.opts.Port
	}
	return addr.Port
}

func discoveryText(nodeID, name, wsPath string) []string {
	return []string{
		"id=" + nodeID,
		"name=" + name,
		"ws_path=" + wsPath,
		"proto=" + protocolVersion,
	}
}

func peerFromEntry(entry *zeroconf.ServiceEntry) (Peer, serviceMetadata, error) {
	if entry == nil {
		return Peer{}, serviceMetadata{}, errors.New("nil service entry")
	}

	meta, err := parseTXT(entry.Text)
	if err != nil {
		return Peer{}, serviceMetadata{}, err
	}
	if entry.Port == 0 {
		return Peer{}, serviceMetadata{}, errors.New("service entry missing port")
	}

	addresses := normalizeIPs(entry.AddrIPv4, entry.AddrIPv6)
	if len(addresses) == 0 {
		return Peer{}, serviceMetadata{}, errors.New("service entry missing addresses")
	}

	host := strings.TrimSuffix(entry.HostName, ".")
	if host == "" {
		host = entry.Instance
	}

	return Peer{
		ID:        meta.id,
		Name:      meta.name,
		Host:      host,
		Port:      entry.Port,
		Addresses: addresses,
		LastSeen:  time.Now().UTC(),
	}, meta, nil
}

func parseTXT(text []string) (serviceMetadata, error) {
	values := make(map[string]string, len(text))
	for _, entry := range text {
		key, value, ok := strings.Cut(entry, "=")
		if !ok || key == "" {
			return serviceMetadata{}, fmt.Errorf("invalid txt entry %q", entry)
		}
		values[key] = value
	}

	meta := serviceMetadata{
		id:     strings.TrimSpace(values["id"]),
		name:   strings.TrimSpace(values["name"]),
		wsPath: strings.TrimSpace(values["ws_path"]),
		proto:  strings.TrimSpace(values["proto"]),
	}

	if meta.id == "" {
		return serviceMetadata{}, errors.New("service entry missing id")
	}
	if meta.name == "" {
		return serviceMetadata{}, errors.New("service entry missing name")
	}
	if meta.wsPath == "" || !strings.HasPrefix(meta.wsPath, "/") {
		return serviceMetadata{}, errors.New("service entry missing websocket path")
	}
	if meta.proto != protocolVersion {
		return serviceMetadata{}, fmt.Errorf("unsupported protocol %q", meta.proto)
	}

	return meta, nil
}

func normalizeIPs(groups ...[]net.IP) []net.IP {
	seen := make(map[string]struct{})
	ips := make([]net.IP, 0)

	for _, group := range groups {
		for _, ip := range group {
			if ip == nil || ip.IsUnspecified() {
				continue
			}
			key := ip.String()
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			ips = append(ips, append(net.IP(nil), ip...))
		}
	}

	sort.Slice(ips, func(i, j int) bool {
		i4 := ips[i].To4() != nil
		j4 := ips[j].To4() != nil
		if i4 != j4 {
			return i4
		}
		return ips[i].String() < ips[j].String()
	})

	return ips
}

func copyIPs(src []net.IP) []net.IP {
	if len(src) == 0 {
		return nil
	}

	dst := make([]net.IP, len(src))
	for i, ip := range src {
		dst[i] = append(net.IP(nil), ip...)
	}
	return dst
}

func withJitter(base time.Duration) time.Duration {
	if base <= 0 {
		return 0
	}
	half := base / 2
	if half == 0 {
		return base
	}
	return half + time.Duration(rand.Int63n(int64(half)))
}

func sleepContext(ctx context.Context, delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}
