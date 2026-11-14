package main

// Boot wiring for the petition desk: tables, and the registry the
// petitioner service pushes through (server/petitioner).

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/server/petitioner"
	"gurimoon/tick"
)

func init() {
	moduleBootHooks = append(moduleBootHooks, func(sqldb *sql.DB, sessReg *esp.SessionRegistry, _ *tick.Manager) {
		if sqldb == nil {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := evedb.EnsurePetitionTables(ctx, sqldb); err != nil {
			log.Printf("[petitions] %v", err)
			return
		}
		petitioner.Install(sessReg, sqldb)
	})
}
