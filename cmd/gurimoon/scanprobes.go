package main

// The System Scanner: scanner probes (group 479) fired from a Scan Probe Launcher (group 481)
// into the scanner window's System Scanner tab (ui/inflight/scanner.py, ui/services/scansvc.py).
// Calls: GetScans, CombineScans(probeIDs, groupID) (Analyze), GiveProbe(charID, probeID),
// WarpToStuff('scan', resultID); DoNewProbe is the refresh push. Results belong to the system
// they were taken in. Probes are in memory, so a restart loses them. Each is also a rigid ball on
// the loot-container lifecycle, put back by loadScanProbesIntoSystem when a system reloads.
// See docs/cmd-misc-notes.md#scanner-probes.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"sync"
	"time"

	evedb "gurimoon/db"
	"gurimoon/destiny"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/syncx"
)

const (
	groupScannerProbe = 479

	attrProbeScanRange   = 765 // AU
	attrMinScanDeviation = 787 // m
	attrMaxScanDeviation = 788 // m
	attrProbesInGroup    = 794
	attrScanDuration     = 73 // on the launcher: systemScan duration, ms

	defaultProbeExpiryMs = 10 * 60 * 1000
	defaultScanDuration  = 250_000 // Scan Probe Launcher I, attr 73
	groupStation         = 15

	// Astrometrics: attr 66 durationBonus (-10) per level on the systemScan
	// duration (effect 1409 systemScanDurationSkillAstrometrics).
	skillAstrometrics = 3412
	attrDurationBonus = 66

	// fullSignal is the signal a whole probe group covering a target gives;
	// only that locks a warp to a cosmic signature.
	fullSignal = 100.0
)

// scanHullBonuses are the scanning frigates' shipBonusScanProbeDuration
// effects: each names the hull attribute holding the per-level percentage
// and the racial frigate skill it scales with (Heron, Imicus, Probe).
var scanHullBonuses = []struct{ effectID, attrID, skillID int64 }{
	{1855, 588, 3330}, // CF: shipBonusCF, Caldari Frigate
	{1856, 586, 3328}, // GF: shipBonusGF, Gallente Frigate
	{1857, 587, 3329}, // MF: shipBonusMF, Minmatar Frigate
}

// scanDurationMs folds the skill and hull terms into a launcher's base
// systemScan duration. Both percentages are per level and negative.
func scanDurationMs(baseMs int64, astroPct float64, astroLvl int, hullPct float64, hullLvl int) int64 {
	d := float64(baseMs) * (1 + astroPct/100*float64(astroLvl)) * (1 + hullPct/100*float64(hullLvl))
	if d < 1000 {
		d = 1000
	}
	return int64(d)
}

type scanProbe struct {
	ID          uint32
	TypeID      int64
	OwnerCharID int64
	Pos         destiny.Vec3
	Spec        destiny.ProbeSpec
	Expiry      time.Time
	sysID       int64
}

type probeSystemState struct {
	mu     sync.Mutex
	probes map[uint32]*scanProbe
}

// scanAnalysis is one pilot's latest Analyze: running until done, then its
// results, which WarpToStuff('scan') resolves by resultID. sysID is where the
// results were taken; they mean nothing anywhere else.
type scanAnalysis struct {
	mu      sync.Mutex
	running bool
	sysID   int64
	groupID int64
	results []destiny.ScanResult
	byID    map[uint32]destiny.ScanResult
}

var (
	scanProbeSystems syncx.Map[int64, *probeSystemState] // sysID to *probeSystemState
	scanAnalyses     syncx.Map[int64, *scanAnalysis]     // charID to *scanAnalysis
	scanSeed         int64
	scanSeedMu       sync.Mutex
)

func probeSystem(sysID int64) *probeSystemState {
	v, _ := scanProbeSystems.LoadOrStore(sysID, &probeSystemState{probes: map[uint32]*scanProbe{}})
	return v
}

func charAnalysis(charID int64) *scanAnalysis {
	v, _ := scanAnalyses.LoadOrStore(charID, &scanAnalysis{})
	return v
}

func nextScanSeed() int64 {
	scanSeedMu.Lock()
	defer scanSeedMu.Unlock()
	scanSeed++
	return time.Now().UnixNano() ^ scanSeed
}

