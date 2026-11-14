package main

import (
	"context"
	"database/sql"
	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/npc"
	"gurimoon/rates"
	"gurimoon/safego"
	"gurimoon/server"
	"log"
	"math/rand"
	"time"
)

// Player-side electronic warfare the targeted loops in main.go do not cover:
// capacitor warfare (neutralizer / nosferatu) and the untargeted area hi-slots
// (smartbomb, ECM burst, doomsday). The area modules' dgmEffects rows are
// category 1, so the client calls Activate(itemID, effectName, None, repeat)
// with no target and the server picks the victims from the attacker's bubble.

// capWarfareThreatSeed is the threat one neut or nos cycle records on an NPC.
// Positive so the brain's highest-threat pick sees it; small so gunfire outranks it.
const capWarfareThreatSeed = 1.0

// warpScrambleHold reports whether this ship's warp drive is held, and by how
// many net points: applied strength (attr 105) minus the warp core strength of
// the victim's online stabilizers. Warp, jump drive and jump bridge ask; a
// scrambler never stopped a dock or a stargate jump (pvptimers.go). Only
// reached when something is scrambling, so an untackled ship pays no query.
func warpScrambleHold(ds *destinyConn, sqldb *sql.DB) (bool, float64) {
	if ds == nil || ds.entity == nil {
		return false, 0
	}
	applied := ds.entity.WarpScrambleTotal()
	if applied <= 0 {
		return false, 0
	}
	var stabs float64
	if sqldb != nil && ds.shipID != 0 {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		v, err := evedb.GetShipWarpCoreStrength(ctx, sqldb, ds.shipID)
		cancel()
		if err != nil {
			// A failed lookup must not hand out a free escape: fall back to no
			// stabilizers, which is what all but a handful of ships fit.
			log.Printf("[ewar] warp core strength shipID=%d: %v", ds.shipID, err)
		} else {
			stabs = v
		}
	}
	net := applied - stabs
	return net > 0, net
}

// warpScrambledError is the refusal every scrambled-departure path answers
// with: the client's own WarpScrambled ("Unable to perform because you are
// warp scrambled.").
func warpScrambledError() marshal.PyRep {
	return server.BuildUserError("WarpScrambled", nil)
}

// isCapWarfareEffect reports whether effectName drains a target's capacitor.
func isCapWarfareEffect(effectName string) bool {
	switch effectName {
	case dogma.EffNameEnergyDestabilization, dogma.EffNameEnergyDestabNew,
		dogma.EffNameEnergyVampire, dogma.EffNameEnergyNosferatu:
		return true
	}
	return false
}

// isAoEEwarEffect reports whether effectName is an untargeted area module.
// empWaveGrid (the doomsday) is the same shape on the wire (effectCategory 1,
// no target) and smartbombVictim does what it needs per ship; only the radius
// (aoeEwarRadius) and the fuel bill differ.
func isAoEEwarEffect(effectName string) bool {
	return effectName == dogma.EffNameEmpWave ||
		effectName == dogma.EffNameECMBurst ||
		effectName == dogma.EffNameEmpWaveGrid
}

// aoeEwarRadius resolves the blast radius of an untargeted area module. The
// doomsdays carry no range attribute (no empFieldRange 99, no maxRange 54):
// their effect is empWaveGrid and the grid is the radius, destiny.BubbleRadius.
func aoeEwarRadius(effectName string, modAttrs map[int64]float64) float64 {
	switch effectName {
	case dogma.EffNameECMBurst:
		return modAttrs[dogma.AttrECMBurstRange]
	case dogma.EffNameEmpWaveGrid:
		return destiny.BubbleRadius
	default:
		return modAttrs[dogma.AttrEmpFieldRange]
	}
}

// drainEntityCap removes amount GJ from an entity's capacitor and returns how
// much was taken. A player victim gets the new charge pushed so the cap gauge
// tracks it; an entity with no capacitor (every NPC) yields nothing here, and
// capWarfareBite gives a nosferatu its full amount against one.
func drainEntityCap(target *destiny.Entity, amount float64) float64 {
	if amount <= 0 || target == nil || !target.HasCap() {
		return 0
	}
	drained := amount
	newVal, ok := target.ConsumeCap(amount)
	if !ok {
		// Not enough left for the full bite: empty it instead, which is what a neut
		// achieves.
		cur, _ := target.CurrentCap()
		newVal, _ = target.ConsumeCap(cur)
		drained = cur
	}
	if drained <= 0 {
		return 0
	}
	if target.IsPlayer && target.Conn != nil {
		if tds, isConn := target.PlayerState.(*destinyConn); isConn {
			sendModuleAttributeChange(target.Conn, int64(target.AccountID), tds.charID,
				tds.shipID, dogma.AttrCharge, newVal, newVal+drained)
		}
	}
	return drained
}

