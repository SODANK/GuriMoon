// Package bookmark implements the "bookmark" service backing the client's
// address book (ui/shared/neocom/addressbook.py).
//
// GetBookmarks returns the union of the player's stored bookmarks and
// synthetic rows derived from accepted missions.  The two ID ranges are kept
// apart by evedb.BookmarkIDFloor so WarpToStuff can tell them apart.
package bookmark

import (
	"context"
	"database/sql"
	"fmt"
	"log"

	evedb "gurimoon/db"
	"gurimoon/marshal"
	evenet "gurimoon/net"
	"gurimoon/server"
)

// bookmarkCols is the column order the client's util.Row unpacking expects.
var bookmarkCols = []string{
	"bookmarkID", "ownerID", "itemID", "typeID", "flag",
	"memo", "created", "x", "y", "z", "locationID",
}

// Location is where the calling character currently is.  It supplies the
// coordinates for "bookmark my current position" and the hangar a bookmark
// voucher is created in.
type Location struct {
	StationID int64 // 0 when in space
	ShipID    int64
	SysID     int64
	InSpace   bool
	X, Y, Z   float64
}

// Handle dispatches a CallReq addressed to the "bookmark" service.
func Handle(
	c *evenet.Conn,
	addr string,
	cs *server.CallStream,
	pkt *evenet.PyPacket,
	accountID int64,
	charID int64,
	loc Location,
	sqldb *sql.DB,
) error {
	ctx := context.Background()

	switch cs.Method {
	case "GetBookmarks":
		rows := buildMissionBookmarks(charID, sqldb)
		stored, err := evedb.GetBookmarks(ctx, sqldb, charID)
		if err != nil {
			log.Printf("[%s] bookmark.GetBookmarks: %v", addr, err)
		}
		for _, b := range stored {
			rows = append(rows, bookmarkRow(b))
		}
		log.Printf("[%s] → bookmark.GetBookmarks (%d rows: %d stored)", addr, len(rows), len(stored))
		return server.SendCallRsp(c, pkt, accountID, makeRowset(bookmarkCols, rows))

	case "BookmarkLocation":
		return handleBookmarkLocation(c, addr, cs, pkt, accountID, charID, loc, sqldb)

	case "UpdateBookmark":
		// addressbook.py: UpdateBookmark(bookmarkID, None, memo)
		bookmarkID := argInt(cs, 0)
		memo := argStr(cs, 2)
		if err := evedb.UpdateBookmarkMemo(ctx, sqldb, charID, bookmarkID, memo); err != nil {
			log.Printf("[%s] bookmark.UpdateBookmark: %v", addr, err)
		}
		log.Printf("[%s] → bookmark.UpdateBookmark %d", addr, bookmarkID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "DeleteBookmarks":
		ids := argIntList(cs, 0)
		n, err := evedb.DeleteBookmarks(ctx, sqldb, charID, ids)
		if err != nil {
			log.Printf("[%s] bookmark.DeleteBookmarks: %v", addr, err)
		}
		log.Printf("[%s] → bookmark.DeleteBookmarks (%d of %d removed)", addr, n, len(ids))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})

	case "CreateBookmarkVoucher":
		// addressbook.py: CreateBookmarkVoucher(bookmarkID, memo[:100])
		voucher := createVoucher(ctx, addr, charID, argInt(cs, 0), argStr(cs, 1), loc, sqldb)
		if voucher == nil {
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		return server.SendCallRsp(c, pkt, accountID, voucher)

	case "CreateBookmarkVouchers":
		// addressbook.py passes a list of (bookmarkID,) or (bookmarkID, memo).
		var out []marshal.PyRep
		for _, entry := range argList(cs, 0) {
			t, ok := entry.(marshal.PyTuple)
			if !ok || len(t.Items) == 0 {
				continue
			}
			bookmarkID := asInt(t.Items[0])
			memo := ""
			if len(t.Items) > 1 {
				memo = asStr(t.Items[1])
			}
			if v := createVoucher(ctx, addr, charID, bookmarkID, memo, loc, sqldb); v != nil {
				out = append(out, v)
			}
		}
		log.Printf("[%s] → bookmark.CreateBookmarkVouchers (%d created)", addr, len(out))
		return server.SendCallRsp(c, pkt, accountID, marshal.PyList{Items: out})

	case "AddBookmarkFromVoucher":
		itemID := argInt(cs, 0)
		bm, err := evedb.RedeemBookmarkVoucher(ctx, sqldb, charID, itemID)
		if err != nil {
			log.Printf("[%s] bookmark.AddBookmarkFromVoucher: %v", addr, err)
		}
		if bm == nil {
			log.Printf("[%s] → bookmark.AddBookmarkFromVoucher: item %d is not a redeemable voucher", addr, itemID)
			return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
		}
		log.Printf("[%s] → bookmark.AddBookmarkFromVoucher: item %d → bookmark %d", addr, itemID, bm.BookmarkID)
		return server.SendCallRsp(c, pkt, accountID, bookmarkKeyVal(*bm))

	default:
		server.RecordUnhandled(addr, "bookmark", cs, true)
		log.Printf("[%s] bookmark: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// HandleVoucher dispatches the "voucher" remote service. voucherCache.py calls
// GetObject(itemID) for every groupVoucher item (hangar, cargo, trade window), so
// ownership is not checked here.
func HandleVoucher(
	c *evenet.Conn,
	addr string,
	cs *server.CallStream,
	pkt *evenet.PyPacket,
	accountID int64,
	sqldb *sql.DB,
) error {
	switch cs.Method {
	case "GetObject":
		itemID := argInt(cs, 0)
		it, err := evedb.GetBookmarkVoucherItem(context.Background(), sqldb, itemID)
		if err != nil {
			log.Printf("[%s] voucher.GetObject %d: %v", addr, itemID, err)
		}
		if it == nil {
			// uix.GetItemName calls GetDescription() with no None check, so an unknown item
			// still needs a well-formed voucher object.
			it = &evedb.BookmarkVoucherItem{ItemID: itemID, TypeID: evedb.TypeBookmark}
		}
		log.Printf("[%s] → voucher.GetObject %d (%q)", addr, itemID, it.ItemName)
		return server.SendCallRsp(c, pkt, accountID, voucherObject(*it))

	default:
		server.RecordUnhandled(addr, "voucher", cs, true)
		log.Printf("[%s] voucher: unhandled method %q — returning None", addr, cs.Method)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}
}

// voucherObject shapes an entity row as a voucher.Bookmark instance. The class is
// __passbyvalue__ with no __setstate__, so the client updates __dict__ with exactly
// header, data and description. A non-empty description is the visible name
// (the itemName column holds the zipped memo).
func voucherObject(it evedb.BookmarkVoucherItem) marshal.PyRep {
	desc := it.ItemName
	if desc == "" {
		desc = "Bookmark"
	}
	cols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
	}
	header := make([]marshal.PyRep, len(cols))
	for i, col := range cols {
		header[i] = marshal.PyString{Value: col}
	}
	line := []marshal.PyRep{
		marshal.PyInt{Value: uint64(it.ItemID)},
		marshal.PyInt{Value: uint64(it.TypeID)},
		marshal.PyInt{Value: uint64(it.OwnerID)},
		marshal.PyInt{Value: uint64(it.LocationID)},
		marshal.PyInt{Value: uint64(it.Flag)},
		marshal.PyInt{Value: 0}, // contraband
		marshal.PyInt{Value: uint64(it.Singleton)},
		marshal.PyInt{Value: uint64(it.Quantity)},
		marshal.PyInt{Value: 24}, // groupVoucher
		marshal.PyInt{Value: 5},  // category of group 24
		marshal.PyString{Value: it.CustomInfo},
	}
	return marshal.PyObject{
		Type: "voucher.Bookmark",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: marshal.PyList{Items: header}},
			{Key: marshal.PyString{Value: "data"}, Value: marshal.PyList{Items: line}},
			{Key: marshal.PyString{Value: "description"}, Value: marshal.PyString{Value: desc}},
		}},
	}
}

