package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// TypeMaterial holds one output material row for reprocessing.
type TypeMaterial struct {
	MaterialTypeID int64
	Quantity       int64
}

// GetTypeMaterials returns the base material composition for a typeID: ore from
// invorereprocessing, other items from tl2materialsfortypewithactivity (activity=6), where
// typeID is the item reprocessed and requiredTypeID the output. recycle=1 rows are stored
// inverted and excluded. damagePerJob below 1 marks a skill or tool the build needs but does
// not consume, so those rows are excluded too; every reprocessing path yields this list.
func GetTypeMaterials(ctx context.Context, db *sql.DB, typeID int64) ([]TypeMaterial, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT mineralTypeID, amountPerBatch
		  FROM invorereprocessing
		 WHERE oreTypeID = ?
		UNION ALL
		SELECT requiredTypeID, quantity
		  FROM tl2materialsfortypewithactivity
		 WHERE typeID = ? AND activity = 6 AND recycle = 0 AND damagePerJob >= 1`, typeID, typeID)
	if err != nil {
		return nil, fmt.Errorf("GetTypeMaterials: %w", err)
	}
	defer rows.Close()
	var mats []TypeMaterial
	for rows.Next() {
		var m TypeMaterial
		if err := rows.Scan(&m.MaterialTypeID, &m.Quantity); err != nil {
			return nil, fmt.Errorf("GetTypeMaterials scan: %w", err)
		}
		mats = append(mats, m)
	}
	return mats, rows.Err()
}

// GetTypePortionSize returns the portionSize from invTypes for a typeID.
func GetTypePortionSize(ctx context.Context, db *sql.DB, typeID int64) (int64, error) {
	var ps int64
	err := db.QueryRowContext(ctx,
		`SELECT portionSize FROM invTypes WHERE typeID = ?`, typeID).Scan(&ps)
	if err != nil {
		return 0, fmt.Errorf("GetTypePortionSize: %w", err)
	}
	return ps, nil
}

// StationReprocessingInfo holds reprocessing parameters for a station.
type StationReprocessingInfo struct {
	Efficiency    float64
	StationTake   float64
	CorporationID int64
}

// GetStationReprocessingInfo returns reprocessing params for a station.
func GetStationReprocessingInfo(ctx context.Context, db *sql.DB, stationID int64) (*StationReprocessingInfo, error) {
	var sri StationReprocessingInfo
	err := db.QueryRowContext(ctx,
		`SELECT reprocessingEfficiency, reprocessingStationsTake, corporationID
		 FROM staStations WHERE stationID = ?`, stationID).Scan(
		&sri.Efficiency, &sri.StationTake, &sri.CorporationID)
	if err != nil {
		return nil, fmt.Errorf("GetStationReprocessingInfo: %w", err)
	}
	return &sri, nil
}

// HangarItemInfo holds info about an item in a character's hangar for reprocessing.
type HangarItemInfo struct {
	ItemID     int64
	TypeID     int64
	OwnerID    int64
	LocationID int64
	Quantity   int64
	Singleton  int64
	GroupID    int64
	CategoryID int64
}

// GetHangarItem returns an item the character can reprocess at the given station: in the
// station hangar (flag=4) or in the cargo of one of their ships docked there (flag=5; the
// client calls Reprocess while the ore is still in cargo). Reprocessing is local.
func GetHangarItem(ctx context.Context, db *sql.DB, itemID, charID, stationID int64) (*HangarItemInfo, error) {
	var hi HangarItemInfo
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.quantity, e.singleton,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ? AND e.ownerID = ?
		  AND ( (e.flag = 4 AND e.locationID = ?)
		     OR (e.flag = 5 AND EXISTS (
		            SELECT 1 FROM entity s
		             WHERE s.itemID = e.locationID AND s.ownerID = e.ownerID
		               AND s.locationID = ?)) )`,
		itemID, charID, stationID, stationID).Scan(
		&hi.ItemID, &hi.TypeID, &hi.OwnerID, &hi.LocationID, &hi.Quantity, &hi.Singleton, &hi.GroupID, &hi.CategoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetHangarItem: %w", err)
	}
	return &hi, nil
}

// ReduceEntityQuantity decreases entity quantity. Deletes if it reaches 0.
func ReduceEntityQuantity(ctx context.Context, db DBTX, itemID, amount int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, amount, itemID)
	if err != nil {
		return fmt.Errorf("ReduceEntityQuantity: %w", err)
	}
	// Clean up zero-quantity rows.
	_, _ = db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ? AND quantity <= 0`, itemID)
	return nil
}

// FindHangarItemByType looks for an existing stack (flag=4) of the given typeID
// owned by ownerID at locationID. Returns (itemID, quantity) or (0, 0).
func FindHangarItemByType(ctx context.Context, db DBTX, typeID, ownerID, locationID int64) (int64, int64, error) {
	var itemID, qty int64
	err := db.QueryRowContext(ctx,
		`SELECT itemID, quantity FROM entity
		  WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = 4 AND singleton = 0
		  LIMIT 1`,
		typeID, ownerID, locationID,
	).Scan(&itemID, &qty)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("FindHangarItemByType: %w", err)
	}
	return itemID, qty, nil
}

// GetOreProcessingSkill returns the specific ore processing skill typeID for a
// given ore groupID. Returns scrapmetal processing (12196) for non-ore items.
func GetOreProcessingSkill(ctx context.Context, db *sql.DB, groupID int64) (int64, error) {
	// Each ore group maps to a skill via dgmTypeAttributes attributeID=790 (reprocessingSkillType)
	// on the group's representative type; fall back to scrapmetal processing.
	var skillTypeID int64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(
			(SELECT COALESCE(da.valueInt, CAST(da.valueFloat AS SIGNED))
			 FROM invTypes t2
			 JOIN dgmTypeAttributes da ON da.typeID = t2.typeID AND da.attributeID = 790
			 WHERE t2.groupID = ?
			 LIMIT 1),
			12196
		)`, groupID).Scan(&skillTypeID)
	if err != nil {
		return 12196, nil // fallback to scrapmetal processing
	}
	if skillTypeID == 0 {
		return 12196, nil
	}
	return skillTypeID, nil
}

// FindStackAt is FindHangarItemByType at any flag: a job delivering into a corporation's
// division of a POS array merges into the stack there.
func FindStackAt(ctx context.Context, db DBTX, typeID, ownerID, locationID, flag int64) (int64, int64, error) {
	var itemID, qty int64
	err := db.QueryRowContext(ctx,
		`SELECT itemID, quantity FROM entity
		  WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = ? AND singleton = 0
		  LIMIT 1`,
		typeID, ownerID, locationID, flag,
	).Scan(&itemID, &qty)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("FindStackAt: %w", err)
	}
	return itemID, qty, nil
}
