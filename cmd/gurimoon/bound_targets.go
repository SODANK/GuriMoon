package main

import (
	"context"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	_ "gurimoon/modules"
	"gurimoon/safego"
	"gurimoon/server"
)

// boundAddTarget handles the bound call "AddTarget".
func boundAddTarget(cc *callCtx) error {
	c, addr, cs, pkt, accountID, charID, sqldb, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.charID, cc.sqldb, cc.ds
	if len(cs.ArgTuple.Items) < 1 {
		log.Printf("[%s] bound.AddTarget: missing args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	targetIDVal, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	tid := uint32(targetIDVal.Value)

	if ds.entity != nil && tid == ds.entity.ID {
		// A (0, targetList) reply here would be "already locked": OnTargetAdded on our own ship.
		log.Printf("[%s] bound.AddTarget: cannot target self (%d)", addr, tid)
		return refuseLock(cc, "DeniedTargetSelf", nil)
	}

	// A cloaked pilot cannot lock, except under a GM cloak (a camera tool that must pick a subject).
	if ds.entity != nil && ds.entity.IsCloaked() && !ds.entity.IsGMCloak() {
		return refuseLock(cc, "DeniedTargetingCloaked", nil)
	}

	// cloakingTargetingDelay: no lock for a while after a cloaking device stops.
	if ds.cloakRecalibrating() {
		log.Printf("[%s] bound.AddTarget: refused — sensors recalibrating after a cloak", addr)
		return refuseLock(cc, "DeniedTargetAfterCloak", nil)
	}
	// A cloaked or GM-mode target answers with a UserError, not None.
	// See docs/main-s5-notes.md#target-locks.
	if ds.solarSystem != nil {
		if te := ds.solarSystem.GetEntity(tid); te != nil {
			if te.IsGMMode() {
				log.Printf("[%s] bound.AddTarget: %d is in GM mode, lock refused", addr, tid)
				return refuseLock(cc, "CustomInfo", map[string]marshal.PyRep{
					"info": marshal.PyString{Value: "That pilot cannot be targeted."},
				})
			}
			if te.IsCloaked() {
				// The client's own message for a lock on a ship that is not there.
				return server.SendCallException(c, pkt, accountID, server.BuildUserError(
					"DeniedTargetingTargetCloaked", nil))
			}
		}
	}

	// Refuse what the server cannot resolve to a real object before starting a lock timer.
	// See docs/main-s5-notes.md#target-locks.
	lockPos, lockable := resolveLockTarget(ds, sqldb, tid)
	if !lockable {
		log.Printf("[%s] bound.AddTarget: %d is not a lockable object", addr, tid)
		return refuseLock(cc, "CustomInfo", map[string]marshal.PyRep{
			"info": marshal.PyString{Value: "You cannot lock that object."},
		})
	}

	// ECM jam: no new locks while jammed.
	ds.targetMu.Lock()
	jammed := time.Now().Before(ds.jamUntil)
	ds.targetMu.Unlock()
	if jammed {
		log.Printf("[%s] bound.AddTarget: jammed, lock refused (until %v)", addr, ds.jamUntil)
		jamCtx, jamCancel := context.WithTimeout(context.Background(), 3*time.Second)
		_, jamName := lockTargetLabel(jamCtx, sqldb, ds, tid)
		jamCancel()
		return refuseLock(cc, "TargetingSystemsBeingJammed", map[string]marshal.PyRep{
			"targetname": marshal.PyString{Value: jamName},
		})
	}

	// Read per lock, so a ship swap or a finished skill applies at once.
	capCtx, capCancel := context.WithTimeout(context.Background(), 3*time.Second)
	maxT, charMax := lockedTargetLimits(capCtx, sqldb, charID, ds.shipTypeID)
	capCancel()
	ds.targetMu.Lock()
	if ds.lockingTargets == nil {
		ds.lockingTargets = make(map[uint32]context.CancelFunc)
	}
	for _, existing := range ds.targets {
		if existing == tid {
			ds.targetMu.Unlock()
			log.Printf("[%s] bound.AddTarget: already targeting %d", addr, tid)
			targetList := make([]marshal.PyRep, len(ds.targets))
			for i, t := range ds.targets {
				targetList[i] = marshal.PyInt{Value: uint64(t)}
			}
			return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 0},
				marshal.PyList{Items: targetList},
			}})
		}
	}
	if _, alreadyLocking := ds.lockingTargets[tid]; alreadyLocking {
		// Same (flag, targetList) reply shape as a fresh lock. A None is a TypeError in the
		// client's TryLockTarget, which ends the spinner of the lock still running here.
		targetList := make([]marshal.PyRep, len(ds.targets))
		for i, t := range ds.targets {
			targetList[i] = marshal.PyInt{Value: uint64(t)}
		}
		ds.targetMu.Unlock()
		log.Printf("[%s] bound.AddTarget: already locking %d", addr, tid)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 1},
			marshal.PyList{Items: targetList},
		}})
	}
	// Over the cap the lock is refused with the client's own message. A (1, targetList) reply
	// here would tell TryLockTarget the lock had started, and with no OnTarget ever following,
	// the client would hold a timerless "locking" bracket until relog. Checked after the
	// duplicate cases above so re-locking a held target is not refused at the cap.
	if totalLocking := len(ds.targets) + len(ds.lockingTargets); totalLocking >= maxT {
		ds.targetMu.Unlock()
		log.Printf("[%s] bound.AddTarget: max targets reached (%d/%d), lock on %d refused", addr, totalLocking, maxT, tid)
		key := "TargetingAptitudeAlreadyFullyUtilized" // the pilot's skills are the limit
		if maxT < charMax {
			key = "TargetingSystemsAlreadyFullyUtilized" // the hull is the limit
		}
		return refuseLock(cc, key, map[string]marshal.PyRep{
			"limit": marshal.PyInt{Value: uint64(maxT)},
		})
	}

	// Compute lock time from scan resolution and target signature radius.
	lockDuration := 2 * time.Second
	// The range the lock was allowed at, re-checked when the timer ends. 0 = range not enforced.
	var lockMaxRange float64
	if sqldb != nil && ds.shipTypeID != 0 {
		lockCtx, lockCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer lockCancel()
		shipAttrs, _ := evedb.GetTypeAttributes(lockCtx, sqldb, ds.shipTypeID)

		// Enforce the maximum targeting range: the client offers "lock target" on every overview
		// entry, stargates and stations AUs away included.
		if ds.dm != nil && ds.solarSystem != nil {
			tpos := lockPos
			{
				maxRange := shipAttrs[dogma.AttrMaxTargetRange]
				if maxRange <= 0 {
					maxRange = 25000
				}
				if lvl, lerr := evedb.GetCharacterSkillLevel(lockCtx, sqldb, charID, dogma.SkillLongRangeTargeting); lerr == nil && lvl > 0 {
					lrAttrs, _ := evedb.GetTypeAttributes(lockCtx, sqldb, dogma.SkillLongRangeTargeting)
					if bonus := lrAttrs[dogma.AttrMaxTargetRangeBonus]; bonus > 0 {
						maxRange *= 1.0 + bonus/100.0*float64(lvl)
					}
				}
				// Signal amplifiers (passive, online) and running sensor boosters stretch it, as on the
				// client's own ship. See docs/fitting-notes.md#lock-range.
				if pb, pbErr := evedb.GetShipPassiveBonuses(lockCtx, sqldb, ds.shipID); pbErr == nil && pb.TargetRangeMult > 0 {
					maxRange *= pb.TargetRangeMult
				}
				rangeActive := map[int64]bool{}
				for id, m := range ds.activeModules {
					if m.effectName != "online" {
						rangeActive[id] = true
					}
				}
				if sbRange, sbErr := evedb.GetActiveSensorBoosterRangeMult(lockCtx, sqldb, ds.shipID, rangeActive); sbErr == nil && sbRange > 0 {
					maxRange *= sbRange
				}
				// Incoming sensor dampeners shrink targeting range;
				// Information Warfare in the gang stretches it.
				if ds.entity != nil {
					maxRange *= ds.entity.EwarTotals().MaxTargetRangeMult
					maxRange *= ds.entity.GangBoost().Mult(dogma.GSMaxTargetRange)
				}
				// Targeting hardwirings (and the fitting window's readout).
				maxRange *= evedb.GetCharImplantMults(lockCtx, sqldb, charID).TargetRange
				if dist := tpos.Sub(ds.dm.Pos).Len(); dist > maxRange {
					ds.targetMu.Unlock()
					log.Printf("[%s] bound.AddTarget: target %d out of range (dist=%.0f max=%.0f)", addr, tid, dist, maxRange)
					group, _ := lockTargetLabel(lockCtx, sqldb, ds, tid)
					return refuseLock(cc, "TargetNotWithinRange", map[string]marshal.PyRep{
						"targetGroupName": marshal.PyString{Value: group},
						"desiredRange":    marshal.PyReal{Value: maxRange},
						"actualDistance":  marshal.PyReal{Value: dist},
					})
				}
				lockMaxRange = maxRange
			}
		}

		scanRes := shipAttrs[dogma.AttrScanResolution]
		if scanRes <= 0 {
			scanRes = 200
		}
		// Apply Signature Analysis skill bonus (+5%/level).
		if lvl, err := evedb.GetCharacterSkillLevel(lockCtx, sqldb, charID, dogma.SkillSignatureAnalysis); err == nil && lvl > 0 {
			saAttrs, _ := evedb.GetTypeAttributes(lockCtx, sqldb, dogma.SkillSignatureAnalysis)
			if bonus := saAttrs[dogma.AttrScanResolutionBonus]; bonus > 0 {
				scanRes *= 1.0 + bonus/100.0*float64(lvl)
			}
		}
		// Signal amplifiers (passive online) and running sensor
		// boosters scale scan resolution.
		if pb, pbErr := evedb.GetShipPassiveBonuses(lockCtx, sqldb, ds.shipID); pbErr == nil {
			scanRes *= pb.ScanResMult
		}
		activeIDs := map[int64]bool{}
		for id, m := range ds.activeModules {
			if m.effectName != "online" {
				activeIDs[id] = true
			}
		}
		if sbMult, sbErr := evedb.GetActiveSensorBoosterMult(lockCtx, sqldb, ds.shipID, activeIDs); sbErr == nil {
			scanRes *= sbMult
		}
		// Incoming sensor dampeners slow the lock; Leadership in the gang
		// speeds it.
		if ds.entity != nil {
			scanRes *= ds.entity.EwarTotals().ScanResolutionMult
			scanRes *= ds.entity.GangBoost().Mult(dogma.GSScanResolution)
		}
		// Get target's signature radius.
		var sigRadius float64
		targetEnt := ds.solarSystem.GetEntity(tid)
		if targetEnt != nil && targetEnt.Slim.TypeID != 0 {
			targetAttrs, _ := evedb.GetTypeAttributes(lockCtx, sqldb, targetEnt.Slim.TypeID)
			sigRadius = targetAttrs[dogma.AttrSignatureRadius]
		} else if astTypeID := ds.solarSystem.GetAsteroidTypeID(tid); astTypeID != 0 {
			targetAttrs, _ := evedb.GetTypeAttributes(lockCtx, sqldb, astTypeID)
			sigRadius = targetAttrs[dogma.AttrSignatureRadius]
		}
		if sigRadius <= 0 {
			sigRadius = 100
		}
		// A painted target has a larger signature and locks faster; one under Evasive
		// Maneuvers locks slower.
		if targetEnt != nil {
			sigRadius *= targetEnt.SigRadiusMult()
		}
		lockDuration = computeLockTime(scanRes, sigRadius)
	}

	lockCtxBg, lockCancelBg := context.WithCancel(context.Background())
	ds.lockingTargets[tid] = lockCancelBg
	ds.targetMu.Unlock()

	log.Printf("[%s] → bound.AddTarget targetID=%d lockTime=%v", addr, tid, lockDuration)

	// The reply is held until the lock resolves, as targetMgr.LockTarget expects: it keeps the
	// target in its "targeting" list (the locking bracket) until AddTarget returns or raises,
	// and an OnTarget "lost" never takes it out. Answering at once and cancelling later left
	// that bracket stuck until relog. Every cancel path (RemoveTarget, jam, warp, the target
	// leaving) cancels lockCtxBg, and the goroutine answers that with a refusal.
	req := *pkt
	go func() {
		defer safego.Recover("handleBoundCall")
		fail := func(why, key string, dict map[string]marshal.PyRep) {
			log.Printf("[%s] → bound.AddTarget CANCELLED targetID=%d (%s)", addr, tid, why)
			if key != "" {
				pushRemoteMessage(c, accountID, key, dict)
			}
			_ = clientMsgErr(c, &req, accountID, lockRefusedSilentKey, nil)
		}
		select {
		case <-lockCtxBg.Done():
			fail("lock cancelled", "", nil)
			return
		case <-time.After(lockDuration):
		}
		// Re-validate the target before completing the lock: one destroyed during the timer
		// would leave a ghost entry in the target list. Same resolver as the up-front check.
		dropPending := func() {
			ds.targetMu.Lock()
			if lockCtxBg.Err() == nil { // a cancelled lock's entry may already be a newer lock's
				delete(ds.lockingTargets, tid)
			}
			ds.targetMu.Unlock()
			lockCancelBg()
		}
		if ds.solarSystem == nil || ds.entity == nil {
			dropPending()
			fail("ship left space", "", nil)
			return
		}
		targetEnt := ds.solarSystem.GetEntity(tid)
		tpos, stillLockable := resolveLockTarget(ds, sqldb, tid)
		if !stillLockable {
			dropPending()
			fail("target gone", "", nil)
			return
		}
		// A target that moved out of range during the timer is not locked.
		if lockMaxRange > 0 {
			if dist := tpos.Sub(ds.entity.Pos()).Len(); dist > lockMaxRange {
				dropPending()
				nameCtx, nameCancel := context.WithTimeout(context.Background(), 3*time.Second)
				group, _ := lockTargetLabel(nameCtx, sqldb, ds, tid)
				nameCancel()
				fail("out of range at lock time", "TargetNotWithinRange", map[string]marshal.PyRep{
					"targetGroupName": marshal.PyString{Value: group},
					"desiredRange":    marshal.PyReal{Value: lockMaxRange},
					"actualDistance":  marshal.PyReal{Value: dist},
				})
				return
			}
		}
		ds.targetMu.Lock()
		if lockCtxBg.Err() != nil {
			// Cancelled between the timer firing and here.
			ds.targetMu.Unlock()
			fail("lock cancelled", "", nil)
			return
		}
		delete(ds.lockingTargets, tid)
		lockCancelBg()
		alreadyLocked := false
		for _, existing := range ds.targets {
			if existing == tid {
				alreadyLocked = true
				break
			}
		}
		if !alreadyLocked {
			ds.targets = append(ds.targets, tid)
		}
		nowLocked := ballIDList(ds.targets)
		ds.targetMu.Unlock()
		if alreadyLocked {
			log.Printf("[%s] → bound.AddTarget SKIPPED duplicate targetID=%d", addr, tid)
			_ = server.SendCallRsp(c, &req, accountID, marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyInt{Value: 1}, nowLocked,
			}})
			return
		}
		log.Printf("[%s] → bound.AddTarget LOCKED targetID=%d (now %d targets)", addr, tid, len(nowLocked.Items))
		// The target ball must be in the client's ballpark before the lock notification, or
		// targetMgr.py (bp.GetInvItem) parks the lock in pendingTargets.
		ensureBallsKnown(ds.entity, targetEnt)
		onTargetArgs := marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: "add"},
			marshal.PyInt{Value: uint64(tid)},
			marshal.PyNone{},
		}}
		if err := server.SendNotification(c, accountID, "OnTarget", "charid", onTargetArgs); err != nil {
			log.Printf("[%s] AddTarget OnTarget notification: %v", addr, err)
		}
		// Flag 1: the OnTarget "add" above already went out. A 0 has the client run
		// OnTargetAdded itself as well.
		if err := server.SendCallRsp(c, &req, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 1}, nowLocked,
		}}); err != nil {
			log.Printf("[%s] AddTarget reply: %v", addr, err)
		}
		// Notify the target player that they are being targeted ("otheradd").
		if targetEnt != nil && targetEnt.IsPlayer && targetEnt.Conn != nil {
			onTargetEvent := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "OnTarget"},
				marshal.PyString{Value: "otheradd"},
				marshal.PyInt{Value: uint64(ds.entity.ID)},
				marshal.PyNone{},
			}}
			multiArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyList{Items: []marshal.PyRep{onTargetEvent}},
			}}
			if err := server.SendNotification(targetEnt.Conn, int64(targetEnt.AccountID), "OnMultiEvent", "clientID", multiArgs); err != nil {
				log.Printf("[%s] AddTarget otheradd to %d: %v", addr, tid, err)
			}
		}
	}()
	return nil
}

