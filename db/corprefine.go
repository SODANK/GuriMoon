package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// RefineCorpStack reprocesses one stack a corporation holds in an office hangar division at a
// net yield the caller has worked out for the member doing it. The minerals stay corporation
// property in the same division; ore is consumed in whole portions only. It exists because the
// reprocessing service is character-bound and would put the output in the member's hangar.
func RefineCorpStack(ctx context.Context, db *sql.DB, corpID, officeID, flag, itemID int64, yield float64) (out map[int64]int64, consumed int64, err error) {
	if !IsCorpHangarFlag(flag) || yield <= 0 {
		return nil, 0, fmt.Errorf("RefineCorpStack: bad flag %d or yield %.3f", flag, yield)
	}
	var typeID, qty int64
	if err := db.QueryRowContext(ctx, `
		SELECT typeID, quantity FROM entity
		 WHERE itemID = ? AND ownerID = ? AND locationID = ? AND flag = ? AND singleton = 0`,
		itemID, corpID, officeID, flag).Scan(&typeID, &qty); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, 0, fmt.Errorf("RefineCorpStack: stack %d is not in the corporation's hangar", itemID)
		}
		return nil, 0, fmt.Errorf("RefineCorpStack: %w", err)
	}
	mats, err := GetTypeMaterials(ctx, db, typeID)
	if err != nil {
		return nil, 0, err
	}
	portion, err := GetTypePortionSize(ctx, db, typeID)
	if err != nil {
		return nil, 0, err
	}
	out, consumed = RefineOutput(qty, portion, mats, yield)
	if consumed <= 0 {
		return nil, 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	if err := ReduceEntityQuantity(ctx, tx, itemID, consumed); err != nil {
		return nil, 0, err
	}
	for mat, n := range out {
		stackID, have, err := FindStackAt(ctx, tx, mat, corpID, officeID, flag)
		if err != nil {
			return nil, 0, err
		}
		if stackID != 0 {
			if _, err := tx.ExecContext(ctx,
				`UPDATE entity SET quantity = ? WHERE itemID = ?`, have+n, stackID); err != nil {
				return nil, 0, fmt.Errorf("RefineCorpStack merge: %w", err)
			}
			continue
		}
		newID, err := CreateItem(ctx, tx, mat, corpID, officeID, int(n))
		if err != nil {
			return nil, 0, err
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET flag = ?, quantity = ?, singleton = 0 WHERE itemID = ?`, flag, n, newID); err != nil {
			return nil, 0, fmt.Errorf("RefineCorpStack flag: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return out, consumed, nil
}
