package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log"

	"gurimoon/corp"
	"gurimoon/marshal"
)

// corporationCols are the column names returned by GetCorporation / DBRowToRow("util.Row").
var corporationCols = []string{
	"corporationID", "corporationName", "description", "tickerName", "url",
	"taxRate", "minimumJoinStanding", "corporationType", "hasPlayerPersonnelManager",
	"sendCharTerminationMessage", "creatorID", "ceoID", "stationID", "raceID",
	"allianceID", "shares", "memberCount", "memberLimit", "allowedMemberRaceIDs",
	"graphicID", "shape1", "shape2", "shape3", "color1", "color2", "color3", "typeface",
	"division1", "division2", "division3", "division4", "division5", "division6", "division7",
	"deleted",
}

// GetCorporation returns the util.Row for a corporation, columns in corporationCols order.
func GetCorporation(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	var (
		corporationID              int64
		corporationName            string
		description                sql.NullString
		tickerName                 string
		url                        sql.NullString
		taxRate                    float64
		minimumJoinStanding        float64
		corporationType            int64
		hasPlayerPersonnelManager  int64
		sendCharTerminationMessage int64
		creatorID                  int64
		ceoID                      int64
		stationID                  int64
		raceID                     sql.NullInt64
		allianceID                 sql.NullInt64
		shares                     int64
		memberCount                int64
		memberLimit                int64
		allowedMemberRaceIDs       sql.NullInt64
		graphicID                  sql.NullInt64
		// shape*/color* go out as PyInt: uix.GetCorpLogo draws a shape only when
		// shape == int(shape), which is False for a string.
		shape1    sql.NullInt64
		shape2    sql.NullInt64
		shape3    sql.NullInt64
		color1    sql.NullInt64
		color2    sql.NullInt64
		color3    sql.NullInt64
		typeface  sql.NullString
		division1 sql.NullString
		division2 sql.NullString
		division3 sql.NullString
		division4 sql.NullString
		division5 sql.NullString
		division6 sql.NullString
		division7 sql.NullString
		deleted   int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT corporationID, corporationName, description, tickerName, url,
		       taxRate, minimumJoinStanding, corporationType, hasPlayerPersonnelManager,
		       sendCharTerminationMessage, creatorID, ceoID, stationID, raceID,
		       allianceID, shares, memberCount, memberLimit, allowedMemberRaceIDs,
		       graphicID, shape1, shape2, shape3, color1, color2, color3, typeface,
		       division1, division2, division3, division4, division5, division6, division7,
		       deleted
		FROM corporation
		WHERE corporationID = ?`, corpID).Scan(
		&corporationID, &corporationName, &description, &tickerName, &url,
		&taxRate, &minimumJoinStanding, &corporationType, &hasPlayerPersonnelManager,
		&sendCharTerminationMessage, &creatorID, &ceoID, &stationID, &raceID,
		&allianceID, &shares, &memberCount, &memberLimit, &allowedMemberRaceIDs,
		&graphicID, &shape1, &shape2, &shape3, &color1, &color2, &color3, &typeface,
		&division1, &division2, &division3, &division4, &division5, &division6, &division7,
		&deleted,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("GetCorporation: corp %d not found", corpID)
	}
	if err != nil {
		return nil, fmt.Errorf("GetCorporation: %w", err)
	}
	// stationID 0 (an NPC corp with no station) falls back to the home solar system:
	// infosvc renders the HQ line with cfg.evelocations.Get, which raises on an unresolvable ID.
	if stationID == 0 {
		var homeSystem sql.NullInt64
		if err := db.QueryRowContext(ctx,
			`SELECT solarSystemID FROM crpNPCCorporations WHERE corporationID = ?`, corpID,
		).Scan(&homeSystem); err == nil && homeSystem.Valid {
			stationID = homeSystem.Int64
		}
	}
	vals := []marshal.PyRep{
		intRep(corporationID), strRep(corporationName), nullStrRep(description),
		strRep(tickerName), nullStrRep(url),
		floatRep(taxRate), floatRep(minimumJoinStanding), intRep(corporationType),
		intRep(hasPlayerPersonnelManager), intRep(sendCharTerminationMessage),
		intRep(creatorID), intRep(ceoID), intRep(stationID), nullIntRep(raceID),
		nullIntRep(allianceID), intRep(shares), intRep(memberCount), intRep(memberLimit),
		nullIntRep(allowedMemberRaceIDs), nullIntRep(graphicID),
		nullIntRep(shape1), nullIntRep(shape2), nullIntRep(shape3),
		nullIntRep(color1), nullIntRep(color2), nullIntRep(color3), nullStrRep(typeface),
		nullStrRep(division1), nullStrRep(division2), nullStrRep(division3), nullStrRep(division4),
		nullStrRep(division5), nullStrRep(division6), nullStrRep(division7),
		intRep(deleted),
	}
	return makeRow(corporationCols, vals), nil
}

// GetCorporationIDForCharacter returns the corporationID for a character.
// Checks character_ first, then falls back to agtAgents for NPC agents.
// Mirrors CorpMgrService::Handle_GetCorporationIDForCharacter.
func GetCorporationIDForCharacter(ctx context.Context, db *sql.DB, charID int64) (int64, error) {
	var corpID int64
	err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM character_ WHERE characterID = ?`, charID,
	).Scan(&corpID)
	if err == nil {
		return corpID, nil
	}
	// Fallback: NPC agent.
	err = db.QueryRowContext(ctx,
		`SELECT corporationID FROM agtAgents WHERE agentID = ?`, charID,
	).Scan(&corpID)
	if err == nil {
		return corpID, nil
	}
	return 0, fmt.Errorf("GetCorporationIDForCharacter: char %d not found", charID)
}

// corpMarketInfoCols are the fields infosvc.py:GetWndData reads off each row of
// the corporationSvc.GetCorpInfo result when building the "Market Activity" tab
// (Supply / Demand headers) of an NPC corporation's info window.
var corpMarketInfoCols = []string{
	"typeID",
	"sellPrice", "sellQuantity", "sellDateTime", "sellStationID",
	"buyPrice", "buyQuantity", "buyDateTime", "buyStationID",
}

