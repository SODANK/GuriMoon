// Moon production: the tick that turns an online tower's harvesters and
// reactors into moon material and reaction products in its silos. The rate is
// shipped data (db/starbaseproddb.go, db/starbasereactiondb.go) and nothing
// here is tunable: the yield must match what the client's Production tab
// prints. One cycle is per tower, harvesters first, then reactors, because a
// reactor consumes what its harvesters produced. The sweep runs every tower
// with something online; the GM Run Cycle button
// (posMgr.RunMoonProcessCycleforTower) runs one with the timers ignored.
package main

import (
	"context"
	"database/sql"
	"log"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	"gurimoon/server"
	"gurimoon/server/posmgr"
	"gurimoon/tick"
)

// startMoonHarvestSweep runs the production cycle for every tower on the
// shard, DB-wide like the fuel sweep, so a tower in an unloaded system keeps
// producing.
func startMoonHarvestSweep(tickMgr *tick.Manager, sqldb *sql.DB) {
	if tickMgr == nil || sqldb == nil {
		return
	}
	posmgr.RunProcessCycle = func(towerID int64) string {
		return runTowerProcessCycle(sqldb, towerID, true)
	}
	const everyTicks = 60 // tick interval is 1s
	tickMgr.RegisterPeriodic("starbase-moon-harvest", everyTicks, func(uint64) bool {
		runMoonHarvestSweep(sqldb)
		return true
	})
}

func runMoonHarvestSweep(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	structures, err := evedb.ListStarbaseStructuresByState(ctx, sqldb, evedb.StarbaseStateOnline)
	cancel()
	if err != nil {
		log.Printf("[pos] harvest sweep: %v", err)
		return
	}
	seen := map[int64]bool{}
	for _, s := range structures {
		if s.ControlTowerID == 0 || seen[s.ControlTowerID] {
			continue
		}
		if s.GroupID != evedb.GroupMoonMining && s.GroupID != evedb.GroupMobileReactor {
			continue
		}
		seen[s.ControlTowerID] = true
		runTowerProcessCycle(sqldb, s.ControlTowerID, false)
	}
}

// towerCycle is one tower's production state for a single pass.
type towerCycle struct {
	ctx        context.Context
	sqldb      *sql.DB
	tower      evedb.StarbaseRecord
	byID       map[int64]evedb.StarbaseRecord
	links      []evedb.StructureLink
	now        uint64
	cycleTicks uint64
	force      bool
	// direct holds what harvesters wired straight into a reactor produced this
	// pass: reactorID → typeID → units. Used or lost; a reactor is not storage.
	direct  map[int64]map[int64]int64
	changed bool
	summary []string
}

// runTowerProcessCycle runs one production pass for a tower. force ignores the
// cycle timers (the GM button). Returns a one-line summary.
func runTowerProcessCycle(sqldb *sql.DB, towerID int64, force bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()
	tower, found, err := evedb.GetStarbase(ctx, sqldb, towerID)
	if err != nil || !found {
		return "no such tower"
	}
	if tower.State != evedb.StarbaseStateOnline && tower.State != evedb.StarbaseStateReinforced {
		return "tower is not online"
	}
	structures, err := evedb.ListStarbaseStructuresForTower(ctx, sqldb, towerID)
	if err != nil {
		log.Printf("[pos] process cycle %d: %v", towerID, err)
		return "error"
	}
	links, err := evedb.ListStructureLinks(ctx, sqldb, towerID)
	if err != nil {
		log.Printf("[pos] process cycle %d links: %v", towerID, err)
	}
	tc := &towerCycle{
		ctx: ctx, sqldb: sqldb, tower: tower, links: links, force: force,
		byID:       map[int64]evedb.StarbaseRecord{},
		now:        server.Win32TimeNow(),
		cycleTicks: uint64(evedb.StarbaseFuelCycleMs(ctx, sqldb, tower.TypeID)) * 10000, // ms → FILETIME
		direct:     map[int64]map[int64]int64{},
	}
	if tc.cycleTicks == 0 {
		return "tower has no cycle time"
	}
	for _, s := range structures {
		tc.byID[s.EntityID] = s
	}
	for _, s := range structures {
		if s.GroupID == evedb.GroupMoonMining && s.State == evedb.StarbaseStateOnline {
			tc.runHarvester(s)
		}
	}
	for _, s := range structures {
		if s.GroupID == evedb.GroupMobileReactor && s.State == evedb.StarbaseStateOnline {
			tc.runReactor(s)
		}
	}
	if tc.changed {
		pushMoonProcessChange(tower)
	}
	if len(tc.summary) == 0 {
		return "nothing ran"
	}
	return joinSummary(tc.summary)
}

