// Package db: POS reactions.
//
// A reaction is an item (category 24: groups 436 Simple, 484 Complex, 661/662 Biochemical)
// inside a Mobile Reactor; the Production tab derives demands and supplies from
// cfg.invreactiontypes, so the server only says which item is inside.
// invtypereactions is the recipe: input=1 rows are consumed, input=0 rows produced, in batches
// of the material's moonMiningAmount (100). A reactor accepts reactionGroup1/2.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// CategoryReaction is invCategories 24.
const CategoryReaction = 24

// ReactionLine is one input or output of a reaction, in batches.
type ReactionLine struct {
	TypeID  int64
	Batches int64
}

// ReactorReaction returns the reaction item inside a reactor (itemID and its
// reaction typeID). ok=false when the reactor holds none.
func ReactorReaction(ctx context.Context, db *sql.DB, reactorID int64) (itemID, typeID int64, ok bool, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.locationID = ? AND g.categoryID = ?
		 ORDER BY e.itemID LIMIT 1`, reactorID, CategoryReaction).Scan(&itemID, &typeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("ReactorReaction %d: %w", reactorID, err)
	}
	return itemID, typeID, true, nil
}

// ReactionRecipe reads a reaction's inputs and outputs.
func ReactionRecipe(ctx context.Context, db *sql.DB, reactionTypeID int64) (inputs, outputs []ReactionLine, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT input, typeID, quantity FROM invtypereactions
		 WHERE reactionTypeID = ? ORDER BY input DESC, typeID`, reactionTypeID)
	if err != nil {
		return nil, nil, fmt.Errorf("ReactionRecipe %d: %w", reactionTypeID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var in int
		var l ReactionLine
		if err := rows.Scan(&in, &l.TypeID, &l.Batches); err != nil {
			return nil, nil, err
		}
		if in != 0 {
			inputs = append(inputs, l)
		} else {
			outputs = append(outputs, l)
		}
	}
	return inputs, outputs, rows.Err()
}

// ReactorAccepts reports whether a reactor type runs a reaction type: the
// reaction's group must be one of the reactor's reactionGroup1/2. The client
// does not check this; a reactor holding the wrong kind simply does nothing.
func ReactorAccepts(ctx context.Context, db *sql.DB, reactorTypeID, reactionTypeID int64) (bool, error) {
	var group int64
	if err := db.QueryRowContext(ctx,
		`SELECT groupID FROM invTypes WHERE typeID = ?`, reactionTypeID).Scan(&group); err != nil {
		return false, fmt.Errorf("ReactorAccepts reaction %d: %w", reactionTypeID, err)
	}
	for _, name := range []string{"reactionGroup1", "reactionGroup2"} {
		if v, ok, err := GetTypeAttributeFloat(ctx, db, reactorTypeID, name); err == nil && ok && int64(v) == group {
			return true, nil
		}
	}
	return false, nil
}

// SiloUnits is the silo gauge the Production tab draws: units of the silo's material held and
// the capacity in units. The client reads both as units; m3 draws a wrong bar.
func SiloUnits(ctx context.Context, db *sql.DB, siloID, siloTypeID, materialTypeID int64) (used, capacity int64, err error) {
	usedM3, capM3, err := SiloUsedCapacity(ctx, db, siloID, siloTypeID)
	if err != nil {
		return 0, 0, err
	}
	var vol float64
	if materialTypeID != 0 {
		_ = db.QueryRowContext(ctx, `SELECT COALESCE(volume, 0) FROM invTypes WHERE typeID = ?`, materialTypeID).Scan(&vol)
	}
	if vol <= 0 {
		return int64(usedM3), int64(capM3), nil
	}
	return int64(usedM3/vol + 0.5), int64(capM3 / vol), nil
}

// SiloQuantity is how many units of typeID a silo holds.
func SiloQuantity(ctx context.Context, db *sql.DB, siloID, typeID int64) (int64, error) {
	return bayQuantity(ctx, db, siloID, typeID)
}

// ReactionUnits converts a recipe line to units: batches × the material's
// moonMiningAmount. Pure.
func ReactionUnits(batches, amountPerBatch int64) int64 {
	if batches <= 0 || amountPerBatch <= 0 {
		return 0
	}
	return batches * amountPerBatch
}
