// Package db: POS moon production (what each structure handles, how they are
// wired, what one harvester cycle yields). The numbers come from shipped data:
//
//	cycle   = the tower's posControlTowerPeriod (attribute 722)
//	batches = min(harvesterQuality, the moon's abundance for the material)
//	units   = batches * the material's moonMiningAmount (attribute 726)
//
// moonMiningAmount is an attribute of the moon material (moonmining.py:194),
// not of the harvester.
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// Structure groups the Production tab knows about. Its sort keyfunc indexes a
// dict of exactly these three (moonmining.py:207), so any other groupID in a
// GetMoonProcessInfoForTower row is a KeyError.
const (
	GroupMoonMining    = 416
	GroupSilo          = 404
	GroupMobileReactor = 438
)

// POS weapon groups. A sentry's loaded ammunition lives in flagHiSlot0 (27),
// the locationFlag the client's Access Ammunition window opens
// (menusvc.py OpenStructureCharges).
const (
	GroupMobileMissileSentry    = 417
	GroupMobileProjectileSentry = 426
	GroupMobileLaserSentry      = 430
	GroupMobileHybridSentry     = 449

	FlagSentryAmmo = 27 // const.flagHiSlot0
)

// IsSentryGroup reports whether groupID is a POS gun that loads ammunition.
func IsSentryGroup(groupID int64) bool {
	switch groupID {
	case GroupMobileMissileSentry, GroupMobileProjectileSentry, GroupMobileLaserSentry, GroupMobileHybridSentry:
		return true
	}
	return false
}

// IsProductionStructureGroup reports whether the Production tab can render a
// structure of this group.
func IsProductionStructureGroup(groupID int64) bool {
	return groupID == GroupMoonMining || groupID == GroupSilo || groupID == GroupMobileReactor
}

// StructureLink is one wire on the Production tab: sourceID supplies typeID
// to destID.
type StructureLink struct {
	TypeID   int64
	SourceID int64
	DestID   int64
}

// SetStructureProvisionType records what a structure handles: the moon material
// a harvester pulls or the type a silo stores. 0 clears it (the client's None).
func SetStructureProvisionType(ctx context.Context, db *sql.DB, entityID, typeID int64) error {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return err
	}
	res, err := db.ExecContext(ctx,
		`UPDATE starbaseStructures SET provisionTypeID = ? WHERE entityID = ?`,
		typeID, entityID)
	if err != nil {
		return fmt.Errorf("SetStructureProvisionType: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("structure %d is not anchored", entityID)
	}
	// Changing the type drops the structure's links, as the client does locally
	// (moonmining.py SetStructureType).
	if _, err := db.ExecContext(ctx,
		`DELETE FROM starbaseStructureLinks WHERE sourceID = ? OR destID = ?`,
		entityID, entityID); err != nil {
		return fmt.Errorf("SetStructureProvisionType unlink: %w", err)
	}
	return nil
}

