package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/corp"
)

// Station corporation jobs: a corporation's blueprint, in its office at an NPC station,
// run on that station's lines for the corporation. The rules live here because the S&I
// window (ramsvc.go) and the rolebots contract builders install the same job. The role
// gate is Factory Manager plus query on the division, never take.
// See docs/db-12-notes.md#station-corporation-jobs.

// corpHangarRoles is each division flag's query and take role, the client's own
// rolesByFlag table (form.CorpHangar) restated where db can read it.
var corpHangarRoles = []struct {
	Flag      int64
	QueryRole uint64
	TakeRole  uint64
}{
	{FlagCorpHangarDiv1, corp.RoleHangarCanQuery1, corp.RoleHangarCanTake1},
	{FlagCorpSAGFirst + 0, corp.RoleHangarCanQuery2, corp.RoleHangarCanTake2},
	{FlagCorpSAGFirst + 1, corp.RoleHangarCanQuery3, corp.RoleHangarCanTake3},
	{FlagCorpSAGFirst + 2, corp.RoleHangarCanQuery4, corp.RoleHangarCanTake4},
	{FlagCorpSAGFirst + 3, corp.RoleHangarCanQuery5, corp.RoleHangarCanTake5},
	{FlagCorpSAGFirst + 4, corp.RoleHangarCanQuery6, corp.RoleHangarCanTake6},
	{FlagCorpSAGFirst + 5, corp.RoleHangarCanQuery7, corp.RoleHangarCanTake7},
}

// CorpHangarQueryRole is the role that lets a member see a division, 0 for a flag
// that is not a corporation hangar division.
func CorpHangarQueryRole(flag int64) uint64 {
	for _, d := range corpHangarRoles {
		if d.Flag == flag {
			return d.QueryRole
		}
	}
	return 0
}

// CorpHangarTakeRole is the role that lets a member take from a division, 0 for a flag
// that is not a corporation hangar division.
func CorpHangarTakeRole(flag int64) uint64 {
	for _, d := range corpHangarRoles {
		if d.Flag == flag {
			return d.TakeRole
		}
	}
	return 0
}

// CorpHangarTakeRoleAllowed is the pure rule for using up a division's stock in place
// (reprocessing it): the CEO, a Director, or take on that division.
func CorpHangarTakeRoleAllowed(roles uint64, isCEO bool, flag int64) bool {
	t := CorpHangarTakeRole(flag)
	if t == 0 {
		return false
	}
	return isCEO || roles&corp.RoleDirector != 0 || roles&t != 0
}

// CorpHangarTakeAllowed reads the member's roles at the station (location roles count a Base
// role only at the Base) and applies CorpHangarTakeRoleAllowed.
func CorpHangarTakeAllowed(ctx context.Context, db *sql.DB, corpID, charID, stationID, flag int64) bool {
	var ceoID int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceoID)
	return CorpHangarTakeRoleAllowed(MemberRolesAt(ctx, db, charID, corpID, stationID), ceoID == charID, flag)
}

// CorpJobRoleAllowed is the pure role rule: may a member holding roles use a print
// (or materials) in division flag for a corporation job?
func CorpJobRoleAllowed(roles uint64, isCEO bool, flag int64) bool {
	if isCEO || roles&corp.RoleDirector != 0 {
		return true
	}
	q := CorpHangarQueryRole(flag)
	return q != 0 && roles&corp.RoleFactoryManager != 0 && roles&q != 0
}

// CorpJobAllowed reads the member's roles and applies CorpJobRoleAllowed.
func CorpJobAllowed(ctx context.Context, db *sql.DB, corpID, charID, flag int64) bool {
	return CorpJobAllowedAt(ctx, db, corpID, charID, 0, flag)
}

// CorpJobAllowedAt is CorpJobAllowed for a job at one station: with location roles on
// (locationroles.go) a role held at Base counts only at the Base.
func CorpJobAllowedAt(ctx context.Context, db *sql.DB, corpID, charID, stationID, flag int64) bool {
	var ceoID int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceoID)
	return CorpJobRoleAllowed(MemberRolesAt(ctx, db, charID, corpID, stationID), ceoID == charID, flag)
}