func joinSummary(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += "; "
		}
		out += p
	}
	return out
}

// due reports whether a structure's cycle has come round. A structure seen
// for the first time since onlining starts its cycle now rather than paying
// out for the time it spent dark.
func (tc *towerCycle) due(s evedb.StarbaseRecord) bool {
	tickAt, err := evedb.StructureHarvestTick(tc.ctx, tc.sqldb, s.EntityID)
	if err != nil {
		return false
	}
	if tc.force {
		return true
	}
	if tickAt == 0 || tickAt < s.OnlinedAt {
		_ = evedb.SetStructureHarvestTick(tc.ctx, tc.sqldb, s.EntityID, tc.now)
		return false
	}
	return tc.now-tickAt >= tc.cycleTicks
}

func (tc *towerCycle) stamp(id int64) {
	if err := evedb.SetStructureHarvestTick(tc.ctx, tc.sqldb, id, tc.now); err != nil {
		log.Printf("[pos] process cycle: stamp %d: %v", id, err)
	}
}

// destOf is where source sends typeID (0 = unwired).
func (tc *towerCycle) destOf(sourceID, typeID int64) int64 {
	for _, l := range tc.links {
		if l.SourceID == sourceID && l.TypeID == typeID {
			return l.DestID
		}
	}
	return 0
}

// sourceOf is what feeds typeID into dest (0 = unwired).
func (tc *towerCycle) sourceOf(destID, typeID int64) int64 {
	for _, l := range tc.links {
		if l.DestID == destID && l.TypeID == typeID {
			return l.SourceID
		}
	}
	return 0
}

func (tc *towerCycle) onlineStructure(id int64, group int64) (evedb.StarbaseRecord, bool) {
	s, ok := tc.byID[id]
	return s, ok && s.GroupID == group && s.State == evedb.StarbaseStateOnline
}

// runHarvester is one harvest into a silo or straight into a reactor.
func (tc *towerCycle) runHarvester(a evedb.StarbaseRecord) {
	if a.ProvisionTypeID == 0 || tc.tower.MoonID == 0 || !tc.due(a) {
		return
	}
	destID := tc.destOf(a.EntityID, a.ProvisionTypeID)
	if destID == 0 {
		// Unwired: the array runs and nothing is produced. The cycle is not stamped,
		// so wiring it up pays out on the next sweep.
		return
	}
	abundance, err := evedb.MoonAbundance(tc.ctx, tc.sqldb, tc.tower.MoonID, a.ProvisionTypeID)
	if err != nil {
		log.Printf("[pos] harvest: abundance for moon %d: %v", tc.tower.MoonID, err)
		return
	}
	units := evedb.HarvestUnitsPerCycle(
		evedb.HarvesterQuality(tc.ctx, tc.sqldb, a.TypeID),
		abundance,
		evedb.MoonMiningAmount(tc.ctx, tc.sqldb, a.ProvisionTypeID))
	if units == 0 {
		// Set to a material this moon does not carry: it ran, found nothing.
		tc.stamp(a.EntityID)
		return
	}
	if _, isReactor := tc.onlineStructure(destID, evedb.GroupMobileReactor); isReactor {
		m := tc.direct[destID]
		if m == nil {
			m = map[int64]int64{}
			tc.direct[destID] = m
		}
		m[a.ProvisionTypeID] += units
		tc.stamp(a.EntityID)
		return
	}
	silo, isSilo := tc.onlineStructure(destID, evedb.GroupSilo)
	if !isSilo {
		return // wired into something that is not an online silo or reactor
	}
	stored, err := evedb.AddToSilo(tc.ctx, tc.sqldb, destID, silo.TypeID, a.ProvisionTypeID, units)
	if err != nil {
		log.Printf("[pos] harvest: store into silo %d: %v", destID, err)
		return
	}
	if stored == 0 {
		// Full silo: unstamped, so emptying it resumes production at once.
		return
	}
	tc.stamp(a.EntityID)
	tc.changed = true
	tc.summary = append(tc.summary, "harvested")
	log.Printf("[pos] harvester %d (tower %d, moon %d) produced %d unit(s) of type %d into silo %d",
		a.EntityID, tc.tower.EntityID, tc.tower.MoonID, stored, a.ProvisionTypeID, destID)
}

