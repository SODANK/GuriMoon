package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"
	"math"
)

// Dogma attribute IDs used by the repair shop.
const (
	attrDamage      = 3   // current damage dealt to a module / ship structure
	attrHP          = 9   // maximum (structure) hitpoints
	attrArmorHP     = 265 // maximum armor hitpoints
	attrArmorDamage = 266 // current armor damage
)

// repairCostFactor converts an item's base price into an ISK-per-hitpoint repair rate:
// server_settings repair.costFactor, default 0.02. The client only displays the quoted figure.
func repairCostFactor() float64 {
	return CachedSettingFloat("repair.costFactor", 0.02, 0, 10)
}

// DamageReport is a single line of a repair quote. Damage/MaxHealth are the
// combined structure+armor pools (the client shows one line per item);
// StructDamage/ArmorDamage keep the split so repairs write back per-attribute.
type DamageReport struct {
	ItemID                      int64
	TypeID                      int64
	Damage                      float64
	MaxHealth                   float64
	StructDamage                float64
	ArmorDamage                 float64
	CostToRepairOneUnitOfDamage float64
}

// GetDamageReports returns the repair quote for each item the character owns. Items with no hp
// attribute are skipped.
func GetDamageReports(ctx context.Context, db *sql.DB, ownerID int64, itemIDs []int64) (map[int64]DamageReport, error) {
	out := make(map[int64]DamageReport, len(itemIDs))
	if len(itemIDs) == 0 {
		return out, nil
	}

	args := make([]any, 0, len(itemIDs)+1)
	args = append(args, ownerID)
	for _, id := range itemIDs {
		args = append(args, id)
	}

	rows, err := db.QueryContext(ctx, `
		SELECT e.itemID, e.typeID,
		       COALESCE(dmg.valueFloat, dmg.valueInt, 0)              AS damage,
		       COALESCE(ehp.valueFloat, ehp.valueInt, thp.valueFloat, thp.valueInt, 0) AS maxHealth,
		       COALESCE(adm.valueFloat, adm.valueInt, 0)              AS armorDamage,
		       COALESCE(ahp.valueFloat, ahp.valueInt, tahp.valueFloat, tahp.valueInt, 0) AS armorHealth,
		       COALESCE(t.basePrice, 0)
		FROM entity e
		JOIN invTypes t ON t.typeID = e.typeID
		LEFT JOIN entity_attributes dmg ON dmg.itemID = e.itemID AND dmg.attributeID = ?
		LEFT JOIN entity_attributes ehp ON ehp.itemID = e.itemID AND ehp.attributeID = ?
		LEFT JOIN dgmTypeAttributes thp ON thp.typeID = e.typeID AND thp.attributeID = ?
		LEFT JOIN entity_attributes adm ON adm.itemID = e.itemID AND adm.attributeID = ?
		LEFT JOIN entity_attributes ahp ON ahp.itemID = e.itemID AND ahp.attributeID = ?
		LEFT JOIN dgmTypeAttributes tahp ON tahp.typeID = e.typeID AND tahp.attributeID = ?
		WHERE e.ownerID = ? AND e.itemID IN (`+placeholders(len(itemIDs))+`)`,
		append([]any{attrDamage, attrHP, attrHP, attrArmorDamage, attrArmorHP, attrArmorHP}, args...)...)
	if err != nil {
		return nil, fmt.Errorf("GetDamageReports: %w", err)
	}
	defer rows.Close()

	// A player owner's Repair cost modifier scales every line; the quote and
	// RepairItems both come through here, so they agree.
	stationID, _ := GetCharStationID(ctx, db, ownerID)
	feeFactor := StationServiceFeeFactor(ctx, db, stationID, ownerID, StationServiceRepair)

	for rows.Next() {
		var r DamageReport
		var structMax, armorMax, basePrice float64
		if err := rows.Scan(&r.ItemID, &r.TypeID, &r.StructDamage, &structMax, &r.ArmorDamage, &armorMax, &basePrice); err != nil {
			return nil, fmt.Errorf("GetDamageReports scan: %w", err)
		}
		if structMax <= 0 {
			continue
		}
		if r.StructDamage > structMax {
			r.StructDamage = structMax
		}
		if r.ArmorDamage > armorMax {
			r.ArmorDamage = armorMax
		}
		// One quote line per item: pool structure and armor together (modules
		// have no armor attributes, so for them this is a no-op).
		r.Damage = r.StructDamage + r.ArmorDamage
		r.MaxHealth = structMax + armorMax
		r.CostToRepairOneUnitOfDamage = basePrice / r.MaxHealth * repairCostFactor()
		if r.CostToRepairOneUnitOfDamage <= 0 {
			// basePrice is 0/NULL for this type: a minimum price keeps the item quotable.
			r.CostToRepairOneUnitOfDamage = repairCostFactor()
		}
		r.CostToRepairOneUnitOfDamage *= feeFactor
		out[r.ItemID] = r
	}
	return out, rows.Err()
}