// capWarfareBite drains one cycle from the victim and reports what the
// attacker gains. A nosferatu gains what it drained; against a victim with no
// capacitor (every NPC) it gains the full module amount. A neutralizer gains
// nothing. See docs/combat-notes.md#capacitor-warfare.
func capWarfareBite(victim *destiny.Entity, amount float64, feedsAttacker bool) (drained, gained float64) {
	if victim == nil || amount <= 0 {
		return 0, 0
	}
	drained = drainEntityCap(victim, amount)
	if !feedsAttacker {
		return drained, 0
	}
	if !victim.HasCap() {
		return 0, amount
	}
	return drained, drained
}

// noteCapWarfareVictim is the aggression a neutralizer or nosferatu cycle lands
// on its victim: the gun-hit trio for a pilot, a threat seed for an NPC so the
// rat engages the ship draining it. SeedThreat, not NotifyDamage: the brain
// ignores zero-damage threat, and NotifyDamage would write a combat-log line
// and share faction aggro every cycle.
func noteCapWarfareVictim(ds *destinyConn, victim *destiny.Entity, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	if ds == nil || ds.entity == nil || victim == nil {
		return
	}
	if victim.IsPlayer {
		victim.AddAggressor(ds.entity.ID, time.Now().Unix())
		recordPlayerAggression(ds.entity, victim, ds.solarSystem)
		maybeTriggerConcordResponse(ds.entity, victim, ds.solarSystem, sqldb, npcMgr)
		return
	}
	if npcMgr != nil && !ds.entity.IsGMMode() {
		npcMgr.SeedThreat(victim.ID, ds.entity.ID, capWarfareThreatSeed)
	}
}

