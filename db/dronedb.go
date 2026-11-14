package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"
)

// EnsureDroneTables creates the droneSpaceState table if absent.
func EnsureDroneTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS droneSpaceState (
			itemID BIGINT NOT NULL PRIMARY KEY,
			ownerCharID BIGINT NOT NULL,
			homeShipID BIGINT NOT NULL,
			solarSystemID BIGINT NOT NULL,
			launchedAt BIGINT NOT NULL,
			KEY idx_dss_system (solarSystemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8`)
	if err != nil {
		return fmt.Errorf("EnsureDroneTables: %w", err)
	}
	return nil
}

// DroneBayItem is one drone stack sitting in a ship's drone bay (flag 87).
type DroneBayItem struct {
	ItemID     int64
	TypeID     int64
	TypeName   string
	GroupID    int64
	CategoryID int64
	Quantity   int64
	Mass       float64
	Radius     float64
	Volume     float64
}

// GetDroneBayItems lists everything in shipID's drone bay (flag 87).
func GetDroneBayItems(ctx context.Context, db *sql.DB, shipID int64) ([]DroneBayItem, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, t.groupID, g.categoryID,
		       e.quantity, t.mass, t.radius, t.volume
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag = 87`, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetDroneBayItems: %w", err)
	}
	defer rows.Close()
	var out []DroneBayItem
	for rows.Next() {
		var d DroneBayItem
		if err := rows.Scan(&d.ItemID, &d.TypeID, &d.TypeName, &d.GroupID, &d.CategoryID,
			&d.Quantity, &d.Mass, &d.Radius, &d.Volume); err != nil {
			return nil, fmt.Errorf("GetDroneBayItems scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetDroneBayItem fetches one drone-bay stack by itemID, verifying it lives
// in shipID's drone bay. Returns nil, nil if absent.
func GetDroneBayItem(ctx context.Context, db *sql.DB, itemID, shipID int64) (*DroneBayItem, error) {
	var d DroneBayItem
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, t.groupID, g.categoryID,
		       e.quantity, t.mass, t.radius, t.volume
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ? AND e.locationID = ? AND e.flag = 87`, itemID, shipID,
	).Scan(&d.ItemID, &d.TypeID, &d.TypeName, &d.GroupID, &d.CategoryID,
		&d.Quantity, &d.Mass, &d.Radius, &d.Volume)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetDroneBayItem: %w", err)
	}
	return &d, nil
}

// LaunchDroneToSpace moves one drone unit from a bay stack into space. A quantity-1 stack moves
// wholesale (keeps its itemID); a larger stack is decremented and a fresh singleton row is
// created. shipID is the home ship for the droneSpaceState row. Returns the itemID of the drone
// now in space.
func LaunchDroneToSpace(ctx context.Context, db *sql.DB, itemID, ownerCharID, shipID, solarSystemID int64, x, y, z float64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("LaunchDroneToSpace begin: %w", err)
	}
	defer tx.Rollback()

	var typeID, quantity int64
	var itemName string
	if err := tx.QueryRowContext(ctx,
		`SELECT typeID, quantity, itemName FROM entity WHERE itemID = ? AND flag = 87`,
		itemID).Scan(&typeID, &quantity, &itemName); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("drone %d not in a drone bay", itemID)
		}
		return 0, fmt.Errorf("LaunchDroneToSpace select: %w", err)
	}

	launchedID := itemID
	if quantity <= 1 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET locationID = ?, flag = 0, singleton = 1, quantity = 1,
			       ownerID = ?, x = ?, y = ?, z = ?
			WHERE itemID = ?`,
			solarSystemID, ownerCharID, x, y, z, itemID); err != nil {
			return 0, fmt.Errorf("LaunchDroneToSpace move: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - 1 WHERE itemID = ?`, itemID); err != nil {
			return 0, fmt.Errorf("LaunchDroneToSpace split: %w", err)
		}
		res, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES (?, ?, ?, ?, 0, 0, 1, 1, ?, ?, ?, NULL)`,
			itemName, typeID, ownerCharID, solarSystemID, x, y, z)
		if err != nil {
			return 0, fmt.Errorf("LaunchDroneToSpace insert: %w", err)
		}
		launchedID, err = res.LastInsertId()
		if err != nil {
			return 0, fmt.Errorf("LaunchDroneToSpace lastID: %w", err)
		}
	}

	now := timeNowUnix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO droneSpaceState (itemID, ownerCharID, homeShipID, solarSystemID, launchedAt)
		VALUES (?, ?, ?, ?, ?)`, launchedID, ownerCharID, shipID, solarSystemID, now); err != nil {
		return 0, fmt.Errorf("LaunchDroneToSpace drone state: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("LaunchDroneToSpace commit: %w", err)
	}
	return launchedID, nil
}

// timeNowUnix returns the current time as unix seconds. Isolated for testing.
func timeNowUnix() int64 { return timeNow().Unix() }

// timeNow is a func var for testability.
var timeNow = func() time.Time { return time.Now() }

// ScoopDroneToBay moves an in-space drone item into shipID's drone bay and
// removes its droneSpaceState tracking row.
func ScoopDroneToBay(ctx context.Context, db *sql.DB, itemID, shipID int64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 87, x = 0, y = 0, z = 0
		WHERE itemID = ?`, shipID, itemID)
	if err != nil {
		return fmt.Errorf("ScoopDroneToBay: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ScoopDroneToBay: item %d not found", itemID)
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM droneSpaceState WHERE itemID = ?`, itemID)
	return nil
}

// ScoopDroneToHold is ScoopDroneToBay for the menu's "Scoop To Cargo Hold": the drone goes to the
// cargo hold (flag 5) and is handed to newOwnerID, because a cargo hold lists only what its pilot
// owns and an abandoned drone scooped by someone else must change hands.
func ScoopDroneToHold(ctx context.Context, db *sql.DB, itemID, shipID, newOwnerID int64) error {
	res, err := db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 5, ownerID = ?, x = 0, y = 0, z = 0
		WHERE itemID = ?`, shipID, newOwnerID, itemID)
	if err != nil {
		return fmt.Errorf("ScoopDroneToHold: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("ScoopDroneToHold: item %d not found", itemID)
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM droneSpaceState WHERE itemID = ?`, itemID)
	return nil
}

// SetEntityOwner reassigns one item row to ownerID. Used when a pilot takes something out of a
// loot container or structure bay, whose rows carry the old owner.
func SetEntityOwner(ctx context.Context, db *sql.DB, itemID, ownerID int64) error {
	if _, err := db.ExecContext(ctx, `UPDATE entity SET ownerID = ? WHERE itemID = ?`, ownerID, itemID); err != nil {
		return fmt.Errorf("SetEntityOwner: %w", err)
	}
	return nil
}

// MoveStackToDroneBay moves a whole stack into shipID's drone bay (flag 87), keeping quantity;
// FitModuleToShip forces singleton and quantity 1.
func MoveStackToDroneBay(ctx context.Context, db *sql.DB, itemID, shipID int64) error {
	res, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = 87 WHERE itemID = ?`, shipID, itemID)
	if err != nil {
		return fmt.Errorf("MoveStackToDroneBay: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("MoveStackToDroneBay: item %d not found", itemID)
	}
	return nil
}

// GetDroneBayUsedVolume sums the volume of everything in the ship's drone bay
// (flag 87).
func GetDroneBayUsedVolume(ctx context.Context, db *sql.DB, shipID int64) (float64, error) {
	var total sql.NullFloat64
	err := db.QueryRowContext(ctx, `
		SELECT SUM(t.volume * e.quantity)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.flag = 87`, shipID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("GetDroneBayUsedVolume: %w", err)
	}
	if !total.Valid {
		return 0, nil
	}
	return total.Float64, nil
}

// DeleteDroneItem removes a destroyed drone's entity row and its
// droneSpaceState tracking row.
func DeleteDroneItem(ctx context.Context, db *sql.DB, itemID int64) error {
	_, _ = db.ExecContext(ctx,
		`DELETE FROM droneSpaceState WHERE itemID = ?`, itemID)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity_attributes WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteDroneItem attrs: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteDroneItem: %w", err)
	}
	return nil
}

// GetSpaceDroneItem fetches an in-space drone entity row (flag 0, category 18)
// by itemID. Returns nil, nil when the item is not an in-space drone.
func GetSpaceDroneItem(ctx context.Context, db *sql.DB, itemID int64) (*DroneBayItem, error) {
	var d DroneBayItem
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, t.typeName, t.groupID, g.categoryID,
		       e.quantity, t.mass, t.radius, t.volume
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ? AND g.categoryID = 18`, itemID,
	).Scan(&d.ItemID, &d.TypeID, &d.TypeName, &d.GroupID, &d.CategoryID,
		&d.Quantity, &d.Mass, &d.Radius, &d.Volume)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetSpaceDroneItem: %w", err)
	}
	return &d, nil
}

