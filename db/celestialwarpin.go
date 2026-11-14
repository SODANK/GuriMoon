package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// Canonical warp-in geometry for planets and moons, and where a control tower sits at a
// moon. Pure arithmetic shared by the player warp, PlantStarbase and TowerAnchorPosition.
// See docs/db-12-notes.md#warp-in-altitude.
const (
	WarpInAltitudeFraction = 0.5

	MoonWarpInAltitudeMin = 150_000.0   // 150 km
	MoonWarpInAltitudeMax = 2_500_000.0 // 2,500 km

	PlanetWarpInAltitudeMin = 500_000.0     // 500 km
	PlanetWarpInAltitudeMax = 100_000_000.0 // 100,000 km

	// MoonTowerStandoff is how far a tower sits from the moon's warp-in, toward the moon:
	// off the landing and inside every shipped tower's moonAnchorDistance (100 km, measured
	// from the warp-in).
	MoonTowerStandoff = 75_000.0
	// moonTowerMinAltitude keeps a tower's structure field (slaves anchor up to
	// maxStructureDistance, 50 km) clear of the surface on the smallest moons.
	moonTowerMinAltitude = 75_000.0
)

const celGroupStarDB = 6

func clampAltitude(radius, lo, hi float64) float64 {
	a := radius * WarpInAltitudeFraction
	if a < lo {
		return lo
	}
	if a > hi {
		return hi
	}
	return a
}

// MoonWarpInAltitude is how far above a moon's surface its warp-in sits.
func MoonWarpInAltitude(radius float64) float64 {
	return clampAltitude(radius, MoonWarpInAltitudeMin, MoonWarpInAltitudeMax)
}

// PlanetWarpInAltitude is how far above a planet's surface its warp-in sits.
func PlanetWarpInAltitude(radius float64) float64 {
	return clampAltitude(radius, PlanetWarpInAltitudeMin, PlanetWarpInAltitudeMax)
}

// WarpInOutward is the unit vector from a body away from its orbit parent, the side the
// warp-in is on. +X when the two coincide.
func WarpInOutward(body, parent [3]float64) [3]float64 {
	d := [3]float64{body[0] - parent[0], body[1] - parent[1], body[2] - parent[2]}
	l := math.Sqrt(d[0]*d[0] + d[1]*d[1] + d[2]*d[2])
	if l == 0 {
		return [3]float64{1, 0, 0}
	}
	return [3]float64{d[0] / l, d[1] / l, d[2] / l}
}

func along(p, dir [3]float64, dist float64) [3]float64 {
	return [3]float64{p[0] + dir[0]*dist, p[1] + dir[1]*dist, p[2] + dir[2]*dist}
}

// MoonWarpInPoint is a moon's canonical warp-in: MoonWarpInAltitude above the
// surface, on the far side from its planet.
func MoonWarpInPoint(moon, parent [3]float64, radius float64) [3]float64 {
	return along(moon, WarpInOutward(moon, parent), radius+MoonWarpInAltitude(radius))
}

// MoonTowerPoint is where a control tower is planted at a moon: on the line
// from the moon through its warp-in, MoonTowerStandoff short of the warp-in,
// and never lower than moonTowerMinAltitude.
func MoonTowerPoint(moon, parent [3]float64, radius float64) [3]float64 {
	alt := MoonWarpInAltitude(radius) - MoonTowerStandoff
	if alt < moonTowerMinAltitude {
		alt = moonTowerMinAltitude
	}
	return along(moon, WarpInOutward(moon, parent), radius+alt)
}

// moonGeometry reads a moon's position, radius and orbit parent's position, falling
// back to the system's star, then to the moon itself (which gives +X outward).
func moonGeometry(ctx context.Context, db *sql.DB, moonID int64) (sysID int64, moon, parent [3]float64, radius float64, name string, err error) {
	var r sql.NullFloat64
	var orbitID sql.NullInt64
	var itemName sql.NullString
	err = db.QueryRowContext(ctx, `
		SELECT solarSystemID, x, y, z, radius, orbitID, itemName FROM mapDenormalize WHERE itemID = ?`, moonID,
	).Scan(&sysID, &moon[0], &moon[1], &moon[2], &r, &orbitID, &itemName)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, moon, parent, 0, "", ErrMoonNotFound
	}
	if err != nil {
		return 0, moon, parent, 0, "", fmt.Errorf("moon %d geometry: %w", moonID, err)
	}
	name = itemName.String
	radius = 1000.0
	if r.Valid && r.Float64 > 0 {
		radius = r.Float64
	}
	parent = moon
	found := false
	if orbitID.Valid && orbitID.Int64 != 0 {
		row := db.QueryRowContext(ctx, `SELECT x, y, z FROM mapDenormalize WHERE itemID = ?`, orbitID.Int64)
		if qerr := row.Scan(&parent[0], &parent[1], &parent[2]); qerr == nil {
			found = true
		}
	}
	if !found {
		_ = db.QueryRowContext(ctx, `SELECT x, y, z FROM mapDenormalize WHERE solarSystemID = ? AND groupID = ? LIMIT 1`,
			sysID, celGroupStarDB).Scan(&parent[0], &parent[1], &parent[2])
	}
	return sysID, moon, parent, radius, name, nil
}

// MoonWarpIn returns a moon's canonical warp-in point from the map.
func MoonWarpIn(ctx context.Context, db *sql.DB, moonID int64) ([3]float64, error) {
	_, moon, parent, radius, _, err := moonGeometry(ctx, db, moonID)
	if err != nil {
		return [3]float64{}, err
	}
	return MoonWarpInPoint(moon, parent, radius), nil
}

// MoonTowerAnchorPoint returns where a control tower is planted at moonID.
func MoonTowerAnchorPoint(ctx context.Context, db *sql.DB, moonID int64) (sysID int64, p [3]float64, moonName string, err error) {
	sysID, moon, parent, radius, name, err := moonGeometry(ctx, db, moonID)
	if err != nil {
		return 0, p, "", err
	}
	return sysID, MoonTowerPoint(moon, parent, radius), name, nil
}