// pushEwarVictimStats republishes a player ship's targeting range, scan resolution and
// signature radius with everything transient folded in: incoming EWAR and the ship's own
// running sensor boosters. It drops any lock a shorter range put out of reach. The client
// folds none of these itself. Call it whenever a debuff is set or lifted and whenever a
// sensor booster starts or stops.
func pushEwarVictimStats(victim *destiny.Entity, sqldb *sql.DB) {
	if victim == nil || !victim.IsPlayer || victim.Conn == nil || sqldb == nil {
		return
	}
	vds, ok := victim.PlayerState.(*destinyConn)
	if !ok || vds.shipID == 0 {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	shipER, err := evedb.GetEntityRow(ctx, sqldb, vds.shipID)
	if err != nil || shipER == nil {
		return
	}
	base := computeShipDerivedAttrs(ctx, "ewar-victim", sqldb, vds.charID, vds.shipID, shipER)
	tot := victim.EwarTotals()
	accountID := int64(victim.AccountID)

	// The running set comes from the entity, which is safe off the connection goroutine.
	running := map[int64]bool{}
	for _, ae := range victim.GetActiveEffects() {
		running[ae.ModuleID] = true
	}
	rangeBoost, resBoost := 1.0, 1.0
	if m, err := evedb.GetActiveSensorBoosterRangeMult(ctx, sqldb, vds.shipID, running); err == nil && m > 0 {
		rangeBoost = m
	}
	if m, err := evedb.GetActiveSensorBoosterMult(ctx, sqldb, vds.shipID, running); err == nil && m > 0 {
		resBoost = m
	}

	maxRange := base[dogma.AttrMaxTargetRange] * tot.MaxTargetRangeMult * rangeBoost
	if v, exists := base[dogma.AttrMaxTargetRange]; exists && v > 0 {
		sendModuleAttributeChange(victim.Conn, accountID, vds.charID, vds.shipID,
			dogma.AttrMaxTargetRange, maxRange, v)
	}
	if v, exists := base[dogma.AttrScanResolution]; exists && v > 0 {
		sendModuleAttributeChange(victim.Conn, accountID, vds.charID, vds.shipID,
			dogma.AttrScanResolution, v*tot.ScanResolutionMult*resBoost, v)
	}
	if v, exists := base[dogma.AttrSignatureRadius]; exists && v > 0 {
		sendModuleAttributeChange(victim.Conn, accountID, vds.charID, vds.shipID,
			dogma.AttrSignatureRadius, v*victim.SigRadiusMult(), v)
	}
	if maxRange > 0 {
		dropLocksBeyondRange(victim, vds, maxRange)
	}
}

// dropLocksBeyondRange breaks the victim's locks on anything now further away
// than maxRange; a damp that only refuses new locks is half a damp.
func dropLocksBeyondRange(victim *destiny.Entity, vds *destinyConn, maxRange float64) {
	ss := vds.solarSystem
	if ss == nil {
		return
	}
	pos := victim.Pos()
	var dropped []uint32
	vds.targetMu.Lock()
	kept := vds.targets[:0]
	for _, tid := range vds.targets {
		tpos, ok := resolveLockTarget(vds, nil, tid)
		if ok && tpos.Sub(pos).Len() > maxRange {
			dropped = append(dropped, tid)
			continue
		}
		kept = append(kept, tid)
	}
	vds.targets = kept
	for _, tid := range dropped {
		if cancelFn, locking := vds.lockingTargets[tid]; locking {
			cancelFn()
			delete(vds.lockingTargets, tid)
		}
	}
	vds.targetMu.Unlock()

	for _, tid := range dropped {
		log.Printf("[ewar] damp broke lock: victim=%d target=%d beyond %.0fm", victim.ID, tid, maxRange)
		_ = server.SendNotification(victim.Conn, int64(victim.AccountID), "OnTarget", "charid",
			marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "lost"},
				marshal.PyInt{Value: uint64(tid)},
				marshal.PyNone{},
			}})
		// Tell the far end its yellow box is gone, same as RemoveTarget does.
		if tgt := ss.GetEntity(tid); tgt != nil && tgt.IsPlayer && tgt.Conn != nil {
			onTargetEvent := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "OnTarget"},
				marshal.PyString{Value: "otherlost"},
				marshal.PyInt{Value: uint64(victim.ID)},
				marshal.PyNone{},
			}}
			_ = server.SendNotification(tgt.Conn, int64(tgt.AccountID), "OnMultiEvent", "clientID",
				marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyList{Items: []marshal.PyRep{onTargetEvent}},
				}})
		}
	}
}