// ── launch ──────────────────────────────────────────────────────────────────

// launchScanProbe is the scanner-probe twin of launchSurveyProbe, called from
// the same Activate site once that one has declined the charge. Same
// contract: handled=false means "not mine, carry on"; a refusal is the
// pilot-facing reason and nothing was consumed.
func launchScanProbe(c *evenet.Conn, addr string, accountID, charID int64, ds *destinyConn,
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
	if g, _, gerr := evedb.GetTypeGroupCategory(ctx, sqldb, lc.TypeID); gerr != nil || g != groupScannerProbe {
		return false, ""
	}
	probeName := typeNameOrID(ctx, sqldb, lc.TypeID)

	if ds.solarSystem == nil || ds.entity == nil {
		return true, "Scanner probes can only be launched in space."
	}
	if missing := missingFitSkills(ctx, sqldb, charID, lc.TypeID); missing != "" {
		return true, fmt.Sprintf("You need %s to launch a %s.", missing, probeName)
	}
	attrs, _ := evedb.GetTypeAttributes(ctx, sqldb, lc.TypeID)

	// Spend the probe (one unit of the loaded stack).
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
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(lc.Quantity)}},
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

	expiryMs := attrs[281] // explosionDelay
	if expiryMs <= 0 {
		expiryMs = defaultProbeExpiryMs
	}
	rangeAU := attrs[attrProbeScanRange]
	if rangeAU <= 0 {
		rangeAU = 1
	}
	groupSize := int(attrs[attrProbesInGroup])
	if groupSize < 1 {
		groupSize = 1
	}
	p := &scanProbe{
		ID: destiny.NextProbeID(), TypeID: lc.TypeID, OwnerCharID: charID,
		Pos: ds.entity.Pos(), sysID: ds.sysID,
		Spec: destiny.ProbeSpec{
			RangeM: rangeAU * metersPerAU,
			MinDev: attrs[attrMinScanDeviation], MaxDev: attrs[attrMaxScanDeviation],
			GroupSize: groupSize,
			// The one probesInGroup-1 class is the Observator, which "scans
			// everything outside its scan range".
			Outside: groupSize == 1,
		},
		Expiry: time.Now().Add(time.Duration(expiryMs) * time.Millisecond),
	}
	p.Spec.Pos = p.Pos

	st := probeSystem(ds.sysID)
	st.mu.Lock()
	st.probes[p.ID] = p
	st.mu.Unlock()

	spawnScanProbeBall(ds.solarSystem, sqldb, p)
	pushDoNewProbe(c, accountID, p)
	log.Printf("[%s] scan probe: char=%d %s id=%d range=%.0f AU group=%d expires in %s",
		addr, charID, probeName, p.ID, rangeAU, groupSize, time.Duration(expiryMs)*time.Millisecond)

	expire := func() { expireScanProbe(p) }
	if globalTickMgr != nil {
		globalTickMgr.ScheduleOnce(fmt.Sprintf("scan-probe-%d", p.ID), p.Expiry, expire)
	} else {
		time.AfterFunc(time.Until(p.Expiry), expire)
	}
	return true, ""
}

// spawnScanProbeBall puts the probe in space as a small rigid ball on the
// loot-container lifecycle. Flip scanProbeBalls off if the client turns out
// not to draw a group-479 slim; the registry and the scan work without it.
const scanProbeBalls = true

func scanProbeBallSlim(p *scanProbe) (destiny.BallDesc, destiny.SlimItem) {
	return destiny.BallDesc{
			EntityID: p.ID, Mode: destiny.ModeRigid, SubType: 2,
			Pos: p.Pos, Radius: 5,
		}, destiny.SlimItem{
			BallID: int64(p.ID), ItemID: int64(p.ID),
			TypeID: p.TypeID, GroupID: groupScannerProbe, CategoryID: 8,
			OwnerID: p.OwnerCharID,
		}
}

func spawnScanProbeBall(ss *destiny.SolarSystem, sqldb *sql.DB, p *scanProbe) {
	if !scanProbeBalls || ss == nil {
		return
	}
	bd, slim := scanProbeBallSlim(p)
	spawnLootBall(ss, sqldb, bd, slim)
}

