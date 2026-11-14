package destiny

import "sync"

// SystemManager is the process-wide registry of active solar systems.
// Created once in main() and passed to all connection handlers.
type SystemManager struct {
	mu      sync.RWMutex
	systems map[int64]*SolarSystem
}

// NewSystemManager creates an empty system manager.
func NewSystemManager() *SystemManager {
	return &SystemManager{
		systems: make(map[int64]*SolarSystem),
	}
}

// GetOrCreate returns the SolarSystem for sysID, creating it if it doesn't
// exist. The second return value is true if the system was newly created
// (caller should load celestials).
func (sm *SystemManager) GetOrCreate(sysID int64) (*SolarSystem, bool) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if ss, ok := sm.systems[sysID]; ok {
		return ss, false
	}
	ss := NewSolarSystem(sysID)
	sm.systems[sysID] = ss
	return ss, true
}

// Get returns the SolarSystem for sysID, or nil if not loaded.
func (sm *SystemManager) Get(sysID int64) *SolarSystem {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	return sm.systems[sysID]
}

// All returns every currently loaded solar system.
func (sm *SystemManager) All() []*SolarSystem {
	sm.mu.RLock()
	defer sm.mu.RUnlock()
	out := make([]*SolarSystem, 0, len(sm.systems))
	for _, ss := range sm.systems {
		out = append(out, ss)
	}
	return out
}

// Remove deletes a solar system from the manager (e.g. when all players leave).
func (sm *SystemManager) Remove(sysID int64) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	delete(sm.systems, sysID)
}

// RemoveIfEmpty tears down ss if it holds no player entities, atomically with
// concurrent AddEntity: the emptiness check, the dead flag and the map removal
// happen under both locks, so a player either lands before the check or AddEntity
// fails and the caller gets a fresh instance from GetOrCreate. True if torn down.
func (sm *SystemManager) RemoveIfEmpty(ss *SolarSystem) bool {
	if ss == nil {
		return false
	}
	sm.mu.Lock()
	ss.mu.Lock()
	empty := ss.playerCountLocked() == 0 && !ss.dead
	if empty {
		ss.dead = true
		// Only unlink if the map still holds this instance; a fresh system with the
		// same ID may already have replaced a previously dead one.
		if cur, ok := sm.systems[ss.ID]; ok && cur == ss {
			delete(sm.systems, ss.ID)
		}
	}
	ss.mu.Unlock()
	sm.mu.Unlock()
	if empty {
		ss.StopTickers()
	}
	return empty
}
