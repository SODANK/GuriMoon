package db

// Ice reprocessing seed. The data dump ships no invorereprocessing rows for ice (groupID 465),
// so this migration inserts the classic ice refine table: every ice yields Heavy Water, Liquid
// Ozone and Strontium Clathrates; the four faction ices add their empire's isotope; the four
// deep-space ices trade isotopes for more coolant and Strontium. portionSize is 1, so
// amountPerBatch is per unit.

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// Ice refine output typeIDs.
const (
	typeHeavyWater      = 16272
	typeLiquidOzone     = 16273
	typeHeliumIsotopes  = 16274 // Amarr
	typeStrontium       = 16275
	typeOxygenIsotopes  = 17887 // Gallente
	typeNitrogenIsotope = 17888 // Caldari
	typeHydrogenIsotope = 17889 // Minmatar
)

// iceYield is one ice type's refine output, per unit.
type iceYield struct {
	oreTypeID  int64
	heavyWater int64
	ozone      int64
	strontium  int64
	isotopeID  int64 // 0 = no isotope (the unaligned deep-space ices)
	isotopes   int64
}

// iceYields is the classic ice refine table. Faction ices and their richer 0.0 variants carry
// their empire's isotope; the four deep-space ices carry none.
var iceYields = []iceYield{
	// Faction ice, base.
	{16262, 69, 35, 1, typeHeliumIsotopes, 414},  // Clear Icicle    (Amarr)
	{16265, 69, 35, 1, typeNitrogenIsotope, 414}, // White Glaze     (Caldari)
	{16264, 69, 35, 1, typeOxygenIsotopes, 414},  // Blue Ice        (Gallente)
	{16263, 69, 35, 1, typeHydrogenIsotope, 414}, // Glacial Mass    (Minmatar)
	// Faction ice, richer 0.0 variants.
	{17978, 138, 69, 1, typeHeliumIsotopes, 460},  // Enriched Clear Icicle
	{17976, 138, 69, 1, typeNitrogenIsotope, 460}, // Pristine White Glaze
	{17975, 138, 69, 1, typeOxygenIsotopes, 460},  // Thick Blue Ice
	{17977, 138, 69, 1, typeHydrogenIsotope, 460}, // Smooth Glacial Mass
	// Unaligned deep-space ice.
	{16266, 1000, 500, 25, 0, 0}, // Glare Crust
	{16267, 500, 1000, 50, 0, 0}, // Dark Glitter
	{16268, 250, 500, 75, 0, 0},  // Gelidus
	{16269, 125, 250, 100, 0, 0}, // Krystallos
}

// MigrateIceReprocessing inserts the ice refine rows into invorereprocessing if
// they are missing. Runs once, guarded by the server_migrations table.
// Returns the number of rows inserted.
func MigrateIceReprocessing(ctx context.Context, db *sql.DB) (int64, error) {
	const migrationName = "ice-reprocessing-seed"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigrateIceReprocessing meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}

	// invorereprocessing is a MyISAM table with no primary key, so a re-run
	// would duplicate rows: clear any existing ice rows before inserting.
	var ids []string
	var args []any
	for _, y := range iceYields {
		ids = append(ids, "?")
		args = append(args, y.oreTypeID)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM invorereprocessing WHERE oreTypeID IN (`+strings.Join(ids, ",")+`)`,
		args...); err != nil {
		return 0, fmt.Errorf("MigrateIceReprocessing clear: %w", err)
	}

	var rows []string
	var vals []any
	add := func(ore, mineral, amount int64) {
		if amount <= 0 {
			return
		}
		rows = append(rows, "(?,?,?)")
		vals = append(vals, ore, mineral, amount)
	}
	for _, y := range iceYields {
		add(y.oreTypeID, typeHeavyWater, y.heavyWater)
		add(y.oreTypeID, typeLiquidOzone, y.ozone)
		add(y.oreTypeID, typeStrontium, y.strontium)
		if y.isotopeID != 0 {
			add(y.oreTypeID, y.isotopeID, y.isotopes)
		}
	}
	res, err := db.ExecContext(ctx,
		`INSERT INTO invorereprocessing (oreTypeID, mineralTypeID, amountPerBatch) VALUES `+
			strings.Join(rows, ","), vals...)
	if err != nil {
		return 0, fmt.Errorf("MigrateIceReprocessing insert: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, ?)`,
		migrationName, win32TimeNow()); err != nil {
		return 0, fmt.Errorf("MigrateIceReprocessing mark applied: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}
