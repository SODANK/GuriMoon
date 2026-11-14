package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ---- Implants ----
//
// A plugged-in implant is an entity row with locationID = characterID and flag 89
// (flagImplant). The slot comes from the implantness attribute (331), one implant per
// slot. Unplugging destroys the implant. See docs/IMPLANTS.md#core.

const flagImplant = 89

// GetCharacterImplants returns the entity rows for every implant plugged into charID,
// joined with invTypes/invGroups for groupID/categoryID.
func GetCharacterImplants(ctx context.Context, db *sql.DB, charID int64) ([]EntityRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       t.groupID, g.categoryID, e.singleton, e.quantity
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag = ?`, charID, flagImplant)
	if err != nil {
		return nil, fmt.Errorf("GetCharacterImplants: %w", err)
	}
	defer rows.Close()
	var out []EntityRow
	for rows.Next() {
		var er EntityRow
		if err := rows.Scan(&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID, &er.Flag,
			&er.GroupID, &er.CategoryID, &er.Singleton, &er.Quantity); err != nil {
			return nil, fmt.Errorf("GetCharacterImplants scan: %w", err)
		}
		out = append(out, er)
	}
	return out, rows.Err()
}

// PlugInImplant moves implant itemID from the character's hangar into the character
// (flag 89), validating ownership, location, category and required skills. An implant
// in the same implantness slot is destroyed, and a stack is split (one plugged, the
// rest stay). Returns the destroyed implant's row (nil if none) and the plugged itemID.
func PlugInImplant(ctx context.Context, db *sql.DB, charID, itemID, stationID int64) (destroyed *EntityRow, pluggedID int64, err error) {
	er, err := GetEntityRow(ctx, db, itemID)
	if err != nil {
		return nil, 0, err
	}
	if er == nil {
		return nil, 0, errors.New("implant not found")
	}
	if er.OwnerID != charID {
		return nil, 0, errors.New("you do not own that item")
	}
	if er.Flag == flagImplant && er.LocationID == charID {
		return nil, 0, errors.New("implant is already plugged in")
	}
	if er.LocationID != stationID || er.Flag != 4 {
		return nil, 0, errors.New("the implant must be in your station hangar")
	}
	if er.CategoryID != 20 {
		return nil, 0, errors.New("that item is not an implant")
	}

	// Required skills: requiredSkill1..3 are attrs 182/183/184, levels 277/278/279.
	reqs, err := GetRequiredSkills(ctx, db, er.TypeID)
	if err != nil {
		return nil, 0, fmt.Errorf("PlugInImplant required skills: %w", err)
	}
	for _, req := range reqs {
		have, _ := GetCharacterSkillLevel(ctx, db, charID, req.SkillID)
		if have < req.Level {
			name, _ := GetTypeName(ctx, db, req.SkillID)
			if name == "" {
				name = fmt.Sprintf("skill %d", req.SkillID)
			}
			return nil, 0, fmt.Errorf("you cannot use this implant: it requires %s to level %d", name, req.Level)
		}
	}

	// Slot conflict: destroy any plugged implant with the same implantness.
	typeAttrs, err := GetTypeAttributes(ctx, db, er.TypeID)
	if err != nil {
		return nil, 0, fmt.Errorf("PlugInImplant type attrs: %w", err)
	}
	slot := int64(typeAttrs[331]) // implantness
	if slot > 0 {
		existing, err := GetCharacterImplants(ctx, db, charID)
		if err != nil {
			return nil, 0, err
		}
		for _, ex := range existing {
			exAttrs, aerr := GetTypeAttributes(ctx, db, ex.TypeID)
			if aerr != nil {
				continue
			}
			if int64(exAttrs[331]) == slot {
				if derr := DeleteEntityDeep(ctx, db, ex.ItemID); derr != nil {
					return nil, 0, fmt.Errorf("PlugInImplant destroy old: %w", derr)
				}
				exCopy := ex
				destroyed = &exCopy
				break
			}
		}
	}

	defer InvalidateImplants(charID)
	defer InvalidateGangSource(charID)
	pluggedID = itemID
	if er.Quantity > 1 {
		// Split the stack: leave quantity-1 in the hangar, plug a fresh singleton row.
		if _, err := db.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - 1 WHERE itemID = ?`, itemID); err != nil {
			return destroyed, 0, fmt.Errorf("PlugInImplant split: %w", err)
		}
		res, err := db.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES ('Implant', ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, NULL)`,
			er.TypeID, charID, charID, flagImplant)
		if err != nil {
			return destroyed, 0, fmt.Errorf("PlugInImplant insert: %w", err)
		}
		pluggedID, err = res.LastInsertId()
		if err != nil {
			return destroyed, 0, fmt.Errorf("PlugInImplant insert id: %w", err)
		}
	} else {
		if _, err := db.ExecContext(ctx, `
			UPDATE entity SET locationID = ?, flag = ?, singleton = 1, quantity = 1
			WHERE itemID = ?`, charID, flagImplant, itemID); err != nil {
			return destroyed, 0, fmt.Errorf("PlugInImplant move: %w", err)
		}
	}
	return destroyed, pluggedID, nil
}

// UnplugImplant removes implant itemID from charID and destroys it.
func UnplugImplant(ctx context.Context, db *sql.DB, charID, itemID int64) (*EntityRow, error) {
	er, err := GetEntityRow(ctx, db, itemID)
	if err != nil {
		return nil, err
	}
	if er == nil {
		return nil, errors.New("implant not found")
	}
	if er.LocationID != charID || er.Flag != flagImplant {
		return nil, errors.New("that implant is not plugged into you")
	}
	if err := DeleteEntityDeep(ctx, db, itemID); err != nil {
		return nil, err
	}
	InvalidateImplants(charID)
	InvalidateGangSource(charID)
	return er, nil
}

// DestroyCharacterImplants is the pod-death rule: every implant plugged into charID is
// destroyed. It returns the removed rows for the OnItemChange pushes and the kill report,
// and is shared with the rolebots respawn.
func DestroyCharacterImplants(ctx context.Context, db *sql.DB, charID int64) ([]EntityRow, error) {
	rows, err := GetCharacterImplants(ctx, db, charID)
	if err != nil {
		return nil, err
	}
	defer InvalidateImplants(charID)
	defer InvalidateGangSource(charID)
	for _, r := range rows {
		if err := DeleteEntityDeep(ctx, db, r.ItemID); err != nil {
			return nil, fmt.Errorf("DestroyCharacterImplants %d: %w", r.ItemID, err)
		}
	}
	return rows, nil
}

// WithTrainingRetimed runs fn, which changes the pilot's attributes (plug, unplug, pod
// death), with the running skill stopped before and restarted after, so SP banked so far
// is priced at the old rate and the remaining end time at the new one. A failure inside
// fn still restarts the skill. Returns the restarted skill item (0 if none) and its end.
func WithTrainingRetimed(ctx context.Context, db *sql.DB, charID int64, fn func() error) (restarted int64, end uint64, err error) {
	stopped, serr := StopTrainingSkill(ctx, db, charID, 0)
	if serr != nil {
		return 0, 0, fmt.Errorf("WithTrainingRetimed stop: %w", serr)
	}
	err = fn()
	if stopped != 0 {
		InvalidateImplants(charID)
		var rerr error
		end, rerr = StartTrainingSkill(ctx, db, charID, stopped)
		if rerr != nil && err == nil {
			err = fmt.Errorf("WithTrainingRetimed restart: %w", rerr)
		}
		restarted = stopped
	}
	return restarted, end, err
}
