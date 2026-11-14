package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"regexp"
	"strings"

	"gurimoon/marshal"
)

// excludedDECategories lists category IDs kept out of the dungeon editor item browser
// (non-space items such as skills, modules and blueprints).
const excludedDECategories = "1,4,7,8,9,14,16,17,20,24,29,30,35"

// EnsureDungeonTables creates the dunDungeons, dunRooms and dunObjects tables if missing.
// Call once at start-up after opening the DB.
func EnsureDungeonTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dunDungeons (
			dungeonVID   INT PRIMARY KEY AUTO_INCREMENT,
			dungeonVName VARCHAR(255) NOT NULL,
			factionID    INT NOT NULL,
			version      INT NOT NULL DEFAULT 1,
			status       INT NOT NULL DEFAULT 4
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureDungeonTables dunDungeons: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dunRooms (
			roomID       INT PRIMARY KEY AUTO_INCREMENT,
			dungeonVID   INT NOT NULL,
			roomName     VARCHAR(255) NOT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureDungeonTables dunRooms: %w", err)
	}

	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dunObjects (
			objectID         INT PRIMARY KEY AUTO_INCREMENT,
			roomID           INT NOT NULL,
			typeID           INT NOT NULL,
			x                DOUBLE NOT NULL DEFAULT 0,
			y                DOUBLE NOT NULL DEFAULT 0,
			z                DOUBLE NOT NULL DEFAULT 0,
			yaw              DOUBLE DEFAULT NULL,
			pitch            DOUBLE DEFAULT NULL,
			roll             DOUBLE DEFAULT NULL,
			radius           DOUBLE DEFAULT NULL,
			aggressionRange  DOUBLE DEFAULT NULL,
			stance           VARCHAR(16) DEFAULT NULL
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureDungeonTables dunObjects: %w", err)
	}

	// Per-entity override columns; the ALTERs fail harmlessly when a column exists.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN orbitDistance DOUBLE DEFAULT NULL`)
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN followRange DOUBLE DEFAULT NULL`)
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN leashRange DOUBLE DEFAULT NULL`)

	// Per-NPC faction override (NULL = use dungeon default).
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN npcFactionID INT DEFAULT NULL`)

	// Acceleration-gate music: a res:/Sound/Music/... url the client plays when the gate
	// activates. NULL = none.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN musicUrl VARCHAR(255) DEFAULT NULL`)

	// Leash: an NPC with leashObjectID idles by orbiting that object (another dunObjects row in
	// the room) instead of holding still. orbitDistance is the radius in metres (NULL = auto).
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN leashObjectID INT DEFAULT NULL`)

	// Free-text description, emitted as slimItem.dunDescription (Show Info text and bookmark
	// hint on beacons).
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN description VARCHAR(1000) DEFAULT NULL`)

	// Wave group: NULL/0 spawns with the room; N >= 1 stays held until a spawnWave trigger
	// with that number fires.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN spawnWave INT DEFAULT NULL`)

	// Custom ball name (destiny ball.name); the client hints cfg.evelocations with it.
	// NULL = the type name.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN ballName VARCHAR(100) DEFAULT NULL`)

	// Structure invulnerability: the structure still locks and draws fire but takes no damage
	// and is never destroyed.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN invulnerable TINYINT(1) NOT NULL DEFAULT 0`)

	// Per-ball HP multiplier on shield, armour and structure (resists untouched); HP otherwise
	// comes from the type (GetEntityHPByType). NULL = 1.0.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN hpScale DOUBLE DEFAULT NULL`)

	// Collision. NULL = the type's default (DunSolidByDefault); 0/1 is the author's override.
	// NULL, not a stored 1, so every row and import follows the one rule without a backfill.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN solid TINYINT(1) DEFAULT NULL`)

	// Per-ball bounds on how many distinct items its authored loot (dunObjectLoot) yields;
	// see RollBallLoot. NULL = no bound on that side.
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN lootMinDrops INT DEFAULT NULL`)
	db.ExecContext(ctx, `ALTER TABLE dunObjects ADD COLUMN lootMaxDrops INT DEFAULT NULL`)

	// Manual room ordering (editor drag and drop). NULL falls back to roomID order.
	db.ExecContext(ctx, `ALTER TABLE dunRooms ADD COLUMN roomOrder INT DEFAULT NULL`)

	// Deadspace natural phenomena: while the room is loaded, warp drives do not work inside the
	// pocket. On by default; turn it off for a warpable room.
	db.ExecContext(ctx, `ALTER TABLE dunRooms ADD COLUMN naturalPhenomena TINYINT(1) NOT NULL DEFAULT 1`)

	// DED rating, 1..10 (0 = unrated), shown by the star map's DED Deadspace Report, which
	// colours a system by its highest rating (unrated counts as 1).
	db.ExecContext(ctx, `ALTER TABLE dunDungeons ADD COLUMN dedLevel TINYINT NOT NULL DEFAULT 0`)

	// Dungeon logic triggers, evaluated server-side. Events: roomEnter, npcKilled, npcDamaged,
	// roomCleared, gateActivated. Actions: transmission, localChat, popup, spawnWave, music.
	_, err = db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS dunTriggers (
			triggerID        INT PRIMARY KEY AUTO_INCREMENT,
			roomID           INT NOT NULL,
			event            VARCHAR(32) NOT NULL,
			targetObjectID   INT DEFAULT NULL,
			action           VARCHAR(32) NOT NULL,
			header           VARCHAR(255) DEFAULT NULL,
			icon             VARCHAR(64) DEFAULT NULL,
			body             TEXT,
			senderName       VARCHAR(128) DEFAULT NULL,
			deferUntilLanded TINYINT NOT NULL DEFAULT 0,
			onceOnly         TINYINT NOT NULL DEFAULT 1,
			delayMs          INT NOT NULL DEFAULT 0,
			spawnWave        INT DEFAULT NULL,
			chatColor        VARCHAR(16) DEFAULT NULL,
			INDEX idx_room (roomID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureDungeonTables dunTriggers: %w", err)
	}

	// Wave number for spawnWave triggers, on pre-existing tables.
	db.ExecContext(ctx, `ALTER TABLE dunTriggers ADD COLUMN spawnWave INT DEFAULT NULL`)
	// localChat text color ("RRGGBB"), on pre-existing tables.
	db.ExecContext(ctx, `ALTER TABLE dunTriggers ADD COLUMN chatColor VARCHAR(16) DEFAULT NULL`)
	// itemPlaced item filter, on pre-existing tables.
	db.ExecContext(ctx, `ALTER TABLE dunTriggers ADD COLUMN itemTypeID INT DEFAULT NULL`)
	db.ExecContext(ctx, `ALTER TABLE dunTriggers ADD COLUMN itemQty INT NOT NULL DEFAULT 1`)

	return nil
}

// MigrateNPCStance replaces the named NPC behaviour profiles with the per-ball stance
// (dunObjects.brainProfile becomes stance) and drops the old column from the belt tables.
// Idempotent; run after EnsureDungeonTables, EnsureSpawnerTables and the belt-wave tables.
// See docs/db-5-notes.md#dungeon-schema-and-authoring-defaults.
func MigrateNPCStance(ctx context.Context, db *sql.DB) error {
	var schemaErr error
	hasTable := func(t string) bool {
		ok, err := tableExists(ctx, db, t)
		if err != nil && schemaErr == nil {
			schemaErr = err
		}
		return ok
	}
	hasColumn := func(t, c string) bool {
		ok, err := columnExists(ctx, db, t, c)
		if err != nil && schemaErr == nil {
			schemaErr = err
		}
		return ok
	}

	if hasColumn("dunObjects", "brainProfile") {
		mapping := `CASE
			WHEN LOWER(o.brainProfile) IN ('passive', 'approach') THEN 'passive'
			WHEN LOWER(o.brainProfile) IN ('relaxed', 'defensive') THEN 'defensive'
			ELSE NULL END`
		join := ""
		if hasTable("npcBehaviorProfiles") {
			join = `LEFT JOIN npcBehaviorProfiles p ON p.profileName = o.brainProfile`
			mapping = `CASE
				WHEN p.profileName IS NULL OR p.movementMode = 'flee' THEN NULL
				WHEN p.targetPreference = 'none' OR p.maxTargets <= 0 THEN 'passive'
				WHEN p.autoAggro = 0 THEN 'defensive'
				ELSE NULL END`
		}
		if _, err := db.ExecContext(ctx, `UPDATE dunObjects o `+join+`
			SET o.brainProfile = `+mapping+`
			WHERE o.brainProfile IS NOT NULL`); err != nil {
			return fmt.Errorf("MigrateNPCStance map dunObjects: %w", err)
		}
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE dunObjects CHANGE COLUMN brainProfile stance VARCHAR(16) DEFAULT NULL`); err != nil {
			return fmt.Errorf("MigrateNPCStance rename dunObjects.brainProfile: %w", err)
		}
		log.Printf("[db] dunObjects.brainProfile -> stance")
	}
	if schemaErr != nil {
		return fmt.Errorf("MigrateNPCStance: %w", schemaErr)
	}
	if !hasColumn("dunObjects", "stance") && schemaErr == nil {
		// schemaErr is re-checked: a failed probe reads as "missing".
		if _, err := db.ExecContext(ctx,
			`ALTER TABLE dunObjects ADD COLUMN stance VARCHAR(16) DEFAULT NULL`); err != nil {
			return fmt.Errorf("MigrateNPCStance add dunObjects.stance: %w", err)
		}
	}

	for _, tbl := range []string{"npcRegionSpawns", "npcSpawnEntries", "npcBeltRats"} {
		if !hasColumn(tbl, "brainProfile") {
			continue
		}
		if _, err := db.ExecContext(ctx, `ALTER TABLE `+tbl+` DROP COLUMN brainProfile`); err != nil {
			return fmt.Errorf("MigrateNPCStance drop %s.brainProfile: %w", tbl, err)
		}
		log.Printf("[db] dropped %s.brainProfile", tbl)
	}

	for _, tbl := range []string{"npcTactics", "npcBehaviorProfiles"} {
		if !hasTable(tbl) {
			continue
		}
		if _, err := db.ExecContext(ctx, `DROP TABLE `+tbl); err != nil {
			return fmt.Errorf("MigrateNPCStance drop %s: %w", tbl, err)
		}
		log.Printf("[db] dropped retired table %s", tbl)
	}
	if schemaErr != nil {
		return fmt.Errorf("MigrateNPCStance: %w", schemaErr)
	}
	return nil
}

