package main

// Research Facility slots: the wire side of db/researchslotdb.go, reached only by a patched
// client (cmd/clientpatch). With research.facilityEnabled off it answers the empty results.
//
// Two bound objects carry the calls, dispatched here by method name:
//   - factoryMgr, Moniker('factory', stationID): GetResearchFacilitySlots, GetFactorySlot,
//     GetFactoryStatus, InstallItemInSlot, EjectItemInSlot. Its first call arrives inline in
//     the bind (computeInlineBindResult routes it here too).
//   - corpStationMgr: the slot-rental quartet; Cancel is always CancelRentOfFactorySlot.
//
// The window never polls, so every change is pushed. See docs/research-facility.md#wire-contract.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"runtime/debug"
	"sync/atomic"
	"time"

	"gurimoon/bugtrack"
	"gurimoon/corp"
	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
	"gurimoon/tick"
)

// researchSlotMethods lists every method handleResearchSlotCall owns.
var researchSlotMethods = map[string]bool{
	// factoryMgr
	"GetResearchFacilitySlots": true,
	"GetFactorySlots":          true,
	"GetFactorySlot":           true,
	"GetFactoryStatus":         true,
	"InstallItemInSlot":        true,
	"EjectItemInSlot":          true,
	// corpStationMgr
	"AreAnyResearchFacilitySlotsAvailableForRenting": true,
	"AreAnyFactorySlotsAvailableForRenting":          true,
	"GetQuoteForRentingAResearchFacilitySlot":        true,
	"GetQuoteForRentingAFactorySlot":                 true,
	"RentResearchFacilitySlot":                       true,
	"RentFactorySlot":                                true,
	"CancelRentOfFactorySlot":                        true,
}

// researchCallEnv is the caller's context for one call.
type researchCallEnv struct {
	c         *evenet.Conn
	addr      string
	accountID int64
	charID    int64
	stationID int64
	shipID    int64
	sqldb     *sql.DB
	sessReg   *esp.SessionRegistry
}

// researchUserError is a refusal the handler raises as a client UserError.
type researchUserError struct {
	key  string
	dict map[string]marshal.PyRep
}

func researchCustomError(msg string) *researchUserError {
	return &researchUserError{key: "CustomError", dict: map[string]marshal.PyRep{
		"error": marshal.PyString{Value: msg},
	}}
}

func rsArgInt(args []marshal.PyRep, i int) int64 {
	if i >= len(args) {
		return 0
	}
	switch v := args[i].(type) {
	case marshal.PyInt:
		return int64(v.Value)
	case marshal.PyBool:
		if v.Value {
			return 1
		}
	case marshal.PyReal:
		return int64(v.Value)
	}
	return 0
}

func rsArgFloat(args []marshal.PyRep, i int) float64 {
	if i >= len(args) {
		return 0
	}
	switch v := args[i].(type) {
	case marshal.PyInt:
		return float64(int64(v.Value))
	case marshal.PyReal:
		return v.Value
	}
	return 0
}

