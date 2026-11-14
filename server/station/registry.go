package station

import (
	"sync"

	evenet "gurimoon/net"
)

// StationRegistry tracks character presence at stations.
// It is safe for concurrent use by multiple goroutines.
type StationRegistry struct {
	mu sync.RWMutex

	// stations maps stationID → slice of character IDs at that station.
	stations map[int64][]int64

	// connStation maps conn → stationID for quick removal on disconnect.
	connStation map[*evenet.Conn]int64

	// connCharID maps conn → charID for quick lookup on disconnect.
	connCharID map[*evenet.Conn]int64

	// botStations maps charID → stationID for synthetic (no-Conn) bot guests.
	botStations map[int64]int64

	// presenceNotify, when set, is called after the registry mutation and outside the
	// lock for every real guest-list change: notify(stationID, charID, arrived). Set by
	// main.go to push OnCharNowInStation / OnCharNoLongerInStation to the other guests.
	presenceNotify func(stationID, charID int64, arrived bool)
}

// presenceEvent is a deferred presenceNotify call collected under the lock.
type presenceEvent struct {
	stationID int64
	charID    int64
	arrived   bool
}

// SetPresenceNotify installs the guest-change callback. Call once at startup
// before connections are accepted.
func (r *StationRegistry) SetPresenceNotify(fn func(stationID, charID int64, arrived bool)) {
	r.mu.Lock()
	r.presenceNotify = fn
	r.mu.Unlock()
}

// firePresence invokes the callback for each collected event, outside the lock.
func (r *StationRegistry) firePresence(events []presenceEvent) {
	r.mu.RLock()
	fn := r.presenceNotify
	r.mu.RUnlock()
	if fn == nil {
		return
	}
	for _, ev := range events {
		fn(ev.stationID, ev.charID, ev.arrived)
	}
}

// NewStationRegistry creates an empty StationRegistry.
func NewStationRegistry() *StationRegistry {
	return &StationRegistry{
		stations:    make(map[int64][]int64),
		connStation: make(map[*evenet.Conn]int64),
		connCharID:  make(map[*evenet.Conn]int64),
		botStations: make(map[int64]int64),
	}
}

// Join adds a character to a station. Idempotent. It returns whether the character
// newly arrived (false when already present) and the station they left (0 if none),
// which the caller uses to push OnCharNowInStation/OnCharNoLongerInStation.
func (r *StationRegistry) Join(conn *evenet.Conn, stationID, charID int64) (joined bool, leftStationID int64) {
	var events []presenceEvent
	r.mu.Lock()

	// Remove from old station if character was previously at a different one.
	if oldStationID, exists := r.connStation[conn]; exists && oldStationID != stationID {
		r.leaveStation(oldStationID, charID)
		leftStationID = oldStationID
		events = append(events, presenceEvent{oldStationID, charID, false})
	}

	// Check if already a member of this station.
	already := false
	for _, existing := range r.stations[stationID] {
		if existing == charID {
			already = true
			break
		}
	}
	if !already {
		r.stations[stationID] = append(r.stations[stationID], charID)
		joined = true
		events = append(events, presenceEvent{stationID, charID, true})
	}
	r.connStation[conn] = stationID
	r.connCharID[conn] = charID
	r.mu.Unlock()

	r.firePresence(events)
	return joined, leftStationID
}

// Remove removes a connection from all stations (undock or disconnect).
// Returns the station and character that left, or (0, 0) when the connection
// was not a guest anywhere.
func (r *StationRegistry) Remove(conn *evenet.Conn) (stationID, charID int64) {
	var events []presenceEvent
	r.mu.Lock()

	sid, hasStation := r.connStation[conn]
	cid, hasCharID := r.connCharID[conn]

	if hasStation && hasCharID {
		r.leaveStation(sid, cid)
		stationID, charID = sid, cid
		events = append(events, presenceEvent{sid, cid, false})
	}

	delete(r.connStation, conn)
	delete(r.connCharID, conn)
	r.mu.Unlock()

	r.firePresence(events)
	return stationID, charID
}

// JoinBot adds a synthetic (no-Conn) bot guest at a station. Idempotent; a
// bot can only be at one station, so joining moves it.
func (r *StationRegistry) JoinBot(stationID, charID int64) {
	var events []presenceEvent
	r.mu.Lock()
	if old, ok := r.botStations[charID]; ok && old != stationID {
		events = append(events, presenceEvent{old, charID, false})
	}
	if r.botStations[charID] != stationID {
		events = append(events, presenceEvent{stationID, charID, true})
	}
	r.botStations[charID] = stationID
	r.mu.Unlock()
	r.firePresence(events)
}

// LeaveBot removes a synthetic bot guest from whatever station it was at.
func (r *StationRegistry) LeaveBot(charID int64) {
	var events []presenceEvent
	r.mu.Lock()
	if old, ok := r.botStations[charID]; ok {
		events = append(events, presenceEvent{old, charID, false})
	}
	delete(r.botStations, charID)
	r.mu.Unlock()
	r.firePresence(events)
}

// Guests returns a snapshot of character IDs currently at a station (may be empty).
func (r *StationRegistry) Guests(stationID int64) []int64 {
	r.mu.RLock()
	defer r.mu.RUnlock()

	charIDs := r.stations[stationID]
	result := make([]int64, len(charIDs))
	copy(result, charIDs)
	for charID, sid := range r.botStations {
		if sid == stationID {
			result = append(result, charID)
		}
	}
	return result
}

// leaveStation is the internal (lock-held) helper.
func (r *StationRegistry) leaveStation(stationID, charID int64) {
	charIDs := r.stations[stationID]
	for i, id := range charIDs {
		if id == charID {
			r.stations[stationID] = append(charIDs[:i], charIDs[i+1:]...)
			break
		}
	}
	if len(r.stations[stationID]) == 0 {
		delete(r.stations, stationID)
	}
}
