package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Corporation-management rules shared by the client handlers and other callers (ESP panel,
// module CEOs). Each is the single statement of its rule.

// ─── member limit ──────────────────────────────────────────────────────────

// CorpHasOpening reports whether corpID can take one more member. used counts players
// (accountID > 0; NPC agents also carry a corporationID), never corporation.memberCount, which
// nothing maintains. NPC corps and a limit of 0 always have room. Checked only at a new join: a
// corp already over its limit keeps its members.
func CorpHasOpening(ctx context.Context, db *sql.DB, corpID int64) (ok bool, used, limit int64, err error) {
	npc, err := IsNPCCorp(ctx, db, corpID)
	if err != nil {
		return false, 0, 0, err
	}
	if npc {
		return true, 0, 0, nil
	}
	if err := db.QueryRowContext(ctx,
		`SELECT memberLimit FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&limit); err != nil {
		return false, 0, 0, fmt.Errorf("CorpHasOpening limit corp=%d: %w", corpID, err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE corporationID = ? AND accountID > 0`, corpID,
	).Scan(&used); err != nil {
		return false, 0, 0, fmt.Errorf("CorpHasOpening count corp=%d: %w", corpID, err)
	}
	if limit <= 0 {
		return true, used, limit, nil
	}
	return used < limit, used, limit, nil
}

// ErrCorpFull is what a join refused on the member limit reports.
var ErrCorpFull = errors.New("the corporation has no room for new members")

// ─── abilities ─────────────────────────────────────────────────────────────

var corpMemberLimitDefault = []float64{10, 10, 50, 200, 1000}

// CorpAbilitiesFromSkills is the member limit and race mask a CEO's skills give a corporation.
// The limit is server_settings corp.memberLimitFormula (default corpMemberLimitDefault: base,
// then per Corporation Management, Megacorp Management, Empire Control, Sovereignty level). The
// mask is the founding race, or all (63) with Ethnic Relations or when the corp has no race.
func CorpAbilitiesFromSkills(sk CorpMgmtSkills, corpRaceID int64) (memberLimit, allowedRaces int64) {
	f := CachedSettingFloats("corp.memberLimitFormula", corpMemberLimitDefault, 0, 1e6)
	memberLimit = int64(f[0] + f[1]*float64(sk.CorpMgmt) + f[2]*float64(sk.Megacorp) +
		f[3]*float64(sk.EmpireControl) + f[4]*float64(sk.Sovereignty))
	allowedRaces = corpRaceID
	if sk.EthnicRelations > 0 || allowedRaces == 0 {
		allowedRaces = 63 // all races
	}
	return memberLimit, allowedRaces
}

// RecomputeCorpAbilities re-derives memberLimit and allowedMemberRaceIDs from the CEO's skills
// and stores them (the corp window's refresh abilities button). Returns old and new values for
// the OnCorporationChanged push.
func RecomputeCorpAbilities(ctx context.Context, db *sql.DB, corpID int64) (oldLimit, newLimit, oldRaces, newRaces int64, err error) {
	var ceoID int64
	var corpRace sql.NullInt64
	if err := db.QueryRowContext(ctx,
		`SELECT ceoID, raceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&ceoID, &corpRace); err != nil {
		return 0, 0, 0, 0, fmt.Errorf("RecomputeCorpAbilities corp=%d: %w", corpID, err)
	}
	newLimit, newRaces = CorpAbilitiesFromSkills(GetCorpMgmtSkills(ctx, db, ceoID), corpRace.Int64)
	oldLimit, oldRaces, err = UpdateCorpAbilities(ctx, db, corpID, newLimit, newRaces)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	return oldLimit, newLimit, oldRaces, newRaces, nil
}

// ─── headquarters ──────────────────────────────────────────────────────────

// ErrNoRealEstate is SetCorpHQ's refusal: a corporation may only be
// headquartered where it owns the station or rents an office
// (CorpCantSetHQAsNoRealEstateHere).
var ErrNoRealEstate = errors.New("the corporation has no office at that station")

// SetCorpHQ moves a corporation's headquarters. The caller pushes OnCorporationChanged and the
// hqID session change. See docs/corp-notes.md#hq-session.
func SetCorpHQ(ctx context.Context, db *sql.DB, corpID, stationID int64) (oldHQ int64, err error) {
	if err := db.QueryRowContext(ctx,
		`SELECT stationID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&oldHQ); err != nil {
		return 0, fmt.Errorf("SetCorpHQ corp=%d: %w", corpID, err)
	}
	if oldHQ == stationID {
		return oldHQ, nil
	}
	if officeID, _, _ := GetCorpOfficeAt(ctx, db, corpID, stationID); officeID == 0 {
		return oldHQ, ErrNoRealEstate
	}
	if _, err := db.ExecContext(ctx,
		`UPDATE corporation SET stationID = ? WHERE corporationID = ?`, stationID, corpID); err != nil {
		return oldHQ, fmt.Errorf("SetCorpHQ corp=%d: %w", corpID, err)
	}
	return oldHQ, nil
}

// ─── money ─────────────────────────────────────────────────────────────────

// ErrInsufficientFunds is TransferISK's refusal when the source wallet cannot
// cover the amount. Nothing has moved when it is returned.
var ErrInsufficientFunds = errors.New("insufficient funds")

// walletTable answers which table holds ownerID's wallet: a character or a
// corporation. ok is false for anything else.
func walletTable(ctx context.Context, tx DBTX, ownerID int64) (table, key string, ok bool, err error) {
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterID = ?`, ownerID).Scan(&n); err != nil {
		return "", "", false, err
	}
	if n > 0 {
		return "character_", "characterID", true, nil
	}
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationID = ?`, ownerID).Scan(&n); err != nil {
		return "", "", false, err
	}
	if n > 0 {
		return "corporation", "corporationID", true, nil
	}
	return "", "", false, nil
}