// GetCorpMarketInfo returns the util.Rowset behind an NPC corporation's Market Activity
// tab: each crpNPCCorporationTrades type with the best live sell (lowest) and buy (highest)
// order at the corp's own stations. A type with no live order on its side is dropped;
// the client renders a row only when its price is not None.
func GetCorpMarketInfo(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT t.typeID, t.supplyDemand,
		       s.price, s.volRemaining, s.issued, s.stationID,
		       b.price, b.volRemaining, b.issued, b.stationID
		  FROM crpNPCCorporationTrades t
		  LEFT JOIN (
		        SELECT typeID, price, volRemaining, issued, stationID
		          FROM (
		            SELECT o.typeID, o.price, o.volRemaining, o.issued, o.stationID,
		                   ROW_NUMBER() OVER (PARTITION BY o.typeID ORDER BY o.price ASC) AS rn
		              FROM market_orders o
		              JOIN staStations st ON st.stationID = o.stationID
		             WHERE st.corporationID = ?
		               AND o.bid = 0 AND o.orderState = 1 AND o.volRemaining > 0
		          ) ranked WHERE rn = 1
		  ) s ON s.typeID = t.typeID
		  LEFT JOIN (
		        SELECT typeID, price, volRemaining, issued, stationID
		          FROM (
		            SELECT o.typeID, o.price, o.volRemaining, o.issued, o.stationID,
		                   ROW_NUMBER() OVER (PARTITION BY o.typeID ORDER BY o.price DESC) AS rn
		              FROM market_orders o
		              JOIN staStations st ON st.stationID = o.stationID
		             WHERE st.corporationID = ?
		               AND o.bid = 1 AND o.orderState = 1 AND o.volRemaining > 0
		          ) ranked WHERE rn = 1
		  ) b ON b.typeID = t.typeID
		 WHERE t.corporationID = ?
		   AND (s.price IS NOT NULL OR b.price IS NOT NULL)`,
		corpID, corpID, corpID)
	if err != nil {
		// Missing table (e.g. a slimmed schema) must not break the info window.
		log.Printf("GetCorpMarketInfo corp %d: %v", corpID, err)
		return makeRowset(corpMarketInfoCols, nil), nil
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var typeID int64
		var supplyDemand sql.NullFloat64
		var sellPrice, buyPrice sql.NullFloat64
		var sellQty, sellIssued, sellStationID sql.NullInt64
		var buyQty, buyIssued, buyStationID sql.NullInt64
		if err := rows.Scan(&typeID, &supplyDemand,
			&sellPrice, &sellQty, &sellIssued, &sellStationID,
			&buyPrice, &buyQty, &buyIssued, &buyStationID); err != nil {
			return nil, fmt.Errorf("GetCorpMarketInfo scan: %w", err)
		}
		// supplyDemand's sign picks the side: positive shows under Supply only, negative under
		// Demand only, zero or NULL leaves both.
		if supplyDemand.Valid && supplyDemand.Float64 > 0 {
			buyPrice, buyQty, buyIssued, buyStationID = sql.NullFloat64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}
		} else if supplyDemand.Valid && supplyDemand.Float64 < 0 {
			sellPrice, sellQty, sellIssued, sellStationID = sql.NullFloat64{}, sql.NullInt64{}, sql.NullInt64{}, sql.NullInt64{}
		}
		if !sellPrice.Valid && !buyPrice.Valid {
			continue
		}
		result = append(result, []marshal.PyRep{
			intRep(typeID),
			nullFloatRep(sellPrice), nullIntRep(sellQty), nullIntRep(sellIssued), nullIntRep(sellStationID),
			nullFloatRep(buyPrice), nullIntRep(buyQty), nullIntRep(buyIssued), nullIntRep(buyStationID),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetCorpMarketInfo rows: %w", err)
	}
	return makeRowset(corpMarketInfoCols, result), nil
}

// GetEmploymentRecord returns a util.Rowset(startDate, corporationID, deleted) for
// charID, newest first.
func GetEmploymentRecord(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	cols := []string{"startDate", "corporationID", "deleted"}
	rows, err := db.QueryContext(ctx, `
		SELECT startDate, corporationID, deleted
		FROM chrEmployment
		WHERE characterID = ?
		ORDER BY startDate DESC`, charID)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("GetEmploymentRecord: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		var startDate int64
		var corporationID int64
		var deleted int64
		if scanErr := rows.Scan(&startDate, &corporationID, &deleted); scanErr != nil {
			return nil, fmt.Errorf("GetEmploymentRecord scan: %w", scanErr)
		}
		result = append(result, []marshal.PyRep{intRep(startDate), intRep(corporationID), intRep(deleted)})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetEmploymentRecord rows: %w", err)
	}
	return makeRowset(cols, result), nil
}

// MinPlayerEntityID is the first ID cfg.py treats as player-owned: IsCorporation and
// IsAlliance return 0 below 100000000, so a corporation or alliance numbered under it
// reads as static NPC data to the client.
const MinPlayerEntityID = 100000000

// nextPlayerEntityID returns the next free player-owned entity ID. Corporations and
// alliances share one owner ID space (eveNames, eveStaticOwners), so the counter clears
// the highest ID either kind holds and never drops below MinPlayerEntityID.
func nextPlayerEntityID(ctx context.Context, db *sql.DB) (int64, error) {
	next := int64(MinPlayerEntityID)
	maxOf := func(query string) error {
		var v sql.NullInt64
		if err := db.QueryRowContext(ctx, query).Scan(&v); err != nil {
			return err
		}
		if v.Valid && v.Int64 >= next {
			next = v.Int64
		}
		return nil
	}
	if err := maxOf(`SELECT COALESCE(MAX(corporationID), 0) FROM corporation`); err != nil {
		return 0, fmt.Errorf("nextPlayerEntityID corporation: %w", err)
	}
	// The alliance tables are created on demand; ignore them when absent.
	for _, q := range []struct{ table, query string }{
		{"alliances", `SELECT COALESCE(MAX(allianceID), 0) FROM alliances`},
		{"alliance_shortnames", `SELECT COALESCE(MAX(allianceID), 0) FROM alliance_shortnames`},
	} {
		ok, err := tableExists(ctx, db, q.table)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		if err := maxOf(q.query); err != nil {
			return 0, fmt.Errorf("nextPlayerEntityID %s: %w", q.table, err)
		}
	}
	return next + 1, nil
}

// ensurePlayerCorpIDFloor raises the corporation table's AUTO_INCREMENT to
// nextPlayerEntityID. MySQL ignores a value below the current maximum, so this only
// moves the counter up and never touches existing rows.
func ensurePlayerCorpIDFloor(ctx context.Context, db *sql.DB) error {
	next, err := nextPlayerEntityID(ctx, db)
	if err != nil {
		return err
	}
	var cur sql.NullInt64
	if err := db.QueryRowContext(ctx, `
		SELECT AUTO_INCREMENT FROM information_schema.TABLES
		 WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = 'corporation'`).Scan(&cur); err != nil {
		return fmt.Errorf("ensurePlayerCorpIDFloor read: %w", err)
	}
	if cur.Valid && cur.Int64 >= next {
		return nil
	}
	if _, err := db.ExecContext(ctx,
		fmt.Sprintf(`ALTER TABLE corporation AUTO_INCREMENT = %d`, next),
	); err != nil {
		return fmt.Errorf("ensurePlayerCorpIDFloor bump: %w", err)
	}
	log.Printf("corporation AUTO_INCREMENT raised to %d (player-entity ID range)", next)
	return nil
}

// AddCorporation creates a player corporation: the corporation row, its share
// treasury, the eveStaticOwners entry and the corp chat channel. Returns the new
// corporationID. See docs/db-notes.md#corporation-creation.
func AddCorporation(
	ctx context.Context, db *sql.DB,
	charID int64,
	corpName, tickerName, description, url string,
	taxRate float64,
	shape1, shape2, shape3 sql.NullInt64,
	color1, color2, color3 sql.NullInt64,
) (int64, error) {
	if err := ensurePlayerCorpIDFloor(ctx, db); err != nil {
		return 0, err
	}

	// hasPlayerPersonnelManager = 1 marks a player corporation: it keeps the corp out of the
	// static tickernames bulk cache, so ticker and logo are served live by GetMultiCorpTickerNamesEx.
	res, err := db.ExecContext(ctx, `
		INSERT INTO corporation
		  (corporationName, description, tickerName, url,
		   taxRate, minimumJoinStanding, corporationType,
		   hasPlayerPersonnelManager, sendCharTerminationMessage,
		   creatorID, ceoID, stationID, raceID, allianceID,
		   shares, memberCount, memberLimit, allowedMemberRaceIDs,
		   graphicID, color1, color2, color3, shape1, shape2, shape3,
		   typeface,
		   division1, division2, division3, division4,
		   division5, division6, division7)
		SELECT ?, ?, ?, ?,
		       ?, 0, 2,
		       1, 1,
		       ?, ?, c.stationID, b.raceID, 0,
		       ?, 0, 10, b.raceID,
		       0, ?, ?, ?, ?, ?, ?,
		       NULL,
		       NULL, NULL, NULL, NULL, NULL, NULL, NULL
		FROM character_ c
		LEFT JOIN chrBloodlines b ON c.bloodlineID = b.bloodlineID
		WHERE c.characterID = ?`,
		corpName, description, tickerName, url,
		taxRate,
		charID, charID,
		CorpStartingShares,
		color1, color2, color3, shape1, shape2, shape3,
		charID,
	)
	if err != nil {
		return 0, fmt.Errorf("AddCorporation INSERT: %w", err)
	}
	corpID, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("AddCorporation LastInsertId: %w", err)
	}

	// No homepage given: point it at its Galnet page (corppageurl.go). Not fatal.
	if url == "" {
		if err := StampCorpPageURL(ctx, db, corpID); err != nil {
			log.Printf("AddCorporation: %v", err)
		}
	}

	// The issued shares start in the corporation's own treasury row (holder = corpID); the
	// corp wallet's Shares tab lists what the corp holds. See docs/db-notes.md#corporation-creation.
	if _, err = db.ExecContext(ctx, `
		INSERT INTO crpcharshares (characterID, corporationID, shares) VALUES (?, ?, ?)
		ON DUPLICATE KEY UPDATE shares = VALUES(shares)`,
		corpID, corpID, CorpStartingShares,
	); err != nil {
		return corpID, fmt.Errorf("AddCorporation treasury: %w", err)
	}

	// Register the new corp as a named entity owner (typeID 2 = Corporation).
	if _, err = db.ExecContext(ctx,
		`REPLACE INTO eveStaticOwners (ownerID, ownerName, typeID) VALUES (?, ?, 2)`,
		corpID, corpName,
	); err != nil {
		return corpID, fmt.Errorf("AddCorporation eveStaticOwners: %w", err)
	}

	// Corp chat channel. displayName is 'Corp' because lscengine.GetDisplayName prints it
	// verbatim as the tab title; memberless must be 0 or the client draws no member roster.
	if _, err = db.ExecContext(ctx, `
		INSERT INTO channels
		  (channelID, ownerID, displayName, motd, comparisonKey,
		   memberless, password, mailingList, cspa, temporary, mode,
		   subscribed, estimatedMemberCount)
		VALUES (?, ?, 'Corp', CONCAT(?, ' MOTD'), ?,
		        0, NULL, 0, 127, 0, 1, 1, 0)`,
		corpID, corpID, corpName, tickerName,
	); err != nil {
		return corpID, fmt.Errorf("AddCorporation channel: %w", err)
	}

	return corpID, nil
}

// CorporationStartupCost is const.corporationStartupCost (eveConstants), the fee the
// founder pays. The client's MemberCanCreateCorporation only hides the button, so the
// server is the gate.
const CorporationStartupCost = 1599800.0

// ChargeCorporationCreation debits CorporationStartupCost from the founder's wallet with
// a conditional UPDATE and journals it as refTypeID 39 paid to the SCC, in one transaction.
// ok=false with a nil error means the wallet could not cover it; balance is then the
// current balance, for the refusal.
func ChargeCorporationCreation(ctx context.Context, db *sql.DB, charID int64) (ok bool, balance float64, err error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return false, 0, fmt.Errorf("ChargeCorporationCreation begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	ok, balance, err = TryChargeISK(ctx, tx, charID, CorporationStartupCost)
	if err != nil {
		return false, 0, err
	}
	if !ok {
		balance, err = readBalance(ctx, tx, charID)
		return false, balance, err
	}
	if _, err = tx.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 39, ?, 1000132, NULL, 0, ?, ?, ?, 1000)`,
		charID, win32TimeNow(), charID,
		-CorporationStartupCost, balance, "Corporation registration fee"); err != nil {
		return false, 0, fmt.Errorf("ChargeCorporationCreation journal: %w", err)
	}
	if err = tx.Commit(); err != nil {
		return false, 0, fmt.Errorf("ChargeCorporationCreation commit: %w", err)
	}
	return true, balance, nil
}

