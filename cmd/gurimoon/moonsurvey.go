package main

// Moon resources: the boot seed of the galaxy's moon pools, and the survey
// probe, the only way a pilot learns what a moon holds. A survey probe (group
// 492) is a charge for a Scan Probe Launcher (group 481); its effect is
// untargeted torpedoLaunching, so the client activates useMissiles with no
// target and the server picks the nearest moon, which must lie within the
// probe's scanRange (attr 765, AU). explosionDelay (attr 281) is the scan
// time; the result is OnMoonScanComplete into the Moon Analysis tab, the
// client's only delivery channel. The pilot must still be there for the
// answer (docking, logging off or leaving loses it), so a pending scan is a
// closure on a tick one-shot and a restart loses it too.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	evedb "gurimoon/db"
	"gurimoon/dogma"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

const (
	groupScanProbeLauncher = 481
	groupSurveyProbe       = 492

	attrSurveyScanRange = 765 // scanRange on a survey probe, in AU

	metersPerAU = 149_597_870_700.0
)

// bootstrapMoonResources rolls every moon with no pool at the current
// generator version: the whole galaxy on a fresh database, a count afterwards.
// Hand-edited moons are never touched.
func bootstrapMoonResources(sqldb *sql.DB) {
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	need, err := evedb.MoonSeedNeeded(ctx, sqldb)
	if err != nil {
		log.Printf("[startup] moon resources: %v", err)
		return
	}
	if !need {
		return
	}
	rep, err := evedb.SeedAllMoons(ctx, sqldb, evedb.MoonSeedOptions{KeepEdited: true, OnlyStale: true})
	if err != nil {
		log.Printf("[startup] moon resources: seeding failed: %v", err)
		return
	}
	log.Printf("[startup] moon resources: rolled %d moon(s) (%d empty, %d material lines, %d kept) in %s",
		rep.Moons, rep.Empty, rep.Resources, rep.Skipped, rep.Took.Round(time.Millisecond))
}

// surveyTimeMult scales every probe's scan time (server setting
// moons.surveyTimeMult, default 1): the playtest lever.
func surveyTimeMult(ctx context.Context, sqldb *sql.DB) float64 {
	v, ok, err := evedb.GetServerSetting(ctx, sqldb, evedb.MoonSettingSurveyTimeMult)
	if err != nil || !ok {
		return 1
	}
	f, perr := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if perr != nil || f <= 0 {
		return 1
	}
	return f
}

// surveyInRange is the aiming rule: the moon's surface within the probe's
// scanRange. A probe with no scanRange reaches 1 AU, which is what all three
// shipped probes carry.
func surveyInRange(distMeters, scanRangeAU float64) bool {
	if scanRangeAU <= 0 {
		scanRangeAU = 1
	}
	return distMeters <= scanRangeAU*metersPerAU
}

