package db

// Corporation item lockdown: the ledger written by the corp window's Lockdown Blueprint and
// Unlock Blueprint votes. A lock covers a corp-owned blueprint original at one station and stops
// moves, not use. See docs/db-11-notes.md#corp-item-lockdown.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"gurimoon/marshal"
)

// Client message keys (eveMessages) for lockdown refusals.
const (
	LockMsgItemNotFound   = "CrpLockFailedItemNotFound"
	LockMsgNotYours       = "CrpCantLockItemNotYours"
	LockMsgIllegalType    = "CrpLockFailedIllegalItemType"
	LockMsgOnlyOriginals  = "CrpLockFailedOnlyBlueprintOriginals"
	LockMsgItemIsLocked   = "CrpItemIsLocked"
	LockMsgItemNotLocked  = "CrpItemIsNotLocked"
	LockMsgJunkHasLocked  = "CrpJunkContainsLockedItem"
	stationIDBandFirst    = 60000000
	stationIDBandPastLast = 64000000
)

// LockedItem is one crpLockedItems row.
type LockedItem struct {
	ItemID, CorpID, TypeID, OwnerID, StationID int64
}

// LockCandidate is what LockRefusal needs to know about an item.
type LockCandidate struct {
	Found      bool
	OwnerID    int64
	TypeID     int64
	CategoryID int64
	Singleton  bool
	IsCopy     bool
	// StationID is the item's office station, or its locationID when on a station floor; 0
	// elsewhere (ship, container, space).
	StationID int64
}

// LockRefusal is the lockdown rule: the client message key refusing a lock of c for corpID at
// stationID, or "" when it may stand. stationID 0 means the vote named no station, so any
// station the item is at will do.
func LockRefusal(corpID, stationID int64, c LockCandidate) string {
	switch {
	case !c.Found:
		return LockMsgItemNotFound
	case c.OwnerID != corpID:
		return LockMsgNotYours
	case c.CategoryID != categoryBlueprint:
		return LockMsgIllegalType
	case c.IsCopy:
		return LockMsgOnlyOriginals
	case !c.Singleton:
		// A packaged stack: the picker never offers one, and a single-unit take would split away from
		// the lock.
		return LockMsgIllegalType
	case c.StationID == 0, stationID != 0 && c.StationID != stationID:
		return LockMsgItemNotFound
	}
	return ""
}

// LoadLockCandidate reads the facts LockRefusal needs for itemID.
func LoadLockCandidate(ctx context.Context, db *sql.DB, itemID int64) (LockCandidate, error) {
	var c LockCandidate
	var loc, officeStation, copyFlag, singleton int64
	err := db.QueryRowContext(ctx, `
		SELECT e.ownerID, e.typeID, e.locationID, e.singleton, g.categoryID,
		       COALESCE(b.copy, 0), COALESCE(o.stationID, 0)
		  FROM entity e
		  JOIN invTypes t ON t.typeID = e.typeID
		  JOIN invGroups g ON g.groupID = t.groupID
		  LEFT JOIN invBlueprints b ON b.blueprintID = e.itemID
		  LEFT JOIN crpoffices o ON o.itemID = e.locationID
		 WHERE e.itemID = ?`, itemID,
	).Scan(&c.OwnerID, &c.TypeID, &loc, &singleton, &c.CategoryID, &copyFlag, &officeStation)
	if errors.Is(err, sql.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, fmt.Errorf("LoadLockCandidate %d: %w", itemID, err)
	}
	c.Found = true
	c.Singleton = singleton != 0
	c.IsCopy = copyFlag != 0
	switch {
	case officeStation != 0:
		c.StationID = officeStation
	case loc >= stationIDBandFirst && loc < stationIDBandPastLast:
		c.StationID = loc
	}
	return c, nil
}

// CheckLockable answers the refusal key for a proposed lock without writing, so InsertVoteCase
// can refuse a vote that could never be carried out.
func CheckLockable(ctx context.Context, db *sql.DB, corpID, itemID, stationID int64) (string, error) {
	c, err := LoadLockCandidate(ctx, db, itemID)
	if err != nil {
		return "", err
	}
	return LockRefusal(corpID, stationID, c), nil
}

// LockItem validates and records a lock (sanctioned-action execution). A
// non-empty refusal is a client message key and nothing was written.
func LockItem(ctx context.Context, db *sql.DB, corpID, itemID, stationID int64) (LockedItem, string, error) {
	c, err := LoadLockCandidate(ctx, db, itemID)
	if err != nil {
		return LockedItem{}, "", err
	}
	if r := LockRefusal(corpID, stationID, c); r != "" {
		return LockedItem{}, r, nil
	}
	li := LockedItem{ItemID: itemID, CorpID: corpID, TypeID: c.TypeID, OwnerID: c.OwnerID, StationID: c.StationID}
	if _, err := db.ExecContext(ctx, `
		REPLACE INTO crpLockedItems (itemID, corporationID, typeID, ownerID, locationID)
		VALUES (?, ?, ?, ?, ?)`,
		li.ItemID, li.CorpID, li.TypeID, li.OwnerID, li.StationID); err != nil {
		return LockedItem{}, "", fmt.Errorf("LockItem: %w", err)
	}
	return li, "", nil
}

