package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	evenet "gurimoon/net"
	"gurimoon/safego"
	"gurimoon/seclaw"
	"gurimoon/server"
)

// Remote repair: shield transporters, armour projectors, remote hull repairers
// and energy transfer arrays. Each cycle validates lock and range, then mends
// the target entity through RepairShield/RepairArmor/RepairStructure and
// broadcasts its damage state, so a player, a module projection or an NPC
// heals alike. Cycle shape follows repairCycleLoop: shield and energy land at
// cycle start, armour and hull at the end; the first cycle's capacitor is
// charged at activation. Energy transfer fills a player target's cap only.

// remoteRepKind is what a remote-repair effect mends.
type remoteRepKind int

const (
	remoteRepNone remoteRepKind = iota
	remoteRepShield
	remoteRepArmor
	remoteRepHull
	remoteRepCap
)

// attrShieldTransferRange is the shield transporter's reach (87); the armour
// projector, remote hull repairer and energy transfer array carry theirs in
// maxRange (54) and powerTransferRange (91).
const attrShieldTransferRange int64 = 87

// remoteRepairSpec reads what a remote-repair activation mends, how much per
// cycle and how far. remoteRepNone for any other effect. The remote hull
// repairer shares the armour projector's effect in this database and is told
// apart by carrying a hull amount and no armour one.
func remoteRepairSpec(effectName string, attrs map[int64]float64) (kind remoteRepKind, amount, rangeM float64) {
	switch effectName {
	case dogma.EffNameShieldTransfer:
		return remoteRepShield, attrs[dogma.AttrShieldBonus], attrs[attrShieldTransferRange]
	case "targetArmorRepair":
		if attrs[dogma.AttrArmorDamageAmount] <= 0 && attrs[dogma.AttrStructureDamageAmount] > 0 {
			return remoteRepHull, attrs[dogma.AttrStructureDamageAmount], attrs[dogma.AttrMaxRange]
		}
		return remoteRepArmor, attrs[dogma.AttrArmorDamageAmount], attrs[dogma.AttrMaxRange]
	case dogma.EffNameEnergyTransfer:
		return remoteRepCap, attrs[dogma.AttrPowerTransferAmount], attrs[dogma.AttrPowerTransferRange]
	}
	return remoteRepNone, 0, 0
}

// startRemoteRepair launches the cycle loop for one remote-repair activation,
// replacing any loop the module already had. Reports whether it took the
// activation: false for an effect that is not remote repair, or one with
// nothing to mend with.
func startRemoteRepair(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modItemID, modTypeID int64, effectName string, targetID uint32, durationMs int64,
	attrs map[int64]float64, capNeed float64, sqldb *sql.DB) bool {

	kind, amount, rangeM := remoteRepairSpec(effectName, attrs)
	if kind == remoteRepNone || amount <= 0 || targetID == 0 || ds.solarSystem == nil {
		return false
	}
	if rangeM <= 0 {
		rangeM = 5000
	}
	if ds.cancelWeapon == nil {
		ds.cancelWeapon = make(map[int64]context.CancelFunc)
	}
	if cancelFn, ok := ds.cancelWeapon[modItemID]; ok {
		cancelFn()
	}
	ctx, cancel := context.WithCancel(context.Background())
	ds.cancelWeapon[modItemID] = cancel
	go remoteRepairCycleLoop(ctx, c, addr, accountID, charID, ds, modItemID, modTypeID,
		effectName, targetID, durationMs, kind, amount, rangeM, capNeed, sqldb)
	return true
}

