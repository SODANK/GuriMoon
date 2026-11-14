package db

// Queries behind the in-game browser's Corporate Portal (esp/corpportal.go): the directory of
// capsuleer corporations, and a corporation's offices. Everything here is public (anyone can
// forge an identity header), so nothing returns a wallet, a role or a member list.

import (
	"context"
	"database/sql"
	"fmt"
)

// CorpDirectoryRow is one capsuleer corporation in the portal's directory.
type CorpDirectoryRow struct {
	CorpID       int64
	Name         string
	Ticker       string
	CEOID        int64
	CEOName      string
	AllianceName string
	Capsuleers   int64 // player characters (accountID > 0), never memberCount
	MemberLimit  int64
	TaxRate      float64 // a fraction, as stored
	Accepting    bool    // CorpHasOpening's rule: a limit of 0 is no limit
}

// CorpDirectory lists capsuleer (non-NPC, not deleted) corporations, the
// largest first. Accepting is computed with CorpHasOpening's rule in the same
// query, so a directory of fifty corporations is one round trip, not fifty.
func CorpDirectory(ctx context.Context, db *sql.DB, limit int) ([]CorpDirectoryRow, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.corporationID, c.corporationName, c.tickerName, c.ceoID,
		       COALESCE(ceo.characterName, ''), COALESCE(a.allianceName, ''),
		       COALESCE(m.n, 0), c.memberLimit, c.taxRate
		  FROM corporation c
		  LEFT JOIN alliances a ON a.allianceID = c.allianceID
		  LEFT JOIN character_ ceo ON ceo.characterID = c.ceoID
		  LEFT JOIN (SELECT corporationID, COUNT(*) AS n FROM character_
		              WHERE accountID > 0 GROUP BY corporationID) m ON m.corporationID = c.corporationID
		 WHERE c.deleted = 0
		   AND c.corporationID NOT IN (SELECT corporationID FROM crpNPCCorporations)
		   AND c.corporationID NOT BETWEEN ? AND ?
		 ORDER BY COALESCE(m.n, 0) DESC, c.corporationName
		 LIMIT ?`, npcCorpMinID, npcCorpMaxID, limit)
	if err != nil {
		return nil, fmt.Errorf("CorpDirectory: %w", err)
	}
	defer rows.Close()
	var out []CorpDirectoryRow
	for rows.Next() {
		var r CorpDirectoryRow
		var ticker sql.NullString
		if err := rows.Scan(&r.CorpID, &r.Name, &ticker, &r.CEOID, &r.CEOName, &r.AllianceName,
			&r.Capsuleers, &r.MemberLimit, &r.TaxRate); err != nil {
			return nil, fmt.Errorf("CorpDirectory scan: %w", err)
		}
		r.Ticker = ticker.String
		r.Accepting = r.MemberLimit <= 0 || r.Capsuleers < r.MemberLimit
		out = append(out, r)
	}
	return out, rows.Err()
}

// CorpOffice is one station where a corporation rents an office.
type CorpOffice struct {
	StationID   int64
	StationName string
}

// CorpOfficeStations lists the stations a corporation keeps offices at.
func CorpOfficeStations(ctx context.Context, db *sql.DB, corpID int64) ([]CorpOffice, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT o.stationID, COALESCE(s.stationName, '')
		  FROM crpoffices o
		  LEFT JOIN staStations s ON s.stationID = o.stationID
		 WHERE o.corporationID = ?
		 ORDER BY s.stationName`, corpID)
	if err != nil {
		return nil, fmt.Errorf("CorpOfficeStations: %w", err)
	}
	defer rows.Close()
	var out []CorpOffice
	for rows.Next() {
		var o CorpOffice
		if err := rows.Scan(&o.StationID, &o.StationName); err != nil {
			return nil, fmt.Errorf("CorpOfficeStations scan: %w", err)
		}
		out = append(out, o)
	}
	return out, rows.Err()
}
