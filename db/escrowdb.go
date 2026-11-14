package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

// ── constants ───────────────────────────────────────────────────────────────

// EscrowRefType is the journal refTypeID for an escrow sale, shipped refType 1 "Player
// Trading". The listing fee journals under CourierRefType.
const EscrowRefType = 1

// EscrowIDBase is the first escrowID. Escrow and courier IDs share one client namespace, so the
// bands must stay disjoint for IsEscrowListing to route. See docs/db-10-notes.md#escrow-listings.
const EscrowIDBase = 1000000000

// ── types ───────────────────────────────────────────────────────────────────

// EscrowListing is one available escrow listing for the browse panel.
type EscrowListing struct {
	MissionID   int64
	IssuerID    int64
	StationID   int64
	Price       float64 // stored in escrows.collateral
	DateExpires int64   // unix
	Description string
	AssigneeID  int64 // 0 = public
	SolarSysID  int64
	RegionID    int64
}

// EscrowCreateParams bundles everything InsertEscrow needs.
type EscrowCreateParams struct {
	IssuerID            int64
	IssuerCorpID        int64
	PickupStationID     int64
	Collateral          float64
	ListingFee          float64
	Volume              float64
	Description         string
	DaysToExpire        int
	ContainerID         int64
	IssuerIsBot         bool
	PickupSolarSystemID int64
	PickupRegionID      int64
	AssigneeID          int64 // 0 = public
}

// ── queries ─────────────────────────────────────────────────────────────────