// capWarfareLoop runs an energy neutralizer or nosferatu against a locked
// target while the module stays active. Both are plain cycled modules (no
// chance roll, binary in/out of range) differing in which amount/range pair
// they read and in whether the drained energy lands in the attacker's cap
// (capWarfareBite). The loop runs until Deactivate, the target leaves or the
// attacker's cap runs out; repeat is not consulted.
func capWarfareLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modItemID, modTypeID int64, targetID uint32, effectName string,
	durationMs int64, modAttrs map[int64]float64, sqldb *sql.DB, npcMgr *npc.NPCManager) {
	defer safego.Recover("capWarfareLoop")

	if ds.solarSystem == nil {
		return
	}
	if durationMs <= 0 {
		durationMs = 5000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond

	isNos := effectName == dogma.EffNameEnergyVampire || effectName == dogma.EffNameEnergyNosferatu
	amount := modAttrs[dogma.AttrEnergyDestabilizationAmount]
	maxRange := modAttrs[dogma.AttrEnergyDestabilizationRange]
	if isNos {
		amount = modAttrs[dogma.AttrPowerTransferAmount]
		maxRange = modAttrs[dogma.AttrPowerTransferRange]
	}
	if amount <= 0 {
		log.Printf("[%s] cap warfare: mod=%d effect=%q has no amount attribute — nothing to drain", addr, modItemID, effectName)
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
		return
	}
	if maxRange <= 0 {
		maxRange = 5000
	}
	capNeed := modAttrs[dogma.AttrCapacitorNeed]

	for cycle := int64(0); ; cycle++ {
		if ds.solarSystem == nil || ds.entity == nil {
			return
		}
		tgt := ds.solarSystem.GetEntity(targetID)
		if tgt == nil || tgt.IsDestroyed() || tgt.IsInWarp() {
			log.Printf("[%s] cap warfare: target %d gone/destroyed/warping — stopping mod=%d", addr, targetID, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
		// One bar per cycle, like the chance-based modules: the activation FX raised
		// one for the first cycle only, and a neut out of range drains nobody.
		dist := ds.entity.Pos().Sub(tgt.Pos()).Len()
		if jt, ok := destiny.JamBucketForFXGUID(moduleFXGUID(effectName)); ok {
			broadcastJamEvent(ds.solarSystem, ds.entity.ID, modItemID, targetID, jt,
				durationMs, dist <= maxRange)
		}
		if dist > maxRange {
			log.Printf("[%s] cap warfare cycle %d: target %d out of range (%.0f > %.0f)", addr, cycle, targetID, dist, maxRange)
		} else {
			// Pre-2007 rules: a nosferatu moves whatever it drains, with no cap-share
			// comparison, and a cap-less rat feeds it the full amount.
			drained, gained := capWarfareBite(tgt, amount, isNos)
			if gained > 0 {
				newVal := ds.entity.BoostCap(gained)
				sendModuleAttributeChange(c, accountID, charID, ds.shipID, dogma.AttrCharge, newVal, newVal-gained)
			}
			if drained > 0 || gained > 0 {
				log.Printf("[%s] cap warfare cycle %d: mod=%d effect=%q drained %.1f GJ from %d, gained %.1f", addr, cycle, modItemID, effectName, drained, targetID, gained)
			}
			// Activation is the aggression, as for every other EWAR cycle: a neut
			// on a rat removes nothing but still turns the rat on the ship.
			noteCapWarfareVictim(ds, tgt, sqldb, npcMgr)
		}

		select {
		case <-ctx.Done():
			// Deactivate cancelled us; it sends its own FX stop.
			return
		case <-time.After(cycleDuration):
		}
		if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] cap warfare: cap out, stopping mod=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, targetID, effectName, sqldb)
			return
		}
	}
}

