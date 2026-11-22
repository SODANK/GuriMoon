package destiny

import (
	"log"
	"sync"
	"time"
)

// Undock lane occupancy: one registry for every ship that leaves a door,
// shared by the server and the rolebots module. A holder (the ship) has at
// most one seat anywhere; claiming at another object releases the old seat,
// which is what frees a station's door. See docs/undock-geometry.md#occupancy.
const (
	// Slots an object hands out before a claim is overflow: the lane's injective
	// period, below which UndockLaneOffset places every slot on its own coordinate.
	UndockLaneMaxSlots = UndockLaneSlotsPerLayer * UndockLaneLayerSpan

	// Default hold for a claimant that names none. Long enough for a ship under
	// thrust to clear a lane step.
	UndockLaneHold = 30 * time.Second

	// How often the registry sweeps expired holds.
	undockLaneSweepEvery = time.Second

	// How often one saturated object logs that it is full.
	undockLaneWarnEvery = time.Minute
)

// StandoffLaneObject is the lane key for ships waiting at a station, kept apart
// from the key for ships leaving it: the two point sets are at least 15 km
// apart, so sharing one band only spent seats. Station IDs are positive, so
// the negation collides with no other object.
func StandoffLaneObject(stationID int64) int64 { return -stationID }

type undockLaneKey struct {
	objectID int64
	slot     int
}

type undockLaneHolder struct {
	holder int64
	until  time.Time
}

var (
	undockLaneMu sync.Mutex
	// slot -> who is in it.
	undockLaneSlots = map[undockLaneKey]undockLaneHolder{}
	// holder -> the one slot it holds.
	undockLaneHeld  = map[int64]undockLaneKey{}
	undockLaneSwept time.Time
	// Last time each object logged that it was full.
	undockLaneWarned = map[int64]time.Time{}
)

// ClaimUndockSlot returns a free lane slot at objectID, held for hold
// (UndockLaneHold when <= 0). holder is the claiming ship and must be the same
// identity at every call site. A holder already seated at this object keeps its
// slot; one seated at another object releases that seat first.
func ClaimUndockSlot(objectID, holder int64, hold time.Duration) int {
	return ClaimUndockSlotWhere(objectID, holder, hold, nil)
}

// undockSameRun reports whether two band slots share a run line: the same layer
// and column, any row. A launcher at slot k flies through slot k+3's exit point a
// row ahead, so two live holders on one line are seated to collide.
func undockSameRun(a, b int) bool {
	if a < 0 || b < 0 || a >= UndockLaneMaxSlots || b >= UndockLaneMaxSlots {
		return false
	}
	la, lb := a/UndockLaneSlotsPerLayer, b/UndockLaneSlotsPerLayer
	ca := (a % UndockLaneSlotsPerLayer) % UndockLaneColumns
	cb := (b % UndockLaneSlotsPerLayer) % UndockLaneColumns
	return la == lb && ca == cb
}

