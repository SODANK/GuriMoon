package db

// ESP corporation and alliance administration. A corp's pilot count is accountID > 0, since
// NPC agents carry a corporationID too; corporation.memberCount is stored and unmaintained, so
// it is shown beside the live count. An alliance's name lives in several tables that
// ESPSyncAllianceNames writes. See docs/db-5-notes.md#esp-corporation-and-alliance-administration.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// ── Corporations ─────────────────────────────────────────────────────────────

// ESPCorpListRow is one line of the corporation browser.
type ESPCorpListRow struct {
	CorporationID   int64
	CorporationName string
	TickerName      string
	AllianceID      int64
	AllianceName    string
	CEOID           int64
	CEOName         string
	StationID       int64
	StationName     string
	Pilots          int64 // live count of player members (accountID > 0)
	StoredMembers   int64 // corporation.memberCount as stored
	TaxRate         float64
	IsNPC           bool
	Deleted         bool
	HasLogo         bool
}

// TaxPct renders taxRate as the client's corp window does. The column is a fraction
// (0.1 = 10%).
func (r ESPCorpListRow) TaxPct() float64 { return r.TaxRate * 100 }

// ESPCorpFull is the corporation editor's view of one corp.
type ESPCorpFull struct {
	ESPCorpListRow
	Description  string
	URL          string
	MemberLimit  int64
	AllowedRaces int64
	RaceID       int64
	Shares       int64
	Balance      float64
	CreatorID    int64
	Shape1       int64 // 0 = unset (the column is nullable)
	Shape2       int64
	Shape3       int64
	Color1       int64
	Color2       int64
	Color3       int64
	Divisions    [7]string
}

// npcCorpRange is the client's minNPCCorporation..maxNPCCorporation band; a corporation
// inside it is static content.
const (
	npcCorpMinID = 1000000
	npcCorpMaxID = 1999999
)

// corpListWhere builds the filter shared by ESPListCorps and ESPCountCorps, so the page and
// its total count the same rows.
func corpListWhere(q string, allianceID int64, kind string) (string, []any) {
	where := " WHERE 1 = 1"
	var args []any
	if q = strings.TrimSpace(q); q != "" {
		where += ` AND (c.corporationName LIKE ? OR c.tickerName LIKE ?)`
		like := "%" + q + "%"
		args = append(args, like, like)
	}
	switch {
	case allianceID > 0:
		where += ` AND c.allianceID = ?`
		args = append(args, allianceID)
	case allianceID < 0:
		where += ` AND c.allianceID = 0`
	}
	switch kind {
	case "npc":
		where += fmt.Sprintf(` AND c.corporationID BETWEEN %d AND %d`, npcCorpMinID, npcCorpMaxID)
	case "player":
		where += fmt.Sprintf(` AND c.corporationID NOT BETWEEN %d AND %d`, npcCorpMinID, npcCorpMaxID)
	}
	return where, args
}

// ESPCountCorps counts what ESPListCorps would return unpaged. It repeats only the filter,
// not the display joins.
func ESPCountCorps(ctx context.Context, db *sql.DB, q string, allianceID int64, kind string) (int64, error) {
	where, args := corpListWhere(q, allianceID, kind)
	var n int64
	if err := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM corporation c"+where, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("ESPCountCorps: %w", err)
	}
	return n, nil
}

