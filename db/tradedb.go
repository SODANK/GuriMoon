package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
)

// flagHangar is the inventory flag for items sitting loose in a hangar.
const flagHangar = 4

// TradeItem is one escrowed item as the trade window renders it.
type TradeItem struct {
	ItemID     int64
	TypeID     int64
	OwnerID    int64
	LocationID int64
	Quantity   int64
	Flag       int64
	Singleton  int64
	GroupID    int64
	CategoryID int64
}

// CreateTradeContainer inserts the container entity a trade session escrows into and
// returns its item ID. It is owned by nobody, so neither trader sees it in a hangar.
func CreateTradeContainer(ctx context.Context, db *sql.DB, stationID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Trade Session', ?, 0, ?, ?, 0, 1, 1, 0, 0, 0, 'trade')`,
		TypeCargoContainer, stationID, flagHangar)
	if err != nil {
		return 0, fmt.Errorf("CreateTradeContainer: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateTradeContainer LastInsertId: %w", err)
	}
	return id, nil
}

// GetTradeItems lists everything currently escrowed in a trade container.
func GetTradeItems(ctx context.Context, db *sql.DB, containerID int64) ([]TradeItem, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.quantity, e.flag, e.singleton,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		FROM entity e
		LEFT JOIN invTypes  t ON t.typeID  = e.typeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ?`, containerID)
	if err != nil {
		return nil, fmt.Errorf("GetTradeItems: %w", err)
	}
	defer rows.Close()

	var out []TradeItem
	for rows.Next() {
		var it TradeItem
		if err := rows.Scan(&it.ItemID, &it.TypeID, &it.OwnerID, &it.LocationID,
			&it.Quantity, &it.Flag, &it.Singleton, &it.GroupID, &it.CategoryID); err != nil {
			return nil, fmt.Errorf("GetTradeItems scan: %w", err)
		}
		out = append(out, it)
	}
	return out, rows.Err()
}

// EscrowTradeItem moves an item (or part of a stack) from the character's hangar into a
// trade container. Splitting a stack creates a new row so the remainder stays put.
func EscrowTradeItem(ctx context.Context, db *sql.DB, charID, stationID, containerID, itemID, quantity int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("EscrowTradeItem begin: %w", err)
	}
	defer tx.Rollback()

	var (
		ownerID, locationID, have, typeID, flag, singleton int64
		itemName                                           string
	)
	err = tx.QueryRowContext(ctx, `
		SELECT ownerID, locationID, quantity, typeID, flag, singleton, itemName
		FROM entity WHERE itemID = ? FOR UPDATE`, itemID,
	).Scan(&ownerID, &locationID, &have, &typeID, &flag, &singleton, &itemName)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("EscrowTradeItem: item %d not found", itemID)
	}
	if err != nil {
		return fmt.Errorf("EscrowTradeItem lookup: %w", err)
	}
	if ownerID != charID {
		return fmt.Errorf("EscrowTradeItem: item %d not owned by char %d", itemID, charID)
	}
	if locationID != stationID && locationID != containerID {
		return fmt.Errorf("EscrowTradeItem: item %d is not in the trade station", itemID)
	}
	if quantity <= 0 || quantity > have {
		quantity = have
	}

	if quantity == have {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ?`,
			containerID, flagHangar, itemID); err != nil {
			return fmt.Errorf("EscrowTradeItem move: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
			quantity, itemID); err != nil {
			return fmt.Errorf("EscrowTradeItem split source: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?, 0, 0, 0, NULL)`,
			itemName, typeID, charID, containerID, flagHangar, singleton, quantity); err != nil {
			return fmt.Errorf("EscrowTradeItem split dest: %w", err)
		}
	}

	return tx.Commit()
}

// ReturnTradeItems moves everything out of a trade container back to its original
// owner's hangar and deletes the container in one transaction, for an aborted trade or a
// disconnect.
func ReturnTradeItems(ctx context.Context, db *sql.DB, containerID, stationID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ReturnTradeItems begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = ? WHERE locationID = ?`,
		stationID, flagHangar, containerID); err != nil {
		return fmt.Errorf("ReturnTradeItems: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
		return fmt.Errorf("ReturnTradeItems delete container: %w", err)
	}
	return tx.Commit()
}

// ReturnOrphanTradeContainers returns the contents of trade-escrow containers left over
// from a previous run (customInfo = 'trade', ownerID = 0) to the station hangar. Every
// container open at boot is an orphan because the trade registry starts empty.
func ReturnOrphanTradeContainers(ctx context.Context, db *sql.DB) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, locationID FROM entity
		WHERE typeID = ? AND customInfo = 'trade'`,
		TypeCargoContainer)
	if err != nil {
		return 0, fmt.Errorf("ReturnOrphanTradeContainers query: %w", err)
	}
	defer rows.Close()
	type orphan struct{ id, station int64 }
	var orphans []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.station); err == nil {
			orphans = append(orphans, o)
		}
	}
	for _, o := range orphans {
		if err := ReturnTradeItems(ctx, db, o.id, o.station); err != nil {
			log.Printf("[db] ReturnOrphanTradeContainers container %d: %v", o.id, err)
		}
	}
	return len(orphans), nil
}

