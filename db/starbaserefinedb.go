// Package db: the POS Refining Array (group 311).
//
// The client has two menu entries: Access Refinery opens the array's storage at flagCargo
// (5, the Processing Area), and Run Refining Process sends a bare
// InventoryBound.RunRefiningProcess() and ignores the reply. A run refines every whole
// portion at the array's refiningYieldMultiplier (attr 717) and takes its
// operationalDuration (attr 719). The ore is taken at the start and the minerals are
// delivered at the end to the ore's owner, from the starbaseRefineJobs table so a restart
// loses nothing. See docs/starbase-pos-investigation.md#22-refining-array.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sync"
)

// ErrRefineryBusy is returned when a run is already in progress.
var ErrRefineryBusy = errors.New("the refinery is already processing")

// ErrNothingToRefine is returned when the processing area holds no whole portion of
// anything refinable.
var ErrNothingToRefine = errors.New("there is nothing refinable in the processing area")

// RefineLine is one mineral a run will deliver to one owner.
type RefineLine struct {
	OwnerID, TypeID, Quantity int64
}

// RefineChange is a processing-area row a run consumed from.
type RefineChange struct {
	ItemID, OldQty, NewQty int64 // NewQty 0 = deleted
	TypeID, OwnerID        int64 // for rebuilding a deleted row's notification
}

// RefineOutput is the arithmetic of one stack: whole portions only, each material
// floor(runs * quantity * yield). Pure.
func RefineOutput(qty, portion int64, mats []TypeMaterial, yield float64) (out map[int64]int64, consumed int64) {
	if portion <= 0 {
		portion = 1
	}
	runs := qty / portion
	if runs <= 0 || len(mats) == 0 {
		return nil, 0
	}
	out = map[int64]int64{}
	for _, m := range mats {
		if n := int64(math.Floor(float64(runs) * float64(m.Quantity) * yield)); n > 0 {
			out[m.MaterialTypeID] += n
		}
	}
	return out, runs * portion
}

var (
	refineSchemaOnce sync.Once
	refineSchemaErr  error
)

func ensureRefineTables(ctx context.Context, db *sql.DB) error {
	refineSchemaOnce.Do(func() {
		for _, ddl := range []string{`
			CREATE TABLE IF NOT EXISTS starbaseRefineJobs (
			  arrayID  INT UNSIGNED NOT NULL,
			  startedBy INT UNSIGNED NOT NULL DEFAULT 0,
			  finishAt BIGINT(20)   NOT NULL,
			  PRIMARY KEY (arrayID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`, `
			CREATE TABLE IF NOT EXISTS starbaseRefineOutput (
			  arrayID  INT UNSIGNED NOT NULL,
			  ownerID  INT UNSIGNED NOT NULL,
			  typeID   INT UNSIGNED NOT NULL,
			  quantity BIGINT       NOT NULL,
			  PRIMARY KEY (arrayID, ownerID, typeID)
			) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`,
		} {
			if _, err := db.ExecContext(ctx, ddl); err != nil {
				refineSchemaErr = fmt.Errorf("ensureRefineTables: %w", err)
				return
			}
		}
	})
	return refineSchemaErr
}