// aoeEwarLoop runs a smartbomb, an ECM burst or a doomsday. All three sweep
// the blast grid every cycle and hit everything inside their radius: no lock,
// so friendly ships and the attacker's own drones are hit too.
func aoeEwarLoop(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modItemID, modTypeID int64, effectName string,
	durationMs, repeat int64, modAttrs map[int64]float64, targetID uint32,
	sqldb *sql.DB, npcMgr *npc.NPCManager) {
	defer safego.Recover("aoeEwarLoop")

	if ds.solarSystem == nil || ds.entity == nil {
		return
	}

	// Where the blast lands: the firing ship's own grid, or for a doomsday aimed
	// at a cynosural field (menusvc.ActivateGridSmartBomb) the beacon's grid.
	// blastRadius is the firing hull: reach is measured from its surface, so a
	// long hull covers more space with the same module.
	blastSS, blastOrigin, blastExclude := ds.solarSystem, ds.entity.Pos(), ds.entity.ID
	blastRadius := 0.0
	if ds.entity.Manager != nil {
		blastRadius = ds.entity.Manager.Radius()
	}
	if effectName == dogma.EffNameEmpWaveGrid && targetID != 0 {
		rec, msgKey := resolveJumpBeacon(ctx, sqldb, int64(targetID), 0, charID)
		if msgKey != "" {
			notifyNotABeacon(ctx, c, addr, accountID, modTypeID, sqldb)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
			return
		}
		// Get, never GetOrCreate: a SolarSystem built outside registerUndock has no
		// celestials or bubbles. An unloaded beacon system has nothing to hit.
		var remote *destiny.SolarSystem
		if globalSysMgr != nil {
			remote = globalSysMgr.Get(rec.SolarSystemID)
		}
		if remote == nil {
			log.Printf("[%s] doomsday: beacon system %d not loaded — nothing to hit", addr, rec.SolarSystemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
			return
		}
		// exclude 0: the titan is not on that grid, so there is nobody to leave out.
		// radius 0: the blast is centred on the beacon, not on a hull.
		blastSS, blastOrigin, blastExclude, blastRadius = remote,
			destiny.Vec3{X: rec.X, Y: rec.Y, Z: rec.Z}, 0, 0
		log.Printf("[%s] doomsday: delivered to beacon %d in system %d",
			addr, targetID, rec.SolarSystemID)
	}
	if durationMs <= 0 {
		durationMs = 5000
	}
	cycleDuration := time.Duration(durationMs) * time.Millisecond
	capNeed := modAttrs[dogma.AttrCapacitorNeed]

	isBurst := effectName == dogma.EffNameECMBurst
	radius := aoeEwarRadius(effectName, modAttrs)
	if radius <= 0 {
		log.Printf("[%s] aoe ewar: mod=%d effect=%q has no radius attribute — stopping", addr, modItemID, effectName)
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
		return
	}

	// disallowRepeatingActivation(1014): one cycle, then stop, whatever repeat
	// the client asked for. The doomsdays carry it.
	if modAttrs[dogma.AttrDisallowRepeatingActivation] > 0 {
		repeat = 1
	}
	// moduleReactivationDelay(669) is armed from the end of the last cycle that
	// fired; a module that never got a shot off (no fuel, no cap) arms nothing.
	fired := false
	defer func() {
		if fired {
			ds.armModuleReactivation(modItemID, modAttrs[dogma.AttrModuleReactivationDelay])
		}
	}()

	for cycle := int64(0); ; cycle++ {
		// The firing pilot docking or dying stops the module; the blast grid gets its
		// own check, since a remote system can be torn down between cycles.
		if ds.solarSystem == nil || ds.entity == nil || blastSS == nil {
			return
		}
		// Fuel is charged before the blast, unlike capacitor below: the blast is the
		// thing the fuel gates.
		if !consumeModuleFuel(ctx, c, addr, accountID, charID, ds, modItemID, modTypeID, effectName, modAttrs, sqldb) {
			return
		}
		// A doomsday sweeps the whole system by distance; the short-range
		// area modules stay on the bubble prefilter they have always used.
		victims := aoeEwarVictims(blastSS, blastOrigin, blastRadius, blastExclude, radius,
			effectName == dogma.EffNameEmpWaveGrid)
		fired = true
		log.Printf("[%s] aoe ewar cycle %d: mod=%d effect=%q radius=%.0f victims=%d",
			addr, cycle, modItemID, effectName, radius, len(victims))
		for _, victim := range victims {
			if isBurst {
				burstJamVictim(c, addr, accountID, charID, ds, modItemID, modTypeID,
					victim, durationMs, modAttrs, sqldb, npcMgr)
				continue
			}
			smartbombVictim(c, addr, accountID, charID, ds, modItemID, modTypeID,
				victim, modAttrs, blastSS, sqldb, npcMgr)
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(cycleDuration):
		}
		if repeat < 1000 {
			repeat--
			if repeat <= 0 {
				stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
				return
			}
		}
		if !consumeCapForModule(c, accountID, charID, ds, capNeed) {
			log.Printf("[%s] aoe ewar: cap out, stopping mod=%d", addr, modItemID)
			stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
			return
		}
	}
}

// consumeModuleFuel burns one activation's worth of consumptionType(713) out of
// the ship's cargo hold and pushes the stack changes to the client. Returns
// false, having taken nothing, when the hold cannot cover it. A module with no
// consumptionQuantity is free; nothing here names a fuel type.
func consumeModuleFuel(ctx context.Context, c *evenet.Conn, addr string, accountID, charID int64,
	ds *destinyConn, modItemID, modTypeID int64, effectName string,
	modAttrs map[int64]float64, sqldb *sql.DB) bool {

	fuelType := int64(modAttrs[dogma.AttrConsumptionType])
	fuelQty := int64(modAttrs[dogma.AttrConsumptionQuantity])
	if fuelType == 0 || fuelQty <= 0 || sqldb == nil || ds.shipID == 0 {
		return true
	}

	fuelCtx, fuelCancel := context.WithTimeout(ctx, 10*time.Second)
	ok, deltas, err := evedb.ConsumeCargoFuel(fuelCtx, sqldb, fuelType, charID, ds.shipID, fuelQty)
	fuelCancel()
	if err != nil {
		// A failed take must not hand out a free shot: treat it as no fuel.
		log.Printf("[%s] module fuel: mod=%d ConsumeCargoFuel: %v", addr, modItemID, err)
		ok = false
	}
	if !ok {
		log.Printf("[%s] module fuel: mod=%d needs %d of type %d — insufficient, stopping",
			addr, modItemID, fuelQty, fuelType)
		notifyModuleRequiresFuel(ctx, c, addr, accountID, modTypeID, fuelType, sqldb)
		stopWeaponFX(c, addr, accountID, charID, ds, modItemID, modTypeID, 0, effectName, sqldb)
		return false
	}
	// The cargo hold is open on screen while this fires; without the pushes the
	// stack keeps its old quantity until the player reopens the window.
	for _, d := range deltas {
		if d.Row == nil {
			continue
		}
		if d.Kind == evedb.ItemDeltaRemoved {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(d.Row))
			continue
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(d.Row),
			marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(d.OldQty)}}, // ixQuantity
			}},
		}})
	}
	log.Printf("[%s] module fuel: mod=%d burned %d of type %d", addr, modItemID, fuelQty, fuelType)
	return true
}

