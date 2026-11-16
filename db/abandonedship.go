package db

// Abandoned ships: a hull its pilot ejected from, floating unpiloted in a
// solar system (locationID = the system). customInfo carries the marker so a
// system reload restores the ball, and only marked hulls with no character
// aboard ever load as abandoned: a logged-off pilot's ship also sits in the
// system, with the pilot inside.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// abandonedShipMarker is entity.customInfo on an ejected-from hull.
const abandonedShipMarker = "abandoned"

// ErrShipNotAbandoned means the hull was boarded, moved or destroyed first.
var ErrShipNotAbandoned = errors.New("ship is no longer abandoned")

// shipUnpilotedSQL holds for ship row s when no character entity is aboard.
const shipUnpilotedSQL = `NOT EXISTS (
	SELECT 1 FROM entity p
	JOIN invTypes pt ON pt.typeID = p.typeID
	JOIN invGroups pg ON pg.groupID = pt.groupID
	WHERE p.locationID = s.itemID AND pg.categoryID = 1)`

// AbandonedShip is one unpiloted hull to restore into a loaded system.
type AbandonedShip struct {
	ShipID, TypeID, GroupID, OwnerID int64
	CorpID, AllianceID               int64
	Name                             string
	Radius                           float64
	X, Y, Z                          float64
}

// MarkShipAbandoned leaves shipID floating at (x, y, z) in sysID with the
// abandoned marker. Call it before the pilot moves out.
func MarkShipAbandoned(ctx context.Context, db *sql.DB, shipID, sysID int64, x, y, z float64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID = ?, flag = 0, x = ?, y = ?, z = ?, customInfo = ? WHERE itemID = ?`,
		sysID, x, y, z, abandonedShipMarker, shipID); err != nil {
		return fmt.Errorf("MarkShipAbandoned %d: %w", shipID, err)
	}
	return nil
}

// ClaimAbandonedShip makes newOwnerID the owner of an abandoned hull in sysID,
// with its fitting, cargo and one level of containers inside it, and clears
// the marker. ErrShipNotAbandoned when it is no longer an unpiloted marked
// hull in that system.
func ClaimAbandonedShip(ctx context.Context, db *sql.DB, shipID, sysID, newOwnerID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ClaimAbandonedShip: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var one int
	err = tx.QueryRowContext(ctx, `
		SELECT 1 FROM entity s
		WHERE s.itemID = ? AND s.locationID = ? AND s.customInfo = ? AND `+shipUnpilotedSQL+`
		FOR UPDATE`, shipID, sysID, abandonedShipMarker).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrShipNotAbandoned
	}
	if err != nil {
		return fmt.Errorf("ClaimAbandonedShip: check %d: %w", shipID, err)
	}
	// Contents first: one level of containers inside the ship, then the ship's
	// own contents. A JOIN avoids MariaDB's "can't specify target table".
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity e
		JOIN entity p ON p.itemID = e.locationID
		SET e.ownerID = ?
		WHERE p.locationID = ?`, newOwnerID, shipID); err != nil {
		return fmt.Errorf("ClaimAbandonedShip: nested contents of %d: %w", shipID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET ownerID = ? WHERE locationID = ?`, newOwnerID, shipID); err != nil {
		return fmt.Errorf("ClaimAbandonedShip: contents of %d: %w", shipID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET ownerID = ?, customInfo = NULL WHERE itemID = ?`, newOwnerID, shipID); err != nil {
		return fmt.Errorf("ClaimAbandonedShip: ship %d: %w", shipID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ClaimAbandonedShip: commit: %w", err)
	}
	return nil
}

// ListAbandonedShips returns the unpiloted marked hulls floating in sysID.
func ListAbandonedShips(ctx context.Context, db *sql.DB, sysID int64) ([]AbandonedShip, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT s.itemID, s.typeID, t.groupID, s.ownerID,
		       COALESCE(ch.corporationID, 0), COALESCE(co.allianceID, 0),
		       COALESCE(s.itemName, ''), COALESCE(t.radius, 0),
		       COALESCE(s.x, 0), COALESCE(s.y, 0), COALESCE(s.z, 0)
		FROM entity s
		JOIN invTypes t ON t.typeID = s.typeID
		JOIN invGroups g ON g.groupID = t.groupID
		LEFT JOIN character_ ch ON ch.characterID = s.ownerID
		LEFT JOIN corporation co ON co.corporationID = ch.corporationID
		WHERE s.locationID = ? AND s.customInfo = ? AND g.categoryID = 6
		  AND `+shipUnpilotedSQL, sysID, abandonedShipMarker)
	if err != nil {
		return nil, fmt.Errorf("ListAbandonedShips %d: %w", sysID, err)
	}
	defer rows.Close()
	var out []AbandonedShip
	for rows.Next() {
		var a AbandonedShip
		if err := rows.Scan(&a.ShipID, &a.TypeID, &a.GroupID, &a.OwnerID,
			&a.CorpID, &a.AllianceID, &a.Name, &a.Radius, &a.X, &a.Y, &a.Z); err != nil {
			return nil, fmt.Errorf("ListAbandonedShips scan: %w", err)
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ShedShipFittingOverload offlines modules on shipID, highest slot first,
// until its online set fits the CPU and powergrid its owner's skills give.
// Call it after the ship changes hands. Returns the modules offlined.
func ShedShipFittingOverload(ctx context.Context, db *sql.DB, shipID, shipTypeID int64) ([]int64, error) {
	return shedOverBudgetModules(ctx, db, shipID, shipTypeID)
}
