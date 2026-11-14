package main

// Killboard recording on the death path: the adapter between the kill-report
// snapshot the death handlers build and db.RecordKillmail. The CONCORD mail and
// the killmail row are independent; neither failing takes the other down.

import (
	"context"
	"database/sql"
	"log"
	"time"

	evedb "gurimoon/db"
)

// recordKillmail files the structured killmail for a death the core saw. Always
// called as a goroutine with its own context: the death path runs on the
// victim's connection goroutine and must not wait on a write. Every failure is
// logged and swallowed.
func recordKillmail(sqldb *sql.DB, km killReport, isPod bool) {
	defer func() {
		if r := recover(); r != nil {
			log.Printf("[killboard] record panic victim=%d: %v", km.VictimCharID, r)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	killID, err := evedb.RecordKillmail(ctx, sqldb, killmailRecordFrom(km, isPod))
	if err != nil {
		log.Printf("[killboard] record victim=%d ship=%d: %v", km.VictimCharID, km.ShipTypeID, err)
		return
	}
	log.Printf("[killboard] kill %d recorded: %s lost a ship type %d in system %d (%d involved)",
		killID, km.VictimName, km.ShipTypeID, km.SysID, len(km.Hits))
}

// killmailRecordFrom flattens the mail-builder's snapshot into the recorder's
// plain-value form. Nothing here reads the database or the live system state:
// by the time this runs the victim's entity and its item rows are gone, so
// everything the board will ever know has to already be in km.
func killmailRecordFrom(km killReport, isPod bool) evedb.KillmailRecord {
	rec := evedb.KillmailRecord{
		Time:             km.Time,
		SolarSystemID:    km.SysID,
		X:                km.Pos.X,
		Y:                km.Pos.Y,
		Z:                km.Pos.Z,
		VictimCharID:     km.VictimCharID,
		VictimName:       km.VictimName,
		VictimCorpID:     km.VictimCorpID,
		VictimAllianceID: km.VictimAllianceID,
		ShipTypeID:       km.ShipTypeID,
		IsPod:            isPod,
		Source:           evedb.KillmailSourceCore,
	}
	if rec.Time.IsZero() {
		rec.Time = time.Now()
	}

	rec.Attackers = make([]evedb.KillmailAttacker, 0, len(km.Hits))
	for _, h := range km.Hits {
		rec.Attackers = append(rec.Attackers, evedb.KillmailAttacker{
			CharID:       h.CharID,
			Name:         h.Name,
			CorpID:       h.CorpID,
			AllianceID:   h.AllianceID,
			SecStatus:    h.SecStatus,
			ShipTypeID:   h.ShipTypeID,
			WeaponTypeID: h.WeaponTypeID,
			Damage:       h.Damage,
			IsNPC:        h.IsNPC,
			FinalBlow:    h.FinalBlow,
			Time:         h.Time,
		})
	}

	rec.Items = make([]evedb.KillmailItem, 0, len(km.DroppedItems)+len(km.DestroyedItems))
	add := func(rows []evedb.EntityRow, dropped bool) {
		for _, it := range rows {
			rec.Items = append(rec.Items, evedb.KillmailItem{
				TypeID:    it.TypeID,
				Flag:      it.Flag,
				Quantity:  it.Quantity,
				Singleton: it.Singleton != 0,
				Dropped:   dropped,
			})
		}
	}
	add(km.DroppedItems, true)
	add(km.DestroyedItems, false)
	return rec
}
