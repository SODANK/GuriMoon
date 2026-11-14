package db

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gurimoon/marshal"
)

// LookupCharacters searches for characters whose name starts with (exact = 0) or equals
// (exact = 1) searchStr. It covers player characters (character_) and NPC agents
// (eveStaticOwners, typeID 1375) so custom agents appear in people search.
func LookupCharacters(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"characterID", "characterName", "typeID"}
	pattern := likePattern(searchStr, exact)
	rows, err := db.QueryContext(ctx, `
		SELECT characterID, characterName, typeID
		FROM character_
		WHERE characterName LIKE ?
		UNION
		SELECT o.ownerID AS characterID, o.ownerName AS characterName, o.typeID
		FROM eveStaticOwners o
		INNER JOIN agtAgents a ON a.agentID = o.ownerID
		WHERE o.ownerName LIKE ?
		ORDER BY characterName
		LIMIT 100`, pattern, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var charID, typeID int64
		var charName string
		if err := rows.Scan(&charID, &charName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(charID), strRep(charName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupCorporations searches all corporations (NPC and player) by name.
func LookupCorporations(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"corporationID", "corporationName", "typeID"}
	pattern := likePattern(searchStr, exact)

	// Search player corporations.
	query := `
		SELECT corporationID, corporationName, 2 AS typeID
		FROM corporation
		WHERE corporationName LIKE ?
		UNION
		SELECT ownerID AS corporationID, ownerName AS corporationName, typeID
		FROM eveStaticOwners
		WHERE typeID = 2 AND ownerName LIKE ?
		ORDER BY corporationName
		LIMIT 100`
	rows, err := db.QueryContext(ctx, query, pattern, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var corpID, typeID int64
		var corpName string
		if err := rows.Scan(&corpID, &corpName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(corpID), strRep(corpName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupPlayerCorporations searches only player-created corporations by name.
func LookupPlayerCorporations(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"corporationID", "corporationName", "typeID"}
	pattern := likePattern(searchStr, exact)
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, corporationName, 2 AS typeID
		FROM corporation
		WHERE corporationType = 2 AND corporationName LIKE ?
		ORDER BY corporationName
		LIMIT 100`, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var corpID, typeID int64
		var corpName string
		if err := rows.Scan(&corpID, &corpName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(corpID), strRep(corpName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupCorporationTickers searches corporations by ticker symbol.
func LookupCorporationTickers(ctx context.Context, db *sql.DB, ticker string, exact int64) (marshal.PyRep, error) {
	cols := []string{"corporationID", "corporationName", "tickerName", "typeID"}
	pattern := likePattern(ticker, exact)
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, corporationName, tickerName, 2 AS typeID
		FROM corporation
		WHERE tickerName LIKE ?
		ORDER BY tickerName
		LIMIT 100`, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var corpID, typeID int64
		var corpName, tickerName string
		if err := rows.Scan(&corpID, &corpName, &tickerName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(corpID), strRep(corpName), strRep(tickerName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupStations searches stations by name.
func LookupStations(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"stationID", "stationName", "typeID"}
	pattern := likePattern(searchStr, exact)
	rows, err := db.QueryContext(ctx, `
		SELECT stationID, stationName, stationTypeID AS typeID
		FROM staStations
		WHERE stationName LIKE ?
		ORDER BY stationName
		LIMIT 100`, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var stationID, typeID int64
		var stationName string
		if err := rows.Scan(&stationID, &stationName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(stationID), strRep(stationName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupFactions searches factions (chrFactions or eveStaticOwners with faction
// typeIDs) by name.
func LookupFactions(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"factionID", "factionName", "typeID"}
	pattern := likePattern(searchStr, exact)
	rows, err := db.QueryContext(ctx, `
		SELECT ownerID AS factionID, ownerName AS factionName, typeID
		FROM eveStaticOwners
		WHERE typeID IN (30, 500) AND ownerName LIKE ?
		ORDER BY ownerName
		LIMIT 100`, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var factionID, typeID int64
		var factionName string
		if err := rows.Scan(&factionID, &factionName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(factionID), strRep(factionName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupOwners searches across characters, corporations and alliances by name.
func LookupOwners(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"ownerID", "ownerName", "typeID"}
	pattern := likePattern(searchStr, exact)
	query := `
		SELECT characterID AS ownerID, characterName AS ownerName, typeID
		FROM character_
		WHERE characterName LIKE ?
		UNION
		SELECT corporationID AS ownerID, corporationName AS ownerName, 2 AS typeID
		FROM corporation
		WHERE corporationName LIKE ?
		UNION
		SELECT ownerID, ownerName, typeID
		FROM eveStaticOwners
		WHERE ownerName LIKE ?
		ORDER BY ownerName
		LIMIT 100`
	rows, err := db.QueryContext(ctx, query, pattern, pattern, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var ownerID, typeID int64
		var ownerName string
		if err := rows.Scan(&ownerID, &ownerName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(ownerID), strRep(ownerName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupPCOwners searches only player characters and player corporations, for chat
// channel owner lookup (lscengine.py).
func LookupPCOwners(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	cols := []string{"ownerID", "ownerName", "typeID"}
	pattern := likePattern(searchStr, exact)
	query := `
		SELECT characterID AS ownerID, characterName AS ownerName, typeID
		FROM character_
		WHERE characterName LIKE ?
		UNION
		SELECT corporationID AS ownerID, corporationName AS ownerName, 2 AS typeID
		FROM corporation
		WHERE corporationType = 2 AND corporationName LIKE ?
		ORDER BY ownerName
		LIMIT 100`
	rows, err := db.QueryContext(ctx, query, pattern, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var ownerID, typeID int64
		var ownerName string
		if err := rows.Scan(&ownerID, &ownerName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(ownerID), strRep(ownerName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupCorporationsOrAlliances searches corporations and alliances by name.
// warableOnly limits it to player corporations and alliances.
func LookupCorporationsOrAlliances(ctx context.Context, db *sql.DB, searchStr string, exact int64, warableOnly bool) (marshal.PyRep, error) {
	cols := []string{"ownerID", "ownerName", "typeID"}
	pattern := likePattern(searchStr, exact)

	var parts []string
	var args []interface{}

	if warableOnly {
		parts = append(parts, `
			SELECT corporationID AS ownerID, corporationName AS ownerName, 2 AS typeID
			FROM corporation
			WHERE corporationType = 2 AND corporationName LIKE ?`)
	} else {
		parts = append(parts, `
			SELECT corporationID AS ownerID, corporationName AS ownerName, 2 AS typeID
			FROM corporation
			WHERE corporationName LIKE ?`)
	}
	args = append(args, pattern)

	if warableOnly {
		// The corporation half filters to warable player corporations; the alliance half must
		// too, or a deleted or system alliance is offered as a war target.
		parts = append(parts, fmt.Sprintf(`
		SELECT allianceID AS ownerID, allianceName AS ownerName, 16159 AS typeID
		FROM alliances
		WHERE deleted = 0 AND allianceID >= %d AND allianceName LIKE ?`, MinPlayerEntityID))
	} else {
		parts = append(parts, `
		SELECT allianceID AS ownerID, allianceName AS ownerName, 16159 AS typeID
		FROM alliances
		WHERE allianceName LIKE ?`)
	}
	args = append(args, pattern)

	query := strings.Join(parts, " UNION ") + " ORDER BY ownerName LIMIT 100"
	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var ownerID, typeID int64
		var ownerName string
		if err := rows.Scan(&ownerID, &ownerName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(ownerID), strRep(ownerName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// LookupNoneNPCAccountOwners searches player characters and player corporations, for
// the wallet owner selection dialog.
func LookupNoneNPCAccountOwners(ctx context.Context, db *sql.DB, searchStr string, exact int64) (marshal.PyRep, error) {
	// Same as LookupPCOwners: player entities only.
	return LookupPCOwners(ctx, db, searchStr, exact)
}

// LookupLocationsByGroup searches celestial objects in one inventory group by name
// (asteroid belts, stargates, ...).
func LookupLocationsByGroup(ctx context.Context, db *sql.DB, groupID int64, searchStr string) (marshal.PyRep, error) {
	cols := []string{"itemID", "itemName", "typeID"}
	pattern := likePattern(searchStr, 0)
	rows, err := db.QueryContext(ctx, `
		SELECT itemID, itemName, typeID
		FROM mapDenormalize
		WHERE groupID = ? AND itemName LIKE ?
		ORDER BY itemName
		LIMIT 100`, groupID, pattern)
	if err != nil {
		return makeRowset(cols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var itemID, typeID int64
		var itemName string
		if err := rows.Scan(&itemID, &itemName, &typeID); err != nil {
			continue
		}
		result = append(result, []marshal.PyRep{intRep(itemID), strRep(itemName), intRep(typeID)})
	}
	return makeRowset(cols, result), nil
}

// likePattern builds a SQL LIKE pattern from a search string: exact = 1 matches
// exactly, anything else is a prefix match.
func likePattern(s string, exact int64) string {
	// Escape SQL LIKE wildcards in the user input.
	s = strings.ReplaceAll(s, "%", "")
	s = strings.ReplaceAll(s, "_", "\\_")
	if exact == 1 {
		return s
	}
	return s + "%"
}
