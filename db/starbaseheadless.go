package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
)

// Headless POS seams for callers with no pilot connection (the rolebots
// module's corporations): anchoring from a real item, onlining, taking out of a
// bay, scooping. Each runs the client path's rule minus the range check, which
// the caller attests. Nothing here is reachable from an RPC. Unlike /posplant
// and template stamps, these consume an item that already exists.

// GroupControlTowerStructures: categoryID 23, the POS family.
const categoryStructure = 23

// ErrNotAControlTower / ErrNotAnchorable are the refusals for the wrong item.
var (
	ErrNotAControlTower = errors.New("that item is not a control tower")
	ErrNotAnchorable    = errors.New("that structure cannot be anchored to a control tower")
	ErrTowerNotOnline   = errors.New("the control tower is not online")
	ErrAnchoringNotDone = errors.New("the control tower is still anchoring")
	ErrNoFuelForOnline  = errors.New("the control tower holds less than one cycle of fuel")
)

// itemOrigin is where an item was before a launch, so a refused anchor can put
// it back.
type itemOrigin struct {
	locationID, flag, ownerID, singleton int64
}

func readItemOrigin(ctx context.Context, db *sql.DB, itemID int64) (itemOrigin, int64, int64, error) {
	var o itemOrigin
	var typeID, groupID int64
	err := db.QueryRowContext(ctx, `
		SELECT e.locationID, e.flag, e.ownerID, e.singleton, e.typeID, t.groupID
		  FROM entity e JOIN invTypes t ON t.typeID = e.typeID WHERE e.itemID = ?`, itemID,
	).Scan(&o.locationID, &o.flag, &o.ownerID, &o.singleton, &typeID, &groupID)
	if errors.Is(err, sql.ErrNoRows) {
		return o, 0, 0, fmt.Errorf("item %d not found", itemID)
	}
	return o, typeID, groupID, err
}

func restoreItemOrigin(ctx context.Context, db *sql.DB, itemID int64, o itemOrigin) {
	_, _ = db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = ?, ownerID = ?, singleton = ? WHERE itemID = ?`,
		o.locationID, o.flag, o.ownerID, o.singleton, itemID)
}

// SplitOneFromStack is splitOneFromStack for the pilot's launch path: the
// client's Launch sends a stack's itemID once per unit.
func SplitOneFromStack(ctx context.Context, db *sql.DB, itemID int64) (int64, error) {
	return splitOneFromStack(ctx, db, itemID)
}

// splitOneFromStack makes the item a single unit: a packaged stack of three
// towers anchors one. Returns the itemID that now holds exactly one unit.
func splitOneFromStack(ctx context.Context, db *sql.DB, itemID int64) (int64, error) {
	var qty int64
	var name string
	var typeID, owner, loc, flag int64
	if err := db.QueryRowContext(ctx, `
		SELECT quantity, COALESCE(itemName, ''), typeID, ownerID, locationID, flag
		  FROM entity WHERE itemID = ?`, itemID).Scan(&qty, &name, &typeID, &owner, &loc, &flag); err != nil {
		return 0, err
	}
	if qty <= 1 {
		return itemID, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity - 1 WHERE itemID = ? AND quantity > 1`, itemID); err != nil {
		return 0, err
	}
	r, err := tx.ExecContext(ctx, `
		INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, ?, 0, 0, 1, 0, 0, 0, NULL)`, name, typeID, owner, loc, flag)
	if err != nil {
		return 0, err
	}
	id, err := r.LastInsertId()
	if err != nil {
		return 0, err
	}
	return id, tx.Commit()
}

// TowerAnchorPosition is where a tower anchors at a moon: MoonTowerPoint, a
// short way in from the moon's warp-in (celestialwarpin.go), the point
// PlantStarbase uses.
func TowerAnchorPosition(ctx context.Context, db *sql.DB, moonID int64) (sysID int64, x, y, z float64, moonName string, err error) {
	sysID, p, moonName, err := MoonTowerAnchorPoint(ctx, db, moonID)
	if err != nil {
		return 0, 0, 0, 0, "", err
	}
	return sysID, p[0], p[1], p[2], moonName, nil
}