// StartRefining takes every whole portion out of arrayID's processing area and books the
// minerals for delivery durationMs later. Returns the changed rows (for OnItemChange)
// and the finish time.
func StartRefining(ctx context.Context, db *sql.DB, arrayID, startedBy int64, yield, durationMs float64) ([]RefineChange, uint64, error) {
	if err := ensureRefineTables(ctx, db); err != nil {
		return nil, 0, err
	}
	type stack struct {
		itemID, typeID, ownerID, qty, singleton int64
	}
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, typeID, ownerID, quantity, singleton FROM entity
		 WHERE locationID = ? AND flag = ?`, arrayID, FlagRefineryInput)
	if err != nil {
		return nil, 0, fmt.Errorf("StartRefining list: %w", err)
	}
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.itemID, &s.typeID, &s.ownerID, &s.qty, &s.singleton); err != nil {
			rows.Close()
			return nil, 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()

	// Materials are read before the transaction (they are static data).
	type plan struct {
		s        stack
		out      map[int64]int64
		consumed int64
	}
	var plans []plan
	for _, s := range stacks {
		if s.singleton != 0 {
			continue // assembled items are not refined, as at a station
		}
		mats, err := GetTypeMaterials(ctx, db, s.typeID)
		if err != nil || len(mats) == 0 {
			continue
		}
		portion, _ := GetTypePortionSize(ctx, db, s.typeID)
		out, consumed := RefineOutput(s.qty, portion, mats, yield)
		if consumed > 0 && len(out) > 0 {
			plans = append(plans, plan{s, out, consumed})
		}
	}
	if len(plans) == 0 {
		return nil, 0, ErrNothingToRefine
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, 0, err
	}
	defer tx.Rollback()
	finishAt := win32TimeNow() + uint64(durationMs*10000)
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO starbaseRefineJobs (arrayID, startedBy, finishAt) VALUES (?, ?, ?)`,
		arrayID, startedBy, finishAt); err != nil {
		if isDuplicateKeyErr(err) {
			return nil, 0, ErrRefineryBusy
		}
		return nil, 0, fmt.Errorf("StartRefining job: %w", err)
	}
	var changes []RefineChange
	for _, p := range plans {
		// Re-read under lock: the stack may have moved since the listing.
		var qty int64
		if err := tx.QueryRowContext(ctx,
			`SELECT quantity FROM entity WHERE itemID = ? AND locationID = ? AND flag = ? FOR UPDATE`,
			p.s.itemID, arrayID, FlagRefineryInput).Scan(&qty); err != nil || qty < p.consumed {
			continue
		}
		left := qty - p.consumed
		if left == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, p.s.itemID); err != nil {
				return nil, 0, err
			}
		} else if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = ? WHERE itemID = ?`, left, p.s.itemID); err != nil {
			return nil, 0, err
		}
		changes = append(changes, RefineChange{ItemID: p.s.itemID, OldQty: qty, NewQty: left, TypeID: p.s.typeID, OwnerID: p.s.ownerID})
		for typeID, n := range p.out {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO starbaseRefineOutput (arrayID, ownerID, typeID, quantity) VALUES (?, ?, ?, ?)
				ON DUPLICATE KEY UPDATE quantity = quantity + VALUES(quantity)`,
				arrayID, p.s.ownerID, typeID, n); err != nil {
				return nil, 0, fmt.Errorf("StartRefining output: %w", err)
			}
		}
	}
	if len(changes) == 0 {
		return nil, 0, ErrNothingToRefine
	}
	if err := tx.Commit(); err != nil {
		return nil, 0, err
	}
	return changes, finishAt, nil
}

// DueRefineJobs lists arrays whose run has finished.
func DueRefineJobs(ctx context.Context, db *sql.DB, now uint64) ([]int64, error) {
	if err := ensureRefineTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT arrayID FROM starbaseRefineJobs WHERE finishAt <= ?`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RefineryBusyUntil is the finish time of a running job (0 = idle).
func RefineryBusyUntil(ctx context.Context, db *sql.DB, arrayID int64) uint64 {
	if ensureRefineTables(ctx, db) != nil {
		return 0
	}
	var at uint64
	_ = db.QueryRowContext(ctx, `SELECT finishAt FROM starbaseRefineJobs WHERE arrayID = ?`, arrayID).Scan(&at)
	return at
}

// FinishRefining delivers a finished run into the processing area, merging into the
// owner's loose stack of that mineral, and returns the itemIDs it created or grew. A job
// for an array that no longer exists is discarded.
func FinishRefining(ctx context.Context, db *sql.DB, arrayID int64) ([]int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var alive int
	_ = tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM entity WHERE itemID = ?`, arrayID).Scan(&alive)
	rows, err := tx.QueryContext(ctx,
		`SELECT ownerID, typeID, quantity FROM starbaseRefineOutput WHERE arrayID = ?`, arrayID)
	if err != nil {
		return nil, err
	}
	var lines []RefineLine
	for rows.Next() {
		var l RefineLine
		if err := rows.Scan(&l.OwnerID, &l.TypeID, &l.Quantity); err != nil {
			rows.Close()
			return nil, err
		}
		lines = append(lines, l)
	}
	rows.Close()
	var touched []int64
	if alive > 0 {
		for _, l := range lines {
			var id int64
			err := tx.QueryRowContext(ctx, `
				SELECT itemID FROM entity
				 WHERE locationID = ? AND flag = ? AND typeID = ? AND ownerID = ? AND singleton = 0
				 LIMIT 1`, arrayID, FlagRefineryInput, l.TypeID, l.OwnerID).Scan(&id)
			switch {
			case err == nil:
				if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, l.Quantity, id); err != nil {
					return nil, err
				}
			case errors.Is(err, sql.ErrNoRows):
				var name string
				_ = tx.QueryRowContext(ctx, `SELECT typeName FROM invTypes WHERE typeID = ?`, l.TypeID).Scan(&name)
				r, err := tx.ExecContext(ctx, `
					INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband,
					                    singleton, quantity, x, y, z, customInfo)
					VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL)`,
					name, l.TypeID, l.OwnerID, arrayID, FlagRefineryInput, l.Quantity)
				if err != nil {
					return nil, err
				}
				id, _ = r.LastInsertId()
			default:
				return nil, err
			}
			touched = append(touched, id)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM starbaseRefineOutput WHERE arrayID = ?`, arrayID); err != nil {
		return nil, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM starbaseRefineJobs WHERE arrayID = ?`, arrayID); err != nil {
		return nil, err
	}
	return touched, tx.Commit()
}