// ListAvailableEscrows returns escrow listings matching the filters. Every filter is optional;
// 0 means no filter.
func ListAvailableEscrows(ctx context.Context, db *sql.DB,
	maxPrice float64, regionID, sysID, stationID, ownerID, charID, corpID int64,
) ([]EscrowListing, error) {

	q := `SELECT escrowID, issuerID, pickupStationID, collateral,
	             dateExpires, description, COALESCE(assigneeID, 0),
	             pickupSolarSystemID, pickupRegionID
	        FROM escrows
	       WHERE state = 0 AND dateExpires > ?`
	args := []any{time.Now().Unix()}

	if maxPrice > 0 {
		q += ` AND collateral <= ?`
		args = append(args, maxPrice)
	}
	if regionID > 0 {
		q += ` AND pickupRegionID = ?`
		args = append(args, regionID)
	}
	if sysID > 0 {
		q += ` AND pickupSolarSystemID = ?`
		args = append(args, sysID)
	}
	if stationID > 0 {
		q += ` AND pickupStationID = ?`
		args = append(args, stationID)
	}
	if ownerID > 0 {
		q += ` AND issuerID = ?`
		args = append(args, ownerID)
	}
	// Visibility: public (assigneeID IS NULL) or assigned to this char/corp.
	q += ` AND (assigneeID IS NULL`
	if charID > 0 {
		q += ` OR assigneeID = ?`
		args = append(args, charID)
	}
	if corpID > 0 {
		q += ` OR assigneeID = ?`
		args = append(args, corpID)
	}
	q += `)`
	q += ` ORDER BY dateExpires ASC LIMIT 200`

	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("ListAvailableEscrows: %w", err)
	}
	defer rows.Close()

	var out []EscrowListing
	for rows.Next() {
		var l EscrowListing
		if err := rows.Scan(&l.MissionID, &l.IssuerID, &l.StationID,
			&l.Price, &l.DateExpires, &l.Description, &l.AssigneeID,
			&l.SolarSysID, &l.RegionID); err != nil {
			return nil, fmt.Errorf("ListAvailableEscrows scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// ResolveEscrowNames bulk-resolves owner IDs to names for the escrow browse.
func ResolveEscrowNames(ctx context.Context, db *sql.DB, ids []int64) map[int64]string {
	names := make(map[int64]string, len(ids))
	if len(ids) == 0 {
		return names
	}
	for _, id := range ids {
		if _, ok := names[id]; ok || id == 0 {
			continue
		}
		var name string
		// Try character first, then corporation, then station.
		err := db.QueryRowContext(ctx,
			`SELECT characterName FROM character_ WHERE characterID = ?`, id).Scan(&name)
		if err == nil {
			names[id] = name
			continue
		}
		err = db.QueryRowContext(ctx,
			`SELECT corporationName FROM corporation WHERE corporationID = ?`, id).Scan(&name)
		if err == nil {
			names[id] = name
			continue
		}
		err = db.QueryRowContext(ctx,
			`SELECT stationName FROM staStations WHERE stationID = ?`, id).Scan(&name)
		if err == nil {
			names[id] = name
		}
	}
	return names
}

// ── insert ──────────────────────────────────────────────────────────────────

// InsertEscrow creates a new escrow listing. The caller has already
// escrowed the items and debited the issuer's wallet.
func InsertEscrow(ctx context.Context, tx *sql.Tx, p EscrowCreateParams) (int64, error) {
	now := time.Now().Unix()
	expires := now + int64(p.DaysToExpire)*86400

	isBot := 0
	if p.IssuerIsBot {
		isBot = 1
	}

	var assigneeID any
	if p.AssigneeID != 0 {
		assigneeID = p.AssigneeID
	}

	res, err := tx.ExecContext(ctx, `
		INSERT INTO escrows
			(state, description, issuerID, assigneeID, collateral,
			 dateIssued, dateExpires,
			 pickupStationID, pickupSolarSystemID, pickupRegionID,
			 crateContainerID, issuerCorpID, issuerIsBot, listingFee, volume)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		CourierStatePosted, p.Description, p.IssuerID, assigneeID, p.Collateral,
		now, expires,
		p.PickupStationID, p.PickupSolarSystemID, p.PickupRegionID,
		p.ContainerID, p.IssuerCorpID, isBot, p.ListingFee, p.Volume)
	if err != nil {
		return 0, fmt.Errorf("InsertEscrow: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsertEscrow LastInsertId: %w", err)
	}
	return id, nil
}

// ── claim ───────────────────────────────────────────────────────────────────

// ClaimEscrowMission executes a one-step escrow purchase: the buyer pays the price, the items
// transfer to the buyer and the ISK credits the seller. It returns the itemIDs moved into the
// buyer's hangar.
func ClaimEscrowMission(ctx context.Context, sqldb *sql.DB,
	escrowID, buyerCharID, buyerCorpID, buyerStationID int64, buyerIsBot bool,
) (movedItemIDs []int64, err error) {
	tx, err := sqldb.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ClaimEscrow begin: %w", err)
	}
	defer tx.Rollback()

	var state int
	var issuerID, pickupStationID, containerID int64
	var assigneeID sql.NullInt64
	var price float64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, pickupStationID, crateContainerID,
		       collateral, assigneeID
		FROM escrows WHERE escrowID = ? FOR UPDATE`,
		escrowID).Scan(&state, &issuerID, &pickupStationID, &containerID,
		&price, &assigneeID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("escrow listing %d not found", escrowID)
		}
		return nil, fmt.Errorf("ClaimEscrow lock: %w", err)
	}
	if state != CourierStatePosted {
		return nil, fmt.Errorf("escrow listing %d is no longer available (state=%d)", escrowID, state)
	}
	if issuerID == buyerCharID {
		return nil, fmt.Errorf("cannot buy your own escrow listing")
	}
	if buyerStationID != pickupStationID {
		return nil, fmt.Errorf("you must be docked at the listing's station")
	}
	// Assignee check.
	if assigneeID.Valid && assigneeID.Int64 != 0 {
		if assigneeID.Int64 != buyerCharID && assigneeID.Int64 != buyerCorpID {
			return nil, fmt.Errorf("this listing is not available to you")
		}
	}

	// Debit buyer.
	var buyerBal float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
		buyerCharID).Scan(&buyerBal); err != nil {
		return nil, fmt.Errorf("ClaimEscrow buyer balance: %w", err)
	}
	if buyerBal < price {
		return nil, fmt.Errorf("insufficient funds (need %.2f, have %.2f)", price, buyerBal)
	}
	newBuyerBal := buyerBal - price
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = ? WHERE characterID = ?`,
		newBuyerBal, buyerCharID); err != nil {
		return nil, fmt.Errorf("ClaimEscrow debit buyer: %w", err)
	}

	// Credit seller.
	var sellerBal float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`,
		issuerID).Scan(&sellerBal); err != nil {
		return nil, fmt.Errorf("ClaimEscrow seller balance: %w", err)
	}
	newSellerBal := sellerBal + price
	if _, err := tx.ExecContext(ctx,
		`UPDATE character_ SET balance = ? WHERE characterID = ?`,
		newSellerBal, issuerID); err != nil {
		return nil, fmt.Errorf("ClaimEscrow credit seller: %w", err)
	}

	// Collect item IDs from the container before moving them.
	const flagHangar = 4
	if containerID > 0 {
		rows, qErr := tx.QueryContext(ctx,
			`SELECT itemID FROM entity WHERE locationID = ?`, containerID)
		if qErr == nil {
			for rows.Next() {
				var id int64
				if rows.Scan(&id) == nil {
					movedItemIDs = append(movedItemIDs, id)
				}
			}
			rows.Close()
		}

		// Move items from escrow container to buyer's hangar.
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			WHERE locationID = ?`,
			buyerCharID, pickupStationID, flagHangar, containerID); err != nil {
			return nil, fmt.Errorf("ClaimEscrow move items: %w", err)
		}
		// Everything inside a delivered item changes owner with it (see reownNested), because dogma and
		// cargo lists filter on ownerID.
		if err := reownNested(ctx, tx, movedItemIDs, buyerCharID); err != nil {
			return nil, fmt.Errorf("ClaimEscrow re-own contents: %w", err)
		}
		// Delete the escrow container.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
			return nil, fmt.Errorf("ClaimEscrow delete container: %w", err)
		}
	}

	// Journal, buyer.
	now := win32TimeNow()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		buyerCharID, now, EscrowRefType, buyerCharID, issuerID,
		-price, newBuyerBal, JournalReason("Escrow purchase")); err != nil {
		log.Printf("[db] ClaimEscrowMission: best-effort write failed: %v", err)
	}

	// Journal, seller.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		issuerID, now, EscrowRefType, issuerID, buyerCharID,
		price, newSellerBal, JournalReason("Escrow sale")); err != nil {
		log.Printf("[db] ClaimEscrowMission: best-effort write failed: %v", err)
	}

	// Mark completed.
	isBotVal := 0
	if buyerIsBot {
		isBotVal = 1
	}
	unixNow := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		UPDATE escrows
		SET state = ?, acceptorID = ?, acceptorCorpID = ?, acceptorIsBot = ?,
		    dateAccepted = ?, dateCompleted = ?
		WHERE escrowID = ?`,
		CourierStateDelivered, buyerCharID, buyerCorpID, isBotVal,
		unixNow, unixNow, escrowID); err != nil {
		return nil, fmt.Errorf("ClaimEscrow update: %w", err)
	}

	return movedItemIDs, tx.Commit()
}

