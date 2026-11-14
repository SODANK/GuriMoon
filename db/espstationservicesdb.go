package db

// Per-station service editing. A station's services come from its operation
// (staOperationServices), which hundreds of stations share, so a divergent station is moved onto
// its own cloned operation in the custom band. The client also joins map.GetStationExtraInfo on
// operationID itself, so a per-station override table would not reach it.
// See docs/db-10-notes.md#station-services.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CustomOperationBase is the first operationID this server allocates for per-station service
// sets. Shipped staOperations run 1..51; anything at or above this is a clone and may be
// rewritten or deleted.
const CustomOperationBase = 10000

// ESPServiceOption is one row of the service checkbox grid: a service the database defines,
// marked with whether the station offers it.
type ESPServiceOption struct {
	ServiceID   int64
	ServiceName string
	Description string
	Enabled     bool
}

// ESPGetStationServiceOptions lists every staServices row, flagging the ones the station's
// operation grants, ordered by serviceID (the mask bit order).
func ESPGetStationServiceOptions(ctx context.Context, db *sql.DB, operationID int64) ([]ESPServiceOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT sv.serviceID, sv.serviceName, sv.description,
		       EXISTS(SELECT 1 FROM staOperationServices os
		              WHERE os.operationID = ? AND os.serviceID = sv.serviceID)
		FROM staServices sv
		ORDER BY sv.serviceID`, operationID)
	if err != nil {
		return nil, fmt.Errorf("ESPGetStationServiceOptions: %w", err)
	}
	defer rows.Close()
	var out []ESPServiceOption
	for rows.Next() {
		var o ESPServiceOption
		if err := rows.Scan(&o.ServiceID, &o.ServiceName, &o.Description, &o.Enabled); err != nil {
			return nil, fmt.Errorf("ESPGetStationServiceOptions scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// ESPStationOperationInfo describes the operation a station is running, for the
// services editor's header.
type ESPStationOperationInfo struct {
	OperationID   int64
	OperationName string
	Custom        bool  // allocated by this server, not shipped
	SharedBy      int64 // stations using this operation, including this one
}

// ESPGetStationOperationInfo reports the station's operation and how many stations share it,
// which decides whether an edit clones.
func ESPGetStationOperationInfo(ctx context.Context, db *sql.DB, stationID int64) (ESPStationOperationInfo, error) {
	var info ESPStationOperationInfo
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(s.operationID, 0), COALESCE(op.operationName, ''),
		       (SELECT COUNT(*) FROM staStations s2 WHERE s2.operationID = s.operationID)
		FROM staStations s
		LEFT JOIN staOperations op ON op.operationID = s.operationID
		WHERE s.stationID = ?`, stationID,
	).Scan(&info.OperationID, &info.OperationName, &info.SharedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return info, fmt.Errorf("station %d not found", stationID)
	}
	if err != nil {
		return info, fmt.Errorf("ESPGetStationOperationInfo: %w", err)
	}
	info.Custom = info.OperationID >= CustomOperationBase
	return info, nil
}