// StoredStance normalises a stance for dunObjects.stance. The aggressive default is stored
// as NULL ("" here). Validation is the caller's: npc.ValidStance.
func StoredStance(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "aggressive" {
		return ""
	}
	return s
}

// DungeonRow represents a row from dunDungeons.
type DungeonRow struct {
	DungeonVID   int64
	DungeonVName string
	FactionID    int64
	Version      int64
	Status       int64
}

// RoomRow represents a row from dunRooms.
type RoomRow struct {
	RoomID     int64
	DungeonVID int64
	RoomName   string
	// NaturalPhenomena blocks warp drives inside the pocket while this room is loaded.
	NaturalPhenomena bool
}

// ObjectRow represents a row from dunObjects.
type ObjectRow struct {
	ObjectID         int64
	RoomID           int64
	TypeID           int64
	X, Y, Z          float64
	Yaw, Pitch, Roll sql.NullFloat64
	Radius           sql.NullFloat64
	AggressionRange  sql.NullFloat64
	Stance           sql.NullString // NULL = aggressive
	NPCFactionID     sql.NullInt64
	MusicUrl         sql.NullString
	LeashObjectID    sql.NullInt64
	OrbitDistance    sql.NullFloat64
	Description      sql.NullString
	SpawnWave        sql.NullInt64
	BallName         sql.NullString  // destiny ball.name override; NULL = type name
	Invulnerable     bool            // structure takes no damage and cannot be destroyed
	HPScale          sql.NullFloat64 // shield/armour/structure multiplier; NULL = 1.0
	Solid            sql.NullBool    // collidable; NULL = DunSolidByDefault
}

