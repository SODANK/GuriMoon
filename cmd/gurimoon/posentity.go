package main

// Player-owned structures as combat entities. Every category-23 ball gets a
// destiny.Entity when it is added to a system: destiny.StructureEntityFactory,
// installed here, is called from SolarSystem.AddStarbase. The entities follow
// the conquerable-station shape (out of every bubble, never in ss.entities,
// broadcasts gated on KnowsBall) plus a zero-velocity Manager for Pos(). HP
// persists in starbaseDamage through a dirty set flushed every 30 s.
// See docs/starbase-pos-investigation.md#17-structures-as-combat-entities.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/server"
	"gurimoon/syncx"
	"gurimoon/tick"
)

// structureHPTypes caches GetEntityHPByType per typeID; a system load builds
// one entity per structure.
var structureHPTypes syncx.Map[int64, *evedb.EntityHP] // typeID → *evedb.EntityHP

// structureDirty holds the latest HP of every structure changed since the last
// flush, keyed by entityID. The factory reads it first, so a structure whose
// ball is swapped keeps HP that has not reached the database yet.
var structureDirty = struct {
	sync.Mutex
	m map[int64]evedb.StructureDamageRow
}{m: map[int64]evedb.StructureDamageRow{}}

// posAggressors records who has shot a POS: towerID → charID → last shot.
// The sentry guns' "attacked" rule reads it; entries expire after
// posAggressionMemory.
var posAggressors = struct {
	sync.Mutex
	m map[int64]map[int64]time.Time
}{m: map[int64]map[int64]time.Time{}}

// posAggressionMemory is how long a tower's guns remember who shot it
// (server_settings starbase.aggressionMemoryMinutes, default 15).
func posAggressionMemory() time.Duration {
	return evedb.CachedSettingDuration("starbase.aggressionMemoryMinutes", time.Minute, 15, 1, 24*60)
}

// installStructureEntities wires the factory, the removal hook and the flush
// tick. Called once at boot, after the database is up.
func installStructureEntities(sqldb *sql.DB, tickMgr *tick.Manager) {
	if sqldb == nil {
		return
	}
	destiny.StructureEntityFactory = func(ss *destiny.SolarSystem, bd destiny.BallDesc, slim destiny.SlimItem) *destiny.Entity {
		return buildStructureEntity(ss, sqldb, bd, slim)
	}
	destiny.StructureEntityRemoved = func(ss *destiny.SolarSystem, e *destiny.Entity) {
		markStructureDirty(e)
	}
	if tickMgr != nil {
		tickMgr.RegisterPeriodic("starbase-damage-flush", 30, func(uint64) bool {
			flushStructureDamage(sqldb)
			return true
		})
	}
}

