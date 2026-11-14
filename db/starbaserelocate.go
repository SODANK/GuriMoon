package db

import (
	"context"
	"database/sql"
	"fmt"
	"math"
)

// One-shot move of towers planted at the legacy point (moon + radius + 5 km on +X, which
// put a tower's guns and arrays inside the moon) onto MoonTowerPoint (celestialwarpin.go),
// with every wired structure moved by the same delta. Only the exact legacy coordinate
// qualifies, so a hand-placed tower is never touched and a moved tower no longer matches.
// See docs/db-12-notes.md#legacy-tower-relocation.

// LegacyTowerTolerance is how close (metres) a tower must sit to the legacy point to
// count as legacy-placed. The legacy rule wrote the sum straight to DOUBLE columns.
const LegacyTowerTolerance = 1.0

// legacyTowerSurfaceOffset is the legacy placement's height above the surface.
const legacyTowerSurfaceOffset = 5000.0

// LegacyTowerPoint is where the legacy rule planted a tower at a moon. radius is
// moonGeometry's (1000 m when the map row carries none).
func LegacyTowerPoint(moon [3]float64, radius float64) [3]float64 {
	return [3]float64{moon[0] + radius + legacyTowerSurfaceOffset, moon[1], moon[2]}
}

// IsLegacyTowerPosition reports whether tower sits on the legacy point of a moon at
// moonPos with the given radius.
func IsLegacyTowerPosition(tower, moonPos [3]float64, radius float64) bool {
	p := LegacyTowerPoint(moonPos, radius)
	dx, dy, dz := tower[0]-p[0], tower[1]-p[1], tower[2]-p[2]
	return math.Sqrt(dx*dx+dy*dy+dz*dz) <= LegacyTowerTolerance
}

// TowerRelocation describes one tower moved by RelocateLegacyTowers.
type TowerRelocation struct {
	TowerID, SolarSystemID, MoonID int64
	From, To                       [3]float64
	Structures                     int64 // wired structures moved with it
}

// RelocateLegacyTowers moves every legacy-placed tower and its wired structures
// (starbaseStructures.towerID) onto MoonTowerPoint, one transaction per tower. skipSystem
// leaves a loaded system's towers alone. A failure is collected and the pass goes on.
func RelocateLegacyTowers(ctx context.Context, db *sql.DB, skipSystem func(sysID int64) bool) ([]TowerRelocation, error) {
	if err := EnsureStarbaseSchema(ctx, db); err != nil {
		return nil, err
	}
	type cand struct {
		towerID, sysID, moonID int64
		pos                    [3]float64
	}
	rows, err := db.QueryContext(ctx, `
		SELECT s.entityID, s.solarSystemID, s.moonID, e.x, e.y, e.z
		FROM starbase s JOIN entity e ON e.itemID = s.entityID
		WHERE s.moonID <> 0`)
	if err != nil {
		return nil, fmt.Errorf("relocate towers: list: %w", err)
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.towerID, &c.sysID, &c.moonID, &c.pos[0], &c.pos[1], &c.pos[2]); err != nil {
			rows.Close()
			return nil, fmt.Errorf("relocate towers: scan: %w", err)
		}
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("relocate towers: list: %w", err)
	}

	var moved []TowerRelocation
	var errs []error
	for _, c := range cands {
		if skipSystem != nil && skipSystem(c.sysID) {
			continue
		}
		_, moon, parent, radius, _, gerr := moonGeometry(ctx, db, c.moonID)
		if gerr != nil {
			errs = append(errs, fmt.Errorf("tower %d: %w", c.towerID, gerr))
			continue
		}
		if !IsLegacyTowerPosition(c.pos, moon, radius) {
			continue
		}
		to := MoonTowerPoint(moon, parent, radius)
		n, rerr := relocateTower(ctx, db, c.towerID, moon, radius, to)
		if rerr != nil {
			errs = append(errs, fmt.Errorf("tower %d: %w", c.towerID, rerr))
			continue
		}
		if n < 0 {
			continue // moved by someone else between the scan and the lock
		}
		moved = append(moved, TowerRelocation{
			TowerID: c.towerID, SolarSystemID: c.sysID, MoonID: c.moonID,
			From: c.pos, To: to, Structures: n,
		})
	}
	if len(errs) > 0 {
		return moved, fmt.Errorf("relocate towers: %d failure(s), first: %w", len(errs), errs[0])
	}
	return moved, nil
}

// relocateTower moves one tower to `to` and its wired structures by the same delta in
// one transaction. The tower's position is re-read under lock and the legacy signature
// re-checked, so two passes cannot both move it; a tower that no longer matches returns
// -1 and changes nothing.
func relocateTower(ctx context.Context, db *sql.DB, towerID int64, moon [3]float64, radius float64, to [3]float64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	var at [3]float64
	if err := tx.QueryRowContext(ctx,
		`SELECT x, y, z FROM entity WHERE itemID = ? FOR UPDATE`, towerID,
	).Scan(&at[0], &at[1], &at[2]); err != nil {
		return 0, fmt.Errorf("lock: %w", err)
	}
	if !IsLegacyTowerPosition(at, moon, radius) {
		return -1, nil
	}
	d := [3]float64{to[0] - at[0], to[1] - at[1], to[2] - at[2]}

	res, err := tx.ExecContext(ctx, `
		UPDATE entity e JOIN starbaseStructures ss ON ss.entityID = e.itemID
		SET e.x = e.x + ?, e.y = e.y + ?, e.z = e.z + ?
		WHERE ss.towerID = ? AND e.itemID <> ?`,
		d[0], d[1], d[2], towerID, towerID)
	if err != nil {
		return 0, fmt.Errorf("structures: %w", err)
	}
	n, _ := res.RowsAffected()
	if _, err := tx.ExecContext(ctx,
		`UPDATE entity SET x = ?, y = ?, z = ? WHERE itemID = ?`, to[0], to[1], to[2], towerID,
	); err != nil {
		return 0, fmt.Errorf("tower: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return n, nil
}
