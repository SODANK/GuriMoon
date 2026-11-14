package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"gurimoon/marshal"
)

// ── constants ───────────────────────────────────────────────────────────────

// CourierBasicFee is the flat ISK cost the courier/escrow creation panel
// quotes for listing a package ("Basic cost of a courier mission is ...").
const CourierBasicFee = 1000.0

// CourierMaxVolumeM3 caps the total volume a single courier contract may carry.
const CourierMaxVolumeM3 = 20000.0

// CourierRefType is the journal refTypeID for courier transactions: shipped refType 23 "Courier
// Mission Escrow". Type 46 is "Broker fee" and would read as a market commission.
const CourierRefType = 23

// TypePlasticWrap is the courier package the acceptor carries. The client sizes it by listing its
// contents (cfg.GetItemVolume), opens it without a capacity bar and offers "Break..." instead of
// jettison; a Cargo Container is 27,500 m3 whatever it holds. See docs/db-6-notes.md#courier-package-model.
const TypePlasticWrap int64 = 3468

// Courier mission states, stored in couriermissions.state.
const (
	CourierStatePosted    = 0 // available for acceptance
	CourierStateAccepted  = 1 // in transit
	CourierStateDelivered = 2 // completed successfully
	CourierStateFailed    = 3 // acceptor didn't deliver; collateral forfeited
	CourierStateExpired   = 4 // nobody accepted before deadline
	CourierStateCancelled = 5 // issuer withdrew before acceptance
)

// ── types ───────────────────────────────────────────────────────────────────

// CourierListing is one available contract as the browse list renders it.
type CourierListing struct {
	MissionID       int64
	IssuerID        int64
	PickupStationID int64
	DropStationID   int64
	TotalReward     float64
	Collateral      float64
	Volume          float64
	DateIssued      int64 // unix
	DateExpires     int64 // unix
	Description     string
}

// CourierRow is one courier contract (issued or accepted) for GetMyCourierContracts.
type CourierRow struct {
	MissionID       int64
	State           int
	IssuerID        int64
	AcceptorID      int64
	PickupStationID int64
	DropStationID   int64
	TotalReward     float64
	AcceptFee       float64
	Collateral      float64
	Volume          float64
	DateIssued      int64 // unix
	DateExpires     int64 // unix
	DateAccepted    int64 // unix
	DateCompleted   int64 // unix
	Description     string
	CraterID        int64 // the crate entity the acceptor carries (0 = not yet accepted)
	CrateContainerID int64 // escrow container holding items
}

// ManifestItem is one item inside a courier escrow container.
type ManifestItem struct {
	ItemID             int64
	TypeID             int64
	OwnerID            int64
	Quantity           int64
	GroupID            int64
	CategoryID         int64
	Copy               int64 // invBlueprints.copy (0 = original, 1 = copy; 0 for non-BPs)
	MaterialLevel      int64 // invBlueprints.materialLevel
	ProductivityLevel  int64 // invBlueprints.productivityLevel
	LicensedRuns       int64 // invBlueprints.licensedProductionRunsRemaining
}

// CourierCreateParams bundles everything InsertCourierMission needs.
type CourierCreateParams struct {
	IssuerID        int64
	IssuerCorpID    int64
	PickupStationID int64
	DropStationID   int64
	Reward          float64
	Collateral      float64
	ListingFee      float64
	Volume          float64
	Description     string
	DaysToExpire    int
	ContainerID     int64
	IssuerIsBot     bool
	// Location geography, filled by the caller.
	PickupSolarSystemID int64
	PickupRegionID      int64
	DropSolarSystemID   int64
	DropRegionID        int64
}

// ── PyRep helpers ───────────────────────────────────────────────────────────

// courierItemCols are the inventory columns uix.GetItemData reads off each
// row of the courier item list (base_missions.py ProcessCreateCourierOrEscrow).
var courierItemCols = []string{
	"itemID", "typeID", "ownerID", "locationID", "flag",
	"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
}

// GetCourierOptions builds the result of missionMgr's bound GetCourierOptions: a util.KeyVal
// with basicFee and itemList (an IndexRowset the client re-wraps). None raises AttributeError and
// leaves the Missions tab on its loading spinner. Items are the character's hangar (flag 4) at
// the pickup station; on a query error the PyRep is still usable, with an empty list.
func GetCourierOptions(ctx context.Context, db *sql.DB, charID, stationID int64) (marshal.PyRep, error) {
	log.Printf("[db] GetCourierOptions charID=%d stationID=%d", charID, stationID)
	rows, err := db.QueryContext(ctx,
		`SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		        e.contraband, e.singleton, e.quantity,
		        COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		   FROM entity e
		   LEFT JOIN invTypes t ON t.typeID = e.typeID
		   LEFT JOIN invGroups g ON g.groupID = t.groupID
		  WHERE e.ownerID = ? AND e.flag = 4 AND e.locationID = ?
		  ORDER BY e.itemID`,
		charID, stationID,
	)
	if err != nil {
		return courierOptionsRep(nil), fmt.Errorf("GetCourierOptions query: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var a AssetItem
		if err := rows.Scan(&a.ItemID, &a.TypeID, &a.OwnerID, &a.LocationID,
			&a.Flag, &a.Contraband, &a.Singleton, &a.Quantity,
			&a.GroupID, &a.CategoryID); err != nil {
			return courierOptionsRep(nil), fmt.Errorf("GetCourierOptions scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(a.ItemID), intRep(a.TypeID), intRep(a.OwnerID), intRep(a.LocationID),
			intRep(a.Flag), intRep(a.Contraband), intRep(a.Singleton), intRep(a.Quantity),
			intRep(a.GroupID), intRep(a.CategoryID), marshal.PyNone{},
		})
	}
	if err := rows.Err(); err != nil {
		return courierOptionsRep(nil), fmt.Errorf("GetCourierOptions rows: %w", err)
	}
	log.Printf("[db] GetCourierOptions charID=%d stationID=%d → %d items", charID, stationID, len(result))
	return courierOptionsRep(result), nil
}

// courierOptionsRep wraps courier item rows in the util.KeyVal the client expects.
func courierOptionsRep(rows [][]marshal.PyRep) marshal.PyRep {
	return makeKeyVal(
		[]string{"basicFee", "itemList"},
		[]marshal.PyRep{
			floatRep(CourierBasicFee),
			makeIndexRowset("itemID", courierItemCols, rows),
		},
	)
}

// ── escrow container ────────────────────────────────────────────────────────

// CreateCourierEscrowContainer inserts the invisible container a contract's items are escrowed
// into. It has ownerID 0, so neither party sees it in their hangar.
func CreateCourierEscrowContainer(ctx context.Context, tx *sql.Tx, stationID int64) (int64, error) {
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Courier Escrow', ?, 0, ?, ?, 0, 1, 1, 0, 0, 0, 'courier_escrow')`,
		TypeCargoContainer, stationID, flagHangar)
	if err != nil {
		return 0, fmt.Errorf("CreateCourierEscrowContainer: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateCourierEscrowContainer LastInsertId: %w", err)
	}
	return id, nil
}