// reownEscrowDepth bounds the walk (crate, ship, container in its cargo, that container's
// contents), so a location cycle in bad data cannot spin the claim.
const reownEscrowDepth = 6

// reownNested sets ownerID on everything located at any depth inside the items in roots.
// Locations and flags are left alone.
func reownNested(ctx context.Context, tx *sql.Tx, roots []int64, ownerID int64) error {
	level := roots
	for depth := 0; depth < reownEscrowDepth && len(level) > 0; depth++ {
		args := make([]any, 0, len(level)+1)
		args = append(args, ownerID)
		for _, id := range level {
			args = append(args, id)
		}
		in := "?" + strings.Repeat(",?", len(level)-1)
		rows, err := tx.QueryContext(ctx,
			`SELECT itemID FROM entity WHERE locationID IN (`+in+`)`, args[1:]...)
		if err != nil {
			return err
		}
		var next []int64
		for rows.Next() {
			var id int64
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return err
			}
			next = append(next, id)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		if len(next) == 0 {
			return nil
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE entity SET ownerID = ? WHERE locationID IN (`+in+`)`, args...); err != nil {
			return err
		}
		level = next
	}
	return nil
}

// ── expire ──────────────────────────────────────────────────────────────────

// ExpireEscrow expires an unaccepted escrow listing and returns the items to the issuer at the
// pickup station. The listing fee is not refunded.
func ExpireEscrow(ctx context.Context, db *sql.DB, escrowID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ExpireEscrow begin: %w", err)
	}
	defer tx.Rollback()

	var state int
	var issuerID, containerID, pickupStation int64
	if err := tx.QueryRowContext(ctx, `
		SELECT state, issuerID, crateContainerID, pickupStationID
		FROM escrows WHERE escrowID = ? FOR UPDATE`,
		escrowID).Scan(&state, &issuerID, &containerID, &pickupStation); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("escrow listing %d not found", escrowID)
		}
		return fmt.Errorf("ExpireEscrow lock: %w", err)
	}
	if state != CourierStatePosted {
		return fmt.Errorf("escrow listing %d is not posted (state=%d)", escrowID, state)
	}

	// Return items.
	if containerID > 0 {
		if _, err := tx.ExecContext(ctx, `
			UPDATE entity SET ownerID = ?, locationID = ?, flag = ?
			WHERE locationID = ?`,
			issuerID, pickupStation, 4, containerID); err != nil {
			return fmt.Errorf("ExpireEscrow return items: %w", err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM entity WHERE itemID = ?`, containerID); err != nil {
			return fmt.Errorf("ExpireEscrow delete container: %w", err)
		}
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx,
		`UPDATE escrows SET state = ?, dateCompleted = ?
		 WHERE escrowID = ?`,
		CourierStateExpired, now, escrowID); err != nil {
		return fmt.Errorf("ExpireEscrow update: %w", err)
	}

	return tx.Commit()
}