// lockRefusedSilentKey is the UserError a refused AddTarget raises. targetMgr.LockTarget drops
// this key without a message (and TryLockTarget, used by ctrl-click and the overview, drops
// every error), while still clearing the locking bracket. The reason goes out beforehand as
// an OnRemoteMessage, so it shows once on every lock path.
const lockRefusedSilentKey = "DeniedShipChanged"

// refuseLock refuses an AddTarget with the client's message key and dict.
func refuseLock(cc *callCtx, key string, dict map[string]marshal.PyRep) error {
	pushRemoteMessage(cc.c, cc.accountID, key, dict)
	return clientMsgErr(cc.c, cc.pkt, cc.accountID, lockRefusedSilentKey, nil)
}

// boundRemoveTarget handles the bound call "RemoveTarget".
func boundRemoveTarget(cc *callCtx) error {
	c, addr, cs, pkt, accountID, ds := cc.c, cc.addr, cc.cs, cc.pkt, cc.accountID, cc.ds
	if len(cs.ArgTuple.Items) < 1 {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	rmTargetIDVal, ok := cs.ArgTuple.Items[0].(marshal.PyInt)
	if !ok {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	rmTid := uint32(rmTargetIDVal.Value)
	ds.targetMu.Lock()
	// Cancel pending lock if still locking.
	if cancelFn, ok := ds.lockingTargets[rmTid]; ok {
		cancelFn()
		delete(ds.lockingTargets, rmTid)
	}
	filtered := ds.targets[:0]
	for _, existing := range ds.targets {
		if existing != rmTid {
			filtered = append(filtered, existing)
		}
	}
	ds.targets = filtered
	ds.targetMu.Unlock()
	log.Printf("[%s] → bound.RemoveTarget targetID=%d (now %d targets)", addr, rmTid, len(ds.targets))
	rmOnTargetArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "lost"},
		marshal.PyInt{Value: uint64(rmTid)},
		marshal.PyNone{},
	}}
	if err := server.SendNotification(c, accountID, "OnTarget", "charid", rmOnTargetArgs); err != nil {
		log.Printf("[%s] RemoveTarget OnTarget notification: %v", addr, err)
	}
	// Notify the target player that we stopped targeting them.
	if ds.solarSystem != nil && ds.entity != nil {
		if targetEnt := ds.solarSystem.GetEntity(rmTid); targetEnt != nil && targetEnt.IsPlayer && targetEnt.Conn != nil {
			onTargetEvent := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "OnTarget"},
				marshal.PyString{Value: "otherlost"},
				marshal.PyInt{Value: uint64(ds.entity.ID)},
				marshal.PyNone{},
			}}
			multiArgs := marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyList{Items: []marshal.PyRep{onTargetEvent}},
			}}
			if err := server.SendNotification(targetEnt.Conn, int64(targetEnt.AccountID), "OnMultiEvent", "clientID", multiArgs); err != nil {
				log.Printf("[%s] RemoveTarget otherlost to %d: %v", addr, rmTid, err)
			}
		}
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