// EscrowCourierItem moves an item (or part of a stack) from the character's
// hangar into the courier escrow container. Same split logic as EscrowTradeItem.
func EscrowCourierItem(ctx context.Context, tx *sql.Tx, charID, stationID, containerID, itemID, quantity int64) error {
	var (
		ownerID, locationID, have, typeID, flag, singleton int64
		itemName                                           string
	)
	err := tx.QueryRowContext(ctx, `
		SELECT ownerID, locationID, quantity, typeID, flag, singleton, itemName
		FROM entity WHERE itemID = ? FOR UPDATE`, itemID,
	).Scan(&ownerID, &locationID, &have, &typeID, &flag, &singleton, &itemName)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("EscrowCourierItem: item %d not found", itemID)
	}
	if err != nil {
		return fmt.Errorf("EscrowCourierItem lookup: %w", err)
	}
	if ownerID != charID {
		return fmt.Errorf("EscrowCourierItem: item %d not owned by char %d", itemID, charID)
	}
	if locationID != stationID && locationID != containerID {
		return fmt.Errorf("EscrowCourierItem: item %d not at station %d", itemID, stationID)
	}
	if quantity <= 0 || quantity > have {
		quantity = have
	}

	if quantity == have {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET locationID = ?, flag = ? WHERE itemID = ?`,
			containerID, flagHangar, itemID); err != nil {
			return fmt.Errorf("EscrowCourierItem move: %w", err)
		}
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`,
			quantity, itemID); err != nil {
			return fmt.Errorf("EscrowCourierItem split source: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES (?, ?, ?, ?, ?, 0, ?, ?, 0, 0, 0, NULL)`,
			itemName, typeID, charID, containerID, flagHangar, singleton, quantity); err != nil {
			return fmt.Errorf("EscrowCourierItem split dest: %w", err)
		}
	}
	return nil
}

// ── insert ──────────────────────────────────────────────────────────────────

