package main

// Item-source authorization for the generic inventory moves. bound.Add and bound.MultiAdd name an
// item by itemID alone, so each generic move classifies where the item is and refuses what the
// caller could not have dragged from an open window. Routed paths (structure-bay deposits, offices,
// member hangars, courier packages) run before this gate with their own rules.
// See docs/cmd-misc-notes.md#item-source.

import (
	"context"
	"database/sql"

	evedb "gurimoon/db"
	"gurimoon/destiny"
)

type itemSourceKind int

const (
	srcDenied        itemSourceKind = iota
	srcOwnShip                      // inside the caller's current ship (any flag)
	srcOwnHangar                    // the caller's own item on the floor of the station they are docked in
	srcOwnNested                    // inside a container/ship the caller owns, which sits in their ship or hangar
	srcLootContainer                // a loot container / jetcan in the caller's system, in range
	srcStructureBay                 // a POS structure bay / outpost platform hold in the caller's system, in range
)

// itemDest names the family of destination a generic move is heading for.
type itemDest int

const (
	destHangar   itemDest = iota // the station hangar bind
	destCargo                    // ship bind, flag 5
	destDroneBay                 // ship bind, flag 87
	destFit                      // ship bind, any other flag (fitting / charge loading)
)

// itemTransferRange is how close a ship must be to take from an in-space container: the client's
// Open Cargo gate (2500 m) plus slack for the server's lag behind the client.
const itemTransferRange = 2500.0 + 1000.0

// itemSourceFacts is everything classifyItemSource needs, gathered by
// loadItemSourceFacts so the rule itself stays pure and testable.
type itemSourceFacts struct {
	CharID    int64
	ShipID    int64 // caller's current ship (0 = none)
	StationID int64 // station the caller is docked in (0 = in space)

	ItemOwner int64
	ItemLoc   int64

	// ItemFlag is the item's own flag: 63 is an item locked inside an Audit
	// Log Secure Container, which no move may take out.
	ItemFlag int64

	// Parent row (the thing ItemLoc names), when it is an item.
	ParentFound bool
	ParentOwner int64
	ParentLoc   int64

	IsLoot  bool // ItemLoc is a loot container in the caller's system
	IsBay   bool // ItemLoc is a structure bay in the caller's system
	InRange bool // the caller's ship is within transfer range of ItemLoc
}

// classifyItemSource decides where an item is, from the caller's point of
// view. reason is a human-readable refusal for srcDenied.
func classifyItemSource(f itemSourceFacts) (itemSourceKind, string) {
	switch {
	case f.CharID == 0:
		return srcDenied, "no character"
	case f.ItemFlag == evedb.FlagLocked:
		return srcDenied, "that item is locked"
	case f.ShipID != 0 && f.ItemLoc == f.ShipID:
		// Inside the pilot's own hull. Ownership is not tested: rows taken from loot can carry foreign
		// owners and must still move out.
		return srcOwnShip, ""
	case f.StationID != 0 && f.ItemLoc == f.StationID && f.ItemOwner == f.CharID:
		return srcOwnHangar, ""
	case f.ParentFound && f.ParentOwner == f.CharID &&
		((f.ShipID != 0 && f.ParentLoc == f.ShipID) || (f.StationID != 0 && f.ParentLoc == f.StationID)):
		return srcOwnNested, ""
	case f.IsLoot || f.IsBay:
		if f.StationID != 0 {
			return srcDenied, "you cannot reach that container while docked"
		}
		if !f.InRange {
			return srcDenied, "you are too far away from that container"
		}
		if f.IsBay {
			return srcStructureBay, ""
		}
		return srcLootContainer, ""
	}
	return srcDenied, "that item is not somewhere you can reach"
}

// sourceAllowsDest reports whether an item from this source kind may go to dest. Items taken from
// an in-space container go to the cargo hold or drone bay, never into a fitting slot or a station.
func sourceAllowsDest(kind itemSourceKind, dest itemDest) bool {
	switch kind {
	case srcOwnShip, srcOwnHangar, srcOwnNested:
		return true
	case srcLootContainer, srcStructureBay:
		return dest == destCargo || dest == destDroneBay
	}
	return false
}