// notifyModuleRequiresFuel sends the client's own refusal. The message takes
// %(fueled)s (the module) and %(fuel)s (what it wanted), both names.
func notifyModuleRequiresFuel(ctx context.Context, c *evenet.Conn, addr string, accountID,
	modTypeID, fuelType int64, sqldb *sql.DB) {

	nameCtx, nameCancel := context.WithTimeout(ctx, 5*time.Second)
	modName := typeNameOrID(nameCtx, sqldb, modTypeID)
	fuelName := typeNameOrID(nameCtx, sqldb, fuelType)
	nameCancel()
	if err := server.SendNotification(c, accountID, "OnRemoteMessage", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "ModuleRequiresFuel"},
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "fueled"}, Value: marshal.PyString{Value: modName}},
			{Key: marshal.PyString{Value: "fuel"}, Value: marshal.PyString{Value: fuelName}},
		}},
	}}); err != nil {
		log.Printf("[%s] module fuel: ModuleRequiresFuel notify: %v", addr, err)
	}
}

// notifyNotABeacon is the client's own wording for a superweapon pointed at
// something that is not a cynosural field.
func notifyNotABeacon(ctx context.Context, c *evenet.Conn, addr string, accountID, modTypeID int64, sqldb *sql.DB) {
	nameCtx, nameCancel := context.WithTimeout(ctx, 5*time.Second)
	modName := typeNameOrID(nameCtx, sqldb, modTypeID)
	nameCancel()
	if err := server.SendNotification(c, accountID, "OnRemoteMessage", "charid", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "ModuleRequiresCynosuralBeaconTarget"},
		marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "moduleName"}, Value: marshal.PyString{Value: modName}},
			{Key: marshal.PyString{Value: "targetName"}, Value: marshal.PyString{Value: "the target"}},
		}},
	}}); err != nil {
		log.Printf("[%s] doomsday: not-a-beacon notify: %v", addr, err)
	}
}

// surfaceDist is the gap between the surface of a ball of originRadius at
// origin and the hull of e, the client's GetSurfaceDist. Negative when they
// overlap. See docs/combat-notes.md#area-reach.
func surfaceDist(origin destiny.Vec3, originRadius float64, e *destiny.Entity) float64 {
	return e.Pos().Sub(origin).Len() - originRadius - e.BallDesc().Radius
}

// aoeEwarVictims lists the entities whose hull lies within radius of the
// surface of a ball of originRadius at origin on ss, skipping destroyed,
// in-warp, untargetable (IsUntargetable: no lock gate runs for an area module,
// so this is where a GM is spared) and hitpoint-less ones. ss is a parameter
// because a doomsday through a cyno blasts the beacon's grid.
func aoeEwarVictims(ss *destiny.SolarSystem, origin destiny.Vec3, originRadius float64, exclude uint32, radius float64, wholeSystem bool) []*destiny.Entity {
	if ss == nil {
		return nil
	}
	// Candidates only; the distance test below is the filter. A doomsday walks the
	// whole system: GetBubbleEntitiesExcept answers nil for an ID not in ss.
	candidates := ss.GetBubbleEntitiesExcept(exclude)
	if wholeSystem {
		candidates = ss.AllEntities()
	}
	var victims []*destiny.Entity
	for _, e := range candidates {
		if e != nil && e.ID == exclude {
			continue
		}
		if e == nil || e.IsDestroyed() || e.IsInWarp() || e.IsUntargetable() {
			continue
		}
		if !e.HasHP() {
			continue
		}
		// Stations carry HP for conquest, not for target practice.
		if ss.IsStationEntity(e.ID) {
			continue
		}
		if surfaceDist(origin, originRadius, e) > radius {
			continue
		}
		victims = append(victims, e)
	}
	return victims
}