// AnchorTowerRequest anchors one control tower item at a moon.
type AnchorTowerRequest struct {
	ItemID     int64 // the tower item, in a ship's hold, a hangar or a bay
	MoonID     int64
	CorpID     int64
	AllianceID int64 // the corporation's current alliance (0 for none)
}

// AnchorTowerFor launches and anchors a real control-tower item at a moon for
// a corporation: placement policy (CheckStarbaseAnchorPolicy), one tower per
// moon, then the overlay AnchorLaunchedStarbase writes. A refusal leaves the
// item where it was.
func AnchorTowerFor(ctx context.Context, db *sql.DB, req AnchorTowerRequest, live StarbaseLiveNotifier) (StarbaseRecord, error) {
	if err := ensureStarbaseTable(ctx, db); err != nil {
		return StarbaseRecord{}, err
	}
	_, _, groupID, err := readItemOrigin(ctx, db, req.ItemID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	if groupID != GroupControlTower {
		return StarbaseRecord{}, ErrNotAControlTower
	}
	sysID, x, y, z, moonName, err := TowerAnchorPosition(ctx, db, req.MoonID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	decision, err := CheckStarbaseAnchorPolicy(ctx, db, sysID, req.CorpID, req.AllianceID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	itemID, err := splitOneFromStack(ctx, db, req.ItemID)
	if err != nil {
		return StarbaseRecord{}, fmt.Errorf("AnchorTowerFor split: %w", err)
	}
	origin, _, _, err := readItemOrigin(ctx, db, itemID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	if err := LaunchStructureToSpace(ctx, db, itemID, req.CorpID, sysID, x, y, z); err != nil {
		return StarbaseRecord{}, err
	}
	if err := AnchorLaunchedStarbase(ctx, db, itemID, req.MoonID, req.CorpID, decision.AllianceID, x, y, z); err != nil {
		restoreItemOrigin(ctx, db, itemID, origin)
		return StarbaseRecord{}, fmt.Errorf("anchor at %s: %w", moonName, err)
	}
	rec, ok, err := GetStarbase(ctx, db, itemID)
	if err != nil || !ok {
		return StarbaseRecord{}, fmt.Errorf("anchored tower %d vanished: %w", itemID, err)
	}
	if n := liveOr(live); n != nil {
		n.StarbasePlanted(itemID)
	}
	EmitStarbaseEvent(StarbaseEventFor(StarbaseEventPlanted, rec))
	return rec, nil
}

// typeAttrByID reads one dgmTypeAttributes value by attributeID.
func typeAttrByID(ctx context.Context, db *sql.DB, typeID, attrID int64) float64 {
	var vi, vf sql.NullFloat64
	if err := db.QueryRowContext(ctx, `
		SELECT valueInt, valueFloat FROM dgmTypeAttributes WHERE typeID = ? AND attributeID = ?`,
		typeID, attrID).Scan(&vi, &vf); err != nil {
		return 0
	}
	if vi.Valid {
		return vi.Float64
	}
	return vf.Float64
}

// Structure fitting attributes the grid check reads.
const (
	attrStructurePower = 30
	attrStructureCPU   = 50
)

// StructureGridRefusal is the power/CPU check a slave faces before it may come
// online: "" when it fits, else the sentence the client is told. Shared by the
// pilot path and OnlineStructureFor. Off when starbase.enforceGrid is off.
func StructureGridRefusal(ctx context.Context, db *sql.DB, tower, rec StarbaseRecord) string {
	if !StarbaseGridEnforced(ctx, db) {
		return ""
	}
	grid, err := GetStarbaseGrid(ctx, db, tower.EntityID, tower.TypeID)
	if err != nil {
		return ""
	}
	power := typeAttrByID(ctx, db, rec.TypeID, attrStructurePower)
	cpu := typeAttrByID(ctx, db, rec.TypeID, attrStructureCPU)
	name := rec.ItemName
	if name == "" {
		name = "this structure"
	}
	switch grid.Shortfall(power, cpu) {
	case "power":
		return fmt.Sprintf("The control tower cannot power %s: it needs %.0f MW and only %.0f of %.0f MW is free.",
			name, power, math.Max(grid.PowerOutput-grid.PowerUsed, 0), grid.PowerOutput)
	case "cpu":
		return fmt.Sprintf("The control tower does not have the CPU for %s: it needs %.0f tf and only %.0f of %.0f tf is free.",
			name, cpu, math.Max(grid.CPUOutput-grid.CPUUsed, 0), grid.CPUOutput)
	}
	return ""
}

// AnchorSlaveFor launches and anchors a real structure item beside an online
// tower at the given offset, under the pilot's rules (anchorSlaveStructureForPlayer:
// anchorable to a tower, security ceiling attribute 1032, sov, offset within
// maxStructureDistance). The slave takes the tower's ownership.
func AnchorSlaveFor(ctx context.Context, db *sql.DB, towerID, itemID int64, dx, dy, dz float64, live StarbaseLiveNotifier) (StarbaseRecord, error) {
	tower, ok, err := GetStarbase(ctx, db, towerID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	if !ok {
		return StarbaseRecord{}, fmt.Errorf("control tower %d not found", towerID)
	}
	if tower.State != StarbaseStateOnline && tower.State != StarbaseStateReinforced {
		return StarbaseRecord{}, ErrTowerNotOnline
	}
	_, typeID, _, err := readItemOrigin(ctx, db, itemID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	anchorable, err := IsAnchorableStructureType(ctx, db, typeID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	if !anchorable {
		return StarbaseRecord{}, ErrNotAnchorable
	}
	if maxSec, ok := AnchoringSecurityLevelMax(ctx, db, typeID); ok {
		if sec, secOK, err := GetSystemSecurity(ctx, db, tower.SolarSystemID); err != nil {
			return StarbaseRecord{}, err
		} else if secOK && sec > maxSec {
			return StarbaseRecord{}, fmt.Errorf("this structure cannot be anchored in %.1f-sec space (%.1f and below only)", sec, maxSec)
		}
	}
	if AnchoringRequiresSovereignty(ctx, db, typeID) {
		sov, owned, err := GetSov(ctx, db, tower.SolarSystemID)
		if err != nil {
			return StarbaseRecord{}, err
		}
		if !owned || tower.OwnerAllianceID == 0 || sov.AllianceID != tower.OwnerAllianceID {
			return StarbaseRecord{}, fmt.Errorf("this structure requires sovereignty in the system")
		}
	}
	if d := math.Sqrt(dx*dx + dy*dy + dz*dz); d > MaxStructureDistance(ctx, db, tower.TypeID) {
		return StarbaseRecord{}, fmt.Errorf("%.0f km is beyond the tower's anchoring range", d/1000)
	}
	if why := ForceFieldAnchorRefusal(ctx, db, tower.TypeID, typeID, dx, dy, dz); why != "" {
		return StarbaseRecord{}, errors.New(why)
	}
	itemID, err = splitOneFromStack(ctx, db, itemID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	origin, _, _, err := readItemOrigin(ctx, db, itemID)
	if err != nil {
		return StarbaseRecord{}, err
	}
	x, y, z := tower.X+dx, tower.Y+dy, tower.Z+dz
	if err := LaunchStructureToSpace(ctx, db, itemID, tower.OwnerCorpID, tower.SolarSystemID, x, y, z); err != nil {
		return StarbaseRecord{}, err
	}
	if err := AnchorSlaveStructure(ctx, db, itemID, tower.EntityID, tower.SolarSystemID,
		tower.OwnerCorpID, tower.OwnerAllianceID, x, y, z); err != nil {
		restoreItemOrigin(ctx, db, itemID, origin)
		return StarbaseRecord{}, err
	}
	rec, ok, err := GetStarbaseStructure(ctx, db, itemID)
	if err != nil || !ok {
		return StarbaseRecord{}, fmt.Errorf("anchored structure %d vanished: %w", itemID, err)
	}
	if n, ok := liveOr(live).(StarbaseRefreshNotifier); ok && n != nil {
		n.StarbaseRefreshed(tower.EntityID)
	}
	return rec, nil
}

// OnlineStructureFor starts onlining a slave structure: its tower must be online
// and the structure must fit the free power and CPU (StructureGridRefusal). The
// onlining sweep completes it.
func OnlineStructureFor(ctx context.Context, db *sql.DB, structureID int64, live StarbaseLiveNotifier) (string, error) {
	rec, ok, err := GetStarbaseStructure(ctx, db, structureID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("structure %d not found", structureID)
	}
	tower, ok, err := GetStarbase(ctx, db, rec.ControlTowerID)
	if err != nil {
		return "", err
	}
	if !ok || (tower.State != StarbaseStateOnline && tower.State != StarbaseStateReinforced) {
		return "", ErrTowerNotOnline
	}
	if why := StructureGridRefusal(ctx, db, tower, rec); why != "" {
		return "", errors.New(why)
	}
	state, err := OnlineStructure(ctx, db, structureID)
	if err != nil {
		return "", err
	}
	if n, ok := liveOr(live).(StarbaseRefreshNotifier); ok && n != nil {
		n.StarbaseRefreshed(tower.EntityID)
	}
	return state, nil
}

// StarbaseAnchoringDelayMs is the tower type's anchoringDelay (attribute 556).
func StarbaseAnchoringDelayMs(ctx context.Context, db *sql.DB, typeID int64) float64 {
	if v, ok, err := GetTypeAttributeFloat(ctx, db, typeID, "anchoringDelay"); err == nil && ok && v > 0 {
		return v
	}
	return 0
}

// OnlineStarbaseForChecked is OnlineStarbaseFor plus the two checks a pilot
// meets: the tower has finished anchoring (anchoringDelay since anchoredAt) and
// its bay holds one cycle of its fuel bill. A tower with no bill needs no fuel.
func OnlineStarbaseForChecked(ctx context.Context, db *sql.DB, towerID int64, live StarbaseLiveNotifier) (string, error) {
	rec, ok, err := GetStarbase(ctx, db, towerID)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("control tower %d not found", towerID)
	}
	if rec.State == StarbaseStateAnchored {
		if delay := StarbaseAnchoringDelayMs(ctx, db, rec.TypeID); delay > 0 &&
			!DelayElapsed(rec.AnchoredAt, delay, win32TimeNow()) {
			return "", ErrAnchoringNotDone
		}
		row := StarbaseFuelRow{EntityID: rec.EntityID, TypeID: rec.TypeID, SolarSystemID: rec.SolarSystemID,
			OwnerCorpID: rec.OwnerCorpID, OwnerAllianceID: rec.OwnerAllianceID}
		hours, billed, err := StarbaseFuelHoursLeft(ctx, db, row)
		if err != nil {
			return "", err
		}
		if billed && hours*3_600_000 < StarbaseFuelCycleMs(ctx, db, rec.TypeID) {
			return "", ErrNoFuelForOnline
		}
	}
	state, _, err := OnlineStarbaseFor(ctx, db, towerID, live)
	return state, err
}

// OfflineStarbaseFor takes a tower offline as a pilot's Offline does: the tower
// to anchored, every slave with it, the live grid told, and the offline event
// (which recomputes sovereignty).
func OfflineStarbaseFor(ctx context.Context, db *sql.DB, towerID int64, live StarbaseLiveNotifier) error {
	rec, ok, err := GetStarbase(ctx, db, towerID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("control tower %d not found", towerID)
	}
	if _, err := OfflineStarbase(ctx, db, towerID); err != nil {
		return err
	}
	_, _ = OfflineStructuresForTower(ctx, db, towerID)
	if n := liveOr(live); n != nil {
		n.StarbaseStateChanged(towerID)
		if rn, ok := n.(StarbaseRefreshNotifier); ok {
			rn.StarbaseRefreshed(towerID)
		}
	}
	EmitStarbaseEvent(StarbaseEventFor(StarbaseEventOffline, rec))
	return nil
}

// OfflineStructureFor takes one slave structure offline: a pilot's Offline on a
// single structure, with the live grid rebuilt as OnlineStructureFor does. The
// tower stays up. Who may do it is the caller's rule.
func OfflineStructureFor(ctx context.Context, db *sql.DB, structureID int64, live StarbaseLiveNotifier) error {
	rec, ok, err := GetStarbaseStructure(ctx, db, structureID)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("structure %d not found", structureID)
	}
	if _, err := OfflineStructure(ctx, db, structureID); err != nil {
		return err
	}
	if n, ok := liveOr(live).(StarbaseRefreshNotifier); ok && n != nil {
		n.StarbaseRefreshed(rec.ControlTowerID)
	}
	return nil
}

// TakeFromStarbaseBay moves up to qty units of typeID out of a structure's bay
// (fromFlag < 0 = any flag) to toLocation/toFlag owned by newOwner (0 keeps each
// stack's owner), merging into a loose stack of the same type and owner. Returns
// the units moved. Who may take, and range, are the caller's.
func TakeFromStarbaseBay(ctx context.Context, db *sql.DB, structureID, typeID, qty, fromFlag,
	toLocation, toFlag, newOwner int64) (int64, error) {
	if qty <= 0 {
		return 0, nil
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `
		SELECT itemID, quantity, ownerID, singleton, COALESCE(itemName, '') FROM entity
		 WHERE locationID = ? AND typeID = ? AND (? < 0 OR flag = ?)
		 ORDER BY quantity DESC, itemID FOR UPDATE`, structureID, typeID, fromFlag, fromFlag)
	if err != nil {
		return 0, fmt.Errorf("TakeFromStarbaseBay select: %w", err)
	}
	type stack struct {
		id, qty, owner, singleton int64
		name                      string
	}
	var stacks []stack
	for rows.Next() {
		var s stack
		if err := rows.Scan(&s.id, &s.qty, &s.owner, &s.singleton, &s.name); err != nil {
			rows.Close()
			return 0, err
		}
		stacks = append(stacks, s)
	}
	rows.Close()
	moved := int64(0)
	for _, s := range stacks {
		if moved >= qty {
			break
		}
		owner := s.owner
		if newOwner > 0 {
			owner = newOwner
		}
		take := min(s.qty, qty-moved)
		// Merge into a loose stack at the destination, or move/split.
		var dst, dstQty int64
		mergeErr := sql.ErrNoRows
		if s.singleton == 0 {
			mergeErr = tx.QueryRowContext(ctx, `
				SELECT itemID, quantity FROM entity
				 WHERE locationID = ? AND flag = ? AND typeID = ? AND ownerID = ? AND singleton = 0
				 ORDER BY itemID LIMIT 1 FOR UPDATE`, toLocation, toFlag, typeID, owner).Scan(&dst, &dstQty)
		}
		switch {
		case mergeErr == nil:
			if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity + ? WHERE itemID = ?`, take, dst); err != nil {
				return 0, err
			}
			if take == s.qty {
				if _, err := tx.ExecContext(ctx, `DELETE FROM entity WHERE itemID = ?`, s.id); err != nil {
					return 0, err
				}
			} else if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, s.id); err != nil {
				return 0, err
			}
		case take == s.qty:
			if _, err := tx.ExecContext(ctx, `
				UPDATE entity SET locationID = ?, flag = ?, ownerID = ? WHERE itemID = ?`,
				toLocation, toFlag, owner, s.id); err != nil {
				return 0, err
			}
		default:
			if _, err := tx.ExecContext(ctx, `UPDATE entity SET quantity = quantity - ? WHERE itemID = ?`, take, s.id); err != nil {
				return 0, err
			}
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO entity (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
				VALUES (?, ?, ?, ?, ?, 0, 0, ?, 0, 0, 0, NULL)`, s.name, typeID, owner, toLocation, toFlag, take); err != nil {
				return 0, err
			}
		}
		moved += take
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return moved, nil
}

// ScoopStructureFor takes a free (unanchored) structure out of space into a
// ship's cargo hold, bay contents inside it. Refused while anchored. The owner
// is unchanged.
func ScoopStructureFor(ctx context.Context, db *sql.DB, itemID, shipID int64, live StarbaseLiveNotifier) error {
	return ScoopStructureToCargo(ctx, db, itemID, shipID, 0, live)
}

// ScoopStructureToCargo is ScoopStructureFor that also hands the item to
// newOwnerID (0 = keep). A cargo hold lists only what the pilot owns and a
// launched structure is corporation property, so a pilot's scoop changes hands.
func ScoopStructureToCargo(ctx context.Context, db *sql.DB, itemID, shipID, newOwnerID int64, live StarbaseLiveNotifier) error {
	var sysID, cat, grp int64
	if err := db.QueryRowContext(ctx, `
		SELECT e.locationID, COALESCE(g.categoryID, 0), t.groupID
		  FROM entity e JOIN invTypes t ON t.typeID = e.typeID JOIN invGroups g ON g.groupID = t.groupID
		 WHERE e.itemID = ?`, itemID).Scan(&sysID, &cat, &grp); err != nil {
		return fmt.Errorf("ScoopStructureFor %d: %w", itemID, err)
	}
	if cat != categoryStructure && !IsDeployableGroup(grp) {
		return fmt.Errorf("item %d is not a structure", itemID)
	}
	if sysID < 30000000 || sysID >= 32000000 {
		return fmt.Errorf("structure %d is not in space", itemID)
	}
	var anchored int
	_ = db.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM starbase WHERE entityID = ?) + (SELECT COUNT(*) FROM starbaseStructures WHERE entityID = ?)`,
		itemID, itemID).Scan(&anchored)
	if anchored > 0 {
		return fmt.Errorf("structure %d is still anchored", itemID)
	}
	// The location guard makes a second scoop of the same item a no-op.
	res, err := db.ExecContext(ctx, `
		UPDATE entity SET locationID = ?, flag = 5, x = 0, y = 0, z = 0,
		       ownerID = IF(? = 0, ownerID, ?)
		 WHERE itemID = ? AND locationID = ?`, shipID, newOwnerID, newOwnerID, itemID, sysID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("structure %d is no longer in space", itemID)
	}
	if n := liveOr(live); n != nil {
		n.StarbaseRemoved(itemID, sysID)
	}
	return nil
}

// HeadlessReinforce puts an online tower into reinforcement through the same
// EnterReinforcement a shot-down shield uses, for a siege resolved off-grid.
// hours = 0 when the bay held no strontium; the caller decides what that
// means. Emits the reinforced event and tells the live grid.
func HeadlessReinforce(ctx context.Context, db *sql.DB, towerID int64, live StarbaseLiveNotifier) (hours int64, until uint64, err error) {
	rec, ok, err := GetStarbase(ctx, db, towerID)
	if err != nil {
		return 0, 0, err
	}
	if !ok || rec.State != StarbaseStateOnline {
		return 0, 0, ErrTowerNotOnline
	}
	hours, until, err = EnterReinforcement(ctx, db, rec)
	if err != nil || hours == 0 {
		return hours, until, err
	}
	if n := liveOr(live); n != nil {
		n.StarbaseStateChanged(towerID)
	}
	rec.State, rec.ReinforceUntil = StarbaseStateReinforced, until
	EmitStarbaseEvent(StarbaseEventFor(StarbaseEventReinforced, rec))
	return hours, until, nil
}

// DestroyStarbaseFor destroys a tower off-grid, for a system nobody has loaded
// (a live system uses finishStructureDeath). Bays spill into a loot container
// (SpillStarbaseBay), structures stay free in space with their contents, the
// tower is deleted and the destroyed event fires (which recomputes sovereignty).
func DestroyStarbaseFor(ctx context.Context, db *sql.DB, towerID int64, live StarbaseLiveNotifier) (canID int64, err error) {
	rec, ok, err := GetStarbase(ctx, db, towerID)
	if err != nil {
		return 0, err
	}
	if !ok {
		return 0, fmt.Errorf("control tower %d not found", towerID)
	}
	canID, _ = SpillStarbaseBay(ctx, db, rec)
	_, _ = OfflineStructuresForTower(ctx, db, towerID)
	if err := RemoveStarbase(ctx, db, towerID); err != nil {
		return canID, err
	}
	_ = DeleteStructureDamage(ctx, db, towerID)
	if n := liveOr(live); n != nil {
		n.StarbaseRemoved(towerID, rec.SolarSystemID)
	}
	EmitStarbaseEvent(StarbaseEventFor(StarbaseEventDestroyed, rec))
	return canID, nil
}
