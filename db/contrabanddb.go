package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Contraband enforcement bounds. Customs officers (customs.go) spawn only in empire systems at
// or above CustomsMinSec and no system exceeds MaxSystemSec, so a security threshold outside
// (0, MaxSystemSec] never fires and one at or below CustomsMinSec applies wherever customs is.
const (
	CustomsMinSec = 0.45
	MaxSystemSec  = 1.0
)

// ContrabandEntry represents a row from invcontrabandtypes joined with faction,
// type, group and category names for display.
type ContrabandEntry struct {
	FactionID        int64
	FactionName      string
	TypeID           int64
	TypeName         string
	CategoryName     string
	GroupName        string
	BasePrice        float64
	HasType          bool // typeID resolved to an invTypes row
	StandingLoss     float64
	ConfiscateMinSec float64
	FineByValue      float64
	AttackMinSec     float64
}

// Confiscates reports whether the seizure rule can ever fire: customs.go tests
// confiscateMinSec > 0 && systemSec >= confiscateMinSec, so a zero, negative or
// above-1.0 threshold never seizes anything.
func (e ContrabandEntry) Confiscates() bool {
	return e.ConfiscateMinSec > 0 && e.ConfiscateMinSec <= MaxSystemSec
}

// Attacks reports whether the shoot-on-sight rule can ever fire, on the same
// threshold rule as Confiscates.
func (e ContrabandEntry) Attacks() bool {
	return e.AttackMinSec > 0 && e.AttackMinSec <= MaxSystemSec
}

// Fines reports whether a fine is charged. Fines have no security gate: any scan in customs
// space charges them.
func (e ContrabandEntry) Fines() bool { return e.FineByValue > 0 }

// FinePerUnit is the ISK charged per unit carried: quantity * basePrice * fineByValue in
// customs.go. Zero when the item has no basePrice, in which case customs.go charges nothing.
func (e ContrabandEntry) FinePerUnit() float64 { return e.BasePrice * e.FineByValue }

// Inert reports whether the row does nothing when scanned: no reachable seizure or attack
// threshold and no fine that resolves to ISK.
func (e ContrabandEntry) Inert() bool {
	if e.Confiscates() || e.Attacks() {
		return false
	}
	return !e.Fines() || e.BasePrice <= 0
}

// SeizeEverywhere reports whether the seizure threshold is low enough to apply
// in every system that has customs officers at all.
func (e ContrabandEntry) SeizeEverywhere() bool {
	return e.Confiscates() && e.ConfiscateMinSec <= CustomsMinSec
}

// AttackEverywhere reports the same for the attack threshold.
func (e ContrabandEntry) AttackEverywhere() bool {
	return e.Attacks() && e.AttackMinSec <= CustomsMinSec
}

// ToggleAttackTarget is the attackMinSec the row-level swap button writes:
// rows that currently attack are turned off, rows that do not are set to fire
// throughout customs space.
func (e ContrabandEntry) ToggleAttackTarget() float64 {
	if e.Attacks() {
		return 0
	}
	return CustomsMinSec
}

// WarnLabel is a short flag for rows that will not behave as the numbers read.
func (e ContrabandEntry) WarnLabel() string {
	switch {
	case !e.HasType:
		return "not in invTypes"
	case e.Inert():
		return "never fires"
	case e.Fines() && e.BasePrice <= 0:
		return "fine is 0 ISK"
	}
	return ""
}

// WarnDetail explains WarnLabel in a sentence, for a tooltip.
func (e ContrabandEntry) WarnDetail() string {
	switch {
	case !e.HasType:
		return "This server's invTypes has no row for this typeID, so no cargo can ever match it."
	case e.Inert():
		var why string
		switch {
		case e.ConfiscateMinSec > MaxSystemSec || e.AttackMinSec > MaxSystemSec:
			why = fmt.Sprintf("its thresholds are above %.1f, the highest security any system has", MaxSystemSec)
		case e.ConfiscateMinSec < 0 || e.AttackMinSec < 0:
			why = "its thresholds are negative, which customs.go reads as off (the original CCP data meant \"any security\")"
		case e.Fines() && e.BasePrice <= 0:
			why = "the item has no basePrice, so the fine works out to 0 ISK"
		default:
			why = "no seizure threshold, no attack threshold and no fine are set"
		}
		return "Scanned and let through: " + why + "."
	case e.Fines() && e.BasePrice <= 0:
		return "fineByValue multiplies the item's basePrice, which is 0 here, so no ISK is taken. Seizure/attack still apply."
	}
	return ""
}

