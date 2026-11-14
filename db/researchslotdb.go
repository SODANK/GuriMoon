package db

// Research Facility slots: Duplicating (activity 6) and Reverse Engineering (activity 7)
// in rented lab slots. The client ships the UI (ui/station/researchfacility,
// ui/station/facilities) but no lobby button; a client patch (cmd/clientpatch) adds it,
// and an unpatched client never reaches these functions.
// Slots and jobs live in their own two tables and never in ramJobs, which the S&I window,
// the RAM sweep and job counts read. Nothing outside this file reads them.
// See docs/research-facility.md#model.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand"
	"strconv"
	"strings"

	"gurimoon/marshal"
	"gurimoon/rates"
)

// ErrNoFreeResearchSlots is RentResearchSlot finding every slot taken.
var ErrNoFreeResearchSlots = errors.New("no research facility slots are free at this station")

// ResearchRefusal is a refusal worded for the pilot.
type ResearchRefusal struct{ Msg string }

func (e *ResearchRefusal) Error() string { return e.Msg }

func researchRefuse(format string, args ...any) error {
	return &ResearchRefusal{Msg: fmt.Sprintf(format, args...)}
}

// ItemChange is one OnItemChange the caller should push: the row to send and
// the change dict (client ix → old value: 3 locationID, 4 flag, 7 quantity).
type ItemChange struct {
	Row    *EntityRow
	Change map[int]int64
}

// ResearchSlotRental is one researchSlotRentals row.
type ResearchSlotRental struct {
	StationID, Flag, RenterID int64
	ForCorp                   bool
	RentedAt, ExpiresAt       int64 // win32
}

// ResearchSlotJob is one researchSlotJobs row.
type ResearchSlotJob struct {
	JobID, StationID, SlotFlag   int64
	InstallerID, OwnerID         int64
	HeldItemID, ItemTypeID       int64
	HeldQty                      int64
	OutputTypeID                 int64
	ActivityID, Runs             int64
	PerRunSeconds                int64
	InstallTime, EndTime         int64 // win32
	InputLocationID, FlagInput   int64
	BOMLocationID, FlagBOM       int64
	OutputLocationID, FlagOutput int64
	Status                       int64
	Successes                    int64
	Chance                       float64
	ItemCategoryID               int64 // joined, not stored
}

// ResearchSlot is one slot at a station: its flag, and the rental and the
// running job when there are any.
type ResearchSlot struct {
	Flag   int64
	Rental *ResearchSlotRental
	Job    *ResearchSlotJob
}

// ── Schema & settings ───────────────────────────────────────────────────────

// EnsureResearchSlotTables creates the two tables.  Idempotent.
func EnsureResearchSlotTables(ctx context.Context, db *sql.DB) error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS researchSlotRentals (
			stationID BIGINT NOT NULL,
			slotFlag  INT    NOT NULL,
			renterID  BIGINT NOT NULL,
			forCorp   TINYINT NOT NULL DEFAULT 0,
			rentedAt  BIGINT NOT NULL,
			expiresAt BIGINT NOT NULL,
			PRIMARY KEY (stationID, slotFlag),
			KEY idx_rsr_expires (expiresAt)
		)`,
		`CREATE TABLE IF NOT EXISTS researchSlotJobs (
			jobID            BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			stationID        BIGINT NOT NULL,
			slotFlag         INT    NOT NULL,
			installerID      BIGINT NOT NULL,
			ownerID          BIGINT NOT NULL,
			heldItemID       BIGINT NOT NULL,
			itemTypeID       BIGINT NOT NULL,
			heldQty          BIGINT NOT NULL,
			outputTypeID     BIGINT NOT NULL,
			activityID       INT    NOT NULL,
			runs             INT    NOT NULL,
			perRunSeconds    BIGINT NOT NULL,
			installTime      BIGINT NOT NULL,
			endTime          BIGINT NOT NULL,
			inputLocationID  BIGINT NOT NULL,
			flagInput        INT    NOT NULL,
			bomLocationID    BIGINT NOT NULL,
			flagBOM          INT    NOT NULL,
			outputLocationID BIGINT NOT NULL,
			flagOutput       INT    NOT NULL,
			chance           DOUBLE NOT NULL,
			status           TINYINT NOT NULL DEFAULT 0,
			successes        INT    NOT NULL DEFAULT 0,
			completedAt      BIGINT NOT NULL DEFAULT 0,
			KEY idx_rsj_slot (stationID, slotFlag, status),
			KEY idx_rsj_due (status, endTime),
			KEY idx_rsj_held (heldItemID)
		)`,
	}
	for _, s := range stmts {
		if _, err := db.ExecContext(ctx, s); err != nil {
			return fmt.Errorf("EnsureResearchSlotTables: %w", err)
		}
	}
	return nil
}

// settingFloat reads a float server setting, clamped, falling back to def.
func settingFloat(ctx context.Context, db *sql.DB, name string, def, min, max float64) float64 {
	v, ok, err := GetServerSetting(ctx, db, name)
	if err != nil || !ok {
		return def
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	if err != nil {
		return def
	}
	if f < min {
		return min
	}
	if f > max {
		return max
	}
	return f
}

// ResearchFacilityEnabled is the server_settings switch research.facilityEnabled: on
// unless set to 0, false, off or no. Off, the slot list is empty and renting refuses.
func ResearchFacilityEnabled(ctx context.Context, db *sql.DB) bool {
	v, ok, err := GetServerSetting(ctx, db, "research.facilityEnabled")
	if err != nil || !ok {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "0", "false", "off", "no":
		return false
	}
	return true
}

// ResearchSlotsPerStation is research.slotsPerStation (default 10, 1-56).
func ResearchSlotsPerStation(ctx context.Context, db *sql.DB) int64 {
	return int64(settingInt(ctx, db, "research.slotsPerStation", 10, 1,
		int(ResearchSlotLast-ResearchSlotFirst+1)))
}

// ResearchSlotRentCost is research.slotRentCost (default 100,000 ISK).
func ResearchSlotRentCost(ctx context.Context, db *sql.DB) float64 {
	return settingFloat(ctx, db, "research.slotRentCost", 100000, 0, 1e12)
}

// ResearchSlotRentDays is research.slotRentDays (default 30).
func ResearchSlotRentDays(ctx context.Context, db *sql.DB) int64 {
	return int64(settingInt(ctx, db, "research.slotRentDays", 30, 1, 365))
}

// ResearchTimeMult is research.timeMult (default 1). It scales every job's duration.
func ResearchTimeMult(ctx context.Context, db *sql.DB) float64 {
	return settingFloat(ctx, db, "research.timeMult", 1, 0.0001, 1000)
}

// ── Station & slots ─────────────────────────────────────────────────────────

// StationHasLaboratory reports whether the station's operation offers the Laboratory
// service, the same mask the lobby gates the button on.
func StationHasLaboratory(ctx context.Context, db *sql.DB, stationID int64) bool {
	bits, err := GetStationItemBits(ctx, db, stationID)
	if err != nil || bits == nil {
		return false
	}
	return bits.ServiceMask&StationServiceLaboratory != 0
}

func stationOwnerAndName(ctx context.Context, q rowQueryer, stationID int64) (owner int64, name string) {
	_ = q.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0), COALESCE(stationName, '') FROM staStations WHERE stationID = ?`,
		stationID).Scan(&owner, &name)
	return owner, name
}