// expireScanProbe drops a probe from the registry and from every grid, then
// tells the owner so the list refreshes.
func expireScanProbe(p *scanProbe) {
	st := probeSystem(p.sysID)
	st.mu.Lock()
	_, live := st.probes[p.ID]
	delete(st.probes, p.ID)
	st.mu.Unlock()
	if !live {
		return
	}
	if scanProbeBalls && globalSysMgr != nil {
		if ss := globalSysMgr.Get(p.sysID); ss != nil && ss.RemoveLootContainer(p.ID) {
			removeLootBallFromGrid(ss, int64(p.ID))
		}
	}
	if conn, acct := charConnLookup(p.OwnerCharID); conn != nil {
		pushDoNewProbe(conn, acct, p)
	}
}

// ── wire shapes ─────────────────────────────────────────────────────────────

var probeRowCols = []string{"probeID", "typeID", "pos", "scanRange", "expiry"}

func probeRow(p *scanProbe) marshal.PyRep {
	return keyValRep(probeRowCols, []marshal.PyRep{
		marshal.PyInt{Value: uint64(p.ID)},
		marshal.PyInt{Value: uint64(p.TypeID)},
		marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyReal{Value: p.Pos.X}, marshal.PyReal{Value: p.Pos.Y}, marshal.PyReal{Value: p.Pos.Z},
		}},
		marshal.PyReal{Value: p.Spec.RangeM}, // metres: the minimap circle's radius
		marshal.PyInt{Value: uint64(evedb.UnixToWin32(p.Expiry.Unix()))},
	})
}

// vector3Rep is a foo.Vector3 the client can subtract its own from.
func vector3Rep(v destiny.Vec3) marshal.PyRep {
	return marshal.PyObject{
		Type: "foo.Vector3",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyReal{Value: v.X}, marshal.PyReal{Value: v.Y}, marshal.PyReal{Value: v.Z},
		}},
	}
}

func pushDoNewProbe(c *evenet.Conn, accountID int64, p *scanProbe) {
	if c == nil {
		return
	}
	if err := server.SendNotification(c, accountID, "DoNewProbe", "charid",
		marshal.PyTuple{Items: []marshal.PyRep{probeRow(p)}}); err != nil {
		log.Printf("DoNewProbe push: %v", err)
	}
}

// ── bound calls ─────────────────────────────────────────────────────────────

func handleGetScans(c *evenet.Conn, addr string, pkt *evenet.PyPacket, accountID, charID int64, ds *destinyConn) error {
	st := probeSystem(ds.sysID)
	st.mu.Lock()
	var probes []marshal.DictEntry
	for _, p := range st.probes {
		if p.OwnerCharID != charID {
			continue
		}
		probes = append(probes, marshal.DictEntry{Key: marshal.PyInt{Value: uint64(p.ID)}, Value: probeRow(p)})
	}
	st.mu.Unlock()

	an := charAnalysis(charID)
	an.mu.Lock()
	results := make([]marshal.PyRep, 0, len(an.results))
	for _, r := range an.results {
		if an.sysID != ds.sysID {
			break // taken in another system
		}
		results = append(results, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: uint64(r.ID)},
			marshal.PyReal{Value: r.Signal},
			vector3Rep(r.Pos),
			marshal.PyReal{Value: r.Accuracy},
		}})
	}
	an.mu.Unlock()

	log.Printf("[%s] → destiny.GetScans charID=%d probes=%d results=%d", addr, charID, len(probes), len(results))
	return server.SendCallRsp(c, pkt, accountID, marshal.PyDict{Items: []marshal.DictEntry{
		{Key: marshal.PyString{Value: "probes"}, Value: marshal.PyDict{Items: probes}},
		{Key: marshal.PyString{Value: "results"}, Value: marshal.PyList{Items: results}},
	}})
}