// RefundCorporationCreation reverses ChargeCorporationCreation after a create that failed
// downstream of the debit. Returns the balance after the refund.
func RefundCorporationCreation(ctx context.Context, db *sql.DB, charID int64) (float64, error) {
	if err := GiveISK(ctx, db, charID, CorporationStartupCost); err != nil {
		return 0, err
	}
	balance, err := readBalance(ctx, db, charID)
	if err != nil {
		return 0, err
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO market_journal
		  (characterID, transDate, refTypeID, ownerID1, ownerID2, argID1,
		   accountID, amount, balance, reason, accountKey)
		VALUES (?, ?, 39, 1000132, ?, NULL, 0, ?, ?, ?, 1000)`,
		charID, win32TimeNow(), charID,
		CorporationStartupCost, balance, "Corporation registration fee refund"); err != nil {
		return balance, fmt.Errorf("RefundCorporationCreation journal: %w", err)
	}
	return balance, nil
}

// JoinCorporation moves charID from oldCorpID to newCorpID: member counts, the character
// row, chrEmployment and chrCorporationRoles. A joining CEO (corporation.ceoID == charID)
// gets RoleAll, everyone else DefaultNewMemberRoles. Returns the granted role mask for
// the caller's session change notification.
func JoinCorporation(ctx context.Context, db *sql.DB, charID, newCorpID, oldCorpID int64) (uint64, error) {
	now := win32TimeNow()

	roles := corp.DefaultNewMemberRoles
	var ceoID int64
	if err := db.QueryRowContext(ctx,
		`SELECT ceoID FROM corporation WHERE corporationID = ?`, newCorpID,
	).Scan(&ceoID); err == nil && ceoID == charID {
		roles = corp.RoleAll
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE corporation SET memberCount = IF(memberCount > 0, memberCount - 1, 0) WHERE corporationID = ?`,
		oldCorpID,
	); err != nil {
		return 0, fmt.Errorf("JoinCorporation decrement old: %w", err)
	}

	// startDateTime is the date the character joined its current corporation (createDateTime
	// is the birth date); the info window, char select and the member list all read it.
	if _, err := db.ExecContext(ctx,
		`UPDATE character_ SET corporationID = ?, corporationDateTime = ?, startDateTime = ? WHERE characterID = ?`,
		newCorpID, now, now, charID,
	); err != nil {
		return 0, fmt.Errorf("JoinCorporation update char: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`UPDATE corporation SET memberCount = memberCount + 1 WHERE corporationID = ?`,
		newCorpID,
	); err != nil {
		return 0, fmt.Errorf("JoinCorporation increment new: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO chrEmployment (characterID, corporationID, startDate, deleted) VALUES (?, ?, ?, 0)`,
		charID, newCorpID, now,
	); err != nil {
		return 0, fmt.Errorf("JoinCorporation employment: %w", err)
	}

	if _, err := db.ExecContext(ctx, `
		REPLACE INTO chrCorporationRoles
		  (characterID, corprole, rolesAtAll, rolesAtBase, rolesAtHQ, rolesAtOther)
		VALUES (?, ?, ?, ?, ?, ?)`,
		charID, roles, roles, roles, roles, roles,
	); err != nil {
		return 0, fmt.Errorf("JoinCorporation roles: %w", err)
	}

	fireCorpMembershipHook(charID, oldCorpID, newCorpID)
	identityChanged(charID)
	return roles, nil
}

// allianceCols are the columns for GetAllianceInfo / the alliances table.
var allianceCols = []string{
	"allianceID", "allianceName", "shortName", "description", "url",
	"executorCorpID", "creatorCorpID", "creatorCharID",
	"dictatorial", "startDate", "memberCount", "deleted",
}

// ensureAlliancesTable creates the alliances table if it does not already exist.
// ensureAlliancesTableGuard runs ensureAlliancesTableDDL once per process: it is called on request paths, and
// even a no-op CREATE TABLE IF NOT EXISTS / ALTER takes a metadata lock and can
// touch the table definition each time.
var ensureAlliancesTableGuard ensureGuard

func ensureAlliancesTable(ctx context.Context, db *sql.DB) error {
	return ensureAlliancesTableGuard.do(func() error { return ensureAlliancesTableDDL(ctx, db) })
}

func ensureAlliancesTableDDL(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS alliances (
		  allianceID    INT UNSIGNED    NOT NULL,
		  allianceName  VARCHAR(100)    NOT NULL DEFAULT '',
		  shortName     VARCHAR(5)      NOT NULL DEFAULT '',
		  description   TEXT,
		  url           TEXT,
		  executorCorpID INT UNSIGNED   NOT NULL DEFAULT 0,
		  creatorCorpID  INT UNSIGNED   NOT NULL DEFAULT 0,
		  creatorCharID  INT UNSIGNED   NOT NULL DEFAULT 0,
		  dictatorial   TINYINT(1)      NOT NULL DEFAULT 0,
		  startDate     BIGINT(20)      NOT NULL DEFAULT 0,
		  memberCount   INT             NOT NULL DEFAULT 0,
		  deleted       TINYINT(1)      NOT NULL DEFAULT 0,
		  PRIMARY KEY (allianceID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	return err
}

// CreateAlliance creates a player alliance, joins corpID to it as founder and executor,
// and returns the new allianceID. description and url come from the client's create
// dialog; the alliance window's Home tab renders both from the GetAlliance row.
func CreateAlliance(ctx context.Context, db *sql.DB, corpID, charID int64, allianceName, shortName, description, url string) (int64, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return 0, fmt.Errorf("CreateAlliance ensureTable: %w", err)
	}

	// A corporation already in an alliance cannot found another: the link below would tear
	// it out of its current alliance and leave that alliance's rows dangling.
	var currentAlliance int64
	if err := db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&currentAlliance); err != nil {
		return 0, fmt.Errorf("CreateAlliance corp lookup: %w", err)
	}
	if currentAlliance != 0 {
		return 0, fmt.Errorf("the corporation is already in an alliance")
	}

	// The ID comes from the shared player-entity counter: cfg.IsAlliance returns 0 for any
	// ID below MinPlayerEntityID, and corporation and alliance IDs are both owner IDs.
	allianceID, err := nextPlayerEntityID(ctx, db)
	if err != nil {
		return 0, fmt.Errorf("CreateAlliance next ID: %w", err)
	}
	now := win32TimeNow()

	// memberCount starts at 0 and is recounted by JoinAlliance; the column counts pilots,
	// as ESPRecountAlliance and the rankings board read it.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO alliances
		  (allianceID, allianceName, shortName, description, url,
		   executorCorpID, creatorCorpID, creatorCharID,
		   dictatorial, startDate, memberCount, deleted)
		VALUES (?, ?, ?, ?, ?,
		        ?, ?, ?,
		        0, ?, 0, 0)`,
		allianceID, allianceName, shortName, description, url,
		corpID, corpID, charID,
		now,
	); err != nil {
		return 0, fmt.Errorf("CreateAlliance alliances: %w", err)
	}

	// ESPSyncAllianceNames writes every table the client resolves the name from: eveNames,
	// eveStaticOwners (cfg.eveowners, where cfg.IsAlliance reads the typeID), alliance_shortnames.
	if err := ESPSyncAllianceNames(ctx, db, allianceID, allianceName, shortName); err != nil {
		return 0, err
	}

	// Link the founder through JoinAlliance so the member row (executor-vote storage) and
	// the pilot recount happen as for every other join.
	if _, err := JoinAlliance(ctx, db, corpID, allianceID); err != nil {
		return 0, fmt.Errorf("CreateAlliance link corp: %w", err)
	}

	return allianceID, nil
}

// GetAllianceInfo returns a util.Row for the given allianceID from the
// alliances table (full columns), falling back to alliance_shortnames if
// the full table row is missing (e.g. NPC/static alliances).
func GetAllianceInfo(ctx context.Context, db *sql.DB, allianceID int64) (marshal.PyRep, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return nil, fmt.Errorf("GetAllianceInfo ensureTable: %w", err)
	}
	var (
		aName          string
		shortName      string
		description    sql.NullString
		url            sql.NullString
		executorCorpID int64
		creatorCorpID  int64
		creatorCharID  int64
		dictatorial    int64
		startDate      int64
		memberCount    int64
		deleted        int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT allianceName, shortName, description, url,
		       executorCorpID, creatorCorpID, creatorCharID,
		       dictatorial, startDate, memberCount, deleted
		FROM alliances WHERE allianceID = ?`, allianceID,
	).Scan(&aName, &shortName, &description, &url,
		&executorCorpID, &creatorCorpID, &creatorCharID,
		&dictatorial, &startDate, &memberCount, &deleted)
	if errors.Is(err, sql.ErrNoRows) {
		// Fall back to alliance_shortnames, and past that answer an empty deleted row: infosvc
		// reads allianceinfo.description off the answer, so None is a dead Show Info window.
		if scanErr := db.QueryRowContext(ctx,
			`SELECT shortName FROM alliance_shortnames WHERE allianceID = ?`, allianceID,
		).Scan(&shortName); scanErr != nil {
			deleted = 1
		}
		aName = shortName
	} else if err != nil {
		return nil, fmt.Errorf("GetAllianceInfo: %w", err)
	}
	// A creator the row does not record falls back to an owner the client can
	// name: the executor corporation, and that corporation's CEO.
	if creatorCorpID == 0 {
		creatorCorpID = executorCorpID
	}
	if creatorCharID == 0 && creatorCorpID != 0 {
		_ = db.QueryRowContext(ctx,
			`SELECT ceoID FROM corporation WHERE corporationID = ?`, creatorCorpID).Scan(&creatorCharID)
	}
	// infosvc draws Created By with cfg.eveowners.Get, which raises on an unknown owner,
	// and walks the row's header; an unknown creator is left out of the row, not sent as 0.
	cols := make([]string, 0, len(allianceCols))
	var vals []marshal.PyRep
	for i, col := range allianceCols {
		v := []marshal.PyRep{
			intRep(allianceID), strRep(aName), strRep(shortName),
			nullStrRep(description), nullStrRep(url),
			intRep(executorCorpID), intRep(creatorCorpID), intRep(creatorCharID),
			intRep(dictatorial), intRep(startDate), intRep(memberCount), intRep(deleted),
		}[i]
		if (col == "creatorCorpID" && creatorCorpID == 0) || (col == "creatorCharID" && creatorCharID == 0) {
			continue
		}
		cols = append(cols, col)
		vals = append(vals, v)
	}
	return makeRow(cols, vals), nil
}

// GetCorpAssets returns a util.Rowset of every item owned by corpID.  The
// client's corp_ui_accounts.ShowAssets iterates the result and reads
// row.locationID on each entry, so a single rowset (not a dict keyed by
// station) is what it expects.
func GetCorpAssets(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	assetCols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
	}
	rows, err := db.QueryContext(ctx,
		`SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		        e.contraband, e.singleton, e.quantity,
		        COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		   FROM entity e
		   LEFT JOIN invTypes t ON t.typeID = e.typeID
		   LEFT JOIN invGroups g ON g.groupID = t.groupID
		  WHERE e.ownerID = ?
		  ORDER BY e.locationID, e.itemID`,
		corpID,
	)
	if err != nil {
		return makeRowset(assetCols, nil), fmt.Errorf("GetCorpAssets query: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			itemID, typeID, ownerID, locationID, flag            int64
			contraband, singleton, quantity, groupID, categoryID int64
		)
		if err := rows.Scan(&itemID, &typeID, &ownerID, &locationID, &flag,
			&contraband, &singleton, &quantity, &groupID, &categoryID); err != nil {
			return makeRowset(assetCols, nil), fmt.Errorf("GetCorpAssets scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(itemID), intRep(typeID), intRep(ownerID), intRep(locationID),
			intRep(flag), intRep(contraband), intRep(singleton), intRep(quantity),
			intRep(groupID), intRep(categoryID), marshal.PyNone{},
		})
	}
	if err := rows.Err(); err != nil {
		return makeRowset(assetCols, nil), fmt.Errorf("GetCorpAssets rows: %w", err)
	}
	return makeRowset(assetCols, result), nil
}

