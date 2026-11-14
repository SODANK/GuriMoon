package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"

	"gurimoon/corp"
	"gurimoon/marshal"
)

// isDupColumn reports whether err is MySQL error 1060 (duplicate column) or
// 1061 (duplicate key name), meaning the migration already ran.
func isDupColumn(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "1060") || strings.Contains(err.Error(), "1061"))
}

// DBTX is satisfied by both *sql.DB and *sql.Tx so job installation can run
// its writes inside a single transaction.
type DBTX interface {
	ExecContext(ctx context.Context, query string, args ...interface{}) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...interface{}) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...interface{}) *sql.Row
}

// EnsureRAMTables creates the invBlueprints and ramJobs tables if they do not
// already exist.  Call once at server startup after opening the DB.
func EnsureRAMTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS invBlueprints (
			blueprintID                    BIGINT NOT NULL PRIMARY KEY,
			materialLevel                  INT NOT NULL DEFAULT 0,
			productivityLevel              INT NOT NULL DEFAULT 0,
			copy                           TINYINT NOT NULL DEFAULT 0,
			licensedProductionRunsRemaining INT NOT NULL DEFAULT -1
		)`)
	if err != nil {
		return fmt.Errorf("EnsureRAMTables invBlueprints: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS ramJobs (
			jobID                    BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
			assemblyLineID           BIGINT NOT NULL DEFAULT 0,
			containerID              BIGINT NOT NULL DEFAULT 0,
			installerID              BIGINT NOT NULL DEFAULT 0,
			installedItemID          BIGINT NOT NULL DEFAULT 0,
			installTime              BIGINT NOT NULL DEFAULT 0,
			beginProductionTime      BIGINT NOT NULL DEFAULT 0,
			endProductionTime        BIGINT NOT NULL DEFAULT 0,
			runs                     INT NOT NULL DEFAULT 1,
			activityID               INT NOT NULL DEFAULT 1,
			outputTypeID             INT NOT NULL DEFAULT 0,
			outputFlag               INT NOT NULL DEFAULT 4,
			completedStatusID        INT NOT NULL DEFAULT 0,
			licensedProductionRuns   INT NOT NULL DEFAULT 0,
			installedInSolarSystemID BIGINT NOT NULL DEFAULT 0,
			INDEX idx_installer (installerID),
			INDEX idx_status (completedStatusID)
		)`)
	if err != nil {
		return fmt.Errorf("EnsureRAMTables ramJobs: %w", err)
	}

	// Add columns missing from tables created by earlier schema versions.
	migrate := []string{
		`ALTER TABLE ramJobs ADD COLUMN completedStatusID INT NOT NULL DEFAULT 0 AFTER outputFlag`,
		`ALTER TABLE ramJobs ADD COLUMN licensedProductionRuns INT NOT NULL DEFAULT 0 AFTER completedStatusID`,
		`ALTER TABLE ramJobs ADD COLUMN installedInSolarSystemID BIGINT NOT NULL DEFAULT 0 AFTER licensedProductionRuns`,
		// POS jobs: ownerID is the owning corporation for a corp job (0 = the installer);
		// bpFlag is the division the blueprint returns to.
		`ALTER TABLE ramJobs ADD COLUMN ownerID BIGINT NOT NULL DEFAULT 0 AFTER installedInSolarSystemID`,
		`ALTER TABLE ramJobs ADD COLUMN bpFlag INT NOT NULL DEFAULT 4 AFTER ownerID`,
		`ALTER TABLE ramJobs ADD INDEX idx_owner (ownerID)`,
		// outputLocationID is the office that receives a station corporation job's output
		// (0 = the container). See docs/db-12-notes.md#station-corporation-jobs.
		`ALTER TABLE ramJobs ADD COLUMN outputLocationID BIGINT NOT NULL DEFAULT 0 AFTER bpFlag`,
	}
	for _, stmt := range migrate {
		if _, err := db.ExecContext(ctx, stmt); err != nil {
			// Error 1060 (duplicate column) is expected once the column exists.
			if !isDupColumn(err) {
				log.Printf("ramdb migrate: %v (stmt: %s)", err, stmt)
			}
		}
	}

	return nil
}

// EnsureBlueprintRow inserts an invBlueprints row for a new blueprint item.
// It does nothing when typeID is not in invBlueprintTypes.
func EnsureBlueprintRow(ctx context.Context, db DBTX, itemID, typeID int64) error {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM invBlueprintTypes WHERE blueprintTypeID = ? LIMIT 1`, typeID).Scan(&exists)
	if err != nil {
		return nil // not a blueprint type
	}
	_, err = db.ExecContext(ctx,
		`INSERT IGNORE INTO invBlueprints (blueprintID, materialLevel, productivityLevel, copy, licensedProductionRunsRemaining)
		 VALUES (?, 0, 0, 0, -1)`, itemID)
	if err != nil {
		return fmt.Errorf("EnsureBlueprintRow: %w", err)
	}
	return nil
}

// FindAssemblyLineForActivity returns the least-busy assembly line at a
// station that can run the given activity (0 when the station has none).
func FindAssemblyLineForActivity(ctx context.Context, db *sql.DB, stationID, activityID int64) (int64, error) {
	var alID int64
	err := db.QueryRowContext(ctx, `
		SELECT al.assemblyLineID
		  FROM ramassemblylines al
		 WHERE al.containerID = ? AND al.activityID = ?
		 ORDER BY (SELECT COALESCE(MAX(j.endProductionTime), 0)
		             FROM ramJobs j
		            WHERE j.assemblyLineID = al.assemblyLineID
		              AND j.completedStatusID = 0) ASC
		 LIMIT 1`, stationID, activityID).Scan(&alID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("FindAssemblyLineForActivity: %w", err)
	}
	return alID, nil
}

// FindAssemblyLineForProduct is FindAssemblyLineForActivity limited to lines whose
// type may build productTypeID (LineCanBuild). Returns 0 when no line qualifies.
// productTypeID 0 accepts any line of the activity.
func FindAssemblyLineForProduct(ctx context.Context, db *sql.DB, stationID, activityID, productTypeID int64) (int64, error) {
	if productTypeID == 0 {
		return FindAssemblyLineForActivity(ctx, db, stationID, activityID)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT al.assemblyLineID, al.assemblyLineTypeID
		  FROM ramassemblylines al
		 WHERE al.containerID = ? AND al.activityID = ?
		 ORDER BY (SELECT COALESCE(MAX(j.endProductionTime), 0)
		             FROM ramJobs j
		            WHERE j.assemblyLineID = al.assemblyLineID
		              AND j.completedStatusID = 0) ASC`, stationID, activityID)
	if err != nil {
		return 0, fmt.Errorf("FindAssemblyLineForProduct: %w", err)
	}
	defer rows.Close()
	canBuild := map[int64]bool{}
	for rows.Next() {
		var alID, typeID int64
		if err := rows.Scan(&alID, &typeID); err != nil {
			return 0, err
		}
		ok, seen := canBuild[typeID]
		if !seen {
			ok = LineCanBuild(ctx, db, typeID, productTypeID)
			canBuild[typeID] = ok
		}
		if ok {
			return alID, nil
		}
	}
	return 0, rows.Err()
}