// SweepExpiredEscrows expires unaccepted listings past their deadline. Items return to the
// issuer; the listing fee is not refunded.
func SweepExpiredEscrows(ctx context.Context, db *sql.DB) (int, error) {
	now := time.Now().Unix()
	rows, err := db.QueryContext(ctx,
		`SELECT escrowID FROM escrows
		 WHERE state = ? AND dateExpires < ?`,
		CourierStatePosted, now)
	if err != nil {
		return 0, fmt.Errorf("SweepExpiredEscrows: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		ids = append(ids, id)
	}
	rows.Close()

	expired := 0
	for _, id := range ids {
		if err := ExpireEscrow(ctx, db, id); err != nil {
			log.Printf("[escrow-sweep] expire %d: %v", id, err)
			continue
		}
		expired++
	}
	return expired, nil
}

// ── schema ──────────────────────────────────────────────────────────────────

// EnsureEscrowSchema creates the escrows table if it does not exist.
// Idempotent; called at boot.
func EnsureEscrowSchema(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS escrows (
		  escrowID            INT UNSIGNED NOT NULL AUTO_INCREMENT,
		  state               TINYINT      NOT NULL DEFAULT 0,
		  description         VARCHAR(255) NOT NULL DEFAULT '',
		  issuerID            BIGINT       NOT NULL,
		  assigneeID          BIGINT       DEFAULT NULL,
		  collateral          DOUBLE       NOT NULL DEFAULT 0,
		  dateIssued          BIGINT       NOT NULL DEFAULT 0,
		  dateExpires         BIGINT       NOT NULL DEFAULT 0,
		  dateAccepted        BIGINT       NOT NULL DEFAULT 0,
		  dateCompleted       BIGINT       NOT NULL DEFAULT 0,
		  pickupStationID     BIGINT       NOT NULL DEFAULT 0,
		  pickupSolarSystemID BIGINT       NOT NULL DEFAULT 0,
		  pickupRegionID      BIGINT       NOT NULL DEFAULT 0,
		  crateContainerID    INT UNSIGNED DEFAULT NULL,
		  issuerCorpID        INT UNSIGNED DEFAULT NULL,
		  acceptorID          BIGINT       DEFAULT NULL,
		  acceptorCorpID      INT UNSIGNED DEFAULT NULL,
		  issuerIsBot         TINYINT(1)   NOT NULL DEFAULT 0,
		  acceptorIsBot       TINYINT(1)   NOT NULL DEFAULT 0,
		  listingFee          DOUBLE       NOT NULL DEFAULT 0,
		  volume              DOUBLE       NOT NULL DEFAULT 0,
		  PRIMARY KEY (escrowID),
		  INDEX idx_escrow_state_station (state, pickupStationID),
		  INDEX idx_escrow_state_region  (state, pickupRegionID),
		  INDEX idx_escrow_state_system  (state, pickupSolarSystemID)
		) ENGINE=InnoDB AUTO_INCREMENT=1000000000 DEFAULT CHARSET=utf8mb4`)
	if err != nil {
		return fmt.Errorf("EnsureEscrowSchema: %w", err)
	}

	// Move escrow IDs into the EscrowIDBase band. Idempotent; DESC order keeps each row clear of the
	// ones above it. Persisted rolebots escrow IDs from before the move fail with "not found".
	res, err := db.ExecContext(ctx, `
		UPDATE escrows SET escrowID = escrowID + ?
		WHERE escrowID < ? ORDER BY escrowID DESC`,
		EscrowIDBase, EscrowIDBase)
	if err != nil {
		return fmt.Errorf("EnsureEscrowSchema renumber: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("[escrow] moved %d escrow listing(s) into the %d+ ID band", n, EscrowIDBase)
	}
	// InnoDB raises a too-low AUTO_INCREMENT to MAX+1 itself, so this is safe every boot.
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE escrows AUTO_INCREMENT = %d`, EscrowIDBase)); err != nil {
		return fmt.Errorf("EnsureEscrowSchema auto_increment: %w", err)
	}

	return nil
}

