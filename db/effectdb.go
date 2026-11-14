package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

// clientBugOffensiveEffects lists effects cleared to isOffensive 0 so the client skips
// its attack confirmation; the server still treats them as offensive.
// See docs/db-12-notes.md#client-attack-confirmation.
var clientBugOffensiveEffects = map[string]bool{
	"empWaveGrid": true, // the four doomsdays (invGroups 588)
}

// applyEffectOverrides restores server-side flags the database clears to work around
// a client bug. Call it on every dgmEffects read.
func applyEffectOverrides(effectName string, ei *EffectInfo) {
	if clientBugOffensiveEffects[effectName] {
		ei.IsOffensive = true
	}
}

// GetAllEffects loads all named effects from dgmEffects into a map keyed by
// effectName, to be loaded once at startup.
func GetAllEffects(ctx context.Context, db *sql.DB) (map[string]EffectInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT effectID, effectName, COALESCE(guid, ''), COALESCE(isOffensive, 0),
		       COALESCE(durationAttributeID, 0),
		       COALESCE(rangeChance, 0), COALESCE(electronicChance, 0),
		       COALESCE(propulsionChance, 0),
		       COALESCE(rangeAttributeID, 0), COALESCE(falloffAttributeID, 0),
		       COALESCE(isWarpSafe, 0)
		FROM dgmEffects
		WHERE effectName IS NOT NULL AND effectName != ''`)
	if err != nil {
		return nil, fmt.Errorf("GetAllEffects: %w", err)
	}
	defer rows.Close()

	out := make(map[string]EffectInfo)
	for rows.Next() {
		var effectName string
		var ei EffectInfo
		var offensive, rangeCh, elecCh, propCh, warpSafe int64
		if err := rows.Scan(&ei.EffectID, &effectName, &ei.Guid, &offensive, &ei.DurationAttributeID,
			&rangeCh, &elecCh, &propCh, &ei.RangeAttributeID, &ei.FalloffAttributeID, &warpSafe); err != nil {
			return nil, fmt.Errorf("GetAllEffects scan: %w", err)
		}
		ei.IsOffensive = offensive != 0
		ei.WarpSafe = warpSafe != 0
		ei.RangeChance = rangeCh != 0
		ei.ElectronicChance = elecCh != 0
		ei.PropulsionChance = propCh != 0
		applyEffectOverrides(effectName, &ei)
		out[effectName] = ei
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetAllEffects rows: %w", err)
	}
	log.Printf("effectdb: loaded %d effects from dgmEffects", len(out))
	return out, nil
}

// GetChargeTypeID returns the typeID of the charge loaded into the module slot
// (locationID = shipID, flag = slotFlag, categoryID 8), or 0 if none.
func GetChargeTypeID(ctx context.Context, db *sql.DB, shipID, slotFlag int64) (int64, error) {
	var typeID int64
	err := db.QueryRowContext(ctx, `
		SELECT e.typeID FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag = ? AND g.categoryID = 8
		LIMIT 1`, shipID, slotFlag).Scan(&typeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, nil
		}
		return 0, fmt.Errorf("GetChargeTypeID: %w", err)
	}
	return typeID, nil
}

// GetChargeItem returns the itemID and typeID of the charge loaded into the module
// slot (locationID = shipID, flag = slotFlag, categoryID 8), or 0, 0 if none.
func GetChargeItem(ctx context.Context, db *sql.DB, shipID, slotFlag int64) (int64, int64, error) {
	var itemID, typeID int64
	err := db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag = ? AND g.categoryID = 8
		LIMIT 1`, shipID, slotFlag).Scan(&itemID, &typeID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, 0, nil
		}
		return 0, 0, fmt.Errorf("GetChargeItem: %w", err)
	}
	return itemID, typeID, nil
}

// GetItemAttrFloat reads one per-item attribute override from entity_attributes,
// returning defVal when the row is absent.
func GetItemAttrFloat(ctx context.Context, db *sql.DB, itemID, attrID int64, defVal float64) float64 {
	var v sql.NullFloat64
	_ = db.QueryRowContext(ctx,
		`SELECT COALESCE(NULLIF(valueFloat, 0), CAST(valueInt AS DOUBLE), valueFloat)
		 FROM entity_attributes WHERE itemID = ? AND attributeID = ?`,
		itemID, attrID,
	).Scan(&v)
	if v.Valid {
		return v.Float64
	}
	return defVal
}

// SetItemAttrFloat upserts one per-item attribute override (mining crystal wear:
// attributeID 3 damage).
func SetItemAttrFloat(ctx context.Context, db *sql.DB, itemID, attrID int64, val float64) error {
	_, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, ?, NULL, ?)
		ON DUPLICATE KEY UPDATE valueInt = NULL, valueFloat = VALUES(valueFloat)`,
		itemID, attrID, val)
	if err != nil {
		return fmt.Errorf("SetItemAttrFloat: %w", err)
	}
	return nil
}

// MarkItemSingleton turns a single non-singleton item into a singleton, so stack
// merges leave it alone. changed is false when it already was one or is a stack.
func MarkItemSingleton(ctx context.Context, db *sql.DB, itemID int64) (changed bool, err error) {
	res, err := db.ExecContext(ctx,
		`UPDATE entity SET singleton = 1 WHERE itemID = ? AND singleton = 0 AND quantity = 1`, itemID)
	if err != nil {
		return false, fmt.Errorf("MarkItemSingleton: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// notWornSQL filters out items that carry per-item damage (attributeID 3), such as a
// worn crystal, so a stack merge never folds that wear into a fresh stack. alias is
// the entity table alias in the enclosing query.
func notWornSQL(alias string) string {
	return ` AND NOT EXISTS (SELECT 1 FROM entity_attributes wa WHERE wa.itemID = ` + alias +
		`.itemID AND wa.attributeID = 3 AND COALESCE(wa.valueFloat, wa.valueInt, 0) > 0)`
}

// DeleteItemWithAttrs removes an item row and its attribute overrides, for a
// mining crystal that shatters.
func DeleteItemWithAttrs(ctx context.Context, db *sql.DB, itemID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity_attributes WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteItemWithAttrs attrs: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteItemWithAttrs: %w", err)
	}
	return nil
}