// EnsureBlueprintSingleton gives one blueprint unit a persistent identity before a
// job: its own entity row and invBlueprints record. A stack (quantity > 1) has one
// unit split off into a new singleton, and the client swaps to the returned ID via
// InstallJob's quote. Returns (installedID, splitOff, oldStackQty).
func EnsureBlueprintSingleton(ctx context.Context, db *sql.DB, blueprintID int64) (int64, bool, int64, error) {
	var qty, singleton, typeID, ownerID, locationID, flag int64
	var itemName string
	err := db.QueryRowContext(ctx, `
		SELECT quantity, singleton, typeID, ownerID, locationID, flag, COALESCE(itemName, '')
		  FROM entity WHERE itemID = ?`, blueprintID).
		Scan(&qty, &singleton, &typeID, &ownerID, &locationID, &flag, &itemName)
	if err != nil {
		return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton load: %w", err)
	}

	if qty <= 1 {
		if singleton == 0 {
			if _, err := db.ExecContext(ctx,
				`UPDATE entity SET singleton = 1 WHERE itemID = ?`, blueprintID); err != nil {
				return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton promote: %w", err)
			}
		}
		if err := EnsureBlueprintRow(ctx, db, blueprintID, typeID); err != nil {
			return 0, false, 0, err
		}
		return blueprintID, false, 0, nil
	}

	// Split one unit off the stack in one transaction.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - 1 WHERE itemID = ?`, blueprintID); err != nil {
		return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton decrement: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, NULL)`,
		itemName, typeID, ownerID, locationID, flag)
	if err != nil {
		return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton insert: %w", err)
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton LastInsertId: %w", err)
	}
	if err := EnsureBlueprintRow(ctx, tx, newID, typeID); err != nil {
		return 0, false, 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, 0, fmt.Errorf("EnsureBlueprintSingleton commit: %w", err)
	}
	return newID, true, qty, nil
}

// BlueprintAttrs holds the per-instance blueprint attributes.
type BlueprintAttrs struct {
	MaterialLevel     int64
	ProductivityLevel int64
	Copy              int
	RunsRemaining     int
}

// GetBlueprintAttributes returns the ME/PE/copy/runs for a blueprint item.
func GetBlueprintAttributes(ctx context.Context, db DBTX, blueprintID int64) (*BlueprintAttrs, error) {
	var a BlueprintAttrs
	err := db.QueryRowContext(ctx,
		`SELECT materialLevel, productivityLevel, copy, licensedProductionRunsRemaining
		   FROM invBlueprints WHERE blueprintID = ?`, blueprintID).
		Scan(&a.MaterialLevel, &a.ProductivityLevel, &a.Copy, &a.RunsRemaining)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("blueprint %d not found in invBlueprints", blueprintID)
		}
		return nil, fmt.Errorf("GetBlueprintAttributes: %w", err)
	}
	return &a, nil
}

// GetBlueprintAttributesRep answers factory.GetBlueprintAttributes. The info window
// reads a plain dict (bpi.has_key, bpi['manufacturingTime']) of per-instance ME/PE
// plus type data, with wastageFactor as a fraction.
func GetBlueprintAttributesRep(ctx context.Context, db *sql.DB, blueprintID int64) (marshal.PyRep, error) {
	a, err := GetBlueprintAttributes(ctx, db, blueprintID)
	if err != nil {
		// No invBlueprints row (seeded item): use default BPO attributes.
		a = &BlueprintAttrs{RunsRemaining: -1}
	}

	var typeID int64
	_ = db.QueryRowContext(ctx,
		`SELECT typeID FROM entity WHERE itemID = ?`, blueprintID).Scan(&typeID)

	bt, btErr := GetBlueprintTypeInfo(ctx, db, typeID)
	if btErr != nil {
		bt = &BlueprintTypeInfo{}
	}

	// The info window applies wastageFactor to base materials as-is, so the ME level
	// is folded in here with the CalcMaterialsNeeded curve.
	wastage := float64(bt.WasteFactor) / 100.0
	if a.MaterialLevel >= 0 {
		wastage /= 1.0 + float64(a.MaterialLevel)
	} else {
		wastage *= 1.0 - float64(a.MaterialLevel)
	}

	e := func(key string, val marshal.PyRep) marshal.DictEntry {
		return marshal.DictEntry{Key: marshal.PyString{Value: key}, Value: val}
	}
	return marshal.PyDict{Items: []marshal.DictEntry{
		e("blueprintID", intRep(blueprintID)),
		e("productTypeID", intRep(bt.ProductTypeID)),
		e("copy", intRep(int64(a.Copy))),
		e("materialLevel", intRep(a.MaterialLevel)),
		e("productivityLevel", intRep(a.ProductivityLevel)),
		e("licensedProductionRunsRemaining", intRep(int64(a.RunsRemaining))),
		e("wastageFactor", marshal.PyReal{Value: wastage}),
		e("manufacturingTime", intRep(bt.ProductionTime)),
		e("maxProductionCount", intRep(bt.MaxProductionLimit)),
		e("researchMaterialTime", intRep(bt.ResearchMaterialTime)),
		e("researchProductivityTime", intRep(bt.ResearchProductivityTime)),
		e("researchCopyTime", intRep(bt.ResearchCopyTime)),
		e("researchTechTime", intRep(0)),
	}}, nil
}

// GetBlueprintInformationRow answers the legacy factory.GetBlueprintInformation RPC;
// the wizard reads .copy and .licensedProductionRunsRemaining as attributes.
func GetBlueprintInformationRow(ctx context.Context, db *sql.DB, blueprintID int64) (marshal.PyRep, error) {
	a, err := GetBlueprintAttributes(ctx, db, blueprintID)
	if err != nil {
		a = &BlueprintAttrs{RunsRemaining: -1}
	}
	cols := []string{"blueprintID", "copy", "licensedProductionRunsRemaining",
		"materialLevel", "productivityLevel"}
	return makeRow(cols, []marshal.PyRep{
		intRep(blueprintID), intRep(int64(a.Copy)), intRep(int64(a.RunsRemaining)),
		intRep(a.MaterialLevel), intRep(a.ProductivityLevel),
	}), nil
}

// GetBlueprintInfoAtLocationWithFlag lists the blueprints a character owns at
// location+flag with their invBlueprints attributes (legacy factory RPC, also used
// by the corp vote UI). forCorp lists the corporation's prints in a hangar division
// of its office, only for a division the caller may query (ramcorp.go). Without an
// office there, a Director gets the corp's station-floor junk (flag 4), which
// corp_ui_votes PopulateAllLockedItems matches locked prints against.
// The rowset must carry singleton: the lockdown picker skips singleton 0.
func GetBlueprintInfoAtLocationWithFlag(ctx context.Context, db *sql.DB, charID, locationID, flag int64, forCorp bool) (marshal.PyRep, error) {
	cols := []string{"itemID", "typeID", "copy", "licensedProductionRunsRemaining",
		"productivityLevel", "materialLevel", "singleton"}
	owner := charID
	if forCorp {
		var corpID int64
		_ = db.QueryRowContext(ctx,
			`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&corpID)
		if corpID == 0 || !IsCorpHangarFlag(flag) {
			return makeRowset(cols, nil), nil
		}
		junk := false
		if !IsOfficeItemID(ctx, db, locationID) {
			officeID, _, _ := GetCorpOfficeAt(ctx, db, corpID, locationID)
			switch {
			case officeID != 0:
				locationID = officeID
			case flag == FlagCorpHangarDiv1:
				junk = true // the station floor itself
			default:
				return makeRowset(cols, nil), nil
			}
		}
		var ceoID int64
		_ = db.QueryRowContext(ctx,
			`SELECT COALESCE(ceoID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&ceoID)
		var roles uint64
		if info, err := GetCorporationMemberInfo(ctx, db, charID); err == nil && info != nil {
			roles = info.Corprole
		}
		if q := CorpHangarQueryRole(flag); ceoID != charID && roles&q == 0 && roles&corp.RoleDirector == 0 {
			return makeRowset(cols, nil), nil
		}
		if junk && ceoID != charID && roles&corp.RoleDirector == 0 {
			return makeRowset(cols, nil), nil
		}
		owner = corpID
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID,
		       COALESCE(b.copy, 0), COALESCE(b.licensedProductionRunsRemaining, -1),
		       COALESCE(b.productivityLevel, 0), COALESCE(b.materialLevel, 0),
		       e.singleton
		  FROM entity e
		  JOIN invBlueprintTypes bt ON bt.blueprintTypeID = e.typeID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		 WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ?`,
		owner, locationID, flag)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("GetBlueprintInfoAtLocationWithFlag: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var itemID, typeID, isCopy, runsRemaining, prodLevel, matLevel, singleton int64
		if err := rows.Scan(&itemID, &typeID, &isCopy, &runsRemaining, &prodLevel, &matLevel, &singleton); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{
			intRep(itemID), intRep(typeID), intRep(isCopy), intRep(runsRemaining),
			intRep(prodLevel), intRep(matLevel), intRep(singleton),
		})
	}
	return makeRowset(cols, result), nil
}

// BlueprintTypeInfo holds static data from invBlueprintTypes for formula calcs.
type BlueprintTypeInfo struct {
	ProductTypeID            int64
	ProductionTime           int64
	ProductivityModifier     int64
	ResearchMaterialTime     int64
	ResearchProductivityTime int64
	ResearchCopyTime         int64
	WasteFactor              int64
	MaxProductionLimit       int64
}