// structureHP returns the (cached) HP profile of a structure type.
func structureHP(sqldb *sql.DB, typeID int64) (*evedb.EntityHP, error) {
	if v, ok := structureHPTypes.Load(typeID); ok {
		return v, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	hp, err := evedb.GetEntityHPByType(ctx, sqldb, typeID)
	if err != nil {
		return nil, err
	}
	structureHPTypes.Store(typeID, hp)
	return hp, nil
}

// buildStructureEntity is the destiny.StructureEntityFactory.
func buildStructureEntity(ss *destiny.SolarSystem, sqldb *sql.DB, bd destiny.BallDesc, slim destiny.SlimItem) *destiny.Entity {
	hp, err := structureHP(sqldb, slim.TypeID)
	if err != nil {
		log.Printf("[pos] structure %d (type %d) HP: %v — left as scenery", bd.EntityID, slim.TypeID, err)
		return nil
	}
	mass := bd.Mass
	if mass <= 0 {
		mass = 1e6
	}
	radius := bd.Radius
	if radius <= 0 {
		radius = 500
	}
	dm := destiny.New(bd.EntityID, mass, 1, 0, radius)
	dm.Pos = bd.Pos
	dm.Stamp = 1

	name := bd.Name
	if name == "" {
		name = fmt.Sprintf("Structure %d", bd.EntityID)
	}
	e := &destiny.Entity{
		ID:       bd.EntityID,
		Manager:  dm,
		BubbleID: -1, // structures are not in bubbles (see destiny.StructureEntities)
		IsPlayer: false,
		CharName: name,
		Slim:     slim,
		Damage:   destiny.DamageState{Shield: 1.0, Tau: hp.ShieldTau, Armor: 1.0, Structure: 1.0},
		HP: destiny.HPState{
			MaxShield: hp.ShieldHP, CurShield: hp.ShieldHP,
			MaxArmor: hp.ArmorHP, CurArmor: hp.ArmorHP,
			MaxStructure: hp.StructureHP, CurStructure: hp.StructureHP,
			ShieldTau: hp.ShieldTau,
		},
		Resists: [3][4]float64{hp.ShieldRes, hp.ArmorRes, hp.HullRes},
	}

	// Saved damage: the unflushed dirty entry first, then the table.
	id := int64(bd.EntityID)
	structureDirty.Lock()
	row, have := structureDirty.m[id]
	structureDirty.Unlock()
	if !have {
		ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
		row, have, err = evedb.LoadStructureDamage(ctx, sqldb, id)
		cancel()
		if err != nil {
			log.Printf("[pos] structure %d saved damage: %v", id, err)
		}
	}
	if have {
		e.HP.CurShield = clamp01(row.Shield) * e.HP.MaxShield
		e.HP.CurArmor = clamp01(row.Armor) * e.HP.MaxArmor
		e.HP.CurStructure = clamp01(row.Hull) * e.HP.MaxStructure
		if e.HP.CurStructure <= 0 {
			// A row saying dead belongs to a structure whose death did not finish; never
			// resurrect it at zero, which HasHP reads as unkillable scenery.
			e.HP.CurStructure = e.HP.MaxStructure * 0.01
		}
		if row.SavedAt > 0 {
			if now := server.Win32TimeNow(); now > row.SavedAt {
				e.RegenShield(float64(now-row.SavedAt) / 1e7)
			}
		}
	}
	configureTowerDefence(e, slim)
	return e
}

func clamp01(v float64) float64 {
	switch {
	case v < 0:
		return 0
	case v > 1:
		return 1
	}
	return v
}

// markStructureDirty records e's current HP for the next flush.
func markStructureDirty(e *destiny.Entity) {
	if e == nil || !e.HasHP() {
		return
	}
	d := e.DamageStateFrac()
	structureDirty.Lock()
	structureDirty.m[int64(e.ID)] = evedb.StructureDamageRow{
		EntityID: int64(e.ID), Shield: d.Shield, Armor: d.Armor, Hull: d.Structure,
		SavedAt: server.Win32TimeNow(),
	}
	structureDirty.Unlock()
}

// forgetStructureDamage drops a structure's HP everywhere: it died or was
// removed, and its entityID never carries HP again.
func forgetStructureDamage(sqldb *sql.DB, entityID int64) {
	structureDirty.Lock()
	delete(structureDirty.m, entityID)
	structureDirty.Unlock()
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	_ = evedb.DeleteStructureDamage(ctx, sqldb, entityID)
}

// flushStructureDamage writes the dirty set.
func flushStructureDamage(sqldb *sql.DB) {
	structureDirty.Lock()
	if len(structureDirty.m) == 0 {
		structureDirty.Unlock()
		return
	}
	rows := make([]evedb.StructureDamageRow, 0, len(structureDirty.m))
	for _, r := range structureDirty.m {
		rows = append(rows, r)
	}
	structureDirty.m = map[int64]evedb.StructureDamageRow{}
	structureDirty.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
	defer cancel()
	if err := evedb.SaveStructureDamage(ctx, sqldb, rows); err != nil {
		log.Printf("[pos] structure damage flush (%d rows): %v", len(rows), err)
		// Put them back so the next flush retries, unless something newer arrived.
		structureDirty.Lock()
		for _, r := range rows {
			if _, newer := structureDirty.m[r.EntityID]; !newer {
				structureDirty.m[r.EntityID] = r
			}
		}
		structureDirty.Unlock()
	}
}

// structureHPChanged is called with every damage-state change of a
// structure (hits and shield regen): SetState copy, persistence, and the
// tower's reinforcement floor.
func structureHPChanged(ss *destiny.SolarSystem, e *destiny.Entity, d destiny.DamageState) {
	ss.SetStarbaseDamage(e.ID, d)
	markStructureDirty(e)
	maybeRearmTowerFloor(e)
}

// structureTowerID is the tower a structure belongs to (itself for a tower,
// 0 for a free or standalone structure).
func structureTowerID(e *destiny.Entity) int64 {
	if e.Slim.GroupID == evedb.GroupControlTower {
		return int64(e.ID)
	}
	return e.Slim.ControlTowerID
}

// structureDamaged is the hook every player weapon path calls after a hit
// lands on a structure: kill credit, aggression memory for the guns, and
// the CONCORD ladder. attacker is the shooting ship (nil for anonymous
// damage).
func structureDamaged(ss *destiny.SolarSystem, target, attacker *destiny.Entity,
	weaponTypeID int64, applied float64, destroyed bool, sqldb *sql.DB) {

	if ss == nil || target == nil {
		return
	}
	if attacker != nil {
		target.RecordKillHit(attacker, weaponTypeID, applied, destroyed)
		if attacker.IsPlayer && attacker.Slim.CharID != 0 {
			if towerID := structureTowerID(target); towerID != 0 {
				posAggressors.Lock()
				m := posAggressors.m[towerID]
				if m == nil {
					m = map[int64]time.Time{}
					posAggressors.m[towerID] = m
				}
				m[attacker.Slim.CharID] = time.Now()
				posAggressors.Unlock()
			}
			maybeTriggerStructureConcord(attacker, target, ss, sqldb, globalNPCMgr)
		}
	}
	d := target.DamageStateFrac()
	d.Timestamp = server.Win32TimeNow()
	structureHPChanged(ss, target, d)
	if target.TakeShieldFloorHit() {
		go onTowerShieldFloor(ss, target, sqldb)
	}
}

// posAggressed reports whether charID has shot any structure of towerID
// within posAggressionMemory.
func posAggressed(towerID, charID int64) bool {
	posAggressors.Lock()
	defer posAggressors.Unlock()
	m := posAggressors.m[towerID]
	if m == nil {
		return false
	}
	at, ok := m[charID]
	if !ok {
		return false
	}
	if time.Since(at) > posAggressionMemory() {
		delete(m, charID)
		return false
	}
	return true
}

// handleStructureDestroyed runs the whole death of a structure entity.
// Returns false for anything that is not one, so the death handlers can
// call it first unconditionally.
func handleStructureDestroyed(ss *destiny.SolarSystem, target *destiny.Entity, killer *destiny.Entity) bool {
	if ss == nil || target == nil || !ss.IsStructureEntity(target.ID) {
		return false
	}
	sqldb := globalDB
	entityID := int64(target.ID)
	pos := target.Pos()
	hits := target.KillHitsSnapshot()

	// Tear down the ball (and force field) for everyone who sees it. The removal
	// is the explosion: TerminalExplosion batched with RemoveBalls plays the death effect.
	ids := []uint32{target.ID}
	ss.RemoveStarbase(target.ID)
	if target.Slim.GroupID == evedb.GroupControlTower && ss.RemoveStarbase(starbaseForcefieldID(entityID)) {
		ids = append(ids, starbaseForcefieldID(entityID))
	}
	for _, p := range ss.GetAllPlayersInSystem() {
		if p.Conn == nil {
			continue
		}
		for _, id := range ids {
			if !p.KnowsBall(id) {
				continue
			}
			bid := id
			p.SendStampedUrgent(func(stamp uint32) error {
				return destiny.SendTerminalRemove(p.Conn, globalNodeID, p.AccountID, stamp, bid)
			})
			p.ForgetBalls(bid)
		}
	}
	forgetStructureDamage(sqldb, entityID)
	stopPOSGun(entityID)
	log.Printf("[pos] structure %d (type %d) destroyed in system %d", entityID, target.Slim.TypeID, ss.ID)

	if sqldb == nil {
		return true
	}
	// Everything else is database work, off the combat goroutine.
	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Printf("[pos] structure %d death panic: %v", entityID, r)
			}
		}()
		ctx, cancel := context.WithTimeout(context.Background(), starbaseSweepTimeout)
		defer cancel()
		finishStructureDeath(ctx, ss, sqldb, target, entityID, pos, hits)
	}()
	return true
}

