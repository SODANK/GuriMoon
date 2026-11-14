package ram

// POS installations: assembly arrays (397) and mobile laboratories (413).
//
// The S&I window (manufacturing.py) sends the array as the installation container,
// the blueprint and materials location as (arrayID, corpID, division flag), and the
// output division. Server side:
//
//   - the pilot is in space, in the array's system, in range and in the owning corp;
//   - the array and its tower are online;
//   - jobs are corporation jobs: blueprint and materials come from and go back to the
//     array's corp hangar divisions; a personal blueprint is refused (corp property only).

import (
	"context"
	"database/sql"
	"fmt"

	evedb "gurimoon/db"
	"gurimoon/marshal"
)

// StructureInRange reports whether charID's ship is in space in solarSystemID
// within operating range of structureID. Set by cmd/gurimoon/posindustry.go
// (it owns the live grids); nil refuses every POS job.
var StructureInRange func(charID, structureID, solarSystemID int64) bool

// CorpHangarMayTake reports whether charID holds the take role for a corp hangar
// division flag. Set by cmd/gurimoon/posindustry.go; nil refuses every take.
var CorpHangarMayTake func(corpID, charID, flag int64) bool

// posJob is the resolved POS side of an InstallJob.
type posJob struct {
	inst       evedb.POSInstallation
	corpID     int64
	bpFlag     int64
	matOwner   int64
	matLoc     int64
	matFlag    int64
	outputFlag int64
}

// locationTriple reads the [containerID, ownerID, flag] triple from the second
// element of a location-data list (manufacturing.py GetPathToItem, bomLocationData).
// That element is a path and the item sits at its last hop: an office at a station
// is three hops, so reading the first hop gives flag 0.
func locationTriple(rep marshal.PyRep) (containerID, ownerID, flag int64, ok bool) {
	l, isList := rep.(marshal.PyList)
	if !isList || len(l.Items) < 2 {
		return 0, 0, 0, false
	}
	inner, isList := l.Items[1].(marshal.PyList)
	if !isList || len(inner.Items) == 0 {
		return 0, 0, 0, false
	}
	hop := inner.Items[len(inner.Items)-1]
	t, isList := hop.(marshal.PyList)
	if !isList {
		if tt, isTuple := hop.(marshal.PyTuple); isTuple {
			t = marshal.PyList(tt)
		} else {
			return 0, 0, 0, false
		}
	}
	vals := make([]int64, 0, 3)
	for _, it := range t.Items {
		if v, isInt := it.(marshal.PyInt); isInt {
			vals = append(vals, int64(v.Value))
		} else {
			vals = append(vals, 0)
		}
	}
	switch len(vals) {
	case 3:
		return vals[0], vals[1], vals[2], true
	case 1:
		return 0, 0, vals[0], true // [[flagNone]]: no materials needed
	}
	return 0, 0, 0, false
}

func isDivisionFlag(groupID, flag int64) bool {
	for _, f := range evedb.StructureBayFlags(groupID) {
		if f == flag {
			return true
		}
	}
	return false
}

// Starbase job refusals answered with the client's own message (ramWhyMessage).
const (
	whyPOSWrongCorp = "That installation belongs to another corporation."
	whyPOSBPRole    = "You do not have the hangar role for the blueprint's division."
	whyPOSMatRole   = "You do not have the hangar role for the materials' division."
)

// resolvePOSJob validates a starbase InstallJob and returns its locations.
// A non-empty refusal is the message for the pilot.
func resolvePOSJob(ctx context.Context, sqldb *sql.DB, inst evedb.POSInstallation, charID, stationID int64,
	isCorpJob bool, bpOwnerID, bpLocationID int64, bpFlag int, args []marshal.PyRep) (posJob, string) {

	pj := posJob{inst: inst}
	if stationID != 0 {
		return pj, "You must be in space at the starbase to use its installations."
	}
	if inst.State != evedb.StarbaseStateOnline {
		return pj, "That installation is not online."
	}
	if tower, ok, _ := evedb.GetStarbase(ctx, sqldb, inst.TowerID); !ok ||
		(tower.State != evedb.StarbaseStateOnline && tower.State != evedb.StarbaseStateReinforced) {
		return pj, "That installation's control tower is not online."
	}
	if StructureInRange == nil || !StructureInRange(charID, inst.ArrayID, inst.SolarSystemID) {
		return pj, "You are too far away from that installation."
	}
	_, corpID, _, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil || corpID != inst.OwnerCorpID {
		return pj, whyPOSWrongCorp
	}
	pj.corpID = corpID
	if !isCorpJob {
		return pj, "At a starbase, install jobs with your corporation's blueprints from the installation's hangar."
	}
	mayTake := func(flag int64) bool { return CorpHangarMayTake != nil && CorpHangarMayTake(corpID, charID, flag) }

	// Blueprint: in the array, corp property, in a division the pilot may take from.
	if bpOwnerID != corpID || bpLocationID != inst.ArrayID || !isDivisionFlag(inst.GroupID, int64(bpFlag)) {
		return pj, "The blueprint must be in this installation's corporate hangar."
	}
	if !mayTake(int64(bpFlag)) {
		return pj, whyPOSBPRole
	}
	pj.bpFlag = int64(bpFlag)

	// Materials: bomLocationData [[loc, grp], [[containerID, ownerID, flag]], []].
	pj.matOwner, pj.matLoc, pj.matFlag = corpID, inst.ArrayID, evedb.FlagCorpHangarDiv1
	if len(args) >= 3 {
		if cid, owner, flag, ok := locationTriple(args[2]); ok && cid != 0 {
			if cid != inst.ArrayID || owner != corpID || !isDivisionFlag(inst.GroupID, flag) {
				return pj, "Materials must come from this installation's corporate hangar."
			}
			if !mayTake(flag) {
				return pj, whyPOSMatRole
			}
			pj.matFlag = flag
		}
	}

	// Output: flagOutput (arg 3) is a division of this array.
	pj.outputFlag = evedb.FlagCorpHangarDiv1
	if len(args) >= 4 {
		if v, ok := args[3].(marshal.PyInt); ok && isDivisionFlag(inst.GroupID, int64(v.Value)) {
			pj.outputFlag = int64(v.Value)
		}
	}
	return pj, ""
}

// posAssemblyLinesFor is AssemblyLinesSelect's corp/alliance answer: the
// starbase installations of the caller's corporation (or alliance). ok=false
// when there are none, so the caller can fall back to the station set.
func posAssemblyLinesFor(ctx context.Context, sqldb *sql.DB, charID int64, alliance bool) (marshal.PyRep, bool) {
	_, corpID, allianceID, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil || corpID == 0 {
		return nil, false
	}
	owners := []int64{corpID}
	if alliance && allianceID != 0 {
		if ids, err := evedb.CorpIDsOfAlliance(ctx, sqldb, allianceID); err == nil && len(ids) > 0 {
			owners = ids
		}
	}
	r, n, err := evedb.GetStructureAssemblyLinesSelect(ctx, sqldb, owners)
	if err != nil || n == 0 {
		return nil, false
	}
	return r, true
}

// corpOfChar is the caller's corporation (0 on error).
func corpOfChar(ctx context.Context, sqldb *sql.DB, charID int64) int64 {
	_, corpID, _, err := evedb.GetCharSovContext(ctx, sqldb, charID)
	if err != nil {
		return 0
	}
	return corpID
}

var _ = fmt.Sprintf
