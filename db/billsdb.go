package db

// Bills: the state behind the wallet's Bills tabs. The client reads nine attributes
// (wallet.py GetTextForBill): billID, billTypeID, debtorID, creditorID, amount,
// dueDateTime, interest, externalID, externalID2. `paid` is never rendered: payable,
// receivable or gone is decided by which RPC answered, so paid bills are filtered out.
// externalID / externalID2 carry per-type context (market fine: typeID / locationID;
// rental: folder typeID 26 or 28 / stationID; broker: raw / locationID; war: enemy
// ownerID; alliance maintenance: allianceID). The absent value is -1, never NULL: the
// client compares `!= -1`. Paying debits the debtor, credits the creditor and journals
// both wallets in one transaction. War and alliance-maintenance bills are CONCORD's;
// rental bills are the station owner's.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"

	"gurimoon/marshal"
)

// Bill type IDs, matching sql/billtypes.sql. cfg.MakeConstantName turns the five rows into
// const.billType* on the client, so the shipped "Maintainance" spelling must stay.
const (
	BillTypeMarketFine           = 1
	BillTypeRentalBill           = 2
	BillTypeBrokerBill           = 3
	BillTypeWarBill              = 4
	BillTypeAllianceMaintainance = 5
)

// Inventory folder types the rental-bill renderer names.
const (
	TypeOfficeFolder  = 26
	TypeFactoryFolder = 28
)

// Journal ref types for bill payments (sql/eveconstants.sql /
// sql/market_reftypes.sql).
const (
	RefOfficeRentalFee         = 13
	RefFactorySlotRentalFee    = 14
	RefAllianceRegistrationFee = 48
	RefWarFee                  = 49
	RefAllianceMaintainanceFee = 50
)

// OwnerCONCORDBills is CONCORD's ownerID: the creditor for war and alliance-maintenance
// bills and the sender of the letters about them. Same value as OwnerCONCORD in
// standingdb.go, redeclared so the bills layer does not import the standings layer.
const OwnerCONCORDBills = 1000125

// billCols is the header of every bill rowset the client reads.
var billCols = []string{
	"billID", "billTypeID", "debtorID", "creditorID",
	"amount", "dueDateTime", "interest", "externalID", "paid", "externalID2",
}

// Bill is one row of the bills table.
type Bill struct {
	BillID      int64
	BillTypeID  int64
	DebtorID    int64
	CreditorID  int64
	Amount      float64
	DueDateTime int64
	Interest    float64
	ExternalID  int64 // -1 = unknown
	Paid        bool
	ExternalID2 int64 // -1 = unknown
	Issued      int64
}

// EnsureBillTables creates the bills table and registers the constants the client reads off
// config.BulkData.constants: the five billType* names and the two folder types the rental
// renderer compares against. cfg.py raises AttributeError on a missing const, so they exist
// before the first bill row. billTablesOnce keeps this off the read paths (see ensureGuard).
var billTablesOnce ensureGuard

func EnsureBillTables(ctx context.Context, db *sql.DB) error {
	return billTablesOnce.do(func() error { return ensureBillTables(ctx, db) })
}