// ESPListCorps returns corporations for the browser. q matches name or ticker, allianceID > 0
// narrows to one alliance (-1 means no alliance), kind is "", "player" or "npc". limit 0
// returns every matching row; a caller that pages or caps says so, so a list is never silently
// truncated.
func ESPListCorps(ctx context.Context, db *sql.DB, q string, allianceID int64, kind string, limit, offset int) ([]ESPCorpListRow, error) {
	// Pilot counts come from a derived table, not a correlated subquery, which would scan
	// character_ once per corporation.
	query := `
		SELECT c.corporationID, c.corporationName, c.tickerName, c.allianceID,
		       COALESCE(a.allianceName, ''), c.ceoID, COALESCE(ceo.characterName, ''),
		       c.stationID, COALESCE(s.stationName, ''),
		       COALESCE(m.pilots, 0), c.memberCount, c.taxRate, c.deleted,
		       (c.shape1 IS NOT NULL OR c.shape2 IS NOT NULL OR c.shape3 IS NOT NULL)
		  FROM corporation c
		  LEFT JOIN alliances a ON a.allianceID = c.allianceID
		  LEFT JOIN character_ ceo ON ceo.characterID = c.ceoID
		  LEFT JOIN staStations s ON s.stationID = c.stationID
		  LEFT JOIN (SELECT corporationID, COUNT(*) AS pilots
		               FROM character_ WHERE accountID > 0
		              GROUP BY corporationID) m ON m.corporationID = c.corporationID`
	where, args := corpListWhere(q, allianceID, kind)
	query += where + ` ORDER BY c.corporationName`
	if limit > 0 {
		query += ` LIMIT ? OFFSET ?`
		args = append(args, limit, offset)
	}

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("ESPListCorps: %w", err)
	}
	defer rows.Close()

	var out []ESPCorpListRow
	for rows.Next() {
		var r ESPCorpListRow
		var deleted, hasLogo int64
		if err := rows.Scan(&r.CorporationID, &r.CorporationName, &r.TickerName,
			&r.AllianceID, &r.AllianceName, &r.CEOID, &r.CEOName,
			&r.StationID, &r.StationName, &r.Pilots, &r.StoredMembers,
			&r.TaxRate, &deleted, &hasLogo); err != nil {
			return nil, fmt.Errorf("ESPListCorps scan: %w", err)
		}
		r.Deleted = deleted != 0
		r.HasLogo = hasLogo != 0
		r.IsNPC = r.CorporationID >= npcCorpMinID && r.CorporationID <= npcCorpMaxID
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPGetCorpFull loads one corporation for the editor.
func ESPGetCorpFull(ctx context.Context, db *sql.DB, corpID int64) (*ESPCorpFull, error) {
	d := &ESPCorpFull{}
	var (
		deleted                            int64
		raceID                             sql.NullInt64
		shape1, shape2, shape3             sql.NullInt64
		color1, color2, color3             sql.NullInt64
		desc, url                          sql.NullString
		div1, div2, div3, div4, div5, div6 sql.NullString
		div7                               sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT c.corporationID, c.corporationName, c.tickerName, c.allianceID,
		       COALESCE(a.allianceName, ''), c.ceoID, COALESCE(ceo.characterName, ''),
		       c.stationID, COALESCE(s.stationName, ''),
		       (SELECT COUNT(*) FROM character_ ch
		         WHERE ch.corporationID = c.corporationID AND ch.accountID > 0),
		       c.memberCount, c.taxRate, c.deleted,
		       c.description, c.url, c.memberLimit, c.allowedMemberRaceIDs, c.raceID,
		       c.shares, c.balance, c.creatorID,
		       c.shape1, c.shape2, c.shape3, c.color1, c.color2, c.color3,
		       c.division1, c.division2, c.division3, c.division4,
		       c.division5, c.division6, c.division7
		  FROM corporation c
		  LEFT JOIN alliances a ON a.allianceID = c.allianceID
		  LEFT JOIN character_ ceo ON ceo.characterID = c.ceoID
		  LEFT JOIN staStations s ON s.stationID = c.stationID
		 WHERE c.corporationID = ?`, corpID,
	).Scan(&d.CorporationID, &d.CorporationName, &d.TickerName, &d.AllianceID,
		&d.AllianceName, &d.CEOID, &d.CEOName, &d.StationID, &d.StationName,
		&d.Pilots, &d.StoredMembers, &d.TaxRate, &deleted,
		&desc, &url, &d.MemberLimit, &d.AllowedRaces, &raceID,
		&d.Shares, &d.Balance, &d.CreatorID,
		&shape1, &shape2, &shape3, &color1, &color2, &color3,
		&div1, &div2, &div3, &div4, &div5, &div6, &div7)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("corporation %d not found", corpID)
	} else if err != nil {
		return nil, fmt.Errorf("ESPGetCorpFull: %w", err)
	}
	d.Deleted = deleted != 0
	d.IsNPC = corpID >= npcCorpMinID && corpID <= npcCorpMaxID
	d.Description, d.URL = desc.String, url.String
	d.RaceID = raceID.Int64
	d.Shape1, d.Shape2, d.Shape3 = shape1.Int64, shape2.Int64, shape3.Int64
	d.Color1, d.Color2, d.Color3 = color1.Int64, color2.Int64, color3.Int64
	d.HasLogo = shape1.Valid || shape2.Valid || shape3.Valid
	for i, v := range []sql.NullString{div1, div2, div3, div4, div5, div6, div7} {
		d.Divisions[i] = v.String
	}
	return d, nil
}

// ESPCorpEdit carries the corporation editor's form.
type ESPCorpEdit struct {
	Name         string
	Ticker       string
	Description  string
	URL          string
	TaxRate      float64
	CEOID        int64
	StationID    int64
	MemberLimit  int64
	AllowedRaces int64
	Divisions    [7]string
}

// ESPUpdateCorp writes the corporation editor's form. The name also lives in
// eveNames/eveStaticOwners, where the client resolves owner names, so a rename updates those too.
func ESPUpdateCorp(ctx context.Context, db *sql.DB, corpID int64, in ESPCorpEdit) error {
	if strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("corporation name may not be empty")
	}
	if in.TaxRate < 0 || in.TaxRate > 1 {
		return fmt.Errorf("tax rate must be between 0 and 1")
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPUpdateCorp begin: %w", err)
	}
	defer tx.Rollback()

	// A rename carries the Galnet address along while it is still the one the server handed
	// out (corppageurl.go).
	var oldName, oldURL string
	if err := tx.QueryRowContext(ctx,
		`SELECT corporationName, COALESCE(url, '') FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&oldName, &oldURL); err != nil {
		return fmt.Errorf("ESPUpdateCorp read: %w", err)
	}
	if in.Name != oldName && strings.TrimSpace(in.URL) == strings.TrimSpace(oldURL) &&
		isStampedCorpURL(oldURL, corpID, oldName) {
		u, err := corpGalnetURL(ctx, tx, corpID, in.Name)
		if err != nil {
			return fmt.Errorf("ESPUpdateCorp url: %w", err)
		}
		if u != "" {
			in.URL = u
		}
	}

	if _, err := tx.ExecContext(ctx, `
		UPDATE corporation
		   SET corporationName = ?, tickerName = ?, description = ?, url = ?,
		       taxRate = ?, ceoID = ?, stationID = ?, memberLimit = ?,
		       allowedMemberRaceIDs = ?,
		       division1 = ?, division2 = ?, division3 = ?, division4 = ?,
		       division5 = ?, division6 = ?, division7 = ?
		 WHERE corporationID = ?`,
		in.Name, in.Ticker, in.Description, in.URL, in.TaxRate,
		in.CEOID, in.StationID, in.MemberLimit, in.AllowedRaces,
		in.Divisions[0], in.Divisions[1], in.Divisions[2], in.Divisions[3],
		in.Divisions[4], in.Divisions[5], in.Divisions[6], corpID,
	); err != nil {
		return fmt.Errorf("ESPUpdateCorp update: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE eveNames SET itemName = ? WHERE itemID = ?`, in.Name, corpID); err != nil {
		return fmt.Errorf("ESPUpdateCorp eveNames: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE eveStaticOwners SET ownerName = ? WHERE ownerID = ?`, in.Name, corpID); err != nil {
		return fmt.Errorf("ESPUpdateCorp eveStaticOwners: %w", err)
	}
	return tx.Commit()
}

// ESPSetCorpAlliance moves a corporation in or out of an alliance (0 = out) through
// JoinAlliance / LeaveAlliance, so a panel edit lands in the state an in-game join would.
// Returns the alliance the corp was in, for the allianceid session push to online members.
func ESPSetCorpAlliance(ctx context.Context, db *sql.DB, corpID, allianceID int64) (int64, error) {
	var oldAlliance int64
	if err := db.QueryRowContext(ctx,
		`SELECT allianceID FROM corporation WHERE corporationID = ?`, corpID,
	).Scan(&oldAlliance); err != nil {
		return 0, fmt.Errorf("ESPSetCorpAlliance read: %w", err)
	}
	if allianceID > 0 {
		var n int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM alliances WHERE allianceID = ?`, allianceID).Scan(&n); err != nil {
			return 0, fmt.Errorf("ESPSetCorpAlliance check: %w", err)
		}
		if n == 0 {
			return 0, fmt.Errorf("alliance %d does not exist", allianceID)
		}
	}
	if oldAlliance > 0 {
		if _, _, err := LeaveAlliance(ctx, db, corpID); err != nil {
			return oldAlliance, err
		}
	}
	if allianceID > 0 {
		if _, err := JoinAlliance(ctx, db, corpID, allianceID); err != nil {
			return oldAlliance, err
		}
	}
	return oldAlliance, nil
}

// ── Corporation deletion ─────────────────────────────────────────────────────

// ESPCorpKick is one pilot moved out of a deleted corporation, with what the caller needs
// for the corpid session change.
type ESPCorpKick struct {
	CharID    int64
	NPCCorpID int64
	Roles     uint64
}

// ESPDeleteCorpResult reports what ESPDeleteCorp actually did.
type ESPDeleteCorpResult struct {
	Name            string
	Kicked          []ESPCorpKick
	OldAllianceID   int64
	WarsFinished    int64
	BillsWrittenOff int64
	ItemsLeft       int64 // entity rows still owned by the corp
}

// ESPDeleteCorp closes a player corporation as the in-game destroy path does, for a corp of
// any size: pilots return to their previous NPC corp, the corp leaves its alliance, wars end,
// bills are written off and dependent rows go. The corporation row is marked deleted, not
// removed. See docs/db-5-notes.md#esp-corporation-and-alliance-administration.
func ESPDeleteCorp(ctx context.Context, db *sql.DB, corpID int64) (*ESPDeleteCorpResult, error) {
	if corpID <= npcCorpMaxID {
		return nil, fmt.Errorf("corporation %d is an NPC corporation (static content) and cannot be deleted", corpID)
	}

	res := &ESPDeleteCorpResult{}
	err := db.QueryRowContext(ctx, `
		SELECT corporationName, allianceID FROM corporation WHERE corporationID = ?`,
		corpID).Scan(&res.Name, &res.OldAllianceID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("corporation %d not found", corpID)
	} else if err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp read: %w", err)
	}

	var agents int64
	if err := db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM character_
		 WHERE corporationID = ? AND COALESCE(accountID, 0) = 0`, corpID).Scan(&agents); err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp agent check: %w", err)
	}
	if agents > 0 {
		return nil, fmt.Errorf("corporation %d carries %d NPC agents on its roster — that is static content, not a player corporation", corpID, agents)
	}

	// The cleanup touches lazily created tables; ensure they exist first.
	for _, ensure := range []func(context.Context, *sql.DB) error{
		EnsureCorpWriteTables, ensureApplicationsTable, ensureCorpTitlesTable,
		EnsureAllianceTables, ensureStarbaseTable, EnsureBillTables, EnsureWarTables,
	} {
		if err := ensure(ctx, db); err != nil {
			return nil, fmt.Errorf("ESPDeleteCorp ensure: %w", err)
		}
	}

	// A tower keeps fuel and standings logic keyed to its owner corp, so the owner cannot go first.
	var towers int64
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM starbase WHERE ownerCorpID = ?`, corpID).Scan(&towers); err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp starbase check: %w", err)
	}
	if towers > 0 {
		return nil, fmt.Errorf("corporation %d still owns %d starbase(s) — remove them first", corpID, towers)
	}

	// Kick every pilot to their fallback NPC corp through JoinCorporation, the in-game
	// expulsion path, before the cleanup transaction.
	memberRows, err := db.QueryContext(ctx,
		`SELECT characterID FROM character_ WHERE corporationID = ? AND accountID > 0`, corpID)
	if err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp members: %w", err)
	}
	var members []int64
	for memberRows.Next() {
		var id int64
		if err := memberRows.Scan(&id); err == nil {
			members = append(members, id)
		}
	}
	memberRows.Close()
	for _, charID := range members {
		npcCorp := PreviousNPCCorp(ctx, db, charID)
		roles, err := JoinCorporation(ctx, db, charID, npcCorp, corpID)
		if err != nil {
			return nil, fmt.Errorf("ESPDeleteCorp kick %d: %w", charID, err)
		}
		res.Kicked = append(res.Kicked, ESPCorpKick{CharID: charID, NPCCorpID: npcCorp, Roles: roles})
	}

	// LeaveAlliance clears the corp's executor vote and application and recomputes the executor.
	if res.OldAllianceID > 0 {
		if _, _, err := LeaveAlliance(ctx, db, corpID); err != nil {
			return nil, fmt.Errorf("ESPDeleteCorp alliance: %w", err)
		}
	}

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp begin: %w", err)
	}
	defer tx.Rollback()

	now := win32TimeNow()
	warRes, err := tx.ExecContext(ctx,
		`UPDATE wars SET timeFinished = ?
		  WHERE (declaredByID = ? OR againstID = ?) AND timeFinished IS NULL`,
		now, corpID, corpID)
	if err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp wars: %w", err)
	}
	res.WarsFinished, _ = warRes.RowsAffected()

	billRes, err := tx.ExecContext(ctx,
		`UPDATE bills SET paid = 1 WHERE paid = 0 AND (debtorID = ? OR creditorID = ?)`,
		corpID, corpID)
	if err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp bills: %w", err)
	}
	res.BillsWrittenOff, _ = billRes.RowsAffected()

	// The corporation's stakes in other corporations return to their treasuries.
	if _, err := ReturnSharesToIssuers(ctx, tx, corpID); err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp shares: %w", err)
	}

	for _, stmt := range []string{
		`DELETE FROM crpOffices WHERE corporationID = ?`,
		`DELETE FROM chrApplications WHERE corporationID = ?`,
		`DELETE FROM crpCharShares WHERE corporationID = ?`,
		`DELETE FROM crpTitles WHERE corporationID = ?`,
		`DELETE FROM crpLockedItems WHERE corporationID = ?`,
		`DELETE v FROM crpVotes v JOIN crpVoteCases c ON c.voteCaseID = v.voteCaseID WHERE c.corporationID = ?`,
		`DELETE o FROM crpVoteOptions o JOIN crpVoteCases c ON c.voteCaseID = o.voteCaseID WHERE c.corporationID = ?`,
		`DELETE FROM crpVoteCases WHERE corporationID = ?`,
		// LeaveAlliance handled the current alliance; these catch pending applications elsewhere.
		`DELETE FROM allianceApplications WHERE corporationID = ?`,
		`DELETE FROM allianceMembers WHERE corporationID = ?`,
		// The corp chat channel is keyed on the corporationID (AddCorporation).
		`DELETE FROM channelMods WHERE channelID = ?`,
		`DELETE FROM channelchars WHERE channelID = ?`,
		`DELETE FROM channels WHERE channelID = ?`,
		`UPDATE solarSystemSov SET claimingCorpID = 0 WHERE claimingCorpID = ?`,
		`DELETE FROM chrStandings WHERE toID = ?`,
		`DELETE FROM crpStandings WHERE corporationID = ?`,
		`DELETE FROM crpStandings WHERE toID = ?`,
		`DELETE FROM bookmarks WHERE ownerID = ?`,
		// Mark closed, not delete; the members are gone and LeaveAlliance zeroed allianceID.
		`UPDATE corporation SET deleted = 1, memberCount = 0, allianceID = 0 WHERE corporationID = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, corpID); err != nil {
			return nil, fmt.Errorf("ESPDeleteCorp %q: %w", stmt, err)
		}
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM chrStandingChanges WHERE fromID = ? OR toID = ?`, corpID, corpID); err != nil {
		return nil, fmt.Errorf("ESPDeleteCorp standing changes: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	NotifyCorpDeleted(corpID)

	// Items the corp still owns (office hangar contents) are left in place and counted.
	_ = db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM entity WHERE ownerID = ?`, corpID).Scan(&res.ItemsLeft)
	return res, nil
}