// finishStructureDeath is the database half of a structure's death: loot,
// overlay rows, sovereignty, the item itself, and the killmail.
func finishStructureDeath(ctx context.Context, ss *destiny.SolarSystem, sqldb *sql.DB,
	target *destiny.Entity, entityID int64, pos destiny.Vec3, hits []destiny.KillHit) {

	ownerCorp := target.Slim.CorpID
	ownerAlliance := target.Slim.AllianceID
	name := target.CharName

	// Whatever was in its bays: the usual survival roll into a can.
	dropped, destroyed := spawnContentsLoot(ctx, sqldb, ss, ownerCorp, entityID, pos)

	if tower, isTower, err := evedb.GetStarbase(ctx, sqldb, entityID); err == nil && isTower {
		// A dead tower's structures float free, as on unanchor; the tower row, its
		// sov and its settings go through the /posremove ops path.
		releaseTowerSlaves(ctx, "pos-death", sqldb, ss, entityID)
		if _, err := evedb.RemoveStarbaseFor(ctx, sqldb, tower, nil); err != nil {
			log.Printf("[pos] tower %d death teardown: %v", entityID, err)
		}
		evedb.EmitStarbaseEvent(evedb.StarbaseEventFor(evedb.StarbaseEventDestroyed, tower))
		ownerCorp, ownerAlliance = tower.OwnerCorpID, tower.OwnerAllianceID
	} else {
		if s, isSlave, err := evedb.GetStarbaseStructure(ctx, sqldb, entityID); err == nil && isSlave {
			ownerCorp, ownerAlliance = s.OwnerCorpID, s.OwnerAllianceID
			if _, err := evedb.UnanchorStructure(ctx, sqldb, entityID); err != nil {
				log.Printf("[pos] structure %d death overlay: %v", entityID, err)
			}
			_ = evedb.DeleteStructureLinksFor(ctx, sqldb, entityID)
			_ = evedb.DeleteStructureAccess(ctx, sqldb, entityID)
		}
		if err := evedb.DeleteEntityDeep(ctx, sqldb, entityID); err != nil {
			log.Printf("[pos] structure %d death entity: %v", entityID, err)
		}
	}

	km := killReport{
		VictimCharID:     0,
		VictimName:       name,
		VictimCorpID:     ownerCorp,
		VictimAllianceID: ownerAlliance,
		ShipTypeID:       target.Slim.TypeID,
		SysID:            ss.ID,
		Pos:              pos,
		Time:             time.Now(),
		Hits:             hits,
		DroppedItems:     dropped,
		DestroyedItems:   destroyed,
	}
	if len(hits) > 0 {
		recordKillmail(sqldb, km, false)
	}
}
