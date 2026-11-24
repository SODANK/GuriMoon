package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"time"

	"gurimoon/marshal"
)

// Loot containers: on ship death a plain Cargo Container (typeID 23, groupID 12) is spawned at
// the kill site holding surviving cargo and modules. It is an entity row located in the solar
// system, with its children re-parented under it (flag 0). spaceLootContainers adds a spawnedAt
// clock for expiry; containers reload when their system is entered, and an hourly sweep expires
// those in systems nobody visits.

// TypeCargoContainer is the plain lootable container spawned on ship death
// (const.typeCargoContainer in the client).
const TypeCargoContainer int64 = 23

// Corpses: a destroyed capsule leaves a Corpse (typeID 25, group 14 Biomass) at the kill site,
// named "<pilot>'s Frozen Corpse". The client draws it (spaceObject.Corpse) and offers Scoop To
// Cargo Hold (menusvc checkScoopable) on any Biomass ball, so the server places the ball and
// answers ship.Scoop. A corpse follows the loot-container lifecycle but is not a container:
// SolarSystem.GetLootContainerPosition refuses it, so nothing can be put into or opened from it.
const (
	TypeCorpse   int64 = 25
	GroupBiomass int64 = 14
)

// CorpseName is the item name a podded pilot's corpse carries, clamped to entity.itemName's
// VARCHAR(85); STRICT_TRANS_TABLES turns an overrun into a rolled-back insert.
func CorpseName(charName string) string {
	const suffix = "'s Frozen Corpse"
	if charName == "" {
		return "Frozen Corpse"
	}
	r := []rune(charName)
	if max := 85 - len([]rune(suffix)); len(r) > max {
		r = r[:max]
	}
	return string(r) + suffix
}

// CreateCorpse inserts a podded pilot's corpse in space at the kill site plus its
// spaceLootContainers clock row. ownerID is the victim. Returns the new itemID.
func CreateCorpse(ctx context.Context, db *sql.DB, ownerID, solarSystemID int64, name string, x, y, z float64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateCorpse begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 0, 0, 1, 1, ?, ?, ?, NULL)`,
		name, TypeCorpse, ownerID, solarSystemID, x, y, z)
	if err != nil {
		return 0, fmt.Errorf("CreateCorpse insert: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateCorpse LastInsertId: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO spaceLootContainers (containerID, solarSystemID, spawnedAt)
		VALUES (?, ?, ?)`, itemID, solarSystemID, time.Now().Unix()); err != nil {
		return 0, fmt.Errorf("CreateCorpse spawn row: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("CreateCorpse commit: %w", err)
	}
	return itemID, nil
}

// ErrCorpseGone is ScoopCorpseToCargo's answer when the corpse is no longer
// floating where the caller saw it.
var ErrCorpseGone = errors.New("the corpse is no longer there")

// ScoopCorpseToCargo moves a corpse floating in solarSystemID into shipID's cargo hold (flag 5)
// as charID's property and drops its expiry clock, in one transaction. The WHERE re-checks that
// the corpse is still in space, so of two pilots scooping the same body exactly one wins.
func ScoopCorpseToCargo(ctx context.Context, db *sql.DB, corpseID, solarSystemID, shipID, charID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ScoopCorpseToCargo begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 5, ownerID = ?, x = 0, y = 0, z = 0
		 WHERE itemID = ? AND locationID = ? AND typeID = ?`,
		shipID, charID, corpseID, solarSystemID, TypeCorpse)
	if err != nil {
		return fmt.Errorf("ScoopCorpseToCargo move: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrCorpseGone
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spaceLootContainers WHERE containerID = ?`, corpseID); err != nil {
		return fmt.Errorf("ScoopCorpseToCargo clock: %w", err)
	}
	return tx.Commit()
}