// ── Corporation logo graphics ────────────────────────────────────────────────

// ESPLogoGraphic is one selectable logo layer graphic.
type ESPLogoGraphic struct {
	GraphicID int64
	Name      string // derived from url3D, e.g. "Luminant Blue"
	URL3D     string
}

// LogoShapeMinID / LogoShapeMaxID bound the corporation logo shape graphics (eveGraphics rows
// for LayerShape/N.blue, composited three to a corporationlogo.blue); 415 is shape 0, the empty
// plate. The ranges stop where the client's art does, not where eveGraphics does.
const (
	LogoShapeMinID = 415
	LogoShapeMaxID = 578
	LogoColorMinID = 671
	LogoColorMaxID = 685
)

// ESPListLogoShapes returns the corporation logo shape graphics in ID order.
func ESPListLogoShapes(ctx context.Context, db *sql.DB) ([]ESPLogoGraphic, error) {
	return listLogoGraphics(ctx, db, LogoShapeMinID, LogoShapeMaxID)
}

// ESPListLogoColors returns the corporation logo colour graphics in ID order.
func ESPListLogoColors(ctx context.Context, db *sql.DB) ([]ESPLogoGraphic, error) {
	return listLogoGraphics(ctx, db, LogoColorMinID, LogoColorMaxID)
}

