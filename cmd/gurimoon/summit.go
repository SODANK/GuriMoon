package main

// The Intergalactic Summit, the server half: the letter that carries a pilot's Summit key (its
// tables are made at boot beside the news feed's). The board itself is esp/summit.go (pages) and db/summitdb.go (storage).

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
	"gurimoon/esp"
	"gurimoon/server"
)

// summitCarrierID signs the Summit's letters: InterBus, which carries the floor.
const summitCarrierID = 1000148

// startSummit installs the Summit's mail hook.
func startSummit(sqldb *sql.DB) {
	esp.SummitMailHook = func(charID int64, subject, body string) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		created := int64(server.Win32TimeNow())
		msgID, err := evedb.SendEveMail(ctx, sqldb, charID, summitCarrierID, subject, body, created)
		if err != nil {
			log.Printf("[summit] letter to %d: %v", charID, err)
			return
		}
		if msgID != 0 {
			pushMailNotification(charID, msgID, summitCarrierID, subject, created)
		}
	}
}