// IsSolid reports whether this ball spawns collidable: the authored override, else the
// type's default.
func (o ObjectRow) IsSolid(groupID int64) bool {
	if o.Solid.Valid {
		return o.Solid.Bool
	}
	return DunSolidByDefault(o.TypeID, groupID)
}

// DunSolidByDefault is whether a dungeon ball of this type collides unless the author says
// otherwise. Beacons, clouds and force fields are not solid: every warp-in lands on the beacon,
// and clouds are gas kilometres across. See docs/db-5-notes.md#dungeon-schema-and-authoring-defaults.
func DunSolidByDefault(typeID, groupID int64) bool {
	if typeID == DungeonBeaconTypeID {
		return false
	}
	switch groupID {
	case 310, // Beacon (Celestial Beacon and kin)
		227, 312, 335, 711, // Cloud, Planetary Cloud, Temporary Cloud, Harvestable Cloud
		411: // Force Field
		return false
	}
	return true
}

// HPMultiplier is the HP scale this ball spawns with: the authored hpScale when positive,
// else 1.
func (o ObjectRow) HPMultiplier() float64 {
	if o.HPScale.Valid && o.HPScale.Float64 > 0 {
		return o.HPScale.Float64
	}
	return 1
}

// TriggerRow represents a row from dunTriggers: one event-to-action rule.
type TriggerRow struct {
	TriggerID        int64
	RoomID           int64
	Event            string // roomEnter | npcKilled | npcDamaged | roomCleared | gateActivated | itemPlaced
	TargetObjectID   sql.NullInt64
	Action           string // transmission | localChat | popup | spawnWave | music | completeDungeon
	Header           sql.NullString
	Icon             sql.NullString
	Body             sql.NullString // message text, or the track url when Action == "music"
	SenderName       sql.NullString
	DeferUntilLanded bool
	OnceOnly         bool
	DelayMs          int64
	SpawnWave        sql.NullInt64  // wave group to spawn when Action == "spawnWave"
	ChatColor        sql.NullString // localChat text color, "RRGGBB" hex (no alpha)
	ItemTypeID       sql.NullInt64  // itemPlaced: the type watched for; none matches any item
	ItemQty          int64          // itemPlaced: how many the container must hold, at least 1
	ItemName         string         // ItemTypeID's typeName, filled by GetRoomTriggers and never stored
}

// ItemPlacedMatches reports whether an itemPlaced trigger is met by a drop of droppedType into a
// container now holding held (typeID to total quantity). The count is the container's total, so
// several small drops add up to ItemQty.
func (t TriggerRow) ItemPlacedMatches(droppedType int64, held map[int64]int64) bool {
	if t.ItemTypeID.Valid && t.ItemTypeID.Int64 != 0 && t.ItemTypeID.Int64 != droppedType {
		return false
	}
	need := t.ItemQty
	if need < 1 {
		need = 1
	}
	return held[droppedType] >= need
}

// musicURLPattern admits only characters that are safe inside a quoted Python string.
var musicURLPattern = regexp.MustCompile(`^res:/[A-Za-z0-9_./-]+\.(?i:ogg|wav|mp3)$`)

// ValidMusicURL reports whether url may go into a music trigger. The url is written into code the
// client executes, so every path that sends one must pass this check first.
func ValidMusicURL(url string) bool {
	return musicURLPattern.MatchString(url)
}

// ResolveTransmissionIcon validates a numeric transmission-window icon ID. The client reads
// cfg.evegraphics.Get(int(icon)).icon (transmissionMgr.py), so the ID must be a graphicID with
// a non-empty icon. An ID that is not is translated through invTypes.graphicID, since authors
// paste typeIDs. Returns (0, false) when nothing resolves.
func ResolveTransmissionIcon(ctx context.Context, db *sql.DB, id int64) (int64, bool) {
	var g int64
	if err := db.QueryRowContext(ctx,
		`SELECT graphicID FROM eveGraphics WHERE graphicID = ? AND COALESCE(icon,'') <> ''`,
		id).Scan(&g); err == nil {
		return g, true
	}
	if err := db.QueryRowContext(ctx,
		`SELECT t.graphicID FROM invTypes t
		 JOIN eveGraphics g ON g.graphicID = t.graphicID
		 WHERE t.typeID = ? AND COALESCE(g.icon,'') <> ''`,
		id).Scan(&g); err == nil {
		return g, true
	}
	return 0, false
}

// GetRoomTriggers returns all triggers defined for a room.
func GetRoomTriggers(ctx context.Context, db *sql.DB, roomID int64) ([]TriggerRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT t.triggerID, t.roomID, t.event, t.targetObjectID, t.action, t.header, t.icon, t.body,
		        t.senderName, t.deferUntilLanded, t.onceOnly, t.delayMs, t.spawnWave, t.chatColor,
		        t.itemTypeID, t.itemQty, COALESCE(i.typeName, '')
		 FROM dunTriggers t LEFT JOIN invTypes i ON i.typeID = t.itemTypeID
		 WHERE t.roomID = ? ORDER BY t.triggerID`, roomID)
	if err != nil {
		return nil, fmt.Errorf("GetRoomTriggers: %w", err)
	}
	defer rows.Close()
	var out []TriggerRow
	for rows.Next() {
		var t TriggerRow
		var deferred, once int
		if err := rows.Scan(&t.TriggerID, &t.RoomID, &t.Event, &t.TargetObjectID, &t.Action,
			&t.Header, &t.Icon, &t.Body, &t.SenderName, &deferred, &once, &t.DelayMs, &t.SpawnWave, &t.ChatColor,
			&t.ItemTypeID, &t.ItemQty, &t.ItemName); err != nil {
			return nil, fmt.Errorf("GetRoomTriggers scan: %w", err)
		}
		t.DeferUntilLanded = deferred != 0
		t.OnceOnly = once != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

// SaveTrigger inserts (TriggerID == 0) or updates a trigger. Returns the triggerID.
func SaveTrigger(ctx context.Context, db *sql.DB, t TriggerRow) (int64, error) {
	if t.ItemQty < 1 {
		t.ItemQty = 1
	}
	deferred, once := 0, 0
	if t.DeferUntilLanded {
		deferred = 1
	}
	if t.OnceOnly {
		once = 1
	}
	if t.TriggerID == 0 {
		res, err := db.ExecContext(ctx,
			`INSERT INTO dunTriggers (roomID, event, targetObjectID, action, header, icon, body,
			        senderName, deferUntilLanded, onceOnly, delayMs, spawnWave, chatColor, itemTypeID, itemQty)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			t.RoomID, t.Event, t.TargetObjectID, t.Action, t.Header, t.Icon, t.Body,
			t.SenderName, deferred, once, t.DelayMs, t.SpawnWave, t.ChatColor, t.ItemTypeID, t.ItemQty)
		if err != nil {
			return 0, fmt.Errorf("SaveTrigger insert: %w", err)
		}
		return res.LastInsertId()
	}
	_, err := db.ExecContext(ctx,
		`UPDATE dunTriggers SET event=?, targetObjectID=?, action=?, header=?, icon=?, body=?,
		        senderName=?, deferUntilLanded=?, onceOnly=?, delayMs=?, spawnWave=?, chatColor=?,
		        itemTypeID=?, itemQty=?
		 WHERE triggerID=?`,
		t.Event, t.TargetObjectID, t.Action, t.Header, t.Icon, t.Body,
		t.SenderName, deferred, once, t.DelayMs, t.SpawnWave, t.ChatColor, t.ItemTypeID, t.ItemQty, t.TriggerID)
	if err != nil {
		return 0, fmt.Errorf("SaveTrigger update: %w", err)
	}
	return t.TriggerID, nil
}

