package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Custom celestials are admin-seeded mapDenormalize rows (planets, moons, belts, beacons)
// tracked in customCelestials; a beacon may link to a dungeon. itemIDs come from
// [CustomCelestialIDFloor, CustomCelestialIDCeil], inside the client's celestial band.
const (
	CustomCelestialIDFloor = 45_000_000
	CustomCelestialIDCeil  = 49_999_999
)

// EnsureCustomCelestialTable creates the customCelestials tracking table. Call once at
// startup after opening the DB.
func EnsureCustomCelestialTable(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS customCelestials (
			itemID         BIGINT PRIMARY KEY,
			solarSystemID  INT NOT NULL,
			dungeonVID     INT DEFAULT NULL,
			dungeonRoomIdx INT NOT NULL DEFAULT 1,
			createdAt      TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
			INDEX idx_ccel_system (solarSystemID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	if err != nil {
		return fmt.Errorf("EnsureCustomCelestialTable: %w", err)
	}
	return nil
}

// CustomCelestialSeed holds everything needed to insert one new celestial.
type CustomCelestialSeed struct {
	SolarSystemID   int64
	ConstellationID int64
	RegionID        int64
	TypeID          int64
	GroupID         int64
	OrbitID         int64 // parent body itemID (0 = none)
	X, Y, Z         float64
	Radius          float64
	Name            string
	Security        float64
	DungeonVID      int64 // 0 = plain celestial, >0 = warp spawns this dungeon
	DungeonRoomIdx  int   // 1-based room index within the dungeon
}

// InsertCustomCelestial allocates the next itemID in the custom band, inserts the
// mapDenormalize row and the customCelestials row atomically, and returns the itemID.
func InsertCustomCelestial(ctx context.Context, db *sql.DB, seed CustomCelestialSeed) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("InsertCustomCelestial begin: %w", err)
	}
	defer tx.Rollback()

	var itemID int64
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(itemID), ?) + 1
		  FROM mapDenormalize
		 WHERE itemID BETWEEN ? AND ?
		 FOR UPDATE`,
		CustomCelestialIDFloor-1, CustomCelestialIDFloor, CustomCelestialIDCeil,
	).Scan(&itemID); err != nil {
		return 0, fmt.Errorf("InsertCustomCelestial alloc: %w", err)
	}
	if itemID > CustomCelestialIDCeil {
		return 0, fmt.Errorf("InsertCustomCelestial: custom celestial ID band exhausted")
	}

	orbitID := sql.NullInt64{Int64: seed.OrbitID, Valid: seed.OrbitID != 0}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mapDenormalize
			(itemID, typeID, groupID, solarSystemID, constellationID, regionID,
			 orbitID, x, y, z, radius, itemName, security)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		itemID, seed.TypeID, seed.GroupID, seed.SolarSystemID,
		seed.ConstellationID, seed.RegionID, orbitID,
		seed.X, seed.Y, seed.Z, seed.Radius, seed.Name, seed.Security,
	); err != nil {
		return 0, fmt.Errorf("InsertCustomCelestial mapDenormalize: %w", err)
	}

	dvid := sql.NullInt64{Int64: seed.DungeonVID, Valid: seed.DungeonVID > 0}
	roomIdx := seed.DungeonRoomIdx
	if roomIdx < 1 {
		roomIdx = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO customCelestials (itemID, solarSystemID, dungeonVID, dungeonRoomIdx)
		VALUES (?, ?, ?, ?)`,
		itemID, seed.SolarSystemID, dvid, roomIdx,
	); err != nil {
		return 0, fmt.Errorf("InsertCustomCelestial tracking: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("InsertCustomCelestial commit: %w", err)
	}
	invalidateSharedCelestials(seed.SolarSystemID)
	return itemID, nil
}

// CustomCelestialRow describes one tracked custom celestial in a system.
type CustomCelestialRow struct {
	ItemID         int64
	DungeonVID     int64 // 0 = no dungeon link
	DungeonRoomIdx int
	DungeonName    string
}

// GetCustomCelestials returns the tracked custom celestials for one system, keyed by
// itemID, with the linked dungeon name resolved when present.
func GetCustomCelestials(ctx context.Context, db *sql.DB, solarSystemID int64) (map[int64]CustomCelestialRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.itemID, COALESCE(c.dungeonVID, 0), c.dungeonRoomIdx,
		       COALESCE(d.dungeonVName, '')
		  FROM customCelestials c
		  LEFT JOIN dunDungeons d ON d.dungeonVID = c.dungeonVID
		 WHERE c.solarSystemID = ?`, solarSystemID)
	if err != nil {
		return nil, fmt.Errorf("GetCustomCelestials: %w", err)
	}
	defer rows.Close()
	out := make(map[int64]CustomCelestialRow)
	for rows.Next() {
		var r CustomCelestialRow
		if err := rows.Scan(&r.ItemID, &r.DungeonVID, &r.DungeonRoomIdx, &r.DungeonName); err != nil {
			return nil, fmt.Errorf("GetCustomCelestials scan: %w", err)
		}
		out[r.ItemID] = r
	}
	return out, rows.Err()
}

// GetCustomCelestialDungeon returns the dungeon link for a custom celestial. ok is false
// when the itemID is not tracked or has no dungeon attached.
func GetCustomCelestialDungeon(ctx context.Context, db *sql.DB, itemID int64) (dungeonVID int64, roomIdx int, ok bool, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT COALESCE(dungeonVID, 0), dungeonRoomIdx
		  FROM customCelestials WHERE itemID = ?`, itemID,
	).Scan(&dungeonVID, &roomIdx)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, fmt.Errorf("GetCustomCelestialDungeon(%d): %w", itemID, err)
	}
	return dungeonVID, roomIdx, dungeonVID > 0, nil
}

