package db

// Corp office rental: the state behind corpStationMgr.RentOffice / CancelRentOfOffice and
// the station lobby's Offices tab. An office is three rows that stay in step: the
// crpoffices row, the office entity (typeID 27, itemID = officeID) and the station's one
// shared folder entity (typeID 26, itemID = stationID + 6,000,000).
// See docs/db-7-notes.md#corporation-offices.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"

	"gurimoon/marshal"
)

const (
	// TypeOffice is invTypes 27 "Office"; TypeOfficeFolder (26) lives in
	// billsdb.go with the bill constants.
	TypeOffice = 27

	// OfficeFolderOffset maps a stationID to its office folder itemID.
	OfficeFolderOffset = 6000000

	// Office slot flags, eveConstants flagOfficeSlotFirst/Last. 16 slots is the ceiling per
	// station regardless of staStationTypes.officeSlots.
	FlagOfficeSlotFirst = 70
	FlagOfficeSlotLast  = 85

	// FlagCorpHangarDiv1 is division 1 of a corp hangar (plain flagHangar);
	// divisions 2–7 are FlagCorpSAG2..FlagCorpSAG7 = 116..121.
	FlagCorpHangarDiv1 = 4
	FlagCorpSAGFirst   = 116
	FlagCorpSAGLast    = 121
)

// Sentinel errors the corpStationMgr handlers map onto the client's own
// message keys (RentingYouHaveAnOfficeHere, NoOfficesAreAvailableForRenting).
var (
	ErrOfficeExists  = errors.New("corporation already has an office at this station")
	ErrNoFreeOffices = errors.New("no office slots are available at this station")
)

// OfficeFolderID returns the office folder itemID for a station.
func OfficeFolderID(stationID int64) int64 { return stationID + OfficeFolderOffset }

// IsOfficeFolderID mirrors the client's cfg.IsOfficeFolder band test.
func IsOfficeFolderID(itemID int64) bool {
	return itemID >= 66000000 && itemID < 68000000
}

// IsCorpHangarFlag reports whether flag is a corp hangar division flag.
func IsCorpHangarFlag(flag int64) bool {
	return flag == FlagCorpHangarDiv1 || (flag >= FlagCorpSAGFirst && flag <= FlagCorpSAGLast)
}

// OfficeRentQuote is what corpStationMgr.GetQuoteForRentingAnOffice answers and what the
// rent sweep bills: staStations.officeRentalCost floored by the bills.officeRentCost
// setting, so the handful of zero-cost stations do not rent for free.
func OfficeRentQuote(ctx context.Context, db *sql.DB, stationID int64) float64 {
	floor := OfficeRentCost(ctx, db)
	var c float64
	if err := db.QueryRowContext(ctx,
		`SELECT officeRentalCost FROM staStations WHERE stationID = ?`, stationID,
	).Scan(&c); err != nil || c < floor {
		return floor
	}
	return c
}

// OfficeRentQuoteFor is OfficeRentQuote for one renting corporation, after a
// player owner's Office Rental cost modifier. The quote, RentOffice and the
// monthly bill all use it.
func OfficeRentQuoteFor(ctx context.Context, db *sql.DB, stationID, corpID int64) float64 {
	return math.Round(StationServiceFeeForCorp(ctx, db, stationID, corpID, StationServiceOfficeRental,
		OfficeRentQuote(ctx, db, stationID)))
}

// GetCorpOfficeAt returns the corp's office at a station, or (0, 0) when it rents none.
// A corp that owns the station counts as having an office: the station itself stands in
// (the virtual row GetStationCorpOffices serves), so an owner never rents a redundant slot.
func GetCorpOfficeAt(ctx context.Context, db *sql.DB, corpID, stationID int64) (officeID, folderID int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT itemID, officeFolderID FROM crpoffices
		 WHERE corporationID = ? AND stationID = ?`, corpID, stationID,
	).Scan(&officeID, &folderID)
	if errors.Is(err, sql.ErrNoRows) {
		var owner int64
		if e := db.QueryRowContext(ctx,
			`SELECT corporationID FROM staStations WHERE stationID = ?`, stationID,
		).Scan(&owner); e == nil && owner == corpID {
			return stationID, 0, nil
		}
		return 0, 0, nil
	}
	if err != nil {
		return 0, 0, fmt.Errorf("GetCorpOfficeAt: %w", err)
	}
	return officeID, folderID, nil
}

// IsOfficeItemID reports whether itemID is a rented office (a crpoffices row). Errors
// read as false, so the inventory bind falls through to the generic path.
func IsOfficeItemID(ctx context.Context, db *sql.DB, itemID int64) bool {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM crpoffices WHERE itemID = ?`, itemID).Scan(&n)
	return err == nil
}