// StationCorpJob is a validated station corporation job: whose, which office, and
// the three division flags it works with.
type StationCorpJob struct {
	CorpID   int64
	OfficeID int64
	BPFlag   int64 // where the blueprint sits and returns to
	MatFlag  int64 // where the materials are consumed from
	OutFlag  int64 // where the output is delivered
}

// Station corporation job refusals. The S&I window answers some with the client's own
// message (server/ram ramWhyMessage); the rest go out as plain text.
const (
	WhyCorpJobNoOffice   = "Your corporation has no office at this station."
	WhyCorpJobBPLocation = "The blueprint must be in your corporation's hangar at this station."
	WhyCorpJobBPRole     = "You need the Factory Manager role and access to the hangar division holding the blueprint."
	WhyCorpJobMatRole    = "You do not have access to the hangar division holding the materials."
)

// ResolveStationCorpJob validates a station corporation job and returns its shape, or
// a refusal worded for the pilot. matFlag 0 means the blueprint's own division and
// outFlag 0 means division 1.
func ResolveStationCorpJob(ctx context.Context, db *sql.DB, charID, stationID, bpOwnerID, bpLocationID, bpFlag,
	matFlag, outFlag int64) (StationCorpJob, string) {
	var j StationCorpJob
	if err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&j.CorpID); err != nil || j.CorpID == 0 {
		return j, "You are not in a corporation."
	}
	officeID, _, err := GetCorpOfficeAt(ctx, db, j.CorpID, stationID)
	if err != nil || officeID == 0 {
		return j, WhyCorpJobNoOffice
	}
	j.OfficeID = officeID
	if bpOwnerID != j.CorpID || bpLocationID != officeID || !IsCorpHangarFlag(bpFlag) {
		return j, WhyCorpJobBPLocation
	}
	if !CorpJobAllowedAt(ctx, db, j.CorpID, charID, stationID, bpFlag) {
		return j, WhyCorpJobBPRole
	}
	j.BPFlag = bpFlag
	j.MatFlag = bpFlag
	if matFlag != 0 {
		if !IsCorpHangarFlag(matFlag) {
			return j, "Materials must come from one of your corporation's hangar divisions."
		}
		if !CorpJobAllowedAt(ctx, db, j.CorpID, charID, stationID, matFlag) {
			return j, WhyCorpJobMatRole
		}
		j.MatFlag = matFlag
	}
	j.OutFlag = FlagCorpHangarDiv1
	if IsCorpHangarFlag(outFlag) {
		j.OutFlag = outFlag
	}
	return j, ""
}

// TryChargeCorpISK debits a corporation's wallet inside tx, refusing (ok = false, no
// error) rather than going negative.
func TryChargeCorpISK(ctx context.Context, tx DBTX, corpID int64, amount float64) (ok bool, newBalance float64, err error) {
	if amount <= 0 {
		err = tx.QueryRowContext(ctx,
			`SELECT balance FROM corporation WHERE corporationID = ?`, corpID).Scan(&newBalance)
		return err == nil, newBalance, err
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE corporation SET balance = balance - ?
		 WHERE corporationID = ? AND balance >= ?`, amount, corpID, amount)
	if err != nil {
		return false, 0, fmt.Errorf("TryChargeCorpISK: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return false, 0, nil
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM corporation WHERE corporationID = ?`, corpID).Scan(&newBalance); err != nil {
		return false, 0, fmt.Errorf("TryChargeCorpISK balance: %w", err)
	}
	return true, newBalance, nil
}

// SetJobOutputLocation records where a job's output is delivered when that is not
// its container (a station corporation job's office).
func SetJobOutputLocation(ctx context.Context, tx DBTX, jobID, locationID int64) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE ramJobs SET outputLocationID = ? WHERE jobID = ?`, locationID, jobID); err != nil {
		return fmt.Errorf("SetJobOutputLocation: %w", err)
	}
	return nil
}

// StationCorpJobsEnabled is the server_settings switch industry.stationCorpJobs: on
// unless set to 0, false, off or no.
func StationCorpJobsEnabled(ctx context.Context, db *sql.DB) bool {
	v, ok, err := GetServerSetting(ctx, db, "industry.stationCorpJobs")
	if err != nil || !ok {
		return true
	}
	switch v {
	case "0", "false", "off", "no":
		return false
	}
	return true
}