// ClaimUndockSlotWhere is ClaimUndockSlot with a veto. fits (nil: accept every
// slot) is asked about each candidate; it runs under the registry lock, so it must
// be pure, and callers snapshot whatever occupancy it reads first. The walk:
//
//  1. the first free seat whose run line no other live holder at this object is
//     on, and that fits accepts;
//  2. the first free seat fits accepts;
//  3. the first free seat (fits could not be satisfied; better a seat than none).
//
// A holder already seated here keeps its seat only while fits accepts it.
func ClaimUndockSlotWhere(objectID, holder int64, hold time.Duration, fits func(slot int) bool) int {
	if hold <= 0 {
		hold = UndockLaneHold
	}
	now := time.Now()
	until := now.Add(hold)

	undockLaneMu.Lock()
	defer undockLaneMu.Unlock()

	pruneUndockLaneLocked(now)

	if k, ok := undockLaneHeld[holder]; ok {
		if k.objectID == objectID && (fits == nil || fits(k.slot)) {
			undockLaneSlots[k] = undockLaneHolder{holder, until}
			return k.slot
		}
		// The ship moved, or its seat no longer fits; release the seat it left behind.
		delete(undockLaneSlots, k)
		delete(undockLaneHeld, holder)
	}

	// Live seats at this object. Expiry is decided here as well as by the amortised
	// sweep, so a freed seat is reusable at once.
	live := map[int]bool{}
	for k, h := range undockLaneSlots {
		if k.objectID != objectID {
			continue
		}
		if now.After(h.until) {
			delete(undockLaneSlots, k)
			if held, ok := undockLaneHeld[h.holder]; ok && held == k {
				delete(undockLaneHeld, h.holder)
			}
			continue
		}
		live[k.slot] = true
	}
	runTaken := func(slot int) bool {
		for s := range live {
			if undockSameRun(s, slot) {
				return true
			}
		}
		return false
	}
	take := func(slot int) int {
		k := undockLaneKey{objectID, slot}
		undockLaneSlots[k] = undockLaneHolder{holder, until}
		undockLaneHeld[holder] = k
		return slot
	}
	for pass := 0; pass < 3; pass++ {
		for slot := 0; slot < UndockLaneMaxSlots; slot++ {
			if live[slot] {
				continue
			}
			if pass == 0 && runTaken(slot) {
				continue
			}
			if pass < 2 && fits != nil && !fits(slot) {
				continue
			}
			return take(slot)
		}
		if fits == nil && pass == 0 {
			// Pass 2 would repeat pass 3 exactly; skip to it.
			pass = 1
		}
	}

	// Band full. Hand out an overflow corridor slot, bucketed by holder and not
	// recorded: an overflow slot is not a reservation.
	if last, ok := undockLaneWarned[objectID]; !ok || now.Sub(last) >= undockLaneWarnEvery {
		undockLaneWarned[objectID] = now
		log.Printf("destiny: undock lane at object %d is full (%d seats) — ships are being placed in the overflow corridor",
			objectID, UndockLaneMaxSlots)
	}
	return UndockLaneMaxSlots + int(uint64(holder)%UndockLaneOverflowSlots)
}

// pruneUndockLaneLocked drops expired holds. Amortised to one walk per
// undockLaneSweepEvery; callers hold undockLaneMu.
func pruneUndockLaneLocked(now time.Time) {
	if now.Sub(undockLaneSwept) < undockLaneSweepEvery {
		return
	}
	undockLaneSwept = now
	for k, h := range undockLaneSlots {
		if now.After(h.until) {
			delete(undockLaneSlots, k)
			if held, ok := undockLaneHeld[h.holder]; ok && held == k {
				delete(undockLaneHeld, h.holder)
			}
		}
	}
}

// ReleaseUndockSlot hands a slot back early, for a ship that docked again or
// lost its hull before the run was over. Safe to call for a holder that has no
// slot, or one whose slot is at some other object.
func ReleaseUndockSlot(objectID, holder int64) {
	undockLaneMu.Lock()
	defer undockLaneMu.Unlock()
	if k, ok := undockLaneHeld[holder]; ok && k.objectID == objectID {
		delete(undockLaneSlots, k)
		delete(undockLaneHeld, holder)
	}
}

// ReleaseHeldSlot drops whatever seat holder sits in, at any object. A docking
// ship may hold either the door seat or the standoff seat, so it asks the
// registry rather than naming one.
func ReleaseHeldSlot(holder int64) {
	undockLaneMu.Lock()
	defer undockLaneMu.Unlock()
	if k, ok := undockLaneHeld[holder]; ok {
		delete(undockLaneSlots, k)
		delete(undockLaneHeld, holder)
	}
}

// UndockLaneOccupancy reports taken seats and capacity of objectID's band.
// Diagnostic only.
func UndockLaneOccupancy(objectID int64) (taken, capacity int) {
	now := time.Now()
	undockLaneMu.Lock()
	defer undockLaneMu.Unlock()
	pruneUndockLaneLocked(now)
	for k := range undockLaneSlots {
		if k.objectID == objectID {
			taken++
		}
	}
	return taken, UndockLaneMaxSlots
}

// ClearUndockSlots drops every claim at every object. For tests; nothing in
// the server calls it.
func ClearUndockSlots() {
	undockLaneMu.Lock()
	undockLaneSlots = map[undockLaneKey]undockLaneHolder{}
	undockLaneHeld = map[int64]undockLaneKey{}
	undockLaneWarned = map[int64]time.Time{}
	undockLaneSwept = time.Time{}
	undockLaneMu.Unlock()
}