// handleBookmarkLocation stores a new bookmark and replies with the 7-tuple
// addressbook.BookmarkLocation unpacks:
//
//	(bookmarkID, itemID, typeID, x, y, z, locationID)
func handleBookmarkLocation(
	c *evenet.Conn, addr string, cs *server.CallStream, pkt *evenet.PyPacket,
	accountID, charID int64, loc Location, sqldb *sql.DB,
) error {
	ctx := context.Background()
	itemID := argInt(cs, 0)
	memo := argStr(cs, 2)

	bm := evedb.Bookmark{OwnerID: charID, Memo: memo}

	// A static map object (celestial, station, stargate) is bookmarked by ID:
	// the client resolves its position locally, so no coordinates are stored.
	info, err := evedb.ResolveBookmarkTarget(ctx, sqldb, itemID)
	if err != nil {
		log.Printf("[%s] bookmark.BookmarkLocation resolve: %v", addr, err)
	}
	switch {
	case info.Found:
		bm.ItemID = info.ItemID
		bm.TypeID = info.TypeID
		bm.LocationID = info.LocationID
	case loc.InSpace:
		// Anything else while undocked is a coordinate bookmark of the ship's position;
		// typeID 5 is "Solar System".
		bm.ItemID = 0
		bm.TypeID = 5
		bm.X, bm.Y, bm.Z = loc.X, loc.Y, loc.Z
		bm.LocationID = loc.SysID
	default:
		log.Printf("[%s] → bookmark.BookmarkLocation: item %d is not bookmarkable from a station", addr, itemID)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyInt{Value: 0}, marshal.PyInt{Value: 0}, marshal.PyInt{Value: 0},
			marshal.PyReal{Value: 0}, marshal.PyReal{Value: 0}, marshal.PyReal{Value: 0},
			marshal.PyInt{Value: 0},
		}})
	}

	saved, err := evedb.InsertBookmark(ctx, sqldb, bm)
	if err != nil {
		log.Printf("[%s] bookmark.BookmarkLocation insert: %v", addr, err)
		return server.SendCallRsp(c, pkt, accountID, marshal.PyNone{})
	}

	log.Printf("[%s] → bookmark.BookmarkLocation: created %d (item=%d type=%d loc=%d)",
		addr, saved.BookmarkID, saved.ItemID, saved.TypeID, saved.LocationID)
	return server.SendCallRsp(c, pkt, accountID, marshal.PyTuple{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(saved.BookmarkID)},
		marshal.PyInt{Value: uint64(saved.ItemID)},
		marshal.PyInt{Value: uint64(saved.TypeID)},
		marshal.PyReal{Value: saved.X},
		marshal.PyReal{Value: saved.Y},
		marshal.PyReal{Value: saved.Z},
		marshal.PyInt{Value: uint64(saved.LocationID)},
	}})
}