// GetCorpAssetsAtLocation returns a util.Rowset of items owned by corpID at
// stationID.  Mirrors the response shape the client expects from
// corpmgr.GetAssetInventoryForLocation.
func GetCorpAssetsAtLocation(ctx context.Context, db *sql.DB, corpID, locationID int64) (marshal.PyRep, error) {
	assetCols := []string{
		"itemID", "typeID", "ownerID", "locationID", "flag",
		"contraband", "singleton", "quantity", "groupID", "categoryID", "customInfo",
	}
	rows, err := db.QueryContext(ctx,
		`SELECT e.itemID, e.typeID, e.ownerID, e.locationID, e.flag,
		        e.contraband, e.singleton, e.quantity,
		        COALESCE(t.groupID, 0), COALESCE(g.categoryID, 0)
		   FROM entity e
		   LEFT JOIN invTypes t ON t.typeID = e.typeID
		   LEFT JOIN invGroups g ON g.groupID = t.groupID
		  WHERE e.ownerID = ? AND e.locationID = ?
		  ORDER BY e.itemID`,
		corpID, locationID,
	)
	if err != nil {
		return makeRowset(assetCols, nil), fmt.Errorf("GetCorpAssetsAtLocation query: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			itemID, typeID, ownerID, locID, flag                 int64
			contraband, singleton, quantity, groupID, categoryID int64
		)
		if err := rows.Scan(&itemID, &typeID, &ownerID, &locID, &flag,
			&contraband, &singleton, &quantity, &groupID, &categoryID); err != nil {
			return makeRowset(assetCols, nil), fmt.Errorf("GetCorpAssetsAtLocation scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(itemID), intRep(typeID), intRep(ownerID), intRep(locID),
			intRep(flag), intRep(contraband), intRep(singleton), intRep(quantity),
			intRep(groupID), intRep(categoryID), marshal.PyNone{},
		})
	}
	if err := rows.Err(); err != nil {
		return makeRowset(assetCols, nil), fmt.Errorf("GetCorpAssetsAtLocation rows: %w", err)
	}
	return makeRowset(assetCols, result), nil
}