// DeleteTrigger removes a trigger by ID.
func DeleteTrigger(ctx context.Context, db *sql.DB, triggerID int64) error {
	_, err := db.ExecContext(ctx, `DELETE FROM dunTriggers WHERE triggerID = ?`, triggerID)
	if err != nil {
		return fmt.Errorf("DeleteTrigger %d: %w", triggerID, err)
	}
	return nil
}

// GetDungeonFactions returns distinct factionIDs from dunDungeons as a util.Rowset.
func GetDungeonFactions(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `SELECT DISTINCT factionID FROM dunDungeons ORDER BY factionID`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var factionID int64
		if err := rows.Scan(&factionID); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{intRep(factionID)})
	}
	return makeRowset([]string{"factionID"}, lines), nil
}

// GetDungeons returns dungeons filtered by factionID as a util.Rowset.
func GetDungeons(ctx context.Context, db *sql.DB, factionID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT dungeonVID, dungeonVName, version, status FROM dunDungeons WHERE factionID = ? ORDER BY dungeonVName`,
		factionID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var d DungeonRow
		if err := rows.Scan(&d.DungeonVID, &d.DungeonVName, &d.Version, &d.Status); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{
			intRep(d.DungeonVID),
			marshal.PyString{Value: d.DungeonVName},
			intRep(d.Version),
			intRep(d.Status),
		})
	}
	return makeRowset([]string{"dungeonVID", "dungeonVName", "version", "status"}, lines), nil
}

// GetDungeonByVID returns a single dungeon by its VID as a util.Rowset.
func GetDungeonByVID(ctx context.Context, db *sql.DB, dungeonVID int64) (marshal.PyRep, error) {
	row := db.QueryRowContext(ctx,
		`SELECT dungeonVID, dungeonVName, factionID, version, status FROM dunDungeons WHERE dungeonVID = ?`,
		dungeonVID)
	var d DungeonRow
	if err := row.Scan(&d.DungeonVID, &d.DungeonVName, &d.FactionID, &d.Version, &d.Status); err != nil {
		return nil, err
	}
	return makeRowset([]string{"dungeonVID", "dungeonVName", "factionID", "version", "status"}, [][]marshal.PyRep{
		{intRep(d.DungeonVID), marshal.PyString{Value: d.DungeonVName}, intRep(d.FactionID), intRep(d.Version), intRep(d.Status)},
	}), nil
}

// GetRooms returns rooms in a dungeon as a util.Rowset.
func GetRooms(ctx context.Context, db *sql.DB, dungeonVID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT roomID, dungeonVID, roomName FROM dunRooms WHERE dungeonVID = ? ORDER BY COALESCE(roomOrder, roomID), roomID`,
		dungeonVID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var r RoomRow
		if err := rows.Scan(&r.RoomID, &r.DungeonVID, &r.RoomName); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{
			intRep(r.RoomID),
			intRep(r.DungeonVID),
			marshal.PyString{Value: r.RoomName},
		})
	}
	return makeRowset([]string{"roomID", "dungeonVID", "roomName"}, lines), nil
}

// GetRoomNaturalPhenomena reports whether the room's deadspace phenomena flag is set. A
// missing row reads as true, as for every stock deadspace site.
func GetRoomNaturalPhenomena(ctx context.Context, db *sql.DB, roomID int64) (bool, error) {
	var v int
	err := db.QueryRowContext(ctx,
		`SELECT COALESCE(naturalPhenomena, 1) FROM dunRooms WHERE roomID = ?`, roomID).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return true, fmt.Errorf("GetRoomNaturalPhenomena %d: %w", roomID, err)
	}
	return v != 0, nil
}

// SetRoomNaturalPhenomena writes the room's deadspace phenomena flag.
func SetRoomNaturalPhenomena(ctx context.Context, db *sql.DB, roomID int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	_, err := db.ExecContext(ctx,
		`UPDATE dunRooms SET naturalPhenomena = ? WHERE roomID = ?`, v, roomID)
	if err != nil {
		return fmt.Errorf("SetRoomNaturalPhenomena %d: %w", roomID, err)
	}
	return nil
}

