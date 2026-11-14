package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/marshal"
)

// factoryMaterialCols matches the columns the client expects for
// factory.GetMaterialsForTypeWithActivity (a util.Rowset). The blueprint info window groups rows
// by `activity` and reads requiredTypeID, quantity and damagePerJob.
var factoryMaterialCols = []string{
	"activity", "requiredTypeID", "quantity", "damagePerJob", "recycle",
}

// GetMaterialsForTypeWithActivity returns a util.Rowset of material requirements for a
// blueprintTypeID from tl2materialsfortypewithactivity. activityID=-1 means all activities.
// Skill rows (category 16) are included; the client renders them under Required Skills.
func GetMaterialsForTypeWithActivity(ctx context.Context, db *sql.DB, typeID, activityID int64) (marshal.PyRep, error) {
	query := `
		SELECT activity, requiredTypeID, quantity, damagePerJob, recycle
		  FROM tl2materialsfortypewithactivity
		 WHERE typeID = ?`
	args := []interface{}{typeID}
	if activityID >= 0 {
		query += ` AND activity = ?`
		args = append(args, activityID)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return makeRowset(factoryMaterialCols, nil), fmt.Errorf("GetMaterialsForTypeWithActivity: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var activity, requiredTypeID, quantity, recycle int64
		var damagePerJob float64
		if err := rows.Scan(&activity, &requiredTypeID, &quantity, &damagePerJob, &recycle); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{
			marshal.PyInt{Value: uint64(activity)},
			marshal.PyInt{Value: uint64(requiredTypeID)},
			marshal.PyInt{Value: uint64(quantity)},
			marshal.PyReal{Value: damagePerJob},
			marshal.PyInt{Value: uint64(recycle)},
		})
	}
	return makeRowset(factoryMaterialCols, result), nil
}

// GetMaterialCompositionOfItemType returns the base material composition of a
// type as a util.Rowset with typeID/quantity columns.  The blueprint info
// window iterates it to compute player-adjusted material amounts.
func GetMaterialCompositionOfItemType(ctx context.Context, db *sql.DB, typeID int64) (marshal.PyRep, error) {
	cols := []string{"typeID", "quantity"}
	mats, err := GetTypeMaterials(ctx, db, typeID)
	if err != nil {
		return makeRowset(cols, nil), err
	}
	if len(mats) == 0 {
		// An outpost station type has no composition of its own; the platform's Show Info asks for it
		// (infosvc) and the platform blueprint's recipe builds that station.
		mats = outpostStationRecipe(ctx, db, typeID)
	}
	var result [][]marshal.PyRep
	for _, m := range mats {
		result = append(result, []marshal.PyRep{
			marshal.PyInt{Value: uint64(m.MaterialTypeID)},
			marshal.PyInt{Value: uint64(m.Quantity)},
		})
	}
	return makeRowset(cols, result), nil
}

// outpostStationRecipe returns the components that build an outpost station
// type: the activity-1 rows (quantity > 0, skills excluded) of the blueprint
// whose product is the platform carrying stationTypeID (attr 472). Nil when
// stationTypeID is no outpost.
func outpostStationRecipe(ctx context.Context, db *sql.DB, stationTypeID int64) []TypeMaterial {
	rows, err := db.QueryContext(ctx, `
		SELECT m.requiredTypeID, m.quantity
		  FROM dgmTypeAttributes a
		  JOIN invBlueprintTypes b ON b.productTypeID = a.typeID
		  JOIN tl2materialsfortypewithactivity m ON m.typeID = b.blueprintTypeID AND m.activity = 1
		  JOIN invTypes t ON t.typeID = m.requiredTypeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE a.attributeID = 472
		   AND COALESCE(a.valueInt, CAST(a.valueFloat AS SIGNED)) = ?
		   AND m.quantity > 0 AND g.categoryID <> 16
		 ORDER BY m.requiredTypeID`, stationTypeID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []TypeMaterial
	seen := map[int64]bool{}
	for rows.Next() {
		var m TypeMaterial
		if rows.Scan(&m.MaterialTypeID, &m.Quantity) == nil && !seen[m.MaterialTypeID] {
			seen[m.MaterialTypeID] = true
			out = append(out, m)
		}
	}
	return out
}