// createVoucher materialises one bookmark as a hangar item.  Returns nil when
// the bookmark is unknown, is a mission-derived row, or the character is not
// docked (there is no hangar to put the item in).
func createVoucher(
	ctx context.Context, addr string, charID, bookmarkID int64, memo string,
	loc Location, sqldb *sql.DB,
) marshal.PyRep {
	if loc.StationID == 0 {
		log.Printf("[%s] → bookmark.CreateBookmarkVoucher: not docked, refusing", addr)
		return nil
	}
	if bookmarkID < evedb.BookmarkIDFloor {
		log.Printf("[%s] → bookmark.CreateBookmarkVoucher: %d is a mission bookmark, refusing", addr, bookmarkID)
		return nil
	}

	bm, err := evedb.GetBookmark(ctx, sqldb, bookmarkID)
	if err != nil {
		log.Printf("[%s] bookmark.CreateBookmarkVoucher lookup: %v", addr, err)
		return nil
	}
	if bm == nil || bm.OwnerID != charID {
		return nil
	}

	voucherID, err := evedb.CreateBookmarkVoucher(ctx, sqldb, *bm, charID, loc.StationID, memo)
	if err != nil {
		log.Printf("[%s] bookmark.CreateBookmarkVoucher: %v", addr, err)
		return nil
	}

	log.Printf("[%s] → bookmark.CreateBookmarkVoucher: bookmark %d → item %d", addr, bookmarkID, voucherID)
	// container.py reads newbookmark.voucherID to add the item to the hangar view.
	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "voucherID"}, Value: marshal.PyInt{Value: uint64(voucherID)}},
			{Key: marshal.PyString{Value: "bookmarkID"}, Value: marshal.PyInt{Value: uint64(bookmarkID)}},
			{Key: marshal.PyString{Value: "typeID"}, Value: marshal.PyInt{Value: evedb.TypeBookmark}},
			{Key: marshal.PyString{Value: "ownerID"}, Value: marshal.PyInt{Value: uint64(charID)}},
		}},
	}
}