func listLogoGraphics(ctx context.Context, db *sql.DB, lo, hi int64) ([]ESPLogoGraphic, error) {
	rows, err := db.QueryContext(ctx,
		`SELECT graphicID, COALESCE(url3D, '') FROM eveGraphics
		  WHERE graphicID BETWEEN ? AND ? ORDER BY graphicID`, lo, hi)
	if err != nil {
		return nil, fmt.Errorf("listLogoGraphics: %w", err)
	}
	defer rows.Close()
	var out []ESPLogoGraphic
	for rows.Next() {
		var g ESPLogoGraphic
		if err := rows.Scan(&g.GraphicID, &g.URL3D); err != nil {
			return nil, fmt.Errorf("listLogoGraphics scan: %w", err)
		}
		g.Name = logoGraphicName(g.URL3D)
		out = append(out, g)
	}
	return out, rows.Err()
}

// logoGraphicName reduces a paintshop url3D to its leaf name (".../Luminant Blue.blue" gives
// "Luminant Blue").
func logoGraphicName(url3D string) string {
	name := url3D
	if i := strings.LastIndexAny(name, "/\\"); i >= 0 {
		name = name[i+1:]
	}
	return strings.TrimSuffix(name, ".blue")
}

// ── Alliances ────────────────────────────────────────────────────────────────

