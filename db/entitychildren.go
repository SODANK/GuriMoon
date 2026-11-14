package db

import (
	"context"
	"database/sql"
	"fmt"
)

// GetChildEntityRows returns every item whose locationID is the given item,
// any flag. GetShipChildItems is the ship-shaped subset (cargo, slots, drone
// bay); this one is for destroying a container or an assembled ship, so no row
// is left in a deleted parent.
func GetChildEntityRows(ctx context.Context, db *sql.DB, locationID int64) ([]EntityRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       e.singleton, e.quantity,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		FROM entity e
		LEFT JOIN invTypes  t ON t.typeID  = e.typeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ?`, locationID)
	if err != nil {
		return nil, fmt.Errorf("GetChildEntityRows: %w", err)
	}
	defer rows.Close()
	var out []EntityRow
	for rows.Next() {
		var er EntityRow
		if err := rows.Scan(&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID,
			&er.Flag, &er.Singleton, &er.Quantity, &er.GroupID, &er.CategoryID); err != nil {
			return nil, fmt.Errorf("GetChildEntityRows scan: %w", err)
		}
		out = append(out, er)
	}
	return out, rows.Err()
}
