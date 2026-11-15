package main

// Conquerable stations change hands when an attacker takes the shield to 0: no timer, no sov
// requirement. They start under CONCORD, the neutral first holder (evedb.MigrateConquerablesToCONCORD).
// conquerStation runs it for a registered station ball in a loaded system; conquerStationHeadless
// for a station whose system nobody has loaded. Notes: docs/station-management.md,
// "Conquerable stations".

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/server"
	"gurimoon/syncx"
)

// conquerableStationEntity is the shootable ball of a conquerable station: its HP and resists,
// and a static manager at the celestial's position and radius. Without the manager Pos() reads
// (0,0,0), and playerNavTargetLookup finds this entity before the celestial ball, so a docking
// burn-in, weapon ranges and approaches all aim at the system's origin.
func conquerableStationEntity(cel evedb.CelestialEntity, ehp *evedb.EntityHP) *destiny.Entity {
	radius := cel.Radius
	if radius <= 0 {
		radius = 5000
	}
	dm := destiny.New(uint32(cel.ItemID), 1e12, 1, 0, radius)
	dm.Pos = destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}
	dm.Stamp = 1
	return &destiny.Entity{
		ID:       uint32(cel.ItemID),
		Manager:  dm,
		BubbleID: -1, // stations are not in bubbles
		IsPlayer: false,
		CharName: cel.Name,
		Slim: destiny.SlimItem{
			BallID:     cel.ItemID,
			ItemID:     cel.ItemID,
			TypeID:     cel.TypeID,
			GroupID:    cel.GroupID,
			CategoryID: cel.CategoryID,
			OwnerID:    cel.OwnerID,
			CorpID:     cel.OwnerID,
		},
		Damage: destiny.DamageState{Shield: 1.0, Tau: ehp.ShieldTau, Armor: 1.0, Structure: 1.0},
		HP: destiny.HPState{
			MaxShield: ehp.ShieldHP, CurShield: ehp.ShieldHP,
			MaxArmor: ehp.ArmorHP, CurArmor: ehp.ArmorHP,
			MaxStructure: ehp.StructureHP, CurStructure: ehp.StructureHP,
			ShieldTau: ehp.ShieldTau,
		},
		Resists: [3][4]float64{ehp.ShieldRes, ehp.ArmorRes, ehp.HullRes},
	}
}

// stationAttackEvery is how often one station's StationUnderAttackEvent may fire.
const stationAttackEvery = time.Minute

// stationAttackLast maps a stationID to the last StationUnderAttackEvent emitted for it.
var stationAttackLast syncx.Map[uint32, time.Time]

// noteStationHit is the side effect of a hit on a conquerable station ball: a StationUnderAttackEvent
// at most once per stationAttackEvery per station, for a module to call its defenders. A station hit
// never touches the NPC threat table, standings or crimewatch: a conquerable is fair game for anyone.
func noteStationHit(ss *destiny.SolarSystem, station *destiny.Entity, attackerCorp, attackerChar int64) {
	if ss == nil || station == nil {
		return
	}
	now := time.Now()
	if last, ok := stationAttackLast.Load(station.ID); ok && now.Sub(last) < stationAttackEvery {
		return
	}
	stationAttackLast.Store(station.ID, now)
	frac := 1.0
	if station.HP.MaxShield > 0 {
		frac = station.HP.CurShield / station.HP.MaxShield
	}
	evedb.EmitWorldEvent(evedb.StationUnderAttackEvent{At: now, StationID: int64(station.ID), SystemID: ss.ID,
		OwnerCorp: station.Slim.CorpID, AttackerCorp: attackerCorp, AttackerChar: attackerChar, ShieldFrac: frac})
}

// stationShieldDown reports whether a station ball's shield is gone: the capture point.
func stationShieldDown(station *destiny.Entity) bool {
	return station != nil && station.HP.MaxShield > 0 && station.HP.CurShield <= 0
}

