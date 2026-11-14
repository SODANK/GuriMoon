package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// entityChildFKs are the shipped foreign keys onto entity.itemID. Dumped as restrict keys, they
// fail any of the ~40 entity DELETE sites that skip the children (error 1451), so they become
// CASCADE: a deleted item's children are dead data.
var entityChildFKs = []struct{ table, name, column string }{
	{"entity_attributes", "entity_attributes_ibfk_1", "itemID"},
}

// EnsureNoteItemUnbound drops chrNotes.itemID's foreign key onto entity. A note can be keyed on
// any inspected character and NPC agents (3008xxx) have no entity row, so the REPLACE fails
// with error 1452. A note about a deleted item is a harmless orphan; DeleteCharacter removes
// notes about the pilot explicitly.
func EnsureNoteItemUnbound(ctx context.Context, db *sql.DB) error {
	var n int
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM information_schema.REFERENTIAL_CONSTRAINTS
		WHERE CONSTRAINT_SCHEMA = DATABASE()
		  AND CONSTRAINT_NAME   = 'chrNotes_ibfk_2'`).Scan(&n); err != nil {
		return fmt.Errorf("EnsureNoteItemUnbound probe: %w", err)
	}
	if n == 0 {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		"ALTER TABLE `chrNotes` DROP FOREIGN KEY `chrNotes_ibfk_2`"); err != nil {
		return fmt.Errorf("EnsureNoteItemUnbound drop: %w", err)
	}
	return nil
}

// EnsureEntityChildCascade rewrites the foreign keys onto entity as ON DELETE
// CASCADE. The probe keeps the ALTERs off every boot but the first.
func EnsureEntityChildCascade(ctx context.Context, db *sql.DB) error {
	for _, fk := range entityChildFKs {
		var rule string
		err := db.QueryRowContext(ctx, `
			SELECT DELETE_RULE FROM information_schema.REFERENTIAL_CONSTRAINTS
			WHERE CONSTRAINT_SCHEMA = DATABASE()
			  AND CONSTRAINT_NAME   = ?
			  AND REFERENCED_TABLE_NAME = 'entity'`, fk.name).Scan(&rule)
		if errors.Is(err, sql.ErrNoRows) {
			continue // table absent or key already replaced under another name
		}
		if err != nil {
			return fmt.Errorf("EnsureEntityChildCascade probe %s: %w", fk.name, err)
		}
		if rule == "CASCADE" {
			continue
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"ALTER TABLE `%s` DROP FOREIGN KEY `%s`", fk.table, fk.name)); err != nil {
			return fmt.Errorf("EnsureEntityChildCascade drop %s: %w", fk.name, err)
		}
		if _, err := db.ExecContext(ctx, fmt.Sprintf(
			"ALTER TABLE `%s` ADD CONSTRAINT `%s` FOREIGN KEY (`%s`) REFERENCES `entity` (`itemID`) ON DELETE CASCADE",
			fk.table, fk.name, fk.column)); err != nil {
			return fmt.Errorf("EnsureEntityChildCascade add %s: %w", fk.name, err)
		}
	}
	return nil
}
