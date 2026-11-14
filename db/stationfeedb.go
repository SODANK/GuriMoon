package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"gurimoon/marshal"
)

// Station fees a player owner sets in the STATION MGMT. window that the
// server charges itself: docking and the medical clone contract. Both pay the
// owner corporation. See docs/station-management.md#enforcement.

// RefDockingFee is const.refDockingFee.
const RefDockingFee = 12

// RefCloneTransfer is const.refCloneTransfer, the journal type of a clone
// contract (a new home station).
const RefCloneTransfer = 52

// DockingFee is what charID pays to dock its ship at stationID: the owner's
// dockingCostPerVolume times the hull's volume, after the Docking cost
// modifier, in whole ISK. 0 at NPC stations, for the owner's own corporation
// and with station.enforceManagement off.
func DockingFee(ctx context.Context, db *sql.DB, stationID, charID, shipTypeID int64) float64 {
	p := ResolveStationPatron(ctx, db, stationID, charID)
	if !p.Managed {
		return 0
	}
	var perVolume, volume float64
	if err := db.QueryRowContext(ctx,
		`SELECT dockingCostPerVolume FROM staStations WHERE stationID = ?`, stationID).Scan(&perVolume); err != nil || perVolume <= 0 {
		return 0
	}
	_ = db.QueryRowContext(ctx, `SELECT COALESCE(volume, 0) FROM invTypes WHERE typeID = ?`, shipTypeID).Scan(&volume)
	return math.Ceil(patronFee(ctx, db, p, stationID, StationServiceDocking, perVolume*volume))
}

// CloneContractFee is what charID pays to make stationID its home station:
// server_settings clone.contractCost (default const.costCloneContract, 5600),
// charged only by a player owner and not to its own corporation. No cost
// modifier: the client quotes the constant itself (medical.py).
func CloneContractFee(ctx context.Context, db *sql.DB, stationID, charID int64) float64 {
	if !ResolveStationPatron(ctx, db, stationID, charID).Managed {
		return 0
	}
	return math.Round(CachedSettingFloat("clone.contractCost", 5600, 0, 1e12))
}

// ── clone contracts (the window's Clone Contracts tab) ───────────────────────

// CloneContractCorps lists the corporations whose pilots have their home
// station (medical clone contract) at stationID.
func CloneContractCorps(ctx context.Context, db *sql.DB, stationID int64) ([]int64, error) {
	return queryIDs(ctx, db, `
		SELECT DISTINCT corporationID FROM character_
		 WHERE homeStationID = ? AND accountID > 0 ORDER BY corporationID`, stationID)
}

// CloneContractChars lists corpID's pilots whose home station is stationID.
func CloneContractChars(ctx context.Context, db *sql.DB, stationID, corpID int64) ([]int64, error) {
	return queryIDs(ctx, db, `
		SELECT characterID FROM character_
		 WHERE homeStationID = ? AND corporationID = ? AND accountID > 0 ORDER BY characterID`,
		stationID, corpID)
}

// ErrNoCloneRelocation is the CrpCanNotRelocateClonesAsNoHQ refusal: the
// pilots' corporation has no headquarters to move the contracts to, or its
// headquarters is this station.
var ErrNoCloneRelocation = MsgErr("CrpCanNotRelocateClonesAsNoHQ", nil,
	"The requested clone contract(s) can not be moved. As there is no where to move them to.")

// RevokeCloneContracts moves the home station of corpID's pilots at stationID
// (one pilot when charID is not -1) to their corporation's headquarters.
// Returns the pilots moved.
func RevokeCloneContracts(ctx context.Context, db *sql.DB, stationID, corpID, charID int64) ([]int64, error) {
	var hq int64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(stationID, 0) FROM corporation WHERE corporationID = ?`, corpID).Scan(&hq); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("RevokeCloneContracts hq %d: %w", corpID, err)
	}
	if hq == 0 || hq == stationID {
		return nil, ErrNoCloneRelocation
	}
	var chars []int64
	var err error
	if charID == -1 {
		chars, err = CloneContractChars(ctx, db, stationID, corpID)
	} else {
		chars, err = queryIDs(ctx, db, `
			SELECT characterID FROM character_
			 WHERE characterID = ? AND homeStationID = ? AND corporationID = ?`, charID, stationID, corpID)
	}
	if err != nil || len(chars) == 0 {
		return nil, err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("RevokeCloneContracts begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, id := range chars {
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET homeStationID = ? WHERE characterID = ? AND homeStationID = ?`,
			hq, id, stationID); err != nil {
			return nil, fmt.Errorf("RevokeCloneContracts %d: %w", id, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("RevokeCloneContracts commit: %w", err)
	}
	return chars, nil
}

func queryIDs(ctx context.Context, db *sql.DB, q string, args ...any) ([]int64, error) {
	rows, err := db.QueryContext(ctx, q, args...)
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

// ChargeStationFee moves fee from charID's wallet to the station's owner,
// journalled on both sides under refTypeID. A refusal for too little ISK is a
// NotEnoughMoney client message.
func ChargeStationFee(ctx context.Context, db *sql.DB, charID, stationID int64, fee float64, refTypeID int, what string) (balance float64, err error) {
	if fee <= 0 {
		return 0, nil
	}
	owner := StationOwnerCorp(ctx, db, stationID)
	if owner == 0 {
		return 0, fmt.Errorf("ChargeStationFee: station %d has no owner", stationID)
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("ChargeStationFee begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	reason := JournalReason(what + " at " + GetStationName(ctx, db, stationID))
	// argID1 is the station: refCloneTransfer prints it ("Clone transfer to %s").
	balance, _, err = transferISKTxArg(ctx, tx, charID, owner, fee, refTypeID, reason, stationID)
	if errors.Is(err, ErrInsufficientFunds) {
		var have float64
		_ = tx.QueryRowContext(ctx, `SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&have)
		return 0, NotEnoughMoneyErr(fee, have)
	}
	if err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, fmt.Errorf("ChargeStationFee commit: %w", err)
	}
	return balance, nil
}

// NotEnoughMoneyErr is the client's NotEnoughMoney box for a fee of need ISK
// against a balance of have.
func NotEnoughMoneyErr(need, have float64) error {
	return MsgErr("NotEnoughMoney", map[string]marshal.PyRep{
		"amount":  marshal.PyString{Value: groupISK(need)},
		"balance": marshal.PyString{Value: groupISK(have)},
	}, fmt.Sprintf("This costs %s ISK; you have %s.", groupISK(need), groupISK(have)))
}

// CharBalance is charID's wallet balance (0 when unreadable).
func CharBalance(ctx context.Context, db *sql.DB, charID int64) float64 {
	var b float64
	_ = db.QueryRowContext(ctx, `SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&b)
	return b
}