// launchSurveyProbe handles a Scan Probe Launcher activation. handled is false
// when the loaded charge is not a survey probe, so the ordinary module path
// keeps everything else (scanner probes included). A non-empty refusal is the
// pilot-facing reason the launch did not happen; nothing was consumed.
func launchSurveyProbe(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
	modItemID, modTypeID, modFlag int64, sqldb *sql.DB) (handled bool, refusal string) {

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if ds == nil || ds.shipID == 0 {
		return false, ""
	}
	lc, err := evedb.GetLoadedCharge(ctx, sqldb, ds.shipID, modFlag)
	if err != nil || lc == nil {
		return false, ""
	}
	if g, _, gerr := evedb.GetTypeGroupCategory(ctx, sqldb, lc.TypeID); gerr != nil || g != groupSurveyProbe {
		return false, ""
	}
	probeName := typeNameOrID(ctx, sqldb, lc.TypeID)

	if ds.solarSystem == nil || ds.entity == nil {
		return true, "Survey probes can only be launched in space."
	}
	if missing := missingFitSkills(ctx, sqldb, charID, lc.TypeID); missing != "" {
		return true, fmt.Sprintf("You need %s to launch a %s.", missing, probeName)
	}

	attrs, _ := evedb.GetTypeAttributes(ctx, sqldb, lc.TypeID)
	pos := ds.entity.Pos()
	moonID, moonName, dist, found, err := evedb.NearestMoon(ctx, sqldb, ds.sysID, pos.X, pos.Y, pos.Z)
	if err != nil {
		log.Printf("[%s] survey probe: NearestMoon: %v", addr, err)
		return true, "The probe could not find a moon to survey."
	}
	if !found {
		return true, "There are no moons in this system to survey."
	}
	scanAU := attrs[attrSurveyScanRange]
	if !surveyInRange(dist, scanAU) {
		if scanAU <= 0 {
			scanAU = 1
		}
		return true, fmt.Sprintf("The nearest moon, %s, is %.1f AU away. Survey probes reach %.0f AU — fly closer to the moon before launching.",
			moonName, dist/metersPerAU, scanAU)
	}

	// Spend the probe.
	if lc.Quantity <= 1 {
		_ = evedb.DeleteEntity(ctx, sqldb, lc.ItemID)
		gone := &evedb.EntityRow{ItemID: lc.ItemID, TypeID: lc.TypeID, OwnerID: charID,
			LocationID: ds.shipID, Flag: modFlag, Quantity: lc.Quantity}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemDeletedNotifArgs(gone))
	} else {
		_ = evedb.UpdateCargoItemQty(ctx, sqldb, lc.ItemID, lc.Quantity-1)
		if upd, _ := evedb.GetEntityRow(ctx, sqldb, lc.ItemID); upd != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(upd),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(lc.Quantity)}}, // ixQuantity
				}},
			}})
		}
	}

	// One launch is one cycle: light the module, then put it out.
	sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, 0, "useMissiles", true, 1000, 1)
	time.AfterFunc(time.Second, func() {
		if !c.IsClosed() {
			sendGodmaShipEffect(c, accountID, modItemID, modTypeID, ds.shipID, charID, 0, "useMissiles", false, 0, 0)
		}
	})

	delayMs := attrs[dogma.AttrExplosionDelay]
	if delayMs <= 0 {
		delayMs = 40 * 60 * 1000
	}
	delay := time.Duration(delayMs*surveyTimeMult(ctx, sqldb)) * time.Millisecond
	sysID := ds.sysID
	log.Printf("[%s] survey probe: char=%d %s → %s (moon %d, %.0f km), result in %s",
		addr, charID, probeName, moonName, moonID, dist/1000, delay.Round(time.Second))
	cynoNotify(c, accountID, fmt.Sprintf("%s launched toward %s. Stay in this system — results in %s.",
		probeName, moonName, fmtSurveyDelay(delay)))

	fire := func() {
		if c.IsClosed() || ds.entity == nil || ds.solarSystem == nil || ds.sysID != sysID {
			log.Printf("[%s] survey probe: char=%d left before %s finished — result lost", addr, charID, moonName)
			return
		}
		fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer fcancel()
		res, err := evedb.GetMoonResources(fctx, sqldb, moonID)
		if err != nil {
			log.Printf("[%s] survey probe: moon %d: %v", addr, moonID, err)
			return
		}
		if len(res) == 0 {
			cynoNotify(c, accountID, fmt.Sprintf("Survey of %s complete: no harvestable materials found.", moonName))
			return
		}
		if err := pushMoonScanResult(c, accountID, moonID, res); err != nil {
			log.Printf("[%s] survey probe: push: %v", addr, err)
		}
	}
	if globalTickMgr != nil {
		globalTickMgr.ScheduleOnce(fmt.Sprintf("moon-survey-%d-%d", charID, moonID), time.Now().Add(delay), fire)
	} else {
		time.AfterFunc(delay, fire)
	}
	return true, ""
}

func fmtSurveyDelay(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d seconds", int(d.Round(time.Second)/time.Second))
	}
	m := int(d.Round(time.Minute) / time.Minute)
	if m == 1 {
		return "1 minute"
	}
	return fmt.Sprintf("%d minutes", m)
}

// pushMoonScanResult delivers a survey to one pilot's Moon Analysis tab as a util.Rowset:
// parklife/moonscan.py indexes it (result[0].celestialID) and ui/inflight/moonscan.py reads
// .typeID/.quantity per row. An empty rowset dies in the client, so callers announce an empty moon
// another way.
func pushMoonScanResult(c *evenet.Conn, accountID, moonID int64, res []evedb.MoonResource) error {
	header := []marshal.PyRep{
		marshal.PyString{Value: "celestialID"},
		marshal.PyString{Value: "typeID"},
		marshal.PyString{Value: "quantity"},
	}
	lines := make([]marshal.PyRep, 0, len(res))
	for _, r := range res {
		lines = append(lines, marshal.PyList{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(moonID)},
			marshal.PyInt{Value: uint64(r.TypeID)},
			marshal.PyInt{Value: uint64(r.Quantity)},
		}})
	}
	rowset := marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			{Key: marshal.PyString{Value: "columns"}, Value: marshal.PyList{Items: header}},
		}},
	}
	return server.SendNotification(c, accountID, "OnMoonScanComplete", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{rowset}})
}
