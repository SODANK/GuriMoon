package db

// Kill sites: where in a solar system a ship died. RecordKillmail resolves the celestial closest
// to the victim's stored position once, at record time, and stores it (locationID, Name,
// GroupID, Dist) so a later rename or removal does not rewrite the record. The client's Near:
// label is client-side, so the server derives its own. Core and module killmails both pass real
// coordinates, so resolving inside RecordKillmail covers both.

import (
	"context"
	"database/sql"
	"fmt"
	"log"
	"math"

	"gurimoon/safego"
)

// mapDenormalize group IDs a kill site can be named after.
const (
	killSiteGroupSun      = 6
	killSiteGroupPlanet   = 7
	killSiteGroupMoon     = 8
	killSiteGroupBelt     = 9 // asteroid belts and ice fields alike
	killSiteGroupStargate = 10
	killSiteGroupStation  = 15
)

// KillLocationReach is how far (metres, from a celestial's surface) a wreck can lie and still
// be at it. Beyond it the site is deep space (deadspace pockets, dungeon rooms far from
// landmarks).
const KillLocationReach = 1e9

// KillLocationDeepSpace is the name stored for a site beyond KillLocationReach.
// Its locationID is 0 and its group 0.
const KillLocationDeepSpace = "Deep space"

// killLocationNameMax is killmails.locationName's width; STRICT_TRANS_TABLES makes an overrun a
// rolled-back insert of the whole killmail.
const killLocationNameMax = 100

// NearestCelestial returns the closest sun, planet, moon, belt, gate or station to (x, y, z).
// Distance is from the surface (centre distance minus radius, floored at 0), as NearestMoon
// does. ok is false when the list holds none of those kinds. Ties go to the smaller body, then
// the lower itemID.
func NearestCelestial(cels []CelestialEntity, x, y, z float64) (best CelestialEntity, surfaceDist float64, ok bool) {
	surfaceDist = -1
	for _, c := range cels {
		switch c.GroupID {
		case killSiteGroupSun, killSiteGroupPlanet, killSiteGroupMoon,
			killSiteGroupBelt, killSiteGroupStargate, killSiteGroupStation:
		default:
			continue
		}
		dx, dy, dz := x-c.X, y-c.Y, z-c.Z
		d := math.Sqrt(dx*dx+dy*dy+dz*dz) - c.Radius
		if d < 0 {
			d = 0
		}
		better := surfaceDist < 0 || d < surfaceDist
		if !better && d == surfaceDist {
			better = c.Radius < best.Radius || (c.Radius == best.Radius && c.ItemID < best.ItemID)
		}
		if better {
			best, surfaceDist, ok = c, d, true
		}
	}
	if !ok {
		return CelestialEntity{}, 0, false
	}
	return best, surfaceDist, true
}

// KillSite is one resolved kill location.
type KillSite struct {
	ID      int64   // mapDenormalize itemID; 0 for deep space
	Name    string  // "" = unresolved
	GroupID int64   // 0 for deep space
	Dist    float64 // metres from the celestial's surface
}

// killSiteFor names the site of a death at (x, y, z) among cels. An all-zero position is
// unresolved, not nearest to the origin: it is what a module without coordinates looks like, and
// the origin would credit every such kill to one sun.
func killSiteFor(cels []CelestialEntity, x, y, z float64) KillSite {
	if x == 0 && y == 0 && z == 0 {
		return KillSite{}
	}
	c, d, ok := NearestCelestial(cels, x, y, z)
	if !ok {
		return KillSite{}
	}
	if d > KillLocationReach {
		return KillSite{Name: KillLocationDeepSpace, Dist: d}
	}
	return KillSite{ID: c.ItemID, Name: clampRunes(c.Name, killLocationNameMax), GroupID: c.GroupID, Dist: d}
}

