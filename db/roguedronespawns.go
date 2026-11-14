package db

import (
	"context"
	"database/sql"
	"fmt"
	"log"
)

// RogueDroneFactionID owns every rogue drone belt listing: chrFactions "Unknown", the same ID as
// npc.FactionUnknown. The spawner sends no warFactionID for it, takes no standing on a kill and
// shows the ball's owner as "Unknown". See docs/db-notes.md#rogue-drone-belts.
const RogueDroneFactionID = 500021

// RogueDroneRegions are the regions whose belts spawn rogue drones: the eight drone regions under
// their map code names and the three Jove regions.
var RogueDroneRegions = []int64{
	10000004, // UUA-F4 (Jove)
	10000013, // LQ-0QN (Malpais)
	10000017, // J7HZ-F (Jove)
	10000018, // VU-WU2 (The Spire)
	10000019, // A821-A (Jove)
	10000021, // 6HL8-L (Outer Passage)
	10000027, // 7-KXBJ (Etherium Reach)
	10000034, // 87-1CW (The Kalevala Expanse)
	10000040, // G5KW-3 (Oasa)
	10000053, // H3J8-U (Cobalt Edge)
	10000066, // S-I6VU (Perrigen Falls)
}

// RogueDroneListing is one npcRegionSpawns row of the rogue drone roster, without its region.
type RogueDroneListing struct {
	TypeID    int64
	MaxSec    float64
	Weight    int
	ShipClass string
	Role      string
}

// RogueDroneRoster is the belt roster every region in RogueDroneRegions gets, all of invGroups 287.
// maxSec follows the hull ladder of the pirate rosters. sql/npcregionspawns.sql carries the same
// rows: change both together.
func RogueDroneRoster() []RogueDroneListing {
	var out []RogueDroneListing
	add := func(maxSec float64, weight int, class, role string, typeIDs ...int64) {
		for _, t := range typeIDs {
			out = append(out, RogueDroneListing{TypeID: t, MaxSec: maxSec, Weight: weight,
				ShipClass: class, Role: role})
		}
	}
	// Infester, Splinter, Render, Decimator, Sunder, Raider, Hunter, Silverfish, Devilfish, Barracuda.
	add(1.0, 3, "frigate", "normal", 3750, 18087, 18088, 3749, 18079, 18080, 18564, 3744, 18085, 18086)
	// Shatter, Ripper, Shredder, Predator, Marauder, Dismantler.
	add(0.8, 3, "destroyer", "normal", 23475, 23476, 23477, 23478, 23479, 23480)
	// Wrecker, Destructor, Annihilator, Devastator, Viral Infector, Violator, Disintegrator, Bomber,
	// Atomizer, Nuker.
	add(0.6, 3, "cruiser", "normal", 3746, 18082, 18083, 18084, 3745, 18076, 18077, 18078, 23487, 23488)
	// Enforcer, Defeater, Crippler, Striker, Siege, Exterminator.
	add(0.4, 2, "battlecruiser", "normal", 3747, 18073, 18074, 18075, 23489, 23490)
	// Drone Controller, Swarm Preserver, Spearhead, Domination, Supreme Drone Parasite, Drone Ruler,
	// Drone Creator.
	add(0.4, 1, "battleship", "normal", 10299, 23497, 23498, 23499, 23500, 23501, 23502)
	// Strain Infester, Splinter, Render, Decimator, Sunder, Raider, Hunter, Silverfish, Devilfish,
	// Barracude.
	add(0.8, 3, "frigate", "commander", 3748, 18070, 18071, 18072, 23481, 23482, 23483, 23484, 23485, 23486)
	// Strain Wrecker, Destructor, Disintegrator, Bomber, Atomizer, Nuker.
	add(0.6, 3, "cruiser", "commander", 23491, 23492, 23493, 23494, 23495, 23496)
	// Patriarch, Matriarch, Drone Queen.
	add(0.4, 1, "battleship", "commander", 23503, 23504, 23505)
	return out
}

// settingRogueDroneSpawnsApplied marks the one-shot rogue drone belt seed as done.
const settingRogueDroneSpawnsApplied = "spawner.rogueDroneSpawnsApplied"

// migrateRogueDroneSpawns seeds RogueDroneRoster into every RogueDroneRegions region that has no
// npcRegionSpawns rows, once, guarded by a server_settings marker so an admin who empties a
// region later keeps it empty. Each region is written in one transaction.
func migrateRogueDroneSpawns(ctx context.Context, db *sql.DB) error {
	if v, ok, _ := GetServerSetting(ctx, db, settingRogueDroneSpawnsApplied); ok && v == "1" {
		return nil
	}
	roster := RogueDroneRoster()
	seeded := 0
	for _, regionID := range RogueDroneRegions {
		var n int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM npcRegionSpawns WHERE regionID = ?`, regionID).Scan(&n); err != nil {
			return fmt.Errorf("migrateRogueDroneSpawns count region %d: %w", regionID, err)
		}
		if n > 0 {
			continue
		}
		if err := insertRogueDroneRoster(ctx, db, regionID, roster); err != nil {
			return err
		}
		seeded++
	}
	if seeded > 0 {
		log.Printf("[spawner] rogue drone belts: seeded %d listings in each of %d region(s)",
			len(roster), seeded)
	}
	return SetServerSetting(ctx, db, settingRogueDroneSpawnsApplied, "1")
}

// insertRogueDroneRoster writes the roster for one region in a single transaction.
func insertRogueDroneRoster(ctx context.Context, db *sql.DB, regionID int64, roster []RogueDroneListing) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("migrateRogueDroneSpawns begin region %d: %w", regionID, err)
	}
	defer tx.Rollback()
	for _, l := range roster {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO npcRegionSpawns
				(regionID, typeID, maxSec, weight, shipClass, role, factionID, corporationID)
			VALUES (?, ?, ?, ?, ?, ?, ?, 0)`,
			regionID, l.TypeID, l.MaxSec, l.Weight, l.ShipClass, l.Role, RogueDroneFactionID); err != nil {
			return fmt.Errorf("migrateRogueDroneSpawns insert region %d type %d: %w", regionID, l.TypeID, err)
		}
	}
	return tx.Commit()
}
