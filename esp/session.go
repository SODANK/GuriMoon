// Package esp implements the EVE Server Pages (ESP) web admin panel.
package esp

import (
	"sync"
	"time"

	evenet "gurimoon/net"
)

// Session holds metadata about a connected EVE client.
type Session struct {
	AccountID     int64
	AccountName   string
	CharID        int64
	CharName      string
	CorpID        int64
	StationID     int64
	SolarSystemID int64
	Conn          *evenet.Conn
	Addr          string
	ConnectedAt   time.Time
	LastActivity  time.Time

	// Bot marks a synthetic session with no connection behind it (a module's pilots). It is in
	// Snapshot and the by-location lookups (presence) and absent from byCharID ("is this character
	// connected"). Conn is nil, so every send path skips it.
	Bot bool

	// Dungeon editor live-refresh support.
	DunRoomID      int64        // currently loaded dungeon room (0 = none)
	DungeonRefresh chan<- int64 // send roomID to respawn; nil until in space
}

// SessionRegistry tracks all active EVE client connections.
// It is safe for concurrent use.
type SessionRegistry struct {
	mu       sync.RWMutex
	sessions map[string]*Session // keyed by remote addr
	byCharID map[int64]*Session  // secondary index by charID
}

// NewSessionRegistry creates an empty registry.
func NewSessionRegistry() *SessionRegistry {
	return &SessionRegistry{
		sessions: make(map[string]*Session),
		byCharID: make(map[int64]*Session),
	}
}

// Register adds a session for the given remote address.
func (r *SessionRegistry) Register(addr string, s *Session) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sessions[addr] = s
	if s.CharID != 0 {
		r.byCharID[s.CharID] = s
	}
}

// Update applies fn to the session identified by addr (if it exists).
func (r *SessionRegistry) Update(addr string, fn func(*Session)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[addr]; ok {
		oldCharID := s.CharID
		fn(s)
		if s.CharID != oldCharID {
			if oldCharID != 0 {
				delete(r.byCharID, oldCharID)
			}
			if s.CharID != 0 {
				r.byCharID[s.CharID] = s
			}
		}
	}
}

// Unregister removes the session for addr.
func (r *SessionRegistry) Unregister(addr string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.sessions[addr]; ok {
		if s.CharID != 0 {
			delete(r.byCharID, s.CharID)
		}
		delete(r.sessions, addr)
	}
}

// Snapshot returns a copy of all current sessions.
func (r *SessionRegistry) Snapshot() []Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]Session, 0, len(r.sessions))
	for _, s := range r.sessions {
		out = append(out, *s)
	}
	return out
}

// FindByCharID returns the session for a character, or nil if not found.
func (r *SessionRegistry) FindByCharID(charID int64) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.byCharID[charID]
}

// PresentCharIDs below and IsPresent see synthetic sessions; PresenceChangedHook, when set, is
// called as a synthetic session appears or disappears, because the socket-driven login and
// disconnect paths never announce those pilots. Set by main.
var PresenceChangedHook func(charID int64, online bool)

// IsPresent reports whether a character has a session of any kind, real or synthetic. Unlike
// FindByCharID it scans the whole session map, because module-driven pilots stay out of byCharID
// so that index keeps meaning "a real client is connected as this character".
func (r *SessionRegistry) IsPresent(charID int64) bool {
	if charID == 0 {
		return false
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.byCharID[charID]; ok {
		return true
	}
	for _, s := range r.sessions {
		if s.CharID == charID {
			return true
		}
	}
	return false
}

// PresentCharIDs returns every character present in the world, real or
// synthetic. One scan, for callers testing many characters at once.
func (r *SessionRegistry) PresentCharIDs() map[int64]struct{} {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[int64]struct{}, len(r.sessions))
	for _, s := range r.sessions {
		if s.CharID != 0 {
			out[s.CharID] = struct{}{}
		}
	}
	return out
}

// ClaimCharID atomically binds charID to the session at addr, refusing when a different session
// already holds the character. It is the duplicate-login gate: check-then-set through
// FindByCharID would let two simultaneous selections both pass.
func (r *SessionRegistry) ClaimCharID(addr string, charID int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	s, ok := r.sessions[addr]
	if !ok {
		// No authenticated session for this address: nothing to bind to.
		return false
	}
	if existing, taken := r.byCharID[charID]; taken && existing != s {
		return false
	}
	if s.CharID != 0 && s.CharID != charID {
		delete(r.byCharID, s.CharID)
	}
	s.CharID = charID
	r.byCharID[charID] = s
	return true
}

// Count returns the number of active sessions.
func (r *SessionRegistry) Count() int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return len(r.sessions)
}

// FindByStationID returns all sessions at the given station.
func (r *SessionRegistry) FindByStationID(stationID int64) []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Session
	for _, s := range r.sessions {
		if s.StationID == stationID {
			out = append(out, s)
		}
	}
	return out
}

// FindBySystemID returns all sessions in the given solar system.
func (r *SessionRegistry) FindBySystemID(systemID int64) []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Session
	for _, s := range r.sessions {
		if s.SolarSystemID == systemID {
			out = append(out, s)
		}
	}
	return out
}

// FindByCorpID returns all sessions belonging to the given corporation.
func (r *SessionRegistry) FindByCorpID(corpID int64) []*Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	var out []*Session
	for _, s := range r.sessions {
		if s.CorpID == corpID {
			out = append(out, s)
		}
	}
	return out
}

// Broadcast calls fn on every session matching filter. The callback must not
// call back into the registry (it holds a read lock).
func (r *SessionRegistry) Broadcast(filter func(*Session) bool, fn func(*Session)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		if filter(s) {
			fn(s)
		}
	}
}

// ForEach calls fn on every active session. The callback must not call back
// into the registry (it holds a read lock).
func (r *SessionRegistry) ForEach(fn func(*Session)) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	for _, s := range r.sessions {
		fn(s)
	}
}

// RefreshDungeonRoom signals all sessions currently viewing the given roomID
// to re-read and respawn the dungeon room from the database.  Returns the
// number of players notified.
func (r *SessionRegistry) RefreshDungeonRoom(roomID int64) int {
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, s := range r.sessions {
		if s.DunRoomID == roomID && s.DungeonRefresh != nil {
			select {
			case s.DungeonRefresh <- roomID:
				n++
			default:
				// channel full, skip
			}
		}
	}
	return n
}

// RefreshDungeonAll signals all sessions viewing any room in the given
// set of roomIDs.  Returns the number of players notified.
func (r *SessionRegistry) RefreshDungeonAll(roomIDs []int64) int {
	set := make(map[int64]struct{}, len(roomIDs))
	for _, id := range roomIDs {
		set[id] = struct{}{}
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	n := 0
	for _, s := range r.sessions {
		if _, ok := set[s.DunRoomID]; ok && s.DungeonRefresh != nil {
			select {
			case s.DungeonRefresh <- s.DunRoomID:
				n++
			default:
			}
		}
	}
	return n
}