// GetBlueprintTypeInfo loads the static blueprint type data for a given blueprintTypeID.
func GetBlueprintTypeInfo(ctx context.Context, db *sql.DB, blueprintTypeID int64) (*BlueprintTypeInfo, error) {
	var b BlueprintTypeInfo
	err := db.QueryRowContext(ctx, `
		SELECT productTypeID,
		       COALESCE(productionTime, 0),
		       COALESCE(productivityModifier, 0),
		       COALESCE(researchMaterialTime, 0),
		       COALESCE(researchProductivityTime, 0),
		       COALESCE(researchCopyTime, 0),
		       COALESCE(wasteFactor, 10),
		       COALESCE(maxProductionLimit, 10)
		  FROM invBlueprintTypes
		 WHERE blueprintTypeID = ?`, blueprintTypeID).
		Scan(&b.ProductTypeID, &b.ProductionTime, &b.ProductivityModifier,
			&b.ResearchMaterialTime, &b.ResearchProductivityTime, &b.ResearchCopyTime,
			&b.WasteFactor, &b.MaxProductionLimit)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("blueprintTypeID %d not found in invBlueprintTypes", blueprintTypeID)
		}
		return nil, fmt.Errorf("GetBlueprintTypeInfo: %w", err)
	}
	return &b, nil
}

// assemblyLineCols are the columns the client expects from ram.AssemblyLinesSelect
// and ram.AssemblyLinesGet.
var assemblyLineCols = []string{
	"assemblyLineID", "assemblyLineTypeID", "containerID",
	"nextFreeTime", "UIGroupingID", "costInstall",
	"costPerHour", "restrictionMask", "discountPerGoodStandingPoint",
	"surchargePerBadStandingPoint", "minimumStanding", "minimumCharSecurity",
	"minimumCorpSecurity", "maximumCharSecurity", "maximumCorpSecurity",
	"ownerID", "activityID", "containerLocationID", "containerTypeID",
	"quantity",
}

// clientAssemblyActivities limits assembly-line lookups to the activities the client
// can name and install. ui/shared/manufacturing.py lists None, Manufacturing,
// Material/Time Research, Copying, Duplicating, Reverse Engineering and Technology,
// filtered by ramActivities.published, which leaves {1, 3, 4, 5}. Any other activity
// (Invention, 8) renders as "Unknown 8" and cannot be installed into.
const clientAssemblyActivities = `al.activityID IN (1, 3, 4, 5)`

// assemblyLineQuery is the shared SELECT used by both region and station lookups.
const assemblyLineQuery = `
		SELECT al.assemblyLineID,
		       al.assemblyLineTypeID,
		       al.containerID,
		       COALESCE((SELECT MAX(j.endProductionTime) FROM ramJobs j
		                  WHERE j.assemblyLineID = al.assemblyLineID
		                    AND j.completedStatusID = 0), 0) AS nextFreeTime,
		       COALESCE(al.UIGroupingID, 0) AS UIGroupingID,
		       COALESCE(al.costInstall, 0) AS costInstall,
		       COALESCE(al.costPerHour, 0) AS costPerHour,
		       COALESCE(al.restrictionMask, 0) AS restrictionMask,
		       COALESCE(al.discountPerGoodStandingPoint, 0),
		       COALESCE(al.surchargePerBadStandingPoint, 0),
		       COALESCE(al.minimumStanding, 0),
		       COALESCE(al.minimumCharSecurity, 0),
		       COALESCE(al.minimumCorpSecurity, 0),
		       COALESCE(al.maximumCharSecurity, 0),
		       COALESCE(al.maximumCorpSecurity, 0),
		       COALESCE(al.ownerID, 0),
		       COALESCE(al.activityID, 1) AS activityID,
		       COALESCE(s.solarSystemID, ce.locationID, 0) AS containerLocationID,
		       COALESCE(s.stationTypeID, ce.typeID, 0) AS containerTypeID,
		       COALESCE(als.quantity,
		                (SELECT COUNT(*) FROM ramassemblylines x
		                  WHERE x.containerID = al.containerID
		                    AND x.assemblyLineTypeID = al.assemblyLineTypeID), 1) AS quantity
		  FROM ramassemblylines al
		  LEFT JOIN staStations s ON s.stationID = al.containerID
		  LEFT JOIN entity ce ON ce.itemID = al.containerID AND s.stationID IS NULL
		  LEFT JOIN ramassemblylinestations als ON als.stationID = al.containerID AND als.assemblyLineTypeID = al.assemblyLineTypeID`

// scanAssemblyLineRows scans rows from the shared assembly line query into PyRep slices.
func scanAssemblyLineRows(rows interface {
	Next() bool
	Scan(dest ...interface{}) error
}) [][]marshal.PyRep {
	var result [][]marshal.PyRep
	for rows.Next() {
		var alID, alTypeID, containerID, nextFree int64
		var uiGroup int64
		var costInstall, costPerHour float64
		var restrictionMask int64
		var discGood, surchBad, minStanding, minCharSec, minCorpSec, maxCharSec, maxCorpSec float64
		var ownerID, activityID, containerLocationID, containerTypeID, quantity int64
		if err := rows.Scan(&alID, &alTypeID, &containerID, &nextFree, &uiGroup,
			&costInstall, &costPerHour, &restrictionMask,
			&discGood, &surchBad, &minStanding, &minCharSec, &minCorpSec,
			&maxCharSec, &maxCorpSec, &ownerID, &activityID,
			&containerLocationID, &containerTypeID, &quantity); err != nil {
			continue
		}
		// An idle line reports "free now", not 0: the client sets InstallJob's
		// maxJobStartTime to nextFreeTime + 1 minute, so 0 fails the start-time check.
		if now := int64(win32TimeNow()); nextFree < now {
			nextFree = now
		}
		result = append(result, []marshal.PyRep{
			intRep(alID), intRep(alTypeID), intRep(containerID),
			intRep(nextFree), intRep(uiGroup),
			floatRep(costInstall), floatRep(costPerHour),
			intRep(restrictionMask),
			floatRep(discGood), floatRep(surchBad),
			floatRep(minStanding), floatRep(minCharSec), floatRep(minCorpSec),
			floatRep(maxCharSec), floatRep(maxCorpSec),
			intRep(ownerID), intRep(activityID),
			intRep(containerLocationID), intRep(containerTypeID),
			intRep(quantity),
		})
	}
	return result
}