// ESPAllianceRow is one line of the alliance browser.
type ESPAllianceRow struct {
	AllianceID     int64
	AllianceName   string
	ShortName      string
	ExecutorCorpID int64
	ExecutorName   string
	MemberCorps    int64
	Pilots         int64
	StoredMembers  int64
	Dictatorial    bool
	Deleted        bool
	StartDate      int64
	SovSystems     int64
	LogoTile       string // client atlas tile this ID draws
	LogoMapped     bool   // false: the client falls back to 01_01
	LowID          bool   // ID below the client's player-entity range
}

// AllianceIDIsUsable reports whether the client treats an ID as an alliance: cfg.IsAlliance
// returns 0 below MinPlayerEntityID. `-renumber` (db/migrateids.go) fixes an existing one.
func AllianceIDIsUsable(allianceID int64) bool { return allianceID >= MinPlayerEntityID }

// ESPAllianceFull is the alliance editor's view.
type ESPAllianceFull struct {
	ESPAllianceRow
	Description   string
	URL           string
	CreatorCorpID int64
	CreatorCharID int64
	Members       []ESPCorpListRow
}

// ESPListAlliancesFull returns every alliance with live counts.
func ESPListAlliancesFull(ctx context.Context, db *sql.DB) ([]ESPAllianceRow, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.allianceID, a.allianceName, a.shortName, a.executorCorpID,
		       COALESCE(e.corporationName, ''), a.dictatorial, a.deleted,
		       a.startDate, a.memberCount,
		       COALESCE(m.corps, 0), COALESCE(m.pilots, 0),
		       COALESCE(v.systems, 0)
		  FROM alliances a
		  LEFT JOIN corporation e ON e.corporationID = a.executorCorpID
		  LEFT JOIN (SELECT c.allianceID,
		                    COUNT(*) AS corps,
		                    SUM((SELECT COUNT(*) FROM character_ ch
		                          WHERE ch.corporationID = c.corporationID
		                            AND ch.accountID > 0)) AS pilots
		               FROM corporation c
		              WHERE c.allianceID > 0
		              GROUP BY c.allianceID) m ON m.allianceID = a.allianceID
		  LEFT JOIN (SELECT allianceID, COUNT(*) AS systems
		               FROM solarSystemSov GROUP BY allianceID) v ON v.allianceID = a.allianceID
		 ORDER BY a.allianceName`)
	if err != nil {
		return nil, fmt.Errorf("ESPListAlliancesFull: %w", err)
	}
	defer rows.Close()

	var out []ESPAllianceRow
	for rows.Next() {
		var r ESPAllianceRow
		var dict, del int64
		if err := rows.Scan(&r.AllianceID, &r.AllianceName, &r.ShortName,
			&r.ExecutorCorpID, &r.ExecutorName, &dict, &del, &r.StartDate,
			&r.StoredMembers, &r.MemberCorps, &r.Pilots, &r.SovSystems); err != nil {
			return nil, fmt.Errorf("ESPListAlliancesFull scan: %w", err)
		}
		r.Dictatorial, r.Deleted = dict != 0, del != 0
		r.LogoTile, r.LogoMapped = AllianceLogoTile(r.AllianceID)
		r.LowID = !AllianceIDIsUsable(r.AllianceID)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ESPGetAllianceFull loads one alliance and its member corporations.
func ESPGetAllianceFull(ctx context.Context, db *sql.DB, allianceID int64) (*ESPAllianceFull, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return nil, err
	}
	a := &ESPAllianceFull{}
	var (
		dict, del  int64
		desc, url  sql.NullString
		executorNm sql.NullString
	)
	err := db.QueryRowContext(ctx, `
		SELECT a.allianceID, a.allianceName, a.shortName, a.description, a.url,
		       a.executorCorpID, e.corporationName, a.creatorCorpID, a.creatorCharID,
		       a.dictatorial, a.startDate, a.memberCount, a.deleted,
		       (SELECT COUNT(*) FROM solarSystemSov v WHERE v.allianceID = a.allianceID)
		  FROM alliances a
		  LEFT JOIN corporation e ON e.corporationID = a.executorCorpID
		 WHERE a.allianceID = ?`, allianceID,
	).Scan(&a.AllianceID, &a.AllianceName, &a.ShortName, &desc, &url,
		&a.ExecutorCorpID, &executorNm, &a.CreatorCorpID, &a.CreatorCharID,
		&dict, &a.StartDate, &a.StoredMembers, &del, &a.SovSystems)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("alliance %d not found", allianceID)
	} else if err != nil {
		return nil, fmt.Errorf("ESPGetAllianceFull: %w", err)
	}
	a.Description, a.URL = desc.String, url.String
	a.ExecutorName = executorNm.String
	a.Dictatorial, a.Deleted = dict != 0, del != 0
	a.LogoTile, a.LogoMapped = AllianceLogoTile(allianceID)
	a.LowID = !AllianceIDIsUsable(allianceID)

	// Every member corp, unpaged: the roster is the page's subject.
	members, err := ESPListCorps(ctx, db, "", allianceID, "", 0, 0)
	if err != nil {
		return a, nil
	}
	a.Members = members
	a.MemberCorps = int64(len(members))
	for _, m := range members {
		a.Pilots += m.Pilots
	}
	return a, nil
}

// ESPAllianceEdit carries the alliance editor's form.
type ESPAllianceEdit struct {
	Name           string
	ShortName      string
	Description    string
	URL            string
	ExecutorCorpID int64
	Dictatorial    bool
}

// ESPCreateAlliance creates an alliance from the panel. allianceID names a specific ID (the
// way a logo is chosen, db/alliancelogoslots.go) or is 0 for the next free player-entity ID.
// A set founderCorpID is linked in and becomes the executor.
func ESPCreateAlliance(ctx context.Context, db *sql.DB, allianceID int64, in ESPAllianceEdit, founderCorpID int64) (int64, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return 0, err
	}
	if strings.TrimSpace(in.Name) == "" {
		return 0, fmt.Errorf("alliance name may not be empty")
	}
	if s := strings.TrimSpace(in.ShortName); s == "" || len(s) > 5 {
		return 0, fmt.Errorf("short name must be 1–5 characters")
	}
	if allianceID > 0 {
		// cfg.IsAlliance returns 0 below the player entity range.
		if allianceID < MinPlayerEntityID {
			return 0, fmt.Errorf("alliance ID must be >= %d — the client does not treat a lower ID as an alliance", MinPlayerEntityID)
		}
		var taken int64
		if err := db.QueryRowContext(ctx,
			`SELECT (SELECT COUNT(*) FROM alliances WHERE allianceID = ?)
			      + (SELECT COUNT(*) FROM corporation WHERE corporationID = ?)
			      + (SELECT COUNT(*) FROM eveNames WHERE itemID = ?)
			      + (SELECT COUNT(*) FROM entity WHERE itemID = ?)`,
			allianceID, allianceID, allianceID, allianceID).Scan(&taken); err != nil {
			return 0, fmt.Errorf("ESPCreateAlliance id check: %w", err)
		}
		if taken > 0 {
			return 0, fmt.Errorf("ID %d is already in use", allianceID)
		}
	} else {
		var err error
		if allianceID, err = nextPlayerEntityID(ctx, db); err != nil {
			return 0, fmt.Errorf("ESPCreateAlliance next ID: %w", err)
		}
	}

	dict := int64(0)
	if in.Dictatorial {
		dict = 1
	}
	// The panel has no founding pilot; creatorCharID 0 breaks the client's Created By link, so
	// the founding corp's CEO stands in. See docs/db-5-notes.md#alliance-membership-and-applications.
	var creatorCharID int64
	if founderCorpID > 0 {
		_ = db.QueryRowContext(ctx,
			`SELECT ceoID FROM corporation WHERE corporationID = ?`, founderCorpID).Scan(&creatorCharID)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO alliances
		  (allianceID, allianceName, shortName, description, url,
		   executorCorpID, creatorCorpID, creatorCharID,
		   dictatorial, startDate, memberCount, deleted)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 0, 0)`,
		allianceID, in.Name, in.ShortName, in.Description, in.URL,
		founderCorpID, founderCorpID, creatorCharID, dict, win32TimeNow(),
	); err != nil {
		return 0, fmt.Errorf("ESPCreateAlliance insert: %w", err)
	}
	if err := ESPSyncAllianceNames(ctx, db, allianceID, in.Name, in.ShortName); err != nil {
		return allianceID, err
	}
	if founderCorpID > 0 {
		if _, err := ESPSetCorpAlliance(ctx, db, founderCorpID, allianceID); err != nil {
			return allianceID, err
		}
	}
	return allianceID, ESPRecountAlliance(ctx, db, allianceID)
}