func ensureBillTables(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS bills (
		  billID      INT UNSIGNED    NOT NULL AUTO_INCREMENT,
		  billTypeID  INT UNSIGNED    NOT NULL,
		  debtorID    INT UNSIGNED    NOT NULL,
		  creditorID  INT UNSIGNED    NOT NULL,
		  amount      DOUBLE          NOT NULL,
		  dueDateTime BIGINT UNSIGNED NOT NULL,
		  interest    DOUBLE          NOT NULL DEFAULT 0,
		  externalID  INT             NOT NULL DEFAULT -1,
		  paid        TINYINT UNSIGNED NOT NULL DEFAULT 0,
		  externalID2 INT             NOT NULL DEFAULT -1,
		  issued      BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  PRIMARY KEY (billID),
		  KEY debtor (debtorID, paid),
		  KEY creditor (creditorID, paid)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return fmt.Errorf("EnsureBillTables: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT IGNORE INTO eveConstants (constantID, constantValue) VALUES
		  ('billTypeMarketFine', 1), ('billTypeRentalBill', 2), ('billTypeBrokerBill', 3),
		  ('billTypeWarBill', 4), ('billTypeAllianceMaintainanceBill', 5),
		  ('typeOfficeFolder', 26), ('typeFactoryFolder', 28)`); err != nil {
		return fmt.Errorf("EnsureBillTables constants: %w", err)
	}
	return nil
}

// GetBillTypesRowset serves billMgr.GetBillTypes from the billtypes table. It must be a
// non-empty util.Rowset: svc.wallet's Run() calls .Index('billTypeID') on it at login and
// GetTextForBill dereferences .billTypeName once a real bill exists.
func GetBillTypesRowset(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT billTypeID, billTypeName, description FROM billtypes ORDER BY billTypeID`)
	if err != nil {
		return nil, fmt.Errorf("GetBillTypesRowset: %w", err)
	}
	defer rows.Close()
	var lines [][]marshal.PyRep
	for rows.Next() {
		var id int64
		var name, desc string
		if err := rows.Scan(&id, &name, &desc); err != nil {
			return nil, fmt.Errorf("GetBillTypesRowset scan: %w", err)
		}
		lines = append(lines, []marshal.PyRep{intRep(id), strRep(name), strRep(desc)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		return nil, fmt.Errorf("billtypes table is empty — load sql/billtypes.sql")
	}
	return makeRowset([]string{"billTypeID", "billTypeName", "description"}, lines), nil
}

// billRowReps renders one bill in billCols order.
func billRowReps(b Bill) []marshal.PyRep {
	extRep := func(v int64) marshal.PyRep {
		// -1 goes out as a real -1 (intRep round-trips through the signed 8-byte encoding);
		// the client compares `!= -1`, so None would take the wrong branch.
		return intRep(v)
	}
	paid := int64(0)
	if b.Paid {
		paid = 1
	}
	return []marshal.PyRep{
		intRep(b.BillID), intRep(b.BillTypeID), intRep(b.DebtorID), intRep(b.CreditorID),
		floatRep(b.Amount), intRep(b.DueDateTime), floatRep(b.Interest),
		extRep(b.ExternalID), intRep(paid), extRep(b.ExternalID2),
	}
}

// GetBillsRowset returns an owner's unpaid bills as util.Rowset. payable selects on the
// debtor, receivable on the creditor; nothing in the row says which list it is.
func GetBillsRowset(ctx context.Context, db *sql.DB, ownerID int64, payable bool) (marshal.PyRep, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, err
	}
	col := "creditorID"
	if payable {
		col = "debtorID"
	}
	rows, err := db.QueryContext(ctx, fmt.Sprintf(`
		SELECT billID, billTypeID, debtorID, creditorID, amount, dueDateTime,
		       interest, externalID, paid, externalID2
		  FROM bills WHERE %s = ? AND paid = 0 ORDER BY dueDateTime`, col), ownerID)
	if err != nil {
		return nil, fmt.Errorf("GetBillsRowset: %w", err)
	}
	defer rows.Close()
	var lines [][]marshal.PyRep
	for rows.Next() {
		b, err := scanBill(rows)
		if err != nil {
			return nil, err
		}
		lines = append(lines, billRowReps(b))
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeRowset(billCols, lines), nil
}

func scanBill(rows *sql.Rows) (Bill, error) {
	var b Bill
	var paid int64
	if err := rows.Scan(&b.BillID, &b.BillTypeID, &b.DebtorID, &b.CreditorID,
		&b.Amount, &b.DueDateTime, &b.Interest, &b.ExternalID, &paid, &b.ExternalID2); err != nil {
		return b, fmt.Errorf("scanBill: %w", err)
	}
	b.Paid = paid != 0
	return b, nil
}

// GetBill reads one bill.
func GetBill(ctx context.Context, db *sql.DB, billID int64) (*Bill, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, err
	}
	var b Bill
	var paid int64
	err := db.QueryRowContext(ctx, `
		SELECT billID, billTypeID, debtorID, creditorID, amount, dueDateTime,
		       interest, externalID, paid, externalID2
		  FROM bills WHERE billID = ?`, billID,
	).Scan(&b.BillID, &b.BillTypeID, &b.DebtorID, &b.CreditorID,
		&b.Amount, &b.DueDateTime, &b.Interest, &b.ExternalID, &paid, &b.ExternalID2)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	} else if err != nil {
		return nil, fmt.Errorf("GetBill: %w", err)
	}
	b.Paid = paid != 0
	return &b, nil
}

// CreateBill issues a bill. A duplicate guard keeps the sweeps idempotent: a second unpaid
// bill with the same type, debtor, creditor, externalID and externalID2 issued within
// dedupWindow ticks is refused (created=false, the existing row returned). externalID2
// is in the key because it names the subject (the office's station, the war).
func CreateBill(ctx context.Context, db *sql.DB, b Bill, dedupWindowTicks uint64) (*Bill, bool, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, false, err
	}
	if dedupWindowTicks > 0 {
		since := win32TimeNow() - dedupWindowTicks
		var existing int64
		err := db.QueryRowContext(ctx, `
			SELECT billID FROM bills
			 WHERE billTypeID = ? AND debtorID = ? AND creditorID = ? AND externalID = ?
			   AND externalID2 = ? AND paid = 0 AND issued > ?
			 LIMIT 1`,
			b.BillTypeID, b.DebtorID, b.CreditorID, b.ExternalID, b.ExternalID2, since).Scan(&existing)
		if err == nil {
			old, gerr := GetBill(ctx, db, existing)
			return old, false, gerr
		} else if !errors.Is(err, sql.ErrNoRows) {
			return nil, false, fmt.Errorf("CreateBill dedup: %w", err)
		}
	}
	paid := int64(0)
	res, err := db.ExecContext(ctx, `
		INSERT INTO bills (billTypeID, debtorID, creditorID, amount, dueDateTime,
		                   interest, externalID, paid, externalID2, issued)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.BillTypeID, b.DebtorID, b.CreditorID, b.Amount, b.DueDateTime,
		b.Interest, b.ExternalID, paid, b.ExternalID2, win32TimeNow())
	if err != nil {
		return nil, false, fmt.Errorf("CreateBill: %w", err)
	}
	b.BillID, _ = res.LastInsertId()
	return &b, true, nil
}

// billRefType maps a bill type to the journal ref type its payment writes.
func billRefType(billTypeID int64) int {
	switch billTypeID {
	case BillTypeRentalBill:
		return RefOfficeRentalFee
	case BillTypeWarBill:
		return RefWarFee
	case BillTypeAllianceMaintainance:
		return RefAllianceMaintainanceFee
	}
	return 6 // refBackwardCompatible, the generic bucket
}

// PayBill settles a bill in one transaction: debtor debited, creditor credited, a journal
// row in each wallet, the bill marked paid. payerCharID names whose wallet pays a
// character bill; a corp debtor pays from the corporation wallet and an alliance debtor
// from its executor corp. Commits before the RPC answers: the client refetches at once.
func PayBill(ctx context.Context, db *sql.DB, billID, payerCharID int64) (*Bill, error) {
	bill, err := GetBill(ctx, db, billID)
	if err != nil {
		return nil, err
	}
	if bill == nil {
		return nil, fmt.Errorf("no such bill")
	}
	if bill.Paid {
		return nil, fmt.Errorf("that bill is already paid")
	}

	// Resolve who actually pays.
	debtorIsChar := IsCharacterIDCtx(ctx, db, bill.DebtorID)
	payingCorpID := int64(0)
	if !debtorIsChar {
		payingCorpID = bill.DebtorID
		// An alliance debtor pays from its executor corporation's wallet.
		var executor int64
		if err := db.QueryRowContext(ctx,
			`SELECT executorCorpID FROM alliances WHERE allianceID = ?`, bill.DebtorID,
		).Scan(&executor); err == nil && executor > 0 {
			payingCorpID = executor
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("PayBill begin: %w", err)
	}
	defer tx.Rollback()

	now := win32TimeNow()
	refType := billRefType(bill.BillTypeID)
	reason := fmt.Sprintf("Bill %d", bill.BillID)

	var debtorBalance float64
	if debtorIsChar {
		if payerCharID != 0 && payerCharID != bill.DebtorID {
			return nil, fmt.Errorf("this bill is not addressed to you")
		}
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ?`, bill.DebtorID,
		).Scan(&debtorBalance); err != nil {
			return nil, fmt.Errorf("PayBill debtor: %w", err)
		}
		if debtorBalance < bill.Amount {
			return nil, fmt.Errorf("you need %.2f ISK to pay this bill and have %.2f", bill.Amount, debtorBalance)
		}
		debtorBalance -= bill.Amount
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = ? WHERE characterID = ?`, debtorBalance, bill.DebtorID); err != nil {
			return nil, fmt.Errorf("PayBill debit: %w", err)
		}
	} else {
		if err := tx.QueryRowContext(ctx,
			`SELECT balance FROM corporation WHERE corporationID = ?`, payingCorpID,
		).Scan(&debtorBalance); err != nil {
			return nil, fmt.Errorf("PayBill debtor corp: %w", err)
		}
		if debtorBalance < bill.Amount {
			return nil, fmt.Errorf("the corporation needs %.2f ISK to pay this bill and has %.2f", bill.Amount, debtorBalance)
		}
		debtorBalance -= bill.Amount
		if _, err := tx.ExecContext(ctx,
			`UPDATE corporation SET balance = balance - ? WHERE corporationID = ?`,
			bill.Amount, payingCorpID); err != nil {
			return nil, fmt.Errorf("PayBill debit corp: %w", err)
		}
	}

	// Credit the creditor. CONCORD and other NPC corps have corporation rows.
	creditorIsChar := IsCharacterIDTx(ctx, tx, bill.CreditorID)
	var creditorBalance float64
	if creditorIsChar {
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`,
			bill.Amount, bill.CreditorID); err != nil {
			return nil, fmt.Errorf("PayBill credit char: %w", err)
		}
		_ = tx.QueryRowContext(ctx,
			`SELECT balance FROM character_ WHERE characterID = ?`, bill.CreditorID).Scan(&creditorBalance)
	} else {
		if _, err := tx.ExecContext(ctx,
			`UPDATE corporation SET balance = balance + ? WHERE corporationID = ?`,
			bill.Amount, bill.CreditorID); err != nil {
			return nil, fmt.Errorf("PayBill credit corp: %w", err)
		}
		_ = tx.QueryRowContext(ctx,
			`SELECT balance FROM corporation WHERE corporationID = ?`, bill.CreditorID).Scan(&creditorBalance)
	}

	// One journal row per wallet. A corp wallet's row is a market_journal row whose
	// characterID is the corporationID, as in every corp-side write.
	journalOwner := bill.DebtorID
	if !debtorIsChar {
		journalOwner = payingCorpID
	}
	const journalSQL = `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, ?, ?, ?, NULL, 0, ?, ?, ?, 1000)`
	if _, err := tx.ExecContext(ctx, journalSQL,
		journalOwner, now, refType, journalOwner, bill.CreditorID,
		-bill.Amount, debtorBalance, reason); err != nil {
		return nil, fmt.Errorf("PayBill debtor journal: %w", err)
	}
	if _, err := tx.ExecContext(ctx, journalSQL,
		bill.CreditorID, now, refType, journalOwner, bill.CreditorID,
		bill.Amount, creditorBalance, reason); err != nil {
		return nil, fmt.Errorf("PayBill creditor journal: %w", err)
	}

	res, err := tx.ExecContext(ctx,
		`UPDATE bills SET paid = 1 WHERE billID = ? AND paid = 0`, billID)
	if err != nil {
		return nil, fmt.Errorf("PayBill mark: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return nil, fmt.Errorf("that bill was just paid")
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("PayBill commit: %w", err)
	}
	bill.Paid = true
	return bill, nil
}