const researchJobCols = `j.jobID, j.stationID, j.slotFlag, j.installerID, j.ownerID, j.heldItemID,
	j.itemTypeID, j.heldQty, j.outputTypeID, j.activityID, j.runs, j.perRunSeconds,
	j.installTime, j.endTime, j.inputLocationID, j.flagInput, j.bomLocationID, j.flagBOM,
	j.outputLocationID, j.flagOutput, j.status, j.successes, j.chance,
	COALESCE(g.categoryID, 0)`

const researchJobFrom = `FROM researchSlotJobs j
	LEFT JOIN invTypes t ON t.typeID = j.itemTypeID
	LEFT JOIN invGroups g ON g.groupID = t.groupID`

func scanResearchJob(sc interface{ Scan(...any) error }) (*ResearchSlotJob, error) {
	var j ResearchSlotJob
	if err := sc.Scan(&j.JobID, &j.StationID, &j.SlotFlag, &j.InstallerID, &j.OwnerID, &j.HeldItemID,
		&j.ItemTypeID, &j.HeldQty, &j.OutputTypeID, &j.ActivityID, &j.Runs, &j.PerRunSeconds,
		&j.InstallTime, &j.EndTime, &j.InputLocationID, &j.FlagInput, &j.BOMLocationID, &j.FlagBOM,
		&j.OutputLocationID, &j.FlagOutput, &j.Status, &j.Successes, &j.Chance,
		&j.ItemCategoryID); err != nil {
		return nil, err
	}
	return &j, nil
}

// ListResearchSlots answers every slot at a station: the configured count plus any
// higher slot still carrying a rental or a job. A rental that has expired with no
// running job reads as free.
func ListResearchSlots(ctx context.Context, db *sql.DB, stationID int64) ([]ResearchSlot, error) {
	now := int64(win32TimeNow())
	rentals := map[int64]*ResearchSlotRental{}
	rows, err := db.QueryContext(ctx, `
		SELECT stationID, slotFlag, renterID, forCorp, rentedAt, expiresAt
		  FROM researchSlotRentals WHERE stationID = ?`, stationID)
	if err != nil {
		return nil, fmt.Errorf("ListResearchSlots rentals: %w", err)
	}
	for rows.Next() {
		var r ResearchSlotRental
		var fc int
		if err := rows.Scan(&r.StationID, &r.Flag, &r.RenterID, &fc, &r.RentedAt, &r.ExpiresAt); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ListResearchSlots scan rental: %w", err)
		}
		r.ForCorp = fc != 0
		rentals[r.Flag] = &r
	}
	rows.Close()

	jobs := map[int64]*ResearchSlotJob{}
	jrows, err := db.QueryContext(ctx, `SELECT `+researchJobCols+` `+researchJobFrom+`
		 WHERE j.stationID = ? AND j.status = ?`, stationID, ResearchJobRunning)
	if err != nil {
		return nil, fmt.Errorf("ListResearchSlots jobs: %w", err)
	}
	for jrows.Next() {
		j, err := scanResearchJob(jrows)
		if err != nil {
			jrows.Close()
			return nil, fmt.Errorf("ListResearchSlots scan job: %w", err)
		}
		jobs[j.SlotFlag] = j
	}
	jrows.Close()

	n := ResearchSlotsPerStation(ctx, db)
	last := ResearchSlotFirst + n - 1
	for f := range rentals {
		if f > last && IsResearchSlotFlag(f) {
			last = f
		}
	}
	for f := range jobs {
		if f > last && IsResearchSlotFlag(f) {
			last = f
		}
	}
	out := make([]ResearchSlot, 0, last-ResearchSlotFirst+1)
	for f := ResearchSlotFirst; f <= last; f++ {
		s := ResearchSlot{Flag: f, Job: jobs[f]}
		if r := rentals[f]; r != nil && (r.ExpiresAt > now || s.Job != nil) {
			s.Rental = r
		}
		out = append(out, s)
	}
	return out, nil
}

// GetResearchSlot answers one slot, or nil when flag is not a slot here.
func GetResearchSlot(ctx context.Context, db *sql.DB, stationID, flag int64) (*ResearchSlot, error) {
	slots, err := ListResearchSlots(ctx, db, stationID)
	if err != nil {
		return nil, err
	}
	for i := range slots {
		if slots[i].Flag == flag {
			return &slots[i], nil
		}
	}
	return nil, nil
}

// View answers how the slot reads to a pilot.
func (s *ResearchSlot) View(charID, corpID int64) ResearchSlotView {
	if s.Rental == nil {
		return ResearchSlotViewFor(false, 0, false, charID, corpID)
	}
	return ResearchSlotViewFor(true, s.Rental.RenterID, s.Rental.ForCorp, charID, corpID)
}

func researchBool(b bool) marshal.PyRep {
	if b {
		return marshal.PyInt{Value: 1}
	}
	return marshal.PyInt{Value: 0}
}

func flagOrNone(f int64) marshal.PyRep {
	if f == 0 {
		return marshal.PyNone{}
	}
	return intRep(f)
}