// ESPUpdateAlliance writes the alliance editor's form.
func ESPUpdateAlliance(ctx context.Context, db *sql.DB, allianceID int64, in ESPAllianceEdit) error {
	if strings.TrimSpace(in.Name) == "" {
		return fmt.Errorf("alliance name may not be empty")
	}
	if s := strings.TrimSpace(in.ShortName); s == "" || len(s) > 5 {
		return fmt.Errorf("short name must be 1–5 characters")
	}
	if in.ExecutorCorpID > 0 {
		var member int64
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM corporation WHERE corporationID = ? AND allianceID = ?`,
			in.ExecutorCorpID, allianceID).Scan(&member); err != nil {
			return fmt.Errorf("ESPUpdateAlliance executor check: %w", err)
		}
		if member == 0 {
			return fmt.Errorf("corporation %d is not in this alliance", in.ExecutorCorpID)
		}
	}
	dict := int64(0)
	if in.Dictatorial {
		dict = 1
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE alliances
		   SET allianceName = ?, shortName = ?, description = ?, url = ?,
		       executorCorpID = ?, dictatorial = ?
		 WHERE allianceID = ?`,
		in.Name, in.ShortName, in.Description, in.URL,
		in.ExecutorCorpID, dict, allianceID,
	); err != nil {
		return fmt.Errorf("ESPUpdateAlliance: %w", err)
	}
	return ESPSyncAllianceNames(ctx, db, allianceID, in.Name, in.ShortName)
}