// IsCharacterIDCtx reports whether an ID has a character_ row, the debtor/creditor
// split classifier.
func IsCharacterIDCtx(ctx context.Context, db *sql.DB, id int64) bool {
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterID = ?`, id).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// IsCharacterIDTx is the in-transaction classifier.
func IsCharacterIDTx(ctx context.Context, tx *sql.Tx, id int64) bool {
	var n int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE characterID = ?`, id).Scan(&n); err != nil {
		return false
	}
	return n > 0
}

// OverdueUnpaidBills returns unpaid bills whose due date has passed, for the
// enforcement sweeps (an unpaid war bill ends the war).
func OverdueUnpaidBills(ctx context.Context, db *sql.DB, billTypeID int64) ([]Bill, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT billID, billTypeID, debtorID, creditorID, amount, dueDateTime,
		       interest, externalID, paid, externalID2
		  FROM bills
		 WHERE billTypeID = ? AND paid = 0 AND dueDateTime < ?`,
		billTypeID, win32TimeNow())
	if err != nil {
		return nil, fmt.Errorf("OverdueUnpaidBills: %w", err)
	}
	defer rows.Close()
	var out []Bill
	for rows.Next() {
		b, err := scanBill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// WriteOffBill voids a bill without payment (a war that ended, an alliance
// that dissolved).  The debtor is off the hook; nothing moves.
func WriteOffBill(ctx context.Context, db *sql.DB, billID int64) error {
	_, err := db.ExecContext(ctx, `UPDATE bills SET paid = 1 WHERE billID = ?`, billID)
	if err != nil {
		return fmt.Errorf("WriteOffBill: %w", err)
	}
	return nil
}

// ListOpenBills returns every unpaid bill with both party names resolved, for
// the ESP panel.
type BillView struct {
	Bill
	TypeName     string
	DebtorName   string
	CreditorName string
	Overdue      bool
}

func ListOpenBills(ctx context.Context, db *sql.DB) ([]BillView, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT b.billID, b.billTypeID, b.debtorID, b.creditorID, b.amount, b.dueDateTime,
		       b.interest, b.externalID, b.paid, b.externalID2,
		       COALESCE(t.billTypeName, ''),
		       COALESCE(dc.corporationName, dch.characterName, da.allianceName, ''),
		       COALESCE(cc.corporationName, cch.characterName, so.ownerName, '')
		  FROM bills b
		  LEFT JOIN billtypes t ON t.billTypeID = b.billTypeID
		  LEFT JOIN corporation dc ON dc.corporationID = b.debtorID
		  LEFT JOIN character_ dch ON dch.characterID = b.debtorID
		  LEFT JOIN alliances da ON da.allianceID = b.debtorID
		  LEFT JOIN corporation cc ON cc.corporationID = b.creditorID
		  LEFT JOIN character_ cch ON cch.characterID = b.creditorID
		  LEFT JOIN eveStaticOwners so ON so.ownerID = b.creditorID
		 WHERE b.paid = 0
		 ORDER BY b.dueDateTime`)
	if err != nil {
		return nil, fmt.Errorf("ListOpenBills: %w", err)
	}
	defer rows.Close()

	now := win32TimeNow()
	var out []BillView
	for rows.Next() {
		var v BillView
		var paid int64
		if err := rows.Scan(&v.BillID, &v.BillTypeID, &v.DebtorID, &v.CreditorID,
			&v.Amount, &v.DueDateTime, &v.Interest, &v.ExternalID, &paid, &v.ExternalID2,
			&v.TypeName, &v.DebtorName, &v.CreditorName); err != nil {
			return nil, fmt.Errorf("ListOpenBills scan: %w", err)
		}
		v.Paid = paid != 0
		v.Overdue = uint64(v.DueDateTime) < now
		out = append(out, v)
	}
	return out, rows.Err()
}