// GetCustomCelestialPosition returns a custom celestial's system and position from its
// mapDenormalize row. ok is false when the itemID has no row.
func GetCustomCelestialPosition(ctx context.Context, db *sql.DB, itemID int64) (solarSystemID int64, x, y, z float64, ok bool, err error) {
	err = db.QueryRowContext(ctx, `
		SELECT solarSystemID, x, y, z FROM mapDenormalize WHERE itemID = ?`, itemID,
	).Scan(&solarSystemID, &x, &y, &z)
	if err == sql.ErrNoRows {
		return 0, 0, 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("GetCustomCelestialPosition(%d): %w", itemID, err)
	}
	return solarSystemID, x, y, z, true, nil
}

// DeleteCustomCelestial removes a custom celestial from both tables. It deletes only rows
// tracked in customCelestials, never original map data.
func DeleteCustomCelestial(ctx context.Context, db *sql.DB, itemID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("DeleteCustomCelestial begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `DELETE FROM customCelestials WHERE itemID = ?`, itemID)
	if err != nil {
		return fmt.Errorf("DeleteCustomCelestial tracking: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("itemID %d is not a custom celestial", itemID)
	}
	var sysID sql.NullInt64
	_ = tx.QueryRowContext(ctx,
		`SELECT solarSystemID FROM mapDenormalize WHERE itemID = ?`, itemID).Scan(&sysID)
	if _, err := tx.ExecContext(ctx, `DELETE FROM mapDenormalize WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteCustomCelestial mapDenormalize: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("DeleteCustomCelestial commit: %w", err)
	}
	invalidateSharedCelestials(sysID.Int64)
	return nil
}

// invalidateSharedCelestials drops a system from SharedCelestialCache after its
// mapDenormalize rows changed, so players and modules reading through it see the change.
func invalidateSharedCelestials(solarSystemID int64) {
	if cc := SharedCelestialCache; cc != nil && solarSystemID != 0 {
		cc.InvalidateSystem(solarSystemID)
	}
}

// CelestialTypeOption is one invTypes choice for the seed form dropdowns.
type CelestialTypeOption struct {
	TypeID   int64
	TypeName string
	GroupID  int64
}

// ListCelestialSeedTypes returns the types usable as seeded celestials: planets (7),
// moons (8), asteroid belts (9) and beacons (310).
func ListCelestialSeedTypes(ctx context.Context, db *sql.DB) ([]CelestialTypeOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT typeID, typeName, groupID
		  FROM invTypes
		 WHERE groupID IN (7, 8, 9, 310)
		 ORDER BY groupID, typeName`)
	if err != nil {
		return nil, fmt.Errorf("ListCelestialSeedTypes: %w", err)
	}
	defer rows.Close()
	var out []CelestialTypeOption
	for rows.Next() {
		var t CelestialTypeOption
		if err := rows.Scan(&t.TypeID, &t.TypeName, &t.GroupID); err != nil {
			return nil, fmt.Errorf("ListCelestialSeedTypes scan: %w", err)
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// DungeonOption is one dunDungeons choice for the dungeon-beacon dropdown.
type DungeonOption struct {
	DungeonVID int64
	Name       string
	RoomCount  int
}

// ListDungeonOptions returns all authored dungeons with their room counts.
func ListDungeonOptions(ctx context.Context, db *sql.DB) ([]DungeonOption, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT d.dungeonVID, d.dungeonVName,
		       (SELECT COUNT(*) FROM dunRooms r WHERE r.dungeonVID = d.dungeonVID)
		  FROM dunDungeons d
		 ORDER BY d.dungeonVName`)
	if err != nil {
		return nil, fmt.Errorf("ListDungeonOptions: %w", err)
	}
	defer rows.Close()
	var out []DungeonOption
	for rows.Next() {
		var d DungeonOption
		if err := rows.Scan(&d.DungeonVID, &d.Name, &d.RoomCount); err != nil {
			return nil, fmt.Errorf("ListDungeonOptions scan: %w", err)
		}
		out = append(out, d)
	}
	return out, rows.Err()
}
