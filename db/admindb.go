package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// ItemSearchResult holds one row returned by ItemSearch.
type ItemSearchResult struct {
	TypeID   uint32
	TypeName string
}

// ItemSearch returns all invTypes whose typeName matches the given SQL RLIKE
// pattern.
func ItemSearch(ctx context.Context, db *sql.DB, query string) ([]ItemSearchResult, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, typeName FROM invTypes WHERE typeName RLIKE ?`, query)
	if err != nil {
		return nil, fmt.Errorf("ItemSearch: %w", err)
	}
	defer rows.Close()

	var results []ItemSearchResult
	for rows.Next() {
		var r ItemSearchResult
		if err := rows.Scan(&r.TypeID, &r.TypeName); err != nil {
			return nil, fmt.Errorf("ItemSearch scan: %w", err)
		}
		results = append(results, r)
	}
	return results, rows.Err()
}

// GiveISK adds amount to the balance of the given characterID in character_.
func GiveISK(ctx context.Context, db *sql.DB, charID int64, amount float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE character_ SET balance = balance + ? WHERE characterID = ?`,
		amount, charID)
	if err != nil {
		return fmt.Errorf("GiveISK: %w", err)
	}
	return nil
}

// GetCharBalance returns the current balance for charID from character_.
func GetCharBalance(ctx context.Context, db *sql.DB, charID int64) (float64, error) {
	var bal float64
	err := db.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ?`, charID).Scan(&bal)
	if err != nil {
		return 0, fmt.Errorf("GetCharBalance: %w", err)
	}
	return bal, nil
}

// StationLocation holds the location hierarchy for a station.
type StationLocation struct {
	StationID       int64
	StationName     string
	SolarSystemID   int64
	ConstellationID int64
	RegionID        int64
}

// GetStationLocation returns the station name and full location hierarchy
// (solar system, constellation, region) for a given stationID.
func GetStationLocation(ctx context.Context, db *sql.DB, stationID int64) (*StationLocation, error) {
	sl := &StationLocation{StationID: stationID}
	err := db.QueryRowContext(ctx, `
		SELECT stationName, solarSystemID, constellationID, regionID
		FROM staStations
		WHERE stationID = ?`, stationID,
	).Scan(&sl.StationName, &sl.SolarSystemID, &sl.ConstellationID, &sl.RegionID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("GetStationLocation: station %d not found", stationID)
		}
		return nil, fmt.Errorf("GetStationLocation: %w", err)
	}
	return sl, nil
}

// CreateItem inserts a new entity with the given typeID, ownerID and locationID at
// flag 4 (hangar). Ships are assembled singletons, everything else is packaged.
// Returns the new itemID.
func CreateItem(ctx context.Context, db DBTX, typeID, ownerID, locationID int64, quantity int) (int64, error) {
	// Look up the type name and category to see whether it is a singleton type.
	var typeName string
	var categoryID int64
	err := db.QueryRowContext(ctx, `
		SELECT t.typeName, COALESCE(g.categoryID, 0)
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, typeID,
	).Scan(&typeName, &categoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("CreateItem: typeID %d not found in invTypes", typeID)
		}
		return 0, fmt.Errorf("CreateItem: %w", err)
	}

	// Ships (categoryID=6) and blueprints (categoryID=9) are singletons with quantity=1.
	singleton := 0
	qty := quantity
	if categoryID == 6 || categoryID == 9 {
		singleton = 1
		qty = 1
	}

	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 4, 0, ?, ?, 0, 0, 0, NULL)`,
		typeName, typeID, ownerID, locationID, singleton, qty,
	)
	if err != nil {
		return 0, fmt.Errorf("CreateItem insert: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateItem LastInsertId: %w", err)
	}

	// A blueprint type also gets its invBlueprints row.
	_ = EnsureBlueprintRow(ctx, db, itemID, typeID)

	return itemID, nil
}

// GetCharCorpID returns the current corporationID for a character.
func GetCharCorpID(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	var corpID int64
	err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID).Scan(&corpID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, fmt.Errorf("GetCharCorpID: character %d not found", charID)
		}
		return 0, fmt.Errorf("GetCharCorpID: %w", err)
	}
	return corpID, nil
}

// CorpExists returns true if a corporation with the given ID exists.
func CorpExists(ctx context.Context, db *sql.DB, corpID int64) (bool, error) {
	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM corporation WHERE corporationID = ?`, corpID).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("CorpExists: %w", err)
	}
	return count > 0, nil
}

// GiveSkill grants (or updates) one skill at the given level (1-5). It validates that
// typeID is a skill (categoryID 16), creates the entity if the character lacks it, and
// sets the level and SP attributes. Returns the skill's typeName.
func GiveSkill(ctx context.Context, db *sql.DB, charID, typeID int64, level int) (string, error) {
	// typeID must be a skill (categoryID 16).
	var typeName string
	var categoryID int64
	err := db.QueryRowContext(ctx, `
		SELECT t.typeName, COALESCE(g.categoryID, 0)
		FROM invTypes t
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE t.typeID = ?`, typeID,
	).Scan(&typeName, &categoryID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", fmt.Errorf("typeID %d not found in invTypes", typeID)
		}
		return "", fmt.Errorf("GiveSkill lookup: %w", err)
	}
	if categoryID != 16 {
		return "", fmt.Errorf("typeID %d (%s) is not a skill (categoryID=%d, need 16)", typeID, typeName, categoryID)
	}

	// Skills live in the character (locationID = charID, flag 7), not in its ship or
	// capsule: the client never primes that container, and godma raises KeyError per skill.
	locationID := charID

	// Calculate SP for the target level.
	tc, _ := GetSkillTimeConstant(ctx, db, typeID)
	sp := int64(skillSPForLevelAdmin(tc, level))

	// Does the character already have this skill?
	var existingItemID sql.NullInt64
	_ = db.QueryRowContext(ctx,
		`SELECT itemID FROM entity WHERE typeID = ? AND ownerID = ? AND flag IN (7, 61) LIMIT 1`,
		typeID, charID,
	).Scan(&existingItemID)

	var itemID int64
	if existingItemID.Valid {
		itemID = existingItemID.Int64
	} else {
		// Insert new skill entity.
		r, err := db.ExecContext(ctx, `
			INSERT INTO entity
			  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
			VALUES ('Skill', ?, ?, ?, 7, 0, 1, 1, 0, 0, 0, NULL)`,
			typeID, charID, locationID)
		if err != nil {
			return "", fmt.Errorf("GiveSkill insert entity: %w", err)
		}
		itemID, err = r.LastInsertId()
		if err != nil {
			return "", fmt.Errorf("GiveSkill LastInsertId: %w", err)
		}
	}

	// Upsert level (attributeID=280).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, 280, ?, NULL)
		ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
		itemID, level); err != nil {
		return "", fmt.Errorf("GiveSkill set level: %w", err)
	}
	// Upsert skill points (attributeID=276).
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, 276, ?, NULL)
		ON DUPLICATE KEY UPDATE valueInt = VALUES(valueInt)`,
		itemID, sp); err != nil {
		return "", fmt.Errorf("GiveSkill set SP: %w", err)
	}

	return typeName, nil
}

// skillSPForLevelAdmin returns cumulative SP for a skill level: 250 * timeConstant *
// 32^((level-1)/2). Duplicated from chardb.go's skillSPForLevel.
func skillSPForLevelAdmin(tc int64, level int) float64 {
	if level <= 0 {
		return 0
	}
	return float64(250*tc) * math.Pow(32.0, float64(level-1)/2.0)
}
