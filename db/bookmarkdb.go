package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// TypeBookmark is the invTypes ID of the "Bookmark" voucher item that a
// bookmark can be turned into and traded (invTypes row 51, group 24).
const TypeBookmark = 51

// Bookmark mirrors a row of the `bookmarks` table.
type Bookmark struct {
	BookmarkID int64
	OwnerID    int64
	ItemID     int64
	TypeID     int64
	Flag       int64
	Memo       string
	Created    int64
	X, Y, Z    float64
	LocationID int64
}

// ensureBookmarksTable creates the table when a fresh database is missing it
// and makes bookmarkID auto-incrementing.  The shipped dump declares the
// column as a plain PK, so the ALTER is what actually lets us insert.
func ensureBookmarksTable(ctx context.Context, db *sql.DB) error {
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS bookmarks (
		  bookmarkID INT UNSIGNED   NOT NULL AUTO_INCREMENT,
		  ownerID    INT UNSIGNED   NOT NULL DEFAULT 0,
		  itemID     INT UNSIGNED   NOT NULL DEFAULT 0,
		  typeID     INT UNSIGNED   NOT NULL DEFAULT 0,
		  flag       INT UNSIGNED   NOT NULL DEFAULT 0,
		  memo       VARCHAR(85)    NOT NULL DEFAULT '',
		  created    INT UNSIGNED   NOT NULL DEFAULT 0,
		  x          DOUBLE         NOT NULL DEFAULT 0,
		  y          DOUBLE         NOT NULL DEFAULT 0,
		  z          DOUBLE         NOT NULL DEFAULT 0,
		  locationID INT UNSIGNED   NOT NULL DEFAULT 0,
		  PRIMARY KEY (bookmarkID),
		  KEY ownerIdx (ownerID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`); err != nil {
		return err
	}
	// Idempotent: a no-op once the column is already AUTO_INCREMENT.
	_, _ = db.ExecContext(ctx, `
		ALTER TABLE bookmarks
		MODIFY bookmarkID INT UNSIGNED NOT NULL AUTO_INCREMENT`)
	// GetBookmarks also returns synthetic rows keyed by missionID, so real bookmarks start above any
	// mission ID. MySQL ignores an AUTO_INCREMENT below the current value.
	_, _ = db.ExecContext(ctx, fmt.Sprintf(
		`ALTER TABLE bookmarks AUTO_INCREMENT = %d`, BookmarkIDFloor))
	return nil
}

// BookmarkIDFloor is the lowest ID assigned to a player-created bookmark.
// Anything below it in a bookmark rowset is a mission-derived synthetic row.
const BookmarkIDFloor = 1000000000

// GetBookmarks returns every bookmark owned by a character.
func GetBookmarks(ctx context.Context, db *sql.DB, ownerID int64) ([]Bookmark, error) {
	if err := ensureBookmarksTable(ctx, db); err != nil {
		return nil, fmt.Errorf("GetBookmarks ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT bookmarkID, ownerID, itemID, typeID, flag, memo, created, x, y, z, locationID
		FROM bookmarks
		WHERE ownerID = ?
		ORDER BY bookmarkID`, ownerID)
	if err != nil {
		return nil, fmt.Errorf("GetBookmarks: %w", err)
	}
	defer rows.Close()

	var out []Bookmark
	for rows.Next() {
		var b Bookmark
		if err := rows.Scan(&b.BookmarkID, &b.OwnerID, &b.ItemID, &b.TypeID, &b.Flag,
			&b.Memo, &b.Created, &b.X, &b.Y, &b.Z, &b.LocationID); err != nil {
			return nil, fmt.Errorf("GetBookmarks scan: %w", err)
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// GetBookmark returns a single bookmark, or nil when it does not exist.
func GetBookmark(ctx context.Context, db *sql.DB, bookmarkID int64) (*Bookmark, error) {
	if err := ensureBookmarksTable(ctx, db); err != nil {
		return nil, fmt.Errorf("GetBookmark ensureTable: %w", err)
	}
	var b Bookmark
	err := db.QueryRowContext(ctx, `
		SELECT bookmarkID, ownerID, itemID, typeID, flag, memo, created, x, y, z, locationID
		FROM bookmarks
		WHERE bookmarkID = ?`, bookmarkID,
	).Scan(&b.BookmarkID, &b.OwnerID, &b.ItemID, &b.TypeID, &b.Flag,
		&b.Memo, &b.Created, &b.X, &b.Y, &b.Z, &b.LocationID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetBookmark: %w", err)
	}
	return &b, nil
}

// InsertBookmark stores a new bookmark and returns it with the assigned ID.
// memo is truncated to the column width so a long caption cannot fail the insert.
func InsertBookmark(ctx context.Context, db *sql.DB, b Bookmark) (*Bookmark, error) {
	if err := ensureBookmarksTable(ctx, db); err != nil {
		return nil, fmt.Errorf("InsertBookmark ensureTable: %w", err)
	}
	if b.Created == 0 {
		b.Created = time.Now().Unix()
	}
	b.Memo = truncateMemo(b.Memo)

	res, err := db.ExecContext(ctx, `
		INSERT INTO bookmarks (ownerID, itemID, typeID, flag, memo, created, x, y, z, locationID)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		b.OwnerID, b.ItemID, b.TypeID, b.Flag, b.Memo, b.Created, b.X, b.Y, b.Z, b.LocationID)
	if err != nil {
		return nil, fmt.Errorf("InsertBookmark: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, fmt.Errorf("InsertBookmark LastInsertId: %w", err)
	}
	b.BookmarkID = id
	return &b, nil
}

// UpdateBookmarkMemo rewrites the memo of a bookmark the character owns.
func UpdateBookmarkMemo(ctx context.Context, db *sql.DB, ownerID, bookmarkID int64, memo string) error {
	if err := ensureBookmarksTable(ctx, db); err != nil {
		return fmt.Errorf("UpdateBookmarkMemo ensureTable: %w", err)
	}
	_, err := db.ExecContext(ctx, `
		UPDATE bookmarks SET memo = ? WHERE bookmarkID = ? AND ownerID = ?`,
		truncateMemo(memo), bookmarkID, ownerID)
	if err != nil {
		return fmt.Errorf("UpdateBookmarkMemo: %w", err)
	}
	return nil
}

// DeleteBookmarks removes the given bookmarks, ignoring any not owned by the
// character.  Returns the number of rows actually deleted.
func DeleteBookmarks(ctx context.Context, db *sql.DB, ownerID int64, bookmarkIDs []int64) (int64, error) {
	if len(bookmarkIDs) == 0 {
		return 0, nil
	}
	if err := ensureBookmarksTable(ctx, db); err != nil {
		return 0, fmt.Errorf("DeleteBookmarks ensureTable: %w", err)
	}

	query := `DELETE FROM bookmarks WHERE ownerID = ? AND bookmarkID IN (` +
		placeholders(len(bookmarkIDs)) + `)`
	args := make([]any, 0, len(bookmarkIDs)+1)
	args = append(args, ownerID)
	for _, id := range bookmarkIDs {
		args = append(args, id)
	}
	res, err := db.ExecContext(ctx, query, args...)
	if err != nil {
		return 0, fmt.Errorf("DeleteBookmarks: %w", err)
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// BookmarkLocationInfo describes where a bookmark target sits in space.
type BookmarkLocationInfo struct {
	ItemID     int64
	TypeID     int64
	X, Y, Z    float64
	LocationID int64 // solar system the bookmark belongs to
	Found      bool
}

// ResolveBookmarkTarget looks an itemID up in mapDenormalize (celestials, stations, stargates)
// so a bookmark can record its type and system. Found is false for anything that is not a
// static map object; the caller then uses the character's live position.
func ResolveBookmarkTarget(ctx context.Context, db *sql.DB, itemID int64) (BookmarkLocationInfo, error) {
	info := BookmarkLocationInfo{ItemID: itemID}
	var solarSystemID sql.NullInt64
	err := db.QueryRowContext(ctx, `
		SELECT typeID, x, y, z, solarSystemID
		FROM mapDenormalize
		WHERE itemID = ?`, itemID,
	).Scan(&info.TypeID, &info.X, &info.Y, &info.Z, &solarSystemID)
	if errors.Is(err, sql.ErrNoRows) {
		return info, nil
	}
	if err != nil {
		return info, fmt.Errorf("ResolveBookmarkTarget: %w", err)
	}
	if solarSystemID.Valid {
		info.LocationID = solarSystemID.Int64
	} else {
		// Solar systems themselves have a NULL solarSystemID column.
		info.LocationID = itemID
	}
	info.Found = true
	return info, nil
}

// ─── bookmark vouchers ─────────────────────────────────────────────────────
//
// A voucher is a real inventory item of type 51 ("Bookmark") that carries the
// bookmark payload: the coordinates live in entity.x/y/z, the caption in
// itemName, and the bookmarked object in customInfo as "bm:<itemID>:<typeID>".

// CreateBookmarkVoucher turns a bookmark into a tradeable item in the owner's
// hangar at locationID.  Returns the new entity itemID.
func CreateBookmarkVoucher(ctx context.Context, db *sql.DB, b Bookmark, ownerID, locationID int64, memo string) (int64, error) {
	if memo == "" {
		memo = b.Memo
	}
	customInfo := fmt.Sprintf("bm:%d:%d", b.ItemID, b.TypeID)

	res, err := db.ExecContext(ctx, `
		INSERT INTO entity
		  (itemName, typeID, ownerID, locationID, flag, contraband, singleton, quantity, x, y, z, customInfo)
		VALUES (?, ?, ?, ?, 4, 0, 1, 1, ?, ?, ?, ?)`,
		truncateMemo(memo), TypeBookmark, ownerID, locationID, b.X, b.Y, b.Z, customInfo)
	if err != nil {
		return 0, fmt.Errorf("CreateBookmarkVoucher: %w", err)
	}
	itemID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("CreateBookmarkVoucher LastInsertId: %w", err)
	}
	// The voucher's own locationID column holds the hangar; the bookmarked
	// system is recorded separately so redeeming it can restore the bookmark.
	if _, err := db.ExecContext(ctx,
		`UPDATE entity SET customInfo = ? WHERE itemID = ?`,
		fmt.Sprintf("%s:%d", customInfo, b.LocationID), itemID); err != nil {
		return 0, fmt.Errorf("CreateBookmarkVoucher customInfo: %w", err)
	}
	return itemID, nil
}

// BookmarkVoucherItem is the entity row of a bookmark voucher, in the shape the
// client's voucher.Bookmark instance carries (voucher.py reads header/line).
type BookmarkVoucherItem struct {
	ItemID     int64
	TypeID     int64
	OwnerID    int64
	LocationID int64
	Flag       int64
	Singleton  int64
	Quantity   int64
	ItemName   string
	CustomInfo string
}

// GetBookmarkVoucherItem loads a voucher item by ID. Returns nil (no error) when the item does
// not exist or is not a bookmark voucher. Ownership is not checked: the client resolves voucher
// names for items on the other side of a trade window too.
func GetBookmarkVoucherItem(ctx context.Context, db *sql.DB, itemID int64) (*BookmarkVoucherItem, error) {
	var (
		it         BookmarkVoucherItem
		customInfo sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT itemID, typeID, ownerID, locationID, flag, singleton, quantity, itemName, customInfo
		FROM entity WHERE itemID = ? AND typeID = ?`, itemID, TypeBookmark,
	).Scan(&it.ItemID, &it.TypeID, &it.OwnerID, &it.LocationID, &it.Flag,
		&it.Singleton, &it.Quantity, &it.ItemName, &customInfo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("GetBookmarkVoucherItem: %w", err)
	}
	it.CustomInfo = customInfo.String
	return &it, nil
}

// RedeemBookmarkVoucher consumes a voucher item owned by charID and writes the
// bookmark it carried back into the bookmarks table.  Returns nil (no error)
// when the item does not exist, is not a bookmark, or belongs to someone else.
func RedeemBookmarkVoucher(ctx context.Context, db *sql.DB, charID, itemID int64) (*Bookmark, error) {
	var (
		itemName   string
		typeID     int64
		ownerID    int64
		x, y, z    float64
		customInfo sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT itemName, typeID, ownerID, x, y, z, customInfo
		FROM entity WHERE itemID = ?`, itemID,
	).Scan(&itemName, &typeID, &ownerID, &x, &y, &z, &customInfo)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("RedeemBookmarkVoucher lookup: %w", err)
	}
	if typeID != TypeBookmark || ownerID != charID {
		return nil, nil
	}

	targetItemID, targetTypeID, locationID := parseVoucherInfo(customInfo.String)

	bm, err := InsertBookmark(ctx, db, Bookmark{
		OwnerID:    charID,
		ItemID:     targetItemID,
		TypeID:     targetTypeID,
		Memo:       itemName,
		X:          x,
		Y:          y,
		Z:          z,
		LocationID: locationID,
	})
	if err != nil {
		return nil, err
	}
	if err := DeleteEntity(ctx, db, itemID); err != nil {
		return nil, fmt.Errorf("RedeemBookmarkVoucher delete: %w", err)
	}
	return bm, nil
}

// parseVoucherInfo decodes "bm:<itemID>:<typeID>:<locationID>".  Missing or
// malformed fields decode as 0, which the client renders as an unnamed
// coordinate bookmark rather than failing.
func parseVoucherInfo(s string) (itemID, typeID, locationID int64) {
	var parsed [3]int64
	n, _ := fmt.Sscanf(s, "bm:%d:%d:%d", &parsed[0], &parsed[1], &parsed[2])
	if n < 1 {
		return 0, 0, 0
	}
	return parsed[0], parsed[1], parsed[2]
}

// truncateMemo clips a memo to the 85-character `memo` column width.
func truncateMemo(memo string) string {
	if len(memo) > 85 {
		return memo[:85]
	}
	return memo
}