// GetAssemblyLinesSelectRegion returns the lines of all NPC stations in a region.
// It reads ramassemblylinestations (one row per station and line type) rather than
// every ramassemblylines row, to keep the result small.
func GetAssemblyLinesSelectRegion(ctx context.Context, db *sql.DB, regionID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT al.assemblyLineID,
		       al.assemblyLineTypeID,
		       al.containerID,
		       0 AS nextFreeTime,
		       COALESCE(al.UIGroupingID, 0) AS UIGroupingID,
		       COALESCE(al.costInstall, 0) AS costInstall,
		       COALESCE(al.costPerHour, 0) AS costPerHour,
		       COALESCE(al.restrictionMask, 0) AS restrictionMask,
		       COALESCE(al.discountPerGoodStandingPoint, 0),
		       COALESCE(al.surchargePerBadStandingPoint, 0),
		       COALESCE(al.minimumStanding, 0),
		       COALESCE(al.minimumCharSecurity, 0),
		       COALESCE(al.minimumCorpSecurity, 0),
		       COALESCE(al.maximumCharSecurity, 0),
		       COALESCE(al.maximumCorpSecurity, 0),
		       COALESCE(al.ownerID, 0),
		       COALESCE(al.activityID, 1) AS activityID,
		       COALESCE(s.solarSystemID, 0) AS containerLocationID,
		       COALESCE(s.stationTypeID, 0) AS containerTypeID,
		       COALESCE(als.quantity, 1) AS quantity
		  FROM ramassemblylinestations als
		  JOIN ramassemblylines al
		       ON al.containerID = als.stationID
		       AND al.assemblyLineTypeID = als.assemblyLineTypeID
		  LEFT JOIN staStations s ON s.stationID = als.stationID
		 WHERE s.regionID = ? AND `+clientAssemblyActivities+`
		 GROUP BY als.stationID, als.assemblyLineTypeID`, regionID)
	if err != nil {
		return makeRowset(assemblyLineCols, nil), fmt.Errorf("GetAssemblyLinesSelectRegion: %w", err)
	}
	defer rows.Close()
	return makeRowset(assemblyLineCols, scanAssemblyLineRows(rows)), nil
}

// GetAssemblyLinesSelectChar returns assembly lines at a specific station.
func GetAssemblyLinesSelectChar(ctx context.Context, db *sql.DB, stationID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, assemblyLineQuery+`
		 WHERE al.containerID = ? AND `+clientAssemblyActivities, stationID)
	if err != nil {
		return makeRowset(assemblyLineCols, nil), fmt.Errorf("GetAssemblyLinesSelectChar: %w", err)
	}
	defer rows.Close()
	return makeRowset(assemblyLineCols, scanAssemblyLineRows(rows)), nil
}

// GetRegionForStation returns the regionID for a given stationID.
func GetRegionForStation(ctx context.Context, db *sql.DB, stationID int64) (int64, error) {
	var regionID int64
	err := db.QueryRowContext(ctx,
		`SELECT regionID FROM staStations WHERE stationID = ?`, stationID).Scan(&regionID)
	if err != nil {
		return 0, fmt.Errorf("GetRegionForStation: %w", err)
	}
	return regionID, nil
}

// GetAssemblyLinesForStation returns assembly lines available at a station.
func GetAssemblyLinesForStation(ctx context.Context, db *sql.DB, stationID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, assemblyLineQuery+`
		 WHERE al.containerID = ? AND `+clientAssemblyActivities, stationID)
	if err != nil {
		return makeRowset(assemblyLineCols, nil), fmt.Errorf("GetAssemblyLinesForStation: %w", err)
	}
	defer rows.Close()
	return makeRowset(assemblyLineCols, scanAssemblyLineRows(rows)), nil
}

// ramJobCols are the columns the client reads from industryMgr.GetJobsByOwner and
// ram.GetJobs2 for the 3913 Jobs tab.
var ramJobCols = []string{
	"jobID", "assemblyLineID", "containerID", "installedItemID",
	"installedItemTypeID", "installedItemOwnerID",
	"installedItemMaterialLevel", "installedItemProductivityLevel",
	"installerID", "installTime", "beginProductionTime", "endProductionTime",
	"runs", "activityID", "outputTypeID", "outputFlag",
	"completed", "completedStatus", "licensedProductionRuns",
	"installedInSolarSystemID", "containerTypeID", "containerLocationID",
	"materialLevel", "productivityLevel",
	"charMaterialLevel", "charProductivityLevel",
}

// GetJobs lists an owner's running and ready jobs, or delivered and aborted ones
// when completed is true. fromTime/toTime bound installTime (Win32 FILETIME, 0 =
// unbounded). forCorp lists the corporation's jobs (ramJobs.ownerID) instead of the
// installer's, for the S&I window's "My Corporation" owner.
func GetJobs(ctx context.Context, db *sql.DB, ownerID int64, completed bool, fromTime, toTime int64, forCorp ...bool) (marshal.PyRep, error) {
	ownerCol := "j.installerID"
	if len(forCorp) > 0 && forCorp[0] {
		ownerCol = "j.ownerID"
	}
	statusFilter := "j.completedStatusID < 100"
	if completed {
		statusFilter = "j.completedStatusID >= 100"
	}
	dateFilter := ""
	args := []interface{}{ownerID}
	if fromTime > 0 {
		dateFilter += " AND j.installTime >= ?"
		args = append(args, fromTime)
	}
	if toTime > 0 {
		dateFilter += " AND j.installTime <= ?"
		args = append(args, toTime)
	}
	query := fmt.Sprintf(`
		SELECT j.jobID, j.assemblyLineID, j.containerID, j.installedItemID,
		       COALESCE(e.typeID, 0), COALESCE(e.ownerID, 0),
		       j.installerID, j.installTime, j.beginProductionTime, j.endProductionTime,
		       j.runs, j.activityID, j.outputTypeID, j.outputFlag,
		       j.completedStatusID, j.licensedProductionRuns,
		       j.installedInSolarSystemID,
		       COALESCE(s.stationTypeID, ce.typeID, 0),
		       COALESCE(s.solarSystemID, NULLIF(j.installedInSolarSystemID, 0), 0),
		       COALESCE(b.materialLevel, 0), COALESCE(b.productivityLevel, 0)
		  FROM ramJobs j
		  LEFT JOIN invBlueprints b ON b.blueprintID = j.installedItemID
		  LEFT JOIN entity e ON e.itemID = j.installedItemID
		  LEFT JOIN staStations s ON s.stationID = j.containerID
		  LEFT JOIN entity ce ON ce.itemID = j.containerID AND s.stationID IS NULL
		 WHERE %s = ? AND %s%s
		 ORDER BY j.installTime DESC`, ownerCol, statusFilter, dateFilter)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return makeRowset(ramJobCols, nil), fmt.Errorf("GetJobs: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var jobID, alID, containerID, installedItemID, itemTypeID, itemOwnerID, installerID int64
		var installTime, beginTime, endTime int64
		var runs, activityID, outputTypeID, outputFlag, statusID, licensedRuns int64
		var solarSystemID, containerTypeID, containerLocationID, matLevel, prodLevel int64
		if err := rows.Scan(&jobID, &alID, &containerID, &installedItemID,
			&itemTypeID, &itemOwnerID, &installerID,
			&installTime, &beginTime, &endTime, &runs, &activityID,
			&outputTypeID, &outputFlag, &statusID, &licensedRuns,
			&solarSystemID, &containerTypeID, &containerLocationID,
			&matLevel, &prodLevel); err != nil {
			continue
		}
		// Status 0 and 1 (running, ready) are not completed. 101 is delivered (completedStatus
		// 1) and 102 aborted (2); the client reads completedStatus only when completed == 1.
		var completedFlag, completedStatus int64
		if statusID >= 100 {
			completedFlag = 1
			switch statusID {
			case 102:
				completedStatus = 2 // Aborted
			default:
				completedStatus = 1 // Delivered
			}
		}
		result = append(result, []marshal.PyRep{
			intRep(jobID), intRep(alID), intRep(containerID), intRep(installedItemID),
			intRep(itemTypeID), intRep(itemOwnerID),
			intRep(matLevel), intRep(prodLevel),
			intRep(installerID), intRep(installTime), intRep(beginTime), intRep(endTime),
			intRep(runs), intRep(activityID), intRep(outputTypeID), intRep(outputFlag),
			intRep(completedFlag), intRep(completedStatus), intRep(licensedRuns),
			intRep(solarSystemID), intRep(containerTypeID), intRep(containerLocationID),
			intRep(matLevel), intRep(prodLevel),
			intRep(0), intRep(0),
		})
	}
	return makeRowset(ramJobCols, result), nil
}

// InsertJob creates a new ramJobs row and returns the new jobID.
func InsertJob(ctx context.Context, db DBTX, assemblyLineID, containerID, installerID, installedItemID int64,
	installTime, beginTime, endTime int64, runs, activityID int, outputTypeID int64, licensedRuns int, solarSystemID int64) (int64, error) {
	return InsertJobEx(ctx, db, assemblyLineID, containerID, installerID, installedItemID,
		installTime, beginTime, endTime, runs, activityID, outputTypeID, licensedRuns, solarSystemID, 4, 0, 4)
}

// InsertJobEx is InsertJob with the POS fields: outputFlag (where the output lands),
// ownerID (0 = the installer) and bpFlag (where the blueprint returns).
func InsertJobEx(ctx context.Context, db DBTX, assemblyLineID, containerID, installerID, installedItemID int64,
	installTime, beginTime, endTime int64, runs, activityID int, outputTypeID int64, licensedRuns int, solarSystemID int64,
	outputFlag, ownerID, bpFlag int64) (int64, error) {

	res, err := db.ExecContext(ctx, `
		INSERT INTO ramJobs
		  (assemblyLineID, containerID, installerID, installedItemID,
		   installTime, beginProductionTime, endProductionTime,
		   runs, activityID, outputTypeID, outputFlag,
		   completedStatusID, licensedProductionRuns, installedInSolarSystemID,
		   ownerID, bpFlag)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		assemblyLineID, containerID, installerID, installedItemID,
		installTime, beginTime, endTime,
		runs, activityID, outputTypeID, outputFlag, licensedRuns, solarSystemID, ownerID, bpFlag)
	if err != nil {
		return 0, fmt.Errorf("InsertJob: %w", err)
	}
	return res.LastInsertId()
}