// InsertCourierMission creates a new courier contract. The caller has already
// escrowed the items and debited the issuer's wallet.
func InsertCourierMission(ctx context.Context, tx *sql.Tx, p CourierCreateParams) (int64, error) {
	now := time.Now().Unix()
	expires := now + int64(p.DaysToExpire)*86400

	isBot := 0
	if p.IssuerIsBot {
		isBot = 1
	}

	const kind = 1 // courier

	res, err := tx.ExecContext(ctx, `
		INSERT INTO couriermissions
			(kind, state, description, issuerID, acceptorID, acceptFee, totalReward,
			 dateIssued, dateExpires, pickupStationID, dropStationID, volume,
			 pickupSolarSystemID, pickupRegionID, dropSolarSystemID, dropRegionID,
			 collateral, rewardEscrow, listingFee, crateContainerID,
			 issuerCorpID, issuerIsBot)
		VALUES (?, ?, ?, ?, NULL, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		kind, CourierStatePosted, p.Description, p.IssuerID, p.Reward,
		now, expires, p.PickupStationID, p.DropStationID, p.Volume,
		p.PickupSolarSystemID, p.PickupRegionID,
		p.DropSolarSystemID, p.DropRegionID,
		p.Collateral, p.Reward, p.ListingFee, p.ContainerID,
		p.IssuerCorpID, isBot)
	if err != nil {
		return 0, fmt.Errorf("InsertCourierMission: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsertCourierMission LastInsertId: %w", err)
	}
	return id, nil
}

// ── accept ──────────────────────────────────────────────────────────────────

// AcceptCourierMissionDB accepts a posted courier contract. It creates a crate
// entity the acceptor will carry, debits collateral, and updates the row to
// state=1. Returns the crate entity ID.
func AcceptCourierMissionDB(ctx context.Context, db *sql.DB,
	missionID, acceptorID, acceptorCorpID int64, acceptorIsBot bool) (crateID int64, err error) {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("AcceptCourierMission begin: %w", err)
	}
	defer tx.Rollback()

	// Lock the row and verify state.
	var state int
	var issuerID, pickupStationID int64
	var collateral float64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, pickupStationID, collateral
		FROM couriermissions WHERE missionID = ? FOR UPDATE`,
		missionID).Scan(&state, &issuerID, &pickupStationID, &collateral); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("courier contract %d not found", missionID)
		}
		return 0, fmt.Errorf("AcceptCourierMission lock: %w", err)
	}
	if state != CourierStatePosted {
		return 0, fmt.Errorf("courier contract %d is not available (state=%d)", missionID, state)
	}
	if issuerID == acceptorID {
		return 0, fmt.Errorf("cannot accept your own courier contract")
	}

	// Verify wallet.
	var balance float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
		acceptorID).Scan(&balance); err != nil {
		return 0, fmt.Errorf("AcceptCourierMission balance: %w", err)
	}
	if balance < collateral {
		return 0, fmt.Errorf("insufficient funds for collateral (need %.2f, have %.2f)", collateral, balance)
	}

	// Debit collateral.
	newBalance := balance - collateral
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = ? WHERE characterID = ?`,
		newBalance, acceptorID); err != nil {
		return 0, fmt.Errorf("AcceptCourierMission debit: %w", err)
	}

	// Journal entry for collateral lock.
	if collateral > 0 {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			acceptorID, win32TimeNow(), CourierRefType, acceptorID, issuerID,
			-collateral, newBalance, JournalReason("Courier collateral deposit")); err != nil {
			log.Printf("[db] AcceptCourierMissionDB: best-effort write failed: %v", err)
		}
	}

	// Create the package at the pickup station, owned by the acceptor. It holds nothing: the goods
	// stay in the issuer's escrow box and List answers with them (CourierPackageItems).
	crateInfo := fmt.Sprintf("courier_crate_%d", missionID)
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Courier Package', ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, ?)`,
		TypePlasticWrap, acceptorID, pickupStationID, flagHangar, crateInfo)
	if err != nil {
		return 0, fmt.Errorf("AcceptCourierMission crate: %w", err)
	}
	crateID, err = res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("AcceptCourierMission crate id: %w", err)
	}

	// Update the mission row.
	isBotVal := 0
	if acceptorIsBot {
		isBotVal = 1
	}
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		UPDATE couriermissions
		SET state = ?, acceptorID = ?, acceptorCorpID = ?, acceptorIsBot = ?,
		    craterID = ?, dateAccepted = ?
		WHERE missionID = ?`,
		CourierStateAccepted, acceptorID, acceptorCorpID, isBotVal,
		crateID, now, missionID); err != nil {
		return 0, fmt.Errorf("AcceptCourierMission update: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("AcceptCourierMission commit: %w", err)
	}
	return crateID, nil
}

// ── deliver ─────────────────────────────────────────────────────────────────

// DeliverCourierMissionDB completes a courier contract. Moves escrowed items
// to the issuer's hangar at the drop station, credits the acceptor with
// reward + collateral, and cleans up the container and crate entities.
func DeliverCourierMissionDB(ctx context.Context, db *sql.DB,
	missionID, acceptorID, dropStationID int64) error {

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("DeliverCourier begin: %w", err)
	}
	defer tx.Rollback()

	var state int
	var issuerID, crateContainerID, craterID, storedDropStation int64
	var reward, collateral float64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, crateContainerID, craterID, dropStationID,
		       rewardEscrow, collateral
		FROM couriermissions WHERE missionID = ? FOR UPDATE`,
		missionID).Scan(&state, &issuerID, &crateContainerID, &craterID,
		&storedDropStation, &reward, &collateral); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("courier contract %d not found", missionID)
		}
		return fmt.Errorf("DeliverCourier lock: %w", err)
	}
	if state != CourierStateAccepted {
		return fmt.Errorf("courier contract %d is not in transit (state=%d)", missionID, state)
	}
	if dropStationID != storedDropStation {
		return fmt.Errorf("must deliver at the designated drop station")
	}
	// The package must be at the drop station, in the hangar or in the hold of a docked ship;
	// otherwise a pilot could be paid for flying there empty.
	if craterID > 0 {
		var crateLoc int64
		if err := tx.QueryRowContext(ctx,
			`SELECT locationID FROM entity WHERE itemID = ?`, craterID).Scan(&crateLoc); err != nil {
			return fmt.Errorf("the courier package for contract %d no longer exists", missionID)
		}
		if crateLoc != dropStationID {
			var shipLoc int64
			if tx.QueryRowContext(ctx,
				`SELECT locationID FROM entity WHERE itemID = ?`, crateLoc).Scan(&shipLoc) != nil ||
				shipLoc != dropStationID {
				return fmt.Errorf("the courier package must be in your hangar or cargo hold at the drop station")
			}
		}
	}

	// Move escrowed items to issuer's hangar at the drop station.
	if crateContainerID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			WHERE locationID = ?`,
			issuerID, dropStationID, flagHangar, crateContainerID); err != nil {
			return fmt.Errorf("DeliverCourier move items: %w", err)
		}
		// Delete the escrow container.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, crateContainerID); err != nil {
			return fmt.Errorf("DeliverCourier delete container: %w", err)
		}
	}

	// Delete the crate entity.
	if craterID > 0 {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, craterID); err != nil {
			return fmt.Errorf("DeliverCourier delete crate: %w", err)
		}
	}

	// Credit the acceptor: reward + collateral returned.
	payout := reward + collateral
	var acceptorBalance float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
		acceptorID).Scan(&acceptorBalance); err != nil {
		return fmt.Errorf("DeliverCourier acceptor balance: %w", err)
	}
	newAccBalance := acceptorBalance + payout
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = ? WHERE characterID = ?`,
		newAccBalance, acceptorID); err != nil {
		return fmt.Errorf("DeliverCourier credit acceptor: %w", err)
	}

	// Journal: acceptor gets paid.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		acceptorID, win32TimeNow(), CourierRefType, acceptorID, issuerID,
		payout, newAccBalance, JournalReason("Courier contract delivered")); err != nil {
		log.Printf("[db] DeliverCourierMissionDB: best-effort write failed: %v", err)
	}

	// Journal: issuer sees the delivery (informational, 0 ISK since reward was already debited).
	var issuerBalance float64
	tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`,
		issuerID).Scan(&issuerBalance)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		issuerID, win32TimeNow(), CourierRefType, issuerID, acceptorID,
		0.0, issuerBalance, JournalReason("Courier package delivered")); err != nil {
		log.Printf("[db] DeliverCourierMissionDB: best-effort write failed: %v", err)
	}

	// Mark completed.
	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		UPDATE couriermissions SET state = ?, dateCompleted = ?
		WHERE missionID = ?`,
		CourierStateDelivered, now, missionID); err != nil {
		return fmt.Errorf("DeliverCourier update: %w", err)
	}

	return tx.Commit()
}

// ── cancel ──────────────────────────────────────────────────────────────────

// CancelCourierMission cancels a posted (state=0) contract. Returns items and
// escrowed reward to the issuer. The listing fee is NOT refunded.
func CancelCourierMission(ctx context.Context, db *sql.DB, missionID, issuerID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("CancelCourier begin: %w", err)
	}
	defer tx.Rollback()

	var state int
	var storedIssuer, containerID, pickupStation int64
	var reward float64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, crateContainerID, pickupStationID, rewardEscrow
		FROM couriermissions WHERE missionID = ? FOR UPDATE`,
		missionID).Scan(&state, &storedIssuer, &containerID, &pickupStation, &reward); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("courier contract %d not found", missionID)
		}
		return fmt.Errorf("CancelCourier lock: %w", err)
	}
	if state != CourierStatePosted {
		return fmt.Errorf("courier contract %d cannot be cancelled (state=%d)", missionID, state)
	}
	if storedIssuer != issuerID {
		return fmt.Errorf("only the issuer can cancel this contract")
	}

	// Return items to issuer's hangar at pickup station.
	if containerID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			WHERE locationID = ?`,
			issuerID, pickupStation, flagHangar, containerID); err != nil {
			return fmt.Errorf("CancelCourier return items: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
			return fmt.Errorf("CancelCourier delete container: %w", err)
		}
	}

	// Refund the reward (listing fee is kept).
	if reward > 0 {
		var balance float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
			issuerID).Scan(&balance); err != nil {
			return fmt.Errorf("CancelCourier balance: %w", err)
		}
		newBalance := balance + reward
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = ? WHERE characterID = ?`,
			newBalance, issuerID); err != nil {
			return fmt.Errorf("CancelCourier refund: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			issuerID, win32TimeNow(), CourierRefType, issuerID, issuerID,
			reward, newBalance, JournalReason("Courier contract cancelled — reward refunded")); err != nil {
			log.Printf("[db] CancelCourierMission: best-effort write failed: %v", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE couriermissions SET state = ?, dateCompleted = ?
		 WHERE missionID = ?`,
		CourierStateCancelled, time.Now().Unix(), missionID); err != nil {
		return fmt.Errorf("CancelCourier update: %w", err)
	}

	return tx.Commit()
}