// handleCombineScans is Analyze. The launcher's systemScan effect runs for
// its duration attribute; the results are computed when it ends, against
// the world as it is THEN, and the effect stop makes the client refresh.
func handleCombineScans(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, sqldb *sql.DB, ds *destinyConn) error {
	none := marshal.PyNone{}
	if len(cs.ArgTuple.Items) < 2 || ds.entity == nil || ds.solarSystem == nil {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	ids := pyIDs(cs.ArgTuple.Items[0])
	var groupID int64
	if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
		groupID = int64(v.Value)
	}

	st := probeSystem(ds.sysID)
	st.mu.Lock()
	var specs []destiny.ProbeSpec
	groupSize := 0
	for _, id := range ids {
		p := st.probes[uint32(id)]
		if p == nil || p.OwnerCharID != charID {
			continue
		}
		if groupSize == 0 {
			groupSize = p.Spec.GroupSize
		} else if p.Spec.GroupSize != groupSize {
			st.mu.Unlock()
			return server.SendCallException(c, pkt, accountID,
				customNotify("Every probe in an analysis must be of the same class."))
		}
		specs = append(specs, p.Spec)
	}
	st.mu.Unlock()
	if len(specs) == 0 {
		return server.SendCallException(c, pkt, accountID, customNotify("Select the probes to analyze first."))
	}
	if len(specs) < groupSize {
		return server.SendCallException(c, pkt, accountID,
			customNotify(fmt.Sprintf("This probe class works in groups of %d.", groupSize)))
	}

	an := charAnalysis(charID)
	an.mu.Lock()
	if an.running {
		an.mu.Unlock()
		return server.SendCallException(c, pkt, accountID, customNotify("An analysis is already running."))
	}
	an.running = true
	an.mu.Unlock()

	// The launcher is what the effect runs on. Any fitted one will do.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	launcherID, launcherType := int64(0), int64(0)
	durationMs := int64(defaultScanDuration)
	if rows, err := evedb.GetShipChildItems(ctx, sqldb, ds.shipID); err == nil {
		for _, r := range rows {
			if r.GroupID == groupScanProbeLauncher && r.Flag >= 11 && r.Flag <= 34 {
				launcherID, launcherType = r.ItemID, r.TypeID
				if attrs, err := evedb.GetTypeAttributes(ctx, sqldb, r.TypeID); err == nil && attrs[attrScanDuration] > 0 {
					durationMs = int64(attrs[attrScanDuration])
				}
				break
			}
		}
	}
	if launcherID != 0 {
		durationMs = scanDurationFor(ctx, sqldb, charID, ds.shipTypeID, durationMs)
	}
	cancel()
	if launcherID == 0 {
		an.mu.Lock()
		an.running = false
		an.mu.Unlock()
		return server.SendCallException(c, pkt, accountID, customNotify("You need a fitted probe launcher to analyze scans."))
	}

	// Taken now, since the timer runs off the connection goroutine and must not read ds. "Still in the
	// system" means the ship is still an entity of it.
	shipID, sysID := ds.shipID, ds.sysID
	ss, selfID, celestials := ds.solarSystem, ds.entity.ID, ds.celestials
	sendGodmaShipEffect(c, accountID, launcherID, launcherType, shipID, charID, 0, "systemScan", true, durationMs, 1)
	if err := server.SendCallRsp(c, pkt, accountID, none); err != nil {
		return err
	}
	log.Printf("[%s] → destiny.CombineScans charID=%d probes=%d group=%d, results in %s",
		addr, charID, len(specs), groupID, time.Duration(durationMs)*time.Millisecond)

	finish := func() {
		an.mu.Lock()
		an.running = false
		an.mu.Unlock()
		if c.IsClosed() {
			return
		}
		if ss.GetEntity(selfID) != nil {
			targets := scanTargets(ss, sysID, selfID, celestials, groupID)
			results := destiny.CombineProbeScan(specs, targets, nextScanSeed())
			an.mu.Lock()
			an.sysID, an.groupID = sysID, groupID
			an.results = results
			an.byID = make(map[uint32]destiny.ScanResult, len(results))
			for _, r := range results {
				an.byID[r.ID] = r
			}
			an.mu.Unlock()
			log.Printf("[%s] scan analysis charID=%d group=%d: %d result(s)", addr, charID, groupID, len(results))
		}
		sendGodmaShipEffect(c, accountID, launcherID, launcherType, shipID, charID, 0, "systemScan", false, 0, 0)
	}
	at := time.Now().Add(time.Duration(durationMs) * time.Millisecond)
	if globalTickMgr != nil {
		globalTickMgr.ScheduleOnce(fmt.Sprintf("scan-analyze-%d", charID), at, finish)
	} else {
		time.AfterFunc(time.Until(at), finish)
	}
	return nil
}