// ── Bill settings ────────────────────────────────────────────────────────────

const (
	settingBillGraceDays  = "bills.graceDays"
	settingOfficeRentCost = "bills.officeRentCost"
	DefaultBillGraceDays  = 7.0
	DefaultOfficeRentCost = 10000.0
)

// BillGraceDays is how long a bill stays payable before the enforcement sweep
// acts on it.
func BillGraceDays(ctx context.Context, db *sql.DB) float64 {
	return warSettingFloat(ctx, db, settingBillGraceDays, DefaultBillGraceDays)
}

// OfficeRentCost is the 30-day office lease price. rentalinfo ships empty, so a flat
// server setting stands in for the per-slot price.
func OfficeRentCost(ctx context.Context, db *sql.DB) float64 {
	return warSettingFloat(ctx, db, settingOfficeRentCost, DefaultOfficeRentCost)
}

// SetBillSettings writes both from the ESP page.
func SetBillSettings(ctx context.Context, db *sql.DB, graceDays, officeRent float64) error {
	if err := SetServerSetting(ctx, db, settingBillGraceDays,
		strconv.FormatFloat(graceDays, 'f', -1, 64)); err != nil {
		return err
	}
	return SetServerSetting(ctx, db, settingOfficeRentCost,
		strconv.FormatFloat(officeRent, 'f', -1, 64))
}

