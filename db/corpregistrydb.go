package db

import (
	"context"
	"database/sql"
	"fmt"

	"gurimoon/corp"
	"gurimoon/marshal"
)

// This file backs the read side of the corpRegistry bound object behind the
// corporation management window (ui/services/corporation/*). Every getter
// returns a correctly shaped rowset even when empty: the client indexes into
// the result without a nil check, and a bare None blanks the window.

// ---- titles ----

var corpTitleCols = []string{
	"titleID", "titleName",
	"roles", "grantableRoles",
	"rolesAtHQ", "grantableRolesAtHQ",
	"rolesAtBase", "grantableRolesAtBase",
	"rolesAtOther", "grantableRolesAtOther",
}

// ensureCorpTitlesTableGuard runs ensureCorpTitlesTableDDL once per process: it is called on request paths, and
// even a no-op CREATE TABLE IF NOT EXISTS / ALTER takes a metadata lock and can
// touch the table definition each time.
var ensureCorpTitlesTableGuard ensureGuard

func ensureCorpTitlesTable(ctx context.Context, db *sql.DB) error {
	return ensureCorpTitlesTableGuard.do(func() error { return ensureCorpTitlesTableDDL(ctx, db) })
}

func ensureCorpTitlesTableDDL(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS crpTitles (
		  corporationID         INT UNSIGNED  NOT NULL,
		  titleID               INT UNSIGNED  NOT NULL,
		  titleName             VARCHAR(100)  NOT NULL DEFAULT '',
		  roles                 BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  grantableRoles        BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  rolesAtHQ             BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  grantableRolesAtHQ    BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  rolesAtBase           BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  grantableRolesAtBase  BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  rolesAtOther          BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  grantableRolesAtOther BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  PRIMARY KEY (corporationID, titleID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	return err
}

// GetCorpTitles returns the corporation's titles keyed by titleID.
func GetCorpTitles(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	if err := ensureCorpTitlesTable(ctx, db); err != nil {
		return makeIndexRowset("titleID", corpTitleCols, nil), nil
	}
	rows, err := db.QueryContext(ctx, `
		SELECT titleID, titleName, roles, grantableRoles,
		       rolesAtHQ, grantableRolesAtHQ, rolesAtBase, grantableRolesAtBase,
		       rolesAtOther, grantableRolesAtOther
		FROM crpTitles WHERE corporationID = ? ORDER BY titleID`, corpID)
	if err != nil {
		return makeIndexRowset("titleID", corpTitleCols, nil), nil
	}
	defer rows.Close()

	var out [][]marshal.PyRep
	for rows.Next() {
		var titleID int64
		var titleName string
		var r [8]uint64
		if err := rows.Scan(&titleID, &titleName,
			&r[0], &r[1], &r[2], &r[3], &r[4], &r[5], &r[6], &r[7]); err != nil {
			return nil, fmt.Errorf("GetCorpTitles scan: %w", err)
		}
		out = append(out, []marshal.PyRep{
			intRep(titleID), strRep(titleName),
			intRep(int64(r[0])), intRep(int64(r[1])),
			intRep(int64(r[2])), intRep(int64(r[3])),
			intRep(int64(r[4])), intRep(int64(r[5])),
			intRep(int64(r[6])), intRep(int64(r[7])),
		})
	}
	return makeIndexRowset("titleID", corpTitleCols, out), rows.Err()
}

// ---- roles ----

var corpRoleCols = []string{"roleID", "roleName", "shortDescription", "description"}

// GetCorpRoles returns the corporation role catalogue from corp.Catalogue(),
// not the shipped everoles table (that one is the account/GM role table).
// It is a plain util.Rowset: the client iterates it for Row objects and sorts
// roles.lines in place, which an IndexRowset does not support.
func GetCorpRoles(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	cat := corp.Catalogue()
	out := make([][]marshal.PyRep, 0, len(cat))
	for _, r := range cat {
		out = append(out, []marshal.PyRep{
			marshal.PyInt{Value: r.ID}, strRep(r.Name),
			strRep(r.Short), strRep(r.Description),
		})
	}
	return makeRowset(corpRoleCols, out), nil
}

var corpRoleGroupCols = []string{
	"roleGroupID", "roleGroupName", "isDivisional", "roleMask",
	"appliesTo", "appliesToGrantable",
}

// GetCorpRoleGroups returns the eight role groups the window's Type combo is
// built from. It is never empty: with no rows the header is None, and
// corp_ui_members and corp_ui_member_titles index group 1 unchecked
// (KeyError: 1). It is a plain util.Rowset: the client reads
// roleGroup.header per row, and an IndexRowset iterates its keys.
func GetCorpRoleGroups(ctx context.Context, db *sql.DB) marshal.PyRep {
	groups := corp.Groups()
	out := make([][]marshal.PyRep, 0, len(groups))
	for _, g := range groups {
		divisional := int64(0)
		if g.IsDivisional {
			divisional = 1
		}
		out = append(out, []marshal.PyRep{
			intRep(g.ID), strRep(g.Name), intRep(divisional),
			marshal.PyInt{Value: g.Mask},
			strRep(g.AppliesTo), strRep(g.AppliesToGrantable),
		})
	}
	return makeRowset(corpRoleGroupCols, out)
}

// ---- members ----

// GetCorpMemberIDs returns the character IDs in a corporation.
func GetCorpMemberIDs(ctx context.Context, db *sql.DB, corpID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT characterID FROM character_ WHERE corporationID = ?`, corpID)
	if err != nil {
		return nil, fmt.Errorf("GetCorpMemberIDs: %w", err)
	}
	defer rows.Close()

	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("GetCorpMemberIDs scan: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetCorpMember returns a single member row (the members rowset's columns),
// or nil when the character is not in the corporation.
func GetCorpMember(ctx context.Context, db *sql.DB, corpID, charID int64) (marshal.PyRep, error) {
	vals, err := GetCorpMemberLine(ctx, db, corpID, charID)
	if err != nil || vals == nil {
		return nil, err
	}
	return makeKeyVal(corpMemberCols, vals), nil
}

// GetCorpMemberLine returns one member's values in CorpMemberColumns order, or
// nil when not a member. OnCorporationMemberChanged dicts are keyed by these
// columns.
func GetCorpMemberLine(ctx context.Context, db *sql.DB, corpID, charID int64) ([]marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		corpMemberQuery+` AND c.characterID = ?`, corpID, charID)
	if err != nil {
		return nil, fmt.Errorf("GetCorpMember: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, nil
	}
	_, line, err := scanMemberLine(rows)
	if err != nil {
		return nil, fmt.Errorf("GetCorpMember scan: %w", err)
	}
	return line.(marshal.PyList).Items, nil
}

// CorpMemberColumns is the member rowset header, copied so a caller cannot
// reorder the shared slice.
func CorpMemberColumns() []string {
	return append([]string(nil), corpMemberCols...)
}

// ---- offices and stations ----

// corpOfficeCols is the header of corpRegistry.GetOffices. The client reads
// row.officeID, never itemID, and hands row.typeID to CelestialMenu as the
// station's type, so typeID is the station's stationTypeID, not the office
// item's 27. See docs/corp-notes.md#offices.
var corpOfficeCols = []string{"corporationID", "stationID", "typeID", "officeID", "officeFolderID"}

// GetCorpOffices returns the offices a corporation rents.
func GetCorpOffices(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db, corpOfficeCols,
		`SELECT o.corporationID, o.stationID, s.stationTypeID, o.itemID, o.officeFolderID
		 FROM crpoffices o JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?`, corpID)
}

// CountCorpOffices returns how many offices a corporation rents. Errors,
// including a missing table, count as zero.
func CountCorpOffices(ctx context.Context, db *sql.DB, corpID int64) int64 {
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM crpoffices WHERE corporationID = ?`, corpID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// MakeOfficesSparseRowset builds the util.SparseRowset GetOffices returns.
// SparseRowset.__setstate__ takes (header, boundObject, rowCount) and calls
// Fetch(startPos, fetchSize) on the bound object for rows it needs; Fetch(0, 0)
// on an empty rowset stays client-side. A plain util.Rowset crashes
// CorpQueryMembersForm (corp_ui_member_find.py), killing CorporationUI.Initialize.
func MakeOfficesSparseRowset(bindSpec string, rowCount int64) marshal.PyRep {
	header := make([]marshal.PyRep, len(corpOfficeCols))
	for i, c := range corpOfficeCols {
		header[i] = marshal.PyString{Value: c}
	}
	return marshal.PyObject{
		Type: "util.SparseRowset",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: header},
			sparseRowsetBoundObject(bindSpec, rowCount),
			marshal.PyInt{Value: uint64(rowCount)},
		}},
	}
}