// scanDurationFor is a launcher's systemScan duration for this pilot and
// hull: Astrometrics always, the scanning frigates' bonus when the hull
// carries one of those effects. A failed lookup leaves that term out.
func scanDurationFor(ctx context.Context, sqldb *sql.DB, charID, shipTypeID, baseMs int64) int64 {
	astroPct, hullPct := 0.0, 0.0
	astroLvl, hullLvl := 0, 0
	if lvl, err := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, skillAstrometrics); err == nil && lvl > 0 {
		if sa, err := evedb.GetTypeAttributes(ctx, sqldb, skillAstrometrics); err == nil {
			astroPct, astroLvl = sa[attrDurationBonus], lvl
		}
	}
	if shipTypeID > 0 {
		if effs, err := evedb.GetTypeEffects(ctx, sqldb, shipTypeID); err == nil {
			for _, b := range scanHullBonuses {
				if !typeHasEffect(effs, b.effectID) {
					continue
				}
				lvl, lerr := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, b.skillID)
				hattrs, aerr := evedb.GetTypeAttributes(ctx, sqldb, shipTypeID)
				if lerr == nil && aerr == nil && lvl > 0 {
					hullPct, hullLvl = hattrs[b.attrID], lvl
				}
				break
			}
		}
	}
	return scanDurationMs(baseMs, astroPct, astroLvl, hullPct, hullLvl)
}

func typeHasEffect(effs []evedb.TypeEffect, effectID int64) bool {
	for _, e := range effs {
		if e.EffectID == effectID {
			return true
		}
	}
	return false
}

// scanTargets is what an Analyze against groupID can find: balls of that group in the system bar
// the scanning ship, cloaked ships and ships mid-warp; stations (group 15) from the celestial list;
// cosmic signatures (group 502) as the live probe-only open sites, by marker ball. Landing grace
// and GM mode do not hide a ship from probes.
func scanTargets(ss *destiny.SolarSystem, sysID int64, selfID uint32,
	celestials []evedb.CelestialEntity, groupID int64) []destiny.ScanTarget {
	var out []destiny.ScanTarget
	switch groupID {
	case groupStation:
		for _, cel := range celestials {
			if cel.GroupID == groupStation {
				out = append(out, destiny.ScanTarget{ID: uint32(cel.ItemID), Pos: destiny.Vec3{X: cel.X, Y: cel.Y, Z: cel.Z}})
			}
		}
		return out
	case destiny.GroupCosmicSignature:
		return signatureTargets(openSitesSnapshot(), sysID)
	}
	for _, e := range ss.AllEntities() {
		if e == nil || e.ID == selfID || e.Slim.GroupID != groupID {
			continue
		}
		if e.IsCloaked() || e.IsInWarp() {
			continue
		}
		out = append(out, destiny.ScanTarget{ID: e.ID, Pos: e.Pos()})
	}
	return out
}

// signatureTargets lists the uncleared probe-only open sites in a system by
// their marker ball ID: the ID WarpToStuff('scan') hands back, which
// beaconWarpArrival turns into the site's room-1 arrival.
func signatureTargets(sites []*openSite, sysID int64) []destiny.ScanTarget {
	var out []destiny.ScanTarget
	for _, s := range sites {
		if s.row.SolarSystemID != sysID || s.row.BeaconVisible || s.isGone() {
			continue
		}
		out = append(out, destiny.ScanTarget{ID: s.ballID, Pos: destiny.Vec3{X: s.row.X, Y: s.row.Y, Z: s.row.Z}})
	}
	return out
}