// GetOfficeByItemID returns the crpoffices row for an office itemID.
func GetOfficeByItemID(ctx context.Context, db *sql.DB, itemID int64) (corpID, stationID, folderID int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT corporationID, stationID, officeFolderID FROM crpoffices WHERE itemID = ?`,
		itemID).Scan(&corpID, &stationID, &folderID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("GetOfficeByItemID: %w", err)
	}
	return corpID, stationID, folderID, nil
}

// stationOfficeCapacity is how many offices a station can hold:
// staStationTypes.officeSlots clamped to the 16 slot flags, 16 when the
// column is NULL or zero (most types ship without it).
func stationOfficeCapacity(ctx context.Context, q rowQueryer, stationID int64) int64 {
	var slots sql.NullInt64
	_ = q.QueryRowContext(ctx, `
		SELECT st.officeSlots FROM staStations s
		JOIN staStationTypes st ON st.stationTypeID = s.stationTypeID
		WHERE s.stationID = ?`, stationID).Scan(&slots)
	max := int64(FlagOfficeSlotLast - FlagOfficeSlotFirst + 1)
	if !slots.Valid || slots.Int64 <= 0 || slots.Int64 > max {
		return max
	}
	return slots.Int64
}

// rowQueryer is the subset of DBTX office helpers need for reads.
type rowQueryer interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// OfficeSlotFlag is the inventory flag of office slot number n (1-based).
func OfficeSlotFlag(n int64) int64 { return FlagOfficeSlotFirst + n - 1 }

// StationOfficeCapacity is stationOfficeCapacity for callers outside a transaction.
func StationOfficeCapacity(ctx context.Context, db *sql.DB, stationID int64) int64 {
	return stationOfficeCapacity(ctx, db, stationID)
}

// privateOfficeSlotFlags is the slot flags a player owner has taken off the
// market (stationOfficeSlots.publiclyAvailable = 0), while
// station.enforceManagement is on. A missing table reads as none.
func privateOfficeSlotFlags(ctx context.Context, q rowQueryer, stationID int64) map[int64]bool {
	out := map[int64]bool{}
	if !StationManagementEnforced(ctx, nil) {
		return out
	}
	rows, err := q.QueryContext(ctx,
		`SELECT slotNumber FROM stationOfficeSlots WHERE stationID = ? AND publiclyAvailable = 0`, stationID)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var n int64
		if rows.Scan(&n) == nil {
			out[OfficeSlotFlag(n)] = true
		}
	}
	return out
}

// freeOfficeSlotFlag picks the lowest unused slot flag at a station, or
// ErrNoFreeOffices when the capacity is exhausted. A renter (ownerUse false)
// never gets a slot the owner made private.
func freeOfficeSlotFlag(ctx context.Context, q rowQueryer, stationID int64, ownerUse bool) (int64, error) {
	capacity := stationOfficeCapacity(ctx, q, stationID)
	private := map[int64]bool{}
	if !ownerUse {
		private = privateOfficeSlotFlags(ctx, q, stationID)
	}
	rows, err := q.QueryContext(ctx, `
		SELECT e.flag FROM crpoffices o
		JOIN entity e ON e.itemID = o.itemID
		WHERE o.stationID = ?`, stationID)
	if err != nil {
		return 0, fmt.Errorf("freeOfficeSlotFlag: %w", err)
	}
	defer rows.Close()
	used := map[int64]bool{}
	for rows.Next() {
		var f int64
		if err := rows.Scan(&f); err != nil {
			return 0, fmt.Errorf("freeOfficeSlotFlag scan: %w", err)
		}
		used[f] = true
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	if int64(len(used)) >= capacity {
		return 0, ErrNoFreeOffices
	}
	for f := int64(FlagOfficeSlotFirst); f < FlagOfficeSlotFirst+capacity; f++ {
		if !used[f] && !private[f] {
			return f, nil
		}
	}
	return 0, ErrNoFreeOffices
}

// RentOffice is the whole rent transaction: charge the corp wallet, pay the station
// owner, journal both sides (refType 13), create the folder entity on a station's first
// office, create the office entity in a free slot, insert the crpoffices row and write
// the paid rental-bill receipt. Committed before the CallRsp: the client refetches.
func RentOffice(ctx context.Context, db *sql.DB, corpID, stationID int64, cost float64) (officeID, folderID int64, err error) {
	var stationOwner int64
	var stationName string
	if err := db.QueryRowContext(ctx,
		`SELECT corporationID, stationName FROM staStations WHERE stationID = ?`, stationID,
	).Scan(&stationOwner, &stationName); err != nil {
		return 0, 0, fmt.Errorf("RentOffice station %d: %w", stationID, err)
	}
	if err := EnsureBillTables(ctx, db); err != nil {
		return 0, 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("RentOffice begin: %w", err)
	}
	defer tx.Rollback()

	// Re-check under the transaction: two directors can click at once.
	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT itemID FROM crpoffices WHERE corporationID = ? AND stationID = ?`,
		corpID, stationID).Scan(&existing)
	if err == nil {
		return 0, 0, ErrOfficeExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("RentOffice check: %w", err)
	}

	slotFlag, err := freeOfficeSlotFlag(ctx, tx, stationID, false)
	if err != nil {
		return 0, 0, err
	}

	// Debit the corp wallet.
	var balance float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&balance); err != nil {
		return 0, 0, fmt.Errorf("RentOffice corp %d: %w", corpID, err)
	}
	if balance < cost {
		return 0, 0, fmt.Errorf("your corporation needs %.0f ISK to rent this office and has %.2f", cost, balance)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE corporation SET balance = balance - ? WHERE corporationID = ?`,
		cost, corpID); err != nil {
		return 0, 0, fmt.Errorf("RentOffice debit: %w", err)
	}
	now := win32TimeNow()
	reason := JournalReason(fmt.Sprintf("Office rental fee at %s", stationName))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		corpID, now, RefOfficeRentalFee, corpID, stationOwner,
		-cost, balance-cost, reason); err != nil {
		return 0, 0, fmt.Errorf("RentOffice journal debtor: %w", err)
	}

	// Credit the station owner. A corp does not pay itself for its own station; the handler
	// refuses that case earlier via GetCorpOfficeAt.
	if stationOwner != 0 && stationOwner != corpID {
		if _, err := tx.ExecContext(ctx,
			`UPDATE corporation SET balance = balance + ? WHERE corporationID = ?`,
			cost, stationOwner); err != nil {
			return 0, 0, fmt.Errorf("RentOffice credit: %w", err)
		}
		var ownerBalance float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM corporation WHERE corporationID = ?`, stationOwner,
		).Scan(&ownerBalance); err == nil {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO market_journal
				  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
				   accountID, amount, balance, reason, accountKey)
				VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
				stationOwner, now, RefOfficeRentalFee, corpID, stationOwner,
				cost, ownerBalance, reason); err != nil {
				return 0, 0, fmt.Errorf("RentOffice journal creditor: %w", err)
			}
		}
	}

	// The station's office folder: one per station, deterministic itemID.
	folderID = OfficeFolderID(stationID)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemID, itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, 'Office Folder', ?, ?, ?, 0, 0, 1, 1, 0, 0, 0, NULL)
		ON DUPLICATE KEY UPDATE itemID = itemID`,
		folderID, TypeOfficeFolder, stationOwner, stationID); err != nil {
		return 0, 0, fmt.Errorf("RentOffice folder: %w", err)
	}

	// The office itself.
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Office', ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, NULL)`,
		TypeOffice, corpID, folderID, slotFlag)
	if err != nil {
		return 0, 0, fmt.Errorf("RentOffice office: %w", err)
	}
	officeID, err = res.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("RentOffice office id: %w", err)
	}

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO crpoffices (corporationID, stationID, itemID, typeID, officeFolderID)
		VALUES (?, ?, ?, ?, ?)`,
		corpID, stationID, officeID, TypeOffice, folderID); err != nil {
		return 0, 0, fmt.Errorf("RentOffice crpoffices: %w", err)
	}

	// Paid receipt: keeps OfficesDueForRent from billing again for 30 days.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO bills
		  (billTypeID, debtorID, creditorID, amount, dueDateTime, interest, externalID, paid, externalID2, issued)
		VALUES (?, ?, ?, ?, ?, 0, ?, 1, ?, ?)`,
		BillTypeRentalBill, corpID, stationOwner, cost, now,
		TypeOfficeFolder, stationID, now); err != nil {
		return 0, 0, fmt.Errorf("RentOffice receipt: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("RentOffice commit: %w", err)
	}
	return officeID, folderID, nil
}