// boundClearTargets handles the bound call "ClearTargets".
func boundClearTargets(cc *callCtx) error {
	c, addr, pkt, accountID, ds := cc.c, cc.addr, cc.pkt, cc.accountID, cc.ds
	ds.targetMu.Lock()
	for tid, cancelFn := range ds.lockingTargets {
		cancelFn()
		delete(ds.lockingTargets, tid)
	}
	prevTargets := make([]uint32, len(ds.targets))
	copy(prevTargets, ds.targets)
	ds.targets = nil
	ds.targetMu.Unlock()
	log.Printf("[%s] → bound.ClearTargets", addr)
	clearOnTargetArgs := marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyString{Value: "clear"},
	}}
	if err := server.SendNotification(c, accountID, "OnTarget", "charid", clearOnTargetArgs); err != nil {
		log.Printf("[%s] ClearTargets OnTarget notification: %v", addr, err)
	}
	// Notify all previously locked player targets that we stopped targeting them.
	if ds.solarSystem != nil && ds.entity != nil {
		for _, tid := range prevTargets {
			if targetEnt := ds.solarSystem.GetEntity(tid); targetEnt != nil && targetEnt.IsPlayer && targetEnt.Conn != nil {
				onTargetEvent := marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyString{Value: "OnTarget"},
					marshal.PyString{Value: "otherlost"},
					marshal.PyInt{Value: uint64(ds.entity.ID)},
					marshal.PyNone{},
				}}
				multiArgs := marshal.PyTuple{Items: []marshal.PyRep{
					marshal.PyList{Items: []marshal.PyRep{onTargetEvent}},
				}}
				if err := server.SendNotification(targetEnt.Conn, int64(targetEnt.AccountID), "OnMultiEvent", "clientID", multiArgs); err != nil {
					log.Printf("[%s] ClearTargets otherlost to %d: %v", addr, tid, err)
				}
			}
		}
	}
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}