// GetDungeonRoomIDs returns the roomIDs for a dungeon in display order (roomOrder, then roomID).
func GetDungeonRoomIDs(ctx context.Context, db *sql.DB, dungeonVID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT roomID FROM dunRooms WHERE dungeonVID = ? ORDER BY COALESCE(roomOrder, roomID), roomID`, dungeonVID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// CountDungeonNPCObjects counts a dungeon's objects, across every room, that spawn as live
// NPCs: category 6 or 11 types with a speed, entityCruiseSpeed (508) or maxVelocity (37) > 0,
// the split spawnDungeonObjects applies through GetNPCStats. Held wave groups count, so this is the full clear requirement.
func CountDungeonNPCObjects(ctx context.Context, db *sql.DB, dungeonVID int64) (int, error) {
	var n int
	err := db.QueryRowContext(ctx, `
		SELECT COUNT(*)
		  FROM dunObjects o
		  JOIN dunRooms r ON r.roomID = o.roomID
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE r.dungeonVID = ?
		   AND g.categoryID IN (6, 11)
		   AND EXISTS (SELECT 1 FROM dgmTypeAttributes a
		                WHERE a.typeID = o.typeID AND a.attributeID IN (37, 508)
		                  AND COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DECIMAL(20,6)), 0) > 0)`,
		dungeonVID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("CountDungeonNPCObjects dungeonVID=%d: %w", dungeonVID, err)
	}
	return n, nil
}

// RoomNPCObjectIDs returns the objectIDs in one room that spawn as live NPCs, by the split
// CountDungeonNPCObjects uses. The killAll site-clear check uses it to find held waves.
func RoomNPCObjectIDs(ctx context.Context, db *sql.DB, roomID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.objectID
		  FROM dunObjects o
		  JOIN invTypes t ON t.typeID = o.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE o.roomID = ?
		   AND g.categoryID IN (6, 11)
		   AND EXISTS (SELECT 1 FROM dgmTypeAttributes a
		                WHERE a.typeID = o.typeID AND a.attributeID IN (37, 508)
		                  AND COALESCE(NULLIF(a.valueFloat, 0), CAST(a.valueInt AS DECIMAL(20,6)), 0) > 0)`,
		roomID)
	if err != nil {
		return nil, fmt.Errorf("RoomNPCObjectIDs roomID=%d: %w", roomID, err)
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("RoomNPCObjectIDs scan: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// GetRoomObjects returns objects in a room as a slice of ObjectRow.
func GetRoomObjects(ctx context.Context, db *sql.DB, roomID int64) ([]ObjectRow, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT objectID, roomID, typeID, x, y, z, yaw, pitch, roll, radius, aggressionRange, stance,
		        npcFactionID, musicUrl, leashObjectID, orbitDistance, description, spawnWave, ballName, invulnerable, hpScale, solid
		 FROM dunObjects WHERE roomID = ? ORDER BY objectID`, roomID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var objs []ObjectRow
	for rows.Next() {
		var o ObjectRow
		if err := rows.Scan(&o.ObjectID, &o.RoomID, &o.TypeID,
			&o.X, &o.Y, &o.Z,
			&o.Yaw, &o.Pitch, &o.Roll,
			&o.Radius, &o.AggressionRange, &o.Stance,
			&o.NPCFactionID, &o.MusicUrl,
			&o.LeashObjectID, &o.OrbitDistance, &o.Description, &o.SpawnWave, &o.BallName, &o.Invulnerable, &o.HPScale, &o.Solid); err != nil {
			return nil, err
		}
		objs = append(objs, o)
	}
	return objs, nil
}

// GetRoomObjectsRowset returns objects in a room as a util.Rowset for the client.
func GetRoomObjectsRowset(ctx context.Context, db *sql.DB, roomID int64) (marshal.PyRep, error) {
	objs, err := GetRoomObjects(ctx, db, roomID)
	if err != nil {
		return nil, err
	}

	var lines [][]marshal.PyRep
	for _, o := range objs {
		var stanceRep marshal.PyRep = marshal.PyNone{}
		if o.Stance.Valid {
			stanceRep = marshal.PyString{Value: o.Stance.String}
		}
		var factionRep marshal.PyRep = marshal.PyNone{}
		if o.NPCFactionID.Valid {
			factionRep = intRep(o.NPCFactionID.Int64)
		}
		lines = append(lines, []marshal.PyRep{
			intRep(o.ObjectID),
			intRep(o.RoomID),
			intRep(o.TypeID),
			marshal.PyReal{Value: o.X},
			marshal.PyReal{Value: o.Y},
			marshal.PyReal{Value: o.Z},
			nullFloatRep(o.Yaw),
			nullFloatRep(o.Pitch),
			nullFloatRep(o.Roll),
			nullFloatRep(o.Radius),
			nullFloatRep(o.AggressionRange),
			stanceRep,
			factionRep,
		})
	}
	return makeRowset([]string{
		"objectID", "roomID", "typeID", "x", "y", "z",
		"yaw", "pitch", "roll", "radius", "aggressionRange", "stance",
		"npcFactionID",
	}, lines), nil
}

// EditObjectFields holds the optional fields EditObject can update. Only non-nil fields are
// written.
type EditObjectFields struct {
	X, Y, Z          *float64
	Yaw, Pitch, Roll *float64
	Radius           *float64
	AggressionRange  *float64
	Stance           *string // "" or "aggressive" = clear (NULL)
	NPCFactionID     *int64
	MusicUrl         *string
	LeashObjectID    *int64   // 0 = clear (NULL)
	OrbitDistance    *float64 // 0 = clear (NULL)
	Description      *string  // "" = clear (NULL)
	SpawnWave        *int64   // 0 = clear (NULL); spawns with the room
	BallName         *string  // "" = clear (NULL)
	Invulnerable     *bool    // structure takes no damage and cannot be destroyed
	HPScale          *float64 // <= 0 or exactly 1 = clear (NULL)
	Solid            *bool    // collidable override
	SolidDefault     bool     // true = clear the override (NULL); wins over Solid
	LootMinDrops     *int64   // <= 0 = clear (NULL)
	LootMaxDrops     *int64   // <= 0 = clear (NULL)
	RoomID           *int64   // move the ball to another room of the same dungeon
}

// EditObject updates only the non-nil fields on a dungeon object.
func EditObject(ctx context.Context, db DBTX, objectID int64, f EditObjectFields) error {
	var sets []string
	var args []interface{}
	if f.X != nil {
		sets = append(sets, "x=?")
		args = append(args, *f.X)
	}
	if f.Y != nil {
		sets = append(sets, "y=?")
		args = append(args, *f.Y)
	}
	if f.Z != nil {
		sets = append(sets, "z=?")
		args = append(args, *f.Z)
	}
	if f.Yaw != nil {
		sets = append(sets, "yaw=?")
		args = append(args, *f.Yaw)
	}
	if f.Pitch != nil {
		sets = append(sets, "pitch=?")
		args = append(args, *f.Pitch)
	}
	if f.Roll != nil {
		sets = append(sets, "roll=?")
		args = append(args, *f.Roll)
	}
	if f.Radius != nil {
		sets = append(sets, "radius=?")
		args = append(args, *f.Radius)
	}
	if f.AggressionRange != nil {
		sets = append(sets, "aggressionRange=?")
		args = append(args, *f.AggressionRange)
	}
	if f.Stance != nil {
		if st := StoredStance(*f.Stance); st == "" {
			sets = append(sets, "stance=NULL")
		} else {
			sets = append(sets, "stance=?")
			args = append(args, st)
		}
	}
	if f.NPCFactionID != nil {
		if *f.NPCFactionID == 0 {
			sets = append(sets, "npcFactionID=NULL")
		} else {
			sets = append(sets, "npcFactionID=?")
			args = append(args, *f.NPCFactionID)
		}
	}
	if f.MusicUrl != nil {
		if *f.MusicUrl == "" {
			sets = append(sets, "musicUrl=NULL")
		} else {
			sets = append(sets, "musicUrl=?")
			args = append(args, *f.MusicUrl)
		}
	}
	if f.LeashObjectID != nil {
		if *f.LeashObjectID == 0 {
			sets = append(sets, "leashObjectID=NULL")
		} else {
			sets = append(sets, "leashObjectID=?")
			args = append(args, *f.LeashObjectID)
		}
	}
	if f.OrbitDistance != nil {
		if *f.OrbitDistance == 0 {
			sets = append(sets, "orbitDistance=NULL")
		} else {
			sets = append(sets, "orbitDistance=?")
			args = append(args, *f.OrbitDistance)
		}
	}
	if f.Description != nil {
		if *f.Description == "" {
			sets = append(sets, "description=NULL")
		} else {
			sets = append(sets, "description=?")
			args = append(args, *f.Description)
		}
	}
	if f.SpawnWave != nil {
		if *f.SpawnWave == 0 {
			sets = append(sets, "spawnWave=NULL")
		} else {
			sets = append(sets, "spawnWave=?")
			args = append(args, *f.SpawnWave)
		}
	}
	if f.BallName != nil {
		if *f.BallName == "" {
			sets = append(sets, "ballName=NULL")
		} else {
			sets = append(sets, "ballName=?")
			args = append(args, *f.BallName)
		}
	}
	if f.Invulnerable != nil {
		sets = append(sets, "invulnerable=?")
		if *f.Invulnerable {
			args = append(args, 1)
		} else {
			args = append(args, 0)
		}
	}
	if f.SolidDefault {
		sets = append(sets, "solid=NULL")
	} else if f.Solid != nil {
		sets = append(sets, "solid=?")
		args = append(args, *f.Solid)
	}
	if f.HPScale != nil {
		if *f.HPScale <= 0 || *f.HPScale == 1 {
			sets = append(sets, "hpScale=NULL")
		} else {
			sets = append(sets, "hpScale=?")
			args = append(args, *f.HPScale)
		}
	}
	if f.LootMinDrops != nil {
		if *f.LootMinDrops <= 0 {
			sets = append(sets, "lootMinDrops=NULL")
		} else {
			sets = append(sets, "lootMinDrops=?")
			args = append(args, *f.LootMinDrops)
		}
	}
	if f.LootMaxDrops != nil {
		if *f.LootMaxDrops <= 0 {
			sets = append(sets, "lootMaxDrops=NULL")
		} else {
			sets = append(sets, "lootMaxDrops=?")
			args = append(args, *f.LootMaxDrops)
		}
	}
	if f.RoomID != nil {
		sets = append(sets, "roomID=?")
		args = append(args, *f.RoomID)
	}
	if len(sets) == 0 {
		return nil
	}
	query := "UPDATE dunObjects SET "
	for i, s := range sets {
		if i > 0 {
			query += ", "
		}
		query += s
	}
	query += " WHERE objectID=?"
	args = append(args, objectID)
	_, err := db.ExecContext(ctx, query, args...)
	return err
}

// CopyObject duplicates a dungeon object with a position offset. Returns the new objectID.
func CopyObject(ctx context.Context, db DBTX, objectID int64, offsetX, offsetY, offsetZ float64) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO dunObjects (roomID, typeID, x, y, z, yaw, pitch, roll, radius, aggressionRange, stance, npcFactionID, musicUrl, leashObjectID, orbitDistance, description, spawnWave, ballName, invulnerable, hpScale, solid, lootMinDrops, lootMaxDrops)
		 SELECT roomID, typeID, x+?, y+?, z+?, yaw, pitch, roll, radius, aggressionRange, stance, npcFactionID, musicUrl, leashObjectID, orbitDistance, description, spawnWave, ballName, invulnerable, hpScale, solid, lootMinDrops, lootMaxDrops
		 FROM dunObjects WHERE objectID = ?`,
		offsetX, offsetY, offsetZ, objectID)
	if err != nil {
		return 0, err
	}
	newID, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	// A copied ball keeps its authored drop list.
	if err := CopyObjectLoot(ctx, db, objectID, newID); err != nil {
		return newID, err
	}
	return newID, nil
}

// GetRoomBeacon returns the room-local position of the first beacon in a room: the room's
// warp-in point for acceleration gates and mission warp-ins. found is false when the room has
// no beacon.
func GetRoomBeacon(ctx context.Context, db *sql.DB, roomID int64) (x, y, z float64, found bool, err error) {
	// The editor's beacon (typeID 10124) is in group 226, not 310; match either.
	row := db.QueryRowContext(ctx,
		`SELECT o.x, o.y, o.z FROM dunObjects o
		 JOIN invTypes t ON t.typeID = o.typeID
		 WHERE o.roomID = ? AND (t.groupID = 310 OR o.typeID = 10124)
		 ORDER BY o.objectID LIMIT 1`, roomID)
	switch scanErr := row.Scan(&x, &y, &z); scanErr {
	case nil:
		return x, y, z, true, nil
	case sql.ErrNoRows:
		return 0, 0, 0, false, nil
	default:
		return 0, 0, 0, false, scanErr
	}
}

// RemoveObject deletes a dungeon object.
func RemoveObject(ctx context.Context, db DBTX, objectID int64) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM dunObjectLoot WHERE objectID = ?`, objectID); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx, `DELETE FROM dunObjects WHERE objectID = ?`, objectID)
	return err
}

// CreateDungeon inserts a new dungeon with a default room and returns its dungeonVID.
func CreateDungeon(ctx context.Context, db *sql.DB, name string, factionID int64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateDungeon: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx,
		`INSERT INTO dunDungeons (dungeonVName, factionID) VALUES (?, ?)`,
		name, factionID)
	if err != nil {
		return 0, fmt.Errorf("CreateDungeon: %w", err)
	}
	vid, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateDungeon lastInsertId: %w", err)
	}

	// A default room, so the editor always has one.
	res, err = tx.ExecContext(ctx,
		`INSERT INTO dunRooms (dungeonVID, roomName) VALUES (?, ?)`,
		vid, "Room 1")
	if err != nil {
		return 0, fmt.Errorf("CreateDungeon default room: %w", err)
	}
	roomID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateDungeon default room id: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO dunObjects (roomID, typeID, x, y, z) VALUES (?, ?, 0, 0, 0)`,
		roomID, DungeonBeaconTypeID); err != nil {
		return 0, fmt.Errorf("CreateDungeon default beacon: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("CreateDungeon commit: %w", err)
	}
	return vid, nil
}