// ErrNotStationOwner is OpenOwnerOffice refusing a station the corp does not own.
var ErrNotStationOwner = errors.New("corporation does not own this station")

// OpenOwnerOffice gives the corp that owns a station a real office there: the folder,
// the office entity in a free slot and the crpoffices row, as RentOffice writes them,
// with no charge, journal or bill. OfficesDueForRent never bills an owner's office.
// The virtual row GetStationCorpOffices serves stands in until this runs; its hangar is
// the station's own location, which personal hangars share, so nothing stocks it.
// See docs/db-7-notes.md#owner-offices.
func OpenOwnerOffice(ctx context.Context, db *sql.DB, corpID, stationID int64) (officeID, folderID int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice begin: %w", err)
	}
	defer tx.Rollback()

	var owner int64
	if err := tx.QueryRowContext(ctx,
		`SELECT corporationID FROM staStations WHERE stationID = ?`, stationID,
	).Scan(&owner); err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice station %d: %w", stationID, err)
	}
	if owner != corpID || corpID == 0 {
		return 0, 0, ErrNotStationOwner
	}
	var existing int64
	err = tx.QueryRowContext(ctx,
		`SELECT itemID FROM crpoffices WHERE corporationID = ? AND stationID = ?`,
		corpID, stationID).Scan(&existing)
	if err == nil {
		return 0, 0, ErrOfficeExists
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return 0, 0, fmt.Errorf("OpenOwnerOffice check: %w", err)
	}
	slotFlag, err := freeOfficeSlotFlag(ctx, tx, stationID, true)
	if err != nil {
		return 0, 0, err
	}

	folderID = OfficeFolderID(stationID)
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemID, itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, 'Office Folder', ?, ?, ?, 0, 0, 1, 1, 0, 0, 0, NULL)
		ON DUPLICATE KEY UPDATE itemID = itemID`,
		folderID, TypeOfficeFolder, owner, stationID); err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice folder: %w", err)
	}
	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Office', ?, ?, ?, ?, 0, 1, 1, 0, 0, 0, NULL)`,
		TypeOffice, corpID, folderID, slotFlag)
	if err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice office: %w", err)
	}
	officeID, err = res.LastInsertId()
	if err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice office id: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO crpoffices (corporationID, stationID, itemID, typeID, officeFolderID)
		VALUES (?, ?, ?, ?, ?)`,
		corpID, stationID, officeID, TypeOffice, folderID); err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice crpoffices: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, fmt.Errorf("OpenOwnerOffice commit: %w", err)
	}
	return officeID, folderID, nil
}

// ReleaseOffice tears an office down. Contents are dumped to the station floor as
// "office junk" (locationID = the station, flag 4, still corp-owned, recoverable through
// the corp-junk path). The office entity and crpoffices row are deleted; the shared
// folder stays. Used by CancelRentOfOffice and the unpaid-rent eviction sweep.
func ReleaseOffice(ctx context.Context, db *sql.DB, corpID, stationID int64) (officeID, folderID, contents int64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice begin: %w", err)
	}
	defer tx.Rollback()

	err = tx.QueryRowContext(ctx,
		`SELECT itemID, officeFolderID FROM crpoffices
		 WHERE corporationID = ? AND stationID = ?`, corpID, stationID,
	).Scan(&officeID, &folderID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, 0, nil
	}
	if err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice lookup: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = ? WHERE locationID = ?`,
		stationID, FlagCorpHangarDiv1, officeID)
	if err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice junk: %w", err)
	}
	contents, _ = res.RowsAffected()

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, officeID); err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice entity: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM crpoffices WHERE corporationID = ? AND stationID = ?`,
		corpID, stationID); err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice row: %w", err)
	}
	// A Base is one of the corporation's offices; the headquarters keeps its members.
	// Online members' session baseID refreshes at their next role push or login.
	if _, err := tx.ExecContext(ctx, `
		UPDATE chrCorporationRoles r
		  JOIN character_ c ON c.characterID = r.characterID
		   SET r.baseID = NULL
		 WHERE c.corporationID = ? AND r.baseID = ?
		   AND NOT EXISTS (SELECT 1 FROM corporation h WHERE h.corporationID = ? AND h.stationID = ?)`,
		corpID, stationID, corpID, stationID); err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice bases: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, 0, 0, fmt.Errorf("ReleaseOffice commit: %w", err)
	}
	return officeID, folderID, contents, nil
}

// CorpJunkCountAt counts a corp's impounded items at a station: corp-owned items on the
// station floor at flag 4, where ReleaseOffice dumps office contents. Nothing else
// parks corp items there (deliveries are flag 62).
func CorpJunkCountAt(ctx context.Context, db *sql.DB, corpID, stationID int64) int64 {
	var n int64
	_ = db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM entity
		WHERE ownerID = ? AND locationID = ? AND flag = ?`,
		corpID, stationID, FlagCorpHangarDiv1).Scan(&n)
	return n
}

