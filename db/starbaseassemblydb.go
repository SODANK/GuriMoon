// Package db: assembly arrays and mobile laboratories as industry installations.
//
// The S&I window locates a POS installation by containerLocationID = its solar system
// (manufacturing.py); this file creates the ramassemblylines rows for an array. An online
// array carries its lines (owned and restricted to the tower's corporation), any other has
// none. SyncStructureAssemblyLines reconciles them, and a line with an undelivered job is
// never removed. Line counts per array type come from starbaseAssemblyLineDefaults, because
// raminstallationtypedefaultcontents is keyed on installationTypeID alone and cannot hold a
// laboratory's three line types.
package db

import (
	"context"
	"database/sql"
	"fmt"
	"sync"

	"gurimoon/marshal"
)

// posLineDefaults are the seed slot counts (installation typeID, assemblyLineTypeID,
// quantity). Only the Mobile Laboratory's 3 ME / 3 PE / 1 copy is evidenced in the data.
var posLineDefaults = []struct{ typeID, lineTypeID, qty int64 }{
	{16216, 28, 3}, {16216, 29, 3}, {16216, 30, 1}, // Mobile Laboratory: ME / PE / Copy
	{13780, 24, 10}, // Efficient Equipment Assembly Array
	{16220, 23, 10}, // Rapid Equipment Assembly Array
	{24658, 25, 10}, // Ammunition Assembly Array
	{24659, 26, 10}, // Drone Assembly Array
	{24660, 27, 10}, // Component Assembly Array
	{24574, 17, 10}, // Small Ship Assembly Array
	{24653, 18, 3},  // Advanced Small Ship Assembly Array
	{24654, 19, 5},  // Medium Ship Assembly Array
	{24655, 20, 3},  // Advanced Medium Ship Assembly Array
	{24656, 21, 2},  // Large Ship Assembly Array
	{24657, 22, 1},  // Advanced Large Ship Assembly Array
	{24575, 10, 1},  // Capital Ship Assembly Array
	{24610, 10, 1},  // Ultra Fast Capital Ship Assembly Array
	{25305, 37, 1},  // Drug Lab
}

// RestrictLineByCorp is ramassemblylines.restrictionMask's corporation bit.
const RestrictLineByCorp = 4

var (
	posLineSchemaOnce sync.Once
	posLineSchemaErr  error
	posLineMu         sync.Mutex // serialises line-ID allocation
)