// MoveBlueprintToFlag sets an entity's flag (111 in production, 4 hangar).
func MoveBlueprintToFlag(ctx context.Context, db DBTX, itemID int64, flag int) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET flag = ? WHERE itemID = ?`, flag, itemID)
	if err != nil {
		return fmt.Errorf("MoveBlueprintToFlag: %w", err)
	}
	return nil
}

// ConsumedStack records one hangar stack that ConsumeMaterials changed, so the
// caller can push OnItemChange. NewQty 0 means the stack was deleted.
type ConsumedStack struct {
	ItemID int64
	TypeID int64
	OldQty int64
	NewQty int64
}

// ErrMissingMaterial is returned by ConsumeMaterials when the hangar lacks a required
// material or extra. TypeName lets the caller send the client's own RamNeedMoreForJob
// message (%(item)s).
type ErrMissingMaterial struct {
	TypeID, Needed int64
	TypeName       string
}

func (e *ErrMissingMaterial) Error() string {
	return fmt.Sprintf("insufficient materials: need %d more %s", e.Needed, e.TypeName)
}

func resolveTypeName(ctx context.Context, db DBTX, typeID int64) string {
	var typeName string
	_ = db.QueryRowContext(ctx, `SELECT typeName FROM invTypes WHERE typeID = ?`, typeID).Scan(&typeName)
	if typeName == "" {
		typeName = fmt.Sprintf("typeID %d", typeID)
	}
	return typeName
}

// ConsumeMaterials removes a manufacturing job's materials from a character's hangar
// (flag 4). Per-run quantity is CalcMaterialsNeeded, then the line's material
// multiplier. Pass a *sql.Tx so the consumption is atomic with the install.
func ConsumeMaterials(ctx context.Context, db DBTX, charID, stationID, blueprintTypeID, meLevel int64, peSkillLevel int, runs int, materialMultiplier float64) ([]ConsumedStack, error) {
	return ConsumeMaterialsFrom(ctx, db, charID, stationID, 4, blueprintTypeID, meLevel, peSkillLevel, runs, materialMultiplier)
}

// ConsumeMaterialsFrom is ConsumeMaterials against any (owner, location, flag):
// a corporation division in a POS array or a pilot's hangar.
func ConsumeMaterialsFrom(ctx context.Context, db DBTX, ownerID, locationID, flag, blueprintTypeID, meLevel int64, peSkillLevel int, runs int, materialMultiplier float64) ([]ConsumedStack, error) {
	charID, stationID := ownerID, locationID
	// Base materials for manufacturing (activity 1) come from the tl2 material table
	// keyed by blueprint typeID. Rows with damagePerJob < 1 are extras: required, not consumed.
	matRows, err := db.QueryContext(ctx, `
		SELECT m.requiredTypeID, m.quantity
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.typeID = ? AND m.activity = 1 AND m.recycle = 0
		   AND m.damagePerJob >= 1 AND COALESCE(g.categoryID, 0) <> 16`, blueprintTypeID)
	if err != nil {
		return nil, fmt.Errorf("ConsumeMaterials query materials: %w", err)
	}
	defer matRows.Close()

	type matReq struct {
		typeID   int64
		quantity int64
	}
	var mats []matReq
	var consumed []ConsumedStack
	for matRows.Next() {
		var m matReq
		if err := matRows.Scan(&m.typeID, &m.quantity); err != nil {
			return nil, fmt.Errorf("ConsumeMaterials scan: %w", err)
		}
		mats = append(mats, m)
	}

	// wasteFactor comes from the blueprint type.
	var wasteFactor int64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(wasteFactor, 10) FROM invBlueprintTypes WHERE blueprintTypeID = ?`,
		blueprintTypeID).Scan(&wasteFactor)

	for _, m := range mats {
		perRun := ApplyMaterialMultiplier(
			CalcMaterialsNeeded(m.quantity, wasteFactor, meLevel, peSkillLevel), materialMultiplier)
		needed := perRun * int64(runs)
		if needed <= 0 {
			continue
		}

		// Buffer the stacks before mutating: db may be a transaction, and MySQL cannot run
		// statements on a connection with an open result cursor.
		itemRows, err := db.QueryContext(ctx, `
			SELECT itemID, quantity FROM entity
			 WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = ?
			 ORDER BY quantity ASC`, m.typeID, charID, stationID, flag)
		if err != nil {
			return nil, fmt.Errorf("ConsumeMaterials find items typeID=%d: %w", m.typeID, err)
		}
		type stack struct{ itemID, qty int64 }
		var stacks []stack
		for itemRows.Next() {
			var s stack
			if err := itemRows.Scan(&s.itemID, &s.qty); err != nil {
				itemRows.Close()
				return nil, fmt.Errorf("ConsumeMaterials scan item: %w", err)
			}
			stacks = append(stacks, s)
		}
		itemRows.Close()
		if err := itemRows.Err(); err != nil {
			return nil, fmt.Errorf("ConsumeMaterials iterate items typeID=%d: %w", m.typeID, err)
		}

		var remaining = needed
		for _, s := range stacks {
			if s.qty <= remaining {
				// Delete the whole stack.
				if _, err := db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.itemID); err != nil {
					return nil, fmt.Errorf("ConsumeMaterials delete stack %d: %w", s.itemID, err)
				}
				consumed = append(consumed, ConsumedStack{ItemID: s.itemID, TypeID: m.typeID, OldQty: s.qty, NewQty: 0})
				remaining -= s.qty
			} else {
				// Decrement the stack.
				if _, err := db.ExecContext(ctx, `UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, remaining, s.itemID); err != nil {
					return nil, fmt.Errorf("ConsumeMaterials decrement stack %d: %w", s.itemID, err)
				}
				consumed = append(consumed, ConsumedStack{ItemID: s.itemID, TypeID: m.typeID, OldQty: s.qty, NewQty: s.qty - remaining})
				remaining = 0
			}
			if remaining <= 0 {
				break
			}
		}

		if remaining > 0 {
			return nil, &ErrMissingMaterial{
				TypeID: m.typeID, Needed: remaining, TypeName: resolveTypeName(ctx, db, m.typeID),
			}
		}
	}

	// Extras (damagePerJob < 1) must be present but are not consumed. Skill requirements
	// (category 16) are excluded and validated elsewhere.
	extraRows, err := db.QueryContext(ctx, `
		SELECT m.requiredTypeID, m.quantity
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.typeID = ? AND m.activity = 1 AND m.recycle = 0
		   AND m.damagePerJob < 1 AND COALESCE(g.categoryID, 0) <> 16`, blueprintTypeID)
	if err != nil {
		return nil, fmt.Errorf("ConsumeMaterials query extras: %w", err)
	}
	// Buffer before querying quantities (open-cursor restriction, as above).
	var extras []matReq
	for extraRows.Next() {
		var m matReq
		if err := extraRows.Scan(&m.typeID, &m.quantity); err != nil {
			extraRows.Close()
			return nil, fmt.Errorf("ConsumeMaterials scan extra: %w", err)
		}
		extras = append(extras, m)
	}
	extraRows.Close()
	if err := extraRows.Err(); err != nil {
		return nil, fmt.Errorf("ConsumeMaterials iterate extras: %w", err)
	}
	for _, m := range extras {
		var have sql.NullInt64
		_ = db.QueryRowContext(ctx, `
			SELECT SUM(quantity) FROM entity
			 WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = ?`,
			m.typeID, charID, stationID, flag).Scan(&have)
		if !have.Valid || have.Int64 < m.quantity {
			return nil, &ErrMissingMaterial{
				TypeID: m.typeID, Needed: m.quantity - have.Int64, TypeName: resolveTypeName(ctx, db, m.typeID),
			}
		}
	}
	return consumed, nil
}

// CalcMaterialsNeeded returns the per-run quantity of one material:
// base + round(base x waste/100 / (1 + ME)) + max(0, round(base x (0.25 - 0.05 x PE skill))),
// at least 1. A negative ME multiplies the waste by (1 - ME), and ME has no upper cap.
// See docs/db-10-notes.md#blueprint-research-has-no-level-cap.
func CalcMaterialsNeeded(baseMaterial, wasteFactor, meLevel int64, peSkillLevel int) int64 {
	base := float64(baseMaterial)
	me := float64(meLevel)
	wf := float64(wasteFactor)

	var meWaste float64
	if meLevel >= 0 {
		meWaste = roundHalfUp(base * (wf / 100.0) / (1.0 + me))
	} else {
		meWaste = roundHalfUp(base * (wf / 100.0) * (1.0 - me))
	}

	peWaste := roundHalfUp(base * (0.25 - 0.05*float64(peSkillLevel)))
	if peWaste < 0 {
		peWaste = 0
	}

	total := baseMaterial + int64(meWaste) + int64(peWaste)
	if total < 1 {
		total = 1
	}
	return total
}

func roundHalfUp(f float64) float64 {
	if f < 0 {
		return float64(int64(f - 0.5))
	}
	return float64(int64(f + 0.5))
}

// JobOutput describes one item a delivered job produced. OldQty > 0 means the output
// merged into an existing hangar stack that held OldQty.
type JobOutput struct {
	ItemID int64
	OldQty int64
}

// CompleteJob refusals; callers map each with errors.Is to the client's RamCompletion*
// message. The rolebots module matches the texts "still running" and "already delivered".
var (
	ErrJobNotFound     = errors.New("no such job")
	ErrJobNotYours     = errors.New("not your job")
	ErrJobStillRunning = errors.New("job in production")
	ErrJobDelivered    = errors.New("job already completed")
)

// CompleteJob delivers a job (completedStatusID 101) or, with cancel, aborts it (102)
// and returns the blueprint. Delivery depends on the activity: 1 manufacturing, 3 PE
// research, 4 ME research, 5 copying. Returns the output items.
func CompleteJob(ctx context.Context, db *sql.DB, jobID, charID int64, cancel bool) ([]JobOutput, error) {
	var alID, containerID, installerID, installedItemID int64
	var runs, activityID, outputTypeID, completedStatus, licensedRuns, endTime int64
	var jobOwnerID, outputFlag, bpFlag int64
	err := db.QueryRowContext(ctx, `
		SELECT assemblyLineID, containerID, installerID, installedItemID,
		       runs, activityID, outputTypeID, completedStatusID, licensedProductionRuns,
		       COALESCE(endProductionTime, 0), ownerID, outputFlag, bpFlag
		  FROM ramJobs WHERE jobID = ?`, jobID).
		Scan(&alID, &containerID, &installerID, &installedItemID,
			&runs, &activityID, &outputTypeID, &completedStatus, &licensedRuns, &endTime,
			&jobOwnerID, &outputFlag, &bpFlag)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("job %d not found: %w", jobID, ErrJobNotFound)
		}
		return nil, fmt.Errorf("CompleteJob load: %w", err)
	}

	// A corporation job (POS) may be delivered by any member of that
	// corporation; anything else only by its installer.
	if installerID != charID {
		var charCorp int64
		_ = db.QueryRowContext(ctx,
			`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&charCorp)
		if jobOwnerID == 0 || jobOwnerID == installerID || charCorp != jobOwnerID {
			return nil, fmt.Errorf("job %d does not belong to character %d: %w", jobID, charID, ErrJobNotYours)
		}
	}
	// Output and blueprint return to the job's owner at the job's flags; station jobs
	// carry owner 0 and flags 4, which is the installer's hangar.
	outOwner := jobOwnerID
	if outOwner == 0 {
		outOwner = installerID
	}
	if outputFlag == 0 {
		outputFlag = 4
	}
	if bpFlag == 0 {
		bpFlag = 4
	}
	// Output lands in the container, or in the office for a station corporation job
	// (ramJobs.outputLocationID; read on its own so a schema without it still delivers).
	outLoc := containerID
	var jobOutLoc int64
	if e := db.QueryRowContext(ctx,
		`SELECT outputLocationID FROM ramJobs WHERE jobID = ?`, jobID).Scan(&jobOutLoc); e == nil && jobOutLoc != 0 {
		outLoc = jobOutLoc
	}
	// A running job (status 0) delivers once endProductionTime has passed, even if
	// TickJobs has not flipped it yet; the client derives readiness from the clock.
	if completedStatus == 0 && !cancel {
		if endTime <= 0 || uint64(endTime) > win32TimeNow() {
			return nil, fmt.Errorf("job %d is still running: %w", jobID, ErrJobStillRunning)
		}
	}
	if completedStatus >= 100 {
		return nil, fmt.Errorf("job %d already delivered: %w", jobID, ErrJobDelivered)
	}

	// One transaction: blueprint return, output, BPC bookkeeping and the delivered flag
	// succeed or fail together, so no job stays deliverable after its blueprint is back.
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("CompleteJob begin tx: %w", err)
	}
	defer tx.Rollback()

	// Return the blueprint to its bpFlag, whatever the activity.
	if err := MoveBlueprintToFlag(ctx, tx, installedItemID, int(bpFlag)); err != nil {
		return nil, err
	}

	if cancel {
		// Cancelled: return the blueprint only, no output.
		if _, err := tx.ExecContext(ctx,
			`UPDATE ramJobs SET completedStatusID = 102 WHERE jobID = ?`, jobID); err != nil {
			return nil, fmt.Errorf("CompleteJob cancel: %w", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("CompleteJob commit: %w", err)
		}
		return nil, nil
	}

	var outputs []JobOutput
	var deliveredUnits int64 // manufacturing output, for the world event

	switch activityID {
	case 1: // Manufacturing
		// Each run yields portionSize units (e.g. ammo blueprints make 100/run).
		var portionSize int64 = 1
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(portionSize, 1) FROM invTypes WHERE typeID = ?`,
			outputTypeID).Scan(&portionSize)
		if portionSize < 1 {
			portionSize = 1
		}
		totalQty := runs * portionSize
		deliveredUnits = totalQty

		// Merge into an existing packaged stack when one is present so
		// repeated deliveries don't litter the hangar with one stack each.
		existingID, existingQty, ferr := FindStackAt(ctx, tx, outputTypeID, outOwner, outLoc, outputFlag)
		if ferr == nil && existingID != 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = ? WHERE itemID = ?`,
				existingQty+totalQty, existingID); err != nil {
				return nil, fmt.Errorf("CompleteJob merge product stack: %w", err)
			}
			outputs = append(outputs, JobOutput{ItemID: existingID, OldQty: existingQty})
		} else {
			itemID, err := CreateItem(ctx, tx, outputTypeID, outOwner, outLoc, int(totalQty))
			if err != nil {
				return nil, fmt.Errorf("CompleteJob create product: %w", err)
			}
			if outputFlag != 4 {
				if _, err := tx.ExecContext(ctx, `UPDATE entity SET flag = ? WHERE itemID = ?`, outputFlag, itemID); err != nil {
					return nil, fmt.Errorf("CompleteJob product flag: %w", err)
				}
			}
			// CreateItem delivers ships as one assembled unit; factory output is always a
			// packaged stack, so set the full quantity and singleton 0.
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = ?, singleton = 0 WHERE itemID = ?`,
				totalQty, itemID); err != nil {
				return nil, fmt.Errorf("CompleteJob set product quantity: %w", err)
			}
			outputs = append(outputs, JobOutput{ItemID: itemID})
		}

		// A BPC loses the delivered runs.
		var isCopy int
		var runsRemaining int
		_ = tx.QueryRowContext(ctx,
			`SELECT copy, licensedProductionRunsRemaining FROM invBlueprints WHERE blueprintID = ?`,
			installedItemID).Scan(&isCopy, &runsRemaining)
		if isCopy == 1 {
			newRuns := runsRemaining - int(runs)
			if newRuns <= 0 {
				// BPC exhausted: delete it.
				if _, err := tx.ExecContext(ctx, `DELETE FROM invBlueprints WHERE blueprintID = ?`, installedItemID); err != nil {
					return nil, fmt.Errorf("CompleteJob delete exhausted BPC: %w", err)
				}
				if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, installedItemID); err != nil {
					return nil, fmt.Errorf("CompleteJob delete exhausted BPC entity: %w", err)
				}
			} else {
				if _, err := tx.ExecContext(ctx,
					`UPDATE invBlueprints SET licensedProductionRunsRemaining = ? WHERE blueprintID = ?`,
					newRuns, installedItemID); err != nil {
					return nil, fmt.Errorf("CompleteJob decrement BPC runs: %w", err)
				}
			}
		}

	case 4: // ME Research
		if _, err := tx.ExecContext(ctx,
			`UPDATE invBlueprints SET materialLevel = materialLevel + ? WHERE blueprintID = ?`,
			runs, installedItemID); err != nil {
			return nil, fmt.Errorf("CompleteJob ME research: %w", err)
		}

	case 3: // PE Research (Time Efficiency)
		if _, err := tx.ExecContext(ctx,
			`UPDATE invBlueprints SET productivityLevel = productivityLevel + ? WHERE blueprintID = ?`,
			runs, installedItemID); err != nil {
			return nil, fmt.Errorf("CompleteJob PE research: %w", err)
		}

	case 5: // Copying
		// Read the original's type, ME and PE.
		var bpTypeID int64
		_ = tx.QueryRowContext(ctx,
			`SELECT typeID FROM entity WHERE itemID = ?`, installedItemID).Scan(&bpTypeID)

		attrs, err := GetBlueprintAttributes(ctx, tx, installedItemID)
		if err != nil {
			return nil, fmt.Errorf("CompleteJob copy read attrs: %w", err)
		}

		// maxProductionLimit caps licensed runs per copy.
		var maxRuns int64 = 10
		_ = tx.QueryRowContext(ctx,
			`SELECT COALESCE(maxProductionLimit, 10) FROM invBlueprintTypes WHERE blueprintTypeID = ?`,
			bpTypeID).Scan(&maxRuns)

		// Licensed runs per copy: the job's stored value when within the limit, otherwise
		// maxProductionLimit.
		runsPerCopy := maxRuns
		if licensedRuns > 0 && licensedRuns <= maxRuns {
			runsPerCopy = licensedRuns
		}

		// One BPC per requested copy.
		for i := int64(0); i < runs; i++ {
			copyItemID, err := CreateItem(ctx, tx, bpTypeID, outOwner, outLoc, 1)
			if err != nil {
				return nil, fmt.Errorf("CompleteJob create BPC entity (copy %d/%d): %w", i+1, runs, err)
			}

			// Blueprints are always singletons.
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET singleton = 1, quantity = 1, flag = ? WHERE itemID = ?`, outputFlag, copyItemID); err != nil {
				return nil, fmt.Errorf("CompleteJob mark BPC singleton: %w", err)
			}

			// CreateItem's EnsureBlueprintRow may have inserted a default row; REPLACE
			// overwrites it with the original's ME/PE.
			if _, err := tx.ExecContext(ctx,
				`REPLACE INTO invBlueprints (blueprintID, materialLevel, productivityLevel, copy, licensedProductionRunsRemaining)
				 VALUES (?, ?, ?, 1, ?)`,
				copyItemID, attrs.MaterialLevel, attrs.ProductivityLevel, runsPerCopy); err != nil {
				return nil, fmt.Errorf("CompleteJob insert BPC row: %w", err)
			}

			outputs = append(outputs, JobOutput{ItemID: copyItemID})
		}
	}

	// Mark the job delivered.
	if _, err := tx.ExecContext(ctx,
		`UPDATE ramJobs SET completedStatusID = 101 WHERE jobID = ?`, jobID); err != nil {
		return nil, fmt.Errorf("CompleteJob mark delivered: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("CompleteJob commit: %w", err)
	}
	if deliveredUnits > 0 && WorldEventsWanted() {
		emitIndustryDelivered(ctx, db, installerID, jobOwnerID, int(activityID), outputTypeID, deliveredUnits, containerID)
	}
	return outputs, nil
}

