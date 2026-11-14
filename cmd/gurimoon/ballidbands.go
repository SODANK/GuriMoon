package main

// Ball-ID band guards for package main. The bands themselves are
// destiny/ballids.go (counter-minted kinds) and ephemeralball.go (force
// fields, cyno beacons); ballidbands_test.go pins every one under 2^31 and
// disjoint from the others.

import (
	"context"
	"database/sql"
	"log"
	"time"

	"gurimoon/destiny"
	"gurimoon/esp"
	"gurimoon/safego"
	"gurimoon/tick"
)

// reconcilePruneSpares reports whether the reconciliation sweep must leave a stale-looking ball
// alone: the client-only balls, which have no system entity for the keep set to vouch for.
// Everything else is enumerated by ballparkKeepReasons, so a real item that reads stale has left
// the grid. See docs/cmd-misc-notes.md#ball-id-bands.
func reconcilePruneSpares(id uint32) bool {
	return isClientOnlyBallID(id)
}

// isClientOnlyBallID reports whether id is in a synthetic band whose balls
// have no system entity: everything destiny mints except NPCs, plus the
// ephemeral bands.
func isClientOnlyBallID(id uint32) bool {
	if id >= forcefieldBallBand {
		return true
	}
	for _, b := range destiny.BallBands() {
		if b.Contains(id) {
			return !destiny.IsNPCBallID(id)
		}
	}
	return false
}

// ballIDHeadroomWarn is how close the entity AUTO_INCREMENT may come to the lowest synthetic band
// before boot warns.
const ballIDHeadroomWarn = 100_000_000

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, _ *esp.SessionRegistry, _ *tick.Manager) {
		go checkBallIDHeadroom(sqldb)
	})
}

// checkBallIDHeadroom warns when real item IDs approach the synthetic ball
// bands. Past destiny.SyntheticBallFloor a new item can share an ID with an
// NPC or missile on the same grid, which is the BallNotInPark loading-screen
// failure the bands exist to prevent.
func checkBallIDHeadroom(sqldb *sql.DB) {
	defer safego.Recover("checkBallIDHeadroom")
	if sqldb == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var next sql.NullInt64
	if err := sqldb.QueryRowContext(ctx,
		`SELECT AUTO_INCREMENT FROM information_schema.tables
		  WHERE table_schema = DATABASE() AND table_name = 'entity'`).Scan(&next); err != nil || !next.Valid {
		if err := sqldb.QueryRowContext(ctx, `SELECT MAX(itemID) + 1 FROM entity`).Scan(&next); err != nil || !next.Valid {
			return
		}
	}
	floor := int64(destiny.SyntheticBallFloor)
	switch {
	case next.Int64 >= floor:
		log.Printf("[ballids] ERROR: entity IDs (next %d) have reached the synthetic ball bands (%d+); NPC/missile balls can now collide with real items — move the bands (destiny/ballids.go)",
			next.Int64, floor)
	case next.Int64 >= floor-ballIDHeadroomWarn:
		log.Printf("[ballids] WARNING: entity IDs (next %d) are within %d of the synthetic ball bands (%d+); move the bands (destiny/ballids.go) before they meet",
			next.Int64, floor-next.Int64, floor)
	}
}