// TransferISK moves amount between two wallets (character or corporation) in one transaction:
// a conditional debit that never takes a wallet below zero, the credit, and a journal row in
// each wallet (reason clamped by JournalReason). Returns both balances after the move.
func TransferISK(ctx context.Context, db *sql.DB, srcOwnerID, dstOwnerID int64, amount float64, refTypeID int, reason string) (srcBal, dstBal float64, err error) {
	return TransferISKBy(ctx, db, srcOwnerID, dstOwnerID, amount, refTypeID, reason, 0)
}

// TransferISKBy is TransferISK with the journal's argID1 set (0 = NULL). The client reads
// argID1 as the acting character on refType 37, where NULL prints Id:None.
func TransferISKBy(ctx context.Context, db *sql.DB, srcOwnerID, dstOwnerID int64, amount float64, refTypeID int, reason string, argID1 int64) (srcBal, dstBal float64, err error) {
	if amount <= 0 {
		return 0, 0, fmt.Errorf("TransferISK: amount must be positive, got %.2f", amount)
	}
	if srcOwnerID == dstOwnerID {
		return 0, 0, fmt.Errorf("TransferISK: source and destination are the same wallet (%d)", srcOwnerID)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("TransferISK begin: %w", err)
	}
	defer func() {
		if err != nil {
			_ = tx.Rollback()
		}
	}()
	if srcBal, dstBal, err = transferISKTxArg(ctx, tx, srcOwnerID, dstOwnerID, amount, refTypeID, reason, argID1); err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("TransferISK commit: %w", err)
	}
	return srcBal, dstBal, nil
}

// transferISKTx is TransferISK's body inside a caller's transaction, so a
// money move can commit together with whatever it pays for (TradeShares).
// Returns ErrInsufficientFunds, unwrapped, when the debit is refused.
func transferISKTx(ctx context.Context, tx *sql.Tx, srcOwnerID, dstOwnerID int64, amount float64, refTypeID int, reason string) (srcBal, dstBal float64, err error) {
	return transferISKTxArg(ctx, tx, srcOwnerID, dstOwnerID, amount, refTypeID, reason, 0)
}

func transferISKTxArg(ctx context.Context, tx *sql.Tx, srcOwnerID, dstOwnerID int64, amount float64, refTypeID int, reason string, argID1 int64) (srcBal, dstBal float64, err error) {
	srcTable, srcKey, ok, err := walletTable(ctx, tx, srcOwnerID)
	if err != nil {
		return 0, 0, fmt.Errorf("TransferISK source: %w", err)
	}
	if !ok {
		return 0, 0, fmt.Errorf("TransferISK: no wallet for owner %d", srcOwnerID)
	}
	dstTable, dstKey, ok, err := walletTable(ctx, tx, dstOwnerID)
	if err != nil {
		return 0, 0, fmt.Errorf("TransferISK destination: %w", err)
	}
	if !ok {
		return 0, 0, fmt.Errorf("TransferISK: no wallet for owner %d", dstOwnerID)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE `+srcTable+` SET balance = balance - ? WHERE `+srcKey+` = ? AND balance >= ?`,
		amount, srcOwnerID, amount)
	if err != nil {
		return 0, 0, fmt.Errorf("TransferISK debit: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, 0, ErrInsufficientFunds
	}
	if _, err = tx.ExecContext(ctx,
		`UPDATE `+dstTable+` SET balance = balance + ? WHERE `+dstKey+` = ?`,
		amount, dstOwnerID); err != nil {
		return 0, 0, fmt.Errorf("TransferISK credit: %w", err)
	}
	if err = tx.QueryRowContext(ctx,
		`SELECT balance FROM `+srcTable+` WHERE `+srcKey+` = ?`, srcOwnerID).Scan(&srcBal); err != nil {
		return 0, 0, fmt.Errorf("TransferISK source balance: %w", err)
	}
	if err = tx.QueryRowContext(ctx,
		`SELECT balance FROM `+dstTable+` WHERE `+dstKey+` = ?`, dstOwnerID).Scan(&dstBal); err != nil {
		return 0, 0, fmt.Errorf("TransferISK destination balance: %w", err)
	}

	reason = JournalReason(reason)
	now := win32TimeNow()
	const journal = `INSERT INTO market_journal
		   (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		    accountID, amount, balance, reason, accountKey)
		 VALUES (?, ?, ?, ?, ?, ?, 0, ?, ?, ?, 1000)`
	var arg any // NULL unless the caller named one
	if argID1 != 0 {
		arg = argID1
	}
	if _, err = tx.ExecContext(ctx, journal,
		srcOwnerID, now, refTypeID, srcOwnerID, dstOwnerID, arg, -amount, srcBal, reason); err != nil {
		return 0, 0, fmt.Errorf("TransferISK source journal: %w", err)
	}
	if _, err = tx.ExecContext(ctx, journal,
		dstOwnerID, now, refTypeID, srcOwnerID, dstOwnerID, arg, amount, dstBal, reason); err != nil {
		return 0, 0, fmt.Errorf("TransferISK destination journal: %w", err)
	}
	return srcBal, dstBal, nil
}
