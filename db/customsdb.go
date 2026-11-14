package db

import (
	"context"
	"database/sql"
	"fmt"
)

// GetSystemFactionID returns the factionID for a solar system, or 0 if null.
func GetSystemFactionID(ctx context.Context, db *sql.DB, sysID int64) (int64, error) {
	var factionID sql.NullInt64
	err := db.QueryRowContext(ctx,
		`SELECT factionID FROM mapSolarSystems WHERE solarSystemID = ?`, sysID,
	).Scan(&factionID)
	if err != nil {
		return 0, fmt.Errorf("GetSystemFactionID sysID=%d: %w", sysID, err)
	}
	return factionID.Int64, nil
}

// CargoItem is a typeID + quantity pair from a ship's cargo hold.
type CargoItem struct {
	ItemID   int64
	TypeID   int64
	Quantity int64
}

// GetCargoTypeIDs returns typeID and quantity of all items in a ship's cargo
// hold (flag 5) owned by charID.
func GetCargoTypeIDs(ctx context.Context, db *sql.DB, charID, shipID int64) ([]CargoItem, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, typeID, COALESCE(quantity, 1)
		  FROM entity
		 WHERE ownerID = ? AND locationID = ? AND flag = 5`,
		charID, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetCargoTypeIDs: %w", err)
	}
	defer rows.Close()
	var out []CargoItem
	for rows.Next() {
		var c CargoItem
		if err := rows.Scan(&c.ItemID, &c.TypeID, &c.Quantity); err != nil {
			return nil, fmt.Errorf("GetCargoTypeIDs scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetShipCargoByShip returns typeID and quantity of everything in a ship's
// cargo hold (flag 5), whoever owns the rows. A cargo scanner reads it; the
// owner filter of GetCargoTypeIDs would hide a corporation-owned kit's cargo.
func GetShipCargoByShip(ctx context.Context, db *sql.DB, shipID int64) ([]CargoItem, error) {
	if shipID <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, typeID, COALESCE(quantity, 1)
		  FROM entity
		 WHERE locationID = ? AND flag = 5`, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetShipCargoByShip: %w", err)
	}
	defer rows.Close()
	var out []CargoItem
	for rows.Next() {
		var c CargoItem
		if err := rows.Scan(&c.ItemID, &c.TypeID, &c.Quantity); err != nil {
			return nil, fmt.Errorf("GetShipCargoByShip scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetContrabandForFaction returns typeID to ContrabandEntry for a faction, for
// customs cargo checks.
func GetContrabandForFaction(ctx context.Context, db *sql.DB, factionID int64) (map[int64]ContrabandEntry, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.factionID, COALESCE(f.factionName, ''), c.typeID, COALESCE(t.typeName, ''),
		       COALESCE(c.standingLoss, 0), COALESCE(c.confiscateMinSec, 0),
		       COALESCE(c.fineByValue, 0), COALESCE(c.attackMinSec, 0)
		  FROM invcontrabandtypes c
		  LEFT JOIN chrFactions f ON f.factionID = c.factionID
		  LEFT JOIN invTypes t ON t.typeID = c.typeID
		 WHERE c.factionID = ?`, factionID)
	if err != nil {
		return nil, fmt.Errorf("GetContrabandForFaction: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]ContrabandEntry)
	for rows.Next() {
		var e ContrabandEntry
		if err := rows.Scan(&e.FactionID, &e.FactionName, &e.TypeID, &e.TypeName,
			&e.StandingLoss, &e.ConfiscateMinSec, &e.FineByValue, &e.AttackMinSec); err != nil {
			return nil, fmt.Errorf("GetContrabandForFaction scan: %w", err)
		}
		out[e.TypeID] = e
	}
	return out, rows.Err()
}

// GetItemBasePrice returns the basePrice from invTypes for a given typeID.
func GetItemBasePrice(ctx context.Context, db *sql.DB, typeID int64) (float64, error) {
	var price float64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(basePrice, 0) FROM invTypes WHERE typeID = ?`, typeID,
	).Scan(&price)
	if err != nil {
		return 0, fmt.Errorf("GetItemBasePrice typeID=%d: %w", typeID, err)
	}
	return price, nil
}

// DeleteCargoItems removes specific items from the entity table by itemID.
func DeleteCargoItems(ctx context.Context, db *sql.DB, itemIDs []int64) error {
	if len(itemIDs) == 0 {
		return nil
	}
	for _, id := range itemIDs {
		if _, err := db.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, id); err != nil {
			return fmt.Errorf("DeleteCargoItems itemID=%d: %w", id, err)
		}
	}
	return nil
}

// DeductWalletFine subtracts amount from a character's balance and inserts a
// journal entry. Returns the new balance.
func DeductWalletFine(ctx context.Context, db *sql.DB, charID int64, amount float64, refTypeID int, reason string) (float64, error) {
	// Deduct from wallet.
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET balance = balance - ? WHERE characterID = ?`,
		amount, charID)
	if err != nil {
		return 0, fmt.Errorf("DeductWalletFine deduct: %w", err)
	}

	// Read new balance.
	var newBal float64
	err = db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID,
	).Scan(&newBal)
	if err != nil {
		return 0, fmt.Errorf("DeductWalletFine balance: %w", err)
	}

	// Record journal entry.
	if err := InsertJournalEntry(ctx, db, charID, refTypeID, 0, charID, -amount, newBal, reason); err != nil {
		return newBal, fmt.Errorf("DeductWalletFine journal: %w", err)
	}
	return newBal, nil
}