// ListStructureLinks returns the wiring for one tower.
func ListStructureLinks(ctx context.Context, db *sql.DB, towerID int64) ([]StructureLink, error) {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx,
		`SELECT typeID, sourceID, destID FROM starbaseStructureLinks WHERE towerID = ?`, towerID)
	if err != nil {
		return nil, fmt.Errorf("ListStructureLinks: %w", err)
	}
	defer rows.Close()
	var out []StructureLink
	for rows.Next() {
		var l StructureLink
		if err := rows.Scan(&l.TypeID, &l.SourceID, &l.DestID); err != nil {
			return nil, fmt.Errorf("ListStructureLinks scan: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// SetStructureLinks replaces a tower's whole wiring in one transaction; the
// client sends its entire structureConnections dict on each Apply, so a merge
// would keep deleted wires. Links naming a structure not anchored to this tower
// are dropped, not refused, since the client's dict can hold a stale itemID.
func SetStructureLinks(ctx context.Context, db *sql.DB, towerID int64, links []StructureLink) error {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return err
	}
	members, err := ListStarbaseStructuresForTower(ctx, db, towerID)
	if err != nil {
		return err
	}
	valid := make(map[int64]bool, len(members))
	for _, m := range members {
		valid[m.EntityID] = true
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("SetStructureLinks begin: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM starbaseStructureLinks WHERE towerID = ?`, towerID); err != nil {
		return fmt.Errorf("SetStructureLinks clear: %w", err)
	}
	for _, l := range links {
		if l.TypeID == 0 || !valid[l.SourceID] || !valid[l.DestID] || l.SourceID == l.DestID {
			continue
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO starbaseStructureLinks (towerID, typeID, sourceID, destID)
			VALUES (?, ?, ?, ?)
			ON DUPLICATE KEY UPDATE towerID = VALUES(towerID), destID = VALUES(destID)`,
			towerID, l.TypeID, l.SourceID, l.DestID); err != nil {
			return fmt.Errorf("SetStructureLinks insert: %w", err)
		}
	}
	return tx.Commit()
}

// ---- harvest arithmetic ----

// HarvesterQuality is attribute 710, the batches per cycle an array can pull
// (1/2/3 for Moon Harvesting Array I/II/III).
func HarvesterQuality(ctx context.Context, db *sql.DB, typeID int64) int64 {
	if v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, "harvesterQuality"); err == nil && ok && v > 0 {
		return int64(v)
	}
	return 1
}

// MoonMiningAmount is attribute 726 on the moon material: units per batch.
func MoonMiningAmount(ctx context.Context, db *sql.DB, materialTypeID int64) int64 {
	if v, ok, err := GetTypeAttributeFloat(ctx, db, materialTypeID, "moonMiningAmount"); err == nil && ok && v > 0 {
		return int64(v)
	}
	return 100
}

// HarvestUnitsPerCycle is the whole yield rule, split out so it is testable and
// the ESP page and the sweep agree. A material the moon lacks yields nothing.
func HarvestUnitsPerCycle(quality, abundance, amountPerBatch int64) int64 {
	if quality <= 0 || abundance <= 0 || amountPerBatch <= 0 {
		return 0
	}
	batches := quality
	if abundance < batches {
		batches = abundance
	}
	return batches * amountPerBatch
}

// MoonAbundance returns a moon's abundance for one material, 0 when the moon
// does not carry it.
func MoonAbundance(ctx context.Context, db *sql.DB, moonID, typeID int64) (int64, error) {
	res, err := GetMoonResources(ctx, db, moonID)
	if err != nil {
		return 0, err
	}
	for _, r := range res {
		if r.TypeID == typeID {
			return r.Quantity, nil
		}
	}
	return 0, nil
}

// SetStructureHarvestTick stamps when a structure last produced.
func SetStructureHarvestTick(ctx context.Context, db *sql.DB, entityID int64, tick uint64) error {
	if _, err := db.ExecContext(ctx,
		`UPDATE starbaseStructures SET harvestTick = ? WHERE entityID = ?`, tick, entityID); err != nil {
		return fmt.Errorf("SetStructureHarvestTick: %w", err)
	}
	return nil
}

// StructureHarvestTick reads it back.
func StructureHarvestTick(ctx context.Context, db *sql.DB, entityID int64) (uint64, error) {
	var t uint64
	err := db.QueryRowContext(ctx,
		`SELECT harvestTick FROM starbaseStructures WHERE entityID = ?`, entityID).Scan(&t)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("StructureHarvestTick: %w", err)
	}
	return t, nil
}

// ---- silo contents ----

// SiloContents is what a silo holds: one type, a quantity, and the volume it
// takes up.
type SiloContents struct {
	TypeID   int64
	Quantity int64
	Volume   float64
}

// ListSiloContents returns everything in a structure's bay: entity rows at
// locationID = structureID, flag 0, the shape the tower's fuel bay uses, so
// GetInventoryFromId shows them unchanged.
func ListSiloContents(ctx context.Context, db *sql.DB, structureID int64) ([]SiloContents, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT e.typeID, SUM(e.quantity), COALESCE(t.volume, 0)
		  FROM entity e JOIN invTypes t ON t.typeID = e.typeID
		 WHERE e.locationID = ? AND e.flag = 0
		 GROUP BY e.typeID, t.volume`, structureID)
	if err != nil {
		return nil, fmt.Errorf("ListSiloContents: %w", err)
	}
	defer rows.Close()
	var out []SiloContents
	for rows.Next() {
		var c SiloContents
		if err := rows.Scan(&c.TypeID, &c.Quantity, &c.Volume); err != nil {
			return nil, fmt.Errorf("ListSiloContents scan: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SiloUsedCapacity returns (used m3, capacity m3) for a structure. The client
// draws it as the silo fill gauge in the reaction slot of the
// GetMoonProcessInfoForTower tuple (moonmining.py:1385).
func SiloUsedCapacity(ctx context.Context, db *sql.DB, structureID, typeID int64) (float64, float64, error) {
	var capacity float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(capacity, 0) FROM invTypes WHERE typeID = ?`, typeID).Scan(&capacity); err != nil {
		return 0, 0, fmt.Errorf("SiloUsedCapacity: %w", err)
	}
	contents, err := ListSiloContents(ctx, db, structureID)
	if err != nil {
		return 0, capacity, err
	}
	used := 0.0
	for _, c := range contents {
		used += float64(c.Quantity) * c.Volume
	}
	return used, capacity, nil
}

// AddToSilo puts harvested material into a silo, merging with the stack there.
// Returns how many units fit, so a full silo stops the harvester instead of
// losing the cycle.
func AddToSilo(ctx context.Context, db *sql.DB, structureID, siloTypeID, materialTypeID, units int64) (int64, error) {
	if units <= 0 {
		return 0, nil
	}
	used, capacity, err := SiloUsedCapacity(ctx, db, structureID, siloTypeID)
	if err != nil {
		return 0, err
	}
	var unitVolume float64
	if err := db.QueryRowContext(ctx,
		`SELECT COALESCE(volume, 0) FROM invTypes WHERE typeID = ?`, materialTypeID).Scan(&unitVolume); err != nil {
		return 0, fmt.Errorf("AddToSilo volume: %w", err)
	}
	if unitVolume > 0 && capacity > 0 {
		room := int64((capacity - used) / unitVolume)
		if room <= 0 {
			return 0, nil
		}
		if room < units {
			units = room
		}
	}

	var existingID int64
	err = db.QueryRowContext(ctx, `
		SELECT itemID FROM entity
		 WHERE locationID = ? AND flag = 0 AND typeID = ? AND singleton = 0
		 LIMIT 1`, structureID, materialTypeID).Scan(&existingID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("AddToSilo lookup: %w", err)
	}
	if err == nil {
		if _, uErr := db.ExecContext(ctx,
			`UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, units, existingID); uErr != nil {
			return 0, fmt.Errorf("AddToSilo merge: %w", uErr)
		}
		return units, nil
	}

	var ownerID int64
	if err := db.QueryRowContext(ctx,
		`SELECT ownerID FROM entity WHERE itemID = ?`, structureID).Scan(&ownerID); err != nil {
		return 0, fmt.Errorf("AddToSilo owner: %w", err)
	}
	var typeName string
	if err := db.QueryRowContext(ctx,
		`SELECT typeName FROM invTypes WHERE typeID = ?`, materialTypeID).Scan(&typeName); err != nil {
		return 0, fmt.Errorf("AddToSilo typeName: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton,
		   quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 0, 0, 0, ?, 0, 0, 0, NULL)`,
		typeName, materialTypeID, ownerID, structureID, units); err != nil {
		return 0, fmt.Errorf("AddToSilo insert: %w", err)
	}
	return units, nil
}

// DeleteStructureLinksFor drops every Production-tab wire touching a destroyed
// structure.
func DeleteStructureLinksFor(ctx context.Context, db *sql.DB, entityID int64) error {
	if err := ensureStarbaseStructureTable(ctx, db); err != nil {
		return err
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM starbaseStructureLinks WHERE sourceID = ? OR destID = ?`, entityID, entityID)
	return err
}

// Structure groups whose storage is a corporate hangar: the client opens all
// three with form.CorpHangarArray (menusvc OpenCorpHangarArray), whose seven
// division tabs are flagHangar (4) and flagCorpSAG2-7 (116-121) and keep only
// items owned by the corp or the structure's owner (hangar.py IsLocatedIn).
const (
	GroupRefiningArray        = 311
	GroupShipMaintenanceArray = 363
	GroupAssemblyArray        = 397
	GroupMobileLaboratory     = 413
	GroupCorpHangarArray      = 471

	FlagRefineryInput = 5 // const.flagCargo, the refinery's Processing Area window
)

// IsCorpHangarStructureGroup reports whether a structure's storage is a corp
// hangar with divisions.
func IsCorpHangarStructureGroup(groupID int64) bool {
	switch groupID {
	case GroupCorpHangarArray, GroupAssemblyArray, GroupMobileLaboratory:
		return true
	}
	return false
}

// corpHangarDivisionFlags are the seven corp hangar division flags.
var corpHangarDivisionFlags = []int64{
	FlagCorpHangarDiv1,
	FlagCorpSAGFirst, FlagCorpSAGFirst + 1, FlagCorpSAGFirst + 2,
	FlagCorpSAGFirst + 3, FlagCorpSAGFirst + 4, FlagCorpSAGFirst + 5,
}

// StructureBayFlags is the one table of which location flags a structure's
// storage holds, by group. The client lists a bound inventory once with no flag
// and filters by flag itself (invCache.py), so a deposit on the wrong flag
// appears in no window.
func StructureBayFlags(groupID int64) []int64 {
	switch {
	case IsCorpHangarStructureGroup(groupID):
		return corpHangarDivisionFlags
	case groupID == GroupRefiningArray:
		return []int64{FlagRefineryInput}
	case IsSentryGroup(groupID):
		return []int64{0, FlagSentryAmmo}
	}
	return []int64{0}
}

// StructureBayFlag resolves the flag a deposit lands on: the requested one when
// the structure has it, else its first (a bare Add sends flag 0 or None).
func StructureBayFlag(groupID, requested int64) int64 {
	flags := StructureBayFlags(groupID)
	for _, f := range flags {
		if f == requested {
			return f
		}
	}
	return flags[0]
}