// DungeonBeaconTypeID is the beacon every authored room is built around. It is in group
// 226, not 310; GetRoomBeacon matches either.
const DungeonBeaconTypeID = 10124

// SeedRoomBeacon places a room's beacon at the local origin; a room is anchored by its
// beacon's local position. Not called on import: an imported room brings its own beacon.
// See docs/db-5-notes.md#dungeon-schema-and-authoring-defaults.
func SeedRoomBeacon(ctx context.Context, db *sql.DB, roomID int64) error {
	_, err := AddObject(ctx, db, roomID, DungeonBeaconTypeID, 0, 0, 0)
	return err
}

// CreateRoom inserts a new room in a dungeon and returns its roomID.
func CreateRoom(ctx context.Context, db *sql.DB, dungeonVID int64, roomName string) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO dunRooms (dungeonVID, roomName) VALUES (?, ?)`,
		dungeonVID, roomName)
	if err != nil {
		return 0, fmt.Errorf("CreateRoom: %w", err)
	}
	return res.LastInsertId()
}

// AddObject inserts a new object into a room and returns its objectID.
func AddObject(ctx context.Context, db DBTX, roomID, typeID int64, x, y, z float64) (int64, error) {
	res, err := db.ExecContext(ctx,
		`INSERT INTO dunObjects (roomID, typeID, x, y, z) VALUES (?, ?, ?, ?, ?)`,
		roomID, typeID, x, y, z)
	if err != nil {
		return 0, fmt.Errorf("AddObject: %w", err)
	}
	return res.LastInsertId()
}

// GetDECategories returns item categories for the dungeon editor browser, excluding
// non-space categories.
func GetDECategories(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `SELECT categoryID, categoryName FROM invCategories WHERE categoryID NOT IN (`+excludedDECategories+`) ORDER BY categoryName`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{intRep(id), marshal.PyString{Value: name}})
	}
	return makeRowset([]string{"categoryID", "categoryName"}, lines), nil
}

// GetDEGroups returns groups within a category for the dungeon editor browser.
func GetDEGroups(ctx context.Context, db *sql.DB, categoryID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT groupID, groupName, categoryID FROM invGroups WHERE categoryID = ? ORDER BY groupName`,
		categoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var groupID, catID int64
		var name string
		if err := rows.Scan(&groupID, &name, &catID); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{intRep(groupID), marshal.PyString{Value: name}, intRep(catID)})
	}
	return makeRowset([]string{"groupID", "groupName", "categoryID"}, lines), nil
}

