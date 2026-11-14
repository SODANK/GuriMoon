package main

// Weapon reload delay. The RMR client blinks a module and shows "changing ammo" or
// "reloading ammo" only while its ReplaceCharges or Add call is open, so a reload
// lasts as long as the server holds that reply. See docs/combat-notes.md#weapon-reload.

import (
	"context"
	"database/sql"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// Turret groupIDs (invGroups) with an in-space reload time.
const (
	groupEnergyWeapon     int64 = 53
	groupProjectileWeapon int64 = 55
	groupHybridWeapon     int64 = 74
)

// Launcher groupIDs not covered by the dogma.GroupLauncher* constants.
const (
	groupLauncherSnowball int64 = 501
	groupLauncherDefender int64 = 512
	groupLauncherCitadel  int64 = 524
)

// Frequency crystal charge groups: a turret cycle never uses one up.
const (
	groupFrequencyCrystal     int64 = 86
	groupAdvancedBeamCrystal  int64 = 374
	groupAdvancedPulseCrystal int64 = 375
)

// isFrequencyCrystalGroup reports whether a charge group is a laser crystal.
func isFrequencyCrystalGroup(groupID int64) bool {
	return groupID == groupFrequencyCrystal || groupID == groupAdvancedBeamCrystal ||
		groupID == groupAdvancedPulseCrystal
}

// reloadDelayFor is the in-space reload time of a module group, read from the
// weapons.reloadSeconds.* settings. Groups not listed load instantly.
func reloadDelayFor(groupID int64) time.Duration {
	switch groupID {
	case groupProjectileWeapon:
		return evedb.CachedSettingDuration("weapons.reloadSeconds.projectile", time.Second, 10, 0, 300)
	case groupHybridWeapon:
		return evedb.CachedSettingDuration("weapons.reloadSeconds.hybrid", time.Second, 5, 0, 300)
	case groupEnergyWeapon:
		return evedb.CachedSettingDuration("weapons.reloadSeconds.laser", time.Second, 0, 0, 300)
	case groupLauncherSnowball, dogma.GroupLauncherCruise, dogma.GroupLauncherRocket,
		dogma.GroupLauncherSiege, dogma.GroupLauncherStandard, dogma.GroupLauncherHeavy,
		dogma.GroupLauncherAssault, groupLauncherDefender, groupLauncherCitadel:
		return evedb.CachedSettingDuration("weapons.reloadSeconds.launcher", time.Second, 10, 0, 300)
	case groupCapacitorBooster:
		return evedb.CachedSettingDuration("weapons.reloadSeconds.capBooster", time.Second, 10, 0, 300)
	}
	return 0
}

// pendingReload is one held charge-load reply. Every module it covers points at
// the same value, so the reply goes out once.
type pendingReload struct {
	until time.Time
	timer *time.Timer
	reply func()
	done  bool
}

// reloadWait reports how long modItemID has left to reload, or zero when it is
// not reloading.
func (ds *destinyConn) reloadWait(modItemID int64) time.Duration {
	if ds == nil || modItemID == 0 {
		return 0
	}
	ds.reloadMu.Lock()
	defer ds.reloadMu.Unlock()
	pr, ok := ds.reloads[modItemID]
	if !ok {
		return 0
	}
	if wait := time.Until(pr.until); wait > 0 && !pr.done {
		return wait
	}
	delete(ds.reloads, modItemID)
	return 0
}

// beginReload marks mods as reloading for d and calls reply once d has passed,
// from a timer goroutine. reply must not touch the connection-owned module maps.
func (ds *destinyConn) beginReload(mods []int64, d time.Duration, reply func()) {
	pr := &pendingReload{until: time.Now().Add(d), reply: reply}
	ds.reloadMu.Lock()
	if ds.reloads == nil {
		ds.reloads = make(map[int64]*pendingReload)
	}
	for _, m := range mods {
		ds.reloads[m] = pr
	}
	pr.timer = time.AfterFunc(d, func() { ds.finishReload(pr, true) })
	ds.reloadMu.Unlock()
}

// finishReload ends pr once: it drops every module entry that points at pr and,
// when send is true, calls the reply.
func (ds *destinyConn) finishReload(pr *pendingReload, send bool) {
	ds.reloadMu.Lock()
	if pr.done {
		ds.reloadMu.Unlock()
		return
	}
	pr.done = true
	for m, p := range ds.reloads {
		if p == pr {
			delete(ds.reloads, m)
		}
	}
	ds.reloadMu.Unlock()
	if send && pr.reply != nil {
		pr.reply()
	}
}

// takeReloads stops every pending timer and returns the pending reloads, each once.
func (ds *destinyConn) takeReloads() []*pendingReload {
	if ds == nil {
		return nil
	}
	ds.reloadMu.Lock()
	seen := make(map[*pendingReload]bool)
	var out []*pendingReload
	for _, pr := range ds.reloads {
		if !seen[pr] && !pr.done {
			seen[pr] = true
			pr.timer.Stop()
			out = append(out, pr)
		}
	}
	ds.reloadMu.Unlock()
	return out
}

// finishReloadsNow completes every reload at once and sends its reply. Called on
// dock and on ship or pod death.
func (ds *destinyConn) finishReloadsNow() {
	for _, pr := range ds.takeReloads() {
		ds.finishReload(pr, true)
	}
}

// dropReloads cancels every reload without replying. Called on disconnect.
func (ds *destinyConn) dropReloads() {
	for _, pr := range ds.takeReloads() {
		ds.finishReload(pr, false)
	}
}

// answerChargeLoad replies to a charge load into mods. Docked, or for a group with
// no reload time, it answers now; in space it holds the reply for the reload.
func answerChargeLoad(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, ds *destinyConn,
	stationID int64, mods []int64, d time.Duration) error {
	if stationID != 0 || d <= 0 || len(mods) == 0 || ds == nil {
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	ds.beginReload(mods, d, func() {
		if !c.IsClosed() {
			_ = server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
	})
	return nil
}

// refuseBusyCharge answers an unload from modER with a client refusal while the
// module is reloading or firing. refused is false when the unload may go ahead.
func refuseBusyCharge(ctx context.Context, c *evenet.Conn, pkt *evenet.PyPacket, accountID int64,
	ds *destinyConn, modER *evedb.EntityRow, sqldb *sql.DB) (refused bool, err error) {
	if ds.reloadWait(modER.ItemID) > 0 {
		return true, clientMsgErr(c, pkt, accountID, "AlreadyBeingReloaded", map[string]marshal.PyRep{
			"item": marshal.PyString{Value: typeNameOrID(ctx, sqldb, modER.TypeID)}})
	}
	if ds.weaponCycling(modER.ItemID) {
		return true, clientMsgErr(c, pkt, accountID, "CannotAccessChargeWhileInUse", nil)
	}
	return false, nil
}

// weaponCycling reports whether modItemID has a weapon cycle running.
func (ds *destinyConn) weaponCycling(modItemID int64) bool {
	_, ok := ds.cancelWeapon[modItemID]
	return ok
}

// stopWeaponForReload ends a running weapon cycle on modItemID before its charge
// changes, sending the same stop effect as a cycle that ends by itself.
func stopWeaponForReload(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn, modItemID int64, sqldb *sql.DB) {
	cancelFn, ok := ds.cancelWeapon[modItemID]
	if !ok {
		return
	}
	cancelFn()
	var typeID int64
	var targetID uint32
	effectName := ""
	if mod, ok := ds.activeModules[modItemID]; ok {
		typeID, targetID, effectName = mod.typeID, mod.targetID, mod.effectName
	}
	stopWeaponFX(c, addr, accountID, charID, ds, modItemID, typeID, targetID, effectName, sqldb)
}