// SettleTrade swaps escrowed items to the opposite trader's hangar, moves the offered
// ISK and deletes the container. It fails without side effects if either trader cannot
// cover their offer.
func SettleTrade(ctx context.Context, db *sql.DB, containerID, stationID, charA, charB int64, moneyA, moneyB float64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("SettleTrade begin: %w", err)
	}
	defer tx.Rollback()

	// Lock both wallets in a stable order so two simultaneous trades between the same pair
	// cannot deadlock.
	first, second := charA, charB
	if first > second {
		first, second = second, first
	}
	balances := make(map[int64]float64, 2)
	for _, id := range []int64{first, second} {
		var bal float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`, id,
		).Scan(&bal); err != nil {
			return fmt.Errorf("SettleTrade balance %d: %w", id, err)
		}
		balances[id] = bal
	}
	if balances[charA] < moneyA {
		return fmt.Errorf("SettleTrade: char %d cannot cover %.2f ISK", charA, moneyA)
	}
	if balances[charB] < moneyB {
		return fmt.Errorf("SettleTrade: char %d cannot cover %.2f ISK", charB, moneyB)
	}

	// What each side put in, read before the move so the contents of each item (a ship's
	// rack and hold, a container's load) can follow it to its new owner.
	offered := map[int64][]int64{}
	for _, owner := range []int64{charA, charB} {
		rows, err := tx.QueryContext(ctx,
			`SELECT itemID FROM entity WHERE locationID = ? AND ownerID = ?`, containerID, owner)
		if err != nil {
			return fmt.Errorf("SettleTrade list items %d: %w", owner, err)
		}
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return fmt.Errorf("SettleTrade list items %d: %w", owner, err)
			}
			offered[owner] = append(offered[owner], id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return fmt.Errorf("SettleTrade list items %d: %w", owner, err)
		}
	}

	// Items each side put in go to the other side's hangar.
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
		WHERE locationID = ? AND ownerID = ?`,
		charB, stationID, flagHangar, containerID, charA); err != nil {
		return fmt.Errorf("SettleTrade items a→b: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
		WHERE locationID = ? AND ownerID = ?`,
		charA, stationID, flagHangar, containerID, charB); err != nil {
		return fmt.Errorf("SettleTrade items b→a: %w", err)
	}
	if err := reownNested(ctx, tx, offered[charA], charB); err != nil {
		return fmt.Errorf("SettleTrade contents a→b: %w", err)
	}
	if err := reownNested(ctx, tx, offered[charB], charA); err != nil {
		return fmt.Errorf("SettleTrade contents b→a: %w", err)
	}
	// Anything left had neither trader as owner; return it to the station instead of
	// deleting it with the container.
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = ? WHERE locationID = ?`,
		stationID, flagHangar, containerID); err != nil {
		return fmt.Errorf("SettleTrade items residual: %w", err)
	}

	net := moneyB - moneyA // what charA gains
	if net != 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`, net, charA); err != nil {
			return fmt.Errorf("SettleTrade credit a: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = balance - ? WHERE characterID = ?`, net, charB); err != nil {
			return fmt.Errorf("SettleTrade credit b: %w", err)
		}
		tradeJournal(ctx, tx, charA, charB, net, balances[charA]+net)
		tradeJournal(ctx, tx, charB, charA, -net, balances[charB]-net)
	}

	if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
		return fmt.Errorf("SettleTrade delete container: %w", err)
	}
	return tx.Commit()
}

// tradeJournal records a wallet line for a completed trade (best effort).
func tradeJournal(ctx context.Context, tx *sql.Tx, charID, otherID int64, amount, balance float64) {
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 2, ?, ?, NULL, 0, ?, ?, 'Player Trade', 1000)`,
		charID, win32TimeNow(), charID, otherID, amount, balance); err != nil {
		log.Printf("[db] tradeJournal: best-effort write failed: %v", err)
	}
}

// GetCharStationID returns the station a character is docked in, or 0 when in space.
func GetCharStationID(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	var stationID int64
	err := db.QueryRowContext(ctx,
		`SELECT stationID FROM character_ WHERE characterID = ?`, charID).Scan(&stationID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("GetCharStationID: character %d not found", charID)
	}
	if err != nil {
		return 0, fmt.Errorf("GetCharStationID: %w", err)
	}
	return stationID, nil
}
