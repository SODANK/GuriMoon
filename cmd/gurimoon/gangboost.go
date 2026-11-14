package main

// Gang bonuses, the server half of the Leadership skill group. dogma/gangbonus.go
// decides what a gang gets (pure); this file decides who gets it and pushes it.
// A bonus comes from a member in space in the same solar system and reaches
// every member in space there, so a gang is resolved once per system. Resolves
// are event-driven (broadcastGangSysChange, registerUndock, link start/stop)
// plus a 5 s sweep that pushes nothing unless a boost changed.

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/gang"
	"gurimoon/tick"
)

// gangSpeedModKey is the gang bonus's slot in a ball's speed-modifier map.
// Every other key is a module or entity itemID (positive), so a negative
// constant cannot collide with an afterburner or a web.
const gangSpeedModKey int64 = -910_000_001

// gangBoostDebounce folds a burst of events (a gang forming member by member,
// a disband walking every member) into one resolve.
const gangBoostDebounce = 500 * time.Millisecond

// gangBoostSweepSeconds is the safety sweep's period.
const gangBoostSweepSeconds = 5

var (
	gangBoostPendingMu sync.Mutex
	gangBoostPending   = map[int64]bool{}
	gangBoostArmed     bool

	// gangBoostMu serialises resolves: two concurrent resolves of one gang
	// would each read an entity's old boost and rescale its HP twice.
	gangBoostMu sync.Mutex

	gangTablesLoaded bool
	gangPerLevel     map[int64]float64
	gangMindlinkPct  map[int64]float64
	gangLinkBonus    map[int64]float64 // link typeID to commandBonus
	gangSquadronPct  float64
)

// activeGangLinks returns the link module types a ship is running, read from
// the entity's active-effect table rather than ds.activeModules: it is
// lock-protected and every stop path (Deactivate, cap-out, target loss) clears it.
func activeGangLinks(e *destiny.Entity) []int64 {
	var out []int64
	for _, ae := range e.GetActiveEffects() {
		if _, ok := dogma.GangLinkSpecFor(ae.ModuleType); ok {
			out = append(out, ae.ModuleType)
		}
	}
	return out
}

// markGangDirty schedules a resolve of each gangID.
func markGangDirty(gangIDs ...int64) {
	gangBoostPendingMu.Lock()
	defer gangBoostPendingMu.Unlock()
	for _, id := range gangIDs {
		if id != 0 {
			gangBoostPending[id] = true
		}
	}
	if len(gangBoostPending) == 0 || gangBoostArmed {
		return
	}
	gangBoostArmed = true
	time.AfterFunc(gangBoostDebounce, runGangBoostPending)
}

// markAllGangsDirty schedules every live gang and every gang the board still
// holds: a disbanded gang's members wear its bonus until a resolve resets them.
func markAllGangsDirty() {
	var ids []int64
	if globalGangReg != nil {
		ids = append(ids, globalGangReg.GangIDs()...)
	}
	ids = append(ids, gang.Boosts.GangIDs()...)
	markGangDirty(ids...)
}

// markCharGangDirty schedules the gang charID belongs to, if any.
func markCharGangDirty(charID int64) {
	if globalGangReg == nil || charID == 0 {
		return
	}
	if gid := globalGangReg.GetCharGang(charID); gid != 0 {
		markGangDirty(gid)
	}
}

func runGangBoostPending() {
	gangBoostPendingMu.Lock()
	ids := make([]int64, 0, len(gangBoostPending))
	for id := range gangBoostPending {
		ids = append(ids, id)
	}
	gangBoostPending = map[int64]bool{}
	gangBoostArmed = false
	gangBoostPendingMu.Unlock()

	for _, id := range ids {
		recomputeGangBoosts(id)
	}
}

// startGangBoostSweep registers the safety sweep. It only marks: the tick
// goroutine runs periodics synchronously, and a resolve does database work.
func startGangBoostSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("gang-boost-sweep", gangBoostSweepSeconds, func(uint64) bool {
		markAllGangsDirty()
		return true
	})
}

// gangPlacement is one gang member found in space.
type gangPlacement struct {
	charID int64
	ss     *destiny.SolarSystem
	ent    *destiny.Entity
}

// findPlayerInSpace returns charID's live ship entity and the system it is in,
// or nils when the pilot is docked, offline or between systems.
func findPlayerInSpace(charID int64) (*destiny.SolarSystem, *destiny.Entity) {
	if globalSysMgr == nil || charID == 0 {
		return nil, nil
	}
	for _, ss := range globalSysMgr.All() {
		if e := ss.GetPlayerByCharID(charID); e != nil {
			return ss, e
		}
	}
	return nil, nil
}