// itemDestFor maps a generic move's bind and flag to its destination family.
func itemDestFor(isHangar bool, flag int64) itemDest {
	switch {
	case isHangar:
		return destHangar
	case flag == 5:
		return destCargo
	case flag == 87:
		return destDroneBay
	}
	return destFit
}

// loadItemSourceFacts gathers the facts for er from the caller's live state.
func loadItemSourceFacts(ctx context.Context, sqldb *sql.DB, ds *destinyConn,
	charID, shipID, stationID int64, er *evedb.EntityRow) itemSourceFacts {

	f := itemSourceFacts{
		CharID:    charID,
		ShipID:    shipID,
		StationID: stationID,
		ItemOwner: er.OwnerID,
		ItemLoc:   er.LocationID,
		ItemFlag:  er.Flag,
	}
	if f.ShipID != 0 && f.ItemLoc == f.ShipID {
		return f
	}
	if f.StationID != 0 && f.ItemLoc == f.StationID {
		return f
	}
	// In-space containers first: they are cheaper to test than a DB read and
	// their rows are not the caller's.
	if ds != nil && ds.solarSystem != nil && f.StationID == 0 {
		switch {
		case isLootContainer(ds, f.ItemLoc):
			f.IsLoot = true
			if p, ok := ds.solarSystem.GetLootContainerPosition(uint32(f.ItemLoc)); ok {
				f.InRange = shipWithin(ds, p, itemTransferRange)
			}
			return f
		case isNPCCargo(ds, f.ItemLoc):
			// A spawn container opened in place (npccargo.go): a loot can
			// whose position is the structure entity's.
			f.IsLoot = true
			if e := npcCargoEntity(ds, f.ItemLoc); e != nil {
				f.InRange = shipWithin(ds, e.Pos(), itemTransferRange+e.Manager.Radius())
			}
			return f
		case isStructureBay(ds, f.ItemLoc):
			f.IsBay = true
			if bd, ok := ds.solarSystem.GetStarbaseBall(uint32(f.ItemLoc)); ok {
				f.InRange = shipWithin(ds, bd.Pos, itemTransferRange+bd.Radius)
			}
			return f
		}
	}
	if sqldb != nil && f.ItemLoc > 0 {
		if p, err := evedb.GetEntityRow(ctx, sqldb, f.ItemLoc); err == nil && p != nil {
			f.ParentFound = true
			f.ParentOwner = p.OwnerID
			f.ParentLoc = p.LocationID
		}
	}
	return f
}

// shipWithin reports whether the caller's ship is within r metres of p.
func shipWithin(ds *destinyConn, p destiny.Vec3, r float64) bool {
	if ds == nil || ds.entity == nil {
		return false
	}
	return ds.entity.Pos().Sub(p).Len() <= r
}

// authorizeItemMove is the gate the generic Add/MultiAdd paths call before
// touching er. ok=false carries the refusal reason for the client.
func authorizeItemMove(ctx context.Context, sqldb *sql.DB, ds *destinyConn,
	charID, shipID, stationID int64, er *evedb.EntityRow, dest itemDest) (itemSourceKind, string, bool) {

	f := loadItemSourceFacts(ctx, sqldb, ds, charID, shipID, stationID, er)
	kind, reason := classifyItemSource(f)
	if kind == srcDenied {
		return kind, reason, false
	}
	if !sourceAllowsDest(kind, dest) {
		return kind, "move it to your cargo hold first", false
	}
	return kind, "", true
}

// takeOwnerFor is the owner an item should carry once it has been taken into
// the caller's ship from src. Loot and structure-bay stock belongs to whoever
// took it: the cargo List filters on ownerID, so a row left under its old
// owner is invisible (and uncounted against capacity) in the taker's hold.
func takeOwnerFor(kind itemSourceKind, charID, oldOwner int64) int64 {
	if kind == srcLootContainer || kind == srcStructureBay {
		return charID
	}
	return oldOwner
}