// GetDETypes returns types within a group for the dungeon editor browser,
// excluding types in non-space categories.
func GetDETypes(ctx context.Context, db *sql.DB, groupID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, COALESCE(t.radius, 100)
		FROM invTypes t
		JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.groupID = ? AND g.categoryID NOT IN (`+excludedDECategories+`)
		ORDER BY t.typeName`,
		groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var typeID, grpID int64
		var name string
		var radius float64
		if err := rows.Scan(&typeID, &name, &grpID, &radius); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{
			intRep(typeID), marshal.PyString{Value: name}, intRep(grpID), marshal.PyReal{Value: radius},
		})
	}
	return makeRowset([]string{"typeID", "typeName", "groupID", "radius"}, lines), nil
}

// SearchDETypes searches invTypes by name (RLIKE) for the dungeon editor browser,
// excluding types in non-space categories.
func SearchDETypes(ctx context.Context, db *sql.DB, query string) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.typeName, t.groupID, COALESCE(g.categoryID, 0), COALESCE(t.radius, 100)
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeName RLIKE ? AND COALESCE(g.categoryID, 0) NOT IN (`+excludedDECategories+`)
		ORDER BY t.typeName LIMIT 200`, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines [][]marshal.PyRep
	for rows.Next() {
		var typeID, grpID, catID int64
		var name string
		var radius float64
		if err := rows.Scan(&typeID, &name, &grpID, &catID, &radius); err != nil {
			return nil, err
		}
		lines = append(lines, []marshal.PyRep{
			intRep(typeID), marshal.PyString{Value: name}, intRep(grpID), intRep(catID), marshal.PyReal{Value: radius},
		})
	}
	return makeRowset([]string{"typeID", "typeName", "groupID", "categoryID", "radius"}, lines), nil
}

// TypeInfo holds the groupID and categoryID for an invTypes row.
type TypeInfo struct {
	GroupID    int64
	CategoryID int64
	Radius     float64
}

// GetTypeInfo returns the groupID, categoryID, and radius for a typeID.
func GetTypeInfo(ctx context.Context, db *sql.DB, typeID int64) (TypeInfo, error) {
	var ti TypeInfo
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(g.groupID, 0), COALESCE(g.categoryID, 0), COALESCE(t.radius, 100)
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, typeID).Scan(&ti.GroupID, &ti.CategoryID, &ti.Radius)
	if err != nil {
		return ti, fmt.Errorf("GetTypeInfo(%d): %w", typeID, err)
	}
	return ti, nil
}