// ResearchSlotRow builds the util.KeyVal the facility window reads (facility.py
// FacilityEntry / DisplayFactory). installedItem is None for a pilot who may not see it,
// because the client treats any non-None item as viewable. The three flags must be None
// or a hangar/division flag: OnDisplayInstalledItem indexes divisionByFlag with them.
func ResearchSlotRow(s *ResearchSlot, charID, corpID int64) marshal.PyRep {
	v := s.View(charID, corpID)
	var installed marshal.PyRep = marshal.PyNone{}
	var installer, fIn, fBOM, fOut marshal.PyRep = marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}, marshal.PyNone{}
	if s.Job != nil && v.AccessAllowed {
		j := s.Job
		installed = makeKeyVal(
			[]string{"itemID", "typeID", "ownerID", "categoryID", "quantity"},
			[]marshal.PyRep{intRep(j.HeldItemID), intRep(j.ItemTypeID), intRep(j.OwnerID),
				intRep(j.ItemCategoryID), intRep(j.HeldQty)})
		installer = intRep(j.InstallerID)
		fIn, fBOM, fOut = flagOrNone(j.FlagInput), flagOrNone(j.FlagBOM), flagOrNone(j.FlagOutput)
	}
	var owner, expiry marshal.PyRep = marshal.PyNone{}, marshal.PyNone{}
	if s.Rental != nil {
		owner = intRep(s.Rental.RenterID)
		expiry = intRep(s.Rental.ExpiresAt)
	}
	return makeKeyVal(
		[]string{"slotID", "canUse", "accessAllowed", "forCorporateUse", "rented", "publiclyAvailable",
			"installedItem", "installerID", "flagInstalledItem", "flagBOM", "flagOutput",
			"ownerID", "expiryTime"},
		[]marshal.PyRep{intRep(s.Flag), researchBool(v.CanUse), researchBool(v.AccessAllowed), researchBool(v.ForCorp),
			researchBool(v.Rented), researchBool(v.Public), installed, installer, fIn, fBOM, fOut, owner, expiry})
}

// AnyResearchSlotFree reports whether a slot is free to rent at a station.
func AnyResearchSlotFree(ctx context.Context, db *sql.DB, stationID int64) bool {
	slots, err := ListResearchSlots(ctx, db, stationID)
	if err != nil {
		return false
	}
	n := ResearchSlotsPerStation(ctx, db)
	for _, s := range slots {
		if s.Flag < ResearchSlotFirst+n && s.Rental == nil && s.Job == nil {
			return true
		}
	}
	return false
}

// ── Rental ──────────────────────────────────────────────────────────────────

// RentResearchSlot rents the lowest free slot to payerID (a character, or a corporation
// when forCorp) for days, paying cost to the station owner in the same transaction.
// Returns the slot flag.
func RentResearchSlot(ctx context.Context, db *sql.DB, stationID, payerID int64, forCorp bool, cost float64, days int64) (int64, error) {
	if err := EnsureResearchSlotTables(ctx, db); err != nil {
		return 0, err
	}
	n := ResearchSlotsPerStation(ctx, db)
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("RentResearchSlot begin: %w", err)
	}
	defer tx.Rollback()

	now := int64(win32TimeNow())
	// Expired rentals with nothing running in them are free; clear them first
	// so the primary key does not refuse the new one.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM researchSlotRentals
		 WHERE stationID = ? AND expiresAt <= ?
		   AND NOT EXISTS (SELECT 1 FROM researchSlotJobs j
		                    WHERE j.stationID = researchSlotRentals.stationID
		                      AND j.slotFlag = researchSlotRentals.slotFlag AND j.status = ?)`,
		stationID, now, ResearchJobRunning); err != nil {
		return 0, fmt.Errorf("RentResearchSlot expire: %w", err)
	}
	used := map[int64]bool{}
	rows, err := tx.QueryContext(ctx,
		`SELECT slotFlag FROM researchSlotRentals WHERE stationID = ? FOR UPDATE`, stationID)
	if err != nil {
		return 0, fmt.Errorf("RentResearchSlot used: %w", err)
	}
	for rows.Next() {
		var f int64
		if err := rows.Scan(&f); err != nil {
			rows.Close()
			return 0, err
		}
		used[f] = true
	}
	rows.Close()
	var flag int64
	for f := ResearchSlotFirst; f < ResearchSlotFirst+n; f++ {
		if !used[f] {
			flag = f
			break
		}
	}
	if flag == 0 {
		return 0, ErrNoFreeResearchSlots
	}

	owner, stationName := stationOwnerAndName(ctx, tx, stationID)
	if cost > 0 && owner != 0 && owner != payerID {
		reason := fmt.Sprintf("Research facility slot rental at %s", stationName)
		if _, _, err := transferISKTx(ctx, tx, payerID, owner, cost, RefFactorySlotRentalFee, reason); err != nil {
			return 0, err
		}
	}
	fc := 0
	if forCorp {
		fc = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO researchSlotRentals (stationID, slotFlag, renterID, forCorp, rentedAt, expiresAt)
		VALUES (?, ?, ?, ?, ?, ?)`,
		stationID, flag, payerID, fc, now, now+days*86400*win32TicksPerSec); err != nil {
		return 0, fmt.Errorf("RentResearchSlot insert: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("RentResearchSlot commit: %w", err)
	}
	return flag, nil
}

