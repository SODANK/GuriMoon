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
	// Flag is the item's entity flag: 4 or 5 for the character's own hangar or cargo, the
	// division flag (4, 116-121) for a stack in its corporation's office.
	Flag int64
}

// CorpOwned reports whether the item is the corporation's, refined in place: the output then
// stays corporation property in the same division (ReprocessOutputSlot).
func (hi *HangarItemInfo) CorpOwned(charID int64) bool {
	return hi != nil && hi.OwnerID != charID
}

// GetHangarItem returns an item the character can reprocess at the given station: in the
// station hangar (flag=4), in the cargo of one of their ships docked there (flag=5; the
// client calls Reprocess while the ore is still in cargo), or in a division of its
// corporation's office there when it may take from that division (corpHangarItem).
// Reprocessing is local.
func GetHangarItem(ctx context.Context, db *sql.DB, itemID, charID, stationID int64) (*HangarItemInfo, error) {
	var hi HangarItemInfo
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.quantity, e.singleton,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0), e.flag
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
		&hi.ItemID, &hi.TypeID, &hi.OwnerID, &hi.LocationID, &hi.Quantity, &hi.Singleton, &hi.GroupID, &hi.CategoryID, &hi.Flag)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return corpHangarItem(ctx, db, itemID, charID, stationID)
		}
		return nil, fmt.Errorf("GetHangarItem: %w", err)
	}
	return &hi, nil
}

// corpHangarItem is GetHangarItem for a stack in the character's corporation's office at the
// station (a rented office, or the station itself for an owner): it answers the item only when
// the member may take from that division there (CorpHangarTakeAllowed), as moving it out would
// need. nil, nil for anything else, which the client reads as "contains no materials".
func corpHangarItem(ctx context.Context, db *sql.DB, itemID, charID, stationID int64) (*HangarItemInfo, error) {
	var corpID int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(corporationID, 0) FROM character_ WHERE characterID = ?`, charID,
	).Scan(&corpID); err != nil || corpID == 0 {
		return nil, nil
	}
	officeID, _, err := GetCorpOfficeAt(ctx, db, corpID, stationID)
	if err != nil || officeID == 0 {
		return nil, err
	}
	var hi HangarItemInfo
	err = db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.quantity, e.singleton,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0), e.flag
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ? AND e.ownerID = ? AND e.locationID = ?
		  AND (e.flag = ? OR e.flag BETWEEN ? AND ?)`,
		itemID, corpID, officeID, FlagCorpHangarDiv1, FlagCorpSAGFirst, FlagCorpSAGLast).Scan(
		&hi.ItemID, &hi.TypeID, &hi.OwnerID, &hi.LocationID, &hi.Quantity, &hi.Singleton, &hi.GroupID, &hi.CategoryID, &hi.Flag)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetHangarItem corp: %w", err)
	}
	if !CorpHangarTakeAllowed(ctx, db, corpID, charID, stationID, hi.Flag) {
		return nil, nil
	}
	return &hi, nil
}

// ReprocessOutputSlot is where a reprocessed item's minerals land: owner, location and flag.
// The character's own ore (hangar or cargo) refines into its station hangar; a corporation's
// refines into the same division of the same office, still the corporation's.
func ReprocessOutputSlot(hi *HangarItemInfo, charID, stationID int64) (ownerID, locationID, flag int64) {
	if hi.CorpOwned(charID) {
		return hi.OwnerID, hi.LocationID, hi.Flag
	}
	return charID, stationID, FlagCorpHangarDiv1
}

// CreateStackAt creates a packaged stack of typeID at an owner, location and flag (CreateItem
// writes flag 4 only).
func CreateStackAt(ctx context.Context, db DBTX, typeID, ownerID, locationID, flag, quantity int64) (int64, error) {
	newID, err := CreateItem(ctx, db, typeID, ownerID, locationID, int(quantity))
	if err != nil {
		return 0, err
	}
	if flag != FlagCorpHangarDiv1 {
		if _, err := db.ExecContext(ctx,
			`UPDATE entity SET flag = ? WHERE itemID = ?`, flag, newID); err != nil {
			return 0, fmt.Errorf("CreateStackAt flag: %w", err)
		}
	}
	return newID, nil
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