// ReturnCorpJunk charges the fee (corp wallet to station owner, refType 13 in both
// journals) and hands the corp's impounded items to the paying director personally
// (ownerID = charID, same location and flag), because the corp has no office here to
// receive them. Returns the moved itemIDs so the handler can push OnItemChange.
func ReturnCorpJunk(ctx context.Context, db *sql.DB, corpID, stationID, charID int64, cost float64) ([]int64, error) {
	var stationOwner int64
	var stationName string
	if err := db.QueryRowContext(ctx,
		`SELECT corporationID, stationName FROM staStations WHERE stationID = ?`, stationID,
	).Scan(&stationOwner, &stationName); err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk station %d: %w", stationID, err)
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk begin: %w", err)
	}
	defer tx.Rollback()

	rows, err := tx.QueryContext(ctx, `
		SELECT itemID FROM entity
		WHERE ownerID = ? AND locationID = ? AND flag = ?`,
		corpID, stationID, FlagCorpHangarDiv1)
	if err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk list: %w", err)
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, fmt.Errorf("ReturnCorpJunk scan: %w", err)
		}
		ids = append(ids, id)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	var balance float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&balance); err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk corp %d: %w", corpID, err)
	}
	if balance < cost {
		return nil, fmt.Errorf("your corporation needs %.0f ISK to reclaim its property and has %.2f", cost, balance)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE corporation SET balance = balance - ? WHERE corporationID = ?`,
		cost, corpID); err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk debit: %w", err)
	}
	now := win32TimeNow()
	reason := JournalReason(fmt.Sprintf("Repossessed property reclaimed at %s", stationName))
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
		corpID, now, RefOfficeRentalFee, corpID, stationOwner,
		-cost, balance-cost, reason); err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk journal debtor: %w", err)
	}
	if stationOwner != 0 && stationOwner != corpID {
		if _, err := tx.ExecContext(ctx,
			`UPDATE corporation SET balance = balance + ? WHERE corporationID = ?`,
			cost, stationOwner); err != nil {
			return nil, fmt.Errorf("ReturnCorpJunk credit: %w", err)
		}
		var ownerBalance float64
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM corporation WHERE corporationID = ?`, stationOwner,
		).Scan(&ownerBalance); err == nil {
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO market_journal
				  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
				   accountID, amount, balance, reason, accountKey)
				VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`,
				stationOwner, now, RefOfficeRentalFee, corpID, stationOwner,
				cost, ownerBalance, reason); err != nil {
				return nil, fmt.Errorf("ReturnCorpJunk journal creditor: %w", err)
			}
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET ownerID = ?
		WHERE ownerID = ? AND locationID = ? AND flag = ?`,
		charID, corpID, stationID, FlagCorpHangarDiv1); err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk move: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("ReturnCorpJunk commit: %w", err)
	}
	return ids, nil
}