// SaveShipCombatDamage persists live structure (attr 3) and armor (attr 266) damage so dock,
// gate jump and relog are not free repairs. Zero values are written too: that records a
// repaired state.
func SaveShipCombatDamage(ctx context.Context, db *sql.DB, shipID int64, structDmg, armorDmg float64) error {
	if structDmg < 0 {
		structDmg = 0
	}
	if armorDmg < 0 {
		armorDmg = 0
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, ?, NULL, ?), (?, ?, NULL, ?)
		ON DUPLICATE KEY UPDATE valueInt = NULL, valueFloat = VALUES(valueFloat)`,
		shipID, attrDamage, structDmg, shipID, attrArmorDamage, armorDmg)
	if err != nil {
		return fmt.Errorf("SaveShipCombatDamage %d: %w", shipID, err)
	}
	return nil
}

// GetShipCombatDamage loads persisted structure/armor damage for a ship.
// Missing rows read as zero damage.
func GetShipCombatDamage(ctx context.Context, db *sql.DB, shipID int64) (structDmg, armorDmg float64, err error) {
	rows, err := db.QueryContext(ctx, `
		SELECT attributeID, COALESCE(valueFloat, valueInt, 0)
		FROM entity_attributes
		WHERE itemID = ? AND attributeID IN (?, ?)`,
		shipID, attrDamage, attrArmorDamage)
	if err != nil {
		return 0, 0, fmt.Errorf("GetShipCombatDamage %d: %w", shipID, err)
	}
	defer rows.Close()
	for rows.Next() {
		var attrID int64
		var val float64
		if err := rows.Scan(&attrID, &val); err != nil {
			return 0, 0, fmt.Errorf("GetShipCombatDamage scan: %w", err)
		}
		switch attrID {
		case attrDamage:
			structDmg = val
		case attrArmorDamage:
			armorDmg = val
		}
	}
	return structDmg, armorDmg, rows.Err()
}

// SetItemDamage writes an absolute damage value for an item, clamped to [0, hp].
func SetItemDamage(ctx context.Context, db *sql.DB, itemID int64, damage float64) error {
	if damage < 0 {
		damage = 0
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
		VALUES (?, ?, NULL, ?)
		ON DUPLICATE KEY UPDATE valueInt = NULL, valueFloat = VALUES(valueFloat)`,
		itemID, attrDamage, damage)
	if err != nil {
		return fmt.Errorf("SetItemDamage %d: %w", itemID, err)
	}
	return nil
}

