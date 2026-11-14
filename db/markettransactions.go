package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/marshal"
)

// GetCharTransactionsBefore answers marketProxy.CharGetTransactions(lastID): the wallet's
// "load older transactions" page, rows strictly older than the transactionID the client holds,
// shaped like GetCharTransactions. before == 0 returns the newest page.
func GetCharTransactionsBefore(ctx context.Context, db *sql.DB, charID, before int64) (marshal.PyRep, error) {
	txnCols := []string{
		"transactionID", "transactionDateTime", "typeID", "quantity",
		"price", "transactionType", "clientID", "regionID", "stationID",
	}
	where := `WHERE characterID = ?`
	args := []any{charID}
	if before > 0 {
		where += ` AND transactionID < ?`
		args = append(args, before)
	}
	rows, err := db.QueryContext(ctx,
		`SELECT transactionID, transactionDateTime, typeID, quantity,
		        price, transactionType, clientID, regionID, stationID
		   FROM market_transactions `+where+`
		  ORDER BY transactionDateTime DESC, transactionID DESC
		  LIMIT 500`, args...)
	if err != nil {
		return nil, fmt.Errorf("GetCharTransactionsBefore: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var txnID, dt, typeID, qty, txnType, cID, rID, sID int64
		var price float64
		if err := rows.Scan(&txnID, &dt, &typeID, &qty, &price, &txnType, &cID, &rID, &sID); err != nil {
			return nil, fmt.Errorf("GetCharTransactionsBefore scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(txnID), intRep(dt), intRep(typeID), intRep(qty),
			floatRep(price), intRep(txnType), intRep(cID), intRep(rID), intRep(sID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeRowset(txnCols, result), nil
}