// conquerStation flips a conquerable station in a loaded system to newCorpID: HP back to full,
// the ball's and the celestial cache's owner, the tables (persistStationConquest), every pilot in
// the system and every pilot docked inside told.
func conquerStation(ss *destiny.SolarSystem, station *destiny.Entity, newCorpID, attackerCharID int64, sqldb *sql.DB) {
	if ss == nil || station == nil || newCorpID == 0 {
		return
	}
	stationID := station.ID
	oldCorpID := station.Slim.CorpID

	station.HP.CurShield = station.HP.MaxShield
	station.HP.CurArmor = station.HP.MaxArmor
	station.HP.CurStructure = station.HP.MaxStructure
	station.Slim.OwnerID = newCorpID
	station.Slim.CorpID = newCorpID
	// New arrivals read the celestial slims.
	for i := range ss.CelSlims {
		if ss.CelSlims[i].ItemID == int64(stationID) {
			ss.CelSlims[i].OwnerID = newCorpID
			ss.CelSlims[i].CorpID = newCorpID
			break
		}
	}
	fullDmg := station.DamageStateFrac()
	fullDmg.Timestamp = server.Win32TimeNow()
	ss.SetCelDamage(stationID, fullDmg)

	persistStationConquest(sqldb, int64(stationID), ss.ID, oldCorpID, newCorpID, attackerCharID)

	// Full HP plus OnSlimItemChange, so the overview shows the new owner.
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		p.SendStamped(func(stamp uint32) error {
			if err := destiny.SendDamageStateChange(p.Conn, globalNodeID, p.AccountID, stamp, stationID, fullDmg); err != nil {
				return err
			}
			return destiny.SendSlimItemChange(p.Conn, globalNodeID, p.AccountID, stamp, station.Slim)
		})
	}
	pushStationOwnerChanged(globalSessReg, int64(stationID), newCorpID)

	log.Printf("[conquer] station %d conquered: corp %d → %d (attacker charID=%d) in system %d",
		stationID, oldCorpID, newCorpID, attackerCharID, ss.ID)
}

// conquerStationHeadless flips a conquerable station by ID: through conquerStation when its system
// is loaded and the ball registered, else in the tables alone (the next arrival builds from them).
// Refuses a station that is not conquerable.
func conquerStationHeadless(sqldb *sql.DB, stationID, newCorpID, attackerCharID int64) error {
	if sqldb == nil || stationID == 0 || newCorpID == 0 {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	ok, err := evedb.IsConquerableStation(ctx, sqldb, stationID)
	if err != nil {
		return err
	}
	if !ok {
		return evedb.ErrNotConquerable
	}
	var systemID, oldCorpID int64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT solarSystemID, corporationID FROM staStations WHERE stationID = ?`, stationID).
		Scan(&systemID, &oldCorpID); err != nil {
		return err
	}
	if ss := sysMgrGet(globalSysMgr, systemID); ss != nil {
		if ball := ss.GetStationEntity(uint32(stationID)); ball != nil {
			conquerStation(ss, ball, newCorpID, attackerCharID, sqldb)
			return nil
		}
	}
	persistStationConquest(sqldb, stationID, systemID, oldCorpID, newCorpID, attackerCharID)
	pushStationOwnerChanged(globalSessReg, stationID, newCorpID)
	log.Printf("[conquer] station %d conquered headless: corp %d → %d (attacker charID=%d) in system %d",
		stationID, oldCorpID, newCorpID, attackerCharID, systemID)
	return nil
}

// persistStationConquest writes a station's new owner: staStations, its assembly lines, the owner
// caches, the system's sovereignty (a held conquerable counts like an outpost, db/sovmajority.go),
// and a StationConqueredEvent.
func persistStationConquest(sqldb *sql.DB, stationID, systemID, oldCorpID, newCorpID, attackerCharID int64) {
	if sqldb != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if err := evedb.UpdateStationOwner(ctx, sqldb, stationID, newCorpID); err != nil {
			log.Printf("[conquer] owner update failed for station %d: %v", stationID, err)
		}
		if err := evedb.UpdateStationLineOwners(ctx, sqldb, stationID, newCorpID); err != nil {
			log.Printf("[conquer] assembly lines of station %d: %v", stationID, err)
		}
		// The new owner starts with a freeport: the old owner's doors, prices and private slots go.
		if err := evedb.ResetStationManagement(ctx, sqldb, stationID); err != nil {
			log.Printf("[conquer] station management of %d: %v", stationID, err)
		}
		if systemID != 0 {
			if _, err := evedb.RecomputeSystemSov(ctx, sqldb, systemID); err != nil {
				log.Printf("[conquer] sov recompute %d: %v", systemID, err)
			}
		}
		cancel()
	}
	refreshStationOwnerCaches(systemID)
	evedb.EmitWorldEvent(evedb.StationConqueredEvent{At: time.Now(), StationID: stationID, SystemID: systemID,
		OldCorp: oldCorpID, NewCorp: newCorpID, AttackerChar: attackerCharID})
}