// FactionRow is a minimal faction record for dropdown population.
type FactionRow struct {
	FactionID   int64
	FactionName string
}

// FactionContrabandStats is one row of the faction picker: how much space the
// faction polices and what its ban list does.
type FactionContrabandStats struct {
	FactionID       int64
	FactionName     string
	Systems         int64 // solar systems claimed by the faction
	HighsecSystems  int64 // of those, ones where customs officers spawn
	Rows            int64 // banned types
	ConfiscateRows  int64
	FineRows        int64
	AttackRows      int64
	InertRows       int64
	MissingTypeRows int64
}

// ListFactionContrabandStats returns every faction with its contraband
// counters, for the faction picker. nameFilter is an optional name substring.
func ListFactionContrabandStats(ctx context.Context, db *sql.DB, nameFilter string) ([]FactionContrabandStats, error) {
	var where string
	var args []any
	if nameFilter != "" {
		where = "WHERE f.factionName LIKE ?"
		args = append(args, "%"+nameFilter+"%")
	}
	rows, err := db.QueryContext(ctx, `
		SELECT f.factionID, COALESCE(f.factionName, ''),
		       (SELECT COUNT(*) FROM mapSolarSystems m WHERE m.factionID = f.factionID),
		       (SELECT COUNT(*) FROM mapSolarSystems m
		         WHERE m.factionID = f.factionID AND m.security >= ?),
		       COUNT(c.typeID),
		       COALESCE(SUM(CASE WHEN c.confiscateMinSec > 0 AND c.confiscateMinSec <= ?
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN c.fineByValue > 0 AND COALESCE(t.basePrice, 0) > 0
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN c.attackMinSec > 0 AND c.attackMinSec <= ?
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN NOT (c.confiscateMinSec > 0 AND c.confiscateMinSec <= ?)
		                          AND NOT (c.attackMinSec > 0 AND c.attackMinSec <= ?)
		                          AND NOT (c.fineByValue > 0 AND COALESCE(t.basePrice, 0) > 0)
		                          AND c.typeID IS NOT NULL
		                         THEN 1 ELSE 0 END), 0),
		       COALESCE(SUM(CASE WHEN c.typeID IS NOT NULL AND t.typeID IS NULL
		                         THEN 1 ELSE 0 END), 0)
		  FROM chrFactions f
		  LEFT JOIN invcontrabandtypes c ON c.factionID = f.factionID
		  LEFT JOIN invTypes t ON t.typeID = c.typeID
		  `+where+`
		 GROUP BY f.factionID, f.factionName
		 ORDER BY f.factionName`,
		append([]any{CustomsMinSec, MaxSystemSec, MaxSystemSec, MaxSystemSec, MaxSystemSec}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("ListFactionContrabandStats: %w", err)
	}
	defer rows.Close()
	var out []FactionContrabandStats
	for rows.Next() {
		var f FactionContrabandStats
		if err := rows.Scan(&f.FactionID, &f.FactionName, &f.Systems, &f.HighsecSystems,
			&f.Rows, &f.ConfiscateRows, &f.FineRows, &f.AttackRows,
			&f.InertRows, &f.MissingTypeRows); err != nil {
			return nil, fmt.Errorf("ListFactionContrabandStats scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// GetFactionSpace returns how many solar systems a faction claims and how many
// of those are at or above the customs security floor.
func GetFactionSpace(ctx context.Context, db *sql.DB, factionID int64) (systems, highsec int64, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COUNT(*), COALESCE(SUM(CASE WHEN security >= ? THEN 1 ELSE 0 END), 0)
		  FROM mapSolarSystems WHERE factionID = ?`, CustomsMinSec, factionID).
		Scan(&systems, &highsec)
	if err != nil {
		return 0, 0, fmt.Errorf("GetFactionSpace factionID=%d: %w", factionID, err)
	}
	return systems, highsec, nil
}

// GetChrFactionName returns one faction's name from chrFactions, or "" if there
// is no such row. (GetFactionName in missiondb.go reads eveStaticOwners, which
// carries player-owned entities too.)
func GetChrFactionName(ctx context.Context, db *sql.DB, factionID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT factionName FROM chrFactions WHERE factionID = ?`, factionID).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("GetFactionName: %w", err)
	}
	return name, nil
}

// ListAllContraband returns every invcontrabandtypes row with faction and type names.
func ListAllContraband(ctx context.Context, db *sql.DB) ([]ContrabandEntry, error) {
	return queryContraband(ctx, db, "", nil)
}

// ListContrabandByFaction returns one faction's contraband entries, ordered the
// way the client groups items: category, then group, then item.
func ListContrabandByFaction(ctx context.Context, db *sql.DB, factionID int64) ([]ContrabandEntry, error) {
	return queryContraband(ctx, db, "c.factionID = ?", []any{factionID})
}

// ListContrabandByType returns every faction's entry for one type.
func ListContrabandByType(ctx context.Context, db *sql.DB, typeID int64) ([]ContrabandEntry, error) {
	return queryContraband(ctx, db, "c.typeID = ?", []any{typeID})
}

func queryContraband(ctx context.Context, db *sql.DB, where string, args []any) ([]ContrabandEntry, error) {
	q := `SELECT c.factionID, COALESCE(f.factionName, ''), c.typeID, COALESCE(t.typeName, ''),
	             COALESCE(ic.categoryName, ''), COALESCE(ig.groupName, ''),
	             COALESCE(t.basePrice, 0), t.typeID IS NOT NULL,
	             COALESCE(c.standingLoss, 0), COALESCE(c.confiscateMinSec, 0),
	             COALESCE(c.fineByValue, 0), COALESCE(c.attackMinSec, 0)
	        FROM invcontrabandtypes c
	        LEFT JOIN chrFactions f ON f.factionID = c.factionID
	        LEFT JOIN invTypes t ON t.typeID = c.typeID
	        LEFT JOIN invGroups ig ON ig.groupID = t.groupID
	        LEFT JOIN invCategories ic ON ic.categoryID = ig.categoryID`
	if where != "" {
		q += " WHERE " + where
	}
	q += " ORDER BY ic.categoryName, ig.groupName, t.typeName, c.typeID"
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("queryContraband: %w", err)
	}
	defer rows.Close()

	var out []ContrabandEntry
	for rows.Next() {
		var e ContrabandEntry
		if err := rows.Scan(&e.FactionID, &e.FactionName, &e.TypeID, &e.TypeName,
			&e.CategoryName, &e.GroupName, &e.BasePrice, &e.HasType,
			&e.StandingLoss, &e.ConfiscateMinSec, &e.FineByValue, &e.AttackMinSec); err != nil {
			return nil, fmt.Errorf("queryContraband scan: %w", err)
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// UpsertContraband inserts or updates a single contraband entry.
func UpsertContraband(ctx context.Context, db *sql.DB, factionID, typeID int64,
	standingLoss, confiscateMinSec, fineByValue, attackMinSec float64) error {
	_, err := db.ExecContext(ctx, `
		REPLACE INTO invcontrabandtypes (factionID, typeID, standingLoss, confiscateMinSec, fineByValue, attackMinSec)
		VALUES (?, ?, ?, ?, ?, ?)`,
		factionID, typeID, standingLoss, confiscateMinSec, fineByValue, attackMinSec)
	if err != nil {
		return fmt.Errorf("UpsertContraband: %w", err)
	}
	return nil
}

// SetContrabandAttack rewrites one row's attack threshold (the row-level swap between the
// seize-only and shoot-on-sight panels).
func SetContrabandAttack(ctx context.Context, db *sql.DB, factionID, typeID int64, value float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE invcontrabandtypes SET attackMinSec = ? WHERE factionID = ? AND typeID = ?`,
		value, factionID, typeID)
	if err != nil {
		return fmt.Errorf("SetContrabandAttack: %w", err)
	}
	return nil
}

// DeleteContraband removes a single contraband entry.
func DeleteContraband(ctx context.Context, db *sql.DB, factionID, typeID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM invcontrabandtypes WHERE factionID = ? AND typeID = ?`,
		factionID, typeID)
	if err != nil {
		return fmt.Errorf("DeleteContraband: %w", err)
	}
	return nil
}

// Fields SetFactionContrabandField can rewrite across a whole faction.
const (
	ContrabandFieldStanding   = "standing"
	ContrabandFieldConfiscate = "confiscate"
	ContrabandFieldFine       = "fine"
	ContrabandFieldAttack     = "attack"
)

var contrabandFieldColumns = map[string]string{
	ContrabandFieldStanding:   "standingLoss",
	ContrabandFieldConfiscate: "confiscateMinSec",
	ContrabandFieldFine:       "fineByValue",
	ContrabandFieldAttack:     "attackMinSec",
}

// SetFactionContrabandField writes one field on every one of a faction's
// contraband rows, and returns how many rows changed value.
func SetFactionContrabandField(ctx context.Context, db *sql.DB, factionID int64, field string, value float64) (int64, error) {
	col, ok := contrabandFieldColumns[field]
	if !ok {
		return 0, fmt.Errorf("SetFactionContrabandField: unknown field %q", field)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE invcontrabandtypes SET `+col+` = ? WHERE factionID = ?`, value, factionID)
	if err != nil {
		return 0, fmt.Errorf("SetFactionContrabandField: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ClampFactionContrabandThresholds rewrites unreachable security thresholds (negative, or above
// the highest system security) to 0, so off reads as off.
func ClampFactionContrabandThresholds(ctx context.Context, db *sql.DB, factionID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		UPDATE invcontrabandtypes
		   SET confiscateMinSec = CASE WHEN confiscateMinSec < 0 OR confiscateMinSec > ?
		                               THEN 0 ELSE confiscateMinSec END,
		       attackMinSec     = CASE WHEN attackMinSec < 0 OR attackMinSec > ?
		                               THEN 0 ELSE attackMinSec END
		 WHERE factionID = ?
		   AND (confiscateMinSec < 0 OR confiscateMinSec > ?
		     OR attackMinSec < 0 OR attackMinSec > ?)`,
		MaxSystemSec, MaxSystemSec, factionID, MaxSystemSec, MaxSystemSec)
	if err != nil {
		return 0, fmt.Errorf("ClampFactionContrabandThresholds: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// DeleteFactionMissingTypeContraband drops a faction's rows whose typeID has no invTypes entry
// on this server; they can never match cargo.
func DeleteFactionMissingTypeContraband(ctx context.Context, db *sql.DB, factionID int64) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE c FROM invcontrabandtypes c
		  LEFT JOIN invTypes t ON t.typeID = c.typeID
		 WHERE c.factionID = ? AND t.typeID IS NULL`, factionID)
	if err != nil {
		return 0, fmt.Errorf("DeleteFactionMissingTypeContraband: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// ListFactions returns all factions for dropdown population.
func ListFactions(ctx context.Context, db *sql.DB) ([]FactionRow, error) {
	rows, err := db.QueryContext(ctx, `SELECT factionID, factionName FROM chrFactions ORDER BY factionName`)
	if err != nil {
		return nil, fmt.Errorf("ListFactions: %w", err)
	}
	defer rows.Close()
	var out []FactionRow
	for rows.Next() {
		var f FactionRow
		if err := rows.Scan(&f.FactionID, &f.FactionName); err != nil {
			return nil, fmt.Errorf("ListFactions scan: %w", err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