// ─── result shaping ────────────────────────────────────────────────────────

// createdRep converts a stored unix-seconds timestamp to a win32 FILETIME PyInt.
// util.FmtDate needs a Python long (FILETIME magnitude); an int fails the Places tab.
func createdRep(unixSec int64) marshal.PyRep {
	return marshal.PyInt{Value: uint64(unixSec+11644473600) * 10000000}
}

func bookmarkRow(b evedb.Bookmark) marshal.PyRep {
	return marshal.PyList{Items: []marshal.PyRep{
		marshal.PyInt{Value: uint64(b.BookmarkID)},
		marshal.PyInt{Value: uint64(b.OwnerID)},
		marshal.PyInt{Value: uint64(b.ItemID)},
		marshal.PyInt{Value: uint64(b.TypeID)},
		marshal.PyInt{Value: uint64(b.Flag)},
		marshal.PyString{Value: b.Memo},
		createdRep(b.Created),
		marshal.PyReal{Value: b.X},
		marshal.PyReal{Value: b.Y},
		marshal.PyReal{Value: b.Z},
		marshal.PyInt{Value: uint64(b.LocationID)},
	}}
}

// bookmarkKeyVal is the single-bookmark form OnBookmarkAdd consumes; it reads
// the fields as attributes, so a util.KeyVal rather than a row.
func bookmarkKeyVal(b evedb.Bookmark) marshal.PyRep {
	return marshal.PyObject{
		Type: "util.KeyVal",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "bookmarkID"}, Value: marshal.PyInt{Value: uint64(b.BookmarkID)}},
			{Key: marshal.PyString{Value: "ownerID"}, Value: marshal.PyInt{Value: uint64(b.OwnerID)}},
			{Key: marshal.PyString{Value: "itemID"}, Value: marshal.PyInt{Value: uint64(b.ItemID)}},
			{Key: marshal.PyString{Value: "typeID"}, Value: marshal.PyInt{Value: uint64(b.TypeID)}},
			{Key: marshal.PyString{Value: "flag"}, Value: marshal.PyInt{Value: uint64(b.Flag)}},
			{Key: marshal.PyString{Value: "memo"}, Value: marshal.PyString{Value: b.Memo}},
			{Key: marshal.PyString{Value: "created"}, Value: createdRep(b.Created)},
			{Key: marshal.PyString{Value: "x"}, Value: marshal.PyReal{Value: b.X}},
			{Key: marshal.PyString{Value: "y"}, Value: marshal.PyReal{Value: b.Y}},
			{Key: marshal.PyString{Value: "z"}, Value: marshal.PyReal{Value: b.Z}},
			{Key: marshal.PyString{Value: "locationID"}, Value: marshal.PyInt{Value: uint64(b.LocationID)}},
		}},
	}
}

// makeRowset builds a util.Rowset.  Duplicated from db.makeRowset because that
// helper is unexported.
func makeRowset(cols []string, rows []marshal.PyRep) marshal.PyRep {
	header := make([]marshal.PyRep, len(cols))
	for i, c := range cols {
		header[i] = marshal.PyString{Value: c}
	}
	headerList := marshal.PyList{Items: header}
	return marshal.PyObject{
		Type: "util.Rowset",
		Args: marshal.PyDict{Items: []marshal.DictEntry{
			{Key: marshal.PyString{Value: "header"}, Value: headerList},
			{Key: marshal.PyString{Value: "RowClass"}, Value: marshal.PyString{Value: "util.Row", IsType1: true}},
			{Key: marshal.PyString{Value: "lines"}, Value: marshal.PyList{Items: rows}},
			{Key: marshal.PyString{Value: "columns"}, Value: headerList},
		}},
	}
}

// ─── argument helpers ──────────────────────────────────────────────────────