// smartbombVictim applies one blast's damage to a single entity and routes
// what follows: combat log lines, threat, aggression and destruction (player
// pod-out, drone, NPC, plain entity, in that order, as the drone fire path).
func smartbombVictim(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modItemID, modTypeID int64, victim *destiny.Entity, modAttrs map[int64]float64,
	blastSS *destiny.SolarSystem, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	em := modAttrs[dogma.AttrEmDamage] * rates.PlayerDamage
	exp := modAttrs[dogma.AttrExplosiveDamage] * rates.PlayerDamage
	kin := modAttrs[dogma.AttrKineticDamage] * rates.PlayerDamage
	therm := modAttrs[dogma.AttrThermalDamage] * rates.PlayerDamage
	if em+exp+kin+therm <= 0 {
		return
	}
	applied, destroyed := victim.ApplyDamage(em, exp, kin, therm)
	if applied <= 0 && !destroyed {
		return
	}
	targetID := victim.ID

	sendCombatMessage(c, accountID, attackMsgGiven[hitQualityID(1.0)], marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(modTypeID)}},
		{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
		{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
	}})

	if victim.IsPlayer {
		if victim.Conn != nil && !victim.IsInWarp() && !victim.InLandingGrace() {
			sendCombatMessage(victim.Conn, int64(victim.AccountID), attackMsgTaken[hitQualityID(1.0)], marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyString{Value: "source"}, Value: marshal.PyInt{Value: uint64(ds.entity.ID)}},
				{Key: marshal.PyString{Value: "weapon"}, Value: marshal.PyInt{Value: uint64(modTypeID)}},
				{Key: marshal.PyString{Value: "target"}, Value: marshal.PyInt{Value: uint64(targetID)}},
				{Key: marshal.PyString{Value: "damage"}, Value: marshal.PyReal{Value: applied}},
			}})
		}
		victim.AddAggressor(ds.entity.ID, time.Now().Unix())
		victim.RecordKillHit(ds.entity, modTypeID, applied, destroyed)
		recordPlayerAggression(ds.entity, victim, ds.solarSystem)
		maybeTriggerConcordResponse(ds.entity, victim, ds.solarSystem, sqldb, npcMgr)
		sendPlayerHPUpdate(victim)
	} else if npcMgr != nil {
		npcMgr.NotifyDamage(targetID, ds.entity.ID, applied)
		npcMgr.SyncHPToDB(targetID)
		if objID := npcMgr.GetDunObjectID(targetID); objID > 0 && ds.dunObjOwns(targetID) {
			fireDungeonTriggers(c, addr, accountID, ds, "npcDamaged", objID)
		}
	}

	dmgState := victim.DamageStateFrac()
	dmgState.Timestamp = server.Win32TimeNow()
	broadcastDamageStateTo(blastSS, targetID, dmgState)

	if !destroyed {
		return
	}
	log.Printf("[%s] smartbomb: target %d DESTROYED by mod=%d", addr, targetID, modItemID)
	var killerID uint32
	if ds.entity != nil {
		killerID = ds.entity.ID
	}
	// One if/else chain, as the NPC, turret and drone death paths. A module-driven
	// bot is an IsPlayer entity with no Conn and must reach the default arm.
	switch {
	case victim.IsPlayer && victim.Conn != nil && victim.PlayerState != nil:
		queuePlayerDeath(victim.PlayerState.(*destinyConn), victim.Slim.TypeID == 670)
	case globalDroneMgr != nil && globalDroneMgr.HandleDestroyed(targetID, killerID):
		// drone kill handled by the drone manager
	case npcMgr != nil && npcMgr.Kill(targetID, killerID):
		// NPC kill handled by the NPC manager (bounty, loot, removal)
	default:
		// Everything else, including a Conn-less synthetic pilot: remove the
		// entity, which is what the rolebots module watches for.
		npcDestroyEntity(blastSS, victim, killerID)
	}
}