// ── fail ────────────────────────────────────────────────────────────────────

// FailCourierMission fails an accepted contract. Returns items to the issuer
// at the pickup station and transfers the collateral to the issuer.
func FailCourierMission(ctx context.Context, db *sql.DB, missionID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("FailCourier begin: %w", err)
	}
	defer tx.Rollback()

	var state int
	var issuerID, acceptorID, containerID, craterID, pickupStation int64
	var collateral float64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, acceptorID, crateContainerID, craterID,
		       pickupStationID, collateral
		FROM couriermissions WHERE missionID = ? FOR UPDATE`,
		missionID).Scan(&state, &issuerID, &acceptorID, &containerID, &craterID,
		&pickupStation, &collateral); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("courier contract %d not found", missionID)
		}
		return fmt.Errorf("FailCourier lock: %w", err)
	}
	if state != CourierStateAccepted {
		return fmt.Errorf("courier contract %d is not in transit (state=%d)", missionID, state)
	}

	// Return escrowed items to issuer at pickup.
	if containerID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			WHERE locationID = ?`,
			issuerID, pickupStation, flagHangar, containerID); err != nil {
			return fmt.Errorf("FailCourier return items: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
			return fmt.Errorf("FailCourier delete container: %w", err)
		}
	}

	// Delete the crate entity (may already be destroyed).
	if craterID > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, craterID); err != nil {
			log.Printf("[db] FailCourierMission: best-effort write failed: %v", err)
		}
	}

	// Transfer collateral to issuer.
	if collateral > 0 {
		var issuerBal float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
			issuerID).Scan(&issuerBal); err != nil {
			return fmt.Errorf("FailCourier issuer balance: %w", err)
		}
		newIssuerBal := issuerBal + collateral
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = ? WHERE characterID = ?`,
			newIssuerBal, issuerID); err != nil {
			return fmt.Errorf("FailCourier credit issuer: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			issuerID, win32TimeNow(), CourierRefType, issuerID, acceptorID,
			collateral, newIssuerBal,
			JournalReason("Courier contract failed — collateral received")); err != nil {
			log.Printf("[db] FailCourierMission: best-effort write failed: %v", err)
		}

		// Journal for acceptor: collateral forfeited.
		var accBal float64
		tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ?`,
			acceptorID).Scan(&accBal)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			acceptorID, win32TimeNow(), CourierRefType, acceptorID, issuerID,
			-collateral, accBal,
			JournalReason("Courier contract failed — collateral forfeited")); err != nil {
			log.Printf("[db] FailCourierMission: best-effort write failed: %v", err)
		}
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE couriermissions SET state = ?, dateCompleted = ?
		 WHERE missionID = ?`,
		CourierStateFailed, now, missionID); err != nil {
		return fmt.Errorf("FailCourier update: %w", err)
	}

	return tx.Commit()
}

// ── expire (unaccepted) ─────────────────────────────────────────────────────

// ExpireCourierMission expires an unaccepted contract. Returns items and
// escrowed reward to the issuer at the pickup station.
func ExpireCourierMission(ctx context.Context, db *sql.DB, missionID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ExpireCourier begin: %w", err)
	}
	defer tx.Rollback()

	var state int
	var issuerID, containerID, pickupStation int64
	var reward float64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, crateContainerID, pickupStationID, rewardEscrow
		FROM couriermissions WHERE missionID = ? FOR UPDATE`,
		missionID).Scan(&state, &issuerID, &containerID, &pickupStation, &reward); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("courier contract %d not found", missionID)
		}
		return fmt.Errorf("ExpireCourier lock: %w", err)
	}
	if state != CourierStatePosted {
		return fmt.Errorf("courier contract %d is not posted (state=%d)", missionID, state)
	}

	// Return items.
	if containerID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			WHERE locationID = ?`,
			issuerID, pickupStation, flagHangar, containerID); err != nil {
			return fmt.Errorf("ExpireCourier return items: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
			return fmt.Errorf("ExpireCourier delete container: %w", err)
		}
	}

	// Refund reward.
	if reward > 0 {
		var balance float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
			issuerID).Scan(&balance); err != nil {
			return fmt.Errorf("ExpireCourier balance: %w", err)
		}
		newBalance := balance + reward
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = ? WHERE characterID = ?`,
			newBalance, issuerID); err != nil {
			return fmt.Errorf("ExpireCourier refund: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
			issuerID, win32TimeNow(), CourierRefType, issuerID, issuerID,
			reward, newBalance, JournalReason("Courier contract expired — reward refunded")); err != nil {
			log.Printf("[db] ExpireCourierMission: best-effort write failed: %v", err)
		}
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE couriermissions SET state = ?, dateCompleted = ?
		 WHERE missionID = ?`,
		CourierStateExpired, now, missionID); err != nil {
		return fmt.Errorf("ExpireCourier update: %w", err)
	}

	return tx.Commit()
}

// ── sweep ───────────────────────────────────────────────────────────────────

// SweepExpiredCouriers finds and processes all expired courier contracts.
// Posted contracts past their expiry → expire. Accepted contracts past their
// expiry → fail (collateral forfeited).
func SweepExpiredCouriers(ctx context.Context, db *sql.DB) (expired, failed int, err error) {
	now := time.Now().Unix()

	// Expire unaccepted.
	rows, err := db.QueryContext(ctx,
		`SELECT missionID FROM couriermissions
		 WHERE state = ? AND dateExpires < ? AND kind = 1`,
		CourierStatePosted, now)
	if err != nil {
		return 0, 0, fmt.Errorf("SweepExpiredCouriers posted: %w", err)
	}
	var expireIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, 0, err
		}
		expireIDs = append(expireIDs, id)
	}
	rows.Close()

	for _, id := range expireIDs {
		if err := ExpireCourierMission(ctx, db, id); err != nil {
			log.Printf("[courier-sweep] expire %d: %v", id, err)
			continue
		}
		expired++
	}

	// Fail accepted but expired.
	rows, err = db.QueryContext(ctx,
		`SELECT missionID FROM couriermissions
		 WHERE state = ? AND dateExpires < ? AND kind = 1`,
		CourierStateAccepted, now)
	if err != nil {
		return expired, 0, fmt.Errorf("SweepExpiredCouriers accepted: %w", err)
	}
	var failIDs []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return expired, 0, err
		}
		failIDs = append(failIDs, id)
	}
	rows.Close()

	for _, id := range failIDs {
		if err := FailCourierMission(ctx, db, id); err != nil {
			log.Printf("[courier-sweep] fail %d: %v", id, err)
			continue
		}
		failed++
	}

	return expired, failed, nil
}

// ── queries ─────────────────────────────────────────────────────────────────

// ListAvailableCouriers lists courier contracts available at a station.
func ListAvailableCouriers(ctx context.Context, db *sql.DB, stationID int64) ([]CourierListing, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, issuerID, pickupStationID, dropStationID,
		       totalReward, collateral, volume, dateIssued, dateExpires, description
		FROM couriermissions
		WHERE state = ? AND kind = 1 AND pickupStationID = ? AND dateExpires > ?`,
		CourierStatePosted, stationID, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("ListAvailableCouriers: %w", err)
	}
	defer rows.Close()

	var out []CourierListing
	for rows.Next() {
		var l CourierListing
		if err := rows.Scan(&l.MissionID, &l.IssuerID, &l.PickupStationID,
			&l.DropStationID, &l.TotalReward, &l.Collateral, &l.Volume,
			&l.DateIssued, &l.DateExpires, &l.Description); err != nil {
			return nil, fmt.Errorf("ListAvailableCouriers scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListAllAvailableCouriers lists all available courier contracts in the game.
// Used by bot scanning.
func ListAllAvailableCouriers(ctx context.Context, db *sql.DB) ([]CourierListing, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, issuerID, pickupStationID, dropStationID,
		       totalReward, collateral, volume, dateIssued, dateExpires, description
		FROM couriermissions
		WHERE state = ? AND kind = 1 AND dateExpires > ?`,
		CourierStatePosted, time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("ListAllAvailableCouriers: %w", err)
	}
	defer rows.Close()

	var out []CourierListing
	for rows.Next() {
		var l CourierListing
		if err := rows.Scan(&l.MissionID, &l.IssuerID, &l.PickupStationID,
			&l.DropStationID, &l.TotalReward, &l.Collateral, &l.Volume,
			&l.DateIssued, &l.DateExpires, &l.Description); err != nil {
			return nil, fmt.Errorf("ListAllAvailableCouriers scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ListAvailableCouriersFiltered lists available courier contracts matching the
// client's browse filters. The client sends all five; zero/nil means unfiltered
// on that axis.
func ListAvailableCouriersFiltered(ctx context.Context, db *sql.DB,
	maxVolume, maxCollateral float64, regionID, solsysID, stationID int64) ([]CourierListing, error) {

	q := `SELECT missionID, issuerID, pickupStationID, dropStationID,
	             totalReward, collateral, volume, dateIssued, dateExpires, description
	      FROM couriermissions
	      WHERE state = ? AND kind = 1 AND dateExpires > ?`
	args := []any{CourierStatePosted, time.Now().Unix()}

	if stationID > 0 {
		q += ` AND pickupStationID = ?`
		args = append(args, stationID)
	} else if solsysID > 0 {
		q += ` AND pickupSolarSystemID = ?`
		args = append(args, solsysID)
	} else if regionID > 0 {
		q += ` AND pickupRegionID = ?`
		args = append(args, regionID)
	}
	if maxVolume > 0 {
		q += ` AND volume <= ?`
		args = append(args, maxVolume)
	}
	if maxCollateral > 0 {
		q += ` AND collateral <= ?`
		args = append(args, maxCollateral)
	}

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListAvailableCouriersFiltered: %w", err)
	}
	defer rows.Close()

	var out []CourierListing
	for rows.Next() {
		var l CourierListing
		if err := rows.Scan(&l.MissionID, &l.IssuerID, &l.PickupStationID,
			&l.DropStationID, &l.TotalReward, &l.Collateral, &l.Volume,
			&l.DateIssued, &l.DateExpires, &l.Description); err != nil {
			return nil, fmt.Errorf("ListAvailableCouriersFiltered scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetCourierContainerID returns the crateContainerID for a courier mission.
// Mirrors GetEscrowContainerID for the courier table.
func GetCourierContainerID(ctx context.Context, db *sql.DB, missionID int64) (int64, error) {
	var containerID int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(crateContainerID, 0) FROM couriermissions WHERE missionID = ? AND kind = 1`,
		missionID).Scan(&containerID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("GetCourierContainerID: %w", err)
	}
	return containerID, nil
}

// CourierCrateEscrowID returns the escrow container ID for a courier crate
// entity. Returns (0, false) if the entity is not a courier crate.
func CourierCrateEscrowID(ctx context.Context, db *sql.DB, entityID int64) (int64, bool) {
	var escrowID int64
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(cm.crateContainerID, 0)
		FROM entity e
		JOIN couriermissions cm ON cm.craterID = e.itemID
		WHERE e.itemID = ?
		  AND e.customInfo LIKE 'courier\_crate\_%'
		  AND cm.state = ?`, entityID, CourierStateAccepted).Scan(&escrowID)
	if err != nil || escrowID == 0 {
		return 0, false
	}
	return escrowID, true
}

// CourierPackageItems lists a courier package's contents for its container window. The goods
// live in the escrow box but every row reports the package as its locationID, because
// LootCargoView.IsMine keeps only rows located at the window's own itemID.
func CourierPackageItems(ctx context.Context, db *sql.DB, crateID, escrowID int64) (marshal.PyRep, error) {
	return containerItemsAs(ctx, db, escrowID, crateID)
}

// CourierPackageVolume is a courier package's volume: the contract's, since
// that is what the package holds. ok is false for anything that is not a live
// package.
func CourierPackageVolume(ctx context.Context, db *sql.DB, itemID int64) (vol float64, ok bool) {
	err := db.QueryRowContext(ctx, `
		SELECT volume FROM couriermissions
		WHERE craterID = ? AND state = ?`, itemID, CourierStateAccepted).Scan(&vol)
	return vol, err == nil
}

// ItemVolume is the per-unit volume of one item: the type's volume, except for a courier package,
// whose contract volume applies (its type volume is a placeholder). Use it wherever a move is
// checked against a hold's capacity.
func ItemVolume(ctx context.Context, db *sql.DB, itemID, typeID int64) float64 {
	if typeID == TypePlasticWrap {
		if vol, ok := CourierPackageVolume(ctx, db, itemID); ok {
			return vol
		}
	}
	vol, _ := GetTypeVolume(ctx, db, typeID)
	return vol
}

// CourierSealed reports whether an item is escrowed goods inside a courier or
// escrow listing. Those items belong to the issuer and are shown to the
// acceptor inside the package; the client is not the gate, so every move
// path refuses them.
func CourierSealed(ctx context.Context, db *sql.DB, itemID int64) bool {
	var one int
	err := db.QueryRowContext(ctx, `
		SELECT 1 FROM entity e
		JOIN entity box ON box.itemID = e.locationID
		WHERE e.itemID = ? AND box.customInfo = 'courier_escrow'`, itemID).Scan(&one)
	return err == nil
}

// manifestDepth caps how far GetCourierManifest walks into nested items: crate,
// ship, the ship's rack and cargo, a can in that cargo.
const manifestDepth = 4

// manifestRow is one GetCourierManifest row with the parent it sits in.
type manifestRow struct {
	ParentID  int64
	Singleton bool
	Item      ManifestItem
}

// GetCourierManifest lists the items in a courier escrow container, each one
// followed by what sits inside it, so a crated fitted ship shows its rack,
// loaded charges and cargo. The client draws the list flat (missionentry.py
// Inspect and ClaimEscrow) and has no fitted marker.
func GetCourierManifest(ctx context.Context, db *sql.DB, containerID int64) ([]ManifestItem, error) {
	if containerID == 0 {
		return nil, nil
	}
	var all []manifestRow
	parents := []int64{containerID}
	for depth := 0; depth < manifestDepth && len(parents) > 0; depth++ {
		level, err := manifestLevel(ctx, db, parents)
		if err != nil {
			return nil, err
		}
		all = append(all, level...)
		// Only an assembled item (ship, container) holds anything.
		parents = parents[:0]
		for _, r := range level {
			if r.Singleton {
				parents = append(parents, r.Item.ItemID)
			}
		}
	}
	return orderManifestTree(containerID, all), nil
}

// manifestLevel reads the items directly inside the given parents, ordered by
// parent, slot flag, then category so a loaded charge follows its module.
func manifestLevel(ctx context.Context, db *sql.DB, parents []int64) ([]manifestRow, error) {
	args := make([]any, len(parents))
	for i, id := range parents {
		args[i] = id
	}
	rows, err := db.QueryContext(ctx, `
		SELECT e.locationID, e.singleton,
		       e.itemID, e.typeID, e.ownerID, e.quantity,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0),
		       COALESCE(b.copy, 0), COALESCE(b.materialLevel, 0),
		       COALESCE(b.productivityLevel, 0),
		       COALESCE(b.licensedProductionRunsRemaining, 0)
		FROM entity e
		LEFT JOIN invTypes      t ON t.typeID      = e.typeID
		LEFT JOIN invGroups     g ON g.groupID     = t.groupID
		LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		WHERE e.locationID IN (`+placeholders(len(parents))+`)
		ORDER BY e.locationID, e.flag, COALESCE(g.categoryID, 0), e.itemID`, args...)
	if err != nil {
		return nil, fmt.Errorf("GetCourierManifest: %w", err)
	}
	defer rows.Close()

	var out []manifestRow
	for rows.Next() {
		var r manifestRow
		var singleton int64
		m := &r.Item
		if err := rows.Scan(&r.ParentID, &singleton,
			&m.ItemID, &m.TypeID, &m.OwnerID, &m.Quantity,
			&m.GroupID, &m.CategoryID,
			&m.Copy, &m.MaterialLevel, &m.ProductivityLevel, &m.LicensedRuns); err != nil {
			return nil, fmt.Errorf("GetCourierManifest scan: %w", err)
		}
		r.Singleton = singleton != 0
		out = append(out, r)
	}
	return out, rows.Err()
}

// orderManifestTree flattens rows depth-first from rootID: each item, then its
// contents, keeping the query order among siblings. An item reached twice is
// listed once.
func orderManifestTree(rootID int64, rows []manifestRow) []ManifestItem {
	children := map[int64][]ManifestItem{}
	for _, r := range rows {
		children[r.ParentID] = append(children[r.ParentID], r.Item)
	}
	out := make([]ManifestItem, 0, len(rows))
	seen := map[int64]bool{rootID: true}
	var walk func(id int64)
	walk = func(id int64) {
		for _, m := range children[id] {
			if seen[m.ItemID] {
				continue
			}
			seen[m.ItemID] = true
			out = append(out, m)
			walk(m.ItemID)
		}
	}
	walk(rootID)
	return out
}

// GetMyCourierContracts returns all player courier contracts where the character
// is either issuer or acceptor, excluding old completed ones (> 7 days).
func GetMyCourierContracts(ctx context.Context, db *sql.DB, charID int64) ([]CourierRow, error) {
	cutoff := time.Now().Unix() - 7*86400
	rows, err := db.QueryContext(ctx, `
		SELECT missionID, state, COALESCE(issuerID,0), COALESCE(acceptorID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropStationID,0),
		       totalReward, acceptFee, collateral, volume,
		       dateIssued, dateExpires, dateAccepted, dateCompleted,
		       description, COALESCE(craterID,0), COALESCE(crateContainerID,0)
		FROM couriermissions
		WHERE kind = 1 AND (issuerID = ? OR acceptorID = ?)
		  AND (state IN (?, ?) OR dateCompleted > ?)
		ORDER BY dateIssued DESC`,
		charID, charID,
		CourierStatePosted, CourierStateAccepted, cutoff)
	if err != nil {
		return nil, fmt.Errorf("GetMyCourierContracts: %w", err)
	}
	defer rows.Close()

	var out []CourierRow
	for rows.Next() {
		var r CourierRow
		if err := rows.Scan(&r.MissionID, &r.State, &r.IssuerID, &r.AcceptorID,
			&r.PickupStationID, &r.DropStationID,
			&r.TotalReward, &r.AcceptFee, &r.Collateral, &r.Volume,
			&r.DateIssued, &r.DateExpires, &r.DateAccepted, &r.DateCompleted,
			&r.Description, &r.CraterID, &r.CrateContainerID); err != nil {
			return nil, fmt.Errorf("GetMyCourierContracts scan: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetCourierByID returns a single courier contract by its ID.
func GetCourierByID(ctx context.Context, db *sql.DB, missionID int64) (*CourierRow, error) {
	var r CourierRow
	err := db.QueryRowContext(ctx, `
		SELECT missionID, state, COALESCE(issuerID,0), COALESCE(acceptorID,0),
		       COALESCE(pickupStationID,0), COALESCE(dropStationID,0),
		       totalReward, acceptFee, collateral, volume,
		       dateIssued, dateExpires, dateAccepted, dateCompleted,
		       description, COALESCE(craterID,0), COALESCE(crateContainerID,0)
		FROM couriermissions WHERE missionID = ? AND kind = 1`,
		missionID).Scan(&r.MissionID, &r.State, &r.IssuerID, &r.AcceptorID,
		&r.PickupStationID, &r.DropStationID,
		&r.TotalReward, &r.AcceptFee, &r.Collateral, &r.Volume,
		&r.DateIssued, &r.DateExpires, &r.DateAccepted, &r.DateCompleted,
		&r.Description, &r.CraterID, &r.CrateContainerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetCourierByID: %w", err)
	}
	return &r, nil
}

// CheckCrateDestroyed looks for a crate entity that should exist for an
// accepted contract. If the entity is missing, the contract should be failed.
// Returns true if the crate is destroyed (missing).
func CheckCrateDestroyed(ctx context.Context, db *sql.DB, crateID int64) (bool, error) {
	if crateID == 0 {
		return false, nil
	}
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM entity WHERE itemID = ?`, crateID).Scan(&exists)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, fmt.Errorf("CheckCrateDestroyed: %w", err)
	}
	return false, nil
}

