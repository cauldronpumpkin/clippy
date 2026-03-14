package osclip

import (
	"sync"
	"time"
)

type pendingKey struct {
	bucket formatBucket
	hash   [32]byte
}

type pendingEntry struct {
	count     int
	expiresAt time.Time
}

// pendingLedger tracks recently injected clipboard hashes so watcher callbacks
// can suppress loopback broadcasts.
type pendingLedger struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[pendingKey]pendingEntry
}

func newPendingLedger(ttl time.Duration) *pendingLedger {
	return &pendingLedger{
		ttl:     ttl,
		entries: make(map[pendingKey]pendingEntry),
	}
}

func (l *pendingLedger) remember(bucket formatBucket, hash [32]byte, now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(now)

	key := pendingKey{bucket: bucket, hash: hash}
	entry := l.entries[key]
	entry.count++
	entry.expiresAt = now.Add(l.ttl)
	l.entries[key] = entry
}

func (l *pendingLedger) consume(bucket formatBucket, hash [32]byte, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.pruneLocked(now)

	key := pendingKey{bucket: bucket, hash: hash}
	entry, ok := l.entries[key]
	if !ok {
		return false
	}

	entry.count--
	if entry.count <= 0 {
		delete(l.entries, key)
		return true
	}

	l.entries[key] = entry
	return true
}

func (l *pendingLedger) forget(bucket formatBucket, hash [32]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()

	key := pendingKey{bucket: bucket, hash: hash}
	entry, ok := l.entries[key]
	if !ok {
		return
	}

	entry.count--
	if entry.count <= 0 {
		delete(l.entries, key)
		return
	}

	l.entries[key] = entry
}

func (l *pendingLedger) pruneLocked(now time.Time) {
	for key, entry := range l.entries {
		if !entry.expiresAt.After(now) {
			delete(l.entries, key)
		}
	}
}