// burstJamVictim rolls one ECM burst attempt against a single entity with the
// targeted-ECM jam-strength roll against the victim's best sensor; range is
// decided by aoeEwarVictims, there is no falloff roll. Nothing is seeded to
// the client: no EffectRepeater runs for an untargeted module, so the
// server's rolls are the only ones drawn.
func burstJamVictim(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modItemID, modTypeID int64, victim *destiny.Entity, durationMs int64,
	modAttrs map[int64]float64, sqldb *sql.DB, npcMgr *npc.NPCManager) {

	// Victim sensor strengths come from its type, exactly as the targeted
	// path reads them via ItemGetInfo.
	tgtAttrs := map[int64]float64{}
	if sqldb != nil {
		taCtx, taCancel := context.WithTimeout(context.Background(), 3*time.Second)
		if a, err := evedb.GetTypeAttributes(taCtx, sqldb, int64(victim.Slim.TypeID)); err == nil {
			tgtAttrs = a
		}
		taCancel()
	}
	var maxA, maxD float64
	havePair := false
	for _, pair := range electronicJamPairs {
		a := modAttrs[pair[0]]
		if a < 1.0 {
			continue
		}
		d := tgtAttrs[pair[1]]
		switch {
		case !havePair || d > maxD:
			maxA, maxD, havePair = a, d, true
		case d == maxD && a > maxA:
			maxA = a
		}
	}
	if havePair && maxD > 0 && rand.Float64() > maxA/maxD {
		log.Printf("[%s] ecm burst: mod=%d MISSED %d (%.1f vs %.1f)", addr, modItemID, victim.ID, maxA, maxD)
		return
	}

	log.Printf("[%s] ecm burst: mod=%d JAMMED %d for %dms", addr, modItemID, victim.ID, durationMs)
	// A burst picks its victims once and never cycles per victim, so the bar it
	// raises is the only one they get and nothing else would ever take it down.
	broadcastJamEvent(ds.solarSystem, ds.entity.ID, modItemID, victim.ID,
		destiny.JamTypeElectronic, durationMs, true)
	scheduleJamEventEnd(ds.solarSystem, ds.entity.ID, modItemID, victim.ID,
		destiny.JamTypeElectronic, durationMs)
	if victim.IsPlayer {
		jamPlayerTargets(victim, durationMs)
		victim.AddAggressor(ds.entity.ID, time.Now().Unix())
		recordPlayerAggression(ds.entity, victim, ds.solarSystem)
		maybeTriggerConcordResponse(ds.entity, victim, ds.solarSystem, sqldb, npcMgr)
	} else if npcMgr != nil {
		npcMgr.JamTargets(victim.ID, time.Now().Add(time.Duration(durationMs)*time.Millisecond))
	}
}

// armModuleReactivation records that modItemID may not be activated again for
// delayMs milliseconds. Called when a cycle ends.
func (ds *destinyConn) armModuleReactivation(modItemID int64, delayMs float64) {
	if ds == nil || modItemID == 0 || delayMs <= 0 {
		return
	}
	ds.moduleReadyMu.Lock()
	defer ds.moduleReadyMu.Unlock()
	if ds.moduleReadyAt == nil {
		ds.moduleReadyAt = make(map[int64]time.Time)
	}
	ds.moduleReadyAt[modItemID] = time.Now().Add(time.Duration(delayMs) * time.Millisecond)
}

// moduleReactivationWait reports how long is left on modItemID's
// moduleReactivationDelay, or zero when it is ready.
func (ds *destinyConn) moduleReactivationWait(modItemID int64) time.Duration {
	if ds == nil || modItemID == 0 {
		return 0
	}
	ds.moduleReadyMu.Lock()
	defer ds.moduleReadyMu.Unlock()
	readyAt, ok := ds.moduleReadyAt[modItemID]
	if !ok {
		return 0
	}
	if wait := time.Until(readyAt); wait > 0 {
		return wait
	}
	// Expired: drop it so the map does not accumulate one entry per module
	// per session.
	delete(ds.moduleReadyAt, modItemID)
	return 0
}

// Nothing clears a delay on unfit: refitting would otherwise reset the hour a
// doomsday owes. Entity itemIDs are never reused, so a surviving entry names
// the same physical module.