// CancelResearchSlotRental gives a slot up. A personal rental is its renter's; a
// corporation's needs corpManager, decided by the caller. Refused while a job runs; no refund.
func CancelResearchSlotRental(ctx context.Context, db *sql.DB, stationID, flag, charID, corpID int64, corpManager bool) error {
	s, err := GetResearchSlot(ctx, db, stationID, flag)
	if err != nil {
		return err
	}
	if s == nil || s.Rental == nil {
		return researchRefuse("That slot is not rented.")
	}
	r := s.Rental
	switch {
	case !r.ForCorp && r.RenterID != charID:
		return researchRefuse("That slot is not rented to you.")
	case r.ForCorp && (r.RenterID != corpID || !corpManager):
		return researchRefuse("Only a Director, or a member who may rent research slots, can give up your corporation's slot.")
	}
	if s.Job != nil {
		return researchRefuse("A job is running in that slot. Uninstall it first.")
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM researchSlotRentals WHERE stationID = ? AND slotFlag = ?`, stationID, flag); err != nil {
		return fmt.Errorf("CancelResearchSlotRental: %w", err)
	}
	return nil
}

// ExpireResearchRentals removes rentals past their term with nothing running
// in them and answers what it removed (the sweep tells docked pilots).
func ExpireResearchRentals(ctx context.Context, db *sql.DB) ([]ResearchSlotRental, error) {
	now := int64(win32TimeNow())
	rows, err := db.QueryContext(ctx, `
		SELECT r.stationID, r.slotFlag, r.renterID, r.forCorp, r.rentedAt, r.expiresAt
		  FROM researchSlotRentals r
		 WHERE r.expiresAt <= ?
		   AND NOT EXISTS (SELECT 1 FROM researchSlotJobs j
		                    WHERE j.stationID = r.stationID AND j.slotFlag = r.slotFlag AND j.status = ?)`,
		now, ResearchJobRunning)
	if err != nil {
		return nil, fmt.Errorf("ExpireResearchRentals: %w", err)
	}
	var out []ResearchSlotRental
	for rows.Next() {
		var r ResearchSlotRental
		var fc int
		if err := rows.Scan(&r.StationID, &r.Flag, &r.RenterID, &fc, &r.RentedAt, &r.ExpiresAt); err != nil {
			rows.Close()
			return nil, err
		}
		r.ForCorp = fc != 0
		out = append(out, r)
	}
	rows.Close()
	var gone []ResearchSlotRental
	for _, r := range out {
		res, err := db.ExecContext(ctx, `
			DELETE FROM researchSlotRentals
			 WHERE stationID = ? AND slotFlag = ? AND expiresAt <= ?
			   AND NOT EXISTS (SELECT 1 FROM researchSlotJobs j
			                    WHERE j.stationID = ? AND j.slotFlag = ? AND j.status = ?)`,
			r.StationID, r.Flag, now, r.StationID, r.Flag, ResearchJobRunning)
		if err != nil {
			return gone, fmt.Errorf("ExpireResearchRentals delete: %w", err)
		}
		if n, _ := res.RowsAffected(); n > 0 {
			gone = append(gone, r)
		}
	}
	return gone, nil
}

// ── Install ─────────────────────────────────────────────────────────────────

// ResearchInstallRequest is InstallItemInSlot's arguments plus the caller.
type ResearchInstallRequest struct {
	StationID, CharID, CorpID int64
	ShipID                    int64 // the active ship, never installable
	ItemID, SlotFlag          int64
	Runs, ActivityID          int64
	FlagBOM, FlagOutput       int64
}

// ResearchInstallResult is what the caller pushes after a successful install.
type ResearchInstallResult struct {
	Job     *ResearchSlotJob
	Changes []ItemChange
}

type researchMaterial struct {
	typeID   int64
	quantity int64
	damage   float64
	isSkill  bool
}

func researchMaterials(ctx context.Context, q rowQueryer, itemTypeID, activityID int64) ([]researchMaterial, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT m.requiredTypeID, m.quantity, m.damagePerJob, (COALESCE(g.categoryID, 0) = 16)
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.typeID = ? AND m.activity = ? AND m.recycle = 0
		 ORDER BY m.requiredTypeID`, itemTypeID, activityID)
	if err != nil {
		return nil, fmt.Errorf("researchMaterials: %w", err)
	}
	defer rows.Close()
	var out []researchMaterial
	for rows.Next() {
		var m researchMaterial
		var sk int
		if err := rows.Scan(&m.typeID, &m.quantity, &m.damage, &sk); err != nil {
			return nil, fmt.Errorf("researchMaterials scan: %w", err)
		}
		m.isSkill = sk != 0
		out = append(out, m)
	}
	return out, rows.Err()
}

// researchSource answers the base chance, the per-run base seconds and the
// output type for an item and activity.  Duplicating borrows the item's own
// blueprint production time; Reverse Engineering its copy time, and needs a
// blueprint to exist at all (the blueprint is the output).
func researchSource(ctx context.Context, q rowQueryer, itemTypeID, activityID int64) (base float64, seconds, outputTypeID int64, err error) {
	var bpTypeID, prodTime, copyTime sql.NullInt64
	var reChance sql.NullFloat64
	e := q.QueryRowContext(ctx, `
		SELECT blueprintTypeID, productionTime, researchCopyTime, chanceOfReverseEngineering
		  FROM invBlueprintTypes WHERE productTypeID = ?
		 ORDER BY blueprintTypeID LIMIT 1`, itemTypeID).Scan(&bpTypeID, &prodTime, &copyTime, &reChance)
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return 0, 0, 0, fmt.Errorf("researchSource blueprint: %w", e)
	}
	if activityID == ActivityDuplicating {
		var dup sql.NullFloat64
		if err := q.QueryRowContext(ctx,
			`SELECT chanceOfDuplicating FROM invTypes WHERE typeID = ?`, itemTypeID).Scan(&dup); err != nil {
			return 0, 0, 0, fmt.Errorf("researchSource type: %w", err)
		}
		return dup.Float64, prodTime.Int64, itemTypeID, nil
	}
	if !bpTypeID.Valid {
		return 0, 0, 0, nil
	}
	return reChance.Float64, copyTime.Int64, bpTypeID.Int64, nil
}