// emitIndustryDelivered tells modules (worldevents.go) a job's output was
// delivered. The container is a station or a POS array; an array's system is
// its entity row's location.
func emitIndustryDelivered(ctx context.Context, db *sql.DB, installerID, ownerID int64, activityID int, productTypeID, units, containerID int64) {
	ev := IndustryDeliveredEvent{At: time.Now(), CharID: installerID, ActivityID: activityID,
		ProductTypeID: productTypeID, Units: units, StationID: containerID}
	if ownerID != 0 && ownerID != installerID {
		ev.CorpID = ownerID
	}
	if p, err := GetStationParents(ctx, db, containerID); err == nil {
		ev.SolarSystemID = p.SolarSystemID
	} else {
		_ = db.QueryRowContext(ctx, `SELECT locationID FROM entity WHERE itemID = ?`, containerID).Scan(&ev.SolarSystemID)
	}
	EmitWorldEvent(ev)
}

// TickJobs moves running jobs (0) to ready (1) once endProductionTime has passed and
// returns how many changed. See docs/esp-industry.md#science--industry-page.
func TickJobs(ctx context.Context, db *sql.DB, nowWin32 uint64) (int64, error) {
	res, err := db.ExecContext(ctx,
		`UPDATE ramJobs SET completedStatusID = 1 WHERE completedStatusID = 0 AND endProductionTime <= ? AND endProductionTime > 0`,
		nowWin32)
	if err != nil {
		return 0, fmt.Errorf("TickJobs: %w", err)
	}
	return res.RowsAffected()
}