// handleResearchSlotCall answers a bound call (handleBoundCall routes every
// researchSlotMethods name here).
func handleResearchSlotCall(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, stationID *int64, ds *destinyConn, sqldb *sql.DB, sessReg *esp.SessionRegistry) error {
	env := researchCallEnv{c: c, addr: addr, accountID: accountID, charID: charID, sqldb: sqldb, sessReg: sessReg}
	if stationID != nil {
		env.stationID = *stationID
	}
	if ds != nil {
		env.shipID = ds.shipID
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	rep, uerr := researchSlotResult(ctx, env, cs.Method, cs.ArgTuple.Items)
	if uerr != nil {
		return server.SendCallException(c, pkt, accountID, server.BuildUserError(uerr.key, uerr.dict))
	}
	return server.SendCallRsp(c, pkt, accountID, rep)
}

// researchSlotResult is the shared body for the bound and inline paths.
func researchSlotResult(ctx context.Context, env researchCallEnv, method string, args []marshal.PyRep) (marshal.PyRep, *researchUserError) {
	db := env.sqldb
	if db == nil || env.charID == 0 {
		return researchSlotIdle(method), nil
	}
	enabled := evedb.ResearchFacilityEnabled(ctx, db)
	docked := env.stationID != 0
	corpID := boundCharCorpID(env.charID, db)

	switch method {
	case "GetResearchFacilitySlots":
		if !enabled || !docked || !evedb.StationHasLaboratory(ctx, db, env.stationID) {
			return marshal.PyList{}, nil
		}
		slots, err := evedb.ListResearchSlots(ctx, db, env.stationID)
		if err != nil {
			log.Printf("[%s] factory.GetResearchFacilitySlots station=%d: %v", env.addr, env.stationID, err)
			return marshal.PyList{}, nil
		}
		out := make([]marshal.PyRep, 0, len(slots))
		for i := range slots {
			out = append(out, evedb.ResearchSlotRow(&slots[i], env.charID, corpID))
		}
		return marshal.PyList{Items: out}, nil

	case "GetFactorySlot":
		flag := rsArgInt(args, 0)
		if !enabled || !docked || !evedb.IsResearchSlotFlag(flag) {
			return marshal.PyNone{}, nil
		}
		s, err := evedb.GetResearchSlot(ctx, db, env.stationID, flag)
		if err != nil || s == nil {
			return marshal.PyNone{}, nil
		}
		return evedb.ResearchSlotRow(s, env.charID, corpID), nil

	case "GetFactoryStatus":
		itemID := rsArgInt(args, 0)
		if j, err := evedb.GetRunningResearchJobByItem(ctx, db, itemID); err == nil && j != nil &&
			researchJobVisible(ctx, db, j, env.charID, corpID) {
			return evedb.ResearchFactoryStatus(ctx, db, j), nil
		}
		return stubFactoryStatus(), nil

	case "InstallItemInSlot":
		if !enabled || !docked {
			return nil, researchCustomError("The research facility is not available.")
		}
		req := evedb.ResearchInstallRequest{
			StationID: env.stationID, CharID: env.charID, CorpID: corpID, ShipID: env.shipID,
			ItemID: rsArgInt(args, 0), SlotFlag: rsArgInt(args, 1), Runs: rsArgInt(args, 2),
			ActivityID: rsArgInt(args, 3), FlagBOM: rsArgInt(args, 4), FlagOutput: rsArgInt(args, 5),
		}
		res, err := evedb.InstallResearchJob(ctx, db, req)
		if err != nil {
			return nil, researchErr(env, "InstallItemInSlot", err)
		}
		log.Printf("[%s] factory.InstallItemInSlot char=%d station=%d slot=%d %s item=%d type=%d runs=%d chance=%.3f ends in %ds",
			env.addr, env.charID, env.stationID, req.SlotFlag, evedb.ResearchActivityName(res.Job.ActivityID),
			res.Job.HeldItemID, res.Job.ItemTypeID, res.Job.Runs, res.Job.Chance, res.Job.Runs*res.Job.PerRunSeconds)
		sendResearchItemChanges(env.c, env.accountID, res.Changes)
		pushSlotStateChanged(env.sessReg, env.stationID, req.SlotFlag)
		return marshal.PyNone{}, nil

	case "EjectItemInSlot":
		flag := rsArgInt(args, 0)
		if !enabled || !docked || !evedb.IsResearchSlotFlag(flag) {
			return marshal.PyNone{}, nil
		}
		manager := stationFactoryManager(ctx, db, env.stationID, env.charID, corpID)
		job, changes, err := evedb.EjectResearchJob(ctx, db, env.stationID, flag, func(j *evedb.ResearchSlotJob) bool {
			if manager || j.OwnerID == env.charID {
				return true
			}
			return corpID != 0 && j.OwnerID == corpID &&
				(j.InstallerID == env.charID || isDirectorOrCEO(ctx, db, corpID, env.charID))
		})
		if err != nil {
			return nil, researchErr(env, "EjectItemInSlot", err)
		}
		log.Printf("[%s] factory.EjectItemInSlot char=%d station=%d slot=%d job=%d", env.addr, env.charID, env.stationID, flag, job.JobID)
		sendResearchItemChanges(env.c, env.accountID, changes)
		if job.InstallerID != env.charID {
			pushResearchItemChangesTo(env.sessReg, job.InstallerID, env.stationID, changes)
		}
		pushSlotStateChanged(env.sessReg, env.stationID, flag)
		return marshal.PyNone{}, nil

	case "AreAnyResearchFacilitySlotsAvailableForRenting":
		forCorp := rsArgInt(args, 0) != 0
		ok := enabled && docked && evedb.StationHasLaboratory(ctx, db, env.stationID) &&
			evedb.AnyResearchSlotFree(ctx, db, env.stationID)
		if ok && forCorp {
			ok = mayRentResearchForCorp(ctx, db, corpID, env.charID)
		}
		return marshal.PyBool{Value: ok}, nil

	case "GetQuoteForRentingAResearchFacilitySlot":
		return marshal.PyReal{Value: evedb.ResearchSlotRentCost(ctx, db)}, nil

	case "RentResearchFacilitySlot":
		if !enabled || !docked || !evedb.StationHasLaboratory(ctx, db, env.stationID) {
			return marshal.PyNone{}, nil
		}
		offered, forCorp := rsArgFloat(args, 0), rsArgInt(args, 1) != 0
		quote := evedb.ResearchSlotRentCost(ctx, db)
		if offered < quote {
			return nil, researchCustomError(fmt.Sprintf("Renting a slot here now costs %.2f ISK. Please try again.", quote))
		}
		payer := env.charID
		if forCorp {
			if !mayRentResearchForCorp(ctx, db, corpID, env.charID) {
				return nil, researchCustomError("Only a Director, or a member who may rent research slots, can rent a slot for your corporation.")
			}
			payer = corpID
		}
		flag, err := evedb.RentResearchSlot(ctx, db, env.stationID, payer, forCorp, quote, evedb.ResearchSlotRentDays(ctx, db))
		switch {
		case errors.Is(err, evedb.ErrNoFreeResearchSlots):
			return marshal.PyNone{}, nil // client: NoResearchFacilitySlotsAreAvailableForRenting
		case errors.Is(err, evedb.ErrInsufficientFunds):
			who := "You do"
			if forCorp {
				who = "Your corporation does"
			}
			return nil, researchCustomError(fmt.Sprintf("%s not have the %.2f ISK this slot costs.", who, quote))
		case err != nil:
			return nil, researchErr(env, "RentResearchFacilitySlot", err)
		}
		log.Printf("[%s] corpStationMgr.RentResearchFacilitySlot payer=%d station=%d → slot %d (%.0f ISK)",
			env.addr, payer, env.stationID, flag, quote)
		if !forCorp {
			if bal, err := evedb.GetCharacterBalance(ctx, db, env.charID); err == nil {
				pushAccountChange(env.c, env.accountID, env.charID, bal)
			}
		}
		pushSlotRentalChanged(env.sessReg, env.stationID, flag)
		return marshal.PyInt{Value: uint64(flag)}, nil

	case "CancelRentOfFactorySlot":
		flag := rsArgInt(args, 0)
		if !docked || !evedb.IsResearchSlotFlag(flag) {
			return marshal.PyNone{}, nil
		}
		err := evedb.CancelResearchSlotRental(ctx, db, env.stationID, flag, env.charID, corpID,
			mayRentResearchForCorp(ctx, db, corpID, env.charID))
		if err != nil {
			return nil, researchErr(env, "CancelRentOfFactorySlot", err)
		}
		pushSlotRentalChanged(env.sessReg, env.stationID, flag)
		return marshal.PyNone{}, nil
	}
	return researchSlotIdle(method), nil
}

// researchSlotIdle is the empty answer for each method. The factory-slot variants always
// answer it: the legacy factory window is unreachable (the lobby's factories button opens
// Science & Industry).
func researchSlotIdle(method string) marshal.PyRep {
	switch method {
	case "GetResearchFacilitySlots", "GetFactorySlots":
		return marshal.PyList{} // the window len()s it
	case "GetFactoryStatus":
		return stubFactoryStatus()
	case "AreAnyResearchFacilitySlotsAvailableForRenting", "AreAnyFactorySlotsAvailableForRenting":
		return marshal.PyBool{Value: false}
	case "GetQuoteForRentingAResearchFacilitySlot", "GetQuoteForRentingAFactorySlot":
		return marshal.PyInt{Value: 0}
	}
	return marshal.PyNone{}
}

// stubFactoryStatus is a complete, empty status dict: a truthy caller reads
// status['activity'] at once, so a stray call degrades instead of
// KeyErroring the slot display.
func stubFactoryStatus() marshal.PyRep {
	kv := func(k string, v marshal.PyRep) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: v}
	}
	return marshal.PyDict{Items: []marshal.DictEntry{
		kv("activity", marshal.PyInt{Value: 1}), // const.activityManufacturing
		kv("secondsRemaining", marshal.PyNone{}),
		kv("batchSecondsRemaining", marshal.PyNone{}),
		kv("secondsTotal", marshal.PyInt{Value: 0}),
		kv("batchSecondsTotal", marshal.PyInt{Value: 0}),
		kv("iterationsRequested", marshal.PyInt{Value: 0}),
		kv("iterationsCompleted", marshal.PyInt{Value: 0}),
		kv("billOfMaterials", marshal.PyNone{}),
		kv("batchProductionTime", marshal.PyInt{Value: 0}),
		kv("wastageFactor", marshal.PyInt{Value: 0}),
		kv("content", marshal.PyList{}),
	}}
}

