package main

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
)

// releaseShareEscrow runs at boot in a build without the SCC module: every open exchange order
// hands its ISK and shares back to its owner (db/shareescrow.go). It does nothing once there
// are no open orders.
func releaseShareEscrow(sqldb *sql.DB) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	n, isk, shares, err := evedb.ReleaseShareEscrow(ctx, sqldb)
	if err != nil {
		log.Printf("[shares] exchange absent: escrow release stopped after %d orders: %v", n, err)
	}
	if n > 0 {
		log.Printf("[shares] exchange absent: released %d open orders, %.0f ISK and %d shares to their owners", n, isk, shares)
	}
}