// RehomeDrones updates homeShipID for all in-space drones of ownerCharID in solarSystemID.
// Called when a pilot reclaims their drones after re-shipping.
func RehomeDrones(ctx context.Context, db *sql.DB, ownerCharID, solarSystemID, newShipID int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE droneSpaceState SET homeShipID = ?
		WHERE ownerCharID = ? AND solarSystemID = ?`,
		newShipID, ownerCharID, solarSystemID)
	if err != nil {
		return fmt.Errorf("RehomeDrones: %w", err)
	}
	return nil
}

// ReconcileSpaceDrones reconciles drone entity rows at boot, before any system loads. Tracked
// drones still in space with a live home ship are scooped to the bay; tracked drones whose home
// ship is gone are deleted, as are tracking rows whose entity has moved and legacy orphans
// (category 18, flag 0, in a solar system, no tracking row).
func ReconcileSpaceDrones(ctx context.Context, db *sql.DB) error {
	// 1. Process tracked drones.
	rows, err := db.QueryContext(ctx, `
		SELECT d.itemID, d.homeShipID, e.flag, e.locationID
		FROM droneSpaceState d
		LEFT JOIN entity e ON e.itemID = d.itemID`)
	if err != nil {
		return fmt.Errorf("ReconcileSpaceDrones query: %w", err)
	}

	type tracked struct {
		itemID, homeShipID int64
		flag               sql.NullInt64
		locationID         sql.NullInt64
	}
	var all []tracked
	for rows.Next() {
		var t tracked
		if err := rows.Scan(&t.itemID, &t.homeShipID, &t.flag, &t.locationID); err != nil {
			rows.Close()
			return fmt.Errorf("ReconcileSpaceDrones scan: %w", err)
		}
		all = append(all, t)
	}
	rows.Close()

	scooped, deleted, stale := 0, 0, 0
	for _, t := range all {
		// Entity gone or already moved out of space (flag != 0).
		if !t.flag.Valid || t.flag.Int64 != 0 {
			_, _ = db.ExecContext(ctx,
				`DELETE FROM droneSpaceState WHERE itemID = ?`, t.itemID)
			stale++
			continue
		}
		// Still in space: check the home ship exists.
		var shipExists int64
		err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM entity WHERE itemID = ?`, t.homeShipID).Scan(&shipExists)
		if err != nil || shipExists == 0 {
			// Home ship gone: delete the drone.
			if err := DeleteDroneItem(ctx, db, t.itemID); err != nil {
				log.Printf("[drone] ReconcileSpaceDrones delete orphan %d: %v", t.itemID, err)
			}
			deleted++
			continue
		}
		// Home ship alive: scoop back.
		if err := ScoopDroneToBay(ctx, db, t.itemID, t.homeShipID); err != nil {
			log.Printf("[drone] ReconcileSpaceDrones scoop %d → ship %d: %v", t.itemID, t.homeShipID, err)
		}
		scooped++
	}

	// 2. Legacy orphans: category-18 items with flag 0 in a solar system,
	//    no droneSpaceState row.
	legacyRows, err := db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN droneSpaceState d ON d.itemID = e.itemID
		WHERE g.categoryID = 18 AND e.flag = 0
		  AND e.locationID BETWEEN 30000000 AND 39999999
		  AND d.itemID IS NULL`)
	if err != nil {
		return fmt.Errorf("ReconcileSpaceDrones legacy query: %w", err)
	}
	var legacyIDs []int64
	for legacyRows.Next() {
		var id int64
		if err := legacyRows.Scan(&id); err == nil {
			legacyIDs = append(legacyIDs, id)
		}
	}
	legacyRows.Close()
	for _, id := range legacyIDs {
		if err := DeleteDroneItem(ctx, db, id); err != nil {
			log.Printf("[drone] ReconcileSpaceDrones legacy delete %d: %v", id, err)
		}
	}

	if scooped+deleted+stale+len(legacyIDs) > 0 {
		log.Printf("[db] ReconcileSpaceDrones: scooped=%d deleted=%d stale=%d legacy=%d",
			scooped, deleted, stale, len(legacyIDs))
	}
	return nil
}

// GetCharSkillLevel returns the trained level of skill typeID for charID
// (0 when untrained).
func GetCharSkillLevel(ctx context.Context, db *sql.DB, charID, skillTypeID int64) (int64, error) {
	var level int64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(a.valueInt, 0)
		FROM entity e
		JOIN entity_attributes a ON a.itemID = e.itemID AND a.attributeID = 280
		WHERE e.ownerID = ? AND e.typeID = ? AND e.flag IN (7, 61)`,
		charID, skillTypeID).Scan(&level)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetCharSkillLevel: %w", err)
	}
	return level, nil
}