// OfficesDueForRent returns corporation offices whose 30-day lease has run out against
// the last rental bill issued, for the rent sweep. The creditor is the station-owning
// corporation, not CONCORD.
type OfficeRentDue struct {
	CorporationID int64
	StationID     int64
	StationOwner  int64
}

func OfficesDueForRent(ctx context.Context, db *sql.DB) ([]OfficeRentDue, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, err
	}
	month := uint64(30) * 24 * 3600 * 10000000
	since := win32TimeNow() - month
	// An office owes rent when no rental bill for (this corp at this station)
	// has been issued in the last 30 days, paid or not. The owner's own office owes none.
	rows, err := db.QueryContext(ctx, `
		SELECT o.corporationID, o.stationID, s.corporationID
		  FROM crpoffices o
		  JOIN staStations s ON s.stationID = o.stationID
		 WHERE s.corporationID <> o.corporationID
		   AND NOT EXISTS (
		         SELECT 1 FROM bills b
		          WHERE b.billTypeID = ? AND b.debtorID = o.corporationID
		            AND b.externalID2 = o.stationID AND b.issued > ?)`,
		BillTypeRentalBill, since)
	if err != nil {
		return nil, fmt.Errorf("OfficesDueForRent: %w", err)
	}
	defer rows.Close()
	var out []OfficeRentDue
	for rows.Next() {
		var d OfficeRentDue
		if err := rows.Scan(&d.CorporationID, &d.StationID, &d.StationOwner); err != nil {
			return nil, fmt.Errorf("OfficesDueForRent scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// AlliancesDueForMaintenance returns (allianceID, member corp) pairs owing this week's
// maintenance fee (const.allianceMembershipCost per member corporation), billed by CONCORD.
type AllianceMaintenanceDue struct {
	AllianceID    int64
	CorporationID int64
}

func AlliancesDueForMaintenance(ctx context.Context, db *sql.DB) ([]AllianceMaintenanceDue, error) {
	if err := EnsureBillTables(ctx, db); err != nil {
		return nil, err
	}
	week := uint64(7) * 24 * 3600 * 10000000
	since := win32TimeNow() - week
	rows, err := db.QueryContext(ctx, `
		SELECT c.allianceID, c.corporationID
		  FROM corporation c
		  JOIN alliances a ON a.allianceID = c.allianceID AND a.deleted = 0
		 WHERE c.allianceID > 0
		   AND NOT EXISTS (
		         SELECT 1 FROM bills b
		          WHERE b.billTypeID = ? AND b.debtorID = c.corporationID
		            AND b.externalID = c.allianceID AND b.issued > ?)`,
		BillTypeAllianceMaintainance, since)
	if err != nil {
		return nil, fmt.Errorf("AlliancesDueForMaintenance: %w", err)
	}
	defer rows.Close()
	var out []AllianceMaintenanceDue
	for rows.Next() {
		var d AllianceMaintenanceDue
		if err := rows.Scan(&d.AllianceID, &d.CorporationID); err != nil {
			return nil, fmt.Errorf("AlliancesDueForMaintenance scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// GetRefTypesRowset serves account.GetRefTypes from market_reftypes. The client's journal
// silently drops every row whose refTypeID is not in this list (accountsvc.GetRefTypeKeyByID),
// so a partial list hides bill, rent and war-fee entries.
func GetRefTypesRowset(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT refTypeID, refTypeText, COALESCE(description, '') FROM market_reftypes ORDER BY refTypeID`)
	if err != nil {
		return nil, fmt.Errorf("GetRefTypesRowset: %w", err)
	}
	defer rows.Close()
	var lines [][]marshal.PyRep
	for rows.Next() {
		var id int64
		var text, desc string
		if err := rows.Scan(&id, &text, &desc); err != nil {
			return nil, fmt.Errorf("GetRefTypesRowset scan: %w", err)
		}
		lines = append(lines, []marshal.PyRep{intRep(id), strRep(text), strRep(desc)})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return makeRowset([]string{"refTypeID", "refTypeText", "description"}, lines), nil
}