// EnsureLootTables creates the spaceLootContainers tracking table if absent.
func EnsureLootTables(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS spaceLootContainers (
			containerID BIGINT NOT NULL PRIMARY KEY,
			solarSystemID BIGINT NOT NULL,
			spawnedAt BIGINT NOT NULL,
			KEY idx_slc_system (solarSystemID),
			KEY idx_slc_spawned (spawnedAt)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8`)
	if err != nil {
		return fmt.Errorf("EnsureLootTables: %w", err)
	}
	// kind: LootKindDropped (jetcans, kill loot) or LootKindLaunched (a cargo container a
	// pilot launched for self or corp), which keeps its own, longer expiry clock.
	if _, err := db.ExecContext(ctx, `
		ALTER TABLE spaceLootContainers ADD COLUMN IF NOT EXISTS kind TINYINT NOT NULL DEFAULT 0`); err != nil {
		return fmt.Errorf("EnsureLootTables kind: %w", err)
	}
	return nil
}

// Loot container kinds (spaceLootContainers.kind).
const (
	LootKindDropped  int64 = 0 // jetcan, kill loot, death can: short clock, gone once emptied
	LootKindLaunched int64 = 1 // launched cargo container: long clock, stays when empty, scoopable
)

// LootCutoffs is the expiry cutoff per kind, unix seconds: a container spawned before the
// cutoff of its kind has expired.
type LootCutoffs struct {
	Dropped  int64
	Launched int64
}

// lootExpiredWhere is the shared WHERE fragment for "this spaceLootContainers row has
// expired"; its two placeholders are Dropped then Launched.
const lootExpiredWhere = `((kind = 1 AND spawnedAt < ?) OR (kind <> 1 AND spawnedAt < ?))`

func (c LootCutoffs) args() []any { return []any{c.Launched, c.Dropped} }

// LaunchContainerIntoSpace moves an assembled cargo container (with whatever is inside it)
// out of a ship and into space at x/y/z as a launched loot container owned by ownerID.
func LaunchContainerIntoSpace(ctx context.Context, db *sql.DB, itemID, ownerID, solarSystemID int64, x, y, z float64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("LaunchContainerIntoSpace begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID=?, flag=0, ownerID=?, singleton=1, x=?, y=?, z=?
		WHERE itemID=?`, solarSystemID, ownerID, x, y, z, itemID); err != nil {
		return fmt.Errorf("LaunchContainerIntoSpace move: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		REPLACE INTO spaceLootContainers (containerID, solarSystemID, spawnedAt, kind)
		VALUES (?, ?, ?, ?)`, itemID, solarSystemID, time.Now().Unix(), LootKindLaunched); err != nil {
		return fmt.Errorf("LaunchContainerIntoSpace spawn row: %w", err)
	}
	return tx.Commit()
}

// ErrContainerGone is ScoopContainerToCargo's answer when the container already left space.
var ErrContainerGone = errors.New("container is no longer in space")

// ScoopContainerToCargo takes a launched container back into a ship's cargo hold with its
// contents still inside it. The scooping pilot becomes its owner: the cargo hold lists and
// weighs only the pilot's own rows, so a corp-owned container would vanish there.
func ScoopContainerToCargo(ctx context.Context, db *sql.DB, itemID, solarSystemID, shipID, charID int64) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ScoopContainerToCargo begin: %w", err)
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE entity SET locationID=?, flag=5, ownerID=?, x=0, y=0, z=0
		WHERE itemID=? AND locationID=?`, shipID, charID, itemID, solarSystemID)
	if err != nil {
		return fmt.Errorf("ScoopContainerToCargo move: %w", err)
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return ErrContainerGone
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM spaceLootContainers WHERE containerID = ?`, itemID); err != nil {
		return fmt.Errorf("ScoopContainerToCargo clock: %w", err)
	}
	return tx.Commit()
}

// CreateLootContainer inserts a Cargo Container entity in space at the given system and
// position, plus its spaceLootContainers row. ownerID is the victim (loot rights are not
// enforced). Returns the new container's itemID.
func CreateLootContainer(ctx context.Context, db *sql.DB, ownerID, solarSystemID int64, x, y, z float64) (int64, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("CreateLootContainer begin: %w", err)
	}
	defer tx.Rollback()

	res, err := tx.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES ('Cargo Container', ?, ?, ?, 0, 0, 1, 1, ?, ?, ?, NULL)`,
		TypeCargoContainer, ownerID, solarSystemID, x, y, z)
	if err != nil {
		return 0, fmt.Errorf("CreateLootContainer insert: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateLootContainer LastInsertId: %w", err)
	}

	now := time.Now().Unix()
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO spaceLootContainers (containerID, solarSystemID, spawnedAt)
		VALUES (?, ?, ?)`, itemID, solarSystemID, now); err != nil {
		return 0, fmt.Errorf("CreateLootContainer spawn row: %w", err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("CreateLootContainer commit: %w", err)
	}
	return itemID, nil
}

// GetShipChildItems returns every item inside a ship: cargo (flag 5), fitted modules and
// loaded charges (flags 11-34) and drone bay (flag 87). The death loop rolls drop or destroy per item.
func GetShipChildItems(ctx context.Context, db *sql.DB, shipID int64) ([]EntityRow, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		       e.singleton, e.quantity,
		       COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		FROM entity e
		LEFT JOIN invTypes  t ON t.typeID  = e.typeID
		LEFT JOIN invGroups g ON g.groupID = t.groupID
		WHERE e.locationID = ?
		  AND (e.flag = 5 OR e.flag BETWEEN 11 AND 34 OR e.flag = 87)`, shipID)
	if err != nil {
		return nil, fmt.Errorf("GetShipChildItems: %w", err)
	}
	defer rows.Close()
	var out []EntityRow
	for rows.Next() {
		var er EntityRow
		if err := rows.Scan(&er.ItemID, &er.TypeID, &er.OwnerID, &er.LocationID,
			&er.Flag, &er.Singleton, &er.Quantity, &er.GroupID, &er.CategoryID); err != nil {
			return nil, fmt.Errorf("GetShipChildItems scan: %w", err)
		}
		out = append(out, er)
	}
	return out, rows.Err()
}