// RepairItems removes damage from the given items, spending at most budget ISK
// from the character's wallet.  Items are repaired in the order given until the
// budget runs out; a partially funded item is partially repaired.
// Returns the ISK actually spent.
func RepairItems(ctx context.Context, db *sql.DB, charID int64, itemIDs []int64, budget float64) (float64, error) {
	reports, err := GetDamageReports(ctx, db, charID, itemIDs)
	if err != nil {
		return 0, err
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("RepairItems begin: %w", err)
	}
	defer tx.Rollback()

	var balance float64
	if err := tx.QueryRowContext(ctx,
		`SELECT balance FROM character_ WHERE characterID = ? FOR UPDATE`, charID,
	).Scan(&balance); err != nil {
		return 0, fmt.Errorf("RepairItems balance: %w", err)
	}
	if budget > balance {
		budget = balance
	}

	spent := 0.0
	for _, itemID := range itemIDs {
		r, ok := reports[itemID]
		if !ok || r.Damage <= 0 {
			continue
		}
		remaining := budget - spent
		if remaining <= 0 {
			break
		}

		repairable := r.Damage
		if r.CostToRepairOneUnitOfDamage > 0 {
			affordable := remaining / r.CostToRepairOneUnitOfDamage
			if affordable < repairable {
				repairable = affordable
			}
		}
		if repairable <= 0 {
			continue
		}

		// Armor is repaired before structure; write each pool back to its
		// own attribute so the split survives a partially funded repair.
		armorRepair := repairable
		if armorRepair > r.ArmorDamage {
			armorRepair = r.ArmorDamage
		}
		structRepair := repairable - armorRepair
		// Clamped: (S+A)-A can come back an ulp over S, and a negative value in an UNSIGNED column
		// (Error 1264) rolls back the whole repair.
		structLeft := math.Max(0, r.StructDamage-structRepair)
		armorLeft := math.Max(0, r.ArmorDamage-armorRepair)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO entity_attributes (itemID, attributeID, valueInt, valueFloat)
			VALUES (?, ?, NULL, ?), (?, ?, NULL, ?)
			ON DUPLICATE KEY UPDATE valueInt = NULL, valueFloat = VALUES(valueFloat)`,
			itemID, attrDamage, structLeft,
			itemID, attrArmorDamage, armorLeft); err != nil {
			return 0, fmt.Errorf("RepairItems set damage %d: %w", itemID, err)
		}
		spent += repairable * r.CostToRepairOneUnitOfDamage
	}

	if spent > 0 {
		if _, err := tx.ExecContext(ctx,
			`UPDATE character_ SET balance = balance - ? WHERE characterID = ?`,
			spent, charID); err != nil {
			return 0, fmt.Errorf("RepairItems debit: %w", err)
		}
		// refRepairBill (15): the client renders "Repair bill between o1 and
		// o2", with o2 the corp that owns the station the character is in.
		var stationCorpID int64
		_ = tx.QueryRowContext(ctx, `
			SELECT COALESCE(s.corporationID, 0)
			  FROM character_ c
			  LEFT JOIN staStations s ON s.stationID = c.stationID
			 WHERE c.characterID = ?`, charID).Scan(&stationCorpID)
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO market_journal
			  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
			   accountID, amount, balance, reason, accountKey)
			VALUES (?, ?, 15, ?, ?, NULL, 0, ?, ?, 'Repair', 1000)`,
			charID, win32TimeNow(), charID, stationCorpID, -spent, balance-spent); err != nil {
			return 0, fmt.Errorf("RepairItems journal: %w", err)
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("RepairItems commit: %w", err)
	}
	return spent, nil
}

// RepackageRefusal is an UnassembleItem refusal worded for the pilot.
type RepackageRefusal string

func (r RepackageRefusal) Error() string { return string(r) }

// RepackagedContent is one item a ship repackage moved out of the hull into
// the station hangar, with where it was so the caller can push OnItemChange.
type RepackagedContent struct {
	ItemID, OldLocationID, OldFlag int64
}

// RepackageCorpItemAllowed is set by cmd/gurimoon. UnassembleItem calls it for a corp-owned
// item in an office (locationID = office, flag = division); true allows the repackage. Nil
// refuses every corporation item.
var RepackageCorpItemAllowed func(ctx context.Context, charID, itemID, ownerID, locationID, flag int64) bool

