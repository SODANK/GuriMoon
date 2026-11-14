// Admin-built stargates in running systems (docs/stargates.md#live-systems). The ESP Stargates
// page writes a pair through these hooks so a loaded system gets the new gate ball, or loses a
// deleted one, without a restart. A system nobody is in needs nothing: the tables carry the
// change and the next arrival builds from them.
package main

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/esp"
)

// gateRemoveClearM is how close a pilot may be to a custom gate before the panel refuses to
// delete it: a ship on the gate grid would lose the ball it is about to jump.
const gateRemoveClearM = 150_000.0

// installStargateESPHooks wires the panel's Create and Delete buttons to the live paths. Called
// once at boot, next to installOutpostESPHooks.
func installStargateESPHooks(sqldb *sql.DB, sysMgr *destiny.SystemManager) {
	esp.StargatePairBuilder = func(a, b evedb.StargateEnd) (evedb.StargatePair, error) {
		return buildStargatePair(sqldb, sysMgr, a, b)
	}
	esp.StargatePairRemover = func(gateID int64) (evedb.StargatePair, error) {
		return removeStargatePair(sqldb, sysMgr, gateID)
	}
}

// buildStargatePair writes a new pair, then presents each gate in its system if that system is
// loaded. Both rows are committed before either ball is built, because a gate's slim carries
// its destination gate (the jumps rowset).
func buildStargatePair(sqldb *sql.DB, sysMgr *destiny.SystemManager, a, b evedb.StargateEnd) (evedb.StargatePair, error) {
	if sqldb == nil {
		return evedb.StargatePair{}, fmt.Errorf("no database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pair, err := evedb.InsertStargatePair(ctx, sqldb, a, b)
	if err != nil {
		return pair, err
	}
	presentGate(ctx, sqldb, sysMgrGet(sysMgr, pair.SystemA), pair.GateA)
	presentGate(ctx, sqldb, sysMgrGet(sysMgr, pair.SystemB), pair.GateB)
	emitStargatePairChanged(pair, false)
	log.Printf("[esp] stargate pair %d (%d) <-> %d (%d) built", pair.GateA, pair.SystemA, pair.GateB, pair.SystemB)
	return pair, nil
}

// presentGate adds one new gate to a loaded system's shared celestial slices, gives it its
// anchored grid and sends the ball to every pilot in the system (a gate is system-wide). Gate
// sentries, customs officers and billboards appear on the next system load. ss nil is a no-op.
func presentGate(ctx context.Context, sqldb *sql.DB, ss *destiny.SolarSystem, gateID int64) {
	if ss == nil {
		return
	}
	cels, err := evedb.GetSystemCelestials(ctx, sqldb, ss.ID)
	if err != nil {
		log.Printf("[esp] presentGate %d: reload celestials: %v", gateID, err)
		return
	}
	for i := range cels {
		if cels[i].ItemID != gateID {
			continue
		}
		// Through the shared builders, so a gate added live is identical to the same gate after
		// a restart.
		bd := makeCelestialBallDescs(cels[i : i+1])[0]
		slim := makeCelestialSlimItems(cels[i : i+1])[0]
		dmg := destiny.DamageState{Shield: 0.75, Tau: 100000, Armor: 0.95, Structure: 1.0}
		ss.AddCelestial(bd, slim, dmg)
		ss.SeedCelestialBubbles([]destiny.BallDesc{bd}, []destiny.SlimItem{slim})
		n := broadcastBallAdd(ss, bd, slim, dmg)
		log.Printf("[esp] gate %d presented in system %d to %d pilots", gateID, ss.ID, n)
		return
	}
	log.Printf("[esp] presentGate: gate %d missing from system %d reload", gateID, ss.ID)
}

// removeStargatePair deletes a custom pair and drops both balls from loaded systems. It refuses
// while a pilot is within gateRemoveClearM of either gate.
func removeStargatePair(sqldb *sql.DB, sysMgr *destiny.SystemManager, gateID int64) (evedb.StargatePair, error) {
	if sqldb == nil {
		return evedb.StargatePair{}, fmt.Errorf("no database")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	rows, err := evedb.ListCustomStargates(ctx, sqldb, 0)
	if err != nil {
		return evedb.StargatePair{}, err
	}
	var pair *evedb.CustomStargateRow
	for i := range rows {
		if rows[i].GateA == gateID || rows[i].GateB == gateID {
			pair = &rows[i]
			break
		}
	}
	if pair == nil {
		return evedb.StargatePair{}, fmt.Errorf("gate %d is not a custom stargate", gateID)
	}
	for _, end := range []struct{ sys, gate int64 }{{pair.SystemA, pair.GateA}, {pair.SystemB, pair.GateB}} {
		if name, near := pilotNearCelestial(sysMgrGet(sysMgr, end.sys), end.gate, gateRemoveClearM); near {
			return evedb.StargatePair{}, fmt.Errorf("%s is within %.0f km of gate %d", name, gateRemoveClearM/1000, end.gate)
		}
	}

	out, err := evedb.DeleteStargatePair(ctx, sqldb, gateID)
	if err != nil {
		return out, err
	}
	for _, end := range []struct{ sys, gate int64 }{{out.SystemA, out.GateA}, {out.SystemB, out.GateB}} {
		if ss := sysMgrGet(sysMgr, end.sys); ss != nil && ss.RemoveCelestial(uint32(end.gate)) {
			broadcastBallRemove(ss, uint32(end.gate))
		}
	}
	emitStargatePairChanged(out, true)
	log.Printf("[esp] stargate pair %d <-> %d removed", out.GateA, out.GateB)
	return out, nil
}

// pilotNearCelestial reports the first pilot in ss within rangeM of the celestial's centre. A
// nil system or an unknown celestial has nobody near it.
func pilotNearCelestial(ss *destiny.SolarSystem, celestialID int64, rangeM float64) (string, bool) {
	if ss == nil {
		return "", false
	}
	pos, ok := ss.CelestialPos(uint32(celestialID))
	if !ok {
		return "", false
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Pos().Sub(pos).LenSq() <= rangeM*rangeM {
			return fmt.Sprintf("a pilot (ship %d)", p.ID), true
		}
	}
	return "", false
}

// emitStargatePairChanged tells world-event subscribers the jump graph changed.
func emitStargatePairChanged(p evedb.StargatePair, removed bool) {
	evedb.EmitWorldEvent(evedb.StargatePairChangedEvent{
		At: time.Now(), GateA: p.GateA, GateB: p.GateB,
		SystemA: p.SystemA, SystemB: p.SystemB, Removed: removed,
	})
}
