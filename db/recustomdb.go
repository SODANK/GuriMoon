package db

// Portrait recustomization (/redoportrait): chrRecustomization flags a character whose
// appearance may be redone through the character-creation flow.
// See docs/db-13-notes.md#portrait-recustomization.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// EnsureRecustomizationTable creates the chrRecustomization table if missing.
func EnsureRecustomizationTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrRecustomization (
			characterID BIGINT NOT NULL PRIMARY KEY,
			flaggedAt   BIGINT NOT NULL DEFAULT 0
		)`); err != nil {
		return fmt.Errorf("EnsureRecustomizationTable: %w", err)
	}
	return nil
}

// RecustomizationInfo describes a character flagged for portrait redo.
type RecustomizationInfo struct {
	CharacterID   int64
	CharacterName string
	BloodlineID   int64
	Gender        int64
}

// GetRecustomizationsForAccount returns all flagged characters belonging to
// accountID.  Empty slice when none are flagged.
func GetRecustomizationsForAccount(ctx context.Context, db *sql.DB, accountID int64) ([]RecustomizationInfo, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID, c.characterName, c.bloodlineID, c.gender
		FROM chrRecustomization r
		JOIN character_ c ON c.characterID = r.characterID
		WHERE c.accountID = ?
		ORDER BY c.characterID`, accountID)
	if err != nil {
		return nil, fmt.Errorf("GetRecustomizationsForAccount: %w", err)
	}
	defer rows.Close()

	var out []RecustomizationInfo
	for rows.Next() {
		var ri RecustomizationInfo
		if err := rows.Scan(&ri.CharacterID, &ri.CharacterName, &ri.BloodlineID, &ri.Gender); err != nil {
			return nil, fmt.Errorf("GetRecustomizationsForAccount scan: %w", err)
		}
		out = append(out, ri)
	}
	return out, rows.Err()
}

// FlagRecustomization marks characterID for portrait redo.  Idempotent.
func FlagRecustomization(ctx context.Context, db *sql.DB, characterID int64) error {
	if _, err := db.ExecContext(ctx, `
		INSERT INTO chrRecustomization (characterID, flaggedAt) VALUES (?, ?)
		ON DUPLICATE KEY UPDATE flaggedAt = VALUES(flaggedAt)`,
		characterID, win32TimeNow()); err != nil {
		return fmt.Errorf("FlagRecustomization: %w", err)
	}
	return nil
}

// ClearRecustomization removes the portrait-redo flag.  Returns whether a
// flag was actually present.
func ClearRecustomization(ctx context.Context, db *sql.DB, characterID int64) (bool, error) {
	res, err := db.ExecContext(ctx,
		`DELETE FROM chrRecustomization WHERE characterID = ?`, characterID)
	if err != nil {
		return false, fmt.Errorf("ClearRecustomization: %w", err)
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// IsRecustomizationFlagged reports whether characterID is flagged.
func IsRecustomizationFlagged(ctx context.Context, db *sql.DB, characterID int64) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM chrRecustomization WHERE characterID = ?`, characterID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("IsRecustomizationFlagged: %w", err)
	}
	return true, nil
}

// FindPlayerCharByName resolves a player character (accountID > 0) by exact name. It
// returns characterID and the canonical-case name, or an error wrapping sql.ErrNoRows.
func FindPlayerCharByName(ctx context.Context, db *sql.DB, name string) (int64, string, error) {
	var charID int64
	var canonical string
	err := db.QueryRowContext(ctx, `
		SELECT characterID, characterName FROM character_
		WHERE characterName = ? AND accountID IS NOT NULL AND accountID > 0`,
		name).Scan(&charID, &canonical)
	if err != nil {
		return 0, "", fmt.Errorf("FindPlayerCharByName %q: %w", name, err)
	}
	return charID, canonical, nil
}

// UpdateCharAppearance overwrites only the appearance columns of an existing
// character with the appearance dict from a CreateCharacter call.
func UpdateCharAppearance(ctx context.Context, db *sql.DB, characterID int64, appearance map[string]interface{}) error {
	app := parseAppearance(appearance)
	_, err := db.ExecContext(ctx, `
		UPDATE character_ SET
			accessoryID = ?, beardID = ?, costumeID = ?, decoID = ?, eyebrowsID = ?, eyesID = ?,
			hairID = ?, lipstickID = ?, makeupID = ?, skinID = ?, backgroundID = ?, lightID = ?,
			headRotation1 = ?, headRotation2 = ?, headRotation3 = ?,
			eyeRotation1 = ?, eyeRotation2 = ?, eyeRotation3 = ?,
			camPos1 = ?, camPos2 = ?, camPos3 = ?,
			morph1e = ?, morph1n = ?, morph1s = ?, morph1w = ?,
			morph2e = ?, morph2n = ?, morph2s = ?, morph2w = ?,
			morph3e = ?, morph3n = ?, morph3s = ?, morph3w = ?,
			morph4e = ?, morph4n = ?, morph4s = ?, morph4w = ?
		WHERE characterID = ?`,
		app.accessoryID, app.beardID, app.costumeID, app.decoID, app.eyebrowsID, app.eyesID,
		app.hairID, app.lipstickID, app.makeupID, app.skinID, app.backgroundID, app.lightID,
		app.headRotation1, app.headRotation2, app.headRotation3,
		app.eyeRotation1, app.eyeRotation2, app.eyeRotation3,
		app.camPos1, app.camPos2, app.camPos3,
		app.morph1e, app.morph1n, app.morph1s, app.morph1w,
		app.morph2e, app.morph2n, app.morph2s, app.morph2w,
		app.morph3e, app.morph3n, app.morph3s, app.morph3w,
		app.morph4e, app.morph4n, app.morph4s, app.morph4w,
		characterID,
	)
	if err != nil {
		return fmt.Errorf("UpdateCharAppearance charID=%d: %w", characterID, err)
	}
	return nil
}

// GetBloodlineName returns the display name for a bloodlineID.
func GetBloodlineName(ctx context.Context, db *sql.DB, bloodlineID int64) (string, error) {
	var name string
	err := db.QueryRowContext(ctx,
		`SELECT bloodlineName FROM chrBloodlines WHERE bloodlineID = ?`, bloodlineID).Scan(&name)
	if err != nil {
		return "", fmt.Errorf("GetBloodlineName %d: %w", bloodlineID, err)
	}
	return name, nil
}
