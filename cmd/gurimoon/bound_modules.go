package main

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/server"
)

// boundActivate handles the bound call "Activate".
func boundActivate(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds, npcMgr := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds, cc.npcMgr
	if len(cs.ArgTuple.Items) < 2 {
		log.Printf("[%s] bound.Activate: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	actModIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	actModID := int64(actModIDRep.Value)
	// Structure effects (online, unanchor) arrive from the right-click menus with numeric
	// effect IDs. They divert before the ship-module path, which reads string effect names.
	if len(cs.ArgTuple.Items) >= 2 {
		if effID, isStructEff := structureEffectID(cs.ArgTuple.Items[1]); isStructEff {
			if handleStructureEffect(ds, addr, charID, actModID, effID, true, sqldb) {
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
		}
	}
	actEffectName := ""
	if len(cs.ArgTuple.Items) >= 2 {
		actEffectName = resolveEffectName(cs.ArgTuple.Items[1])
	}
	var actTargetID uint32
	if len(cs.ArgTuple.Items) >= 3 {
		if tidVal, ok := cs.ArgTuple.Items[2].(marshal.PyInt); ok {
			actTargetID = uint32(tidVal.Value)
		}
	}
	if actTargetID != 0 && ds.entity != nil && actTargetID == ds.entity.ID {
		log.Printf("[%s] bound.Activate: cannot target self (%d)", addr, actTargetID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	// Police in pursuit jam a criminal's cloaking device.
	if isCloakEffect(actEffectName) && (concordIsFlagged(charID) || policePursuing(charID)) {
		return clientMsgErr(c, pkt, accountID, "CantCloakPolicePursuit", nil)
	}
	// A warp interferes with modules that need a target, area modules and, inside the tunnel,
	// propulsion modules (modulestop.go). Refused before the gate cloak or anything else moves.
	if ds.entity != nil && warpDeniesActivation(actEffectName, actTargetID, ds.entity.IsWarpPending(), ds.entity.IsInWarp()) {
		log.Printf("[%s] bound.Activate: refused — effect %q interferes with the warp", addr, actEffectName)
		return clientMsgErr(c, pkt, accountID, "DeniedActivateInWarp", nil)
	}
	// A cloaking device has its own conditions (cloak.go), and nothing else starts under one.
	if isCloakEffect(actEffectName) {
		clkCtx, clkCancel := context.WithTimeout(context.Background(), 5*time.Second)
		key, dict := cloakRefusal(clkCtx, sqldb, ds, actModID)
		clkCancel()
		if key != "" {
			log.Printf("[%s] bound.Activate: cloak refused for mod=%d — %s", addr, actModID, key)
			return clientMsgErr(c, pkt, accountID, key, dict)
		}
	} else if actEffectName != "online" && moduleCloakOn(ds) {
		log.Printf("[%s] bound.Activate: refused — effect %q under a cloaking device", addr, actEffectName)
		return clientMsgErr(c, pkt, accountID, "DeniedActivateCloaked", nil)
	}
	// Running a module drops the arrival cloak. "online" is fitting bring-up and does not
	// count; a cloaking device takes the arrival cloak over instead of breaking it.
	if actEffectName != "online" && !isCloakEffect(actEffectName) {
		breakGateCloak(ds)
	}
	actRepeat := int64(1)
	if len(cs.ArgTuple.Items) >= 4 {
		if r, ok := cs.ArgTuple.Items[3].(marshal.PyInt); ok {
			actRepeat = int64(r.Value)
		}
	}
	log.Printf("[%s] → bound.Activate itemID=%d effect=%q targetID=%d repeat=%d",
		addr, actModID, actEffectName, actTargetID, actRepeat)
	var actModTypeID int64
	var actModFlag int64
	var actModGroupID int64
	if sqldb != nil {
		actCtx, actCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if er, err := evedb.GetEntityRow(actCtx, sqldb, actModID); err == nil && er != nil {
			actModTypeID = er.TypeID
			actModFlag = er.Flag
			actModGroupID = er.GroupID
		}
		actCancel()
	}
	// A Scan Probe Launcher holding a survey probe fires at the nearest moon (moonsurvey.go).
	// Any other charge, scanner probes included, falls through to the module path.
	if actModGroupID == groupScanProbeLauncher && actEffectName == "useMissiles" && sqldb != nil {
		if handled, refusal := launchSurveyProbe(c, addr, accountID, charID, ds,
			actModID, actModTypeID, actModFlag, sqldb); handled {
			if refusal != "" {
				return server.SendCallException(c, pkt, accountID, customNotify(refusal))
			}
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		// A scanner probe (group 479) is the System Scanner's
		// (scanprobes.go); anything else still falls through.
		if handled, refusal := launchScanProbe(c, addr, accountID, charID, ds,
			actModID, actModTypeID, actModFlag, sqldb); handled {
			if refusal != "" {
				return server.SendCallException(c, pkt, accountID, customNotify(refusal))
			}
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}
	// A missile launcher holding a mine lays it (mines.go); any other charge falls through.
	if mineLauncherGroups[actModGroupID] && actEffectName == "useMissiles" && sqldb != nil {
		if handled, refusal := launchMine(c, addr, accountID, charID, ds,
			actModID, actModTypeID, actModFlag, sqldb); handled {
			if refusal != "" {
				return server.SendCallException(c, pkt, accountID, customNotify(refusal))
			}
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}
	// ds.shipID is set by initDestiny on undock and is 0 while docked, where modules are fitted.
	// The gates and persistence below resolve the docked ship themselves.
	actShipID := ds.shipID
	if actShipID == 0 && sqldb != nil && charID != 0 {
		shCtx, shCancel := context.WithTimeout(context.Background(), 5*time.Second)
		actShipID, _ = evedb.GetCurrentShipID(shCtx, sqldb, charID)
		shCancel()
	}
	// Enforce required-skill prerequisites before onlining a module.
	// Exempt Basic Miner (typeID 3651) from skill checks.
	if actEffectName == "online" && sqldb != nil && charID != 0 && actModTypeID != 0 {
		skCtx, skCancel := context.WithTimeout(context.Background(), 5*time.Second)
		missing := missingFitSkills(skCtx, sqldb, charID, actModTypeID)
		modName := ""
		if missing != "" {
			modName = typeNameOrID(skCtx, sqldb, actModTypeID)
		}
		skCancel()
		if missing != "" {
			log.Printf("[%s] bound.Activate: online rejected for item %d — missing skills: %s",
				addr, actModID, missing)
			// The effectname tag lets DelayedOnlineAttempt (auto-online after fitting) swallow this
			// error. See docs/cmd-misc-notes.md#fit-online-refusal.
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"OnlineHasSkillPrerequisites", map[string]marshal.PyRep{
					"effectname":     marshal.PyString{Value: "online"},
					"itemName":       marshal.PyString{Value: modName},
					"requiredSkills": marshal.PyString{Value: missing},
				}))
		}
	}
	// Validate CPU/PG capacity before onlining a module.
	if actEffectName == "online" && sqldb != nil && actShipID != 0 && actModTypeID != 0 {
		actCtxV, actCancelV := context.WithTimeout(context.Background(), 5*time.Second)
		actShipER, _ := evedb.GetEntityRow(actCtxV, sqldb, actShipID)
		if actShipER != nil {
			actCpuCap, actPgCap, _ := evedb.GetShipCPUPGCapacityFitted(actCtxV, sqldb, actShipER.TypeID, actShipID)
			actCurCPU, actCurPG, _ := evedb.GetShipCPUPowerLoad(actCtxV, sqldb, actShipID)
			actModAttrs, _ := evedb.GetTypeAttributes(actCtxV, sqldb, actModTypeID)
			// Superweapons pay cpu (50) x cpuBonusSuperWeapon (1081), dgmEffects 2436; the pilot's
			// fitting skills and the hull's role bonuses scale it. GetShipCPUPowerLoad folds fitted
			// modules the same way.
			actModCPU, actModPG, _ := evedb.GetModuleFittingNeed(actCtxV, sqldb, charID, actShipER.TypeID, actModTypeID)
			// maxGroupOnline (978): at most N of this invGroup online at once. It is 1 on the six
			// group-588 superweapons.
			if maxOnline := int(actModAttrs[dogma.AttrMaxGroupOnline]); maxOnline > 0 && actModGroupID != 0 {
				nOnline, grpErr := evedb.CountOnlineModulesInGroup(actCtxV, sqldb, actShipID, actModGroupID, actModID)
				if grpErr != nil {
					log.Printf("[%s] bound.Activate: CountOnlineModulesInGroup: %v", addr, grpErr)
				} else if nOnline >= maxOnline {
					log.Printf("[%s] bound.Activate: online rejected for item %d — group %d already has %d online (max %d)",
						addr, actModID, actModGroupID, nOnline, maxOnline)
					actCancelV()
					return server.SendCallException(c, pkt, accountID, server.BuildUserError(
						"CustomInfo", map[string]marshal.PyRep{
							"effectname": marshal.PyString{Value: "online"},
							"info": marshal.PyString{Value: fmt.Sprintf(
								"You can only have %d module of this type online at a time.", maxOnline)},
						}))
				}
			}
			if key, dict := onlineShortfall(typeNameOrID(actCtxV, sqldb, actModTypeID),
				actModCPU, actModPG, actCurCPU, actCurPG, actCpuCap, actPgCap); key != "" {
				log.Printf("[%s] bound.Activate: online rejected for item %d (CPU %.0f+%.0f/%.0f PG %.0f+%.0f/%.0f)",
					addr, actModID, actCurCPU, actModCPU, actCpuCap, actCurPG, actModPG, actPgCap)
				actCancelV()
				// effectname keeps the fitting window's automatic retry
				// silent; the fit path has already said why (clientmsg.go).
				dict["effectname"] = marshal.PyString{Value: "online"}
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(key, dict))
			}
		}
		actCancelV()
	}
	// Gate: non-online activations require the module to be online.
	// Offline modules are greyed-out and should not be activatable.
	if actEffectName != "online" && sqldb != nil && actShipID != 0 && actModID != 0 {
		onlineCtx, onlineCancel := context.WithTimeout(context.Background(), 3*time.Second)
		onIDs, onErr := evedb.GetOnlineModuleIDs(onlineCtx, sqldb, actShipID)
		onlineCancel()
		if onErr == nil && !onIDs[actModID] {
			log.Printf("[%s] bound.Activate: rejected — module %d is offline", addr, actModID)
			nameCtx, nameCancel := context.WithTimeout(context.Background(), 3*time.Second)
			modName := typeNameOrID(nameCtx, sqldb, actModTypeID)
			nameCancel()
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"ModuleNotOnline", map[string]marshal.PyRep{
					"moduleName": marshal.PyString{Value: modName},
				}))
		}
	}
	// moduleReactivationDelay (669). ModuleReactivationDelayed takes %(item)s and %(delay)0.1f
	// in seconds; the attribute is in milliseconds. See docs/main-s8-notes.md#connection-module-state.
	if actEffectName != "online" && actModID != 0 {
		if wait := ds.moduleReactivationWait(actModID); wait > 0 {
			log.Printf("[%s] bound.Activate: rejected — module %d reactivating for another %.1fs",
				addr, actModID, wait.Seconds())
			modName := ""
			if sqldb != nil && actModTypeID != 0 {
				nmCtx, nmCancel := context.WithTimeout(context.Background(), 3*time.Second)
				modName = typeNameOrID(nmCtx, sqldb, actModTypeID)
				nmCancel()
			}
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"ModuleReactivationDelayed", map[string]marshal.PyRep{
					"item":  marshal.PyString{Value: modName},
					"delay": marshal.PyReal{Value: wait.Seconds()},
				}))
		}
		if wait := ds.reloadWait(actModID); wait > 0 {
			log.Printf("[%s] bound.Activate: rejected — module %d reloading for another %.1fs",
				addr, actModID, wait.Seconds())
			modName := ""
			if sqldb != nil && actModTypeID != 0 {
				nmCtx, nmCancel := context.WithTimeout(context.Background(), 3*time.Second)
				modName = typeNameOrID(nmCtx, sqldb, actModTypeID)
				nmCancel()
			}
			return clientMsgErr(c, pkt, accountID, "CantActivateReloadingModule", map[string]marshal.PyRep{
				"module": marshal.PyString{Value: modName}})
		}
	}
	// A module that needs a lock is refused here, before capacitor is spent or a start event
	// goes out. Area modules take no lock: the doomsday entry passes a beacon that is never in
	// ds.targets.
	if actTargetID != 0 && ds.solarSystem != nil && effectIsOffensive(actEffectName) &&
		!isAoEEwarEffect(actEffectName) && !targetIsLocked(ds, actTargetID) {
		log.Printf("[%s] bound.Activate: refused — target %d is not locked (mod=%d effect=%q)", addr, actTargetID, actModID, actEffectName)
		return clientMsgErr(c, pkt, accountID, "EntityTargetMustBeTargeted",
			moduleNameDict(sqldb, "targetTypeName", actModTypeID))
	}
	if ds.activeModules == nil {
		ds.activeModules = make(map[int64]*activeModule)
	}
	// Look up module attributes for cycle duration and mining yield.
	// The full map is kept for EWAR/prop-mod math further down.
	var actDurationMs int64
	var actMiningAmount float64
	actModAttrs := map[int64]float64{}
	if actModTypeID != 0 && sqldb != nil {
		actAttrCtx, actAttrCancel := context.WithTimeout(context.Background(), 5*time.Second)
		modAttrs, attrErr := evedb.GetTypeAttributes(actAttrCtx, sqldb, actModTypeID)
		actAttrCancel()
		if attrErr == nil {
			actModAttrs = modAttrs
			if d, ok := modAttrs[dogma.AttrDuration]; ok {
				actDurationMs = int64(d)
			}
			// Weapons cycle on speed (51), not duration (73), which turrets and launchers do not
			// carry. See docs/main-s5-notes.md#weapon-cycle-attribute.
			if ei, ok := effectMap[actEffectName]; ok && ei.DurationAttributeID == dogma.AttrSpeed {
				if d, ok := modAttrs[dogma.AttrSpeed]; ok && d > 0 {
					actDurationMs = int64(d)
				}
			}
			if m, ok := modAttrs[dogma.AttrMiningAmount]; ok {
				actMiningAmount = m
			}
		}
	}
	// Capacitor gate: the first cycle's cost is consumed here; later cycles are charged by
	// the cycle loop (capMaintLoop for continuous modules).
	actCapNeed := actModAttrs[dogma.AttrCapacitorNeed]
	// Hull bonuses ("Special Ability" traits), resolved once per activation and folded into
	// the repair, mining and weapon paths below.
	var actHullBonuses evedb.HullBonuses
	if sqldb != nil && ds.shipTypeID != 0 {
		hbCtx, hbCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if hbs, hbErr := evedb.GetShipHullBonuses(hbCtx, sqldb, charID, ds.shipTypeID); hbErr == nil {
			actHullBonuses = hbs
		} else {
			log.Printf("[%s] bound.Activate: GetShipHullBonuses: %v", addr, hbErr)
		}
		hbCancel()
	}
	// Self-repair modules apply skill and amplifier bonuses before the cap gate and FX sends,
	// so first-cycle cost, client animation and heal loop agree.
	actIsRepair := actEffectName == "shieldBoosting" ||
		actEffectName == "armorRepair" ||
		actEffectName == "structureRepair"
	actRepairAmountMult := 1.0
	if actIsRepair && sqldb != nil && actModTypeID != 0 {
		repCtx, repCancel := context.WithTimeout(context.Background(), 5*time.Second)
		amountMult, cycleMult, capMult, repErr := evedb.GetRepairModuleBonuses(
			repCtx, sqldb, charID, ds.shipID, actModTypeID, actEffectName == "shieldBoosting")
		repCancel()
		if repErr != nil {
			log.Printf("[%s] bound.Activate: GetRepairModuleBonuses: %v", addr, repErr)
		} else {
			actRepairAmountMult = amountMult
			if cycleMult != 1.0 && actDurationMs > 0 {
				actDurationMs = int64(float64(actDurationMs) * cycleMult)
			}
			actCapNeed *= capMult
			if amountMult != 1.0 || cycleMult != 1.0 || capMult != 1.0 {
				log.Printf("[%s] repair bonuses: mod=%d amount×%.3f cycle×%.3f cap×%.3f → duration=%dms cap=%.1f",
					addr, actModID, amountMult, cycleMult, capMult, actDurationMs, actCapNeed)
			}
		}
		// Hull bonuses: shield-boost / armor-repair amount, cap need and
		// cycle time (e.g. Cyclone +7.5% boost amount per BC level).
		if len(actHullBonuses) > 0 {
			boostAmt, boostCap, repAmt, repCap, repDur := actHullBonuses.RepairMults()
			switch actEffectName {
			case "shieldBoosting":
				actRepairAmountMult *= boostAmt
				actCapNeed *= boostCap
				if boostAmt != 1.0 || boostCap != 1.0 {
					log.Printf("[%s] hull repair bonuses: mod=%d amount×%.3f cap×%.3f", addr, actModID, boostAmt, boostCap)
				}
			case "armorRepair":
				actRepairAmountMult *= repAmt
				actCapNeed *= repCap
				if repDur != 1.0 && actDurationMs > 0 {
					actDurationMs = int64(float64(actDurationMs) * repDur)
				}
				if repAmt != 1.0 || repCap != 1.0 || repDur != 1.0 {
					log.Printf("[%s] hull repair bonuses: mod=%d amount×%.3f cap×%.3f cycle×%.3f", addr, actModID, repAmt, repCap, repDur)
				}
			}
		}
		// Gang links (Damage Control, Rapid Repair, Shield Efficiency, Active Shielding), read
		// once per activation. The duration rides the effect message to the client.
		if ds.entity != nil {
			gb := ds.entity.GangBoost()
			capStat, durStat := dogma.GSArmorRepCapNeed, dogma.GSArmorRepDuration
			if actEffectName == "shieldBoosting" {
				capStat, durStat = dogma.GSShieldBoostCapNeed, dogma.GSShieldBoostDuration
			}
			if actEffectName != "structureRepair" {
				actCapNeed *= gb.Mult(capStat)
				if m := gb.Mult(durStat); m != 1 && actDurationMs > 0 {
					actDurationMs = int64(float64(actDurationMs) * m)
				}
				if gb.Mult(capStat) != 1 || gb.Mult(durStat) != 1 {
					log.Printf("[%s] gang repair links: mod=%d cap×%.3f cycle×%.3f", addr, actModID, gb.Mult(capStat), gb.Mult(durStat))
				}
			}
		}
	}
	// Remote repair skills shorten the cycle and cut the cap a cycle costs. Applied before the
	// first-cycle cap gate and the FX sends. See docs/fitting-notes.md#remote-repair.
	if (actEffectName == dogma.EffNameShieldTransfer || actEffectName == "targetArmorRepair" ||
		actEffectName == dogma.EffNameEnergyTransfer) && sqldb != nil && actModTypeID != 0 && charID != 0 {
		rrCtx, rrCancel := context.WithTimeout(context.Background(), 3*time.Second)
		rrCycle, rrCap := evedb.GetRemoteRepairBonuses(rrCtx, sqldb, charID, actModTypeID)
		rrCancel()
		if rrCycle != 1.0 && actDurationMs > 0 {
			actDurationMs = int64(float64(actDurationMs) * rrCycle)
		}
		actCapNeed *= rrCap
		if rrCycle != 1.0 || rrCap != 1.0 {
			log.Printf("[%s] remote repair skills: mod=%d cycle×%.3f cap×%.3f → duration=%dms cap=%.1f",
				addr, actModID, rrCycle, rrCap, actDurationMs, actCapNeed)
		}
	}
	// Warfare Links: one per ship plus one per online Command Processor. Neither the client nor
	// this server reads maxGroupActive (dogma/attributes.go), so the limit is enforced here.
	if _, isLink := dogma.GangLinkSpecFor(actModTypeID); isLink && actEffectName != "online" && ds.entity != nil && sqldb != nil {
		running := 0
		for _, ae := range ds.entity.GetActiveEffects() {
			if _, ok := dogma.GangLinkSpecFor(ae.ModuleType); ok && ae.ModuleID != actModID {
				running++
			}
		}
		if running > 0 {
			slCtx, slCancel := context.WithTimeout(context.Background(), 3*time.Second)
			slots, slErr := evedb.ShipGangLinkSlots(slCtx, sqldb, ds.shipID)
			slCancel()
			if slErr == nil && running >= slots {
				log.Printf("[%s] bound.Activate: link %d refused — %d running, %d slot(s)", addr, actModID, running, slots)
				lnCtx, lnCancel := context.WithTimeout(context.Background(), 3*time.Second)
				lnName := typeNameOrID(lnCtx, sqldb, actModTypeID)
				lnCancel()
				return clientMsgErr(c, pkt, accountID, "EffectCrowdedOut", map[string]marshal.PyRep{
					"moduleName": marshal.PyString{Value: lnName},
					"count":      marshal.PyInt{Value: uint64(running)},
				})
			}
		}
	}
	if actEffectName != "online" && actCapNeed > 0 {
		if !consumeCapForModule(c, accountID, charID, ds, actCapNeed) {
			log.Printf("[%s] bound.Activate: mod=%d rejected — not enough capacitor (need %.1f)", addr, actModID, actCapNeed)
			var capHave float64
			if ds.entity != nil {
				capHave, _ = ds.entity.CurrentCap()
			}
			capCtx, capCancel := context.WithTimeout(context.Background(), 3*time.Second)
			capMod := typeNameOrID(capCtx, sqldb, actModTypeID)
			capCancel()
			return server.SendCallException(c, pkt, accountID, server.BuildUserError(
				"NotEnoughEnergy", map[string]marshal.PyRep{
					"effectname": marshal.PyString{Value: capMod},
					"need":       marshal.PyReal{Value: actCapNeed},
					"got":        marshal.PyReal{Value: capHave},
				}))
		}
	}
	// Classify EWAR / propulsion effects for special handling below.
	actIsPropMod := isPropModEffectName(actEffectName)
	actIsWeb := actEffectName == "decreaseTargetSpeed" || actEffectName == "modifyTargetSpeed"
	actIsScram := actEffectName == "warpScramble"
	// Range check for targeted activations, mining lasers only. Combat turrets activate on
	// any locked target; the weapon cycle loop applies tracking and falloff.
	if actMiningAmount > 0 && actTargetID != 0 && ds.dm != nil && ds.solarSystem != nil {
		// Read the ship position directly: the 200 ms system tick keeps ds.dm.Pos current, and
		// a Step() here would step twice.
		shipPos := ds.dm.Pos

		// Resolve target position: check asteroids, entities (players/NPCs), and celestials.
		var targetPos destiny.Vec3
		var foundTarget bool
		if tp, ok := ds.solarSystem.GetAsteroidPosition(actTargetID); ok {
			targetPos, foundTarget = tp, true
		} else if target := ds.solarSystem.GetEntity(actTargetID); target != nil {
			targetPos, foundTarget = target.Pos(), true
			// Station entities have no Manager; use the celestial position.
			if targetPos.IsZero() {
				for _, cb := range ds.solarSystem.CelBalls {
					if cb.EntityID == actTargetID {
						targetPos, foundTarget = cb.Pos, true
						break
					}
				}
			}
		} else {
			// Check celestial positions (stargates, planets, etc.).
			for _, cb := range ds.solarSystem.CelBalls {
				if cb.EntityID == actTargetID {
					targetPos, foundTarget = cb.Pos, true
					break
				}
			}
		}
		if foundTarget {
			maxRange := 15000.0 // metres
			if actModTypeID != 0 && sqldb != nil {
				rangeCtx, rangeCancel := context.WithTimeout(context.Background(), 3*time.Second)
				rangeAttrs, rangeErr := evedb.GetTypeAttributes(rangeCtx, sqldb, actModTypeID)
				rangeCancel()
				if rangeErr == nil {
					if r, ok := rangeAttrs[dogma.AttrMaxRange]; ok && r > 0 {
						maxRange = r
					}
				}
			}
			dx := shipPos.X - targetPos.X
			dy := shipPos.Y - targetPos.Y
			dz := shipPos.Z - targetPos.Z
			dist := math.Sqrt(dx*dx + dy*dy + dz*dz)
			if dist > maxRange {
				log.Printf("[%s] bound.Activate: target %d out of range (%.0f > %.0f)", addr, actTargetID, dist, maxRange)
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
		}
	}

	// Mining gates: ice needs an ice harvester, Mercoxit a deep core miner, and an ice
	// harvester cannot mine regular ore. Rejected before the capacitor is drained.
	if actMiningAmount > 0 && actTargetID != 0 && ds.solarSystem != nil && sqldb != nil {
		if astGroup := ds.solarSystem.GetAsteroidGroupID(actTargetID); astGroup != 0 {
			gateCtx, gateCancel := context.WithTimeout(context.Background(), 5*time.Second)
			gateReqs, _ := evedb.GetRequiredSkills(gateCtx, sqldb, actModTypeID)
			gateCancel()
			hasReqSkill := func(skillID int64) bool {
				for _, r := range gateReqs {
					if r.SkillID == skillID {
						return true
					}
				}
				return false
			}
			isIceHarvester := hasReqSkill(dogma.SkillIceHarvesting)
			// Deep core capable: skill-gated lasers (Deep Core Mining Laser I/II) or modulated deep
			// core miners, the only modules taking Mercoxit crystals (chargeGroup 663).
			isDeepCore := hasReqSkill(dogma.SkillDeepCoreMining) ||
				int64(actModAttrs[dogma.AttrChargeGroup1]) == 663
			gateMsg := ""
			switch {
			case astGroup == destiny.GroupIce && !isIceHarvester:
				gateMsg = "Only an ice harvester can extract from ice formations."
			case astGroup != destiny.GroupIce && isIceHarvester:
				gateMsg = "An ice harvester can only be used on ice formations."
			case astGroup == destiny.GroupMercoxit && !isDeepCore:
				gateMsg = "Mercoxit can only be mined with a deep core mining laser."
			}
			if gateMsg != "" {
				log.Printf("[%s] bound.Activate: mining gate rejected mod=%d (type %d) on asteroid group %d: %s",
					addr, actModID, actModTypeID, astGroup, gateMsg)
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"CustomInfo", map[string]marshal.PyRep{
						"info": marshal.PyString{Value: gateMsg},
					}))
			}
			// Ice Harvesting link in the gang shortens the harvester's
			// cycle; the duration reaches the client with the effect.
			if isIceHarvester && ds.entity != nil && actDurationMs > 0 {
				if m := ds.entity.GangBoost().Mult(dogma.GSIceHarvestDuration); m != 1 {
					actDurationMs = int64(float64(actDurationMs) * m)
					log.Printf("[%s] gang ice link: mod=%d cycle×%.3f → %dms", addr, actModID, m, actDurationMs)
				}
			}
			// Ice Harvesting hardwirings shorten the cycle too.
			if isIceHarvester && actDurationMs > 0 && charID != 0 && sqldb != nil {
				iceCtx, iceCancel := context.WithTimeout(context.Background(), 3*time.Second)
				if m := evedb.GetIceCycleMult(iceCtx, sqldb, charID); m != 1 {
					actDurationMs = int64(float64(actDurationMs) * m)
					log.Printf("[%s] ice implants: mod=%d cycle×%.3f → %dms", addr, actModID, m, actDurationMs)
				}
				iceCancel()
			}
		}
	}

	// Apply mining skill + fitted mining-upgrade bonuses.
	if actMiningAmount > 0 && charID != 0 && sqldb != nil {
		skillCtx, skillCancel := context.WithTimeout(context.Background(), 3*time.Second)
		miningLevel, _ := evedb.GetCharacterSkillLevel(skillCtx, sqldb, charID, dogma.SkillMining)
		astroLevel, _ := evedb.GetCharacterSkillLevel(skillCtx, sqldb, charID, dogma.SkillAstrogeology)
		// Skills and mining hardwirings in one term; the levels above feed the log line only.
		skillMult := evedb.GetMiningSkillMult(skillCtx, sqldb, charID)
		upgradeMult, _ := evedb.GetMiningYieldBonus(skillCtx, sqldb, ds.shipID)
		skillCancel()
		// Hull mining bonuses (Osprey +20%/Caldari Cruiser level, barges
		// +3%/Mining Barge level, mining frigates, Hulk...).
		hullMiningMult := actHullBonuses.MiningMult()
		// Mining Foreman in the gang, read once at activation like the rest of this chain.
		gangMiningMult := 1.0
		if ds.entity != nil {
			gangMiningMult = ds.entity.GangBoost().Mult(dogma.GSMiningYield)
		}
		multiplier := skillMult * upgradeMult * hullMiningMult * gangMiningMult
		if multiplier != 1.0 {
			actMiningAmount *= multiplier
			log.Printf("[%s] mining bonuses: Mining=%d Astro=%d skills+implants×%.3f upgrades×%.2f hull×%.2f gang×%.2f total×%.2f yield=%.1f",
				addr, miningLevel, astroLevel, skillMult, upgradeMult, hullMiningMult, gangMiningMult, multiplier, actMiningAmount)
		}
	}

	// Mining crystal: modulated (frequency) miners get a yield boost when
	// the loaded crystal matches the target asteroid's ore group, and the
	// crystal wears out cycle by cycle.
	var actCrystal *miningCrystalState
	if actMiningAmount > 0 && actTargetID != 0 && sqldb != nil && ds.solarSystem != nil {
		chargeGroup := int64(actModAttrs[dogma.AttrChargeGroup1])
		if chargeGroup == 482 || chargeGroup == 663 { // mining / Mercoxit crystals
			crysCtx, crysCancel := context.WithTimeout(context.Background(), 5*time.Second)
			crysItemID, crysTypeID, crysErr := evedb.GetChargeItem(crysCtx, sqldb, ds.shipID, actModFlag)
			if crysErr == nil && crysItemID != 0 {
				crysAttrs, caErr := evedb.GetTypeAttributes(crysCtx, sqldb, crysTypeID)
				if caErr == nil {
					astGroup := ds.solarSystem.GetAsteroidGroupID(actTargetID)
					yieldMult := 1.0
					if int64(crysAttrs[dogma.AttrSpecialisationAsteroidGroup]) == astGroup &&
						crysAttrs[dogma.AttrSpecialisationAsteroidYieldMultiplier] > 0 {
						yieldMult = crysAttrs[dogma.AttrSpecialisationAsteroidYieldMultiplier]
					}
					actCrystal = newCrystalState(crysCtx, sqldb, crysItemID, crysTypeID, crysAttrs)
					actCrystal.yieldMult = yieldMult
					log.Printf("[%s] mining crystal: item=%d type=%d yield×%.3f dmg=%.3f/%.0f",
						addr, crysItemID, crysTypeID, yieldMult, actCrystal.damage, actCrystal.hp)
				}
			}
			crysCancel()
		}
	}

	// Whether this is an offensive effect (DB flag or known weapon effect name). Computed
	// before the FX sends so RoF bonuses can adjust the FX cycle duration too.
	actIsOffensive := effectIsOffensive(actEffectName)
	actIsMissile := actEffectName == "missileLaunching" || actEffectName == "useMissiles"

	// Weapon skill + fitted damage-mod bonuses: scale damage and cycle time.
	actDmgMult := 1.0
	actHullWM := evedb.HullWeaponMults{Dmg: 1, PerType: [4]float64{1, 1, 1, 1}, Cycle: 1, Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1}
	if actIsOffensive && !actIsWeb && !actIsScram && actTargetID != 0 &&
		charID != 0 && sqldb != nil && actModTypeID != 0 {
		bonCtx, bonCancel := context.WithTimeout(context.Background(), 5*time.Second)
		bonChargeType, _ := evedb.GetChargeTypeID(bonCtx, sqldb, ds.shipID, actModFlag)
		dmgMult, cycleMult, bonErr := evedb.GetWeaponCombatBonuses(
			bonCtx, sqldb, charID, ds.shipID, actModTypeID, actModGroupID, bonChargeType, !actIsMissile)
		if bonErr != nil {
			log.Printf("[%s] bound.Activate: GetWeaponCombatBonuses: %v", addr, bonErr)
		} else {
			actDmgMult = dmgMult
			if cycleMult != 1.0 && actDurationMs > 0 {
				actDurationMs = int64(float64(actDurationMs) * cycleMult)
			}
			if dmgMult != 1.0 || cycleMult != 1.0 {
				log.Printf("[%s] weapon bonuses: mod=%d dmg×%.3f cycle×%.3f → duration=%dms",
					addr, actModID, dmgMult, cycleMult, actDurationMs)
			}
		}
		// Hull bonuses matching this weapon class: damage, RoF, range, tracking and cap need.
		if len(actHullBonuses) > 0 {
			wpnReqSkills := map[int64]bool{}
			if reqs, rqErr := evedb.GetRequiredSkills(bonCtx, sqldb, actModTypeID); rqErr == nil {
				for _, r := range reqs {
					wpnReqSkills[r.SkillID] = true
				}
			}
			actHullWM = actHullBonuses.WeaponMults(wpnReqSkills, actModGroupID, actIsMissile)
			actDmgMult *= actHullWM.Dmg
			if actHullWM.Cycle != 1.0 && actDurationMs > 0 {
				actDurationMs = int64(float64(actDurationMs) * actHullWM.Cycle)
			}
			if actHullWM != (evedb.HullWeaponMults{Dmg: 1, PerType: [4]float64{1, 1, 1, 1}, Cycle: 1, Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1}) {
				log.Printf("[%s] hull weapon bonuses: mod=%d dmg×%.3f types×[%.2f %.2f %.2f %.2f] cycle×%.3f opt×%.2f fall×%.2f track×%.2f cap×%.2f → duration=%dms",
					addr, actModID, actHullWM.Dmg, actHullWM.PerType[0], actHullWM.PerType[1], actHullWM.PerType[2], actHullWM.PerType[3],
					actHullWM.Cycle, actHullWM.Optimal, actHullWM.Falloff, actHullWM.Tracking, actHullWM.CapNeed, actDurationMs)
			}
		}
		bonCancel()
	}

	// The "online" effect is persistent, not cyclic: override the cycle duration and repeat
	// so the client keeps it active (the ShipGetInfo contract).
	if actEffectName == "online" {
		actDurationMs = 60000
		actRepeat = 0
	}
	ds.activeModules[actModID] = &activeModule{
		itemID: actModID, typeID: actModTypeID,
		targetID: actTargetID, effectName: actEffectName, isActive: true,
	}
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	// Chance-based effects (ECM, damps, painters) get a WichmannHill
	// 3-tuple randomSeed so the client replays the per-cycle rolls
	// locally; the server draws identical rolls in chanceEffectLoop.
	var chanceRNG *dogma.WichmannHill
	var chanceSeed *[3]int64
	if ei, ok := effectMap[actEffectName]; ok && ei.IsChanceBased() && actTargetID != 0 {
		seed := [3]int64{rand.Int63n(30000) + 1, rand.Int63n(30000) + 1, rand.Int63n(30000) + 1}
		chanceSeed = &seed
		chanceRNG = dogma.NewWichmannHill(seed[0], seed[1], seed[2])
	}
	sendGodmaShipEffectSeeded(c, accountID, actModID, actModTypeID, actShipID, charID, actTargetID, actEffectName, true, actDurationMs, actRepeat, chanceSeed)
	// A doomsday through a cynosural field carries the beacon as its target.
	// See docs/main-s5-notes.md#doomsday-fx-target.
	fxTargetID := actTargetID
	if actEffectName == dogma.EffNameEmpWaveGrid && fxTargetID != 0 &&
		ds.solarSystem != nil && ds.solarSystem.GetEntity(fxTargetID) == nil {
		if _, onGrid := ds.solarSystem.GetStarbasePosition(fxTargetID); !onGrid {
			fxTargetID = 0
		}
	}
	sendModuleFX(c, addr, accountID, ds, actModID, actModTypeID, actModFlag, fxTargetID, actEffectName, true, actDurationMs, actRepeat, sqldb)
	// Track active effect on entity for effectStates in SetState.
	if ds.entity != nil && actEffectName != "online" {
		guid := ""
		isOff := false
		if ei, ok := effectMap[actEffectName]; ok {
			guid = ei.Guid
			isOff = ei.IsOffensive
		}
		if guid == "" {
			// Use same fallback as sendModuleFX.
			guid, isOff = dogma.FallbackGUID(actEffectName)
		}
		guid = dogma.NormalizeClientGUID(guid)
		var chargeType int64
		if sqldb != nil && (isOff || actEffectName == dogma.EffNameMiningLaser) {
			chCtx, chCancel := context.WithTimeout(context.Background(), 3*time.Second)
			chargeType, _ = evedb.GetChargeTypeID(chCtx, sqldb, ds.shipID, actModFlag)
			chCancel()
		}
		ds.entity.SetActiveEffect(actModID, destiny.ActiveEffect{
			EntityID:    ds.entity.ID,
			ModuleID:    actModID,
			ModuleType:  actModTypeID,
			TargetID:    actTargetID,
			OtherType:   chargeType,
			GUID:        guid,
			IsOffensive: isOff,
			DurationMs:  actDurationMs,
			StartTime:   server.Win32TimeNow(),
		})
	}
	// A cloaking device hides the ship now that its start event is out.
	if isCloakEffect(actEffectName) {
		engageModuleCloak(c, addr, accountID, charID, ds, actModID, actModTypeID, actEffectName, actModAttrs, sqldb)
	}
	// Persist online state, then notify isOnline + CPU/PG load changes.
	if actEffectName == "online" && sqldb != nil && actShipID != 0 {
		actCtx2, actCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		if err := evedb.SetModuleOnlineState(actCtx2, sqldb, actModID, true); err != nil {
			log.Printf("[%s] bound.Activate SetModuleOnlineState(%d): %v", addr, actModID, err)
		}
		actCancel2()
		sendModuleAttributeChange(c, accountID, charID, actModID, dogma.AttrIsOnline, 1, 0)
		sendShipFittingStats(c, accountID, charID, actShipID, sqldb)
		pushWeaponAttrs(c, accountID, charID, actShipID, sqldb, ds.activeEffectIDs())
	}
	// A tracking computer moves turret optimal and tracking only while it
	// runs, so the shown ranges follow its start (and its stop, below).
	if actModGroupID == trackingComputerGroup && actEffectName != "online" && sqldb != nil {
		pushWeaponAttrs(c, accountID, charID, ds.shipID, sqldb, ds.activeEffectIDs())
	}
	// A sensor booster moves targeting range and scan resolution only while it runs. The
	// client folds neither, so its Lock Target entry and lock timer follow this push.
	if actModGroupID == sensorBoosterGroup && actEffectName != "online" && sqldb != nil {
		pushEwarVictimStats(ds.entity, sqldb)
	}
	// Resist modules: hardener/damage-control activation or any module
	// coming online can change effective resistances.
	if isResistEffectName(actEffectName) {
		recalcShipResists(ds, sqldb)
	}
	if actTargetID != 0 && ds.solarSystem != nil && ds.solarSystem.GetAsteroidTypeID(actTargetID) != 0 {
		if ds.miningSessions == nil {
			ds.miningSessions = make(map[int64]*miningSession)
		}
		if sess, ok := ds.miningSessions[actModID]; ok {
			sess.cancel()
		}
		mineCtx, mineCancel := context.WithCancel(context.Background())
		sess := &miningSession{cancel: mineCancel, stopCh: make(chan struct{})}
		ds.miningSessions[actModID] = sess
		go miningCycleLoop(mineCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actTargetID, actRepeat, actDurationMs, actMiningAmount, actCapNeed, actCrystal, sess.stopCh, sqldb)
	}
	// Survey scanner: no target. Runs the scan cycle and pushes OnSurveyScanComplete when it
	// lands; registered in miningSessions so Deactivate, warp and dock stop it like a miner.
	if actEffectName == dogma.EffNameSurveyScan {
		if ds.miningSessions == nil {
			ds.miningSessions = make(map[int64]*miningSession)
		}
		if sess, ok := ds.miningSessions[actModID]; ok {
			sess.cancel()
		}
		scanCtx, scanCancel := context.WithCancel(context.Background())
		sess := &miningSession{cancel: scanCancel, stopCh: make(chan struct{})}
		ds.miningSessions[actModID] = sess
		go surveyScanLoop(scanCtx, c, addr, accountID, charID, ds, actModID, actModTypeID,
			actDurationMs, actModAttrs[dogma.AttrSurveyScanRange], actCapNeed, actRepeat, sess.stopCh, sqldb)
	}
	// Cargo/ship scanner: a targeted single-cycle scan. Afterwards only the scanning client
	// gets OnCargoScanComplete / OnShipScanComplete (shipui.py).
	if actEffectName == "cargoScan" || actEffectName == "shipScan" {
		go scannerCycle(c, addr, accountID, charID, ds, actModID, actModTypeID,
			actTargetID, actEffectName, actDurationMs, sqldb)
	}
	// Propulsion module (AB/MWD): apply the self speed boost and push the new max velocity to
	// every client tracking the ball. boost = 1 + speedFactor% x thrust / (shipMass + massAddition).
	if actIsPropMod && ds.entity != nil && ds.dm != nil {
		// Rapid Deployment in the gang scales the prop mod's own
		// speedFactor, read at activation like the rest of this formula.
		speedFactor := actModAttrs[dogma.AttrSpeedFactor] * ds.entity.GangBoost().Mult(dogma.GSPropSpeedFactor)
		boostFactor := actModAttrs[dogma.AttrSpeedBoostFactor]
		massAdd := actModAttrs[dogma.AttrMassAddition]
		mass := ds.dm.Mass()
		boost := 1.0
		if speedFactor > 0 && boostFactor > 0 && mass+massAdd > 0 {
			boost = 1.0 + (speedFactor/100.0)*(boostFactor/(mass+massAdd))
		} else if speedFactor > 0 {
			boost = 1.0 + speedFactor/100.0
		}
		newSpeed := ds.entity.SetSpeedModifier(actModID, boost)
		broadcastSetMaxSpeed(ds, ds.entity, newSpeed)
		applyMWDBloom(ds.entity, actModAttrs)
		log.Printf("[%s] prop mod ON: mod=%d boost=%.2fx maxVel=%.0f m/s", addr, actModID, boost, newSpeed)
	}
	// Self-repair module (shield booster, armor repairer, hull repairer):
	// launch a repair cycle loop that heals the player each cycle.
	if actIsRepair && ds.entity != nil {
		var repairAmount float64
		switch actEffectName {
		case "shieldBoosting":
			repairAmount = actModAttrs[dogma.AttrShieldBonus]
		case "armorRepair":
			repairAmount = actModAttrs[dogma.AttrArmorDamageAmount]
		case "structureRepair":
			repairAmount = actModAttrs[dogma.AttrStructureDamageAmount]
		}
		repairAmount *= actRepairAmountMult
		if repairAmount > 0 {
			if ds.cancelWeapon == nil {
				ds.cancelWeapon = make(map[int64]context.CancelFunc)
			}
			if cancelFn, ok := ds.cancelWeapon[actModID]; ok {
				cancelFn()
			}
			repCtx, repCancel := context.WithCancel(context.Background())
			ds.cancelWeapon[actModID] = repCancel
			go repairCycleLoop(repCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actEffectName, actDurationMs, repairAmount, actCapNeed)
		}
	}
	// Capacitor booster: each cycle uses one loaded charge and injects its capacitorBonus
	// (capBoosterLoop).
	if actEffectName == "powerBooster" && ds.entity != nil && sqldb != nil {
		cbCtx, cbCancel := context.WithTimeout(context.Background(), 3*time.Second)
		cbChargeType, _ := evedb.GetChargeTypeID(cbCtx, sqldb, ds.shipID, actModFlag)
		var cbAmount float64
		if cbChargeType != 0 {
			if cAttrs, cErr := evedb.GetTypeAttributes(cbCtx, sqldb, cbChargeType); cErr == nil {
				cbAmount = cAttrs[dogma.AttrCapacitorBonus]
			}
		}
		cbCancel()
		if cbAmount > 0 {
			if ds.cancelWeapon == nil {
				ds.cancelWeapon = make(map[int64]context.CancelFunc)
			}
			if cancelFn, ok := ds.cancelWeapon[actModID]; ok {
				cancelFn()
			}
			cbLoopCtx, cbLoopCancel := context.WithCancel(context.Background())
			ds.cancelWeapon[actModID] = cbLoopCancel
			go capBoosterLoop(cbLoopCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actModFlag, actEffectName, actDurationMs, cbAmount, sqldb)
		} else {
			log.Printf("[%s] bound.Activate: cap booster mod=%d has no charge loaded — no effect", addr, actModID)
		}
	}
	// Remote repair (remoterep.go): the target is mended each cycle. Its loop is registered in
	// cancelWeapon, so the generic cap-drain loop below stays out.
	actIsRemoteRep := !actIsOffensive && startRemoteRepair(c, addr, accountID, charID, ds,
		actModID, actModTypeID, actEffectName, actTargetID, actDurationMs, actModAttrs, actCapNeed, sqldb)
	if actTargetID != 0 && !actIsOffensive && !actIsRepair && !actIsRemoteRep {
		log.Printf("[%s] bound.Activate: effect %q NOT recognized as offensive (targetID=%d) — weapon will not fire", addr, actEffectName, actTargetID)
	}
	// Launch the weapon damage or EWAR cycle for offensive effects that target entities.
	// Untargeted area modules are excluded: the doomsday entry passes a beacon not in ds.targets.
	if actIsOffensive && actTargetID != 0 && !isAoEEwarEffect(actEffectName) && ds.solarSystem != nil {
		// The lock was checked above the capacitor gate. It is gone here only if something
		// dropped it since (a jam, the target leaving): stop what was just started.
		if !targetIsLocked(ds, actTargetID) {
			log.Printf("[%s] bound.Activate: lock on %d lost during activation — stopping mod=%d", addr, actTargetID, actModID)
			stopModuleNow(c, addr, accountID, charID, ds, actModID, sqldb)
			return nil
		}
		if ds.cancelWeapon == nil {
			ds.cancelWeapon = make(map[int64]context.CancelFunc)
		}
		if cancelFn, ok := ds.cancelWeapon[actModID]; ok {
			cancelFn()
		}
		wpnCtx, wpnCancel := context.WithCancel(context.Background())
		ds.cancelWeapon[actModID] = wpnCancel
		if chanceRNG != nil {
			go chanceEffectLoop(wpnCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actTargetID, actEffectName,
				actDurationMs, actRepeat, actModAttrs, effectMap[actEffectName], chanceRNG, sqldb, npcMgr)
		} else if isCapWarfareEffect(actEffectName) {
			// The Talisman set shortens a nosferatu's or neutralizer's cycle.
			capDurationMs := actDurationMs
			if sqldb != nil && charID != 0 && capDurationMs > 0 {
				nosCtx, nosCancel := context.WithTimeout(context.Background(), 3*time.Second)
				if m := evedb.GetCharImplantMults(nosCtx, sqldb, charID).NosDuration; m != 1 {
					capDurationMs = int64(float64(capDurationMs) * m)
					log.Printf("[%s] Talisman: mod=%d cycle×%.3f → %dms", addr, actModID, m, capDurationMs)
				}
				nosCancel()
			}
			go capWarfareLoop(wpnCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actTargetID, actEffectName,
				capDurationMs, actModAttrs, sqldb, npcMgr)
		} else if actIsWeb || actIsScram {
			go ewarCycleLoop(wpnCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actTargetID, actEffectName, actDurationMs, actModAttrs, sqldb, npcMgr)
		} else {
			// Everything but the hull that moves a turret's reach; GetTurretRangeMults also feeds the
			// attributes shown to the client. See docs/fitting-notes.md#turret-range-skills.
			wpnRangeMult, wpnTrackMult := 1.0, 1.0
			wpnFalloffMult, wpnCapMult := 1.0, 1.0
			if !actIsMissile && sqldb != nil {
				rmCtx, rmCancel := context.WithTimeout(context.Background(), 3*time.Second)
				rmCharge, _ := evedb.GetChargeTypeID(rmCtx, sqldb, ds.shipID, actModFlag)
				rmm := evedb.GetTurretRangeMults(rmCtx, sqldb, charID, ds.shipID, rmCharge, ds.activeEffectIDs())
				rmCancel()
				wpnRangeMult, wpnTrackMult = rmm.Optimal, rmm.Tracking
				wpnFalloffMult, wpnCapMult = rmm.Falloff, rmm.CapNeed
				if rmm != (evedb.TurretRangeMults{Optimal: 1, Falloff: 1, Tracking: 1, CapNeed: 1}) {
					log.Printf("[%s] turret range: mod=%d charge=%d optimal×%.3f falloff×%.3f tracking×%.3f cap×%.3f",
						addr, actModID, rmCharge, rmm.Optimal, rmm.Falloff, rmm.Tracking, rmm.CapNeed)
				}
			}
			go weaponCycleLoop(wpnCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actModFlag, actTargetID, actEffectName, actRepeat, actDurationMs, actDmgMult,
				actModAttrs[dogma.AttrMaxRange]*wpnRangeMult*actHullWM.Optimal,
				actModAttrs[dogma.AttrFalloff]*wpnFalloffMult*actHullWM.Falloff,
				actModAttrs[dogma.AttrTrackingSpeed]*wpnTrackMult*actHullWM.Tracking,
				actHullWM.PerType, actHullWM.CapNeed*wpnCapMult,
				sqldb, npcMgr)
		}
	}
	// Cynosural field: untargeted like the area modules, but it spawns a beacon and returns,
	// so the generic cap-drain loop does not start for a module with no capacitorNeed.
	if actEffectName == dogma.EffNameCynoField {
		lightCynoField(c, addr, accountID, charID, ds, actModID, actModTypeID,
			actModAttrs, actDurationMs, sqldb)
		return nil
	}
	// Titan jump portal: untargeted, pins the ship, burns fuel and arms a one-shot. It
	// returns here so the cap-drain loop does not start a second cycle over the portal window.
	if actEffectName == dogma.EffNameJumpPortal {
		openJumpBridge(c, addr, accountID, charID, ds, actModID, actModTypeID,
			actModAttrs, actDurationMs, sqldb)
		return nil
	}

	// Area modules (smartbomb, ECM burst) arrive with no target: their effect rows are
	// category 1, so the client never asks for one. They pick victims from the bubble each cycle.
	if isAoEEwarEffect(actEffectName) && ds.solarSystem != nil && ds.entity != nil {
		if ds.cancelWeapon == nil {
			ds.cancelWeapon = make(map[int64]context.CancelFunc)
		}
		if cancelFn, ok := ds.cancelWeapon[actModID]; ok {
			cancelFn()
		}
		aoeCtx, aoeCancel := context.WithCancel(context.Background())
		ds.cancelWeapon[actModID] = aoeCancel
		go aoeEwarLoop(aoeCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actEffectName,
			actDurationMs, actRepeat, actModAttrs, actTargetID, sqldb, npcMgr)
	}
	// Continuous cap drain for duration-cycled modules with no dedicated
	// loop (prop mods, active hardeners, damage controls).
	if actCapNeed > 0 && actDurationMs > 0 && actEffectName != "online" {
		_, hasLoop := ds.cancelWeapon[actModID]
		if !hasLoop && ds.miningSessions[actModID] == nil {
			if ds.cancelWeapon == nil {
				ds.cancelWeapon = make(map[int64]context.CancelFunc)
			}
			capCtx, capCancel := context.WithCancel(context.Background())
			ds.cancelWeapon[actModID] = capCancel
			go capMaintLoop(capCtx, c, addr, accountID, charID, ds, actModID, actModTypeID, actEffectName, actDurationMs, actCapNeed, sqldb)
		}
	}
	// A warfare link just started: the gang re-resolves now rather than
	// on the next sweep. The active-effect entry it reads was set above.
	if _, isLink := dogma.GangLinkSpecFor(actModTypeID); isLink && actEffectName != "online" {
		markCharGangDirty(charID)
	}
	return nil
}