// MaterialReq represents a single material requirement from ramTypeRequirements.
type MaterialReq struct {
	TypeID       int64
	Quantity     int64
	DamagePerJob float64
}

// GetRequiredMaterials returns the raw material requirements for a blueprint
// type and activity (e.g. activityID=1 for manufacturing).
func GetRequiredMaterials(ctx context.Context, db *sql.DB, blueprintTypeID int64, activityID int) ([]MaterialReq, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.requiredTypeID, m.quantity, COALESCE(m.damagePerJob, 1.0)
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.typeID = ? AND m.activity = ? AND m.recycle = 0
		   AND COALESCE(g.categoryID, 0) <> 16`,
		blueprintTypeID, activityID)
	if err != nil {
		return nil, fmt.Errorf("GetRequiredMaterials: %w", err)
	}
	defer rows.Close()

	var mats []MaterialReq
	for rows.Next() {
		var m MaterialReq
		if err := rows.Scan(&m.TypeID, &m.Quantity, &m.DamagePerJob); err != nil {
			return nil, fmt.Errorf("GetRequiredMaterials scan: %w", err)
		}
		mats = append(mats, m)
	}
	return mats, nil
}

// GetActivitySkillReqs returns a blueprint type's skill requirements (skill typeID
// to level) for an activity, from the category-16 rows of the material table
// (quantity is the level, damagePerJob 0).
func GetActivitySkillReqs(ctx context.Context, db *sql.DB, blueprintTypeID int64, activityID int) ([]SkillReq, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.requiredTypeID, m.quantity
		  FROM tl2materialsfortypewithactivity m
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE m.typeID = ? AND m.activity = ? AND g.categoryID = 16`,
		blueprintTypeID, activityID)
	if err != nil {
		return nil, fmt.Errorf("GetActivitySkillReqs: %w", err)
	}
	defer rows.Close()

	var reqs []SkillReq
	for rows.Next() {
		var r SkillReq
		if err := rows.Scan(&r.SkillID, &r.Level); err != nil {
			return nil, fmt.Errorf("GetActivitySkillReqs scan: %w", err)
		}
		reqs = append(reqs, r)
	}
	return reqs, rows.Err()
}

// AssemblyLineInfo holds everything InstallJob needs to know about one line:
// costs, restrictions, the owning corp, when it next frees up, and the base
// time/material multipliers of its line type.
type AssemblyLineInfo struct {
	AssemblyLineID          int64
	AssemblyLineTypeID      int64
	ContainerID             int64
	ActivityID              int64
	OwnerID                 int64
	CostInstall             float64
	CostPerHour             float64
	RestrictionMask         int64
	DiscountPerGoodStanding float64
	SurchargePerBadStanding float64
	MinimumStanding         float64
	MinimumCharSecurity     float64
	MaximumCharSecurity     float64
	NextFreeTime            int64 // Win32 time the line's last queued job ends; 0 = free
	BaseTimeMultiplier      float64
	BaseMaterialMultiplier  float64
}