// GetItemVolume returns the volume of an item type.
func GetItemVolume(ctx context.Context, db *sql.DB, typeID int64) (float64, error) {
	var vol float64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(volume, 0) FROM invTypes WHERE typeID = ?`, typeID).Scan(&vol)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return vol, err
}

// FailCourierCratesOnShip checks whether the dead ship was carrying any
// courier crates (state=Accepted) and fails those contracts immediately.
// Called from the death handler after spawnDeathLoot.
func FailCourierCratesOnShip(ctx context.Context, db *sql.DB, acceptorID, shipDBID int64) int {
	// Find accepted missions for this acceptor whose crate entity no longer exists
	// (spawnDeathLoot already deleted/moved the children).
	rows, err := db.QueryContext(ctx, `
		SELECT cm.missionID
		FROM couriermissions cm
		WHERE cm.acceptorID = ? AND cm.state = ? AND cm.kind = 1
		  AND cm.crateContainerID IS NOT NULL
		  AND NOT EXISTS (
			SELECT 1 FROM entity e WHERE e.itemID = cm.crateContainerID
		  )`, acceptorID, CourierStateAccepted)
	if err != nil {
		log.Printf("[courier-death] query for %d: %v", acceptorID, err)
		return 0
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0
		}
		ids = append(ids, id)
	}
	rows.Close()

	failed := 0
	for _, id := range ids {
		if err := FailCourierMission(ctx, db, id); err != nil {
			log.Printf("[courier-death] fail mission %d: %v", id, err)
			continue
		}
		log.Printf("[courier-death] mission %d failed — crate destroyed with ship %d", id, shipDBID)
		failed++
	}
	return failed
}

// ── ESP admin ───────────────────────────────────────────────────────────────

// ESPCourierRow is a courier contract with resolved names for the admin panel.
type ESPCourierRow struct {
	CourierRow
	IssuerName    string
	AcceptorName  string
	PickupStation string
	DropStation   string
	StateLabel    string
	IssuerIsBot   bool
	AcceptorIsBot bool
}

// ESPListCouriers returns all courier contracts, optionally filtered by state.
// stateFilter -1 means all states.
func ESPListCouriers(ctx context.Context, db *sql.DB, stateFilter int) ([]ESPCourierRow, error) {
	where := "WHERE cm.kind = 1"
	var args []any
	if stateFilter >= 0 {
		where += " AND cm.state = ?"
		args = append(args, stateFilter)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT cm.missionID, cm.state, COALESCE(cm.issuerID,0), COALESCE(cm.acceptorID,0),
		       COALESCE(cm.pickupStationID,0), COALESCE(cm.dropStationID,0),
		       cm.totalReward, cm.acceptFee, cm.collateral, cm.volume,
		       cm.dateIssued, cm.dateExpires, cm.dateAccepted, cm.dateCompleted,
		       cm.description, COALESCE(cm.craterID,0), COALESCE(cm.crateContainerID,0),
		       COALESCE(ci.characterName, ''), COALESCE(ca.characterName, ''),
		       COALESCE(sp.stationName, ''), COALESCE(sd.stationName, ''),
		       COALESCE(cm.issuerIsBot, 0), COALESCE(cm.acceptorIsBot, 0)
		FROM couriermissions cm
		LEFT JOIN character_ ci ON ci.characterID = cm.issuerID
		LEFT JOIN character_ ca ON ca.characterID = cm.acceptorID
		LEFT JOIN staStations sp ON sp.stationID = cm.pickupStationID
		LEFT JOIN staStations sd ON sd.stationID = cm.dropStationID
		`+where+`
		ORDER BY cm.dateIssued DESC
		LIMIT 500`, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListCouriers: %w", err)
	}
	defer rows.Close()

	stateLabels := map[int]string{
		CourierStatePosted:    "Posted",
		CourierStateAccepted:  "Accepted",
		CourierStateDelivered: "Delivered",
		CourierStateFailed:    "Failed",
		CourierStateExpired:   "Expired",
		CourierStateCancelled: "Cancelled",
	}

	var out []ESPCourierRow
	for rows.Next() {
		var r ESPCourierRow
		var issuerBot, acceptorBot int
		if err := rows.Scan(&r.MissionID, &r.State, &r.IssuerID, &r.AcceptorID,
			&r.PickupStationID, &r.DropStationID,
			&r.TotalReward, &r.AcceptFee, &r.Collateral, &r.Volume,
			&r.DateIssued, &r.DateExpires, &r.DateAccepted, &r.DateCompleted,
			&r.Description, &r.CraterID, &r.CrateContainerID,
			&r.IssuerName, &r.AcceptorName,
			&r.PickupStation, &r.DropStation,
			&issuerBot, &acceptorBot); err != nil {
			return nil, fmt.Errorf("ESPListCouriers scan: %w", err)
		}
		r.IssuerIsBot = issuerBot != 0
		r.AcceptorIsBot = acceptorBot != 0
		r.StateLabel = stateLabels[r.State]
		if r.StateLabel == "" {
			r.StateLabel = fmt.Sprintf("Unknown(%d)", r.State)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPCourierStats returns summary statistics for the admin panel.
type ESPCourierStats struct {
	Total         int
	Posted        int
	Accepted      int
	Delivered     int
	Failed        int
	TotalReward   float64
	TotalCollat   float64
	BotIssued     int
	BotAccepted   int
}

func ESPGetCourierStats(ctx context.Context, db *sql.DB) ESPCourierStats {
	var s ESPCourierStats
	db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       SUM(CASE WHEN state = 0 THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 1 THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 2 THEN 1 ELSE 0 END),
		       SUM(CASE WHEN state = 3 THEN 1 ELSE 0 END),
		       COALESCE(SUM(totalReward), 0),
		       COALESCE(SUM(CASE WHEN state = 1 THEN collateral ELSE 0 END), 0),
		       SUM(CASE WHEN issuerIsBot = 1 THEN 1 ELSE 0 END),
		       SUM(CASE WHEN acceptorIsBot = 1 THEN 1 ELSE 0 END)
		FROM couriermissions WHERE kind = 1`,
	).Scan(&s.Total, &s.Posted, &s.Accepted, &s.Delivered, &s.Failed,
		&s.TotalReward, &s.TotalCollat, &s.BotIssued, &s.BotAccepted)
	return s
}

// EnsureCourierSchema runs the schema migration inline if the columns are
// missing. Called at boot as a safety net in case the SQL file was not loaded.
func EnsureCourierSchema(ctx context.Context, db *sql.DB) error {
	// Check if the collateral column exists.
	var count int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.COLUMNS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
		AND COLUMN_NAME = 'collateral'`).Scan(&count)
	if err != nil {
		return fmt.Errorf("EnsureCourierSchema probe: %w", err)
	}
	if count == 0 {
		migrateCourierColumns(ctx, db)
	}
	return ensureCourierPackages(ctx, db)
}

// ensureCourierPackages keeps the package side of the schema current. Every
// step is idempotent and cheap on an already-migrated database.
func ensureCourierPackages(ctx context.Context, db *sql.DB) error {
	// craterID is how a package finds its contract (bind, List, volume on every cargo move), so it
	// is indexed.
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM INFORMATION_SCHEMA.STATISTICS
		WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'couriermissions'
		AND INDEX_NAME = 'idx_courier_crater'`).Scan(&n); err != nil {
		return fmt.Errorf("ensureCourierPackages probe: %w", err)
	}
	if n == 0 {
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE couriermissions ADD INDEX idx_courier_crater (craterID)`); err != nil {
			return fmt.Errorf("ensureCourierPackages index: %w", err)
		}
	}
	// Re-type packages issued as Cargo Containers (27,500 m3 regardless of contents) to Plastic
	// Wrap. Joined from the contract side so the update walks entity by primary key.
	res, err := db.ExecContext(ctx, `
		UPDATE entity e JOIN couriermissions cm ON cm.craterID = e.itemID
		SET e.typeID = ?
		WHERE e.typeID = ?`, TypePlasticWrap, TypeCargoContainer)
	if err != nil {
		return fmt.Errorf("ensureCourierPackages retype: %w", err)
	}
	if k, _ := res.RowsAffected(); k > 0 {
		log.Printf("[db] courier: rewrapped %d package(s) as Plastic Wrap", k)
	}
	// Load in-flight bot packages aboard the acceptor's current ship, since delivery checks the
	// package location. Matches nothing once contracts accepted before that check have run out.
	res, err = db.ExecContext(ctx, `
		UPDATE entity pkg
		JOIN couriermissions cm ON cm.craterID = pkg.itemID
		JOIN entity pilot ON pilot.itemID = cm.acceptorID
		JOIN entity ship ON ship.itemID = pilot.locationID
		JOIN invTypes st ON st.typeID = ship.typeID
		JOIN invGroups sg ON sg.groupID = st.groupID AND sg.categoryID = 6
		SET pkg.locationID = ship.itemID, pkg.flag = 5
		WHERE cm.state = ? AND cm.acceptorIsBot = 1
		  AND pkg.locationID = cm.pickupStationID`, CourierStateAccepted)
	if err != nil {
		return fmt.Errorf("ensureCourierPackages load in-flight: %w", err)
	}
	if k, _ := res.RowsAffected(); k > 0 {
		log.Printf("[db] courier: loaded %d in-flight bot package(s) aboard", k)
	}
	return retagCourierJournal(ctx, db)
}