// MoveItemIntoContainer re-parents an item into a loot container (flag 0). Any persisted
// module online state is dropped, since loose modules carry none.
func MoveItemIntoContainer(ctx context.Context, db *sql.DB, itemID, containerID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE entity SET locationID=?, flag=0 WHERE itemID=?`, containerID, itemID)
	if err != nil {
		return fmt.Errorf("MoveItemIntoContainer: %w", err)
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM entity_attributes WHERE itemID=? AND attributeID=2`, itemID)
	return nil
}

// DeleteEntityDeep removes an entity row plus its entity_attributes rows
// (the FK requires attributes to go first).
func DeleteEntityDeep(ctx context.Context, db *sql.DB, itemID int64) error {
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity_attributes WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteEntityDeep attrs: %w", err)
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity WHERE itemID = ?`, itemID); err != nil {
		return fmt.Errorf("DeleteEntityDeep: %w", err)
	}
	return nil
}

// GetContainerItems returns the contents of a loot container as an
// InventoryBound.List() rowset (same columns as the cargo/hangar lists).
func GetContainerItems(ctx context.Context, db *sql.DB, containerID int64) (marshal.PyRep, error) {
	return containerItemsAs(ctx, db, containerID, containerID)
}

// containerItemsAs lists containerID's contents with every row's locationID reported as
// reportAs, for a container shown to the client under a different itemID than the one holding
// the goods (courier packages).
func containerItemsAs(ctx context.Context, db *sql.DB, containerID, reportAs int64) (marshal.PyRep, error) {
	return containerItemsWhere(ctx, db, containerID, reportAs, -1)
}

// GetContainerItemsFlag is GetContainerItems narrowed to one location flag. A sentry keeps its
// ammunition in flag 27 and its crystal storage in flag 0, and the client opens them separately.
func GetContainerItemsFlag(ctx context.Context, db *sql.DB, containerID, flag int64) (marshal.PyRep, error) {
	return containerItemsWhere(ctx, db, containerID, containerID, flag)
}

// containerItemsWhere is the shared body: flag < 0 lists every flag.
func containerItemsWhere(ctx context.Context, db *sql.DB, containerID, reportAs, flag int64) (marshal.PyRep, error) {
	assetCols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
	}
	rows, err := db.QueryContext(ctx,
		`SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		        e.contraband, e.singleton, e.quantity,
		        COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		   FROM entity e
		   LEFT JOIN invTypes t ON t.typeID = e.typeID
		   LEFT JOIN invGroups g ON g.groupID = t.groupID
		  WHERE e.locationID = ? AND (? < 0 OR e.flag = ?)
		  ORDER BY t.typeID, e.itemID`, containerID, flag, flag)
	if err != nil {
		return nil, fmt.Errorf("GetContainerItems query: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		var a AssetItem
		if err := rows.Scan(&a.ItemID, &a.TypeID, &a.OwnerID, &a.LocationID,
			&a.Flag, &a.Contraband, &a.Singleton, &a.Quantity,
			&a.GroupID, &a.CategoryID); err != nil {
			return nil, fmt.Errorf("GetContainerItems scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(a.ItemID), intRep(a.TypeID), intRep(a.OwnerID), intRep(reportAs),
			intRep(a.Flag), intRep(a.Contraband), intRep(a.Singleton), intRep(a.Quantity),
			intRep(a.GroupID), intRep(a.CategoryID), marshal.PyNone{},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetContainerItems rows: %w", err)
	}
	return makeRowset(assetCols, result), nil
}

// IsContainerEmpty reports whether a loot container has no children left.
func IsContainerEmpty(ctx context.Context, db *sql.DB, containerID int64) (bool, error) {
	var n int64
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ?`, containerID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("IsContainerEmpty: %w", err)
	}
	return n == 0, nil
}