// GetAssemblyLineInfo loads one assembly line with its type's base multipliers
// and the computed next-free time (end of the latest still-running job).
func GetAssemblyLineInfo(ctx context.Context, db DBTX, assemblyLineID int64) (*AssemblyLineInfo, error) {
	var li AssemblyLineInfo
	err := db.QueryRowContext(ctx, `
		SELECT al.assemblyLineID, al.assemblyLineTypeID, al.containerID,
		       COALESCE(al.activityID, 1),
		       COALESCE(al.ownerID, 0),
		       COALESCE(al.costInstall, 0), COALESCE(al.costPerHour, 0),
		       COALESCE(al.restrictionMask, 0),
		       COALESCE(al.discountPerGoodStandingPoint, 0),
		       COALESCE(al.surchargePerBadStandingPoint, 0),
		       COALESCE(al.minimumStanding, 0),
		       COALESCE(al.minimumCharSecurity, 0),
		       COALESCE(al.maximumCharSecurity, 0),
		       COALESCE((SELECT MAX(j.endProductionTime) FROM ramJobs j
		                  WHERE j.assemblyLineID = al.assemblyLineID
		                    AND j.completedStatusID = 0), 0),
		       COALESCE(t.baseTimeMultiplier, 1),
		       COALESCE(t.baseMaterialMultiplier, 1)
		  FROM ramassemblylines al
		  LEFT JOIN ramaltypes t ON t.assemblyLineTypeID = al.assemblyLineTypeID
		 WHERE al.assemblyLineID = ?`, assemblyLineID).
		Scan(&li.AssemblyLineID, &li.AssemblyLineTypeID, &li.ContainerID,
			&li.ActivityID, &li.OwnerID,
			&li.CostInstall, &li.CostPerHour, &li.RestrictionMask,
			&li.DiscountPerGoodStanding, &li.SurchargePerBadStanding,
			&li.MinimumStanding, &li.MinimumCharSecurity, &li.MaximumCharSecurity,
			&li.NextFreeTime, &li.BaseTimeMultiplier, &li.BaseMaterialMultiplier)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("assembly line %d not found", assemblyLineID)
		}
		return nil, fmt.Errorf("GetAssemblyLineInfo: %w", err)
	}
	if li.BaseTimeMultiplier <= 0 {
		li.BaseTimeMultiplier = 1
	}
	if li.BaseMaterialMultiplier <= 0 {
		li.BaseMaterialMultiplier = 1
	}
	return &li, nil
}

// GetLineDetailMultipliers returns the time and material multipliers of an assembly
// line type for a product type: the category row times the group row. A missing row
// contributes 1.0.
func GetLineDetailMultipliers(ctx context.Context, db DBTX, assemblyLineTypeID, productTypeID int64) (timeMul, matMul float64) {
	timeMul, matMul = 1.0, 1.0

	var groupID, categoryID int64
	if err := db.QueryRowContext(ctx, `
		SELECT t.groupID, COALESCE(g.categoryID, 0)
		  FROM invTypes t LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID = ?`, productTypeID).Scan(&groupID, &categoryID); err != nil {
		return
	}

	var tm, mm float64
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(timeMultiplier, 1), COALESCE(materialMultiplier, 1)
		  FROM ramaltypecategory
		 WHERE assemblyLineTypeID = ? AND categoryID = ?`,
		assemblyLineTypeID, categoryID).Scan(&tm, &mm); err == nil {
		if tm > 0 {
			timeMul *= tm
		}
		if mm > 0 {
			matMul *= mm
		}
	}
	if err := db.QueryRowContext(ctx, `
		SELECT COALESCE(timeMultiplier, 1), COALESCE(materialMultiplier, 1)
		  FROM ramaltypegroup
		 WHERE assemblyLineTypeID = ? AND groupID = ?`,
		assemblyLineTypeID, groupID).Scan(&tm, &mm); err == nil {
		if tm > 0 {
			timeMul *= tm
		}
		if mm > 0 {
			matMul *= mm
		}
	}
	return
}

// CountActiveJobs returns how many undelivered jobs the installer has in an activity
// class (manufacturing = activityID 1, otherwise research and copying). Ready jobs
// still occupy a slot.
func CountActiveJobs(ctx context.Context, db DBTX, installerID int64, manufacturing bool) (int, error) {
	activityFilter := "activityID = 1"
	if !manufacturing {
		activityFilter = "activityID <> 1"
	}
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM ramJobs
		  WHERE installerID = ? AND completedStatusID < 100 AND `+activityFilter,
		installerID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountActiveJobs: %w", err)
	}
	return n, nil
}

// GetCharSecurityRating returns a character's security status.
func GetCharSecurityRating(ctx context.Context, db DBTX, charID int64) (float64, error) {
	var sec float64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(securityRating, 0) FROM character_ WHERE characterID = ?`, charID).Scan(&sec)
	if err != nil {
		return 0, fmt.Errorf("GetCharSecurityRating: %w", err)
	}
	return sec, nil
}

// TryChargeISK atomically deducts amount from a character's balance, failing
// (ok=false) without changing anything when the balance is insufficient.
// Returns the new balance on success.
func TryChargeISK(ctx context.Context, db DBTX, charID int64, amount float64) (ok bool, newBalance float64, err error) {
	if amount <= 0 {
		bal, berr := readBalance(ctx, db, charID)
		return true, bal, berr
	}
	res, err := db.ExecContext(ctx,
		`UPDATE character_ SET balance = balance - ? WHERE characterID = ? AND balance >= ?`,
		amount, charID, amount)
	if err != nil {
		return false, 0, fmt.Errorf("TryChargeISK: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return false, 0, nil
	}
	bal, err := readBalance(ctx, db, charID)
	return true, bal, err
}

func readBalance(ctx context.Context, db DBTX, charID int64) (float64, error) {
	var bal float64
	err := db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&bal)
	if err != nil {
		return 0, fmt.Errorf("readBalance: %w", err)
	}
	return bal, nil
}

// ApplyMaterialMultiplier scales a per-run material quantity by the assembly
// line's material multiplier, rounding half-up. Used identically by the quote
// and the consumption path so the two always agree.
func ApplyMaterialMultiplier(perRun int64, mul float64) int64 {
	if mul == 1.0 || perRun <= 0 {
		return perRun
	}
	out := int64(roundHalfUp(float64(perRun) * mul))
	if out < 1 {
		out = 1
	}
	return out
}

// GetAssemblyLineCosts returns the install cost and per-hour cost for an assembly line.
func GetAssemblyLineCosts(ctx context.Context, db *sql.DB, assemblyLineID int64) (installCost, costPerHour float64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(costInstall, 0), COALESCE(costPerHour, 0)
		  FROM ramassemblylines
		 WHERE assemblyLineID = ?`, assemblyLineID).Scan(&installCost, &costPerHour)
	if err != nil {
		return 0, 0, fmt.Errorf("GetAssemblyLineCosts: %w", err)
	}
	return
}

// GetCharMaterialQuantity returns the total quantity of a given typeID in a
// character's hangar (flag=4) at a station.
func GetCharMaterialQuantity(ctx context.Context, db *sql.DB, charID, stationID, typeID int64) (int64, error) {
	return GetMaterialQuantityAt(ctx, db, charID, stationID, 4, typeID)
}

// GetMaterialQuantityAt sums a type held by owner at (location, flag).
func GetMaterialQuantityAt(ctx context.Context, db *sql.DB, charID, stationID, flag, typeID int64) (int64, error) {
	var total sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT SUM(quantity) FROM entity
		 WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = ?`,
		typeID, charID, stationID, flag).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("GetCharMaterialQuantity: %w", err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Int64, nil
}

// GetSolarSystemForStation returns the solarSystemID for a given stationID.
func GetSolarSystemForStation(ctx context.Context, db *sql.DB, stationID int64) (int64, error) {
	var ssID int64
	err := db.QueryRowContext(ctx,
		`SELECT solarSystemID FROM staStations WHERE stationID = ?`, stationID).Scan(&ssID)
	if err != nil {
		return 0, fmt.Errorf("GetSolarSystemForStation: %w", err)
	}
	return ssID, nil
}
