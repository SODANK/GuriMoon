package main

// Loot theft is aggression: the era can-flip rule. RMR has no suspect flag;
// taking from another pilot's container writes the taker as an aggressor of
// the owner and the owner's corporation in the crimewatch table for the
// 15-minute window, so the owner and their corpmates may shoot the thief
// (seclaw's VictimAggressedKiller rung). The rolebots module's hauler and
// looter write the same table through PvPHooks.NoteTheft.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/syncx"
)

// lootOwner is who a taken row belonged to, resolved to the two keys the
// crimewatch table is written with. A corporation-owned row (a kit hull's
// cargo) carries CharID 0; an NPC owner resolves to neither.
type lootOwner struct {
	CharID int64
	CorpID int64 // the owner's player corp; 0 for an NPC corp or none
}

// lootTakeIsTheft is the rule, pure: a take is theft when the row belonged to
// another player or player corporation outside the taker's own corp and fleet.
// Fleetmates are exempt so a mixed-corp mining fleet's hauler can empty every
// miner's can.
func lootTakeIsTheft(thiefCharID, thiefCorpID int64, owner lootOwner, sameGang bool) bool {
	if thiefCharID <= 0 {
		return false
	}
	if owner.CharID <= 0 && owner.CorpID <= 0 {
		return false
	}
	if owner.CharID == thiefCharID {
		return false
	}
	if thiefCorpID > 0 && owner.CorpID == thiefCorpID {
		return false
	}
	if sameGang {
		return false
	}
	return true
}

// Player corporation IDs live in this band (db: IsCorporation(id) is
// 1,000,000 <= id < 2,000,000); NPC corporations share it and are told apart
// by crpNPCCorporations.
const (
	corpIDBandLo = 1_000_000
	corpIDBandHi = 2_000_000
)

type lootOwnerEntry struct {
	owner lootOwner
	at    int64
}

// lootOwnerCache keeps a resolved owner for a minute: a pilot emptying a
// twenty-stack wreck asks about the same owner twenty times in a row.
var lootOwnerCache syncx.Map[int64, lootOwnerEntry] // ownerID to lootOwnerEntry

const lootOwnerCacheSec = 60

// resolveLootOwner turns an entity row's ownerID into the keys to aggress. A
// character resolves with its corp when that corp is a player corp; a player
// corporation resolves to itself; anything else (an NPC corp, an NPC
// character, the system) resolves to nothing, and nothing is theft.
func resolveLootOwner(ctx context.Context, sqldb *sql.DB, ownerID int64) lootOwner {
	if ownerID <= 0 || sqldb == nil {
		return lootOwner{}
	}
	now := time.Now().Unix()
	if v, ok := lootOwnerCache.Load(ownerID); ok {
		if e := v; now-e.at < lootOwnerCacheSec {
			return e.owner
		}
	}
	var out lootOwner
	switch {
	case ownerID >= evedb.MinPlayerEntityID:
		out.CharID = ownerID
		if corpID, err := evedb.GetCharCorpID(ctx, sqldb, ownerID); err == nil && corpID > 0 {
			if npcCorp, err := evedb.IsNPCCorp(ctx, sqldb, corpID); err == nil && !npcCorp {
				out.CorpID = corpID
			}
		}
	case ownerID >= corpIDBandLo && ownerID < corpIDBandHi:
		if npcCorp, err := evedb.IsNPCCorp(ctx, sqldb, ownerID); err == nil && !npcCorp {
			out.CorpID = ownerID
		}
	}
	lootOwnerCache.Store(ownerID, lootOwnerEntry{owner: out, at: now})
	return out
}

// recordLootTheft writes thief->owner and thief->owner's corp into the system's
// crimewatch table and pushes it to the grid, so the owner's client paints the
// thief and the owner's guns are lawful. Same set() as the shot path: a repeat
// take inside the rebroadcast throttle refreshes the timer quietly.
func recordLootTheft(ss *destiny.SolarSystem, thiefCharID int64, owner lootOwner) {
	if ss == nil || thiefCharID <= 0 {
		return
	}
	t := aggroTableFor(ss.ID)
	now := time.Now().Unix()
	changed := false
	if owner.CharID > 0 && t.set(thiefCharID, owner.CharID, now) {
		changed = true
	}
	if owner.CorpID > 0 && t.set(thiefCharID, owner.CorpID, now) {
		changed = true
	}
	if !changed {
		return
	}
	log.Printf("[aggro] charID=%d took from a container of %d (corp %d) in system %d — aggressed for %s",
		thiefCharID, owner.CharID, owner.CorpID, ss.ID, aggressionDuration)
	broadcastAggression(ss)
}

