package db

import (
	"context"
	"database/sql"
)

// StructureCorpJob is a starbase installation job resolved for a corporation:
// the array the job runs in, and the divisions its blueprint, materials and
// output use. The POS twin of StationCorpJob.
type StructureCorpJob struct {
	CorpID        int64
	ArrayID       int64
	SolarSystemID int64
	BPFlag        int64
	MatFlag       int64
	OutFlag       int64
}

// ResolveStructureCorpJob applies the S&I window's starbase installation rules
// (server/ram/posinstall.go resolvePOSJob) for a caller with no client: array and tower online,
// installer in the owning corporation with Factory Manager and the take role on the blueprint and
// materials divisions, blueprint corp property in an array division. Range is not checked; a
// non-empty string is the refusal.
func ResolveStructureCorpJob(ctx context.Context, db *sql.DB, charID, arrayID, bpItemID, matFlag, outFlag int64) (StructureCorpJob, string) {
	var j StructureCorpJob
	inst, ok, err := GetPOSInstallation(ctx, db, arrayID)
	if err != nil || !ok {
		return j, "That is not a starbase installation."
	}
	if inst.State != StarbaseStateOnline {
		return j, "That installation is not online."
	}
	if tower, ok, _ := GetStarbase(ctx, db, inst.TowerID); !ok ||
		(tower.State != StarbaseStateOnline && tower.State != StarbaseStateReinforced) {
		return j, "That installation's control tower is not online."
	}
	var corpID int64
	if err := db.QueryRowContext(ctx, `SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&corpID); err != nil ||
		corpID != inst.OwnerCorpID {
		return j, "That installation belongs to another corporation."
	}
	j.CorpID, j.ArrayID, j.SolarSystemID = corpID, arrayID, inst.SolarSystemID
	inDivision := func(flag int64) bool {
		for _, f := range StructureBayFlags(inst.GroupID) {
			if f == flag {
				return true
			}
		}
		return false
	}
	var bpOwner, bpLoc, bpFlag int64
	if err := db.QueryRowContext(ctx, `SELECT ownerID, locationID, flag FROM entity WHERE itemID = ?`, bpItemID).
		Scan(&bpOwner, &bpLoc, &bpFlag); err != nil {
		return j, "The blueprint is gone."
	}
	if bpOwner != corpID || bpLoc != arrayID || !inDivision(bpFlag) {
		return j, "The blueprint must be in this installation's corporate hangar."
	}
	if !CorpJobAllowed(ctx, db, corpID, charID, bpFlag) {
		return j, "You need the Factory Manager role and the hangar role for the blueprint's division."
	}
	j.BPFlag, j.MatFlag, j.OutFlag = bpFlag, FlagCorpHangarDiv1, FlagCorpHangarDiv1
	if matFlag != 0 {
		if !inDivision(matFlag) || !CorpJobAllowed(ctx, db, corpID, charID, matFlag) {
			return j, "Materials must come from a division of this installation you may take from."
		}
		j.MatFlag = matFlag
	}
	if outFlag != 0 && inDivision(outFlag) {
		j.OutFlag = outFlag
	}
	return j, ""
}