// ── ESP admin ───────────────────────────────────────────────────────────────

// ESPEscrowRow is an escrow listing with resolved names for the admin panel.
type ESPEscrowRow struct {
	MissionID    int64
	State        int
	IssuerID     int64
	AcceptorID   int64
	StationID    int64
	Price        float64
	DateIssued   int64
	DateExpires  int64
	DateComplete int64
	Description  string
	IssuerName   string
	AcceptorName string
	StationName  string
	StateLabel   string
	IssuerIsBot  bool
}

// ESPEscrowStats holds summary stats for the escrow admin panel.
type ESPEscrowStats struct {
	Total       int
	Posted      int
	Claimed     int
	Expired     int
	Cancelled   int
	TotalVolume float64 // total ISK traded
	BotIssued   int
}

func ESPGetEscrowStats(ctx context.Context, db *sql.DB) ESPEscrowStats {
	var s ESPEscrowStats
	db.QueryRowContext(ctx, `
		SELECT COUNT(*),
		       COALESCE(SUM(CASE WHEN state = 0 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN state = 2 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN state = 4 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN state = 5 THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN state = 2 THEN collateral ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN issuerIsBot = 1 THEN 1 ELSE 0 END), 0)
		FROM escrows`,
	).Scan(&s.Total, &s.Posted, &s.Claimed, &s.Expired, &s.Cancelled,
		&s.TotalVolume, &s.BotIssued)
	return s
}