// sparseRowsetBoundObject encodes the bound-object reference inside a
// util.SparseRowset. ParseObjectID accepts a 2-tuple (objectID, refID) or a
// 3-tuple (objectID, publicAttributes, refID); only the 3-tuple fills
// __publicattributes__, which carries realRowCount for SparseRowset.__len__.
// With a 2-tuple every len() raises AttributeError and the corp window draws
// only its frame.
func sparseRowsetBoundObject(bindSpec string, rowCount int64) marshal.PyRep {
	pa := marshal.PyDict{Items: []marshal.DictEntry{{
		Key:   marshal.PyString{Value: "realRowCount"},
		Value: marshal.PyInt{Value: uint64(rowCount)},
	}}}
	return marshal.PySubStruct{Value: &marshal.PySubStream{
		Decoded: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyString{Value: bindSpec},
			pa,
			marshal.PyInt{Value: 0}, // refID, used by the client for refcounting
		}},
	}}
}

// FetchCorpOffices serves the SparseRowset's bound Fetch(startPos, fetchSize):
// a list of (primaryKey, line) pairs, the key being the office itemID.
func FetchCorpOffices(ctx context.Context, db *sql.DB, corpID, startPos, fetchSize int64) marshal.PyRep {
	rows, err := db.QueryContext(ctx,
		`SELECT o.corporationID, o.stationID, s.stationTypeID, o.itemID, o.officeFolderID
		 FROM crpoffices o JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?
		 ORDER BY o.itemID LIMIT ? OFFSET ?`, corpID, fetchSize, startPos)
	if err != nil {
		return marshal.PyList{}
	}
	defer rows.Close()

	var out []marshal.PyRep
	for rows.Next() {
		var corporationID, stationID, stationTypeID, officeID, officeFolderID int64
		if err := rows.Scan(&corporationID, &stationID, &stationTypeID, &officeID, &officeFolderID); err != nil {
			return marshal.PyList{}
		}
		line := marshal.PyList{Items: []marshal.PyRep{
			intRep(corporationID), intRep(stationID), intRep(stationTypeID),
			intRep(officeID), intRep(officeFolderID),
		}}
		out = append(out, marshal.PyTuple{Items: []marshal.PyRep{intRep(officeID), line}})
	}
	return marshal.PyList{Items: out}
}