// runReactor is one reaction: every input covered from its wired source (a
// silo, or a harvester feeding the reactor this pass) and every output wired
// to an online silo, or nothing happens at all.
func (tc *towerCycle) runReactor(r evedb.StarbaseRecord) {
	_, reactionTypeID, ok, err := evedb.ReactorReaction(tc.ctx, tc.sqldb, r.EntityID)
	if err != nil || !ok {
		return
	}
	if accepts, err := evedb.ReactorAccepts(tc.ctx, tc.sqldb, r.TypeID, reactionTypeID); err != nil || !accepts {
		return
	}
	if !tc.due(r) {
		return
	}
	inputs, outputs, err := evedb.ReactionRecipe(tc.ctx, tc.sqldb, reactionTypeID)
	if err != nil || len(outputs) == 0 {
		return
	}

	type take struct {
		siloID, typeID, units int64
	}
	var takes []take
	direct := tc.direct[r.EntityID]
	for _, in := range inputs {
		need := evedb.ReactionUnits(in.Batches, evedb.MoonMiningAmount(tc.ctx, tc.sqldb, in.TypeID))
		if direct[in.TypeID] >= need {
			continue // fed straight from a harvester this pass
		}
		src := tc.sourceOf(r.EntityID, in.TypeID)
		if _, isSilo := tc.onlineStructure(src, evedb.GroupSilo); !isSilo {
			return // input unwired, or its source is not an online silo
		}
		have, err := evedb.SiloQuantity(tc.ctx, tc.sqldb, src, in.TypeID)
		if err != nil || have < need {
			return // short: wait (unstamped) until it is there
		}
		takes = append(takes, take{src, in.TypeID, need})
	}
	type put struct {
		silo           evedb.StarbaseRecord
		typeID, units  int64
	}
	var puts []put
	for _, out := range outputs {
		dst := tc.destOf(r.EntityID, out.TypeID)
		silo, isSilo := tc.onlineStructure(dst, evedb.GroupSilo)
		if !isSilo {
			return // an unwired output means the reactor does not run
		}
		puts = append(puts, put{silo, out.TypeID, evedb.ReactionUnits(out.Batches, evedb.MoonMiningAmount(tc.ctx, tc.sqldb, out.TypeID))})
	}

	for _, t := range takes {
		if _, ok, err := evedb.ConsumeFromStarbaseBay(tc.ctx, tc.sqldb, t.siloID,
			[]evedb.FuelNeed{{TypeID: t.typeID, Quantity: t.units}}); err != nil || !ok {
			log.Printf("[pos] reactor %d: input %d vanished from silo %d mid-cycle", r.EntityID, t.typeID, t.siloID)
			return
		}
	}
	for _, p := range puts {
		if _, err := evedb.AddToSilo(tc.ctx, tc.sqldb, p.silo.EntityID, p.silo.TypeID, p.typeID, p.units); err != nil {
			log.Printf("[pos] reactor %d: output into silo %d: %v", r.EntityID, p.silo.EntityID, err)
		}
	}
	tc.stamp(r.EntityID)
	tc.changed = true
	tc.summary = append(tc.summary, "reacted")
	log.Printf("[pos] reactor %d (tower %d) ran reaction %d", r.EntityID, tc.tower.EntityID, reactionTypeID)
}

// pushMoonProcessChange sends OnMoonProcessChange(towerID, structures), the
// same 6-tuples GetMoonProcessInfoForTower answers, to every pilot in the
// tower's system, so an open Production tab redraws without Reload.
func pushMoonProcessChange(tower evedb.StarbaseRecord) {
	if globalSysMgr == nil || globalDB == nil {
		return
	}
	ss := globalSysMgr.Get(tower.SolarSystemID)
	if ss == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	info, err := posmgr.MoonProcessInfo(ctx, "pos-cycle", globalDB, tower.EntityID)
	if err != nil {
		return
	}
	args := marshal.PyTuple{Items: []marshal.PyRep{marshal.PyInt{Value: uint64(tower.EntityID)}, info}}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		_ = server.SendNotification(p.Conn, int64(p.AccountID), "OnMoonProcessChange", "charid", args)
	}
}