// GetOfficeContents lists a rented office's hangar items as invbroker rows, limited to
// the division flags the caller's roles may query. Nil or empty allowedFlags answers no
// rows; the client shows its own "insufficient rights" hint.
func GetOfficeContents(ctx context.Context, db *sql.DB, officeID int64, allowedFlags []int64) ([][]marshal.PyRep, error) {
	if len(allowedFlags) == 0 {
		return nil, nil
	}
	q := `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       e.contraband, e.singleton, e.quantity, t.groupID, g.categoryID, e.customInfo
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ? AND e.flag IN (`
	args := []any{officeID}
	for i, f := range allowedFlags {
		if i > 0 {
			q += ","
		}
		q += "?"
		args = append(args, f)
	}
	q += `) ORDER BY t.typeID, e.itemID`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("GetOfficeContents: %w", err)
	}
	defer rows.Close()
	return scanInvBrokerRows(rows)
}

// GetOfficeFolderItems lists the office items inside a station's folder, which the
// client's folder.List() primes its cache with before opening a corp hangar window.
func GetOfficeFolderItems(ctx context.Context, db *sql.DB, folderID int64) ([][]marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       e.contraband, e.singleton, e.quantity, t.groupID, g.categoryID, e.customInfo
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ?
		ORDER BY t.typeID, e.itemID`, folderID)
	if err != nil {
		return nil, fmt.Errorf("GetOfficeFolderItems: %w", err)
	}
	defer rows.Close()
	return scanInvBrokerRows(rows)
}

// scanInvBrokerRows folds an 11-column entity join into invbroker item rows
// (the invBrokerItemCols shape List answers everywhere).
func scanInvBrokerRows(rows *sql.Rows) ([][]marshal.PyRep, error) {
	var out [][]marshal.PyRep
	for rows.Next() {
		var itemID, typeID, ownerID, locationID, flag int64
		var contraband, singleton, quantity, groupID, categoryID int64
		var customInfo sql.NullString
		if err := rows.Scan(&itemID, &typeID, &ownerID, &locationID, &flag,
			&contraband, &singleton, &quantity, &groupID, &categoryID, &customInfo); err != nil {
			return nil, fmt.Errorf("scanInvBrokerRows: %w", err)
		}
		out = append(out, []marshal.PyRep{
			intRep(itemID), intRep(typeID), intRep(ownerID), intRep(locationID),
			intRep(flag), intRep(contraband), intRep(singleton), intRep(quantity),
			intRep(groupID), intRep(categoryID), nullStrRep(customInfo),
		})
	}
	return out, rows.Err()
}

// StationOwnerCorp returns the corporation that owns stationID, or 0 when the station is
// unknown. The lobby's ALL HANGARS mode is gated on the caller's corp being this corp,
// the same test bco_locations.DoesCharactersCorpOwnThisStation makes client-side.
func StationOwnerCorp(ctx context.Context, db *sql.DB, stationID int64) int64 {
	var owner int64
	if err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM staStations WHERE stationID = ?`, stationID,
	).Scan(&owner); err != nil {
		return 0
	}
	return owner
}