func ensurePOSLineDefaults(ctx context.Context, db *sql.DB) error {
	posLineSchemaOnce.Do(func() {
		if _, err := db.ExecContext(ctx, `
			CREATE TABLE IF NOT EXISTS starbaseAssemblyLineDefaults (
			  installationTypeID INT UNSIGNED NOT NULL,
			  assemblyLineTypeID INT UNSIGNED NOT NULL,
			  quantity           INT UNSIGNED NOT NULL,
			  PRIMARY KEY (installationTypeID, assemblyLineTypeID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
			posLineSchemaErr = fmt.Errorf("ensurePOSLineDefaults: %w", err)
			return
		}
		// Seed once: an operator who deletes or edits rows keeps the edit.
		var n int
		_ = db.QueryRowContext(ctx, `SELECT COUNT(*) FROM starbaseAssemblyLineDefaults`).Scan(&n)
		if n > 0 {
			return
		}
		for _, d := range posLineDefaults {
			if _, err := db.ExecContext(ctx, `
				INSERT IGNORE INTO starbaseAssemblyLineDefaults (installationTypeID, assemblyLineTypeID, quantity)
				VALUES (?, ?, ?)`, d.typeID, d.lineTypeID, d.qty); err != nil {
				posLineSchemaErr = fmt.Errorf("seed POS line defaults: %w", err)
				return
			}
		}
	})
	return posLineSchemaErr
}

// SyncStructureAssemblyLines reconciles the lines of one array (arrayID > 0)
// or of every array on the shard (arrayID == 0) with the arrays' states.
func SyncStructureAssemblyLines(ctx context.Context, db *sql.DB, arrayID int64) error {
	if err := ensurePOSLineDefaults(ctx, db); err != nil {
		return err
	}
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return err
	}
	posLineMu.Lock()
	defer posLineMu.Unlock()

	where := ""
	args := []interface{}{GroupAssemblyArray, GroupMobileLaboratory}
	if arrayID > 0 {
		where = " AND s.entityID = ?"
		args = append(args, arrayID)
	}
	type arr struct{ id, typeID, owner int64 }
	rows, err := db.QueryContext(ctx, `
		SELECT s.entityID, e.typeID, s.ownerCorpID
		  FROM starbaseStructures s
		  JOIN entity e ON e.itemID = s.entityID
		  JOIN invTypes t ON t.typeID = e.typeID
		 WHERE t.groupID IN (?, ?) AND s.state = 'online'`+where, args...)
	if err != nil {
		return fmt.Errorf("SyncStructureAssemblyLines list: %w", err)
	}
	var online []arr
	for rows.Next() {
		var a arr
		if err := rows.Scan(&a.id, &a.typeID, &a.owner); err != nil {
			rows.Close()
			return err
		}
		online = append(online, a)
	}
	rows.Close()

	for _, a := range online {
		if err := ensureArrayLines(ctx, db, a.id, a.typeID, a.owner); err != nil {
			return err
		}
	}

	// Remove idle lines on containers that are not online arrays. POS lines are the
	// ones whose container is outside the station ID band 60M-64M.
	delWhere := `al.containerID NOT BETWEEN 60000000 AND 63999999
	             AND NOT EXISTS (SELECT 1 FROM starbaseStructures s
	                              WHERE s.entityID = al.containerID AND s.state = 'online')
	             AND NOT EXISTS (SELECT 1 FROM ramJobs j
	                              WHERE j.assemblyLineID = al.assemblyLineID AND j.completedStatusID < 100)`
	delArgs := []interface{}{}
	if arrayID > 0 {
		delWhere += ` AND al.containerID = ?`
		delArgs = append(delArgs, arrayID)
	}
	if _, err := db.ExecContext(ctx, `DELETE al FROM ramassemblylines al WHERE `+delWhere, delArgs...); err != nil {
		return fmt.Errorf("SyncStructureAssemblyLines remove: %w", err)
	}
	return nil
}

// ensureArrayLines tops an online array up to its default line counts and keeps
// the lines owned by the tower's current corporation.
func ensureArrayLines(ctx context.Context, db *sql.DB, arrayID, typeID, ownerCorpID int64) error {
	defs, err := db.QueryContext(ctx, `
		SELECT d.assemblyLineTypeID, d.quantity, COALESCE(t.activityID, 1)
		  FROM starbaseAssemblyLineDefaults d
		  LEFT JOIN ramassemblylinetypes t ON t.assemblyLineTypeID = d.assemblyLineTypeID
		 WHERE d.installationTypeID = ?`, typeID)
	if err != nil {
		return fmt.Errorf("ensureArrayLines defaults: %w", err)
	}
	type def struct{ lineType, qty, activity int64 }
	var want []def
	for defs.Next() {
		var d def
		if err := defs.Scan(&d.lineType, &d.qty, &d.activity); err != nil {
			defs.Close()
			return err
		}
		want = append(want, d)
	}
	defs.Close()
	if len(want) == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE ramassemblylines SET ownerID = ? WHERE containerID = ? AND COALESCE(ownerID, 0) <> ?`,
		ownerCorpID, arrayID, ownerCorpID); err != nil {
		return fmt.Errorf("ensureArrayLines owner: %w", err)
	}
	for _, d := range want {
		var have int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM ramassemblylines WHERE containerID = ? AND assemblyLineTypeID = ?`,
			arrayID, d.lineType).Scan(&have); err != nil {
			return err
		}
		for i := have; i < d.qty; i++ {
			var next int64
			if err := db.QueryRowContext(ctx,
				`SELECT COALESCE(MAX(assemblyLineID), 0) + 1 FROM ramassemblylines`).Scan(&next); err != nil {
				return err
			}
			if _, err := db.ExecContext(ctx, `
				INSERT INTO ramassemblylines
				  (assemblyLineID, assemblyLineTypeID, containerID, UIGroupingID,
				   costInstall, costPerHour, restrictionMask,
				   discountPerGoodStandingPoint, surchargePerBadStandingPoint,
				   minimumStanding, minimumCharSecurity, minimumCorpSecurity,
				   maximumCharSecurity, maximumCorpSecurity, ownerID, activityID)
				VALUES (?, ?, ?, 0, 0, 0, ?, 0, 0, 0, 0, 0, 0, 0, ?, ?)`,
				next, d.lineType, arrayID, RestrictLineByCorp, ownerCorpID, d.activity); err != nil {
				return fmt.Errorf("ensureArrayLines insert: %w", err)
			}
		}
	}
	return nil
}

// POSInstallation describes an in-space installation container.
type POSInstallation struct {
	ArrayID, TypeID, GroupID, SolarSystemID, OwnerCorpID, TowerID int64
	State                                                        string
}

// GetPOSInstallation reports whether containerID is an assembly array or
// mobile laboratory anchored at a tower.
func GetPOSInstallation(ctx context.Context, db *sql.DB, containerID int64) (POSInstallation, bool, error) {
	s, ok, err := GetStarbaseStructure(ctx, db, containerID)
	if err != nil || !ok {
		return POSInstallation{}, false, err
	}
	if s.GroupID != GroupAssemblyArray && s.GroupID != GroupMobileLaboratory {
		return POSInstallation{}, false, nil
	}
	return POSInstallation{
		ArrayID: s.EntityID, TypeID: s.TypeID, GroupID: s.GroupID, SolarSystemID: s.SolarSystemID,
		OwnerCorpID: s.OwnerCorpID, TowerID: s.ControlTowerID, State: s.State,
	}, true, nil
}

// GetStructureAssemblyLinesSelect is AssemblyLinesSelect's answer for the POS
// installations owned by any of ownerIDs: one row per array and line type.
func GetStructureAssemblyLinesSelect(ctx context.Context, db *sql.DB, ownerIDs []int64) (marshal.PyRep, int, error) {
	if len(ownerIDs) == 0 {
		return makeRowset(assemblyLineCols, nil), 0, nil
	}
	q := assemblyLineQuery + ` WHERE al.containerID NOT BETWEEN 60000000 AND 63999999 AND al.ownerID IN (`
	args := make([]interface{}, 0, len(ownerIDs))
	for i, id := range ownerIDs {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, id)
	}
	q += `) AND ` + clientAssemblyActivities + ` GROUP BY al.containerID, al.assemblyLineTypeID`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return makeRowset(assemblyLineCols, nil), 0, fmt.Errorf("GetStructureAssemblyLinesSelect: %w", err)
	}
	defer rows.Close()
	out := scanAssemblyLineRows(rows)
	return makeRowset(assemblyLineCols, out), len(out), nil
}

// CorpIDsOfAlliance lists the member corporations of an alliance.
func CorpIDsOfAlliance(ctx context.Context, db *sql.DB, allianceID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `SELECT corporationID FROM corporation WHERE allianceID = ?`, allianceID)
	if err != nil {
		return nil, err
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

// LineCanBuild reports whether an assembly line type may manufacture a product. A
// line type with no ramaltypegroup/ramaltypecategory rows builds anything; one with
// rows builds only what they name.
func LineCanBuild(ctx context.Context, db DBTX, lineTypeID, productTypeID int64) bool {
	var rowsN, match int
	_ = db.QueryRowContext(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM ramaltypegroup WHERE assemblyLineTypeID = ?) +
		  (SELECT COUNT(*) FROM ramaltypecategory WHERE assemblyLineTypeID = ?),
		  (SELECT COUNT(*) FROM invTypes t
		     JOIN invGroups g ON g.groupID = t.groupID
		    WHERE t.typeID = ? AND (
		      t.groupID IN (SELECT groupID FROM ramaltypegroup WHERE assemblyLineTypeID = ?) OR
		      g.categoryID IN (SELECT categoryID FROM ramaltypecategory WHERE assemblyLineTypeID = ?)))`,
		lineTypeID, lineTypeID, productTypeID, lineTypeID, lineTypeID).Scan(&rowsN, &match)
	return rowsN == 0 || match > 0
}
