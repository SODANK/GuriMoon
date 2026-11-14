package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"gurimoon/marshal"
)

// EnsureInsuranceTable creates the insurance table if it does not exist.
func EnsureInsuranceTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS insurance (
			contractID BIGINT AUTO_INCREMENT PRIMARY KEY,
			shipID     BIGINT NOT NULL,
			ownerID    BIGINT NOT NULL,
			fraction   DOUBLE NOT NULL,
			startDate  BIGINT NOT NULL,
			endDate    BIGINT NOT NULL,
			UNIQUE KEY (shipID)
		)`)
	if err != nil {
		return fmt.Errorf("EnsureInsuranceTable: %w", err)
	}
	return nil
}

// InsuranceContract holds one insurance row.
type InsuranceContract struct {
	ContractID int64
	ShipID     int64
	OwnerID    int64
	Fraction   float64
	StartDate  int64
	EndDate    int64
}

// GetInsurancePrice returns the basePrice from invTypes for the given typeID.
func GetInsurancePrice(ctx context.Context, db *sql.DB, typeID int64) (float64, error) {
	var price float64
	err := db.QueryRowContext(ctx,
		`SELECT basePrice FROM invTypes WHERE typeID = ?`, typeID).Scan(&price)
	if err != nil {
		return 0, fmt.Errorf("GetInsurancePrice: %w", err)
	}
	return price, nil
}

// GetContractForShip returns the active insurance contract for a ship, or nil.
func GetContractForShip(ctx context.Context, db *sql.DB, shipID int64) (*InsuranceContract, error) {
	var ic InsuranceContract
	err := db.QueryRowContext(ctx,
		`SELECT contractID, shipID, ownerID, fraction, startDate, endDate
		 FROM insurance WHERE shipID = ?`, shipID).Scan(
		&ic.ContractID, &ic.ShipID, &ic.OwnerID, &ic.Fraction,
		&ic.StartDate, &ic.EndDate)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("GetContractForShip: %w", err)
	}
	return &ic, nil
}

// InsureShip creates an insurance contract for shipID owned by ownerID, replacing any
// existing one. It does not deduct ISK; the caller does. startDate/endDate are Win32 filetimes.
func InsureShip(ctx context.Context, db *sql.DB, shipID, ownerID int64, fraction float64, startDate, endDate int64) (int64, error) {
	// Remove existing contract if any.
	_, _ = db.ExecContext(ctx, `DELETE FROM insurance WHERE shipID = ?`, shipID)

	res, err := db.ExecContext(ctx,
		`INSERT INTO insurance (shipID, ownerID, fraction, startDate, endDate)
		 VALUES (?, ?, ?, ?, ?)`,
		shipID, ownerID, fraction, startDate, endDate)
	if err != nil {
		return 0, fmt.Errorf("InsureShip: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("InsureShip LastInsertId: %w", err)
	}
	return id, nil
}

// UnInsureShip deletes the insurance contract for shipID.
func UnInsureShip(ctx context.Context, db *sql.DB, shipID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM insurance WHERE shipID = ?`, shipID)
	if err != nil {
		return fmt.Errorf("UnInsureShip: %w", err)
	}
	return nil
}

// insuranceCols is the contract rowset header.  base_insurance reads shipID,
// startDate, endDate and fraction off each row; infosvc adds ownerID.
var insuranceCols = []string{"contractID", "shipID", "ownerID", "fraction", "startDate", "endDate"}

// EmptyInsuredItemsRowset is a correctly shaped rowset with no rows, for paths that must
// answer something iterable without the database (base_insurance.GetContracts iterates it).
func EmptyInsuredItemsRowset() marshal.PyRep {
	return makeRowset(insuranceCols, nil)
}

// GetInsuredItems returns all active insurance contracts for ownerID as a util.Rowset.
func GetInsuredItems(ctx context.Context, db *sql.DB, ownerID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT contractID, shipID, ownerID, fraction, startDate, endDate
		 FROM insurance WHERE ownerID = ?`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("GetInsuredItems: %w", err)
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var ic InsuranceContract
		if err := rows.Scan(&ic.ContractID, &ic.ShipID, &ic.OwnerID, &ic.Fraction,
			&ic.StartDate, &ic.EndDate); err != nil {
			return nil, fmt.Errorf("GetInsuredItems scan: %w", err)
		}
		lines = append(lines, []marshal.PyRep{
			marshal.PyInt{Value: uint64(ic.ContractID)},
			marshal.PyInt{Value: uint64(ic.ShipID)},
			marshal.PyInt{Value: uint64(ic.OwnerID)},
			marshal.PyReal{Value: ic.Fraction},
			marshal.PyInt{Value: uint64(ic.StartDate)},
			marshal.PyInt{Value: uint64(ic.EndDate)},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetInsuredItems rows: %w", err)
	}
	return makeRowset(insuranceCols, lines), nil
}

// OwnerPendInsurance is the corporation the insurance payout mail comes from.
const OwnerPendInsurance = 1000113

// SendInsuranceMail mails a payout notice from Pend Insurance to charID. It goes through
// SendEveMail, the funnel that carries the MailboxSuppressed gate.
func SendInsuranceMail(ctx context.Context, db *sql.DB, charID int64, subject, body string, created int64) error {
	if _, err := SendEveMail(ctx, db, charID, OwnerPendInsurance, subject, body, created); err != nil {
		return fmt.Errorf("SendInsuranceMail: %w", err)
	}
	return nil
}

// GetShipEntity returns the typeID and ownerID for a ship entity.
func GetShipEntity(ctx context.Context, db *sql.DB, shipID int64) (typeID, ownerID int64, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT typeID, ownerID FROM entity WHERE itemID = ?`, shipID).Scan(&typeID, &ownerID)
	if err != nil {
		return 0, 0, fmt.Errorf("GetShipEntity: %w", err)
	}
	return typeID, ownerID, nil
}

// MigrateShipBasePrices sets invTypes.basePrice for ships (category 6) still at 0, from the
// mineral cost of their blueprint; the client hides a ship with no basePrice from the
// insurance window. Runs once, guarded by server_migrations. Returns rows updated.
func MigrateShipBasePrices(ctx context.Context, db *sql.DB) (int64, error) {
	const migrationName = "ship-base-prices"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigrateShipBasePrices meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}

	// Set basePrice = sum(material quantity * material basePrice) for each
	// ship whose blueprint has manufacturing materials (activity = 1).
	res, err := db.ExecContext(ctx, `
		UPDATE invTypes it
		  JOIN invGroups ig ON ig.groupID = it.groupID
		  JOIN (
		    SELECT bp.productTypeID,
		           SUM(m.quantity * mat.basePrice) AS mineralCost
		      FROM invblueprinttypes bp
		      JOIN tl2materialsfortypewithactivity m
		        ON m.typeID = bp.blueprintTypeID AND m.activity = 1
		      JOIN invTypes mat ON mat.typeID = m.requiredTypeID
		     GROUP BY bp.productTypeID
		    HAVING mineralCost > 0
		  ) costs ON costs.productTypeID = it.typeID
		   SET it.basePrice = costs.mineralCost
		 WHERE ig.categoryID = 6
		   AND it.basePrice = 0`)
	if err != nil {
		return 0, fmt.Errorf("MigrateShipBasePrices update: %w", err)
	}
	n, _ := res.RowsAffected()

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, UNIX_TIMESTAMP())`,
		migrationName); err != nil {
		return 0, fmt.Errorf("MigrateShipBasePrices guard: %w", err)
	}
	return n, nil
}