// courierJournalRetagged marks the one-shot retag below as done; market_journal has no
// refTypeID index, so the scan must not repeat each boot.
const courierJournalRetagged = "courier.journalRetagged"

// retagCourierJournal moves journal rows written under refType 46 ("Broker fee") or 47
// ("Corporation Bulk Payment") onto CourierRefType and EscrowRefType. Rows are matched on the
// reason text this code writes, so genuine broker fees are untouched.
func retagCourierJournal(ctx context.Context, db *sql.DB) error {
	if _, done, err := GetServerSetting(ctx, db, courierJournalRetagged); err != nil || done {
		return err
	}
	res, err := db.ExecContext(ctx, `
		UPDATE market_journal SET refTypeID = ?
		WHERE refTypeID = 46
		  AND (reason LIKE 'Courier %' OR reason = 'Escrow listing fee')`, CourierRefType)
	if err != nil {
		return fmt.Errorf("retagCourierJournal courier: %w", err)
	}
	nCourier, _ := res.RowsAffected()
	res, err = db.ExecContext(ctx, `
		UPDATE market_journal SET refTypeID = ?
		WHERE refTypeID = 47 AND reason IN ('Escrow purchase', 'Escrow sale')`, EscrowRefType)
	if err != nil {
		return fmt.Errorf("retagCourierJournal escrow: %w", err)
	}
	nEscrow, _ := res.RowsAffected()
	log.Printf("[db] courier: retagged %d courier and %d escrow journal row(s)", nCourier, nEscrow)
	return SetServerSetting(ctx, db, courierJournalRetagged, "1")
}