// InstallResearchJob validates and installs a Duplicating or Reverse
// Engineering job in one transaction: materials consumed, the item moved into
// the slot, the job row written.
func InstallResearchJob(ctx context.Context, db *sql.DB, req ResearchInstallRequest) (*ResearchInstallResult, error) {
	if req.ActivityID != ActivityDuplicating && req.ActivityID != ActivityReverseEngineering {
		return nil, researchRefuse("This research facility only duplicates and reverse engineers items. " +
			"Manufacturing, copying and research run in the Science & Industry window.")
	}
	slot, err := GetResearchSlot(ctx, db, req.StationID, req.SlotFlag)
	if err != nil {
		return nil, err
	}
	if slot == nil {
		return nil, researchRefuse("That is not a research facility slot at this station.")
	}
	if !slot.View(req.CharID, req.CorpID).CanUse {
		return nil, researchRefuse("You have not rented that slot.")
	}
	if slot.Job != nil {
		return nil, researchRefuse("Something is already installed in that slot.")
	}
	forCorp := slot.Rental.ForCorp

	item, err := GetEntityRow(ctx, db, req.ItemID)
	if err != nil {
		return nil, err
	}
	if item == nil {
		return nil, researchRefuse("That item no longer exists.")
	}
	if item.ItemID == req.ShipID {
		return nil, researchRefuse("You cannot install the ship you are flying.")
	}
	if !ResearchItemCategoryAllowed(item.CategoryID) {
		return nil, researchRefuse("Only ships, modules, charges, drones and implants can be installed here.")
	}

	// Where things come from and go to.
	var ownerID, inputLoc, bomLoc, outLoc, flagBOM, flagOut int64
	if forCorp {
		officeID, _, err := GetCorpOfficeAt(ctx, db, req.CorpID, req.StationID)
		if err != nil || officeID == 0 {
			return nil, researchRefuse("Your corporation has no office at this station.")
		}
		if item.OwnerID != req.CorpID || item.LocationID != officeID || !IsCorpHangarFlag(item.Flag) {
			return nil, researchRefuse("A corporation slot takes its item from your corporation's hangar at this station.")
		}
		if !CorpJobAllowed(ctx, db, req.CorpID, req.CharID, item.Flag) {
			return nil, researchRefuse("You need the Factory Manager role and access to the hangar division holding the item.")
		}
		flagBOM = item.Flag
		if IsCorpHangarFlag(req.FlagBOM) {
			flagBOM = req.FlagBOM
		}
		if !CorpJobAllowed(ctx, db, req.CorpID, req.CharID, flagBOM) {
			return nil, researchRefuse("You do not have access to the hangar division holding the materials.")
		}
		flagOut = FlagCorpHangarDiv1
		if IsCorpHangarFlag(req.FlagOutput) {
			flagOut = req.FlagOutput
		}
		ownerID, inputLoc, bomLoc, outLoc = req.CorpID, officeID, officeID, officeID
	} else {
		if item.OwnerID != req.CharID || item.LocationID != req.StationID || item.Flag != 4 {
			return nil, researchRefuse("The item must be in your hangar at this station.")
		}
		ownerID, inputLoc, bomLoc, outLoc = req.CharID, req.StationID, req.StationID, req.StationID
		flagBOM, flagOut = 4, 4
	}
	if item.Singleton != 0 {
		var n int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity WHERE locationID = ?`, item.ItemID).Scan(&n)
		if n > 0 {
			return nil, researchRefuse("Empty the %s first — nothing may be fitted to or stored in it.", typeNameOf(ctx, db, item.TypeID))
		}
	}

	runs := ClampResearchRuns(req.Runs)
	heldQty := int64(1)
	if req.ActivityID == ActivityReverseEngineering {
		heldQty = runs
	}
	if item.Quantity < heldQty {
		return nil, researchRefuse("Reverse engineering takes one %s per run: you have %d, and asked for %d runs.",
			typeNameOf(ctx, db, item.TypeID), item.Quantity, runs)
	}

	base, baseSeconds, outputTypeID, err := researchSource(ctx, db, item.TypeID, req.ActivityID)
	if err != nil {
		return nil, err
	}
	if outputTypeID == 0 {
		return nil, researchRefuse("No blueprint exists for %s, so it cannot be reverse engineered.", typeNameOf(ctx, db, item.TypeID))
	}
	levels, err := GetCharacterSkillLevels(ctx, db, req.CharID)
	if err != nil {
		return nil, err
	}
	if levels[SkillScience] < 1 {
		return nil, researchRefuse("You need the Science skill to use a research facility.")
	}
	skill := ResearchActivitySkill(req.ActivityID)
	chance := ResearchSuccessChance(base, levels[skill])
	if chance <= 0 {
		if levels[skill] < 1 {
			return nil, researchRefuse("You need the %s skill for this job.", typeNameOf(ctx, db, skill))
		}
		return nil, researchRefuse("%s cannot be %s.", typeNameOf(ctx, db, item.TypeID),
			map[int64]string{ActivityDuplicating: "duplicated", ActivityReverseEngineering: "reverse engineered"}[req.ActivityID])
	}

	mats, err := researchMaterials(ctx, db, item.TypeID, req.ActivityID)
	if err != nil {
		return nil, err
	}
	for _, m := range mats {
		if m.isSkill && int64(levels[m.typeID]) < m.quantity {
			return nil, researchRefuse("This job requires %s at level %d.", typeNameOf(ctx, db, m.typeID), m.quantity)
		}
	}

	perRun := ResearchRunSeconds(baseSeconds, ResearchTimeMult(ctx, db), rates.IndustrySpeed, rates.ResearchSpeed)
	now := int64(win32TimeNow())
	end := now + runs*perRun*win32TicksPerSec

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("InstallResearchJob begin: %w", err)
	}
	defer tx.Rollback()

	// Two installs into one slot at once: the rental row is the lock.
	var lockFlag int64
	if err := tx.QueryRowContext(ctx,
		`SELECT slotFlag FROM researchSlotRentals WHERE stationID = ? AND slotFlag = ? FOR UPDATE`,
		req.StationID, req.SlotFlag).Scan(&lockFlag); err != nil {
		return nil, researchRefuse("You have not rented that slot.")
	}
	var busy int
	_ = tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM researchSlotJobs WHERE stationID = ? AND slotFlag = ? AND status = ?`,
		req.StationID, req.SlotFlag, ResearchJobRunning).Scan(&busy)
	if busy > 0 {
		return nil, researchRefuse("Something is already installed in that slot.")
	}

	var consumed []ConsumedStack
	var missing []string
	for _, m := range mats {
		if m.isSkill {
			continue
		}
		consume, present := ResearchMaterialNeed(m.quantity, m.damage, runs)
		if present > 0 {
			var have sql.NullInt64
			_ = tx.QueryRowContext(ctx, `
				SELECT SUM(quantity) FROM entity
				 WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = ? AND itemID <> ?`,
				m.typeID, ownerID, bomLoc, flagBOM, item.ItemID).Scan(&have)
			if have.Int64 < present {
				missing = append(missing, fmt.Sprintf("%d x %s", present-have.Int64, typeNameOf(ctx, tx, m.typeID)))
			}
			continue
		}
		if consume <= 0 {
			continue
		}
		used, short, err := consumeStacks(ctx, tx, m.typeID, ownerID, bomLoc, flagBOM, item.ItemID, consume)
		if err != nil {
			return nil, err
		}
		consumed = append(consumed, used...)
		if short > 0 {
			missing = append(missing, fmt.Sprintf("%d x %s", short, typeNameOf(ctx, tx, m.typeID)))
		}
	}
	if len(missing) > 0 {
		return nil, &ErrResearchIngredients{Minerals: strings.Join(missing, ", ")}
	}

	// Hold the item in the slot: the whole stack when it is exactly what the
	// job takes, otherwise split the job's units off it.
	var cur int64
	var curLoc, curFlag, curOwner int64
	if err := tx.QueryRowContext(ctx,
		`SELECT quantity, locationID, flag, ownerID FROM entity WHERE itemID = ? FOR UPDATE`, item.ItemID,
	).Scan(&cur, &curLoc, &curFlag, &curOwner); err != nil {
		return nil, researchRefuse("That item no longer exists.")
	}
	if curLoc != item.LocationID || curFlag != item.Flag || curOwner != item.OwnerID || cur < heldQty {
		return nil, researchRefuse("That item moved while the job was being set up. Try again.")
	}
	heldID := item.ItemID
	split := cur > heldQty
	if split {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, heldQty, item.ItemID); err != nil {
			return nil, fmt.Errorf("InstallResearchJob split: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			SELECT itemName, typeID, ownerID, ?, ?, contraband, 0, ?, 0, 0, 0, NULL
			  FROM entity WHERE itemID = ?`,
			req.StationID, req.SlotFlag, heldQty, item.ItemID)
		if err != nil {
			return nil, fmt.Errorf("InstallResearchJob hold: %w", err)
		}
		if heldID, err = res.LastInsertId(); err != nil {
			return nil, fmt.Errorf("InstallResearchJob hold id: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ?`,
		req.StationID, req.SlotFlag, item.ItemID); err != nil {
		return nil, fmt.Errorf("InstallResearchJob move: %w", err)
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO researchSlotJobs
		  (stationID, slotFlag, installerID, ownerID, heldItemID, itemTypeID, heldQty, outputTypeID,
		   activityID, runs, perRunSeconds, installTime, endTime, inputLocationID, flagInput,
		   bomLocationID, flagBOM, outputLocationID, flagOutput, chance, status)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		req.StationID, req.SlotFlag, req.CharID, ownerID, heldID, item.TypeID, heldQty, outputTypeID,
		req.ActivityID, runs, perRun, now, end, inputLoc, item.Flag,
		bomLoc, flagBOM, outLoc, flagOut, chance, ResearchJobRunning)
	if err != nil {
		return nil, fmt.Errorf("InstallResearchJob insert: %w", err)
	}
	jobID, _ := res.LastInsertId()

	// Pre-commit snapshots: the pool still sees the rows as they were.
	changes := deletedOrShrunk(ctx, db, consumed)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("InstallResearchJob commit: %w", err)
	}

	if split {
		if er, _ := GetEntityRow(ctx, db, item.ItemID); er != nil {
			changes = append(changes, ItemChange{Row: er, Change: map[int]int64{7: cur}})
		}
	} else if er, _ := GetEntityRow(ctx, db, item.ItemID); er != nil {
		changes = append(changes, ItemChange{Row: er, Change: map[int]int64{3: item.LocationID, 4: item.Flag}})
	}

	job := &ResearchSlotJob{
		JobID: jobID, StationID: req.StationID, SlotFlag: req.SlotFlag, InstallerID: req.CharID,
		OwnerID: ownerID, HeldItemID: heldID, ItemTypeID: item.TypeID, HeldQty: heldQty,
		OutputTypeID: outputTypeID, ActivityID: req.ActivityID, Runs: runs, PerRunSeconds: perRun,
		InstallTime: now, EndTime: end, InputLocationID: inputLoc, FlagInput: item.Flag,
		BOMLocationID: bomLoc, FlagBOM: flagBOM, OutputLocationID: outLoc, FlagOutput: flagOut,
		Chance: chance, ItemCategoryID: item.CategoryID,
	}
	return &ResearchInstallResult{Job: job, Changes: changes}, nil
}

