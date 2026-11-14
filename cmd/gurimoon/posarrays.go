package main

// Control-tower support arrays: structures that modify something else on the
// same POS. Shield Hardening Arrays (444) carry the multiplier in their own
// damage resonances (130-133) and apply it to the tower's shield; the
// Tracking Array (473) boosts turret sentries' optimal (769) and tracking
// (767) by percentage; the Stealth Emitter Array (480) scales the tower's
// signature (554). No dgmExpressions ship, so the semantics are authored here
// and only the numbers come from the types. An array counts while online under
// an online or reinforced tower; several of a kind stack with the usual penalty.
// Applied by the starbase-guns tick once a second per loaded system.
// See docs/starbase-pos-investigation.md#26-grid-fuels-powercpu-unanchoring-support-arrays-missile-balls.

import (
	"database/sql"
	"math"
	"sort"
	"sync"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

const (
	groupShieldHardeningArray = 444
	groupTrackingArray        = 473
	groupStealthEmitterArray  = 480
)

// posArrayBonus is everything a tower's online arrays add. The zero value is
// not the identity; use noPOSArrayBonus.
type posArrayBonus struct {
	shieldRes   [4]float64 // multipliers on the tower's shield resonances: em, explosive, kinetic, thermal
	sigMult     float64    // on the tower's signature radius
	turretRange float64    // on turret sentries' optimal range
	turretTrack float64    // on turret sentries' tracking speed
}

var noPOSArrayBonus = posArrayBonus{
	shieldRes: [4]float64{1, 1, 1, 1}, sigMult: 1, turretRange: 1, turretTrack: 1,
}

// posArray is one online array as the bonus calculation sees it.
type posArray struct {
	groupID int64
	attrs   map[int]float64
}

// stackMult folds percentage bonuses (+10 = +10%, −25 = −25%) into one
// multiplier, strongest first, with the stacking penalty. Pure.
func stackMult(pcts []float64) float64 {
	sort.Slice(pcts, func(i, j int) bool { return math.Abs(pcts[i]) > math.Abs(pcts[j]) })
	m := 1.0
	for i, p := range pcts {
		m *= 1 + p/100*evedb.StackingPenalty(i)
	}
	return m
}

// posArrayBonuses is the bonus from a set of online arrays. Pure.
func posArrayBonuses(arrays []posArray) posArrayBonus {
	b := noPOSArrayBonus
	// Hardener resonance attributes in [em, explosive, kinetic, thermal]
	// order, the order destiny.Entity keeps its resists in.
	resAttr := [4]int{133, 132, 131, 130}
	var res [4][]float64
	var sig, rng, trk []float64
	for _, a := range arrays {
		switch a.groupID {
		case groupShieldHardeningArray:
			for d, id := range resAttr {
				if v, ok := a.attrs[id]; ok && v > 0 && v < 1 {
					res[d] = append(res[d], (v-1)*100) // 0.75 → −25%
				}
			}
		case groupTrackingArray:
			if v := a.attrs[769]; v != 0 {
				rng = append(rng, v)
			}
			if v := a.attrs[767]; v != 0 {
				trk = append(trk, v)
			}
		case groupStealthEmitterArray:
			if v := a.attrs[554]; v != 0 {
				sig = append(sig, v)
			}
		}
	}
	for d := range res {
		b.shieldRes[d] = stackMult(res[d])
	}
	b.sigMult = stackMult(sig)
	b.turretRange = stackMult(rng)
	b.turretTrack = stackMult(trk)
	return b
}

// posArrayApplied is what was last pushed onto one tower's entity. ent is
// kept so a rebuilt entity (ball swap, system reload), which starts from its
// type's base resists, is noticed and re-applied.
type posArrayApplied struct {
	ent   *destiny.Entity
	sysID int64
	bonus posArrayBonus
}

var posArrayState = struct {
	sync.Mutex
	m map[uint32]posArrayApplied // towerID → applied
}{m: map[uint32]posArrayApplied{}}

// posTowerBonus is the bonus a tower currently gives its structures; the
// guns read their tracking-array bonus here.
func posTowerBonus(towerID int64) posArrayBonus {
	posArrayState.Lock()
	defer posArrayState.Unlock()
	if a, ok := posArrayState.m[uint32(towerID)]; ok {
		return a.bonus
	}
	return noPOSArrayBonus
}

// applyPOSArrays recomputes every tower's array bonus in one system and pushes
// any change onto the tower entity.
func applyPOSArrays(ss *destiny.SolarSystem, sqldb *sql.DB) {
	structures := ss.SnapshotStructureEntities()
	towers := map[uint32]*destiny.Entity{}
	arrays := map[uint32][]posArray{}
	for _, e := range structures {
		if e.IsDestroyed() {
			continue
		}
		if e.Slim.GroupID == evedb.GroupControlTower {
			towers[e.ID] = e
			continue
		}
		switch e.Slim.GroupID {
		case groupShieldHardeningArray, groupTrackingArray, groupStealthEmitterArray:
		default:
			continue
		}
		if e.Slim.ControlTowerID == 0 || posPosState(ss, e.ID) != 4 {
			continue
		}
		tid := uint32(e.Slim.ControlTowerID)
		arrays[tid] = append(arrays[tid], posArray{groupID: e.Slim.GroupID, attrs: posAttrs(sqldb, e.Slim.TypeID)})
	}

	posArrayState.Lock()
	defer posArrayState.Unlock()
	for id, tower := range towers {
		b := noPOSArrayBonus
		if ts := posPosState(ss, id); ts == 4 || ts == 3 {
			b = posArrayBonuses(arrays[id])
		}
		prev, had := posArrayState.m[id]
		if had && prev.ent == tower && prev.bonus == b {
			continue
		}
		if !applyTowerArrayBonus(sqldb, tower, b) {
			continue
		}
		posArrayState.m[id] = posArrayApplied{ent: tower, sysID: ss.ID, bonus: b}
	}
	for id, a := range posArrayState.m {
		if a.sysID == ss.ID && towers[id] == nil {
			delete(posArrayState.m, id)
		}
	}
}

// applyTowerArrayBonus sets the tower entity's shield resists and signature
// from its type's base values and the bonus. false when the type's base stats
// cannot be read (the next pass retries).
func applyTowerArrayBonus(sqldb *sql.DB, tower *destiny.Entity, b posArrayBonus) bool {
	hp, err := structureHP(sqldb, tower.Slim.TypeID)
	if err != nil {
		return false
	}
	var r [3][4]float64
	r[0], r[1], r[2] = hp.ShieldRes, hp.ArmorRes, hp.HullRes
	for d := 0; d < 4; d++ {
		r[0][d] *= b.shieldRes[d]
	}
	tower.SetBaseResists(r)
	tower.SetSigRadiusModifier(b.sigMult)
	return true
}