func argAt(cs *server.CallStream, idx int) marshal.PyRep {
	if idx >= len(cs.ArgTuple.Items) {
		return nil
	}
	return cs.ArgTuple.Items[idx]
}

func asInt(v marshal.PyRep) int64 {
	if i, ok := v.(marshal.PyInt); ok {
		return int64(i.Value)
	}
	return 0
}

func asStr(v marshal.PyRep) string {
	if s, ok := v.(marshal.PyString); ok {
		return s.Value
	}
	return ""
}

func argInt(cs *server.CallStream, idx int) int64  { return asInt(argAt(cs, idx)) }
func argStr(cs *server.CallStream, idx int) string { return asStr(argAt(cs, idx)) }

// argList returns the items of a PyList or PyTuple argument.
func argList(cs *server.CallStream, idx int) []marshal.PyRep {
	switch v := argAt(cs, idx).(type) {
	case marshal.PyList:
		return v.Items
	case marshal.PyTuple:
		return v.Items
	}
	return nil
}

func argIntList(cs *server.CallStream, idx int) []int64 {
	items := argList(cs, idx)
	out := make([]int64, 0, len(items))
	for _, it := range items {
		if id := asInt(it); id != 0 {
			out = append(out, id)
		}
	}
	return out
}

// buildMissionBookmarks synthesizes bookmark rows from accepted missions.
func buildMissionBookmarks(charID int64, sqldb *sql.DB) []marshal.PyRep {
	ctx := context.Background()
	missions, err := evedb.GetAcceptedMissions(ctx, sqldb, charID)
	if err != nil {
		log.Printf("bookmark: GetAcceptedMissions error: %v", err)
		return nil
	}

	var rows []marshal.PyRep
	for _, m := range missions {
		tmpl, _ := evedb.GetTemplateByID(ctx, sqldb, m.TemplateID)
		name := "Mission"
		if tmpl != nil {
			name = tmpl.MissionName
		}

		var createdSec int64
		if m.AcceptedAt.Valid {
			createdSec = m.AcceptedAt.Time.Unix()
		}
		created := createdRep(createdSec)

		hasBookmarkCoords := m.BookmarkX != 0 || m.BookmarkY != 0 || m.BookmarkZ != 0

		// Kill missions with deadspace coordinates.
		if hasBookmarkCoords && m.DestSystemID > 0 {
			rows = append(rows, marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(m.MissionID)},                    // bookmarkID
				marshal.PyInt{Value: uint64(charID)},                         // ownerID
				marshal.PyInt{Value: 0},                                      // itemID
				marshal.PyInt{Value: 5},                                      // typeID (Solar System)
				marshal.PyInt{Value: 0},                                      // flag
				marshal.PyString{Value: fmt.Sprintf("%s (Deadspace)", name)}, // memo
				created, // created (FILETIME)
				marshal.PyReal{Value: m.BookmarkX},                           // x
				marshal.PyReal{Value: m.BookmarkY},                           // y
				marshal.PyReal{Value: m.BookmarkZ},                           // z
				marshal.PyInt{Value: uint64(m.DestSystemID)},                 // locationID
			}})
		}

		// Courier missions with destination station.
		if m.DestStationID > 0 && !hasBookmarkCoords {
			locationID := int64(0)
			loc, err := evedb.GetStationLocation(ctx, sqldb, m.DestStationID)
			if err == nil {
				locationID = loc.SolarSystemID
			}
			rows = append(rows, marshal.PyList{Items: []marshal.PyRep{
				marshal.PyInt{Value: uint64(m.MissionID)},                      // bookmarkID
				marshal.PyInt{Value: uint64(charID)},                           // ownerID
				marshal.PyInt{Value: uint64(m.DestStationID)},                  // itemID
				marshal.PyInt{Value: 15},                                       // typeID (Station)
				marshal.PyInt{Value: 0},                                        // flag
				marshal.PyString{Value: fmt.Sprintf("%s - Destination", name)}, // memo
				created, // created (FILETIME)
				marshal.PyReal{Value: 0},                                       // x
				marshal.PyReal{Value: 0},                                       // y
				marshal.PyReal{Value: 0},                                       // z
				marshal.PyInt{Value: uint64(locationID)},                       // locationID
			}})
		}
	}

	return rows
}