// sameRealGang reports whether two pilots share a live fleet in the core
// gang registry.
func sameRealGang(a, b int64) bool {
	if globalGangReg == nil || a <= 0 || b <= 0 {
		return false
	}
	g := globalGangReg.GetCharGang(a)
	return g != 0 && g == globalGangReg.GetCharGang(b)
}

// mayWarpToMember reports whether a pilot may warp to another pilot's ship: a different
// pilot of the same fleet whose ship can be seen (not cloaked, not in GM mode).
func mayWarpToMember(charID, targetCharID int64, target *destiny.Entity) bool {
	if charID == targetCharID || target == nil || target.IsUntargetable() {
		return false
	}
	return sameRealGang(charID, targetCharID)
}

// minWarpDistanceM is the client's const.minWarpDistance: it offers Warp To only beyond it.
const minWarpDistanceM = 150_000

// minWarpToleranceM absorbs the drift between the client's ball positions and the server's, so
// a warp the client offered at the limit is not refused.
const minWarpToleranceM = 2_000

// tooCloseToWarp reports whether dest is inside the minimum warp distance of a ship at from,
// centre to centre.
func tooCloseToWarp(from, dest destiny.Vec3) bool {
	return dest.Sub(from).Len() < minWarpDistanceM-minWarpToleranceM
}

// warpableBall resolves a ball in the pilot's system that Warp To may aim at and no static
// list holds: its position and radius. The pilot's own ship and anything that cannot be seen
// (cloaked, GM mode, landing grace) do not resolve.
func warpableBall(ds *destinyConn, ballID uint32) (pos destiny.Vec3, radius float64, ok bool) {
	ss := ds.solarSystem
	if ss == nil || (ds.entity != nil && ds.entity.ID == ballID) {
		return destiny.Vec3{}, 0, false
	}
	if e := ss.GetEntity(ballID); e != nil && e.IsUntargetable() {
		return destiny.Vec3{}, 0, false
	}
	pos, ok = playerNavTargetLookup(ss)(ballID)
	if !ok {
		return destiny.Vec3{}, 0, false
	}
	return pos, playerNavTargetRadius(ss, ballID), true
}

// noteLootTake is called by the generic Add/MultiAdd paths beside takeOwnerFor,
// the moment a row changes hands, for a take out of an in-space container.
// Anything else, or the law switch off, is a no-op.
func noteLootTake(ctx context.Context, sqldb *sql.DB, ds *destinyConn, kind itemSourceKind, charID, oldOwner int64) {
	if kind != srcLootContainer || !lootTheftAggroEnabled() {
		return
	}
	if ds == nil || ds.solarSystem == nil || sqldb == nil || oldOwner <= 0 || oldOwner == charID {
		return
	}
	owner := resolveLootOwner(ctx, sqldb, oldOwner)
	if !lootTakeIsTheft(charID, ds.corpID, owner, sameRealGang(charID, owner.CharID)) {
		return
	}
	recordLootTheft(ds.solarSystem, charID, owner)
}

// noteLootTheftFor is the module-facing form (PvPHooks.NoteTheft): a bot took
// from ownerID's container in ss. The module has already excused its own
// fleetmates; thiefCorpID 0 is resolved here.
func noteLootTheftFor(ss *destiny.SolarSystem, thiefCharID, thiefCorpID, ownerID int64) {
	if !lootTheftAggroEnabled() || ss == nil || globalDB == nil || thiefCharID <= 0 || ownerID <= 0 || ownerID == thiefCharID {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if thiefCorpID == 0 {
		thiefCorpID, _ = evedb.GetCharCorpID(ctx, globalDB, thiefCharID)
	}
	owner := resolveLootOwner(ctx, globalDB, ownerID)
	if !lootTakeIsTheft(thiefCharID, thiefCorpID, owner, sameRealGang(thiefCharID, owner.CharID)) {
		return
	}
	recordLootTheft(ss, thiefCharID, owner)
}