// DeleteLootContainer removes a container and everything still inside it,
// plus its spaceLootContainers tracking row.
func DeleteLootContainer(ctx context.Context, db *sql.DB, containerID int64) error {
	_, _ = db.ExecContext(ctx, `
		DELETE ea FROM entity_attributes ea
		JOIN entity e ON e.itemID = ea.itemID
		WHERE e.locationID = ?`, containerID)
	if _, err := db.ExecContext(ctx,
		`DELETE FROM entity WHERE locationID = ?`, containerID); err != nil {
		return fmt.Errorf("DeleteLootContainer children: %w", err)
	}
	_, _ = db.ExecContext(ctx,
		`DELETE FROM spaceLootContainers WHERE containerID = ?`, containerID)
	return DeleteEntityDeep(ctx, db, containerID)
}

// CleanupSpaceLootContainers deletes in-space loot containers that are expired (spawned
// before the cutoff of their kind) or have no spaceLootContainers row. Tracked, unexpired
// containers survive a restart and reload when their system is entered.
func CleanupSpaceLootContainers(ctx context.Context, db *sql.DB, cutoff LootCutoffs) error {

	// Legacy containers: entity rows with no spawn-tracking row.
	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID FROM entity e
		LEFT JOIN spaceLootContainers s ON s.containerID = e.itemID
		WHERE e.typeID = ? AND e.locationID BETWEEN 30000000 AND 39999999
		  AND s.containerID IS NULL`,
		TypeCargoContainer)
	if err != nil {
		return fmt.Errorf("CleanupSpaceLootContainers legacy query: %w", err)
	}
	var legacy []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err == nil {
			legacy = append(legacy, id)
		}
	}
	rows.Close()

	// Expired tracked containers.
	rows2, err := db.QueryContext(ctx, `
		SELECT containerID FROM spaceLootContainers
		WHERE `+lootExpiredWhere, cutoff.args()...)
	if err != nil {
		return fmt.Errorf("CleanupSpaceLootContainers expired query: %w", err)
	}
	var expired []int64
	for rows2.Next() {
		var id int64
		if err := rows2.Scan(&id); err == nil {
			expired = append(expired, id)
		}
	}
	rows2.Close()

	for _, id := range legacy {
		if err := DeleteLootContainer(ctx, db, id); err != nil {
			log.Printf("[db] CleanupSpaceLootContainers legacy %d: %v", id, err)
		}
	}
	for _, id := range expired {
		if err := DeleteLootContainer(ctx, db, id); err != nil {
			log.Printf("[db] CleanupSpaceLootContainers expired %d: %v", id, err)
		}
	}
	total := len(legacy) + len(expired)
	if total > 0 {
		log.Printf("[db] CleanupSpaceLootContainers: removed %d stale containers (%d legacy, %d expired)", total, len(legacy), len(expired))
	}
	return nil
}

// ListLootContainersForSystem returns the unexpired tracked loot containers in a solar
// system, with position and owner from the entity row.
func ListLootContainersForSystem(ctx context.Context, db *sql.DB, sysID int64, cutoff LootCutoffs) ([]SpaceLootContainer, error) {
	args := append([]any{sysID}, cutoff.args()...)
	rows, err := db.QueryContext(ctx, `
		SELECT s.containerID, e.ownerID, COALESCE(e.x, 0), COALESCE(e.y, 0), COALESCE(e.z, 0), s.spawnedAt, e.typeID,
		       COALESCE(NULLIF(e.itemName, ''), t.typeName, ''),
		       COALESCE(t.groupID, 0), COALESCE(t.radius, 0), s.kind
		FROM spaceLootContainers s
		JOIN entity e ON e.itemID = s.containerID
		LEFT JOIN invTypes t ON t.typeID = e.typeID
		WHERE s.solarSystemID = ? AND NOT `+lootExpiredWhere, args...)
	if err != nil {
		return nil, fmt.Errorf("ListLootContainersForSystem: %w", err)
	}
	defer rows.Close()
	var out []SpaceLootContainer
	for rows.Next() {
		var c SpaceLootContainer
		if err := rows.Scan(&c.ContainerID, &c.OwnerID, &c.X, &c.Y, &c.Z, &c.SpawnedAt, &c.TypeID, &c.Name, &c.GroupID, &c.Radius, &c.Kind); err != nil {
			return nil, fmt.Errorf("ListLootContainersForSystem scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SpaceLootContainer is one tracked container loaded at system init.
type SpaceLootContainer struct {
	ContainerID int64
	OwnerID     int64
	X, Y, Z     float64
	SpawnedAt   int64 // unix seconds
	TypeID      int64 // TypeCargoContainer, TypeCorpse for a podded pilot, or a launched container's type
	Name        string
	GroupID     int64
	Radius      float64 // the type's radius, 0 when unknown
	Kind        int64   // LootKindDropped or LootKindLaunched
}

// DeleteExpiredLootContainers removes expired tracked containers in systems not in
// loadedSystems, so a can is never pulled out from under an on-grid client. Returns the count.
func DeleteExpiredLootContainers(ctx context.Context, db *sql.DB, cutoff LootCutoffs, loadedSystems map[int64]struct{}) (int, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT containerID, solarSystemID FROM spaceLootContainers
		WHERE `+lootExpiredWhere, cutoff.args()...)
	if err != nil {
		return 0, fmt.Errorf("DeleteExpiredLootContainers query: %w", err)
	}
	defer rows.Close()
	var toDelete []int64
	for rows.Next() {
		var id, sysID int64
		if err := rows.Scan(&id, &sysID); err != nil {
			continue
		}
		if _, loaded := loadedSystems[sysID]; !loaded {
			toDelete = append(toDelete, id)
		}
	}
	for _, id := range toDelete {
		if err := DeleteLootContainer(ctx, db, id); err != nil {
			log.Printf("[db] DeleteExpiredLootContainers %d: %v", id, err)
		}
	}
	return len(toDelete), nil
}