// resolveKillSite is killSiteFor over the shared celestial cache. A failed load is
// an unresolved site, not a failed killmail.
func resolveKillSite(ctx context.Context, db *sql.DB, systemID int64, x, y, z float64) KillSite {
	if systemID == 0 {
		return KillSite{}
	}
	cels, err := CachedSystemCelestials(ctx, db, systemID)
	if err != nil {
		return KillSite{}
	}
	return killSiteFor(cels, x, y, z)
}

// clampRunes cuts s to at most n characters.
func clampRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// BackfillKillmailLocations resolves the site of killmails with locationName IS NULL from their
// stored position. Idempotent: an unresolvable row (no usable position, or no celestials) is set
// to '' so it is not selected again. Batched by system so each system's celestials load once.
// Returns the count resolved to a real site. Run on its own goroutine at boot.
func BackfillKillmailLocations(ctx context.Context, db *sql.DB) (resolved int, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("BackfillKillmailLocations panic: %v", r)
		}
	}()
	const batch = 5000
	type row struct {
		killID, sys int64
		x, y, z     float64
	}
	for {
		if err := ctx.Err(); err != nil {
			return resolved, err
		}
		rs, err := db.QueryContext(ctx, `
			SELECT killID, solarSystemID, posX, posY, posZ
			  FROM killmails
			 WHERE locationName IS NULL
			 ORDER BY solarSystemID, killID
			 LIMIT ?`, batch)
		if err != nil {
			return resolved, fmt.Errorf("BackfillKillmailLocations select: %w", err)
		}
		var rows []row
		for rs.Next() {
			var r row
			if err := rs.Scan(&r.killID, &r.sys, &r.x, &r.y, &r.z); err != nil {
				rs.Close()
				return resolved, fmt.Errorf("BackfillKillmailLocations scan: %w", err)
			}
			rows = append(rows, r)
		}
		rs.Close()
		if err := rs.Err(); err != nil {
			return resolved, err
		}
		if len(rows) == 0 {
			return resolved, nil
		}

		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return resolved, fmt.Errorf("BackfillKillmailLocations begin: %w", err)
		}
		stmt, err := tx.PrepareContext(ctx, `
			UPDATE killmails
			   SET locationID = ?, locationName = ?, locationGroupID = ?, locationDist = ?
			 WHERE killID = ? AND locationName IS NULL`)
		if err != nil {
			tx.Rollback()
			return resolved, fmt.Errorf("BackfillKillmailLocations prepare: %w", err)
		}
		var curSys int64 = -1
		var cels []CelestialEntity
		for _, r := range rows {
			if r.sys != curSys {
				curSys = r.sys
				cels = nil
				if r.sys != 0 {
					if c, e := CachedSystemCelestials(ctx, db, r.sys); e == nil {
						cels = c
					}
				}
			}
			site := killSiteFor(cels, r.x, r.y, r.z)
			if _, err := stmt.ExecContext(ctx, site.ID, site.Name, site.GroupID, site.Dist, r.killID); err != nil {
				stmt.Close()
				tx.Rollback()
				return resolved, fmt.Errorf("BackfillKillmailLocations update %d: %w", r.killID, err)
			}
			if site.Name != "" {
				resolved++
			}
		}
		stmt.Close()
		if err := tx.Commit(); err != nil {
			return resolved, fmt.Errorf("BackfillKillmailLocations commit: %w", err)
		}
		if len(rows) < batch {
			return resolved, nil
		}
	}
}

// LogKillLocationBackfill runs the backfill and logs the outcome. For main's boot.
func LogKillLocationBackfill(ctx context.Context, db *sql.DB) {
	defer safego.Recover("LogKillLocationBackfill")
	n, err := BackfillKillmailLocations(ctx, db)
	if err != nil {
		log.Printf("[killboard] location backfill: %v (resolved %d before stopping)", err, n)
		return
	}
	if n > 0 {
		log.Printf("[killboard] location backfill: resolved %d killmails", n)
	}
}
