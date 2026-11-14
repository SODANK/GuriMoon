// Package infocache memoizes the per-entity identity lookups the client makes once
// per ball on grid: config GetMultiOwnersEx (one id), charmgr GetPublicInfo and
// GetPublicInfo3, character GetCharacterAppearanceList, corporationSvc
// GetEmploymentRecord, standing2 GetSecurityRating and bound GetInfoWindowDataForChar.
// Each is handled serially on the per-connection goroutine, so an uncached grid of
// pilots is a long run of sequential queries. Player characters stay out of
// config.BulkData.owners (see cache/catalog.go); this memo serves the repeated
// per-ID lookups instead.
package infocache

import (
	"sync"
	"time"

	"gurimoon/marshal"
)

// Default TTLs. Short enough that a missed invalidation costs seconds of stale name,
// long enough that a grid of pilots is one query per pilot.
const (
	// TTLStatic covers what barely changes in a session: a pilot's face and employment history.
	TTLStatic = 5 * time.Minute
	// TTLIdentity covers what can change while watched: corporation, title, security status.
	TTLIdentity = 60 * time.Second
)

type key struct {
	method string
	id     int64
}

type entry struct {
	rep marshal.PyRep
	at  time.Time
	// ready is closed when rep is populated. A concurrent second caller waits on it
	// instead of starting its own query.
	ready chan struct{}
	err   error
}

// Memo is a TTL cache of marshalled RPC replies keyed by (method, id). Stored values
// are immutable, so sharing one across connections is safe: SendCallRsp re-marshals
// it with each call's own packet header.
type Memo struct {
	mu      sync.Mutex
	entries map[key]*entry
}

// New returns an empty Memo.
func New() *Memo { return &Memo{entries: make(map[key]*entry)} }

// Get returns the memoized reply for (method, id), calling gen on a miss or an
// expired entry. Concurrent callers for one key share one gen call. A failed gen is
// not cached: every waiter gets the error and the next call retries.
func (m *Memo) Get(method string, id int64, ttl time.Duration, gen func() (marshal.PyRep, error)) (marshal.PyRep, error) {
	if m == nil {
		return gen()
	}
	k := key{method: method, id: id}

	m.mu.Lock()
	if e, ok := m.entries[k]; ok {
		select {
		case <-e.ready:
			// Populated: serve it if fresh, otherwise replace it.
			if e.err == nil && time.Since(e.at) < ttl {
				m.mu.Unlock()
				return e.rep, nil
			}
		default:
			// A gen for this key is in flight: wait for it.
			m.mu.Unlock()
			<-e.ready
			return e.rep, e.err
		}
	}
	e := &entry{ready: make(chan struct{})}
	m.entries[k] = e
	m.mu.Unlock()

	rep, err := gen()

	m.mu.Lock()
	e.rep, e.err, e.at = rep, err, time.Now()
	if err != nil {
		// Do not leave a failed entry for the next caller to serve or wait on.
		if m.entries[k] == e {
			delete(m.entries, k)
		}
	}
	m.mu.Unlock()
	close(e.ready)
	return rep, err
}

// InvalidateID drops every memoized method for one owner. Call it wherever the server
// writes something an info window shows (rename, corporation change, security hit);
// the TTL is the backstop for missed hooks.
func (m *Memo) InvalidateID(id int64) {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, e := range m.entries {
		if k.id != id {
			continue
		}
		// An in-flight gen is left alone: deleting its entry would strand the goroutines
		// waiting on its ready channel.
		select {
		case <-e.ready:
			delete(m.entries, k)
		default:
		}
	}
}

// Clear empties the whole memo (bulk data reload, roster wipe).
func (m *Memo) Clear() {
	if m == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.entries = make(map[key]*entry)
}

// Sweep drops entries older than maxAge. Get never evicts, so an ID nobody asks about
// again would be held forever. Register it on the tick manager.
func (m *Memo) Sweep(maxAge time.Duration) int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for k, e := range m.entries {
		select {
		case <-e.ready:
			if time.Since(e.at) > maxAge {
				delete(m.entries, k)
				n++
			}
		default:
		}
	}
	return n
}

// Len reports the number of memoized entries (diagnostics).
func (m *Memo) Len() int {
	if m == nil {
		return 0
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.entries)
}

// Shared is the process-wide memo. The identity RPCs are read-only lookups of
// globally visible data, so one instance serves every connection.
var Shared = New()