// DeleteRoom deletes all objects and triggers in a room, then the room itself.
func DeleteRoom(ctx context.Context, db *sql.DB, roomID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE l FROM dunObjectLoot l JOIN dunObjects o ON o.objectID = l.objectID WHERE o.roomID = ?`,
		roomID); err != nil {
		return fmt.Errorf("DeleteRoom object loot: %w", err)
	}
	_, err := db.ExecContext(ctx, `DELETE FROM dunObjects WHERE roomID = ?`, roomID)
	if err != nil {
		return fmt.Errorf("DeleteRoom objects: %w", err)
	}
	if _, err := db.ExecContext(ctx, `DELETE FROM dunTriggers WHERE roomID = ?`, roomID); err != nil {
		return fmt.Errorf("DeleteRoom triggers: %w", err)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM dunRooms WHERE roomID = ?`, roomID)
	if err != nil {
		return fmt.Errorf("DeleteRoom room: %w", err)
	}
	return nil
}

// RenameRoom updates the name of a dungeon room.
func RenameRoom(ctx context.Context, db *sql.DB, roomID int64, newName string) error {
	_, err := db.ExecContext(ctx, `UPDATE dunRooms SET roomName = ? WHERE roomID = ?`, newName, roomID)
	if err != nil {
		return fmt.Errorf("RenameRoom: %w", err)
	}
	return nil
}

// ReorderRooms sets the display order of a dungeon's rooms to match roomIDs. Only rows of
// dungeonVID are updated, so a stray ID cannot move another dungeon's room.
func ReorderRooms(ctx context.Context, db *sql.DB, dungeonVID int64, roomIDs []int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ReorderRooms begin: %w", err)
	}
	defer tx.Rollback()
	for i, roomID := range roomIDs {
		if _, err := tx.ExecContext(ctx,
			`UPDATE dunRooms SET roomOrder = ? WHERE roomID = ? AND dungeonVID = ?`,
			i+1, roomID, dungeonVID); err != nil {
			return fmt.Errorf("ReorderRooms room %d: %w", roomID, err)
		}
	}
	return tx.Commit()
}

// UpdateDungeonMeta updates the name, faction and DED rating of a dungeon.
// dedLevel is clamped to 0 (unrated) .. 10.
func UpdateDungeonMeta(ctx context.Context, db *sql.DB, dungeonVID int64, name string, factionID, dedLevel int64) error {
	dedLevel = max(0, min(10, dedLevel))
	_, err := db.ExecContext(ctx,
		`UPDATE dunDungeons SET dungeonVName = ?, factionID = ?, dedLevel = ? WHERE dungeonVID = ?`,
		name, factionID, dedLevel, dungeonVID)
	if err != nil {
		return fmt.Errorf("UpdateDungeonMeta: %w", err)
	}
	return nil
}

// DeleteDungeon cascade-deletes all objects, rooms, and the dungeon itself.
func DeleteDungeon(ctx context.Context, db *sql.DB, dungeonVID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE l FROM dunObjectLoot l
		 JOIN dunObjects o ON o.objectID = l.objectID
		 JOIN dunRooms r ON r.roomID = o.roomID WHERE r.dungeonVID = ?`,
		dungeonVID); err != nil {
		return fmt.Errorf("DeleteDungeon object loot: %w", err)
	}
	_, err := db.ExecContext(ctx,
		`DELETE o FROM dunObjects o JOIN dunRooms r ON r.roomID = o.roomID WHERE r.dungeonVID = ?`,
		dungeonVID)
	if err != nil {
		return fmt.Errorf("DeleteDungeon objects: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE t FROM dunTriggers t JOIN dunRooms r ON r.roomID = t.roomID WHERE r.dungeonVID = ?`,
		dungeonVID); err != nil {
		return fmt.Errorf("DeleteDungeon triggers: %w", err)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM dunRooms WHERE dungeonVID = ?`, dungeonVID)
	if err != nil {
		return fmt.Errorf("DeleteDungeon rooms: %w", err)
	}
	_, err = db.ExecContext(ctx, `DELETE FROM dunDungeons WHERE dungeonVID = ?`, dungeonVID)
	if err != nil {
		return fmt.Errorf("DeleteDungeon dungeon: %w", err)
	}
	return nil
}

// GetObjectByID returns a single dungeon object by its objectID.
func GetObjectByID(ctx context.Context, db *sql.DB, objectID int64) (*ObjectRow, error) {
	o := &ObjectRow{}
	err := db.QueryRowContext(ctx,
		`SELECT objectID, roomID, typeID, x, y, z, yaw, pitch, roll, radius, aggressionRange, stance,
		        npcFactionID, musicUrl, leashObjectID, orbitDistance, description, spawnWave, ballName, invulnerable, hpScale, solid
		 FROM dunObjects WHERE objectID = ?`, objectID,
	).Scan(&o.ObjectID, &o.RoomID, &o.TypeID,
		&o.X, &o.Y, &o.Z,
		&o.Yaw, &o.Pitch, &o.Roll,
		&o.Radius, &o.AggressionRange, &o.Stance,
		&o.NPCFactionID, &o.MusicUrl,
		&o.LeashObjectID, &o.OrbitDistance, &o.Description, &o.SpawnWave, &o.BallName, &o.Invulnerable, &o.HPScale, &o.Solid)
	if err != nil {
		return nil, fmt.Errorf("GetObjectByID(%d): %w", objectID, err)
	}
	return o, nil
}

// GetDungeonFactionDED returns a dungeon template's faction (0 = none) and DED
// rating (0 = unrated).
func GetDungeonFactionDED(ctx context.Context, db *sql.DB, dungeonVID int64) (factionID int64, dedLevel int, err error) {
	err = db.QueryRowContext(ctx,
		`SELECT COALESCE(factionID, 0), COALESCE(dedLevel, 0) FROM dunDungeons WHERE dungeonVID = ?`,
		dungeonVID).Scan(&factionID, &dedLevel)
	return
}
