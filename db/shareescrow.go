package db

// Share escrow without an exchange. The SCC module keeps open orders in sccOrders: a buy
// holds its ISK, a sell parks its shares with DepositoryHolderID. A server built without the
// module would leave both out of their owners' reach, so boot calls ReleaseShareEscrow, which
// closes every open order and sends the ISK and shares home. Listings, trades, candles,
// filings and closed orders are left as they are; the module re-places orders when it is
// back. See docs/module-seams.md#share-market.

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"time"
)

// RefMarketEscrow is the journal refType of an escrow debit or refund.
const RefMarketEscrow = 42

// ShareMarketInstalled is set by the SCC module's init. Boot releases escrow only when it is
// false.
var ShareMarketInstalled bool

// shareEscrowBatch is how many orders one transaction closes.
const shareEscrowBatch = 500

// ReleaseShareEscrow closes every open exchange order: a buy's escrowed ISK goes back to its
// owner's wallet (journal refType 42) and a sell's shares move from the depository back to the
// seller. Each batch is one transaction, so an interrupted run leaves whole batches done and
// the next boot finishes the rest. A database without sccOrders has nothing to release.
func ReleaseShareEscrow(ctx context.Context, db *sql.DB) (orders int, isk float64, shares int64, err error) {
	type openOrder struct {
		id, owner, corp, qty int64
		buy                  bool
		escrow               float64
	}
	for {
		rows, qerr := db.QueryContext(ctx, `SELECT orderID, ownerID, corpID, isBuy, qtyOpen, escrowISK
			FROM sccOrders WHERE state = 0 ORDER BY orderID LIMIT ?`, shareEscrowBatch)
		if qerr != nil {
			if tableMissing(qerr) {
				return orders, isk, shares, nil
			}
			return orders, isk, shares, fmt.Errorf("ReleaseShareEscrow read: %w", qerr)
		}
		var batch []openOrder
		for rows.Next() {
			var o openOrder
			if err := rows.Scan(&o.id, &o.owner, &o.corp, &o.buy, &o.qty, &o.escrow); err != nil {
				rows.Close()
				return orders, isk, shares, fmt.Errorf("ReleaseShareEscrow scan: %w", err)
			}
			batch = append(batch, o)
		}
		rows.Close()
		if len(batch) == 0 {
			return orders, isk, shares, nil
		}
		tx, berr := db.BeginTx(ctx, nil)
		if berr != nil {
			return orders, isk, shares, berr
		}
		var bIsk float64
		var bShares int64
		now := time.Now().Unix()
		for _, o := range batch {
			if o.buy && o.escrow > 0 {
				credited, err := refundShareEscrow(ctx, tx, o.owner, o.corp, o.escrow, "SCC escrow refund: exchange closed")
				if err != nil {
					tx.Rollback()
					return orders, isk, shares, fmt.Errorf("ReleaseShareEscrow order %d: %w", o.id, err)
				}
				if credited {
					bIsk += o.escrow
				}
			} else if !o.buy && o.qty > 0 {
				if _, err := moveSharesTx(ctx, tx, o.corp, DepositoryHolderID, o.owner, o.qty); err != nil {
					tx.Rollback()
					return orders, isk, shares, fmt.Errorf("ReleaseShareEscrow order %d shares: %w", o.id, err)
				}
				bShares += o.qty
			}
			if _, err := tx.ExecContext(ctx, `UPDATE sccOrders SET escrowISK = 0, state = 2, closedAt = ?
				WHERE orderID = ? AND state = 0`, now, o.id); err != nil {
				tx.Rollback()
				return orders, isk, shares, fmt.Errorf("ReleaseShareEscrow close %d: %w", o.id, err)
			}
		}
		if err := tx.Commit(); err != nil {
			return orders, isk, shares, fmt.Errorf("ReleaseShareEscrow commit: %w", err)
		}
		orders += len(batch)
		isk += bIsk
		shares += bShares
	}
}

// refundShareEscrow credits ISK back to a character or corporation wallet and journals it.
// An owner with no wallet (deleted since it placed the order) is skipped, credited false: its
// ISK went with it.
func refundShareEscrow(ctx context.Context, tx DBTX, ownerID, corpID int64, amount float64, reason string) (credited bool, err error) {
	table, key, ok, err := walletTable(ctx, tx, ownerID)
	if err != nil || !ok {
		return false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE `+table+` SET balance = balance + ? WHERE `+key+` = ?`, amount, ownerID); err != nil {
		return false, fmt.Errorf("credit %d: %w", ownerID, err)
	}
	var bal float64
	if err := tx.QueryRowContext(ctx, `SELECT balance FROM `+table+` WHERE `+key+` = ?`, ownerID).Scan(&bal); err != nil {
		return false, fmt.Errorf("balance %d: %w", ownerID, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO market_journal
		   (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		    accountID, amount, balance, reason, accountKey)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 1000)`,
		ownerID, win32TimeNow(), RefMarketEscrow, DepositoryHolderID, ownerID, strconv.FormatInt(corpID, 10),
		amount, bal, JournalReason(reason))
	return err == nil, err
}
