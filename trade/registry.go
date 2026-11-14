// Package trade holds in-memory state for player-to-player trade sessions.
//
// A session is keyed by its trade container: an entity row both traders move
// items into.  Ownership stays with the original trader until the trade
// completes, so an aborted session just moves everything back.
package trade

import (
	"fmt"
	"sync"
)

// Session is one open trade between exactly two characters.
type Session struct {
	ContainerID int64
	StationID   int64
	Traders     [2]int64
	Money       [2]float64
	Accepted    [2]bool
	CEOTrade    bool
}

// Index returns the slot (0 or 1) a character occupies, or -1 if they are not
// party to this trade.
func (s *Session) Index(charID int64) int {
	for i, id := range s.Traders {
		if id == charID {
			return i
		}
	}
	return -1
}

// Other returns the character ID on the far side of the trade.
func (s *Session) Other(charID int64) int64 {
	switch {
	case s.Traders[0] == charID:
		return s.Traders[1]
	case s.Traders[1] == charID:
		return s.Traders[0]
	}
	return 0
}

// BothAccepted reports whether the trade is ready to be settled.
func (s *Session) BothAccepted() bool { return s.Accepted[0] && s.Accepted[1] }

// Registry tracks the open trade sessions on this node.
type Registry struct {
	mu       sync.RWMutex
	sessions map[int64]*Session // containerID → session
	byChar   map[int64]int64    // charID → containerID
}

// NewRegistry creates an empty trade registry.
func NewRegistry() *Registry {
	return &Registry{
		sessions: make(map[int64]*Session),
		byChar:   make(map[int64]int64),
	}
}

// Open registers a new session against an already-created container item.
// Fails if either character is already trading.
func (r *Registry) Open(containerID, stationID, charA, charB int64, ceoTrade bool) (*Session, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if id, ok := r.byChar[charA]; ok {
		return nil, fmt.Errorf("character %d is already trading (container %d)", charA, id)
	}
	if id, ok := r.byChar[charB]; ok {
		return nil, fmt.Errorf("character %d is already trading (container %d)", charB, id)
	}

	s := &Session{
		ContainerID: containerID,
		StationID:   stationID,
		Traders:     [2]int64{charA, charB},
		CEOTrade:    ceoTrade,
	}
	r.sessions[containerID] = s
	r.byChar[charA] = containerID
	r.byChar[charB] = containerID
	return s.clone(), nil
}

// Get returns a snapshot of the session for a container, or nil.
func (r *Registry) Get(containerID int64) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if s, ok := r.sessions[containerID]; ok {
		return s.clone()
	}
	return nil
}

// GetByChar returns a snapshot of the session a character is in, or nil.
func (r *Registry) GetByChar(charID int64) *Session {
	r.mu.RLock()
	defer r.mu.RUnlock()
	if id, ok := r.byChar[charID]; ok {
		if s, ok := r.sessions[id]; ok {
			return s.clone()
		}
	}
	return nil
}

// IsTradeContainer reports whether an item ID is an open trade container.
func (r *Registry) IsTradeContainer(containerID int64) bool {
	r.mu.RLock()
	defer r.mu.RUnlock()
	_, ok := r.sessions[containerID]
	return ok
}

// SetMoney records a character's ISK offer.  Changing an offer clears both
// accept flags, matching the client's expectation that any change to the terms
// invalidates agreement.  Returns the updated snapshot.
func (r *Registry) SetMoney(containerID, charID int64, amount float64) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[containerID]
	if !ok {
		return nil
	}
	i := s.Index(charID)
	if i < 0 {
		return nil
	}
	s.Money[i] = amount
	s.Accepted = [2]bool{false, false}
	return s.clone()
}

// SetAccepted records one side's accept toggle and returns the snapshot.
func (r *Registry) SetAccepted(containerID, charID int64, accepted bool) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[containerID]
	if !ok {
		return nil
	}
	i := s.Index(charID)
	if i < 0 {
		return nil
	}
	s.Accepted[i] = accepted
	return s.clone()
}

// ClearAccepts drops both accept flags; used when the item contents change.
func (r *Registry) ClearAccepts(containerID int64) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[containerID]
	if !ok {
		return nil
	}
	s.Accepted = [2]bool{false, false}
	return s.clone()
}

// Close removes a session and returns its final snapshot, or nil if it was
// already gone.  Safe to call twice, so both the abort and complete paths can
// use it without racing.
func (r *Registry) Close(containerID int64) *Session {
	r.mu.Lock()
	defer r.mu.Unlock()

	s, ok := r.sessions[containerID]
	if !ok {
		return nil
	}
	delete(r.sessions, containerID)
	for _, charID := range s.Traders {
		if r.byChar[charID] == containerID {
			delete(r.byChar, charID)
		}
	}
	return s.clone()
}

func (s *Session) clone() *Session {
	cp := *s
	return &cp
}