// ---- members (sparse rowset) ----

// corpMemberCols is the member-row shape the corp window reads and writes
// (corp_ui_member_listentry, corp_dlg_edit_member, UpdateMember[s]). Names match
// UpdateMember's argument names: change dicts and property strings key on them.
var corpMemberCols = []string{
	"characterID", "title", "divisionID", "squadronID",
	"roles", "grantableRoles",
	"rolesAtHQ", "grantableRolesAtHQ",
	"rolesAtBase", "grantableRolesAtBase",
	"rolesAtOther", "grantableRolesAtOther",
	"baseID", "titleMask", "corporationID",
	// Last, so every older column keeps its position. The edit-member dialog prints it.
	"startDateTime",
}

// CountCorpMembers returns the corporation's member count.
func CountCorpMembers(ctx context.Context, db *sql.DB, corpID int64) int64 {
	var n int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM character_ WHERE corporationID = ?`, corpID).Scan(&n); err != nil {
		return 0
	}
	return n
}

// MakeMembersSparseRowset builds the util.SparseRowset for GetMembers. The Role
// Management panel pages it with Fetch/FetchByKey/GetByKey, so a plain rowset
// raises AttributeError on the Members tab.
func MakeMembersSparseRowset(bindSpec string, rowCount int64) marshal.PyRep {
	header := make([]marshal.PyRep, len(corpMemberCols))
	for i, c := range corpMemberCols {
		header[i] = marshal.PyString{Value: c}
	}
	return marshal.PyObject{
		Type: "util.SparseRowset",
		Args: marshal.PyTuple{Items: []marshal.PyRep{
			marshal.PyList{Items: header},
			sparseRowsetBoundObject(bindSpec, rowCount),
			marshal.PyInt{Value: uint64(rowCount)},
		}},
	}
}

// scanMemberLine reads one member row into a PyList line; the query column
// order must match corpMemberCols.
func scanMemberLine(rows *sql.Rows) (int64, marshal.PyRep, error) {
	var (
		characterID, corporationID     int64
		title                          string
		divisionID, squadronID, baseID sql.NullInt64
		roles, grHQ, grBase, grOther   uint64
		grantable, atHQ, atBase        uint64
		atOther, titleMask             uint64
		startDateTime                  int64
	)
	if err := rows.Scan(&characterID, &title, &divisionID, &squadronID,
		&roles, &grantable, &atHQ, &grHQ, &atBase, &grBase, &atOther, &grOther,
		&baseID, &titleMask, &corporationID, &startDateTime); err != nil {
		return 0, nil, err
	}
	nullable := func(v sql.NullInt64) marshal.PyRep {
		if !v.Valid {
			return marshal.PyNone{}
		}
		return intRep(v.Int64)
	}
	return characterID, marshal.PyList{Items: []marshal.PyRep{
		intRep(characterID), strRep(title), nullable(divisionID), nullable(squadronID),
		intRep(int64(roles)), intRep(int64(grantable)),
		intRep(int64(atHQ)), intRep(int64(grHQ)),
		intRep(int64(atBase)), intRep(int64(grBase)),
		intRep(int64(atOther)), intRep(int64(grOther)),
		nullable(baseID), intRep(int64(titleMask)), intRep(corporationID),
		intRep(startDateTime),
	}}, nil
}

const corpMemberQuery = `
	SELECT c.characterID,
	       COALESCE(c.title, ''),
	       r.divisionID,
	       r.squadronID,
	       COALESCE(r.corprole,              0),
	       COALESCE(r.grantableRoles,        0),
	       COALESCE(r.rolesAtHQ,             0),
	       COALESCE(r.grantableRolesAtHQ,    0),
	       COALESCE(r.rolesAtBase,           0),
	       COALESCE(r.grantableRolesAtBase,  0),
	       COALESCE(r.rolesAtOther,          0),
	       COALESCE(r.grantableRolesAtOther, 0),
	       r.baseID,
	       COALESCE(r.titleMask, 0),
	       c.corporationID,
	       COALESCE(c.startDateTime, 0)
	FROM character_ c
	LEFT JOIN chrCorporationRoles r ON r.characterID = c.characterID
	WHERE c.corporationID = ?`

// FetchCorpMembers serves Fetch(startPos, fetchSize): (characterID, line)
// pairs in characterID order.
func FetchCorpMembers(ctx context.Context, db *sql.DB, corpID, startPos, fetchSize int64) marshal.PyRep {
	rows, err := db.QueryContext(ctx,
		corpMemberQuery+` ORDER BY c.characterID LIMIT ? OFFSET ?`,
		corpID, fetchSize, startPos)
	if err != nil {
		return marshal.PyList{}
	}
	defer rows.Close()

	var out []marshal.PyRep
	for rows.Next() {
		charID, line, err := scanMemberLine(rows)
		if err != nil {
			return marshal.PyList{}
		}
		out = append(out, marshal.PyTuple{Items: []marshal.PyRep{intRep(charID), line}})
	}
	return marshal.PyList{Items: out}
}

// FetchCorpMembersByKey serves FetchByKey(primaryKeys): (characterID, rowIndex,
// line) triples, rowIndex being the position in the characterID-ordered rowset.
func FetchCorpMembersByKey(ctx context.Context, db *sql.DB, corpID int64, keys []int64) marshal.PyRep {
	want := make(map[int64]bool, len(keys))
	for _, k := range keys {
		want[k] = true
	}
	rows, err := db.QueryContext(ctx, corpMemberQuery+` ORDER BY c.characterID`, corpID)
	if err != nil {
		return marshal.PyList{}
	}
	defer rows.Close()

	var out []marshal.PyRep
	idx := int64(-1)
	for rows.Next() {
		idx++
		charID, line, err := scanMemberLine(rows)
		if err != nil {
			return marshal.PyList{}
		}
		if !want[charID] {
			continue
		}
		out = append(out, marshal.PyTuple{Items: []marshal.PyRep{
			intRep(charID), intRep(idx), line,
		}})
	}
	return marshal.PyList{Items: out}
}

// GetCorpStations returns the distinct stations a corporation has offices in.
func GetCorpStations(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db, []string{"stationID", "typeID"},
		`SELECT DISTINCT o.stationID, COALESCE(s.stationTypeID, 0)
		 FROM crpoffices o
		 LEFT JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?`, corpID)
}

// ---- shares ----

// GetSharesByShareholder returns the corporations a character holds shares in.
func GetSharesByShareholder(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db, []string{"corporationID", "shares"},
		`SELECT corporationID, shares FROM crpcharshares WHERE characterID = ?`, charID)
}

// GetSharesForCorp returns every shareholder of a corporation.
func GetSharesForCorp(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	return queryRows(ctx, db, []string{"characterID", "shares"},
		`SELECT characterID, shares FROM crpcharshares WHERE corporationID = ?`, corpID)
}

// ---- empty catalogues ----
// Office rental details have no persistence. The vote system getters live in
// corpwritedb.go beside their writers.

func GetCorpRentalDetails(ctx context.Context, db *sql.DB) marshal.PyRep {
	return makeRowset([]string{"ownerID", "typeID", "quantity", "issued", "periodCost"}, nil)
}