// ESPSetStationServices gives the station exactly the listed services. It returns the
// operationID the station ended on and whether a new operation was allocated.
func ESPSetStationServices(ctx context.Context, db *sql.DB, stationID int64, serviceIDs []int64) (opID int64, cloned bool, err error) {
	want, err := validateServiceIDs(ctx, db, serviceIDs)
	if err != nil {
		return 0, false, err
	}
	// An operation with no service rows is refused: GetStationExtraInfo's readers index a dict by
	// operationID unchecked and raise KeyError for every player.
	if len(want) == 0 {
		return 0, false, fmt.Errorf("a station must keep at least one service: an operation with " +
			"no service rows is absent from map.GetStationExtraInfo, and the client indexes that " +
			"list by operationID unchecked (star map service overlay, S&I install gate)")
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, fmt.Errorf("ESPSetStationServices begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var (
		curOp    sql.NullInt64
		opName   sql.NullString
		sharedBy int64
	)
	err = tx.QueryRowContext(ctx, `
		SELECT s.operationID, op.operationName,
		       (SELECT COUNT(*) FROM staStations s2 WHERE s2.operationID = s.operationID)
		FROM staStations s
		LEFT JOIN staOperations op ON op.operationID = s.operationID
		WHERE s.stationID = ?`, stationID).Scan(&curOp, &opName, &sharedBy)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, fmt.Errorf("ESPSetStationServices: station %d not found", stationID)
	}
	if err != nil {
		return 0, false, fmt.Errorf("ESPSetStationServices lookup: %w", err)
	}

	target := curOp.Int64
	// A clone only this station uses is rewritten in place; shipped or shared operations are
	// cloned first.
	inPlace := curOp.Valid && curOp.Int64 >= CustomOperationBase && sharedBy == 1
	if !inPlace {
		target, err = cloneOperation(ctx, tx, curOp, opName)
		if err != nil {
			return 0, false, err
		}
		cloned = true
		if _, err := tx.ExecContext(ctx,
			`UPDATE staStations SET operationID = ? WHERE stationID = ?`, target, stationID); err != nil {
			return 0, false, fmt.Errorf("ESPSetStationServices assign: %w", err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		`DELETE FROM staOperationServices WHERE operationID = ?`, target); err != nil {
		return 0, false, fmt.Errorf("ESPSetStationServices clear: %w", err)
	}
	for _, id := range want {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO staOperationServices (operationID, serviceID) VALUES (?, ?)`,
			target, id); err != nil {
			return 0, false, fmt.Errorf("ESPSetStationServices insert %d: %w", id, err)
		}
	}

	// The old operation may now be an orphaned clone.
	if cloned && curOp.Valid {
		if err := deleteOrphanCustomOperationTx(ctx, tx, curOp.Int64); err != nil {
			return 0, false, err
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, false, fmt.Errorf("ESPSetStationServices commit: %w", err)
	}
	return target, cloned, nil
}

// validateServiceIDs de-duplicates the requested IDs and rejects any that is not a staServices
// row. A serviceID is a mask bit the client tests, so an invented one silently does nothing.
func validateServiceIDs(ctx context.Context, db *sql.DB, serviceIDs []int64) ([]int64, error) {
	seen := make(map[int64]bool, len(serviceIDs))
	var want []int64
	for _, id := range serviceIDs {
		if id <= 0 || seen[id] {
			continue
		}
		seen[id] = true
		want = append(want, id)
	}
	sort.Slice(want, func(i, j int) bool { return want[i] < want[j] })
	if len(want) == 0 {
		return nil, nil
	}

	place := make([]string, len(want))
	args := make([]any, len(want))
	for i, id := range want {
		place[i] = "?"
		args[i] = id
	}
	rows, err := db.QueryContext(ctx,
		`SELECT serviceID FROM staServices WHERE serviceID IN (`+strings.Join(place, ",")+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("validateServiceIDs: %w", err)
	}
	defer rows.Close()
	known := make(map[int64]bool, len(want))
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("validateServiceIDs scan: %w", err)
		}
		known[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for _, id := range want {
		if !known[id] {
			return nil, fmt.Errorf("service %d is not a staServices row", id)
		}
	}
	return want, nil
}

// cloneOperation copies the station's staOperations row into the custom band and returns the
// new operationID. The clone keeps activityID, description and the racial station type columns;
// description is what GetStation returns as the station's own text. A station with no
// operation row gets a minimal row.
func cloneOperation(ctx context.Context, tx *sql.Tx, curOp sql.NullInt64, opName sql.NullString) (int64, error) {
	var next int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(operationID) + 1, ?) FROM staOperations WHERE operationID >= ?`,
		CustomOperationBase, CustomOperationBase).Scan(&next); err != nil {
		return 0, fmt.Errorf("cloneOperation next id: %w", err)
	}
	if next < CustomOperationBase {
		next = CustomOperationBase
	}

	name := customName(opName.String)
	if curOp.Valid && opName.Valid {
		res, err := tx.ExecContext(ctx, `
			INSERT INTO staOperations
			  (activityID, operationID, operationName, description, fringe, corridor,
			   hub, border, ratio, caldariStationTypeID, minmatarStationTypeID,
			   amarrStationTypeID, gallenteStationTypeID, joveStationTypeID)
			SELECT activityID, ?, ?, description, fringe, corridor,
			       hub, border, ratio, caldariStationTypeID, minmatarStationTypeID,
			       amarrStationTypeID, gallenteStationTypeID, joveStationTypeID
			FROM staOperations WHERE operationID = ?`, next, name, curOp.Int64)
		if err != nil {
			return 0, fmt.Errorf("cloneOperation copy: %w", err)
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return next, nil
		}
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO staOperations (activityID, operationID, operationName, description)
		VALUES (0, ?, ?, '')`, next, name); err != nil {
		return 0, fmt.Errorf("cloneOperation create: %w", err)
	}
	return next, nil
}

// customName labels a clone so the operation dropdown does not read as the shipped operation.
// Re-cloning a custom name must not stack the suffix.
func customName(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		base = "Custom Operation"
	}
	const suffix = " (custom)"
	if strings.HasSuffix(base, suffix) {
		return truncName(base)
	}
	return truncName(base + suffix)
}

// operationName is varchar(100).
func truncName(s string) string {
	if len(s) <= 100 {
		return s
	}
	return s[:100]
}

// ESPDeleteOrphanCustomOperation drops a custom operation no station uses. The station
// editor's operation dropdown calls it after moving a station off a clone.
func ESPDeleteOrphanCustomOperation(ctx context.Context, db *sql.DB, operationID int64) error {
	if operationID < CustomOperationBase {
		return nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPDeleteOrphanCustomOperation begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := deleteOrphanCustomOperationTx(ctx, tx, operationID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("ESPDeleteOrphanCustomOperation commit: %w", err)
	}
	return nil
}

func deleteOrphanCustomOperationTx(ctx context.Context, tx *sql.Tx, operationID int64) error {
	if operationID < CustomOperationBase {
		return nil
	}
	var users int64
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM staStations WHERE operationID = ?`, operationID).Scan(&users); err != nil {
		return fmt.Errorf("deleteOrphanCustomOperation count: %w", err)
	}
	if users > 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM staOperationServices WHERE operationID = ?`, operationID); err != nil {
		return fmt.Errorf("deleteOrphanCustomOperation services: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM staOperations WHERE operationID = ?`, operationID); err != nil {
		return fmt.Errorf("deleteOrphanCustomOperation row: %w", err)
	}
	return nil
}
