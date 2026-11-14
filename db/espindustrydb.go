package db

// ESP Industry queries: the static ore/ice reference data behind the asteroid belt tool (type
// names, cargo volume, refine batch size and the mineral outputs each rock type reprocesses into).

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
)

// ESPAsteroidType is one ore/ice type's static data.
type ESPAsteroidType struct {
	TypeID      int64
	TypeName    string
	GroupID     int64
	GroupName   string
	Volume      float64 // m³ per unit
	PortionSize int64   // units per refine batch
	BasePrice   float64
}

// ESPMineralYield is one output of reprocessing a batch of an ore type.
type ESPMineralYield struct {
	TypeID   int64
	TypeName string
	Amount   int64 // per refine batch (PortionSize units)
}

// idList renders a []int64 as a SQL IN-list. Callers only ever pass typeIDs
// they produced themselves, so this stays out of the placeholder dance.
func idList(ids []int64) string {
	parts := make([]string, len(ids))
	for i, id := range ids {
		parts[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(parts, ",")
}

// ESPGetAsteroidTypes returns static data for the given ore/ice typeIDs.
func ESPGetAsteroidTypes(ctx context.Context, db *sql.DB, typeIDs []int64) (map[int64]ESPAsteroidType, error) {
	out := make(map[int64]ESPAsteroidType, len(typeIDs))
	if len(typeIDs) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, COALESCE(g.groupName, ''),
		       COALESCE(t.volume, 0), COALESCE(t.portionSize, 1), COALESCE(t.basePrice, 0)
		  FROM invTypes t
		  LEFT JOIN invGroups g ON g.groupID = t.groupID
		 WHERE t.typeID IN (`+idList(typeIDs)+`)`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetAsteroidTypes: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var a ESPAsteroidType
		if err := rows.Scan(&a.TypeID, &a.TypeName, &a.GroupID, &a.GroupName,
			&a.Volume, &a.PortionSize, &a.BasePrice); err != nil {
			return nil, fmt.Errorf("ESPGetAsteroidTypes scan: %w", err)
		}
		if a.PortionSize <= 0 {
			a.PortionSize = 1
		}
		out[a.TypeID] = a
	}
	return out, rows.Err()
}

// ESPGetReprocessOutputs returns the reprocessing yield of each given ore/ice typeID, keyed by
// typeID. Amounts are per refine batch; divide by the type's portionSize for a per-unit figure.
func ESPGetReprocessOutputs(ctx context.Context, db *sql.DB, typeIDs []int64) (map[int64][]ESPMineralYield, error) {
	out := make(map[int64][]ESPMineralYield, len(typeIDs))
	if len(typeIDs) == 0 {
		return out, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT r.oreTypeID, r.mineralTypeID, COALESCE(t.typeName, ''), r.amountPerBatch
		  FROM invorereprocessing r
		  LEFT JOIN invTypes t ON t.typeID = r.mineralTypeID
		 WHERE r.oreTypeID IN (`+idList(typeIDs)+`)
		 ORDER BY r.oreTypeID, r.amountPerBatch DESC`)
	if err != nil {
		return nil, fmt.Errorf("ESPGetReprocessOutputs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var oreID int64
		var y ESPMineralYield
		if err := rows.Scan(&oreID, &y.TypeID, &y.TypeName, &y.Amount); err != nil {
			return nil, fmt.Errorf("ESPGetReprocessOutputs scan: %w", err)
		}
		out[oreID] = append(out[oreID], y)
	}
	return out, rows.Err()
}
