package main

import (
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Every entity that leaves a system drops out of the lock lists of the pilots still there.
// The client never sends RemoveTarget for a removed ball, so a stale entry answers the next
// AddTarget with "already targeting" (no lock timer, no lock animation) and holds a slot
// against the max-target cap. See docs/main-s5-notes.md#target-locks.
func init() {
	destiny.EntityRemoved = dropLocksOnEntity
}

// dropLocksOnEntity cancels every pending lock on entityID in ss and removes it from every
// pilot's target list, sending OnTarget "lost" to each pilot that held it locked.
func dropLocksOnEntity(ss *destiny.SolarSystem, entityID uint32) {
	for _, p := range ss.GetAllPlayersInSystem() {
		pds, ok := p.PlayerState.(*destinyConn)
		if !ok || pds == nil {
			continue
		}
		if dropLockOn(pds, entityID) && p.Conn != nil {
			sendTargetLost(p.Conn, int64(p.AccountID), entityID)
		}
	}
}

// dropLockOn removes tid from one pilot's pending and completed locks. It reports whether
// tid was a completed lock.
func dropLockOn(pds *destinyConn, tid uint32) bool {
	pds.targetMu.Lock()
	defer pds.targetMu.Unlock()
	if cancelFn, ok := pds.lockingTargets[tid]; ok {
		cancelFn()
		delete(pds.lockingTargets, tid)
	}
	wasLocked := false
	filtered := pds.targets[:0]
	for _, t := range pds.targets {
		if t == tid {
			wasLocked = true
		} else {
			filtered = append(filtered, t)
		}
	}
	pds.targets = filtered
	return wasLocked
}

// playerLockersOf lists the ships of the pilots in ss that hold a completed lock on entityID.
func playerLockersOf(ss *destiny.SolarSystem, entityID uint32) []uint32 {
	var out []uint32
	for _, p := range ss.GetAllPlayersInSystem() {
		pds, ok := p.PlayerState.(*destinyConn)
		if !ok || pds == nil || p.ID == entityID {
			continue
		}
		pds.targetMu.Lock()
		for _, t := range pds.targets {
			if t == entityID {
				out = append(out, p.ID)
				break
			}
		}
		pds.targetMu.Unlock()
	}
	return out
}

// lockersOf lists every ball holding a lock on the pilot's ship: NPCs, then other pilots.
func lockersOf(ds *destinyConn) []uint32 {
	if ds == nil || ds.entity == nil {
		return nil
	}
	var out []uint32
	if globalNPCMgr != nil {
		out = globalNPCMgr.GetTargetersOf(ds.entity.ID)
	}
	if ds.solarSystem != nil {
		out = append(out, playerLockersOf(ds.solarSystem, ds.entity.ID)...)
	}
	return out
}

// ballIDList is a list of ball IDs as the PyList the targeting calls answer with.
func ballIDList(ids []uint32) marshal.PyList {
	items := make([]marshal.PyRep, len(ids))
	for i, id := range ids {
		items[i] = marshal.PyInt{Value: uint64(id)}
	}
	return marshal.PyList{Items: items}
}

// lockedTargetList is the pilot's completed locks as a PyList.
func lockedTargetList(ds *destinyConn) marshal.PyList {
	if ds == nil {
		return marshal.PyList{}
	}
	ds.targetMu.Lock()
	defer ds.targetMu.Unlock()
	return ballIDList(ds.targets)
}

// sendTargetLost tells a pilot's client that its lock on tid is gone.
func sendTargetLost(c *evenet.Conn, accountID int64, tid uint32) {
	_ = server.SendNotification(c, accountID, "OnTarget", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "lost"},
		marshal.PyInt{Value: uint64(tid)},
		marshal.PyNone{},
	}})
}
