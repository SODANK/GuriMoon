package destiny

import "time"

// Asteroid field persistence. A field is a pure function of its seed, so only
// what has been mined out of it is carried across an unload. This package must
// not import db; the server converts these values to its own rows.

// AsteroidDepletionRecord is one rock whose quantity no longer matches the seed.
// Qty 0 is a mined-out rock and is a tombstone: ApplyAsteroidDepletion reads a
// missing entry as full, so dropping the row puts the rock back in the belt.
type AsteroidDepletionRecord struct {
	EntityID uint32
	BeltID   int64
	Qty      int64
}

// ApplyAsteroidDepletion overlays persisted quantities onto a freshly generated
// field, recomputing each radius from the surviving quantity and marking rocks at
// zero depleted so NearbyAsteroids and AsteroidsInRange keep hiding them. Entries
// for rocks the field does not contain (a belt removed by an edit) are ignored.
func (ss *SolarSystem) ApplyAsteroidDepletion(qtys map[uint32]int64) int {
	if len(qtys) == 0 {
		return 0
	}
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if ss.AsteroidDepletedAt == nil {
		ss.AsteroidDepletedAt = make(map[uint32]time.Time)
	}
	// One pass over the slices rather than a scan per entry: thousands of rows
	// against thousands of rocks otherwise.
	typeByID := make(map[uint32]int64, len(ss.AsteroidSlims))
	for i := range ss.AsteroidSlims {
		typeByID[uint32(ss.AsteroidSlims[i].ItemID)] = ss.AsteroidSlims[i].TypeID
	}
	applied := 0
	now := time.Now()
	for id, qty := range qtys {
		if _, ok := typeByID[id]; !ok {
			continue
		}
		if qty < 0 {
			qty = 0
		}
		if max := ss.AsteroidMaxQty[id]; max > 0 && qty > max {
			qty = max
		}
		ss.AsteroidQty[id] = qty
		if qty == 0 {
			ss.AsteroidDepletedAt[id] = now
		}
		applied++
	}
	for i := range ss.AsteroidBalls {
		id := ss.AsteroidBalls[i].EntityID
		if _, ok := qtys[id]; !ok {
			continue
		}
		ss.AsteroidBalls[i].Radius = radiusForType(typeByID[id], ss.AsteroidQty[id])
	}
	return applied
}

// ExportAsteroidDepletion returns every rock whose quantity differs from the
// seed's, for the flush at system unload: the dirty set only holds what moved
// since the last flush, and a field can be worked all evening with a clean set.
func (ss *SolarSystem) ExportAsteroidDepletion() []AsteroidDepletionRecord {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	var out []AsteroidDepletionRecord
	for id, qty := range ss.AsteroidQty {
		if max, ok := ss.AsteroidMaxQty[id]; ok && qty >= max {
			continue
		}
		out = append(out, AsteroidDepletionRecord{
			EntityID: id, BeltID: ss.AsteroidBelt[id], Qty: qty,
		})
	}
	return out
}

// markAsteroidDirtyLocked records a rock for the next flush. Caller holds mu.
func (ss *SolarSystem) markAsteroidDirtyLocked(entityID uint32) {
	if ss.asteroidDirty == nil {
		ss.asteroidDirty = make(map[uint32]struct{})
	}
	ss.asteroidDirty[entityID] = struct{}{}
}

// DrainDirtyAsteroids returns the rocks whose quantity has moved since the last
// call and clears the set. Returns nil when nothing changed, so an unmined system
// costs the flusher one lock.
func (ss *SolarSystem) DrainDirtyAsteroids() []AsteroidDepletionRecord {
	ss.mu.Lock()
	defer ss.mu.Unlock()
	if len(ss.asteroidDirty) == 0 {
		return nil
	}
	out := make([]AsteroidDepletionRecord, 0, len(ss.asteroidDirty))
	for id := range ss.asteroidDirty {
		out = append(out, AsteroidDepletionRecord{
			EntityID: id, BeltID: ss.AsteroidBelt[id], Qty: ss.AsteroidQty[id],
		})
	}
	ss.asteroidDirty = nil
	return out
}

// SetAsteroidRespawnDelay sets how long a mined-out rock stays gone. Zero (the
// shipped default) disables respawn; see the asteroidRespawn field.
func (ss *SolarSystem) SetAsteroidRespawnDelay(d time.Duration) {
	ss.mu.Lock()
	ss.asteroidRespawn = d
	ss.mu.Unlock()
}

// AsteroidRockCount reports how many rocks the field holds, depleted included.
func (ss *SolarSystem) AsteroidRockCount() int {
	ss.mu.RLock()
	defer ss.mu.RUnlock()
	return len(ss.AsteroidBalls)
}
