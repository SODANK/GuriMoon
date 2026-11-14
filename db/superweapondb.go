package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Superweapon (doomsday) support queries for three rules: one superweapon online per ship
// (maxGroupOnline), a fuel bill per shot (consumptionType/consumptionQuantity), and the CPU
// self-bonus that makes it fittable (dogma.SuperWeaponCPUUsage, applied in GetShipCPUPowerLoad).

// CountOnlineModulesInGroup counts the online modules of one invGroup fitted to a ship,
// ignoring excludeItemID (the module being onlined, which may carry a stale isOnline row).
// entity_attributes attr 2 is the persisted isOnline flag and flags 11-34 the slot range, the
// same pair GetShipCPUPowerLoad sums over.
func CountOnlineModulesInGroup(ctx context.Context, db *sql.DB, shipID, groupID, excludeItemID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		FROM entity e
		JOIN entity_attributes ea ON ea.itemID = e.itemID
		 AND ea.attributeID = 2 AND ea.valueInt = 1
		JOIN invTypes t ON t.typeID = e.typeID
		WHERE e.locationID = ? AND e.flag BETWEEN 11 AND 34
		  AND t.groupID = ? AND e.itemID != ?`, shipID, groupID, excludeItemID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountOnlineModulesInGroup: %w", err)
	}
	return n, nil
}

// CountCargoItemsByType sums every cargo stack (flag 5) of typeID inside shipID. A fuel bill
// needs the total, since FindCargoItemByType answers the first stack only.
func CountCargoItemsByType(ctx context.Context, db *sql.DB, typeID, ownerID, shipID int64) (int64, error) {
	var total sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT SUM(quantity) FROM entity
		  WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = 5`,
		typeID, ownerID, shipID).Scan(&total)
	if err != nil {
		return 0, fmt.Errorf("CountCargoItemsByType: %w", err)
	}
	return total.Int64, nil
}

// ConsumeCargoFuel takes qty units of typeID out of shipID's cargo hold, draining stacks in
// itemID order, and returns the per-entity changes for the OnItemChange push. All-or-nothing:
// a short hold returns ok=false. One transaction locks the stacks FOR UPDATE (no JOIN, so only
// entity rows lock), so racing activations cannot both see enough fuel.
func ConsumeCargoFuel(ctx context.Context, db *sql.DB, typeID, ownerID, shipID, qty int64) (ok bool, deltas []ItemDelta, err error) {
	if qty <= 0 {
		return true, nil, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, nil, fmt.Errorf("ConsumeCargoFuel begin: %w", err)
	}
	defer func() {
		if err != nil || !ok {
			_ = tx.Rollback()
		}
	}()

	rows, qErr := tx.QueryContext(ctx,
		`SELECT itemID, quantity FROM entity
		  WHERE typeID = ? AND ownerID = ? AND locationID = ? AND flag = 5
		  ORDER BY itemID FOR UPDATE`, typeID, ownerID, shipID)
	if qErr != nil {
		err = fmt.Errorf("ConsumeCargoFuel query: %w", qErr)
		return false, nil, err
	}
	type stack struct{ itemID, quantity int64 }
	var stacks []stack
	var have int64
	for rows.Next() {
		var s stack
		if sErr := rows.Scan(&s.itemID, &s.quantity); sErr != nil {
			rows.Close()
			err = fmt.Errorf("ConsumeCargoFuel scan: %w", sErr)
			return false, nil, err
		}
		stacks = append(stacks, s)
		have += s.quantity
	}
	rows.Close()
	if rErr := rows.Err(); rErr != nil {
		err = fmt.Errorf("ConsumeCargoFuel rows: %w", rErr)
		return false, nil, err
	}
	if have < qty {
		return false, nil, nil
	}

	remaining := qty
	for _, s := range stacks {
		if remaining <= 0 {
			break
		}
		// Snapshot before mutating: a deleted stack cannot be re-read, and the client needs the old
		// quantity to reconcile a partial take.
		er, _ := getEntityRowTx(ctx, tx, s.itemID)
		if s.quantity <= remaining {
			if _, dErr := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.itemID); dErr != nil {
				err = fmt.Errorf("ConsumeCargoFuel delete %d: %w", s.itemID, dErr)
				return false, nil, err
			}
			remaining -= s.quantity
			if er != nil {
				deltas = append(deltas, ItemDelta{Kind: ItemDeltaRemoved, Row: er})
			}
			continue
		}
		if _, uErr := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, remaining, s.itemID); uErr != nil {
			err = fmt.Errorf("ConsumeCargoFuel update %d: %w", s.itemID, uErr)
			return false, nil, err
		}
		if er != nil {
			oldQty := er.Quantity
			newER := *er
			newER.Quantity = oldQty - remaining
			deltas = append(deltas, ItemDelta{Kind: ItemDeltaQty, Row: &newER, OldQty: oldQty})
		}
		remaining = 0
	}
	if cErr := tx.Commit(); cErr != nil {
		err = fmt.Errorf("ConsumeCargoFuel commit: %w", cErr)
		return false, nil, err
	}
	return true, deltas, nil
}

// getEntityRowTx is GetEntityRow against an open transaction, so a take's snapshot is read
// under the lock that took it.
func getEntityRowTx(ctx context.Context, tx DBTX, itemID int64) (*EntityRow, error) {
	var er EntityRow
	err := tx.QueryRowContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       t.groupID, g.categoryID, e.singleton, e.quantity
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ?`, itemID,
	).Scan(&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID, &er.Flag,
		&er.GroupID, &er.CategoryID, &er.Singleton, &er.Quantity)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("getEntityRowTx: %w", err)
	}
	return &er, nil
}
