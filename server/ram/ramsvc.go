// Package ram implements the "ram" service: industry jobs, assembly lines and
// blueprints for Research & Manufacturing.
package ram

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/rates"
	"gurimoon/server"
)

// Handle dispatches a CallReq addressed to the "ram" service. spaceRegionID is the
// ship's region while undocked; the Installations tab works in space, so lookups
// cannot key on the docked station alone.
func Handle(c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID, stationID, spaceRegionID int64, sqldb *sql.DB) error {

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	switch cs.Method {
	case "AssemblyLinesSelect":
		return handleAssemblyLinesSelect(ctx, c, addr, cs, pkt, accountID, charID, stationID, spaceRegionID, sqldb)
	case "AssemblyLinesGet":
		return handleAssemblyLinesGet(ctx, c, addr, cs, pkt, accountID, stationID, sqldb)
	case "InstallJob":
		return handleInstallJob(ctx, c, addr, cs, pkt, accountID, charID, stationID, sqldb)
	case "CompleteJob":
		return handleCompleteJob(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetJobs2":
		return handleGetJobs2(ctx, c, addr, cs, pkt, accountID, charID, sqldb)
	case "GetInstallationDetails":
		// manufacturing.py wraps this in a pass-through with no callers; None is safe.
		log.Printf("[%s] → ram.GetInstallationDetails (None, dead client path)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	case "UpdateAssemblyLineConfigurations":
		// Station-owner assembly-line pricing editor. Player-owned installations do not
		// exist, so this acknowledges without persisting; the client re-queries the lines.
		log.Printf("[%s] → ram.UpdateAssemblyLineConfigurations (no-op)", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	default:
		server.RecordUnhandled(addr, "ram", cs, true)
		log.Printf("[%s] ram: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

func handleAssemblyLinesSelect(ctx context.Context, c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID, spaceRegionID int64,
	sqldb *sql.DB) error {

	// Args: filter (string: "region", "char", "corp", "alliance").
	filter := "char"
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyString); ok {
			filter = v.Value
		}
	}

	// The Installations tab reloads on every solarsystemid/regionid session change,
	// so the scope is the docked station or, undocked, the ship's region.
	regionID := spaceRegionID
	if stationID != 0 {
		if rid, rerr := evedb.GetRegionForStation(ctx, sqldb, stationID); rerr != nil {
			log.Printf("[%s] ram.AssemblyLinesSelect: can't resolve region for station %d: %v", addr, stationID, rerr)
		} else {
			regionID = rid
		}
	}

	// Station scope applies only while docked; the region is the narrowest scope in
	// space. Player-owned installations do not exist, so every filter answers the public set.
	selectLocal := func() (marshal.PyRep, error) {
		if stationID != 0 {
			return evedb.GetAssemblyLinesSelectChar(ctx, sqldb, stationID)
		}
		if regionID != 0 {
			return evedb.GetAssemblyLinesSelectRegion(ctx, sqldb, regionID)
		}
		return evedb.GetAssemblyLinesSelectChar(ctx, sqldb, 0)
	}

	var result marshal.PyRep
	var err error

	switch filter {
	case "region":
		if regionID == 0 {
			log.Printf("[%s] ram.AssemblyLinesSelect region: no region for station %d / ship, falling back", addr, stationID)
			result, err = selectLocal()
			if err != nil {
				log.Printf("[%s] ram.AssemblyLinesSelect region fallback: %v", addr, err)
			}
		} else {
			result, err = evedb.GetAssemblyLinesSelectRegion(ctx, sqldb, regionID)
			if err != nil {
				log.Printf("[%s] ram.AssemblyLinesSelect region=%d: %v", addr, regionID, err)
			}
			log.Printf("[%s] → ram.AssemblyLinesSelect filter=%q regionID=%d", addr, filter, regionID)
		}
	case "corp", "alliance":
		// Corporation installations are the starbase arrays and labs the corp or alliance
		// owns; with none, the station set is returned.
		if r, ok := posAssemblyLinesFor(ctx, sqldb, charID, filter == "alliance"); ok {
			log.Printf("[%s] → ram.AssemblyLinesSelect filter=%q (starbase installations)", addr, filter)
			return server.SendCallRsp(c, pkt, accountID, r)
		}
		result, err = selectLocal()
		if err != nil {
			log.Printf("[%s] ram.AssemblyLinesSelect %s: %v", addr, filter, err)
		}
		log.Printf("[%s] → ram.AssemblyLinesSelect filter=%q stationID=%d regionID=%d (fallback)", addr, filter, stationID, regionID)
	default: // "char" or unknown
		result, err = selectLocal()
		if err != nil {
			log.Printf("[%s] ram.AssemblyLinesSelect char: %v", addr, err)
		}
		log.Printf("[%s] → ram.AssemblyLinesSelect filter=%q stationID=%d regionID=%d", addr, filter, stationID, regionID)
	}

	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleAssemblyLinesGet(ctx context.Context, c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket, accountID, stationID int64, sqldb *sql.DB) error {

	containerID := stationID
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			containerID = int64(v.Value)
		}
	}

	result, err := evedb.GetAssemblyLinesForStation(ctx, sqldb, containerID)
	if err != nil {
		log.Printf("[%s] ram.AssemblyLinesGet: %v", addr, err)
	}
	log.Printf("[%s] → ram.AssemblyLinesGet containerID=%d", addr, containerID)
	return server.SendCallRsp(c, pkt, accountID, result)
}

func handleInstallJob(ctx context.Context, c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID, stationID int64, sqldb *sql.DB) error {

	// The Blood-build client sends location-data based args:
	//   arg[0]: installationLocationData  (PyList: [[stationID, groupID], [path...], [stationID, assemblyLineID]])
	//   arg[1]: installedItemLocationData (PyList: [[stationID, groupID], [[stationID, ownerID, flag]], [blueprintItemID]])
	//   arg[2]: bomLocationData           (PyList: ignored)
	//   arg[3]: flagOutput                (PyInt)
	//   arg[4]: buildRuns                 (PyInt)
	//   arg[5]: activityID               (PyInt)
	//   arg[6]: licensedProductionRuns    (PyInt)
	//   arg[7]: isCorpJob                 (PyBool)
	//   arg[8]: description              (PyString)
	if len(cs.ArgTuple.Items) < 6 {
		log.Printf("[%s] ram.InstallJob: insufficient args (%d)", addr, len(cs.ArgTuple.Items))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	// Extract assemblyLineID from installationLocationData[2] (last element of inner list).
	var assemblyLineID int64
	if instLocData, ok := cs.ArgTuple.Items[0].(marshal.PyList); ok && len(instLocData.Items) >= 3 {
		if pathList, ok := instLocData.Items[2].(marshal.PyList); ok && len(pathList.Items) >= 2 {
			if v, ok := pathList.Items[len(pathList.Items)-1].(marshal.PyInt); ok {
				assemblyLineID = int64(v.Value)
			}
		}
	}

	// Extract blueprintID from installedItemLocationData[2] (first element of inner list).
	var blueprintID int64
	if itemLocData, ok := cs.ArgTuple.Items[1].(marshal.PyList); ok && len(itemLocData.Items) >= 3 {
		if idList, ok := itemLocData.Items[2].(marshal.PyList); ok && len(idList.Items) >= 1 {
			if v, ok := idList.Items[0].(marshal.PyInt); ok {
				blueprintID = int64(v.Value)
			}
		}
	}

	if assemblyLineID == 0 || blueprintID == 0 {
		log.Printf("[%s] ram.InstallJob: failed to extract assemblyLineID=%d or blueprintID=%d from location data", addr, assemblyLineID, blueprintID)
		for i, a := range cs.ArgTuple.Items {
			log.Printf("[%s]   arg[%d]: %T %+v", addr, i, a, a)
		}
		if assemblyLineID == 0 {
			return sendRamMessage(c, pkt, accountID, "RamPleasePickAnInstalltion", nil)
		}
		return sendRamMessage(c, pkt, accountID, "RamPleasePickAnItemToInstall", nil)
	}

	// Positional ints: flagOutput(3), runs(4), activityID(5).
	runsRep, ok1 := cs.ArgTuple.Items[4].(marshal.PyInt)
	actRep, ok2 := cs.ArgTuple.Items[5].(marshal.PyInt)
	if !ok1 || !ok2 {
		log.Printf("[%s] ram.InstallJob: bad runs/activity arg types", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	runs := int(runsRep.Value)
	activityID := int(actRep.Value)

	// Extract licensedProductionRuns from arg[6] and quoteOnly from kwargs.
	var licensedProductionRuns int64
	if len(cs.ArgTuple.Items) >= 7 {
		if v, ok := cs.ArgTuple.Items[6].(marshal.PyInt); ok {
			licensedProductionRuns = int64(v.Value)
		}
	}
	var isCorpJob bool
	if len(cs.ArgTuple.Items) >= 8 {
		switch v := cs.ArgTuple.Items[7].(type) {
		case marshal.PyBool:
			isCorpJob = v.Value
		case marshal.PyInt:
			isCorpJob = v.Value != 0
		}
	}
	var quoteOnly bool
	var authorizedCost float64
	var haveAuthorizedCost bool
	var maxJobStartTime int64
	for _, entry := range cs.ArgDict.Items {
		k, ok := entry.Key.(marshal.PyString)
		if !ok {
			continue
		}
		switch k.Value {
		case "quoteOnly":
			if v, ok := entry.Value.(marshal.PyInt); ok {
				quoteOnly = v.Value != 0
			} else if v, ok := entry.Value.(marshal.PyBool); ok {
				quoteOnly = v.Value
			}
		case "authorizedCost":
			switch v := entry.Value.(type) {
			case marshal.PyReal:
				authorizedCost = v.Value
				haveAuthorizedCost = true
			case marshal.PyInt:
				authorizedCost = float64(v.Value)
				haveAuthorizedCost = true
			}
		case "maxJobStartTime":
			if v, ok := entry.Value.(marshal.PyInt); ok {
				maxJobStartTime = int64(v.Value)
			}
		}
	}

	// A line inside a starbase array or lab makes this a POS job (posinstall.go). A
	// corporation job elsewhere is a station corporation job: the corporation's print
	// and materials in its office here (ramcorp.go).
	var pj posJob
	isPOS := false
	if li0, lerr := evedb.GetAssemblyLineInfo(ctx, sqldb, assemblyLineID); lerr == nil && li0.ContainerID != stationID {
		if inst, ok, _ := evedb.GetPOSInstallation(ctx, sqldb, li0.ContainerID); ok {
			isPOS = true
			pj.inst = inst
		}
	}
	if isCorpJob && !isPOS && !evedb.StationCorpJobsEnabled(ctx, sqldb) {
		return sendUserError(c, pkt, accountID, "Corporation jobs are only available at your corporation's starbase installations.")
	}
	if runs < 1 {
		return sendUserError(c, pkt, accountID, "Invalid number of runs.")
	}

	log.Printf("[%s] ram.InstallJob: alID=%d bpID=%d activity=%d runs=%d licensedRuns=%d quoteOnly=%v charID=%d",
		addr, assemblyLineID, blueprintID, activityID, runs, licensedProductionRuns, quoteOnly, charID)

	// Validate blueprint ownership and location.
	var bpOwnerID, bpLocationID, bpTypeID int64
	var bpFlag int
	err := sqldb.QueryRowContext(ctx,
		`SELECT ownerID, locationID, typeID, flag FROM entity WHERE itemID = ?`, blueprintID).
		Scan(&bpOwnerID, &bpLocationID, &bpTypeID, &bpFlag)
	if err != nil {
		log.Printf("[%s] ram.InstallJob: blueprint %d not found: %v", addr, blueprintID, err)
		return sendRamMessage(c, pkt, accountID, "RamPleasePickAnItemToInstall", nil)
	}
	container := stationID
	matOwner, matLoc, matFlag := charID, stationID, int64(4)
	var scj evedb.StationCorpJob // set for a station corporation job
	if isPOS {
		var why string
		pj, why = resolvePOSJob(ctx, sqldb, pj.inst, charID, stationID, isCorpJob,
			bpOwnerID, bpLocationID, bpFlag, cs.ArgTuple.Items)
		if why != "" {
			return sendRamWhy(ctx, c, pkt, accountID, sqldb, stationID, why)
		}
		container = pj.inst.ArrayID
		matOwner, matLoc, matFlag = pj.matOwner, pj.matLoc, pj.matFlag
	} else if isCorpJob {
		var reqMatFlag, reqOutFlag int64
		if len(cs.ArgTuple.Items) >= 3 {
			if _, _, f, ok := locationTriple(cs.ArgTuple.Items[2]); ok {
				reqMatFlag = f
			}
		}
		if v, ok := cs.ArgTuple.Items[3].(marshal.PyInt); ok {
			reqOutFlag = int64(v.Value)
		}
		var why string
		scj, why = evedb.ResolveStationCorpJob(ctx, sqldb, charID, stationID,
			bpOwnerID, bpLocationID, int64(bpFlag), reqMatFlag, reqOutFlag)
		if why != "" {
			return sendRamWhy(ctx, c, pkt, accountID, sqldb, stationID, why)
		}
		matOwner, matLoc, matFlag = scj.CorpID, scj.OfficeID, scj.MatFlag
	} else {
		if bpOwnerID != charID {
			return sendRamMessage(c, pkt, accountID, "RamCannotInstallItemForAnother", nil)
		}
		if bpLocationID != stationID {
			return sendRamMessage(c, pkt, accountID, "RamInstalledItemWrongLocation", map[string]marshal.PyRep{
				"location": marshal.PyString{Value: evedb.GetStationName(ctx, sqldb, stationID)},
			})
		}
		if bpFlag != 4 {
			// In the station but not on the hangar floor: a container or a docked ship's cargo.
			return sendRamMessage(c, pkt, accountID, "RamInstalledItemBadLocation", nil)
		}
	}

	// A packaged blueprint stack has one unit split off as a singleton; the quote's
	// second element hands the client the replacement itemID.
	installedID, splitOff, oldStackQty, err := evedb.EnsureBlueprintSingleton(ctx, sqldb, blueprintID)
	if err != nil {
		log.Printf("[%s] ram.InstallJob: %v", addr, err)
		return sendUserError(c, pkt, accountID, "Failed to prepare blueprint.")
	}
	var replacementID int64
	if splitOff {
		replacementID = installedID
		log.Printf("[%s] ram.InstallJob: split blueprint singleton %d off stack %d (was qty=%d)",
			addr, installedID, blueprintID, oldStackQty)
		if er, e := evedb.GetEntityRow(ctx, sqldb, blueprintID); e == nil && er != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(er),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(oldStackQty)}}, // ixQuantity
				}},
			}})
		}
		if er, e := evedb.GetEntityRow(ctx, sqldb, installedID); e == nil && er != nil {
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(er),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}}, // new item
				}},
			}})
		}
		blueprintID = installedID
	}

	// Load blueprint type info.
	bpType, err := evedb.GetBlueprintTypeInfo(ctx, sqldb, bpTypeID)
	if err != nil {
		log.Printf("[%s] ram.InstallJob: %v", addr, err)
		// No invBlueprintTypes row: the installed item is not a blueprint.
		return sendRamMessage(c, pkt, accountID, "RamActivityRequiresABlueprint", nil)
	}

	// Load blueprint instance attrs, auto-creating the row if missing.
	bpAttrs, err := evedb.GetBlueprintAttributes(ctx, sqldb, blueprintID)
	if err != nil {
		// Row missing: create the default BPO entry and retry.
		if err2 := evedb.EnsureBlueprintRow(ctx, sqldb, blueprintID, bpTypeID); err2 != nil {
			log.Printf("[%s] ram.InstallJob: EnsureBlueprintRow: %v", addr, err2)
			return sendUserError(c, pkt, accountID, "Blueprint attributes not found.")
		}
		bpAttrs, err = evedb.GetBlueprintAttributes(ctx, sqldb, blueprintID)
		if err != nil {
			log.Printf("[%s] ram.InstallJob: %v (after auto-create)", addr, err)
			return sendUserError(c, pkt, accountID, "Blueprint attributes not found.")
		}
		log.Printf("[%s] ram.InstallJob: auto-created invBlueprints row for %d", addr, blueprintID)
	}

	// Validate activity vs copy status.
	if bpAttrs.Copy == 1 && activityID == 5 {
		return sendRamMessage(c, pkt, accountID, "RamCannotCopyABlueprintCopy", nil)
	}
	if bpAttrs.Copy == 1 && (activityID == 4 || activityID == 3) {
		return sendRamMessage(c, pkt, accountID, "RamCannotResearchABlueprintCopy", nil)
	}
	if bpAttrs.Copy == 1 && activityID == 1 {
		// Check BPC has enough runs.
		if bpAttrs.RunsRemaining >= 0 && runs > bpAttrs.RunsRemaining {
			return sendRamMessage(c, pkt, accountID, "RamTooManyProductionRuns", nil)
		}
	}

	// Manufacturing runs per job are capped by the blueprint's production limit.
	if activityID == 1 && bpType.MaxProductionLimit > 0 && int64(runs) > bpType.MaxProductionLimit {
		return sendUserError(c, pkt, accountID,
			fmt.Sprintf("This blueprint is limited to %d production runs per job.", bpType.MaxProductionLimit))
	}

	// Skill lookups.
	industrySkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, 3380)
	peSkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, 3388) // Production Efficiency
	metallurgySkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, 3409)
	researchSkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, 3403)
	scienceSkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, 3402)

	// Default licensed runs to max if not specified by client (needed for both quote and install).
	if activityID == 5 && (licensedProductionRuns <= 0 || licensedProductionRuns > bpType.MaxProductionLimit) {
		licensedProductionRuns = bpType.MaxProductionLimit
	}

	// Calculate duration (in seconds).
	var durationSec float64
	var outputTypeID int64

	switch activityID {
	case 1: // Manufacturing
		outputTypeID = bpType.ProductTypeID
		durationSec = CalcProductionTime(bpType.ProductionTime, bpType.ProductivityModifier, bpAttrs.ProductivityLevel, industrySkill) * float64(runs)

	case 4: // ME Research
		outputTypeID = bpTypeID
		durationSec = CalcMEResearchTime(bpType.ResearchMaterialTime, bpAttrs.MaterialLevel, runs, metallurgySkill)

	case 3: // PE Research
		outputTypeID = bpTypeID
		durationSec = CalcPEResearchTime(bpType.ResearchProductivityTime, bpAttrs.ProductivityLevel, runs, researchSkill)

	case 5: // Copying
		outputTypeID = bpTypeID
		durationSec = CalcCopyTime(bpType.ResearchCopyTime, runs, licensedProductionRuns, bpType.MaxProductionLimit, scienceSkill)

	default:
		return sendRamMessage(c, pkt, accountID, "RamActivityInvalid", nil)
	}
	// Load the assembly line: costs, restrictions, occupancy, base multipliers.
	lineInfo, err := evedb.GetAssemblyLineInfo(ctx, sqldb, assemblyLineID)
	if err != nil {
		log.Printf("[%s] ram.InstallJob: %v", addr, err)
		return sendRamMessage(c, pkt, accountID, "RamInstallationIDUnexpected", nil)
	}
	if lineInfo.ContainerID != container {
		return sendRamMessage(c, pkt, accountID, "RamInstallationIDUnexpected", nil)
	}
	if lineInfo.ActivityID != int64(activityID) {
		// The picker often submits a line from the last installation viewed (a factory
		// slot for ME research, say). Redirect to a matching line at this station.
		altID, aerr := evedb.FindAssemblyLineForActivity(ctx, sqldb, container, int64(activityID))
		if aerr != nil || altID == 0 {
			return sendUserError(c, pkt, accountID,
				"This station has no assembly line for that activity.")
		}
		altInfo, aerr := evedb.GetAssemblyLineInfo(ctx, sqldb, altID)
		if aerr != nil {
			return sendRamMessage(c, pkt, accountID, "RamInstallationIDUnexpected", nil)
		}
		log.Printf("[%s] ram.InstallJob: line %d is activity %d — redirected to line %d for activity %d",
			addr, assemblyLineID, lineInfo.ActivityID, altID, activityID)
		assemblyLineID = altID
		lineInfo = altInfo
	}

	// A specialised line builds only what its type names (a Small Ship
	// Assembly Array makes no ammunition); generic station lines build all.
	if activityID == 1 && !evedb.LineCanBuild(ctx, sqldb, lineInfo.AssemblyLineTypeID, outputTypeID) {
		return sendRamMessage(c, pkt, accountID, "RamBadEndProductForActivity", nil)
	}

	// Line restrictions: security status and standing with the facility owner. An NPC
	// owner's standing lives in chrNPCStandings; skill bonuses fold in as elsewhere.
	ownerFactionID, _ := evedb.GetAgentCorpFactionID(ctx, sqldb, lineInfo.OwnerID)
	ownerCorpStanding := evedb.GetNPCStandingToChar(ctx, sqldb, charID, lineInfo.OwnerID)
	ownerFacStanding := evedb.GetNPCStandingToChar(ctx, sqldb, charID, ownerFactionID)
	standingBonus := evedb.StandingSkillBonus(ctx, sqldb, charID, ownerCorpStanding, ownerFacStanding, ownerFactionID)
	standing := evedb.ApplyStandingBonus(ownerCorpStanding, standingBonus)
	if lineInfo.RestrictionMask&RestrictByStanding != 0 && standing < lineInfo.MinimumStanding {
		log.Printf("[%s] ram.InstallJob: standing %.2f < line minimum %.2f", addr, standing, lineInfo.MinimumStanding)
		return sendRamMessage(c, pkt, accountID, "RamAccessDeniedStandingTooLow", nil)
	}
	if lineInfo.RestrictionMask&RestrictBySecurity != 0 {
		sec, _ := evedb.GetCharSecurityRating(ctx, sqldb, charID)
		if sec < lineInfo.MinimumCharSecurity {
			log.Printf("[%s] ram.InstallJob: sec %.2f < line minimum %.2f", addr, sec, lineInfo.MinimumCharSecurity)
			return sendRamMessage(c, pkt, accountID, "RamAccessDeniedSecStatusTooLow", nil)
		}
		if lineInfo.MaximumCharSecurity > 0 && sec > lineInfo.MaximumCharSecurity {
			log.Printf("[%s] ram.InstallJob: sec %.2f > line maximum %.2f", addr, sec, lineInfo.MaximumCharSecurity)
			return sendRamMessage(c, pkt, accountID, "RamAccessDeniedSecStatusTooHigh", nil)
		}
	}
	if lineInfo.RestrictionMask&(RestrictByCorp|RestrictByAlliance) != 0 {
		var charCorpID int64
		_ = sqldb.QueryRowContext(ctx,
			`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&charCorpID)
		if charCorpID != lineInfo.OwnerID {
			key := "RamAccessDeniedWrongCorp"
			if lineInfo.RestrictionMask&RestrictByCorp == 0 {
				key = "RamAccessDeniedWrongAlliance"
			}
			return sendRamMessage(c, pkt, accountID, key, nil)
		}
	}

	// Assembly line time/material multipliers: line type base × per-category ×
	// per-group detail for the output type.
	detailTime, detailMat := evedb.GetLineDetailMultipliers(ctx, sqldb, lineInfo.AssemblyLineTypeID, outputTypeID)
	timeMultiplier := lineInfo.BaseTimeMultiplier * detailTime
	materialMultiplier := lineInfo.BaseMaterialMultiplier * detailMat
	durationSec *= timeMultiplier

	durationSec /= rates.IndustrySpeed
	if activityID == 1 {
		durationSec /= rates.ProductionSpeed
	} else {
		durationSec /= rates.ResearchSpeed
	}

	// Line occupancy: a busy line queues the job to start when it frees up.
	now := server.Win32TimeNow()
	beginTime := int64(now)
	if lineInfo.NextFreeTime > beginTime {
		beginTime = lineInfo.NextFreeTime
	}
	if !quoteOnly && maxJobStartTime > 0 && beginTime > maxJobStartTime {
		return sendRamMessage(c, pkt, accountID, "RamCannotGuaranteeStartTime", nil)
	}

	// Costs: install fee plus hourly usage, adjusted by standing with the owner.
	hours := durationSec / 3600.0
	installCost := lineInfo.CostInstall
	usageCost := lineInfo.CostPerHour * hours
	totalCost := AdjustCostForStanding(installCost+usageCost, standing,
		lineInfo.DiscountPerGoodStanding, lineInfo.SurchargePerBadStanding)

	// Quote-only: return cost/time/BOM without installing.
	if quoteOnly {
		log.Printf("[%s] ram.InstallJob quoteOnly: alID=%d bpTypeID=%d activity=%d runs=%d duration=%.0fs cost=%.2f",
			addr, assemblyLineID, bpTypeID, activityID, runs, durationSec, totalCost)
		return handleQuote(ctx, c, pkt, accountID, matOwner, matLoc, matFlag, sqldb,
			bpTypeID, activityID, runs,
			bpType, bpAttrs, peSkill, durationSec,
			installCost, usageCost, totalCost, timeMultiplier, materialMultiplier, beginTime,
			replacementID)
	}

	// Reject if the price rose above what the client's quote authorized.
	if haveAuthorizedCost && totalCost > authorizedCost+0.01 {
		return sendUserError(c, pkt, accountID,
			"The installation cost has changed. Please request a new quote.")
	}

	// Concurrent job limits from Mass Production / Laboratory Operation skills.
	isManufacturingJob := activityID == 1
	var baseSkillID, advSkillID int64 = SkillLaboratoryOperation, SkillAdvancedLabOperation
	if isManufacturingJob {
		baseSkillID, advSkillID = SkillMassProduction, SkillAdvancedMassProduction
	}
	baseSkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, baseSkillID)
	advSkill, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, advSkillID)
	maxJobs := MaxConcurrentJobs(baseSkill, advSkill)
	activeJobs, cerr := evedb.CountActiveJobs(ctx, sqldb, charID, isManufacturingJob)
	if cerr == nil && activeJobs >= maxJobs {
		key := "MaxResearchFacilitySlotUsageReached"
		if isManufacturingJob {
			key = "MaxFactorySlotUsageReached"
		}
		return sendRamMessage(c, pkt, accountID, key, map[string]marshal.PyRep{
			"current": marshal.PyInt{Value: uint64(activeJobs)},
			"max":     marshal.PyInt{Value: uint64(maxJobs)},
		})
	}

	// Enforce skill requirements for this activity (install path only).
	skillReqs, _ := evedb.GetActivitySkillReqs(ctx, sqldb, bpTypeID, activityID)
	for _, req := range skillReqs {
		have, _ := evedb.GetCharacterSkillLevel(ctx, sqldb, charID, req.SkillID)
		if have < req.Level {
			name, _ := evedb.GetTypeName(ctx, sqldb, req.SkillID)
			return sendRamMessage(c, pkt, accountID, "RamNeedSkillForJob", map[string]marshal.PyRep{
				"skillLevel": marshal.PyInt{Value: uint64(req.Level)},
				"item":       marshal.PyString{Value: name},
			})
		}
	}

	// Compute Win32 times: the job runs from beginTime (line free) for durationSec.
	// Duration in Win32 ticks (100ns intervals): seconds * 10_000_000.
	durationTicks := int64(durationSec * 10000000)
	endTime := beginTime + durationTicks

	// Get solar system.
	solarSystemID, _ := evedb.GetSolarSystemForStation(ctx, sqldb, stationID)
	if isPOS {
		solarSystemID = pj.inst.SolarSystemID
	}

	var jobLicensedRuns int
	if activityID == 5 {
		// For copying jobs, store the licensed runs per copy.
		jobLicensedRuns = int(licensedProductionRuns)
	} else if bpAttrs.Copy == 1 {
		jobLicensedRuns = bpAttrs.RunsRemaining
	}

	// Charge ISK, consume materials, move the blueprint and create the job in
	// one transaction so a failure at any step leaves everything untouched.
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		log.Printf("[%s] ram.InstallJob: begin tx: %v", addr, err)
		return sendUserError(c, pkt, accountID, "Failed to create job.")
	}
	defer tx.Rollback()

	// A corporation job, starbase or station, is paid from the corporation's wallet.
	// Everything else is charged to the pilot.
	var payerCorp int64
	if isCorpJob {
		payerCorp = scj.CorpID
		if isPOS {
			payerCorp = pj.corpID
		}
	}
	var charged bool
	var newBalance float64
	if payerCorp != 0 {
		charged, newBalance, err = evedb.TryChargeCorpISK(ctx, tx, payerCorp, totalCost)
	} else {
		charged, newBalance, err = evedb.TryChargeISK(ctx, tx, charID, totalCost)
	}
	if err != nil {
		log.Printf("[%s] ram.InstallJob: charge ISK: %v", addr, err)
		return sendUserError(c, pkt, accountID, "Failed to create job.")
	}
	if !charged {
		if payerCorp != 0 {
			return sendUserError(c, pkt, accountID,
				fmt.Sprintf("Your corporation cannot afford the %.2f ISK installation cost.", totalCost))
		}
		bal, _ := evedb.GetCharacterBalance(ctx, sqldb, charID)
		return sendRamMessage(c, pkt, accountID, "NotEnoughMoney", map[string]marshal.PyRep{
			"amount":  marshal.PyString{Value: ramISK(totalCost)},
			"balance": marshal.PyString{Value: ramISK(bal)},
		})
	}

	var consumed []evedb.ConsumedStack
	if activityID == 1 {
		consumed, err = evedb.ConsumeMaterialsFrom(ctx, tx, matOwner, matLoc, matFlag, bpTypeID,
			bpAttrs.MaterialLevel, peSkill, runs, materialMultiplier)
		if err != nil {
			log.Printf("[%s] ram.InstallJob: material consumption failed: %v", addr, err)
			var missing *evedb.ErrMissingMaterial
			if errors.As(err, &missing) {
				return sendRamNeedMoreForJob(c, pkt, accountID, missing)
			}
			return sendUserError(c, pkt, accountID, "Failed to create job.")
		}
	}

	// Snapshot consumed stacks' entity rows before commit: the pool connection still
	// sees the pre-transaction rows the post-commit OnItemChange needs.
	consumedRows := make(map[int64]*evedb.EntityRow, len(consumed))
	for _, cs := range consumed {
		if er, err := evedb.GetEntityRow(ctx, sqldb, cs.ItemID); err == nil && er != nil {
			consumedRows[cs.ItemID] = er
		}
	}

	// Move blueprint to in-production flag (111).
	if err := evedb.MoveBlueprintToFlag(ctx, tx, blueprintID, 111); err != nil {
		log.Printf("[%s] ram.InstallJob: move blueprint: %v", addr, err)
		return sendUserError(c, pkt, accountID, "Failed to create job.")
	}

	// Insert job row.
	outFlag, jobOwner, retFlag := int64(4), int64(0), int64(4)
	if isPOS {
		outFlag, jobOwner, retFlag = pj.outputFlag, pj.corpID, pj.bpFlag
	} else if scj.CorpID != 0 {
		outFlag, jobOwner, retFlag = scj.OutFlag, scj.CorpID, scj.BPFlag
	}
	jobID, err := evedb.InsertJobEx(ctx, tx, assemblyLineID, container, charID, blueprintID,
		int64(now), beginTime, endTime, runs, activityID, outputTypeID, jobLicensedRuns, solarSystemID,
		outFlag, jobOwner, retFlag)
	if err != nil {
		log.Printf("[%s] ram.InstallJob: insert job: %v", addr, err)
		return sendUserError(c, pkt, accountID, "Failed to create job.")
	}
	if !isPOS && scj.CorpID != 0 {
		// The line is the station's; the output belongs in the office.
		if err := evedb.SetJobOutputLocation(ctx, tx, jobID, scj.OfficeID); err != nil {
			log.Printf("[%s] ram.InstallJob: %v", addr, err)
			return sendUserError(c, pkt, accountID, "Failed to create job.")
		}
	}

	if err := tx.Commit(); err != nil {
		log.Printf("[%s] ram.InstallJob: commit: %v", addr, err)
		return sendUserError(c, pkt, accountID, "Failed to create job.")
	}

	log.Printf("[%s] → ram.InstallJob jobID=%d activity=%d runs=%d duration=%.0fs outputType=%d cost=%.2f begin=%d",
		addr, jobID, activityID, runs, durationSec, outputTypeID, totalCost, beginTime)

	// Journal the fee under the activity's refType (ramActivityRefType) and credit
	// the line owner.
	if totalCost > 0 {
		refType := ramActivityRefType(activityID)
		payer, reason := charID, "Industry job installation"
		if payerCorp != 0 {
			payer, reason = payerCorp, "Corporation industry job installation"
		}
		_ = evedb.InsertJournalEntry(ctx, sqldb, payer, refType, charID, lineInfo.OwnerID,
			-totalCost, newBalance, reason)
		if lineInfo.OwnerID > 0 {
			if cerr := evedb.CreditCorpWallet(ctx, sqldb, lineInfo.OwnerID, totalCost); cerr == nil {
				if ownerBal, berr := evedb.GetCorpBalance(ctx, sqldb, lineInfo.OwnerID); berr == nil {
					_ = evedb.InsertJournalEntry(ctx, sqldb, lineInfo.OwnerID, refType,
						charID, lineInfo.OwnerID, totalCost, ownerBal, "Assembly line usage fee")
				}
			}
		}
		if payerCorp == 0 {
			// A corporation job's fee left the corporation's wallet, which the pilot's cash
			// display does not show.
			_ = server.SendNotification(c, accountID, "OnAccountChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				marshal.PyString{Value: "cash"},
				marshal.PyInt{Value: uint64(charID)},
				marshal.PyReal{Value: newBalance},
			}})
		}
	}

	// OnItemChange per consumed stack. A deleted stack is re-parented to the junkyard
	// (6) with quantity 0, old location and quantity in the change dict.
	for _, cs := range consumed {
		er := consumedRows[cs.ItemID]
		if er == nil {
			continue
		}
		if cs.NewQty == 0 {
			gone := *er
			gone.LocationID = 6
			gone.Quantity = 0
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(&gone),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(er.LocationID)}}, // ixLocationID
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(cs.OldQty)}},     // ixQuantity
				}},
			}})
		} else {
			upd := *er
			upd.Quantity = cs.NewQty
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(&upd),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(cs.OldQty)}}, // ixQuantity
				}},
			}})
		}
	}

	// Send OnItemChange for blueprint moving to in-production.
	bpER, _ := evedb.GetEntityRow(ctx, sqldb, blueprintID)
	if bpER != nil {
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: uint64(bpFlag)}}, // old flag
		}}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(bpER),
			changeDict,
		}})
	}

	// Return jobID to client.
	return server.SendCallRsp(c, pkt, accountID, marshal.PyInt{Value: uint64(jobID)})
}

func handleCompleteJob(ctx context.Context, c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {

	// Client signature: CompleteJob(installationLocationData, jobID, cancel). The
	// jobID is the first PyInt in the args.
	var jobID int64
	var cancel bool
	jobIDIdx := -1
	for i, item := range cs.ArgTuple.Items {
		if v, ok := item.(marshal.PyInt); ok {
			jobID = int64(v.Value)
			jobIDIdx = i
			break
		}
	}
	if jobIDIdx < 0 {
		log.Printf("[%s] ram.CompleteJob: no jobID in args", addr)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
	if len(cs.ArgTuple.Items) > jobIDIdx+1 {
		switch b := cs.ArgTuple.Items[jobIDIdx+1].(type) {
		case marshal.PyBool:
			cancel = b.Value
		case marshal.PyInt:
			cancel = b.Value != 0
		}
	}

	log.Printf("[%s] ram.CompleteJob: jobID=%d cancel=%v charID=%d", addr, jobID, cancel, charID)

	// Snapshot the installed blueprint before delivery: an exhausted BPC is deleted
	// inside CompleteJob.
	var installedItemID int64
	_ = sqldb.QueryRowContext(ctx,
		`SELECT installedItemID FROM ramJobs WHERE jobID = ?`, jobID).Scan(&installedItemID)
	var bpBefore *evedb.EntityRow
	if installedItemID > 0 {
		bpBefore, _ = evedb.GetEntityRow(ctx, sqldb, installedItemID)
	}

	outputs, err := evedb.CompleteJob(ctx, sqldb, jobID, charID, cancel)
	if err != nil {
		log.Printf("[%s] ram.CompleteJob: %v", addr, err)
		switch {
		case errors.Is(err, evedb.ErrJobNotFound):
			return sendRamMessage(c, pkt, accountID, "RamCompletionNoSuchJob", nil)
		case errors.Is(err, evedb.ErrJobNotYours):
			return sendRamMessage(c, pkt, accountID, "RamCompletionAccessDenied", nil)
		case errors.Is(err, evedb.ErrJobStillRunning):
			return sendRamMessage(c, pkt, accountID, "RamCompletionInProduction", nil)
		case errors.Is(err, evedb.ErrJobDelivered):
			return sendRamMessage(c, pkt, accountID, "RamCompletionJobCompleted", nil)
		}
		return sendUserError(c, pkt, accountID, err.Error())
	}

	// OnItemChange for the returned blueprint and the new products.
	if installedItemID > 0 {
		bpER, _ := evedb.GetEntityRow(ctx, sqldb, installedItemID)
		if bpER != nil {
			changeDict := marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 4}, Value: marshal.PyInt{Value: 111}}, // old flag = 111 (in-production)
			}}
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(bpER),
				changeDict,
			}})
		} else if bpBefore != nil {
			// BPC exhausted on this delivery: tell the client it is gone.
			gone := *bpBefore
			gone.LocationID = 6 // junkyard
			gone.Quantity = 0
			_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
				evedb.BuildEntityRow(&gone),
				marshal.PyDict{Items: []marshal.DictEntry{
					{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: uint64(bpBefore.LocationID)}}, // ixLocationID
					{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(bpBefore.Quantity)}},   // ixQuantity
				}},
			}})
		}
	}

	for _, out := range outputs {
		outER, _ := evedb.GetEntityRow(ctx, sqldb, out.ItemID)
		if outER == nil {
			continue
		}
		changeDict := marshal.PyDict{Items: []marshal.DictEntry{
			// New item: old locationID 0 marks it as newly arrived.
			{Key: marshal.PyInt{Value: 3}, Value: marshal.PyInt{Value: 0}},
		}}
		if out.OldQty > 0 {
			// Merged into an existing stack: quantity change, not a new item.
			changeDict = marshal.PyDict{Items: []marshal.DictEntry{
				{Key: marshal.PyInt{Value: 7}, Value: marshal.PyInt{Value: uint64(out.OldQty)}}, // ixQuantity
			}}
		}
		_ = server.SendNotification(c, accountID, "OnItemChange", "charid", marshal.PyTuple{Items: []marshal.PyRep{
			evedb.BuildEntityRow(outER),
			changeDict,
		}})
	}

	log.Printf("[%s] → ram.CompleteJob jobID=%d outputs=%+v", addr, jobID, outputs)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
}

func handleGetJobs2(ctx context.Context, c *evenet.Conn, addr string,
	cs *server.CallStream, pkt *evenet.PyPacket, accountID, charID int64, sqldb *sql.DB) error {

	// Args: ownerID, completed (bool)
	ownerID := charID
	completed := false
	if len(cs.ArgTuple.Items) >= 1 {
		if v, ok := cs.ArgTuple.Items[0].(marshal.PyInt); ok {
			ownerID = int64(v.Value)
		}
	}
	if len(cs.ArgTuple.Items) >= 2 {
		if b, ok := cs.ArgTuple.Items[1].(marshal.PyBool); ok {
			completed = b.Value
		} else if v, ok := cs.ArgTuple.Items[1].(marshal.PyInt); ok {
			completed = v.Value != 0
		}
	}
	// Advanced-filter date bounds (Win32 FILETIME); PyNone when not filtering.
	var fromTime, toTime int64
	if len(cs.ArgTuple.Items) >= 3 {
		if v, ok := cs.ArgTuple.Items[2].(marshal.PyInt); ok {
			fromTime = int64(v.Value)
		}
	}
	if len(cs.ArgTuple.Items) >= 4 {
		if v, ok := cs.ArgTuple.Items[3].(marshal.PyInt); ok {
			toTime = int64(v.Value)
		}
	}

	// "My Corporation": the corp's own jobs (starbase jobs are owned by the
	// corporation), and only for a member of it.
	var result marshal.PyRep
	var err error
	if ownerID != charID {
		if ownerID == corpOfChar(ctx, sqldb, charID) {
			result, err = evedb.GetJobs(ctx, sqldb, ownerID, completed, fromTime, toTime, true)
		} else {
			result, err = evedb.GetJobs(ctx, sqldb, 0, completed, fromTime, toTime, true)
		}
	} else {
		result, err = evedb.GetJobs(ctx, sqldb, ownerID, completed, fromTime, toTime)
	}
	if err != nil {
		log.Printf("[%s] ram.GetJobs2: %v", addr, err)
	}
	log.Printf("[%s] → ram.GetJobs2 ownerID=%d completed=%v", addr, ownerID, completed)
	return server.SendCallRsp(c, pkt, accountID, result)
}

// handleQuote builds the response for quoteOnly=1 InstallJob calls. Costs and
// multipliers arrive from handleInstallJob so quote and install agree.
func handleQuote(ctx context.Context, c *evenet.Conn, pkt *evenet.PyPacket,
	accountID, matOwner, matLoc, matFlag int64, sqldb *sql.DB,
	bpTypeID int64, activityID, runs int,
	bpType *evedb.BlueprintTypeInfo, bpAttrs *evedb.BlueprintAttrs,
	peSkill int, durationSec float64,
	installCost, usageCost, totalCost, timeMultiplier, materialMultiplier float64,
	jobStartTime, replacementItemID int64) error {

	// Fetch BOM for this activity.
	mats, err := evedb.GetRequiredMaterials(ctx, sqldb, bpTypeID, activityID)
	if err != nil {
		log.Printf("ram.quote: GetRequiredMaterials: %v", err)
		mats = nil
	}

	// Material list columns.
	matCols := []string{"requiredTypeID", "quantity", "damagePerJob", "isSkillCheck", "requiresHP"}

	var rawRows, extraRows, wasteRows [][]marshal.PyRep
	missingMats := marshal.PyDict{}

	isManufacturing := activityID == 1

	for _, m := range mats {
		var needed int64
		var wasteQty int64

		// Materials with damagePerJob < 1 are "extras": required but only partially
		// consumed. ME waste does not apply to them.
		isExtra := m.DamagePerJob < 1.0

		if isManufacturing && !isExtra {
			perRun := evedb.ApplyMaterialMultiplier(
				evedb.CalcMaterialsNeeded(m.Quantity, bpType.WasteFactor, bpAttrs.MaterialLevel, peSkill),
				materialMultiplier)
			needed = perRun * int64(runs)
			baseNoWaste := m.Quantity * int64(runs)
			wasteQty = needed - baseNoWaste
			if wasteQty < 0 {
				wasteQty = 0
			}
		} else {
			needed = m.Quantity * int64(runs)
		}

		row := []marshal.PyRep{
			marshal.PyInt{Value: uint64(m.TypeID)},
			marshal.PyInt{Value: uint64(needed)},
			marshal.PyReal{Value: m.DamagePerJob},
			marshal.PyInt{Value: 0},
			marshal.PyInt{Value: 0},
		}
		if isExtra {
			extraRows = append(extraRows, row)
		} else {
			rawRows = append(rawRows, row)
		}

		if wasteQty > 0 {
			wasteRows = append(wasteRows, []marshal.PyRep{
				marshal.PyInt{Value: uint64(m.TypeID)},
				marshal.PyInt{Value: uint64(wasteQty)},
				marshal.PyReal{Value: 1.0},
				marshal.PyInt{Value: 0},
				marshal.PyInt{Value: 0},
			})
		}

		// Check available quantity.
		avail, _ := evedb.GetMaterialQuantityAt(ctx, sqldb, matOwner, matLoc, matFlag, m.TypeID)
		if avail < needed {
			missingMats.Items = append(missingMats.Items, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(m.TypeID)},
				Value: marshal.PyInt{Value: uint64(needed - avail)},
			})
		}
	}

	rawMaterials := buildRowset(matCols, rawRows)
	wasteMaterials := buildRowset(matCols, wasteRows)
	extras := buildRowset(matCols, extraRows)

	// Skill requirements {skillTypeID: level}, rendered by the quote window.
	skills := marshal.PyDict{}
	if skillReqs, serr := evedb.GetActivitySkillReqs(ctx, sqldb, bpTypeID, activityID); serr == nil {
		for _, req := range skillReqs {
			skills.Items = append(skills.Items, marshal.DictEntry{
				Key:   marshal.PyInt{Value: uint64(req.SkillID)},
				Value: marshal.PyInt{Value: uint64(req.Level)},
			})
		}
	}

	// BOM as util.Row with sub-rowsets.
	bomCols := []string{"rawMaterials", "extras", "wasteMaterials", "skills"}
	bomVals := []marshal.PyRep{rawMaterials, extras, wasteMaterials, skills}
	bom := buildRow(bomCols, bomVals)

	// Multipliers. "line & item" multipliers come from the assembly line type;
	// the char multipliers reflect skill effects.
	materialMul := materialMultiplier
	charMaterialMul := 1.0
	if isManufacturing {
		charMaterialMul = 1.0 + (0.25 - 0.05*float64(peSkill))
		if charMaterialMul < 1.0 {
			charMaterialMul = 1.0
		}
	}
	timeMul := timeMultiplier
	charTimeMul := 1.0
	// charTimeMul is reported as 1.0: durationSec already has the skill reduction.

	// maxJobStartTime: Win32 time the job would begin, now or when queued jobs finish.
	maxJobStartTime := uint64(jobStartTime)

	// Production time in seconds (integer).
	prodTime := int64(durationSec)

	// bomLocationData: location info for BOM items. PyNone when not tracked.
	var bomLocationData marshal.PyRep = marshal.PyNone{}

	// Build quote row.
	quoteCols := []string{
		"bom", "materialMultiplier", "charMaterialMultiplier",
		"timeMultiplier", "charTimeMultiplier",
		"cost", "installCost", "usageCost", "productionTime",
		"maxJobStartTime", "bomLocationData", "missingMaterials",
	}
	quoteVals := []marshal.PyRep{
		bom,
		marshal.PyReal{Value: materialMul},
		marshal.PyReal{Value: charMaterialMul},
		marshal.PyReal{Value: timeMul},
		marshal.PyReal{Value: charTimeMul},
		marshal.PyReal{Value: totalCost},
		marshal.PyReal{Value: installCost},
		marshal.PyReal{Value: usageCost},
		marshal.PyInt{Value: uint64(prodTime)},
		marshal.PyInt{Value: maxJobStartTime},
		bomLocationData,
		missingMats,
	}
	quoteRow := buildRow(quoteCols, quoteVals)

	// The client unpacks the response as (quote, d). d is a replacement itemID for the
	// installed blueprint when a singleton was split off a stack, else None.
	var replacement marshal.PyRep = marshal.PyNone{}
	if replacementItemID != 0 {
		replacement = marshal.PyInt{Value: uint64(replacementItemID)}
	}
	result := marshal.PyTuple{Items: []marshal.PyRep{
		quoteRow,
		replacement,
	}}

	return server.SendCallRsp(c, pkt, accountID, result)
}

// buildRow constructs a util.Row PyObject (same as db.makeRow but local to ram package).
func buildRow(cols []string, vals []marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	line := make([]marshal.PyRep, len(vals))
	copy(line, vals)
	return marshal.PyObject{
		Type: "util.Row",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "line"}, Value: marshal.PyList{Items: line}},
		}},
	}
}

// buildRowset constructs a util.Rowset PyObject (same as db.makeRowset but local to ram package).
func buildRowset(cols []string, rows [][]marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	lines := make([]marshal.PyRep, len(rows))
	for i, row := range rows {
		items := make([]marshal.PyRep, len(row))
		copy(items, row)
		lines[i] = marshal.PyList{Items: items}
	}
	headerList := marshal.PyList{Items: header}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: lines}},
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}

// sendUserError sends a UserError exception back to the client.
func sendUserError(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, msg string) error {
	log.Printf("[%s] ram: rejected: %s", c.RemoteAddr(), msg)
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(
		"CustomInfo", map[string]marshal.PyRep{
			"info": marshal.PyString{Value: msg},
		}))
}

// sendRamMessage refuses with one of the client's own message keys
// (sql/evemessages.sql, config.BulkData.messages), which the client shows as a
// titled popup. dict carries the key's %(name)s arguments; nil when it takes none.
func sendRamMessage(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, key string, dict map[string]marshal.PyRep) error {
	log.Printf("[%s] ram: rejected: %s %v", c.RemoteAddr(), key, dict)
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(key, dict))
}

// sendRamWhy answers a corporation/starbase job refusal: with the client's own
// message where one says the same thing, otherwise with the refusal's text.
func sendRamWhy(ctx context.Context, c *evenet.Conn, pkt *evenet.PyPacket, accountID int64,
	sqldb *sql.DB, stationID int64, why string) error {
	here := func() map[string]marshal.PyRep {
		return map[string]marshal.PyRep{"location": marshal.PyString{Value: evedb.GetStationName(ctx, sqldb, stationID)}}
	}
	switch why {
	case evedb.WhyCorpJobNoOffice:
		return sendRamMessage(c, pkt, accountID, "RamCorpBOMItemNoSuchOffice", here())
	case evedb.WhyCorpJobBPLocation:
		return sendRamMessage(c, pkt, accountID, "RamCorpInstalledItemWrongLocation", here())
	case evedb.WhyCorpJobBPRole:
		return sendRamMessage(c, pkt, accountID, "RamCannotInstallForCorpByRoleFactoryManager", nil)
	case evedb.WhyCorpJobMatRole, whyPOSMatRole:
		return sendRamMessage(c, pkt, accountID, "RamAccessDeniedToBOMHangar", nil)
	case whyPOSWrongCorp:
		return sendRamMessage(c, pkt, accountID, "RamAccessDeniedWrongCorp", nil)
	case whyPOSBPRole:
		return sendRamMessage(c, pkt, accountID, "RamCannotInstallForCorpByRole", nil)
	}
	return sendUserError(c, pkt, accountID, why)
}

// ramISK formats an amount the way the client's FmtISK does ("1,234,567.89"),
// for message keys whose ISK argument is a pre-formatted %(name)s string.
func ramISK(v float64) string {
	neg := v < 0
	if neg {
		v = -v
	}
	cents := int64(v*100 + 0.5)
	whole, frac := cents/100, cents%100
	s := fmt.Sprintf("%d", whole)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	if neg {
		s = "-" + s
	}
	return fmt.Sprintf("%s.%02d", s, frac)
}

// sendRamNeedMoreForJob pushes the client's RamNeedMoreForJob message
// (sql/evemessages.sql) for a material shortfall, like RamNeedSkillForJob.
func sendRamNeedMoreForJob(c *evenet.Conn, pkt *evenet.PyPacket, accountID int64, missing *evedb.ErrMissingMaterial) error {
	item := fmt.Sprintf("%dx %s", missing.Needed, missing.TypeName)
	log.Printf("[%s] ram: rejected: need %s", c.RemoteAddr(), item)
	return server.SendCallException(c, pkt, accountID, server.BuildUserError(
		"RamNeedMoreForJob", map[string]marshal.PyRep{
			"item": marshal.PyString{Value: item},
		}))
}

// ramActivityRefType maps a RAM activity to the journal ref type its fee is
// recorded under (sql/market_reftypes.sql: refManufacturing 56, researching
// technology/time/material 57/58/59, copying 60, duplicating 61).
func ramActivityRefType(activityID int) int {
	switch activityID {
	case 1:
		return 56
	case 2:
		return 57
	case 3:
		return 58
	case 4:
		return 59
	case 5:
		return 60
	case 6:
		return 61
	}
	return 56
}