// GetLockedItem returns the corp's lock on itemID, ok=false when there is none.
func GetLockedItem(ctx context.Context, db *sql.DB, corpID, itemID int64) (LockedItem, bool, error) {
	li := LockedItem{ItemID: itemID, CorpID: corpID}
	err := db.QueryRowContext(ctx, `
		SELECT typeID, ownerID, locationID FROM crpLockedItems
		WHERE corporationID = ? AND itemID = ?`, corpID, itemID,
	).Scan(&li.TypeID, &li.OwnerID, &li.StationID)
	if errors.Is(err, sql.ErrNoRows) {
		return LockedItem{}, false, nil
	}
	if err != nil {
		return LockedItem{}, false, fmt.Errorf("GetLockedItem: %w", err)
	}
	return li, true, nil
}

// UnlockItem removes a lock. A non-empty refusal (CrpItemIsNotLocked) means
// there was nothing to remove.
func UnlockItem(ctx context.Context, db *sql.DB, corpID, itemID int64) (LockedItem, string, error) {
	li, ok, err := GetLockedItem(ctx, db, corpID, itemID)
	if err != nil {
		return LockedItem{}, "", err
	}
	if !ok {
		return LockedItem{}, LockMsgItemNotLocked, nil
	}
	if _, err := db.ExecContext(ctx,
		`DELETE FROM crpLockedItems WHERE corporationID = ? AND itemID = ?`, corpID, itemID); err != nil {
		return LockedItem{}, "", fmt.Errorf("UnlockItem: %w", err)
	}
	return li, "", nil
}

// IsItemLockedDown reports whether itemID is locked by any corporation.
// Errors read as unlocked: a failed lookup must not freeze every corp hangar.
func IsItemLockedDown(ctx context.Context, q rowQueryer, itemID int64) bool {
	var n int
	return q.QueryRowContext(ctx,
		`SELECT 1 FROM crpLockedItems WHERE itemID = ?`, itemID).Scan(&n) == nil
}

// LockedDownAmong returns the subset of ids that are locked.
func LockedDownAmong(ctx context.Context, q rowQueryer, ids []int64) (map[int64]bool, error) {
	out := map[int64]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	rows, err := q.QueryContext(ctx, `
		SELECT itemID FROM crpLockedItems
		WHERE itemID IN (?`+strings.Repeat(", ?", len(ids)-1)+`)`, args...)
	if err != nil {
		return nil, fmt.Errorf("LockedDownAmong: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// CorpJunkHasLockedItem reports whether the corp's repossessed property at a station (what
// ReturnCorpJunk hands to a director) holds a locked item. Such junk stays impounded
// (CrpJunkContainsLockedItem).
func CorpJunkHasLockedItem(ctx context.Context, db *sql.DB, corpID, stationID int64) bool {
	var n int
	return db.QueryRowContext(ctx, `
		SELECT 1 FROM crpLockedItems l
		JOIN entity e ON e.itemID = l.itemID
		WHERE e.ownerID = ? AND e.locationID = ? AND e.flag = ?
		LIMIT 1`, corpID, stationID, FlagCorpHangarDiv1).Scan(&n) == nil
}

// PruneLockedItems drops locks whose item no longer exists or no longer belongs to the locking
// corporation. Run at boot.
func PruneLockedItems(ctx context.Context, db *sql.DB) (int64, error) {
	res, err := db.ExecContext(ctx, `
		DELETE l FROM crpLockedItems l
		LEFT JOIN entity e ON e.itemID = l.itemID
		WHERE e.itemID IS NULL OR e.ownerID <> l.corporationID`)
	if err != nil {
		return 0, fmt.Errorf("PruneLockedItems: %w", err)
	}
	return res.RowsAffected()
}

// GetLockedItemLocationIDs returns the stations where the corp has locked items; the client
// asks only for these locations.
func GetLockedItemLocationIDs(ctx context.Context, db *sql.DB, corpID int64) []int64 {
	rows, err := db.QueryContext(ctx,
		`SELECT DISTINCT locationID FROM crpLockedItems WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out
}

// LockedItemCols is the locked-items rowset header. OnLockedItemChange's
// change dict must carry one entry per column: the client's add branch
// (bco_itemLocking) builds the new row by walking this header.
var LockedItemCols = []string{"itemID", "typeID", "ownerID"}

// GetLockedItemsAtLocation returns the corp's locked items at a station as a
// util.IndexRowset keyed on itemID.
func GetLockedItemsAtLocation(ctx context.Context, db *sql.DB, corpID, locationID int64) marshal.PyRep {
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, typeID, ownerID FROM crpLockedItems
		WHERE corporationID = ? AND locationID = ?`, corpID, locationID)
	if err != nil {
		return makeIndexRowset("itemID", LockedItemCols, nil)
	}
	defer rows.Close()
	var out [][]marshal.PyRep
	for rows.Next() {
		var itemID, typeID, ownerID int64
		if rows.Scan(&itemID, &typeID, &ownerID) != nil {
			continue
		}
		out = append(out, []marshal.PyRep{intRep(itemID), intRep(typeID), intRep(ownerID)})
	}
	return makeIndexRowset("itemID", LockedItemCols, out)
}