// researchErr turns a db error into the client's message: a refusal worded
// for the pilot, the facility window's own IngredientsMissingForBlueprint,
// or a generic failure (logged).
func researchErr(env researchCallEnv, method string, err error) *researchUserError {
	var refusal *evedb.ResearchRefusal
	if errors.As(err, &refusal) {
		return researchCustomError(refusal.Msg)
	}
	var missing *evedb.ErrResearchIngredients
	if errors.As(err, &missing) {
		return &researchUserError{key: "IngredientsMissingForBlueprint", dict: map[string]marshal.PyRep{
			"minerals": marshal.PyString{Value: missing.Minerals},
		}}
	}
	log.Printf("[%s] research facility %s char=%d station=%d: %v", env.addr, method, env.charID, env.stationID, err)
	return researchCustomError("The research facility could not complete that request.")
}

// researchJobVisible: a job's status is shown to whoever may see the slot.
func researchJobVisible(ctx context.Context, db *sql.DB, j *evedb.ResearchSlotJob, charID, corpID int64) bool {
	if j.OwnerID == charID || (corpID != 0 && j.OwnerID == corpID) {
		return true
	}
	return stationFactoryManager(ctx, db, j.StationID, charID, corpID)
}

// mayRentResearchForCorp: a player corporation's CEO, a Director, or a member
// holding Can Rent Research Slot (corpRoleCanRentResearchSlot).
func mayRentResearchForCorp(ctx context.Context, db *sql.DB, corpID, charID int64) bool {
	if corpID == 0 {
		return false
	}
	if npc, _ := evedb.IsNPCCorp(ctx, db, corpID); npc {
		return false
	}
	if isDirectorOrCEO(ctx, db, corpID, charID) {
		return true
	}
	return corp.HasRole(callerRoles(ctx, db, charID), corp.RoleCanRentResearchSlot)
}