// ErrResearchIngredients is a job short of materials; the handler raises the
// client's own IngredientsMissingForBlueprint, whose {minerals} the facility
// window prints as a hint (facility.py InstallItemInSlot).
type ErrResearchIngredients struct{ Minerals string }

func (e *ErrResearchIngredients) Error() string { return "missing materials: " + e.Minerals }

func typeNameOf(ctx context.Context, q DBTX, typeID int64) string {
	return resolveTypeName(ctx, q, typeID)
}

// consumeStacks takes need units of typeID from (owner, location, flag),
// smallest stacks first, never touching skipItemID (the job's own item).
// Answers what it took and how many it could not find.
func consumeStacks(ctx context.Context, tx *sql.Tx, typeID, ownerID, locationID, flag, skipItemID, need int64) ([]ConsumedStack, int64, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT itemID, quantity FROM entity
		 WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = ? AND itemID <> ?
		 ORDER BY quantity ASC FOR UPDATE`, typeID, ownerID, locationID, flag, skipItemID)
	if err != nil {
		return nil, 0, fmt.Errorf("consumeStacks: %w", err)
	}
	type stack struct{ id, qty int64 }
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.id, &s.qty); err != nil {
			rows.Close()
			return nil, 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	var out []ConsumedStack
	remaining := need
	for _, s := range stacks {
		if remaining <= 0 {
			break
		}
		if s.qty <= remaining {
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.id); err != nil {
				return nil, 0, fmt.Errorf("consumeStacks delete: %w", err)
			}
			out = append(out, ConsumedStack{ItemID: s.id, TypeID: typeID, OldQty: s.qty, NewQty: 0})
			remaining -= s.qty
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, remaining, s.id); err != nil {
			return nil, 0, fmt.Errorf("consumeStacks decrement: %w", err)
		}
		out = append(out, ConsumedStack{ItemID: s.id, TypeID: typeID, OldQty: s.qty, NewQty: s.qty - remaining})
		remaining = 0
	}
	return out, remaining, nil
}

// deletedOrShrunk turns consumed stacks into item changes, reading the rows
// through the pool (pre-commit, so a deleted row is still visible).
func deletedOrShrunk(ctx context.Context, db *sql.DB, consumed []ConsumedStack) []ItemChange {
	var out []ItemChange
	for _, c := range consumed {
		er, _ := GetEntityRow(ctx, db, c.ItemID)
		if er == nil {
			continue
		}
		row := *er
		if c.NewQty == 0 {
			oldLoc := row.LocationID
			row.LocationID, row.Quantity = 6, 0
			out = append(out, ItemChange{Row: &row, Change: map[int]int64{3: oldLoc, 7: c.OldQty}})
			continue
		}
		row.Quantity = c.NewQty
		out = append(out, ItemChange{Row: &row, Change: map[int]int64{7: c.OldQty}})
	}
	return out
}

// ── Status ──────────────────────────────────────────────────────────────────

// GetRunningResearchJobByItem answers the running job holding itemID.
func GetRunningResearchJobByItem(ctx context.Context, db *sql.DB, itemID int64) (*ResearchSlotJob, error) {
	j, err := scanResearchJob(db.QueryRowContext(ctx, `SELECT `+researchJobCols+` `+researchJobFrom+`
		 WHERE j.heldItemID = ? AND j.status = ? LIMIT 1`, itemID, ResearchJobRunning))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return j, err
}

// ResearchFactoryStatus builds GetFactoryStatus's dict (facility.py DisplayFactory reads
// every key). Seconds are ints: the client's FormatTime relies on integer division.
func ResearchFactoryStatus(ctx context.Context, db *sql.DB, j *ResearchSlotJob) marshal.PyRep {
	now := int64(win32TimeNow())
	total := j.Runs * j.PerRunSeconds
	remaining := (j.EndTime - now) / win32TicksPerSec
	if remaining < 0 {
		remaining = 0
	}
	if remaining > total {
		remaining = total
	}
	batchRemaining := int64(0)
	if remaining > 0 && j.PerRunSeconds > 0 {
		batchRemaining = (remaining-1)%j.PerRunSeconds + 1
	}

	var bom []marshal.PyRep
	if mats, err := researchMaterials(ctx, db, j.ItemTypeID, j.ActivityID); err == nil {
		for _, m := range mats {
			qty := m.quantity
			if !m.isSkill && m.damage >= 1 {
				qty = m.quantity * j.Runs
			}
			bom = append(bom, makeKeyVal(
				[]string{"requiredTypeID", "isSkillCheck", "quantity", "requiresHP", "damagePerJob"},
				[]marshal.PyRep{intRep(m.typeID), researchBool(m.isSkill), intRep(qty), intRep(0), floatRep(m.damage)}))
		}
	}
	content := []marshal.PyRep{makeKeyVal(
		[]string{"typeID", "quantity", "itemID", "health", "showHealth"},
		[]marshal.PyRep{intRep(j.ItemTypeID), intRep(j.HeldQty), intRep(j.HeldItemID), floatRep(1.0), intRep(0)})}

	kv := func(k string, v marshal.PyRep) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: k}, Value: v}
	}
	return marshal.PyDict{Items: []marshal.DictEntry{
		kv("activity", intRep(j.ActivityID)),
		kv("secondsTotal", intRep(total)),
		kv("secondsRemaining", intRep(remaining)),
		kv("batchSecondsTotal", intRep(j.PerRunSeconds)),
		kv("batchSecondsRemaining", intRep(batchRemaining)),
		kv("iterationsRequested", intRep(j.Runs)),
		kv("iterationsCompleted", intRep(ResearchIterationsCompleted(j.Runs, j.PerRunSeconds, total-remaining))),
		kv("billOfMaterials", marshal.PyList{Items: bom}),
		kv("batchProductionTime", intRep(j.PerRunSeconds)),
		kv("wastageFactor", intRep(0)),
		kv("content", marshal.PyList{Items: content}),
	}}
}

// ── Completion & eject ──────────────────────────────────────────────────────

// ResearchCompletion is one finished job, for the caller's pushes and mail.
type ResearchCompletion struct {
	Job         *ResearchSlotJob
	Successes   int64
	StationName string
	Changes     []ItemChange
}

// DueResearchJobs answers the running jobs whose time is up.
func DueResearchJobs(ctx context.Context, db *sql.DB) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT jobID FROM researchSlotJobs WHERE status = ? AND endTime <= ? ORDER BY endTime LIMIT 500`,
		ResearchJobRunning, int64(win32TimeNow()))
	if err != nil {
		return nil, fmt.Errorf("DueResearchJobs: %w", err)
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CompleteResearchJob rolls a due job and delivers it in one transaction. Answers nil,
// nil when the job is not or no longer due, so the sweep can run it twice safely.
func CompleteResearchJob(ctx context.Context, db *sql.DB, jobID int64, rnd func() float64) (*ResearchCompletion, error) {
	if rnd == nil {
		rnd = rand.Float64
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("CompleteResearchJob begin: %w", err)
	}
	defer tx.Rollback()

	// Lock the job row alone: FOR UPDATE over the type/group join would lock
	// those shared invTypes/invGroups rows too and serialise every reader.
	var locked int64
	err = tx.QueryRowContext(ctx,
		`SELECT jobID FROM researchSlotJobs WHERE jobID = ? AND status = ? AND endTime <= ? FOR UPDATE`,
		jobID, ResearchJobRunning, int64(win32TimeNow())).Scan(&locked)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("CompleteResearchJob lock: %w", err)
	}
	j, err := scanResearchJob(tx.QueryRowContext(ctx, `SELECT `+researchJobCols+` `+researchJobFrom+`
		 WHERE j.jobID = ?`, jobID))
	if err != nil {
		return nil, fmt.Errorf("CompleteResearchJob load: %w", err)
	}

	successes := ResearchRollSuccesses(j.Runs, j.Chance, rnd)
	var created []int64
	var merged []ItemChange
	if successes > 0 {
		created, merged, err = deliverResearchOutput(ctx, tx, db, j, successes)
		if err != nil {
			return nil, err
		}
	}

	held, _ := GetEntityRow(ctx, db, j.HeldItemID) // pre-commit snapshot
	var ret heldReturn
	if j.ActivityID == ActivityDuplicating {
		if ret, err = returnHeldItem(ctx, tx, db, j); err != nil {
			return nil, fmt.Errorf("CompleteResearchJob return template: %w", err)
		}
	} else if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, j.HeldItemID); err != nil {
		return nil, fmt.Errorf("CompleteResearchJob consume: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE researchSlotJobs SET status = ?, successes = ?, completedAt = ? WHERE jobID = ?`,
		ResearchJobDelivered, successes, int64(win32TimeNow()), j.JobID); err != nil {
		return nil, fmt.Errorf("CompleteResearchJob mark: %w", err)
	}
	_, stationName := stationOwnerAndName(ctx, tx, j.StationID)
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("CompleteResearchJob commit: %w", err)
	}

	changes := merged
	for _, id := range created {
		if er, _ := GetEntityRow(ctx, db, id); er != nil {
			changes = append(changes, ItemChange{Row: er, Change: map[int]int64{3: 0}})
		}
	}
	if held != nil {
		if j.ActivityID == ActivityDuplicating {
			changes = append(changes, ret.changes(ctx, db, j.HeldItemID)...)
		} else {
			gone := *held
			gone.LocationID, gone.Quantity = 6, 0
			changes = append(changes, ItemChange{Row: &gone, Change: map[int]int64{3: held.LocationID, 7: held.Quantity}})
		}
	}
	return &ResearchCompletion{Job: j, Successes: successes, StationName: stationName, Changes: changes}, nil
}