// recomputeGangBoosts resolves one gang and pushes every change.
func recomputeGangBoosts(gangID int64) {
	if globalDB == nil || globalGangReg == nil {
		return
	}
	gangBoostMu.Lock()
	defer gangBoostMu.Unlock()

	prev, hadPrev := gang.Boosts.View(gangID)
	g := globalGangReg.GetGang(gangID)
	if g == nil {
		// Disbanded: everyone it was boosting goes back to base.
		if hadPrev {
			for _, m := range prev.Members {
				resetGangBoostIfOrphaned(m.CharID, gangID)
			}
		}
		gang.Boosts.Forget(gangID)
		return
	}

	// Place every member.
	var placed []gangPlacement
	for charID := range g.Members {
		if ss, e := findPlayerInSpace(charID); e != nil {
			placed = append(placed, gangPlacement{charID: charID, ss: ss, ent: e})
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// Static data, loaded on the first resolve that can reach it and retried until
	// it succeeds. gangBoostMu is held.
	if !gangTablesLoaded {
		pl, ml, err1 := evedb.GangSkillTables(ctx, globalDB)
		lb, sq, err2 := evedb.GangLinkTables(ctx, globalDB)
		if err1 != nil || err2 != nil {
			log.Printf("[gang-boost] static tables: %v / %v", err1, err2)
			return
		}
		gangPerLevel, gangMindlinkPct, gangLinkBonus, gangSquadronPct = pl, ml, lb, sq
		gangTablesLoaded = true
	}
	linkScale := evedb.GangLinkStrengthScale(ctx, globalDB)
	ids := make([]int64, len(placed))
	for i, p := range placed {
		ids[i] = p.charID
	}
	data, err := evedb.GetGangSourceData(ctx, globalDB, ids)
	if err != nil {
		log.Printf("[gang-boost] gang %d: %v", gangID, err)
		return // leave everyone as they are rather than strip a live fight
	}

	// Resolve per system.
	bySys := map[int64][]dogma.GangContribution{}
	for _, p := range placed {
		d := data[p.charID]
		bySys[p.ss.ID] = append(bySys[p.ss.ID],
			dogma.SkillContributions(p.charID, d.Levels, d.Implants, gangPerLevel, gangMindlinkPct)...)
		if links := activeGangLinks(p.ent); len(links) > 0 {
			slots := 1
			if len(links) > 1 { // only worth a query when there is a choice to make
				if n, err := evedb.ShipGangLinkSlots(ctx, globalDB, int64(p.ent.ID)); err == nil {
					slots = n
				}
			}
			bySys[p.ss.ID] = append(bySys[p.ss.ID], dogma.LinkContributions(p.charID, links,
				d.Levels, d.Implants, gangLinkBonus, gangSquadronPct, gangMindlinkPct, linkScale, slots)...)
		}
	}
	boostBySys := map[int64]dogma.GangBoost{}
	winners := map[int64][]dogma.GangContribution{}
	for sysID, contribs := range bySys {
		b, w := dogma.ResolveGangBoost(contribs)
		boostBySys[sysID] = b
		if len(w) > 0 {
			winners[sysID] = w
		}
	}

	// Publish before pushing: the attribute push reads the board through
	// computeShipDerivedAttrs and must see the new numbers.
	view := gang.GangView{GangID: gangID, Winners: winners, At: time.Now()}
	placedSet := make(map[int64]bool, len(placed))
	for _, p := range placed {
		placedSet[p.charID] = true
		view.Members = append(view.Members, gang.MemberPlacement{
			CharID: p.charID, SolarSystemID: p.ss.ID, Boost: boostBySys[p.ss.ID].Normalized(),
		})
	}
	for charID := range g.Members {
		if !placedSet[charID] {
			view.Members = append(view.Members, gang.MemberPlacement{CharID: charID})
		}
	}
	gang.Boosts.Publish(view)

	for _, p := range placed {
		applyGangBoost(p.ss, p.ent, boostBySys[p.ss.ID])
	}
	// Anyone this gang boosted last time who has since left it.
	if hadPrev {
		for _, m := range prev.Members {
			if _, still := g.Members[m.CharID]; !still {
				resetGangBoostIfOrphaned(m.CharID, gangID)
			}
		}
	}
}

// resetGangBoostIfOrphaned strips the bonus from a pilot who has left gangID,
// unless they are already in another gang, whose own resolve owns their boost.
func resetGangBoostIfOrphaned(charID, gangID int64) {
	if cur := globalGangReg.GetCharGang(charID); cur != 0 && cur != gangID {
		return
	}
	if ss, e := findPlayerInSpace(charID); e != nil {
		applyGangBoost(ss, e, dogma.GangBoost{})
	}
}

// applyGangBoost installs b on a pilot's entity and, if it changed anything,
// pushes the new speed to the grid and the new stats to the pilot.
func applyGangBoost(ss *destiny.SolarSystem, e *destiny.Entity, b dogma.GangBoost) {
	if e == nil || !e.IsPlayer {
		return
	}
	old := e.GangBoost()
	if !e.ApplyGangBoost(b) {
		return
	}
	charID := e.Slim.CharID
	var parts []string
	for s := dogma.GangStat(0); s < dogma.NumGangStats; s++ {
		if m := b.Mult(s); m != 1 {
			parts = append(parts, fmt.Sprintf("%s×%.3f", s, m))
		}
	}
	if len(parts) == 0 {
		parts = []string{"none"}
	}
	log.Printf("[gang-boost] char %d ship %d: %s", charID, e.ID, strings.Join(parts, ", "))

	// Speed rides the speed-modifier map like a prop mod, so the server sim
	// and every observer's ball move together.
	if b.Mult(dogma.GSMaxVelocity) != old.Mult(dogma.GSMaxVelocity) {
		var speed float64
		if m := b.Mult(dogma.GSMaxVelocity); m == 1 {
			speed, _ = e.RemoveSpeedModifier(gangSpeedModKey)
		} else {
			speed = e.SetSpeedModifier(gangSpeedModKey, m)
		}
		broadcastSetMaxSpeedIn(ss, e, speed)
	}

	if e.Conn == nil {
		return // a bot's embodied ship: the sim is updated, nobody to tell
	}
	shipID := int64(e.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if shipER, err := evedb.GetEntityRow(ctx, globalDB, shipID); err == nil && shipER != nil {
		sendShipDerivedStats(e.Conn, int64(e.AccountID), charID, shipID, shipER, globalDB)
	}
	// sendShipDerivedStats pushes range and scan resolution without incoming EWAR
	// and never sig radius; re-push those three and drop locks now out of range.
	pushEwarVictimStats(e, globalDB)
	// The pools moved, so the absolute damage values did too.
	pushPlayerHP(e)
}