func ESPListEscrows(ctx context.Context, db *sql.DB, stateFilter int) ([]ESPEscrowRow, error) {
	where := "WHERE 1=1"
	var args []any
	if stateFilter >= 0 {
		where += " AND e.state = ?"
		args = append(args, stateFilter)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT e.escrowID, e.state, COALESCE(e.issuerID,0), COALESCE(e.acceptorID,0),
		       COALESCE(e.pickupStationID,0), e.collateral,
		       e.dateIssued, e.dateExpires, e.dateCompleted,
		       e.description,
		       COALESCE(ci.characterName, ''), COALESCE(ca.characterName, ''),
		       COALESCE(sp.stationName, ''),
		       COALESCE(e.issuerIsBot, 0)
		FROM escrows e
		LEFT JOIN character_ ci ON ci.characterID = e.issuerID
		LEFT JOIN character_ ca ON ca.characterID = e.acceptorID
		LEFT JOIN staStations sp ON sp.stationID = e.pickupStationID
		`+where+`
		ORDER BY e.dateIssued DESC
		LIMIT 500`, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListEscrows: %w", err)
	}
	defer rows.Close()

	stateLabels := map[int]string{
		CourierStatePosted:    "Listed",
		CourierStateDelivered: "Claimed",
		CourierStateExpired:   "Expired",
		CourierStateCancelled: "Cancelled",
	}

	var out []ESPEscrowRow
	for rows.Next() {
		var r ESPEscrowRow
		var issuerBot int
		if err := rows.Scan(&r.MissionID, &r.State, &r.IssuerID, &r.AcceptorID,
			&r.StationID, &r.Price,
			&r.DateIssued, &r.DateExpires, &r.DateComplete,
			&r.Description,
			&r.IssuerName, &r.AcceptorName, &r.StationName,
			&issuerBot); err != nil {
			return nil, fmt.Errorf("ESPListEscrows scan: %w", err)
		}
		r.IssuerIsBot = issuerBot != 0
		r.StateLabel = stateLabels[r.State]
		if r.StateLabel == "" {
			r.StateLabel = fmt.Sprintf("Unknown(%d)", r.State)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListAllAvailableEscrows lists all available escrow listings in the game.
// Used by bot scanning.
func ListAllAvailableEscrows(ctx context.Context, db *sql.DB) ([]EscrowListing, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT escrowID, issuerID, pickupStationID, collateral,
		       dateExpires, description, COALESCE(assigneeID, 0),
		       pickupSolarSystemID, pickupRegionID
		FROM escrows
		WHERE state = 0 AND dateExpires > ?
		  AND assigneeID IS NULL
		ORDER BY collateral DESC`,
		time.Now().Unix())
	if err != nil {
		return nil, fmt.Errorf("ListAllAvailableEscrows: %w", err)
	}
	defer rows.Close()

	var out []EscrowListing
	for rows.Next() {
		var l EscrowListing
		if err := rows.Scan(&l.MissionID, &l.IssuerID, &l.StationID,
			&l.Price, &l.DateExpires, &l.Description, &l.AssigneeID,
			&l.SolarSysID, &l.RegionID); err != nil {
			return nil, fmt.Errorf("ListAllAvailableEscrows scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// GetEscrowContainerItems returns the items inside an escrow container,
// with their typeIDs. Used by bot evaluation of escrow listings.
func GetEscrowContainerItems(ctx context.Context, db *sql.DB, containerID int64) ([]ManifestItem, error) {
	return GetCourierManifest(ctx, db, containerID)
}

// GetEscrowContainerID returns the crateContainerID for an escrow listing.
func GetEscrowContainerID(ctx context.Context, db *sql.DB, escrowID int64) (int64, error) {
	var containerID int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(crateContainerID, 0) FROM escrows WHERE escrowID = ?`,
		escrowID).Scan(&containerID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("escrow listing %d not found", escrowID)
	}
	if err != nil {
		return 0, fmt.Errorf("GetEscrowContainerID: %w", err)
	}
	return containerID, nil
}

// IsEscrowListing checks if the given ID exists in the escrows table.
// Used by the accept dispatcher to route escrow vs courier.
func IsEscrowListing(ctx context.Context, db *sql.DB, escrowID int64) bool {
	var exists int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM escrows WHERE escrowID = ?`, escrowID).Scan(&exists)
	return err == nil
}

// GetEscrowIssuerID returns the issuerID from a completed/active escrow row.
func GetEscrowIssuerID(ctx context.Context, db *sql.DB, escrowID int64) (int64, error) {
	var issuerID int64
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(issuerID, 0) FROM escrows WHERE escrowID = ?`,
		escrowID).Scan(&issuerID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("escrow listing %d not found", escrowID)
	}
	if err != nil {
		return 0, fmt.Errorf("GetEscrowIssuerID: %w", err)
	}
	return issuerID, nil
}