func migrateCourierColumns(ctx context.Context, db *sql.DB) {
	log.Printf("[db] Running courier schema migration inline")
	migrations := []string{
		`ALTER TABLE couriermissions ADD COLUMN collateral DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE couriermissions ADD COLUMN rewardEscrow DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE couriermissions ADD COLUMN listingFee DOUBLE NOT NULL DEFAULT 0`,
		`ALTER TABLE couriermissions ADD COLUMN crateContainerID INT UNSIGNED DEFAULT NULL`,
		`ALTER TABLE couriermissions ADD COLUMN issuerCorpID INT UNSIGNED DEFAULT NULL`,
		`ALTER TABLE couriermissions ADD COLUMN acceptorCorpID INT UNSIGNED DEFAULT NULL`,
		`ALTER TABLE couriermissions ADD COLUMN issuerIsBot TINYINT(1) NOT NULL DEFAULT 0`,
		`ALTER TABLE couriermissions ADD COLUMN acceptorIsBot TINYINT(1) NOT NULL DEFAULT 0`,
		`ALTER TABLE couriermissions ADD INDEX idx_courier_pickup_state (pickupStationID, state)`,
		`ALTER TABLE couriermissions ADD INDEX idx_courier_acceptor_state (acceptorID, state)`,
		`ALTER TABLE couriermissions ADD INDEX idx_courier_issuer_state (issuerID, state)`,
		`ALTER TABLE couriermissions ADD INDEX idx_courier_state (state)`,
	}
	for _, q := range migrations {
		if _, err := db.ExecContext(ctx, q); err != nil {
			log.Printf("[db] courier migration warning: %v (query: %s)", err, q[:min(len(q), 80)])
		}
	}
}