// handleGiveProbe hands one of the caller's probes to a gang mate in the
// same system.
func handleGiveProbe(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, ds *destinyConn) error {
	none := marshal.PyNone{}
	if len(cs.ArgTuple.Items) < 2 {
		return server.SendCallRsp(c, pkt, accountID, none)
	}
	var toCharID, probeID int64
	if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
		toCharID = int64(v.Value)
	}
	if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
		probeID = int64(v.Value)
	}
	toConn, toAcct := charConnLookup(toCharID)
	if toConn == nil {
		return server.SendCallException(c, pkt, accountID, customNotify("That pilot is not online."))
	}
	if globalGangReg == nil || globalGangReg.GetCharGang(charID) == 0 ||
		globalGangReg.GetCharGang(charID) != globalGangReg.GetCharGang(toCharID) {
		return server.SendCallException(c, pkt, accountID, customNotify("You can only give probes to a member of your gang."))
	}
	inSystem := false
	if ds.solarSystem != nil {
		for _, e := range ds.solarSystem.GetAllPlayerEntities() {
			if e != nil && e.Slim.CharID == toCharID {
				inSystem = true
				break
			}
		}
	}
	if !inSystem {
		return server.SendCallException(c, pkt, accountID, customNotify("That pilot is not in this solar system."))
	}
	st := probeSystem(ds.sysID)
	st.mu.Lock()
	p := st.probes[uint32(probeID)]
	if p == nil || p.OwnerCharID != charID {
		st.mu.Unlock()
		return server.SendCallException(c, pkt, accountID, customNotify("That is not one of your probes."))
	}
	p.OwnerCharID = toCharID
	st.mu.Unlock()
	// The ball's slim carries the owner too: re-present it under the new one.
	if scanProbeBalls && ds.solarSystem != nil && ds.solarSystem.RemoveLootContainer(p.ID) {
		removeLootBallFromGrid(ds.solarSystem, int64(p.ID))
		spawnScanProbeBall(ds.solarSystem, nil, p)
	}
	log.Printf("[%s] → destiny.GiveProbe %d: char %d → %d", addr, probeID, charID, toCharID)
	if err := server.SendCallRsp(c, pkt, accountID, none); err != nil {
		return err
	}
	pushDoNewProbe(c, accountID, p)
	pushDoNewProbe(toConn, toAcct, p)
	return nil
}

// warpTargetForScan resolves WarpToStuff('scan', resultID) to the point the pilot's last analysis
// in this system reported. A cosmic signature lands on the site's marker, and only at full signal.
// refusal is the pilot-facing reason when the result exists but cannot be warped to.
func warpTargetForScan(charID, sysID, resultID int64) (pos destiny.Vec3, ok bool, refusal string) {
	v, found := scanAnalyses.Load(charID)
	if !found {
		return destiny.Vec3{}, false, ""
	}
	an := v
	an.mu.Lock()
	r, have := an.byID[uint32(resultID)]
	resSys, group := an.sysID, an.groupID
	an.mu.Unlock()
	if !have || resSys != sysID {
		return destiny.Vec3{}, false, ""
	}
	if group != destiny.GroupCosmicSignature {
		return r.Pos, true, ""
	}
	if r.Signal < fullSignal {
		return destiny.Vec3{}, false, "Signal too weak to lock a warp. Analyze with full probe coverage."
	}
	site := openSiteByBall(uint32(resultID))
	if site == nil || site.isGone() {
		return destiny.Vec3{}, false, "That signature has faded."
	}
	return destiny.Vec3{X: site.row.X, Y: site.row.Y, Z: site.row.Z}, true, ""
}

// loadScanProbesIntoSystem puts a freshly loaded system's live probes back
// in space. The registry outlives the system (a SolarSystem is torn down when
// its last pilot leaves); the ball does not.
func loadScanProbesIntoSystem(ss *destiny.SolarSystem, sysID int64) {
	if !scanProbeBalls || ss == nil {
		return
	}
	v, ok := scanProbeSystems.Load(sysID)
	if !ok {
		return
	}
	st := v
	now := time.Now()
	st.mu.Lock()
	live := make([]*scanProbe, 0, len(st.probes))
	for _, p := range st.probes {
		if p.Expiry.After(now) {
			live = append(live, p)
		}
	}
	st.mu.Unlock()
	for _, p := range live {
		bd, slim := scanProbeBallSlim(p)
		ss.AddLootContainer(bd, slim)
	}
}