// boundDeactivate handles the bound call "Deactivate".
func boundDeactivate(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	deactModIDRep, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	deactModID := int64(deactModIDRep.Value)
	// Structure offline (Deactivate onlineForStructures) diverts before the ship-module
	// path, as in Activate.
	if len(cs.ArgTuple.Items) >= 2 {
		if effID, isStructEff := structureEffectID(cs.ArgTuple.Items[1]); isStructEff {
			if handleStructureEffect(ds, addr, charID, deactModID, effID, false, sqldb) {
				return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
			}
		}
	}
	// disallowEarlyDeactivation (906): a module mid-cycle that carries it cannot be switched off.
	// Runs above the activeModules delete below. See docs/jump-notes.md#cyno.
	if sqldb != nil && deactModID != 0 {
		ddCtx, ddCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if er, err := evedb.GetEntityRow(ddCtx, sqldb, deactModID); err == nil && er != nil {
			if attrs, aErr := evedb.GetTypeAttributes(ddCtx, sqldb, er.TypeID); aErr == nil &&
				attrs[dogma.AttrDisallowEarlyDeactivation] > 0 {
				modName := typeNameOrID(ddCtx, sqldb, er.TypeID)
				ddCancel()
				log.Printf("[%s] bound.Deactivate: refused — %s cannot be interrupted", addr, modName)
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"EffectNotDeactivatible", map[string]marshal.PyRep{
						"moduleName": marshal.PyString{Value: modName},
					}))
			}
		}
		ddCancel()
	}
	deactEffectName := ""
	if len(cs.ArgTuple.Items) >= 2 {
		deactEffectName = resolveEffectName(cs.ArgTuple.Items[1])
	}
	log.Printf("[%s] → bound.Deactivate itemID=%d effect=%q", addr, deactModID, deactEffectName)
	var deactModTypeID int64
	var deactTargetID uint32
	if mod, ok := ds.activeModules[deactModID]; ok {
		deactModTypeID = mod.typeID
		if deactEffectName == "online" && mod.effectName != "online" {
			// Going offline with the default effect still cycling: that effect stops first, with
			// its own stop event, or the button of an offline module keeps glowing. This also
			// ends a mining or survey session, so the offline write below is reached.
			stopModuleNow(c, addr, accountID, charID, ds, deactModID, sqldb)
		} else {
			deactTargetID = mod.targetID
			delete(ds.activeModules, deactModID)
		}
	}
	// Docked, or after a reconnect, activeModules holds nothing. The type is read back so
	// the stop effect still names the module.
	if deactModTypeID == 0 && sqldb != nil {
		dtCtx, dtCancel := context.WithTimeout(context.Background(), 5*time.Second)
		if er, err := evedb.GetEntityRow(dtCtx, sqldb, deactModID); err == nil && er != nil {
			deactModTypeID = er.TypeID
		}
		dtCancel()
	}
	// Same docked-ship resolution as Activate: ds.shipID is 0 in station.
	deactShipID := ds.shipID
	if deactShipID == 0 && sqldb != nil && charID != 0 {
		dsCtx, dsCancel := context.WithTimeout(context.Background(), 5*time.Second)
		deactShipID, _ = evedb.GetCurrentShipID(dsCtx, sqldb, charID)
		dsCancel()
	}
	if ds.miningSessions != nil {
		if sess, ok := ds.miningSessions[deactModID]; ok {
			// Instant stop: close the stop channel so the mining loop
			// breaks out of its cycle wait immediately and sends stop FX.
			select {
			case <-sess.stopCh:
				// already closed
			default:
				close(sess.stopCh)
			}
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	}
	if ds.cancelWeapon != nil {
		if cancelFn, ok := ds.cancelWeapon[deactModID]; ok {
			cancelFn()
			delete(ds.cancelWeapon, deactModID)
		}
	}
	if err := server.SendCallRsp(c, pkt, accountID, marshal.PyNone{}); err != nil {
		return err
	}
	sendGodmaShipEffect(c, accountID, deactModID, deactModTypeID, deactShipID, charID, deactTargetID, deactEffectName, false, 0, 0)
	sendModuleFX(c, addr, accountID, ds, deactModID, deactModTypeID, 0, deactTargetID, deactEffectName, false, 0, 0, sqldb)
	// Clear active effect tracking on entity.
	if ds.entity != nil {
		ds.entity.ClearActiveEffect(deactModID)
		// Prop mod turned off: drop its speed boost and push the restored max velocity. Web and
		// scram debuffs on the target are removed by their ewarCycleLoop when the cancel fires.
		if newSpeed, removed := ds.entity.RemoveSpeedModifier(deactModID); removed {
			broadcastSetMaxSpeed(ds, ds.entity, newSpeed)
			clearMWDBloom(ds.entity)
			log.Printf("[%s] prop mod OFF: mod=%d maxVel=%.0f m/s", addr, deactModID, newSpeed)
		}
		// A cloaking device switched off: reveal the ship and start its delays. No-op for
		// any other module.
		disengageModuleCloak(ds, ds.entity, deactModID, deactModTypeID, sqldb)
	}
	// Persist offline state, then notify isOnline + CPU/PG load changes.
	if deactEffectName == "online" && sqldb != nil && deactShipID != 0 {
		deactCtx2, deactCancel2 := context.WithTimeout(context.Background(), 5*time.Second)
		if err := evedb.SetModuleOnlineState(deactCtx2, sqldb, deactModID, false); err != nil {
			log.Printf("[%s] bound.Deactivate SetModuleOnlineState(%d): %v", addr, deactModID, err)
		}
		deactCancel2()
		sendModuleAttributeChange(c, accountID, charID, deactModID, dogma.AttrIsOnline, 0, 1)
		sendShipFittingStats(c, accountID, charID, deactShipID, sqldb)
		pushWeaponAttrs(c, accountID, charID, deactShipID, sqldb, ds.activeEffectIDs())
	}
	if deactEffectName != "online" {
		pushStatsAfterModuleChange(c, accountID, charID, ds, deactModTypeID, sqldb)
	}
	// Resist modules: hardener/damage-control shutdown or any module
	// going offline can change effective resistances.
	if isResistEffectName(deactEffectName) {
		recalcShipResists(ds, sqldb)
	}
	if _, isLink := dogma.GangLinkSpecFor(deactModTypeID); isLink {
		markCharGangDirty(charID)
	}
	return nil
}
