package main

// Live side of POS templates (db/starbasetemplatedb.go): after a stamp the
// tower cluster (tower, force field, every structure) must appear on a grid
// that may already hold pilots, and the stamp's GM online option settles
// states the timers would reach later. StarbaseRefreshed rebuilds it.

import (
	"context"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
)

// StarbaseRefreshed implements db.StarbaseRefreshNotifier: rebuild a tower's
// balls (tower, force field when up, and every structure bound to it) in its
// loaded system, swapping balls a pilot already holds and adding new ones.
func (t *slashTeleporter) StarbaseRefreshed(towerID int64) string {
	if t.sqldb == nil || t.sysMgr == nil {
		return ""
	}
	ctx, cancel := context.WithTimeout(context.Background(), starbaseCtxTimeout)
	defer cancel()
	rec, ok, err := evedb.GetStarbase(ctx, t.sqldb, towerID)
	if err != nil || !ok {
		return ""
	}
	ss := t.sysMgr.Get(rec.SolarSystemID)
	if ss == nil {
		return "" // not loaded; the loader builds all of it on first entry
	}
	present := func(id uint32) bool { _, ok := ss.GetStarbaseSlim(id); return ok }

	bd, slim, dmg := starbaseBallSlimDmg(rec)
	if present(bd.EntityID) {
		ss.RemoveStarbase(bd.EntityID)
		ss.AddStarbase(bd, slim, dmg)
		broadcastBallSwap(ss, bd.EntityID, bd, slim, dmg)
	} else {
		ss.AddStarbase(bd, slim, dmg)
		broadcastBallAdd(ss, bd, slim, dmg)
	}
	if rec.State == evedb.StarbaseStateOnline || rec.State == evedb.StarbaseStateReinforced {
		if ffID := starbaseForcefieldID(towerID); !present(ffID) {
			ffBd, ffSlim, ffDmg := starbaseForcefield(t.sqldb, rec)
			ss.AddStarbase(ffBd, ffSlim, ffDmg)
			broadcastBallAdd(ss, ffBd, ffSlim, ffDmg)
		}
	}
	slaves, err := evedb.ListStarbaseStructuresForTower(ctx, t.sqldb, towerID)
	if err != nil {
		log.Printf("[%s] StarbaseRefreshed %d structures: %v", t.addr, towerID, err)
	}
	for _, s := range slaves {
		sbd, sslim, sdmg := starbaseBallSlimDmg(s)
		if present(sbd.EntityID) {
			ss.RemoveStarbase(sbd.EntityID)
			ss.AddStarbase(sbd, sslim, sdmg)
			broadcastBallSwap(ss, sbd.EntityID, sbd, sslim, sdmg)
		} else {
			ss.AddStarbase(sbd, sslim, sdmg)
			broadcastBallAdd(ss, sbd, sslim, sdmg)
		}
	}
	return fmt.Sprintf(" Built live: tower + %d structure(s).", len(slaves))
}

// installStarbaseTemplateESPHooks lets the ESP /starbases page capture and
// stamp templates through the same db ops the slash commands use, with the
// process-wide live notifier.
func installStarbaseTemplateESPHooks() {
	esp.StarbaseTemplateSpawner = func(templateID, moonID, corpID int64, online bool, fuelDays float64) (string, error) {
		if globalDB == nil {
			return "", fmt.Errorf("no database")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		tpl, found, err := evedb.GetStarbaseTemplate(ctx, globalDB, templateID, "")
		if err != nil {
			return "", err
		}
		if !found {
			return "", fmt.Errorf("no template %d", templateID)
		}
		allianceID, _ := evedb.GetCorpAllianceID(ctx, globalDB, corpID)
		res, err := evedb.StampStarbaseTemplate(ctx, globalDB, tpl, evedb.PlantRequest{
			CorpID: corpID, AllianceID: allianceID, MoonID: moonID,
		}, evedb.StampOptions{Online: online, FuelDays: fuelDays}, nil)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Stamped %q at %s: tower %d, %d structure(s), %d link(s).%s",
			tpl.Name, res.Plant.MoonName, res.Plant.EntityID, len(res.Structures), res.Links, res.Live), nil
	}
}