// UnassembleItem repackages an assembled item by clearing its singleton flag. A ship is emptied
// first (modules, charges, cargo and drones go to the station hangar through UnfitModuleToHangar)
// and its insurance contract is voided; any other container must already be empty. Refuses the
// active ship and anything the character does not own.
func UnassembleItem(ctx context.Context, db *sql.DB, charID, itemID int64) ([]RepackagedContent, error) {
	var ownerID, typeID, singleton, locationID, flag, categoryID int64
	err := db.QueryRowContext(ctx, `
		SELECT e.ownerID, e.typeID, e.singleton, e.locationID, e.flag, g.categoryID
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.itemID = ?`, itemID,
	).Scan(&ownerID, &typeID, &singleton, &locationID, &flag, &categoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("UnassembleItem: item %d not found", itemID)
	}
	if err != nil {
		return nil, fmt.Errorf("UnassembleItem lookup: %w", err)
	}
	if ownerID != charID {
		hook := RepackageCorpItemAllowed
		if hook == nil || !hook(ctx, charID, itemID, ownerID, locationID, flag) {
			return nil, RepackageRefusal("You may only repackage your own items, or your corporation's where you hold the hangar's take role.")
		}
	}
	if singleton == 0 {
		return nil, nil // already packaged
	}

	var contained int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE locationID = ?`, itemID).Scan(&contained); err != nil {
		return nil, fmt.Errorf("UnassembleItem contents: %w", err)
	}

	var moved []RepackagedContent
	if contained > 0 {
		if categoryID != 6 {
			return nil, RepackageRefusal("Empty the container before repackaging it.")
		}
		// A pilot sits in their ship as an entity row at locationID = shipID; the client hides
		// Repackage on the active ship and the server refuses it.
		var aboard int64
		if err := db.QueryRowContext(ctx, `
			SELECT COUNT(*) FROM entity e
			  JOIN character_ c ON c.characterID = e.itemID
			 WHERE e.locationID = ?`, itemID).Scan(&aboard); err != nil {
			return nil, fmt.Errorf("UnassembleItem pilot check: %w", err)
		}
		if aboard > 0 {
			return nil, RepackageRefusal("You cannot repackage the ship you are flying.")
		}
		// Contents go to the hangar the ship is in; a ship elsewhere (office, maintenance array,
		// space) has no hangar to take them.
		if flag != 4 || locationID < 60000000 || locationID >= 64000000 {
			return nil, RepackageRefusal("The ship must be in your station hangar to be repackaged.")
		}

		rows, err := db.QueryContext(ctx,
			`SELECT itemID, flag FROM entity WHERE locationID = ?`, itemID)
		if err != nil {
			return nil, fmt.Errorf("UnassembleItem contents list: %w", err)
		}
		var contents []RepackagedContent
		for rows.Next() {
			rc := RepackagedContent{OldLocationID: itemID}
			if err := rows.Scan(&rc.ItemID, &rc.OldFlag); err != nil {
				rows.Close()
				return nil, fmt.Errorf("UnassembleItem contents scan: %w", err)
			}
			contents = append(contents, rc)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, fmt.Errorf("UnassembleItem contents rows: %w", err)
		}

		for _, rc := range contents {
			if err := UnfitModuleToHangar(ctx, db, rc.ItemID, locationID); err != nil {
				// What already moved stays moved (it is in the hangar); the ship stays assembled.
				return moved, fmt.Errorf("UnassembleItem unload %d: %w", rc.ItemID, err)
			}
			moved = append(moved, rc)
		}
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET singleton = 0, itemName = '' WHERE itemID = ?`, itemID); err != nil {
		return moved, fmt.Errorf("UnassembleItem update: %w", err)
	}
	if categoryID == 6 {
		if err := UnInsureShip(ctx, db, itemID); err != nil {
			log.Printf("UnassembleItem: void insurance on %d: %v", itemID, err)
		}
	}
	// A repackaged item is undamaged by definition.
	return moved, SetItemDamage(ctx, db, itemID, 0)
}