// GetCorpMembers returns a util.IndexRowset keyed by characterID containing
// every character currently in corpID.  Mirrors what the client expects from
// corporationSvc.GetMembers(corpID).
func GetCorpMembers(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	cols := []string{
		"characterID", "title", "startDateTime",
		"corprole", "rolesAtAll", "rolesAtBase", "rolesAtHQ", "rolesAtOther",
	}
	rows, err := db.QueryContext(ctx, `
		SELECT c.characterID,
		       COALESCE(c.title, ''),
		       c.corporationDateTime,
		       COALESCE(r.corprole,    0),
		       COALESCE(r.rolesAtAll,  0),
		       COALESCE(r.rolesAtBase, 0),
		       COALESCE(r.rolesAtHQ,   0),
		       COALESCE(r.rolesAtOther,0)
		FROM character_ c
		LEFT JOIN chrCorporationRoles r ON r.characterID = c.characterID
		WHERE c.corporationID = ?`, corpID)
	if err != nil {
		return makeIndexRowset("characterID", cols, nil), fmt.Errorf("GetCorpMembers: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			characterID   int64
			title         string
			startDateTime int64
			// Role columns are BIGINT UNSIGNED; scan into uint64 to avoid
			// int64 overflow for masks like 0xFFFFFFFFFFFFFFFF.
			corprole     uint64
			rolesAtAll   uint64
			rolesAtBase  uint64
			rolesAtHQ    uint64
			rolesAtOther uint64
		)
		if err := rows.Scan(&characterID, &title, &startDateTime,
			&corprole, &rolesAtAll, &rolesAtBase, &rolesAtHQ, &rolesAtOther); err != nil {
			return nil, fmt.Errorf("GetCorpMembers scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(characterID), strRep(title), intRep(startDateTime),
			intRep(int64(corprole)), intRep(int64(rolesAtAll)), intRep(int64(rolesAtBase)),
			intRep(int64(rolesAtHQ)), intRep(int64(rolesAtOther)),
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetCorpMembers rows: %w", err)
	}
	return makeIndexRowset("characterID", cols, result), nil
}

// applicationCols are the column names for chrApplications rowsets.
// Must match the SELECT order in GetMyApplications / GetApplications.
var applicationCols = []string{
	"corporationID", "characterID", "applicationText",
	"roles", "grantableRoles", "status",
	"applicationDateTime", "deleted", "lastCorpUpdaterID",
}

// ensureApplicationsTable creates chrApplications if it does not yet exist.
// ensureApplicationsTableGuard runs ensureApplicationsTableDDL once per process: it is called on request paths, and
// even a no-op CREATE TABLE IF NOT EXISTS / ALTER takes a metadata lock and can
// touch the table definition each time.
var ensureApplicationsTableGuard ensureGuard

func ensureApplicationsTable(ctx context.Context, db *sql.DB) error {
	return ensureApplicationsTableGuard.do(func() error { return ensureApplicationsTableDDL(ctx, db) })
}

func ensureApplicationsTableDDL(ctx context.Context, db *sql.DB) error {
	_, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS chrApplications (
		  corporationID       INT UNSIGNED  NOT NULL DEFAULT 0,
		  characterID         INT UNSIGNED  NOT NULL DEFAULT 0,
		  applicationText     TEXT          NOT NULL,
		  roles               BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  grantableRoles      BIGINT UNSIGNED NOT NULL DEFAULT 0,
		  status              INT UNSIGNED  NOT NULL DEFAULT 0,
		  applicationDateTime BIGINT(20)    NOT NULL DEFAULT 0,
		  deleted             TINYINT(1)    NOT NULL DEFAULT 0,
		  lastCorpUpdaterID   INT UNSIGNED  NOT NULL DEFAULT 0,
		  PRIMARY KEY (corporationID, characterID)
		) ENGINE=InnoDB DEFAULT CHARSET=utf8mb3`)
	return err
}

// scanApplicationRow scans one chrApplications row in applicationCols order.
func scanApplicationRow(rows *sql.Rows) ([]marshal.PyRep, error) {
	var (
		corporationID       int64
		characterID         int64
		applicationText     string
		roles               uint64
		grantableRoles      uint64
		status              int64
		applicationDateTime int64
		deleted             int64
		lastCorpUpdaterID   int64
	)
	if err := rows.Scan(
		&corporationID, &characterID, &applicationText,
		&roles, &grantableRoles, &status,
		&applicationDateTime, &deleted, &lastCorpUpdaterID,
	); err != nil {
		return nil, err
	}
	return []marshal.PyRep{
		intRep(corporationID), intRep(characterID), strRep(applicationText),
		intRep(int64(roles)), intRep(int64(grantableRoles)), intRep(status),
		intRep(applicationDateTime), intRep(deleted), intRep(lastCorpUpdaterID),
	}, nil
}

// GetMyApplications returns a util.Rowset of all pending/historical
// applications the given character has submitted.
func GetMyApplications(ctx context.Context, db *sql.DB, charID int64) (marshal.PyRep, error) {
	if err := ensureApplicationsTable(ctx, db); err != nil {
		return makeRowset(applicationCols, nil), fmt.Errorf("GetMyApplications ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, characterID, applicationText,
		       roles, grantableRoles, status,
		       applicationDateTime, deleted, lastCorpUpdaterID
		FROM chrApplications
		WHERE characterID = ? AND deleted = 0`, charID)
	if err != nil {
		return makeRowset(applicationCols, nil), fmt.Errorf("GetMyApplications query: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		row, err := scanApplicationRow(rows)
		if err != nil {
			return nil, fmt.Errorf("GetMyApplications scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetMyApplications rows: %w", err)
	}
	return makeRowset(applicationCols, result), nil
}

// InsertApplication creates a new application from charID to corpID.
func InsertApplication(ctx context.Context, db *sql.DB, charID, corpID int64, appText string) error {
	if err := ensureApplicationsTable(ctx, db); err != nil {
		return fmt.Errorf("InsertApplication ensureTable: %w", err)
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO chrApplications
		  (corporationID, characterID, applicationText, roles, grantableRoles,
		   status, applicationDateTime, deleted, lastCorpUpdaterID)
		VALUES (?, ?, ?, 0, 0, 0, ?, 0, 0)
		ON DUPLICATE KEY UPDATE applicationText=VALUES(applicationText),
		  status=0, applicationDateTime=VALUES(applicationDateTime), deleted=0`,
		corpID, charID, appText, win32TimeNow(),
	)
	if err != nil {
		return fmt.Errorf("InsertApplication insert: %w", err)
	}
	return nil
}

// ApplicationAddChange reads one live application back as the
// OnCorporationApplicationChanged ADD dict: every applicationCols column, old None, new
// the stored value, which is the row bco_applications caches. ok is false when there is
// no live row (an NPC corp's auto-join deletes it).
func ApplicationAddChange(ctx context.Context, db *sql.DB, charID, corpID int64) (marshal.PyDict, bool, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, characterID, applicationText,
		       roles, grantableRoles, status,
		       applicationDateTime, deleted, lastCorpUpdaterID
		FROM chrApplications
		WHERE characterID = ? AND corporationID = ? AND deleted = 0`, charID, corpID)
	if err != nil {
		return marshal.PyDict{}, false, fmt.Errorf("ApplicationAddChange query: %w", err)
	}
	defer rows.Close()
	if !rows.Next() {
		return marshal.PyDict{}, false, rows.Err()
	}
	row, err := scanApplicationRow(rows)
	if err != nil {
		return marshal.PyDict{}, false, fmt.Errorf("ApplicationAddChange scan: %w", err)
	}
	d := marshal.PyDict{Items: make([]marshal.DictEntry, 0, len(applicationCols))}
	for i, col := range applicationCols {
		d.Items = append(d.Items, marshal.DictEntry{
			Key:   marshal.PyString{Value: col},
			Value: marshal.PyTuple{Items: []marshal.PyRep{marshal.PyNone{}, row[i]}},
		})
	}
	return d, true, nil
}

// GetApplications returns a util.IndexRowset (keyed by characterID) of all
// applications to the given corporation.  Called by directors.
func GetApplications(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	if err := ensureApplicationsTable(ctx, db); err != nil {
		return makeIndexRowset("characterID", applicationCols, nil), fmt.Errorf("GetApplications ensureTable: %w", err)
	}
	rows, err := db.QueryContext(ctx, `
		SELECT corporationID, characterID, applicationText,
		       roles, grantableRoles, status,
		       applicationDateTime, deleted, lastCorpUpdaterID
		FROM chrApplications
		WHERE corporationID = ? AND deleted = 0`, corpID)
	if err != nil {
		return makeIndexRowset("characterID", applicationCols, nil), fmt.Errorf("GetApplications query: %w", err)
	}
	defer rows.Close()
	var result [][]marshal.PyRep
	for rows.Next() {
		row, err := scanApplicationRow(rows)
		if err != nil {
			return nil, fmt.Errorf("GetApplications scan: %w", err)
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetApplications rows: %w", err)
	}
	return makeIndexRowset("characterID", applicationCols, result), nil
}

// DeleteApplication removes the application row for (charID, corpID).
func DeleteApplication(ctx context.Context, db *sql.DB, charID, corpID int64) error {
	if err := ensureApplicationsTable(ctx, db); err != nil {
		return fmt.Errorf("DeleteApplication ensureTable: %w", err)
	}
	_, err := db.ExecContext(ctx,
		`DELETE FROM chrApplications WHERE corporationID = ? AND characterID = ?`,
		corpID, charID,
	)
	if err != nil {
		return fmt.Errorf("DeleteApplication delete: %w", err)
	}
	return nil
}

// IsNPCCorp returns true if corpID exists in the crpNPCCorporations table.
func IsNPCCorp(ctx context.Context, db *sql.DB, corpID int64) (bool, error) {
	var n int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM crpNPCCorporations WHERE corporationID = ?`, corpID,
	).Scan(&n)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("IsNPCCorp: %w", err)
	}
	return true, nil
}

// GetStationCorpOffices returns the (owners, corporations, offices) rowsets for
// corpStationMgr.GetCorporateStationInfo: the owning corporation with a virtual office
// (itemID = the station, officeFolderID 0) or its real one, every crpoffices tenant, and every player
// corporation headquartered here (the lobby labels those by corporation.stationID).
func GetStationCorpOffices(ctx context.Context, db *sql.DB, stationID int64) ([][]marshal.PyRep, [][]marshal.PyRep, [][]marshal.PyRep, error) {
	var ownerCorpID int64
	err := db.QueryRowContext(ctx,
		`SELECT corporationID FROM staStations WHERE stationID = ?`, stationID,
	).Scan(&ownerCorpID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GetStationCorpOffices station: %w", err)
	}

	corpIDs := []int64{ownerCorpID}
	seen := map[int64]bool{ownerCorpID: true}

	// Virtual office row for the owner.
	offices := [][]marshal.PyRep{{
		intRep(ownerCorpID), intRep(stationID), intRep(0),
	}}

	// Real offices.
	rows, err := db.QueryContext(ctx,
		`SELECT corporationID, itemID, officeFolderID FROM crpoffices
		 WHERE stationID = ? ORDER BY itemID`, stationID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GetStationCorpOffices offices: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var corpID, itemID, folderID int64
		if err := rows.Scan(&corpID, &itemID, &folderID); err != nil {
			return nil, nil, nil, fmt.Errorf("GetStationCorpOffices scan: %w", err)
		}
		if corpID == ownerCorpID {
			// The owner's real office (OpenOwnerOffice) replaces its virtual row.
			offices[0] = []marshal.PyRep{intRep(corpID), intRep(itemID), intRep(folderID)}
			continue
		}
		offices = append(offices, []marshal.PyRep{
			intRep(corpID), intRep(itemID), intRep(folderID),
		})
		if !seen[corpID] {
			seen[corpID] = true
			corpIDs = append(corpIDs, corpID)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, nil, err
	}

	// Player corps headquartered here.
	hqRows, err := db.QueryContext(ctx, `
		SELECT c.corporationID FROM corporation c
		WHERE c.stationID = ?
		  AND NOT EXISTS (SELECT 1 FROM crpNPCCorporations n
		                  WHERE n.corporationID = c.corporationID)`, stationID)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("GetStationCorpOffices hq: %w", err)
	}
	defer hqRows.Close()
	for hqRows.Next() {
		var corpID int64
		if err := hqRows.Scan(&corpID); err != nil {
			return nil, nil, nil, fmt.Errorf("GetStationCorpOffices hq scan: %w", err)
		}
		if !seen[corpID] {
			seen[corpID] = true
			corpIDs = append(corpIDs, corpID)
		}
	}
	if err := hqRows.Err(); err != nil {
		return nil, nil, nil, err
	}

	var owners, corps [][]marshal.PyRep
	for _, corpID := range corpIDs {
		row, name, err := stationInfoCorpRow(ctx, db, corpID)
		if err != nil {
			return nil, nil, nil, err
		}
		owners = append(owners, []marshal.PyRep{
			intRep(corpID), strRep(name), intRep(2),
		})
		corps = append(corps, row)
	}
	return owners, corps, offices, nil
}

// stationInfoCorpRow loads one corporation as the 35-column row
// GetCorporateStationInfo's corpCols expects, plus its name for the owner row.
func stationInfoCorpRow(ctx context.Context, db *sql.DB, corpID int64) ([]marshal.PyRep, string, error) {
	var (
		corporationName            string
		description                sql.NullString
		shares                     int64
		graphicID                  sql.NullInt64
		memberCount                int64
		ceoID                      int64
		corpStationID              int64
		raceID                     sql.NullInt64
		corporationType            int64
		creatorID                  int64
		hasPlayerPersonnelManager  int64
		tickerName                 string
		sendCharTerminationMessage int64
		shape1, shape2, shape3     sql.NullInt64
		color1, color2, color3     sql.NullInt64
		typeface                   sql.NullString
		memberLimit                int64
		allowedMemberRaceIDs       sql.NullInt64
		url                        sql.NullString
		taxRate                    float64
		minimumJoinStanding        float64
		division1, division2       sql.NullString
		division3, division4       sql.NullString
		division5, division6       sql.NullString
		division7                  sql.NullString
		allianceID                 sql.NullInt64
		deleted                    int64
	)
	err := db.QueryRowContext(ctx, `
		SELECT corporationName, description, shares, graphicID,
		       memberCount, ceoID, stationID, raceID, corporationType, creatorID,
		       hasPlayerPersonnelManager, tickerName, sendCharTerminationMessage,
		       shape1, shape2, shape3, color1, color2, color3, typeface,
		       memberLimit, allowedMemberRaceIDs, url, taxRate, minimumJoinStanding,
		       division1, division2, division3, division4, division5, division6,
		       division7, allianceID, deleted
		FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(
		&corporationName, &description, &shares, &graphicID,
		&memberCount, &ceoID, &corpStationID, &raceID, &corporationType, &creatorID,
		&hasPlayerPersonnelManager, &tickerName, &sendCharTerminationMessage,
		&shape1, &shape2, &shape3, &color1, &color2, &color3, &typeface,
		&memberLimit, &allowedMemberRaceIDs, &url, &taxRate, &minimumJoinStanding,
		&division1, &division2, &division3, &division4, &division5, &division6,
		&division7, &allianceID, &deleted,
	)
	if err != nil {
		return nil, "", fmt.Errorf("stationInfoCorpRow %d: %w", corpID, err)
	}

	// Column order matches GetCorporateStationInfo's corpCols.
	row := []marshal.PyRep{
		intRep(corpID), strRep(corporationName), nullStrRep(description), intRep(shares), nullIntRep(graphicID),
		intRep(memberCount), intRep(ceoID), intRep(corpStationID), nullIntRep(raceID), intRep(corporationType), intRep(creatorID),
		intRep(hasPlayerPersonnelManager), strRep(tickerName), intRep(sendCharTerminationMessage),
		nullIntRep(shape1), nullIntRep(shape2), nullIntRep(shape3),
		nullIntRep(color1), nullIntRep(color2), nullIntRep(color3), nullStrRep(typeface),
		intRep(memberLimit), nullIntRep(allowedMemberRaceIDs), nullStrRep(url),
		floatRep(taxRate), floatRep(minimumJoinStanding),
		nullStrRep(division1), nullStrRep(division2), nullStrRep(division3), nullStrRep(division4),
		nullStrRep(division5), nullStrRep(division6), nullStrRep(division7),
		nullIntRep(allianceID), intRep(deleted),
	}
	return row, corporationName, nil
}

// GetNPCDivisions returns a util.Rowset (divisionID, divisionName, description,
// leaderType) of all NPC corporation divisions.
func GetNPCDivisions(ctx context.Context, db *sql.DB) (marshal.PyRep, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT divisionID, divisionName, description, leaderType FROM crpNPCDivisions ORDER BY divisionID`)
	if err != nil {
		return nil, fmt.Errorf("GetNPCDivisions query: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var divID int64
		var divName, description, leaderType string
		if err := rows.Scan(&divID, &divName, &description, &leaderType); err != nil {
			return nil, fmt.Errorf("GetNPCDivisions scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			marshal.PyInt{Value: uint64(divID)},
			marshal.PyString{Value: divName},
			marshal.PyString{Value: description},
			marshal.PyString{Value: leaderType},
		})
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("GetNPCDivisions rows: %w", err)
	}
	cols := []string{"divisionID", "divisionName", "description", "leaderType"}
	return makeRowset(cols, result), nil
}

// GetApplicationStatus returns the status of a live application, or -1 if there is none.
// Callers use it to build the old/new halves of a change dict.
func GetApplicationStatus(ctx context.Context, db *sql.DB, charID, corpID int64) int64 {
	var s int64
	if err := db.QueryRowContext(ctx,
		`SELECT status FROM chrApplications WHERE corporationID = ? AND characterID = ? AND deleted = 0`,
		corpID, charID,
	).Scan(&s); err != nil {
		return -1
	}
	return s
}

// UpdateApplication updates the status (and optionally text) of an existing
// application.  Called by directors to accept/reject.
func UpdateApplication(ctx context.Context, db *sql.DB, charID, corpID, updaterID, status int64, appText string) error {
	if err := ensureApplicationsTable(ctx, db); err != nil {
		return fmt.Errorf("UpdateApplication ensureTable: %w", err)
	}
	_, err := db.ExecContext(ctx, `
		UPDATE chrApplications
		SET status = ?, lastCorpUpdaterID = ?, applicationText = ?
		WHERE corporationID = ? AND characterID = ?`,
		status, updaterID, appText, corpID, charID,
	)
	if err != nil {
		return fmt.Errorf("UpdateApplication update: %w", err)
	}
	return nil
}

// IsTerminalAppStatus returns true for application statuses that represent a
// concluded application (rejected by either side, or accepted by the applicant).
func IsTerminalAppStatus(status int64) bool {
	return status == AppAcceptedByCharacter ||
		status == AppRejectedByCharacter ||
		status == AppRejectedByCorporation
}

// SoftDeleteApplication marks the application as deleted so it no longer
// appears in GetApplications / GetMyApplications.
func SoftDeleteApplication(ctx context.Context, db *sql.DB, charID, corpID int64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE chrApplications SET deleted = 1 WHERE corporationID = ? AND characterID = ?`,
		corpID, charID,
	)
	if err != nil {
		return fmt.Errorf("SoftDeleteApplication: %w", err)
	}
	return nil
}

// GetCorpTaxRate returns the taxRate for a corporation.
// Returns 0 on error (safe fallback = no tax).
func GetCorpTaxRate(ctx context.Context, db *sql.DB, corpID int64) float64 {
	var rate float64
	err := db.QueryRowContext(ctx,
		`SELECT taxRate FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&rate)
	if err != nil {
		return 0
	}
	return rate
}

// MigrateCorpMemberRoles resets chrCorporationRoles to DefaultNewMemberRoles for every
// non-CEO and RoleAll for every CEO (RoleAll draws the client's CEO/director corp UI).
// Runs once, guarded by the server_migrations table. Returns rows changed.
func MigrateCorpMemberRoles(ctx context.Context, db *sql.DB) (int64, error) {
	const migrationName = "corp-member-roles-reset"
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS server_migrations (
			name    VARCHAR(64) NOT NULL,
			applied BIGINT NOT NULL,
			PRIMARY KEY (name)
		)`); err != nil {
		return 0, fmt.Errorf("MigrateCorpMemberRoles meta table: %w", err)
	}
	var done int
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM server_migrations WHERE name = ?`, migrationName).Scan(&done)
	if done > 0 {
		return 0, nil
	}

	memberRes, err := db.ExecContext(ctx, `
		UPDATE chrCorporationRoles r
		JOIN character_ ch ON ch.characterID = r.characterID
		JOIN corporation co ON co.corporationID = ch.corporationID
		SET r.corprole = ?, r.rolesAtAll = ?, r.rolesAtBase = ?,
		    r.rolesAtHQ = ?, r.rolesAtOther = ?
		WHERE co.ceoID <> r.characterID`,
		corp.DefaultNewMemberRoles, corp.DefaultNewMemberRoles, corp.DefaultNewMemberRoles,
		corp.DefaultNewMemberRoles, corp.DefaultNewMemberRoles,
	)
	if err != nil {
		return 0, fmt.Errorf("MigrateCorpMemberRoles members: %w", err)
	}
	ceoRes, err := db.ExecContext(ctx, `
		UPDATE chrCorporationRoles r
		JOIN character_ ch ON ch.characterID = r.characterID
		JOIN corporation co ON co.corporationID = ch.corporationID
		SET r.corprole = ?, r.rolesAtAll = ?, r.rolesAtBase = ?,
		    r.rolesAtHQ = ?, r.rolesAtOther = ?
		WHERE co.ceoID = r.characterID`,
		corp.RoleAll, corp.RoleAll, corp.RoleAll, corp.RoleAll, corp.RoleAll,
	)
	if err != nil {
		return 0, fmt.Errorf("MigrateCorpMemberRoles ceos: %w", err)
	}

	if _, err := db.ExecContext(ctx,
		`INSERT INTO server_migrations (name, applied) VALUES (?, ?)`,
		migrationName, win32TimeNow()); err != nil {
		return 0, fmt.Errorf("MigrateCorpMemberRoles mark applied: %w", err)
	}
	nMembers, _ := memberRes.RowsAffected()
	nCEOs, _ := ceoRes.RowsAffected()
	return nMembers + nCEOs, nil
}

// GetCorpBalance returns a corporation's wallet balance.
func GetCorpBalance(ctx context.Context, db *sql.DB, corpID int64) (float64, error) {
	var balance float64
	err := db.QueryRowContext(ctx,
		`SELECT balance FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&balance)
	if err != nil {
		return 0, fmt.Errorf("GetCorpBalance corpID=%d: %w", corpID, err)
	}
	return balance, nil
}

// CreditCorpWallet adds amount to a corporation's wallet balance.
func CreditCorpWallet(ctx context.Context, db *sql.DB, corpID int64, amount float64) error {
	_, err := db.ExecContext(ctx,
		`UPDATE corporation SET balance = balance + ? WHERE corporationID = ?`,
		amount, corpID)
	if err != nil {
		return fmt.Errorf("CreditCorpWallet corpID=%d: %w", corpID, err)
	}
	return nil
}

// CorpMemberOwners returns the (ownerID, ownerName, typeID) rowset behind
// corpRegistry.GetEveOwners: every player character (accountID > 0) in corpID. bco_members
// copies each row into cfg.eveowners and keeps the IDs as the member-hangar picker list.
// Not a bulk-data addition: config.BulkData.owners leaves player characters out (cache/catalog.go).
func CorpMemberOwners(ctx context.Context, db *sql.DB, corpID int64) (marshal.PyRep, error) {
	cols := []string{"ownerID", "ownerName", "typeID"}
	rows, err := db.QueryContext(ctx, `
		SELECT characterID, characterName, typeID
		  FROM character_
		 WHERE corporationID = ? AND accountID > 0
		 ORDER BY characterName`, corpID)
	if err != nil {
		return makeRowset(cols, nil), fmt.Errorf("CorpMemberOwners query: %w", err)
	}
	defer rows.Close()

	var result [][]marshal.PyRep
	for rows.Next() {
		var (
			ownerID, typeID int64
			ownerName       string
		)
		if err := rows.Scan(&ownerID, &ownerName, &typeID); err != nil {
			return makeRowset(cols, nil), fmt.Errorf("CorpMemberOwners scan: %w", err)
		}
		result = append(result, []marshal.PyRep{
			intRep(ownerID), strRep(ownerName), intRep(typeID),
		})
	}
	if err := rows.Err(); err != nil {
		return makeRowset(cols, nil), fmt.Errorf("CorpMemberOwners rows: %w", err)
	}
	return makeRowset(cols, result), nil
}