// remoteRepairCycleLoop is one remote repairer running on one target.
func remoteRepairCycleLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modItemID, modTypeID int64, effectName string, targetID uint32, durationMs int64,
	kind remoteRepKind, amount, rangeM, capNeed float64, sqldb *sql.DB) {
	defer safego.Recover("remoteRepairCycleLoop")

	if durationMs <= 0 {
		durationMs = 5000
	}
	cycle := time.Duration(durationMs) * time.Millisecond
	healsAtStart := kind == remoteRepShield || kind == remoteRepCap
	log.Printf("[%s] remote repair start: mod=%d effect=%q target=%d amount=%.1f range=%.0f",
		addr, modItemID, effectName, targetID, amount, rangeM)

	stop := func(why string) {
		log.Printf("[%s] remote repair stop: mod=%d target=%d — %s", addr, modItemID, targetID, why)
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
	}
	// valid answers the per-cycle gate: the target still exists, is still
	// locked, and is still inside the module's reach.
	valid := func() (*destiny.Entity, string) {
		if ds.entity == nil || ds.solarSystem == nil {
			return nil, "ship gone"
		}
		target := ds.solarSystem.GetEntity(targetID)
		if target == nil || target.IsDestroyed() {
			return nil, "target gone"
		}
		ds.targetMu.Lock()
		locked := false
		for _, t := range ds.targets {
			if t == targetID {
				locked = true
				break
			}
		}
		ds.targetMu.Unlock()
		if !locked {
			return nil, "lock lost"
		}
		if target.Pos().Sub(ds.entity.Pos()).Len() > rangeM {
			return nil, "out of range"
		}
		return target, ""
	}
	heal := func(target *destiny.Entity) {
		// Checked at the moment of the aid, every cycle: the target can turn
		// criminal mid-fight, and the act is the repair, not the activation.
		policeRemoteAid(ds, target, charID)
		switch kind {
		case remoteRepShield:
			target.RepairShield(amount)
		case remoteRepArmor:
			target.RepairArmor(amount)
		case remoteRepHull:
			target.RepairStructure(amount)
		case remoteRepCap:
			if target.IsPlayer && target.Conn != nil && target.HasCap() {
				target.BoostCap(amount)
			}
			return
		}
		dmg := target.DamageStateFrac()
		dmg.Timestamp = server.Win32TimeNow()
		broadcastDamageState(ds, target.ID, dmg)
	}

	for n := 0; ; n++ {
		if n > 0 && !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			stop("capacitor empty")
			return
		}
		target, why := valid()
		if target == nil {
			stop(why)
			return
		}
		if healsAtStart {
			heal(target)
		}
		select {
		case <-ctx.Done():
			// Deactivated by the pilot: Deactivate already cleans up.
			return
		case <-time.After(cycle):
		}
		if healsAtStart {
			continue
		}
		target, why = valid()
		if target == nil {
			stop(why)
			return
		}
		heal(target)
	}
}

// policeRemoteAid flags the aider as a global criminal when the pilot they
// just mended is a global criminal or an outlaw in empire space
// (seclaw.AidIsCrime, the client's own aid confirmation) and charges
// security.aidPenaltyPct once per aider->target pair per criminal window. The
// repair still lands. An NPC target is never charged.
func policeRemoteAid(ds *destinyConn, target *destiny.Entity, aiderCharID int64) {
	if ds == nil || ds.entity == nil || ds.solarSystem == nil || target == nil {
		return
	}
	ss := ds.solarSystem
	facts := seclaw.AidFacts{
		AiderCharID:      aiderCharID,
		AiderCorpID:      ds.entity.Slim.CorpID,
		AiderAllianceID:  ds.entity.Slim.AllianceID,
		TargetCharID:     target.Slim.CharID,
		TargetCorpID:     target.Slim.CorpID,
		TargetAllianceID: target.Slim.AllianceID,
		TargetSecStatus:  target.Slim.SecurityStatus,
		SystemID:         ss.ID,
		Security:         ss.Security,
	}
	deps := seclaw.AidDeps{
		TargetIsLegalTgt: func(sysID, targetCharID int64) bool {
			return concordIsFlagged(targetCharID) || isGlobalCriminal(sysID, targetCharID)
		},
		SentryQualifies: sentrySecurityQualifies,
	}
	if crime, _ := seclaw.AidIsCrime(facts, deps); crime {
		markGlobalCriminal(ss, aiderCharID)
		sentryNoteCrime(ss, aiderCharID, ds.entity.Pos())
		if globalDB != nil && aidCharged.claim(aiderCharID, target.Slim.CharID) {
			aider, targetCharID := ds.entity, target.Slim.CharID
			go func() {
				defer safego.Recover("policeRemoteAid")
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				applySecStatusPenalty(ctx, globalDB, aider, aiderCharID, aidPenaltyPct(),
					evedb.EventStandingCombatAssistance, targetCharID, ss.ID, aider.Slim.TypeID,
					"SecurityStatusAssistanceChange", nil)
			}()
		}
	}
}

// ---- microwarpdrive signature bloom ----

// applyMWDBloom puts a running microwarpdrive's signature bloom (attribute
// 554, percent) on the ship through Entity.SigRadiusMult, which locks, missile
// application and turret hits all read. An afterburner carries no 554.
func applyMWDBloom(e *destiny.Entity, attrs map[int64]float64) {
	if e == nil {
		return
	}
	if b := attrs[dogma.AttrSignatureRadiusBonus]; b > 0 {
		e.SetSigRadiusModifier(1 + b/100)
	}
}

// clearMWDBloom lifts the bloom when the prop module stops. Nothing else on a
// ship sets the standing multiplier (a Stealth Emitter Array sets it on the
// tower), so resetting to 1 erases nothing else.
func clearMWDBloom(e *destiny.Entity) {
	if e != nil {
		e.SetSigRadiusModifier(1)
	}
}