// stationFactoryManager: the station owner's own factory manager (or a Director/CEO of the
// owner) may eject anything, per the window's EJECT button (facility.py DisplayFactory).
func stationFactoryManager(ctx context.Context, db *sql.DB, stationID, charID, corpID int64) bool {
	if corpID == 0 {
		return false
	}
	var owner int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM staStations WHERE stationID = ?`, stationID).Scan(&owner); err != nil || owner != corpID {
		return false
	}
	if isDirectorOrCEO(ctx, db, corpID, charID) {
		return true
	}
	return corp.HasRole(callerRoles(ctx, db, charID), corp.RoleFactoryManager)
}

// ── Pushes ───────────────────────────────────────────────────────────────────

func itemChangeArgs(ch evedb.ItemChange) marshal.PyRep {
	items := make([]marshal.DictEntry, 0, len(ch.Change))
	for _, ix := range []int{3, 4, 7} { // stable order
		if v, ok := ch.Change[ix]; ok {
			items = append(items, marshal.DictEntry{
				Key: marshal.PyInt{Value: uint64(ix)}, Value: marshal.PyInt{Value: uint64(v)},
			})
		}
	}
	return marshal.PyTuple{Items: []marshal.PyRep{evedb.BuildEntityRow(ch.Row), marshal.PyDict{Items: items}}}
}

// sendResearchItemChanges pushes OnItemChange for each change on one
// connection.
func sendResearchItemChanges(c *evenet.Conn, accountID int64, changes []evedb.ItemChange) {
	if c == nil {
		return
	}
	for _, ch := range changes {
		if ch.Row == nil {
			continue
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", itemChangeArgs(ch))
	}
}

// pushResearchItemChangesTo pushes item changes to a character docked at the station the items
// are at; the client only tracks the inventories of the station it is in.
func pushResearchItemChangesTo(sessReg *esp.SessionRegistry, charID, stationID int64, changes []evedb.ItemChange) {
	if sessReg == nil || charID == 0 {
		return
	}
	s := sessReg.FindByCharID(charID)
	if s == nil || s.Conn == nil || s.StationID != stationID {
		return
	}
	sendResearchItemChanges(s.Conn, s.AccountID, changes)
}

// pushSlotStateChanged tells every pilot docked at the station to refresh a
// slot (the window ignores it while closed).
func pushSlotStateChanged(sessReg *esp.SessionRegistry, stationID, slotFlag int64) {
	pushToStation(sessReg, stationID, "OnSlotStateChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(stationID)}, marshal.PyInt{Value: uint64(slotFlag)},
		marshal.PyNone{}, marshal.PyNone{},
	}})
}

// pushSlotRentalChanged is OnFactorySlotRentalChanged(slotID) to the station.
func pushSlotRentalChanged(sessReg *esp.SessionRegistry, stationID, slotFlag int64) {
	pushToStation(sessReg, stationID, "OnFactorySlotRentalChanged", marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(slotFlag)},
	}})
}

func pushToStation(sessReg *esp.SessionRegistry, stationID int64, event string, args marshal.PyRep) {
	if sessReg == nil || stationID == 0 {
		return
	}
	for _, s := range sessReg.FindByStationID(stationID) {
		if s.Conn == nil {
			continue
		}
		if err := server.SendNotification(s.Conn, s.AccountID, event, "charid", args); err != nil {
			log.Printf("[%s] %s: %v", s.Addr, event, err)
		}
	}
}

// ── Sweep ────────────────────────────────────────────────────────────────────

var researchSweepRunning atomic.Bool

// startResearchSlotSweep completes due jobs and frees expired rentals every
// 30 s.  Off the tick goroutine, guarded against overlapping itself.
func startResearchSlotSweep(tickMgr *tick.Manager) {
	tickMgr.RegisterPeriodic("research-slot-sweep", 30, func(uint64) bool {
		if globalDB == nil || !researchSweepRunning.CompareAndSwap(false, true) {
			return true
		}
		go func() {
			defer researchSweepRunning.Store(false)
			defer func() {
				if r := recover(); r != nil {
					stack := debug.Stack()
					log.Printf("PANIC in research-slot-sweep: %v\n%s", r, stack)
					bugtrack.ReportPanic("sweepResearchSlots", r, stack)
				}
			}()
			sweepResearchSlots(globalDB, globalSessReg)
		}()
		return true
	})
}

func sweepResearchSlots(db *sql.DB, sessReg *esp.SessionRegistry) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ids, err := evedb.DueResearchJobs(ctx, db)
	if err != nil {
		log.Printf("[research-slot-sweep] %v", err)
		return
	}
	for _, id := range ids {
		done, err := evedb.CompleteResearchJob(ctx, db, id, nil)
		if err != nil {
			log.Printf("[research-slot-sweep] job %d: %v", id, err)
			continue
		}
		if done == nil {
			continue
		}
		j := done.Job
		log.Printf("[research-slot-sweep] job %d %s type=%d: %d/%d succeeded",
			j.JobID, evedb.ResearchActivityName(j.ActivityID), j.ItemTypeID, done.Successes, j.Runs)
		pushResearchItemChangesTo(sessReg, j.InstallerID, j.StationID, done.Changes)
		pushSlotStateChanged(sessReg, j.StationID, j.SlotFlag)
		mailResearchCompletion(ctx, db, done)
	}
	gone, err := evedb.ExpireResearchRentals(ctx, db)
	if err != nil {
		log.Printf("[research-slot-sweep] expire: %v", err)
	}
	for _, r := range gone {
		pushSlotRentalChanged(sessReg, r.StationID, r.Flag)
	}
}

// mailResearchCompletion tells the installer how the job went, the only feedback for a pilot
// who is not watching the slot. The sender is the station's owner.
func mailResearchCompletion(ctx context.Context, db *sql.DB, done *evedb.ResearchCompletion) {
	j := done.Job
	var owner int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM staStations WHERE stationID = ?`, j.StationID).Scan(&owner)
	if owner == 0 {
		owner = j.InstallerID
	}
	var itemName, outName string
	_ = db.QueryRowContext(ctx, `SELECT typeName FROM invTypes WHERE typeID = ?`, j.ItemTypeID).Scan(&itemName)
	_ = db.QueryRowContext(ctx, `SELECT typeName FROM invTypes WHERE typeID = ?`, j.OutputTypeID).Scan(&outName)

	activity := evedb.ResearchActivityName(j.ActivityID)
	subject := fmt.Sprintf("%s complete: %s", activity, itemName)
	where := "your hangar"
	if j.OutputLocationID != j.StationID {
		where = "your corporation's hangar"
	}
	var body string
	switch {
	case done.Successes == 0:
		body = fmt.Sprintf("Your %s job on %s at %s has finished.<br><br>None of the %d run(s) succeeded.",
			activity, itemName, done.StationName, j.Runs)
	default:
		body = fmt.Sprintf("Your %s job on %s at %s has finished.<br><br>%d of %d run(s) succeeded: %d x %s delivered to %s.",
			activity, itemName, done.StationName, done.Successes, j.Runs, done.Successes, outName, where)
	}
	if j.ActivityID == evedb.ActivityDuplicating {
		body += fmt.Sprintf("<br>The original %s has been returned.", itemName)
	}
	created := int64(server.Win32TimeNow())
	msgID, err := evedb.SendEveMail(ctx, db, j.InstallerID, owner, subject, body, created)
	if err != nil {
		log.Printf("[research-slot-sweep] mail job %d: %v", j.JobID, err)
		return
	}
	if msgID != 0 {
		pushMailNotification(j.InstallerID, msgID, owner, subject, created)
	}
}