// MoveItemFollowingContents moves one whole item row to (locationID, flag) as ownerID
// (corp-office deposit and take-out). An assembled ship (category 6, singleton) has its
// contents re-owned in the same transaction: modules, cargo, drones and one level of
// containers, but never the pilot's seat (flag 57). Returns the rows re-owned.
func MoveItemFollowingContents(ctx context.Context, db *sql.DB, itemID, locationID, ownerID, flag int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("MoveItemFollowingContents: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	var categoryID, singleton int64
	if err := tx.QueryRowContext(ctx, `
		SELECT g.categoryID, e.singleton
		FROM entity e
		JOIN invTypes  t ON t.typeID  = e.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.itemID = ? FOR UPDATE`, itemID).Scan(&categoryID, &singleton); err != nil {
		return 0, fmt.Errorf("MoveItemFollowingContents: item %d: %w", itemID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, ownerID = ?, flag = ? WHERE itemID = ?`,
		locationID, ownerID, flag, itemID); err != nil {
		return 0, fmt.Errorf("MoveItemFollowingContents: move %d: %w", itemID, err)
	}
	var moved int64
	if categoryID == 6 && singleton != 0 {
		// One level of containers inside the ship first, then the ship's own contents. A JOIN
		// avoids MariaDB's "can't specify target table" on a self-subquery.
		res, err := tx.ExecContext(ctx, `
			UPDATE entity e
			JOIN entity p ON p.itemID = e.locationID
			SET e.ownerID = ?
			WHERE p.locationID = ? AND p.flag <> 57 AND e.flag <> 57`, ownerID, itemID)
		if err != nil {
			return 0, fmt.Errorf("MoveItemFollowingContents: nested contents of %d: %w", itemID, err)
		}
		n, _ := res.RowsAffected()
		moved += n
		res, err = tx.ExecContext(ctx,
			`UPDATE entity SET ownerID = ? WHERE locationID = ? AND flag <> 57`, ownerID, itemID)
		if err != nil {
			return 0, fmt.Errorf("MoveItemFollowingContents: contents of %d: %w", itemID, err)
		}
		n, _ = res.RowsAffected()
		moved += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("MoveItemFollowingContents: commit: %w", err)
	}
	return moved, nil
}