// deliverResearchOutput creates the job's output: copies of a duplicated item
// (merged into a stack at the output when the type stacks), or one original
// blueprint (ME 0 / PE 0) per successful reverse engineering run.
func deliverResearchOutput(ctx context.Context, tx *sql.Tx, db *sql.DB, j *ResearchSlotJob, successes int64) ([]int64, []ItemChange, error) {
	var catID int64
	_ = tx.QueryRowContext(ctx, `
		SELECT COALESCE(g.categoryID, 0) FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID WHERE t.typeID = ?`, j.OutputTypeID).Scan(&catID)
	singleton := catID == 6 || catID == 9
	if !singleton {
		stackID, qty, err := FindStackAt(ctx, tx, j.OutputTypeID, j.OwnerID, j.OutputLocationID, j.FlagOutput)
		if err != nil {
			return nil, nil, err
		}
		if stackID != 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, successes, stackID); err != nil {
				return nil, nil, fmt.Errorf("deliverResearchOutput merge: %w", err)
			}
			var changes []ItemChange
			if er, _ := GetEntityRow(ctx, db, stackID); er != nil { // pre-commit row
				row := *er
				row.Quantity = qty + successes
				changes = append(changes, ItemChange{Row: &row, Change: map[int]int64{7: qty}})
			}
			return nil, changes, nil
		}
		id, err := CreateItem(ctx, tx, j.OutputTypeID, j.OwnerID, j.OutputLocationID, int(successes))
		if err != nil {
			return nil, nil, err
		}
		if err := setItemFlag(ctx, tx, id, j.FlagOutput); err != nil {
			return nil, nil, err
		}
		return []int64{id}, nil, nil
	}
	var ids []int64
	for i := int64(0); i < successes; i++ {
		id, err := CreateItem(ctx, tx, j.OutputTypeID, j.OwnerID, j.OutputLocationID, 1)
		if err != nil {
			return nil, nil, err
		}
		if err := setItemFlag(ctx, tx, id, j.FlagOutput); err != nil {
			return nil, nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil, nil
}

func setItemFlag(ctx context.Context, tx *sql.Tx, itemID, flag int64) error {
	if flag == 4 {
		return nil // CreateItem's own flag
	}
	if _, err := tx.ExecContext(ctx, `UPDATE entity SET flag = ? WHERE itemID = ?`, flag, itemID); err != nil {
		return fmt.Errorf("setItemFlag: %w", err)
	}
	return nil
}

// EjectResearchJob uninstalls a running job: the held item goes back where it came from
// and materials are lost (the client warns facAcceptEjectMaterialLoss). The caller
// decides mayEject: the job's owner, or the station owner's factory manager.
func EjectResearchJob(ctx context.Context, db *sql.DB, stationID, flag int64, mayEject func(*ResearchSlotJob) bool) (*ResearchSlotJob, []ItemChange, error) {
	slot, err := GetResearchSlot(ctx, db, stationID, flag)
	if err != nil {
		return nil, nil, err
	}
	if slot == nil || slot.Job == nil {
		return nil, nil, researchRefuse("Nothing is installed in that slot.")
	}
	j := slot.Job
	if !mayEject(j) {
		return nil, nil, researchRefuse("You may not uninstall that job.")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, nil, fmt.Errorf("EjectResearchJob begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx,
		`UPDATE researchSlotJobs SET status = ?, completedAt = ? WHERE jobID = ? AND status = ?`,
		ResearchJobEjected, int64(win32TimeNow()), j.JobID, ResearchJobRunning)
	if err != nil {
		return nil, nil, fmt.Errorf("EjectResearchJob mark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, nil, researchRefuse("That job has already finished.")
	}
	ret, err := returnHeldItem(ctx, tx, db, j)
	if err != nil {
		return nil, nil, fmt.Errorf("EjectResearchJob return: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, nil, fmt.Errorf("EjectResearchJob commit: %w", err)
	}
	return j, ret.changes(ctx, db, j.HeldItemID), nil
}

// heldReturn records how returnHeldItem put an item back, for the pushes.
type heldReturn struct {
	pre        *EntityRow // the held row before the return (pool snapshot)
	mergedInto int64      // stack it was folded into, 0 when moved as-is
	mergedOld  int64      // that stack's quantity before
}

// returnHeldItem puts a job's held item back where it came from, folded into a matching
// stack when the type stacks. Runs inside tx.
func returnHeldItem(ctx context.Context, tx *sql.Tx, db *sql.DB, j *ResearchSlotJob) (heldReturn, error) {
	r := heldReturn{}
	r.pre, _ = GetEntityRow(ctx, db, j.HeldItemID)
	if r.pre != nil && r.pre.Singleton == 0 {
		stackID, qty, err := FindStackAt(ctx, tx, j.ItemTypeID, j.OwnerID, j.InputLocationID, j.FlagInput)
		if err != nil {
			return r, err
		}
		if stackID != 0 && stackID != j.HeldItemID {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, r.pre.Quantity, stackID); err != nil {
				return r, fmt.Errorf("returnHeldItem merge: %w", err)
			}
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, j.HeldItemID); err != nil {
				return r, fmt.Errorf("returnHeldItem delete: %w", err)
			}
			r.mergedInto, r.mergedOld = stackID, qty
			return r, nil
		}
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ?`,
		j.InputLocationID, j.FlagInput, j.HeldItemID); err != nil {
		return r, fmt.Errorf("returnHeldItem move: %w", err)
	}
	return r, nil
}

// changes answers the pushes for a committed return.
func (r heldReturn) changes(ctx context.Context, db *sql.DB, heldItemID int64) []ItemChange {
	if r.pre == nil {
		return nil
	}
	if r.mergedInto != 0 {
		gone := *r.pre
		gone.LocationID, gone.Quantity = 6, 0
		out := []ItemChange{{Row: &gone, Change: map[int]int64{3: r.pre.LocationID, 7: r.pre.Quantity}}}
		if er, _ := GetEntityRow(ctx, db, r.mergedInto); er != nil {
			out = append(out, ItemChange{Row: er, Change: map[int]int64{7: r.mergedOld}})
		}
		return out
	}
	if er, _ := GetEntityRow(ctx, db, heldItemID); er != nil {
		return []ItemChange{{Row: er, Change: map[int]int64{3: r.pre.LocationID, 4: r.pre.Flag}}}
	}
	return nil
}