// ESPDeleteAlliance unlinks every member corporation and drops the alliance, its name rows,
// wars and unpaid maintenance bills in one transaction. Returns the former member corporations
// for the allianceid session push; without it their sessions stay bound to a deleted alliance.
func ESPDeleteAlliance(ctx context.Context, db *sql.DB, allianceID int64) ([]int64, error) {
	memberRows, err := db.QueryContext(ctx,
		`SELECT corporationID FROM corporation WHERE allianceID = ?`, allianceID)
	if err != nil {
		return nil, fmt.Errorf("ESPDeleteAlliance members: %w", err)
	}
	var members []int64
	for memberRows.Next() {
		var id int64
		if err := memberRows.Scan(&id); err == nil {
			members = append(members, id)
		}
	}
	memberRows.Close()

	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("ESPDeleteAlliance begin: %w", err)
	}
	defer tx.Rollback()

	now := win32TimeNow()
	if _, err := tx.ExecContext(ctx,
		`UPDATE wars SET timeFinished = ?
		  WHERE (declaredByID = ? OR againstID = ?) AND timeFinished IS NULL`,
		now, allianceID, allianceID); err != nil {
		return nil, fmt.Errorf("ESPDeleteAlliance wars: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE bills SET paid = 1
		  WHERE paid = 0 AND ((billTypeID = ? AND externalID = ?) OR debtorID = ? OR creditorID = ?)`,
		BillTypeAllianceMaintainance, allianceID, allianceID, allianceID); err != nil {
		return nil, fmt.Errorf("ESPDeleteAlliance bills: %w", err)
	}

	for _, stmt := range []string{
		`UPDATE corporation SET allianceID = 0 WHERE allianceID = ?`,
		`DELETE FROM solarSystemSov WHERE allianceID = ?`,
		// Member, application and relationship rows go too; a left-behind member row claims a corp
		// for a deleted alliance.
		`DELETE FROM allianceMembers WHERE allianceID = ?`,
		`DELETE FROM allianceApplications WHERE allianceID = ?`,
		`DELETE FROM allianceRelationships WHERE allianceID = ?`,
		`DELETE FROM allianceRelationships WHERE toID = ?`,
		`DELETE FROM crpStandings WHERE toID = ?`,
		`DELETE FROM alliances WHERE allianceID = ?`,
		`DELETE FROM alliance_shortnames WHERE allianceID = ?`,
		`DELETE FROM eveNames WHERE itemID = ?`,
		`DELETE FROM eveStaticOwners WHERE ownerID = ?`,
		`DELETE FROM entity WHERE itemID = ? AND typeID = 16159`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, allianceID); err != nil {
			return nil, fmt.Errorf("ESPDeleteAlliance %q: %w", stmt, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	// Rows with toID = allianceID belonged to other alliances.
	allianceRelationshipsChanged(0)
	return members, nil
}

// ESPRecountAlliance refreshes alliances.memberCount; nothing else maintains it.
func ESPRecountAlliance(ctx context.Context, db *sql.DB, allianceID int64) error {
	_, err := db.ExecContext(ctx, `
		UPDATE alliances SET memberCount =
		  COALESCE((SELECT COUNT(*) FROM character_ ch
		              JOIN corporation c ON c.corporationID = ch.corporationID
		             WHERE c.allianceID = ? AND ch.accountID > 0), 0)
		 WHERE allianceID = ?`, allianceID, allianceID)
	if err != nil {
		return fmt.Errorf("ESPRecountAlliance: %w", err)
	}
	return nil
}

// ESPSyncAllianceNames writes an alliance's name into eveNames, eveStaticOwners and
// alliance_shortnames, the tables the client resolves owner names from.
func ESPSyncAllianceNames(ctx context.Context, db *sql.DB, allianceID int64, name, shortName string) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("ESPSyncAllianceNames begin: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO eveNames (itemID, itemName, categoryID, groupID, typeID)
		 VALUES (?, ?, 19, 32, 16159)`, allianceID, name); err != nil {
		return fmt.Errorf("ESPSyncAllianceNames eveNames: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO eveStaticOwners (ownerID, ownerName, typeID) VALUES (?, ?, 16159)`,
		allianceID, name); err != nil {
		return fmt.Errorf("ESPSyncAllianceNames eveStaticOwners: %w", err)
	}
	if _, err := tx.ExecContext(ctx,
		`REPLACE INTO alliance_shortnames (allianceID, shortName) VALUES (?, ?)`,
		allianceID, shortName); err != nil {
		return fmt.Errorf("ESPSyncAllianceNames alliance_shortnames: %w", err)
	}
	// No entity row: `entity` is the item allocator, and an insert at an alliance ID moves its
	// AUTO_INCREMENT onto the alliance band. See docs/db-5-notes.md#esp-corporation-and-alliance-administration.
	return tx.Commit()
}

// RepairAllianceEntityRows removes stray typeID-16159 rows from `entity` and settles the item
// allocator's AUTO_INCREMENT back onto the real items.
func RepairAllianceEntityRows(ctx context.Context, db *sql.DB) error {
	res, err := db.ExecContext(ctx,
		`DELETE FROM entity WHERE typeID = 16159 AND itemID >= ?`, MinPlayerEntityID)
	if err != nil {
		return fmt.Errorf("RepairAllianceEntityRows delete: %w", err)
	}
	if n, _ := res.RowsAffected(); n > 0 {
		var maxID sql.NullInt64
		if err := db.QueryRowContext(ctx, `SELECT MAX(itemID) FROM entity`).Scan(&maxID); err == nil && maxID.Valid {
			_, _ = db.ExecContext(ctx,
				fmt.Sprintf(`ALTER TABLE entity AUTO_INCREMENT = %d`, maxID.Int64+1))
		}
	}
	return nil
}

// ESPAllianceNameIssues reports alliances whose name rows are missing or stale, one line per
// problem, empty when everything agrees.
func ESPAllianceNameIssues(ctx context.Context, db *sql.DB) ([]string, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `
		SELECT a.allianceID, a.allianceName, a.shortName,
		       (SELECT COUNT(*) FROM alliance_shortnames sn WHERE sn.allianceID = a.allianceID),
		       (SELECT COUNT(*) FROM eveNames n WHERE n.itemID = a.allianceID),
		       (SELECT COUNT(*) FROM eveStaticOwners o WHERE o.ownerID = a.allianceID)
		  FROM alliances a`)
	if err != nil {
		return nil, fmt.Errorf("ESPAllianceNameIssues: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var (
			id                        int64
			name, short               string
			hasEntity, hasName, hasSO int64
		)
		if err := rows.Scan(&id, &name, &short, &hasEntity, &hasName, &hasSO); err != nil {
			return nil, fmt.Errorf("ESPAllianceNameIssues scan: %w", err)
		}
		var missing []string
		if hasEntity == 0 {
			missing = append(missing, "alliance_shortnames (client short-name lookup)")
		}
		if hasName == 0 {
			missing = append(missing, "eveNames")
		}
		if hasSO == 0 {
			missing = append(missing, "eveStaticOwners")
		}
		if len(missing) > 0 {
			out = append(out, fmt.Sprintf("%s (%d): missing %s", name, id, strings.Join(missing, ", ")))
		}
	}
	return out, rows.Err()
}

// ESPRepairAllianceNames rewrites the name rows for every alliance.
func ESPRepairAllianceNames(ctx context.Context, db *sql.DB) (int, error) {
	if err := ensureAlliancesTable(ctx, db); err != nil {
		return 0, err
	}
	rows, err := db.QueryContext(ctx, `SELECT allianceID, allianceName, shortName FROM alliances`)
	if err != nil {
		return 0, fmt.Errorf("ESPRepairAllianceNames: %w", err)
	}
	type row struct {
		id          int64
		name, short string
	}
	var all []row
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.id, &r.name, &r.short); err != nil {
			rows.Close()
			return 0, fmt.Errorf("ESPRepairAllianceNames scan: %w", err)
		}
		all = append(all, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	fixed := 0
	for _, r := range all {
		if err := ESPSyncAllianceNames(ctx, db, r.id, r.name, r.short); err != nil {
			return fixed, err
		}
		_ = ESPRecountAlliance(ctx, db, r.id)
		fixed++
	}
	return fixed, nil
}
